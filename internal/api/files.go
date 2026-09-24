// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// tarMedia is the body that means the whole-tree transfer rather than one
// file, which is what separates the two writes on the files collection.
const tarMedia = "application/x-tar"

// entry is one file or directory as the API renders it. The mode is octal
// text: a JSON number for a permission set reads as decimal and is misread.
type entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

func rendered(info runtime.FileInfo) entry {
	return entry{
		Name:    info.Name,
		Path:    info.Path,
		Size:    info.Size,
		Mode:    "0" + strconv.FormatUint(uint64(info.Mode.Perm()), 8),
		ModTime: info.ModTime.UTC(),
		IsDir:   info.IsDir,
	}
}

// pathRequest is the body of the two operations that name a path in JSON
// rather than in the query, because the caller sends no other bytes.
type pathRequest struct {
	Path string `json:"path"`
}

// moveRequest names both ends of a move.
type moveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// fileTarget is the preamble of every file route: read the sandbox,
// authorize, refuse an environment without the capability before any driver
// call, and stamp the activity the request is.
func (h *handler) fileTarget(w http.ResponseWriter, r *http.Request) (v1.Sandbox, runtime.FileStore, bool) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return obj, nil, false
	}
	store, err := h.fileStore(obj)
	if err != nil {
		respondError(w, err)
		return obj, nil, false
	}
	h.touch(r, obj)
	return obj, store, true
}

// fileStore is the driver's per-file half, or the capability the environment
// does not have.
func (h *handler) fileStore(obj v1.Sandbox) (runtime.FileStore, error) {
	if !h.Controller.CapabilitiesOf(obj.Status.Environment).Files {
		return nil, runtime.ErrUnsupported
	}
	return h.Controller.Files(obj.Status.ID)
}

// queryPath reads the one path a route takes, held to the sandbox's
// workspace. A repeated selector is refused rather than answered for one of
// its values.
func queryPath(r *http.Request, key string, obj v1.Sandbox) (string, error) {
	values := r.URL.Query()[key]
	if len(values) != 1 {
		return "", &manifest.Error{Code: "invalid_field", Detail: "exactly one " + key + " is required"}
	}
	return values[0], workspacePathError(obj, values[0])
}

// workspacePathError is the lexical half of the containment rule of design
// 033, which the API applies before a driver is called at all. The half that
// resolves symbolic links belongs where the filesystem is.
func workspacePathError(obj v1.Sandbox, p string) error {
	if root := workspaceOf(obj); !validWorkspacePath(root, p) {
		return &manifest.Error{Code: "invalid_field", Detail: "the path must be absolute and below the workspace, " + root}
	}
	return nil
}

// fileContent streams one file.
func (h *handler) fileContent(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	path, err := queryPath(r, "path", obj)
	if err != nil {
		respondError(w, err)
		return
	}
	body, info, err := store.Open(r.Context(), obj.Status.ID, path)
	if err != nil {
		respondError(w, err)
		return
	}
	defer func() { _ = body.Close() }()
	stream := newStream(w, "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	if _, err = io.Copy(stream, body); err != nil {
		stream.fail(err)
	}
	h.emit(r, obj, events.TypeFiles, events.Files{
		Operation: events.OperationRead, Direction: events.DirectionExport,
		Paths: []string{path}, Bytes: stream.bytes,
	})
}

// fileStat describes one entry.
func (h *handler) fileStat(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	path, err := queryPath(r, "path", obj)
	if err != nil {
		respondError(w, err)
		return
	}
	info, err := store.Stat(r.Context(), obj.Status.ID, path)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, rendered(info))
	h.emit(r, obj, events.TypeFiles, events.Files{Operation: events.OperationStat, Paths: []string{path}})
}

// fileList lists a directory, whole: a workspace directory large enough to
// need a cursor is an archive.
func (h *handler) fileList(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	path, err := queryPath(r, "path", obj)
	if err != nil {
		respondError(w, err)
		return
	}
	infos, err := store.ReadDir(r.Context(), obj.Status.ID, path)
	if err != nil {
		respondError(w, err)
		return
	}
	items := make([]entry, 0, len(infos))
	for _, info := range infos {
		items = append(items, rendered(info))
	}
	respond(w, http.StatusOK, map[string]any{"items": items, "next": ""})
	h.emit(r, obj, events.TypeFiles, events.Files{Operation: events.OperationList, Paths: []string{path}})
}

