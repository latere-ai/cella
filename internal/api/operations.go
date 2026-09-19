// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

func (h *handler) authorizedObject(r *http.Request, action string) (v1.Sandbox, error) {
	obj, err := h.Controller.Get(r.Context(), r.PathValue("id"), caller(r).Subject)
	if err != nil {
		return obj, err
	}
	_, err = h.decide(r, action, resource(obj))
	return obj, err
}

// touch stamps activity on the sandbox a request drives, the activity path of
// design 005. The controller coalesces it per sandbox, so a busy session
// reaches the driver once an interval. A stamp that fails is logged and never
// returned: activity is a note on a sandbox, not the work the caller asked for.
func (h *handler) touch(r *http.Request, obj v1.Sandbox) {
	if err := h.Controller.Touch(r.Context(), obj.Status.ID); err != nil {
		slog.WarnContext(r.Context(), "activity stamp failed", "sandbox", obj.Status.ID, "error", err)
	}
}
func validWorkspacePath(p string) bool {
	return path.Clean(p) == p && !strings.ContainsRune(p, 0) && (p == "/workspace" || strings.HasPrefix(p, "/workspace/"))
}
func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return
	}
	if !h.Controller.Capabilities().Files {
		respondError(w, runtime.ErrUnsupported)
		return
	}
	h.touch(r, obj)
	if r.Method == http.MethodPut {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/x-tar" {
			respondError(w, &manifest.Error{Code: "unsupported_media_type", Detail: "file upload requires application/x-tar"})
			return
		}
		dest := r.URL.Query().Get("dest")
		if !validWorkspacePath(dest) {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "dest must be an absolute path below /workspace"})
			return
		}
		// Validate total request size before extraction begins. A tar reader may stop
		// at its terminator without reading an oversized suffix, so a reader cap alone
		// would not enforce the HTTP upload limit or prevent partial writes.
		spool, err := os.CreateTemp("", "cella-upload-*")
		if err != nil {
			respondError(w, err)
			return
		}
		defer func() { _ = os.Remove(spool.Name()) }()
		defer func() { _ = spool.Close() }()
		_, err = io.Copy(spool, http.MaxBytesReader(w, r.Body, h.MaxUploadBytes))
		if err != nil {
			respondError(w, err)
			return
		}
		if _, err = spool.Seek(0, io.SeekStart); err == nil {
			err = h.Controller.ImportTar(r.Context(), obj.Status.ID, dest, spool)
		}
		if err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	paths := r.URL.Query()["path"]
	for _, p := range paths {
		if !validWorkspacePath(p) {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "path must be an absolute path below /workspace"})
			return
		}
	}
	stream := newStream(w, "application/x-tar")
	if err = h.Controller.ExportTar(r.Context(), obj.Status.ID, paths, stream); err != nil {
		stream.fail(err)
	}
}
func (h *handler) logs(w http.ResponseWriter, r *http.Request) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxRead)
	if err != nil {
		respondError(w, err)
		return
	}
	var req runtime.LogsRequest
	q := r.URL.Query()
	if follow := q.Get("follow"); follow != "" {
		if follow != "0" && follow != "1" {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "follow must be 0 or 1"})
			return
		}
		req.Follow = follow == "1"
	}
	if since := q.Get("since"); since != "" {
		req.Since, err = time.Parse(time.RFC3339, since)
		if err != nil {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "since must be RFC3339"})
			return
		}
	}
	if tail := q.Get("tail"); tail != "" {
		req.TailLines, err = strconv.Atoi(tail)
		if err != nil || req.TailLines < 0 || req.TailLines > 100000 {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "tail must be between 0 and 100000"})
			return
		}
	}
	logs, err := h.Controller.Logs(r.Context(), obj.Status.ID, req)
	if err != nil {
		respondError(w, err)
		return
	}
	defer func() { _ = logs.Close() }()
	stream := newStream(w, "text/plain; charset=utf-8")
	stream.flush = req.Follow
	if _, err = io.Copy(stream, logs); err != nil {
		stream.fail(err)
	}
}

// responseStream preserves structured failures before streaming and advertises
// a trailer for failures that occur after status and body have been sent.
type responseStream struct {
	w       http.ResponseWriter
	written bool
	flush   bool
}

func newStream(w http.ResponseWriter, contentType string) *responseStream {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Trailer", "X-Cella-Error")
	return &responseStream{w: w}
}
func (s *responseStream) Write(p []byte) (int, error) {
	if len(p) > 0 {
		s.written = true
	}
	n, err := s.w.Write(p)
	if s.flush {
		if flusher, ok := s.w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return n, err
}
func (s *responseStream) fail(err error) {
	if !s.written {
		s.w.Header().Del("Trailer")
		respondError(s.w, err)
		return
	}
	code := "driver_unavailable"
	if errors.Is(err, runtime.ErrInvalid) {
		code = "invalid_field"
	}
	s.w.Header().Set("X-Cella-Error", code)
}