// filesPut separates the two writes the files collection takes: an archive
// extracted below dest, and one file at path. The selector says which, so a
// caller can store an archive as one file, and the content type is what an
// archive is then held to.
func (h *handler) filesPut(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, archive := q["dest"]
	_, single := q["path"]
	switch {
	case archive && single:
		respondError(w, &manifest.Error{Code: "exclusive_fields", Detail: "dest extracts an archive and path writes one file"})
	case archive:
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != tarMedia {
			respondError(w, &manifest.Error{Code: "unsupported_media_type", Detail: "extracting below dest requires " + tarMedia})
			return
		}
		h.files(w, r)
	default:
		h.fileWrite(w, r)
	}
}

// fileWrite stores one file. The body streams to the driver under the upload
// bound, which the driver applies as well, so a body past it never reaches
// the file the write names.
func (h *handler) fileWrite(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	path, err := queryPath(r, "path", obj)
	if err != nil {
		respondError(w, err)
		return
	}
	mode, err := fileMode(r.URL.Query().Get("mode"))
	if err != nil {
		respondError(w, err)
		return
	}
	n, err := store.Write(r.Context(), obj.Status.ID, runtime.WriteRequest{
		Path:     path,
		Mode:     mode,
		MaxBytes: h.MaxUploadBytes,
		Body:     http.MaxBytesReader(w, r.Body, h.MaxUploadBytes),
	})
	if err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.emit(r, obj, events.TypeFiles, events.Files{
		Operation: events.OperationWrite, Direction: events.DirectionImport,
		Paths: []string{path}, Bytes: n,
	})
}

// fileMode reads the octal text a write names its mode with.
func fileMode(text string) (fs.FileMode, error) {
	if text == "" {
		return 0, nil
	}
	bits, err := strconv.ParseUint(text, 8, 32)
	if err != nil || bits > 0o777 {
		return 0, &manifest.Error{Code: "invalid_field", Detail: "mode is octal text of at most 0777"}
	}
	return fs.FileMode(bits), nil
}

// fileRemove deletes a file or a directory tree.
func (h *handler) fileRemove(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	path, err := queryPath(r, "path", obj)
	if err != nil {
		respondError(w, err)
		return
	}
	if err = store.Remove(r.Context(), obj.Status.ID, path); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.emit(r, obj, events.TypeFiles, events.Files{Operation: events.OperationRemove, Paths: []string{path}})
}

// fileMkdir creates a directory and the missing parents.
func (h *handler) fileMkdir(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	var req pathRequest
	if err := h.decodeBody(w, r, &req); err != nil {
		respondError(w, err)
		return
	}
	if err := workspacePathError(obj, req.Path); err != nil {
		respondError(w, err)
		return
	}
	if err := store.Mkdir(r.Context(), obj.Status.ID, req.Path); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.emit(r, obj, events.TypeFiles, events.Files{Operation: events.OperationMkdir, Paths: []string{req.Path}})
}

// fileMove renames one path onto another, both inside the workspace.
func (h *handler) fileMove(w http.ResponseWriter, r *http.Request) {
	obj, store, ok := h.fileTarget(w, r)
	if !ok {
		return
	}
	var req moveRequest
	if err := h.decodeBody(w, r, &req); err != nil {
		respondError(w, err)
		return
	}
	if err := errors.Join(workspacePathError(obj, req.From), workspacePathError(obj, req.To)); err != nil {
		respondError(w, firstError(err))
		return
	}
	if err := store.Move(r.Context(), obj.Status.ID, req.From, req.To); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.emit(r, obj, events.TypeFiles, events.Files{
		Operation: events.OperationMove, Paths: []string{req.From, req.To},
	})
}

// firstError is the first of a joined pair, so two bad paths answer with one
// code and one sentence.
func firstError(err error) error {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		return joined.Unwrap()[0]
	}
	return err
}

// decodeBody reads one JSON object of a bounded body and refuses a field the
// route does not have, which is the rule every other body of design 008 has.
func (h *handler) decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	body, err := h.readBody(w, r)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(into); err == nil {
		var tail any
		if dec.Decode(&tail) != io.EOF {
			err = errors.New("expected one JSON object")
		}
	}
	if err != nil {
		return &manifest.Error{Code: "invalid_field", Detail: err.Error()}
	}
	return nil
}
