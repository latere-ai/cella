// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a server built from the designs the suite reads, not from this
// repository's handlers. It is what proves the suite is a contract and not a
// description of one implementation: the cases pass against it, and each of
// its three broken modes turns them into the failures a report has to carry.
//
// It keeps objects in memory and answers the routes a case reaches over
// HTTP. The two WebSocket streams are not here: a socket is the one part of
// the contract this package proves against a real server.
type fake struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	names   map[string]string
	secrets map[string]map[string]any
	volumes map[string]map[string]any
	sets    map[string]map[string]any
	files   map[string]map[string]string
	events  map[string][]map[string]any
	deleted []string
	counter int
	seq     int64

	// wrongMessage answers a refusal with a sentence outside the table, and
	// wrongValues answers every call with the shape the design states and a
	// value it does not: the exit code of another command, a mode that is not
	// the one written, a geometry nobody asked for, a secret's own value. It
	// is what proves a case reads the answer and not only its status.
	wrongMessage bool
	wrongValues  bool
	// driftedDefault resolves spec.mesh.spawn.budget to 1 where the manifest
	// contract's default is 0, and answers everything else as an honest
	// server does: the fake's form of the drift seam of design 015.
	driftedDefault bool
	// execStream makes the framed exec stream malformed in one named way, so
	// a test reads every assertion case008ExecStream makes and not only the
	// first. The empty string serves the stream design 008 states.
	execStream string
	// noSecretKey answers a secret's apply the way an installation that
	// holds no key to seal a value under answers it.
	noSecretKey bool
	// declared, when set, is the capability set the fake honors: a gated
	// route of any other capability answers capability_unsupported before
	// it is routed, as the server's own gate does. desktopOnly answers the
	// display route not_found for a sandbox whose manifest asked for no
	// desktop, as design 023 states.
	declared    map[string]bool
	desktopOnly bool
	// mintFailure is how the issuer route fails: empty mints, "status" for a
	// refusal, "body" for an answer that is no token, "empty" for a token
	// that is not there.
	mintFailure string
	// breakMode is how this server breaks: empty never, "gone" for a
	// connection that goes away, "garbage" for an answer that is no answer.
	// breakAfter is how many calls it answers before it breaks, so a sweep
	// over it breaks one step of a case after another.
	breakMode  string
	breakAfter int
	requests   int

	server *httptest.Server
}

// workloadToken is what this fake hands a sandbox that reads its own token
// file, and what it recognizes as one on the way back in.
const workloadToken = "fake-token-workload"

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{
		objects: map[string]map[string]any{}, names: map[string]string{},
		secrets: map[string]map[string]any{}, volumes: map[string]map[string]any{},
		sets: map[string]map[string]any{}, files: map[string]map[string]string{},
		events: map[string][]map[string]any{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, http.StatusOK, map[string]any{"version": "fake"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		if f.wrongValues {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "openapi: 3.1.0\n")
	})
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		keys := []any{map[string]any{"kty": "RSA", "kid": "fake"}}
		if f.wrongValues {
			keys = nil
		}
		f.write(w, http.StatusOK, map[string]any{"keys": keys})
	})
	// The control contract of the suite and the sink's own records.
	mux.HandleFunc("POST /fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /mint", f.mint)
	mux.HandleFunc("GET /events", f.sinkRecords)

	mux.Handle("POST /v1/sandboxes", f.authenticated(f.create))
	mux.Handle("PUT /v1/sandboxes/{name}", f.authenticated(f.apply))
	mux.Handle("GET /v1/sandboxes", f.authenticated(f.list))
	mux.Handle("GET /v1/sandboxes/{id}", f.authenticated(f.read))
	mux.Handle("DELETE /v1/sandboxes/{id}", f.authenticated(f.remove))
	mux.Handle("POST /v1/sandboxes/{id}/{verb}", f.authenticated(f.verb))
	mux.Handle("GET /v1/sandboxes/{id}/logs", f.authenticated(f.logs))
	mux.Handle("GET /v1/sandboxes/{id}/egress", f.authenticated(f.egress))
	mux.Handle("GET /v1/sandboxes/{id}/display", f.authenticated(f.display))
	mux.Handle("GET /v1/sandboxes/{id}/screenshot", f.authenticated(f.screenshot))
	mux.Handle("GET /v1/sandboxes/{id}/files", f.authenticated(f.exportTar))
	mux.Handle("PUT /v1/sandboxes/{id}/files", f.authenticated(f.writeFile))
	mux.Handle("DELETE /v1/sandboxes/{id}/files", f.authenticated(f.removeFile))
	mux.Handle("GET /v1/sandboxes/{id}/files/{op}", f.authenticated(f.readFile))
	mux.Handle("POST /v1/sandboxes/{id}/files/{op}", f.authenticated(f.changeFiles))
	mux.Handle("GET /v1/events", f.authenticated(f.objectFeed))
	mux.Handle("PUT /v1/secrets/{name}", f.authenticated(f.applySecret))
	mux.Handle("GET /v1/secrets/{name}", f.authenticated(f.readSecret))
	mux.Handle("DELETE /v1/secrets/{name}", f.authenticated(f.deleteSecret))
	mux.Handle("GET /v1/secrets", f.authenticated(f.listSecrets))
	mux.Handle("PUT /v1/volumes/{name}", f.authenticated(f.applyKind("volumes")))
	mux.Handle("GET /v1/volumes/{name}", f.authenticated(f.readKind("volumes")))
	mux.Handle("DELETE /v1/volumes/{name}", f.authenticated(f.deleteKind("volumes")))
	mux.Handle("PUT /v1/sandboxsets/{name}", f.authenticated(f.applyKind("sandboxsets")))
	mux.Handle("DELETE /v1/sandboxsets/{name}", f.authenticated(f.deleteKind("sandboxsets")))
	mux.Handle("GET /v1/environments", f.authenticated(f.environments))
	mux.Handle("/", f.authenticated(func(w http.ResponseWriter, _ *http.Request) {
		f.refuse(w, "not_found")
	}))
	f.server = httptest.NewServer(f.gate(mux))
	t.Cleanup(f.server.Close)
	return f
}

// gate refuses a gated route whose capability the fake does not declare,
// when a set is declared. The route suffixes are the suite's own gates.
func (f *fake) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		declared := f.declared
		f.mu.Unlock()
		if declared != nil && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") {
			for _, g := range gates() {
				suffix, _, _ := strings.Cut(g.suffix, "?")
				if strings.HasSuffix(r.URL.Path, suffix) && !declared[g.capability] {
					f.refuse(w, "capability_unsupported")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fake) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// refuse answers the envelope of design 008, with the sentence of the table
// unless this fake was asked to answer another.
func (f *fake) refuse(w http.ResponseWriter, code string, paths ...string) {
	r := errorTable[code]
	message := r.Message
	if f.wrongMessage {
		message = "Something went wrong."
	}
	details := map[string]any{"request_id": "req_fake", "detail": "the fake refused"}
	if len(paths) > 0 {
		details["paths"] = paths
	}
	f.write(w, r.Status, map[string]any{"error": map[string]any{
		"code": code, "message": message, "details": details,
	}})
}

// authenticated is the door of design 008: every route under /v1 needs a
// bearer. It is also where the broken modes live, because what breaks has to
// be able to break on any route.
func (f *fake) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		broken := f.breakMode != "" && f.requests > f.breakAfter
		stop := broken && f.breakMode == "gone"
		garbage := broken && f.breakMode == "garbage"
		f.mu.Unlock()
		if stop {
			// A connection that goes away mid-run, which is what a case has
			// to report rather than read as an answer.
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer != "fake-token" && bearer != workloadToken {
			f.refuse(w, "unauthenticated")
			return
		}
		if garbage {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("this is no JSON"))
			return
		}
		next(w, r)
	})
}

// decode is the decoding order of design 003: the media type, the size, one
// document, the version, the kind, then the fields.
func (f *fake) decode(w http.ResponseWriter, r *http.Request, kind string) (map[string]any, bool) {
	media := r.Header.Get("Content-Type")
	switch media {
	case "application/json":
	case "application/yaml", "application/x-yaml", "text/yaml":
		// The fake does not parse YAML: what a case asserts is that the type
		// is accepted, so the body is read and one object of that kind is
		// made from it.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
		return map[string]any{"apiVersion": APIVersion, "kind": kind, "metadata": map[string]any{}, "spec": map[string]any{}}, true
	default:
		f.refuse(w, "unsupported_media_type")
		return nil, false
	}
	var body map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := decoder.Decode(&body); err != nil {
		f.refuse(w, "body_too_large")
		return nil, false
	}
	if decoder.More() {
		f.refuse(w, "multi_document")
		return nil, false
	}
	if version, _ := body["apiVersion"].(string); version != APIVersion {
		f.refuse(w, "unsupported_version")
		return nil, false
	}
	if got, _ := body["kind"].(string); got != kind {
		f.refuse(w, "unsupported_kind")
		return nil, false
	}
	for key := range body {
		if !slices.Contains([]string{"apiVersion", "kind", "metadata", "spec", "status"}, key) {
			f.refuse(w, "unknown_field")
			return nil, false
		}
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		for key := range metadata {
			if !slices.Contains([]string{"name", "labels"}, key) {
				f.refuse(w, "unknown_field")
				return nil, false
			}
		}
	}
	if spec, ok := body["spec"].(map[string]any); ok && kind == "Sandbox" {
		for key := range spec {
			if !slices.Contains(sandboxSpecFields, key) {
				f.refuse(w, "unknown_field")
				return nil, false
			}
		}
	}
	return body, true
}

// sandboxSpecFields are the fields of a sandbox's specification in design
// 003's field table, so a manifest a case builds with a field the schema does
// not know is refused here as a real server refuses it, and not stored.
var sandboxSpecFields = []string{
	"environment", "image", "command", "args", "workdir", "user", "resources",
	"workspace", "volumes", "env", "secrets", "network", "mesh", "scheduling",
	"lifecycle", "display",
}

// record files one event for an object, which is what the feed and the sink
// answer from.
func (f *fake) record(id, kind string) {
	f.seq++
	f.events[id] = append(f.events[id], map[string]any{
		"id": fmt.Sprintf("evt_fake%018d", f.seq), "seq": f.seq, "type": kind,
		"time": time.Now().UTC().Format(time.RFC3339), "object": map[string]any{"id": id, "kind": "Sandbox"},
	})
}

func (f *fake) create(w http.ResponseWriter, r *http.Request) {
	body, ok := f.decode(w, r, "Sandbox")
	if !ok {
		return
	}
	if bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); bearer == workloadToken {
		// A sandbox applying a child may not widen what it holds, which is
		// the one answer this fake gives a workload's apply, at the field
		// the one child a case applies widens.
		widened := []string{"spec.mesh.spawn.budget"}
		if f.wrongValues {
			widened = []string{"spec.resources.cpu"}
		}
		f.refuse(w, "boundary_exceeded", widened...)
		return
	}
	metadata, _ := body["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.names[name]; taken && name != "" {
		f.refuse(w, "name_taken")
		return
	}
	f.write(w, http.StatusCreated, f.store(body, name))
}

// apply is the apply by name of design 008: the path names the object and a
// body that names another is refused at the field.
func (f *fake) apply(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, ok := f.decode(w, r, "Sandbox")
	if !ok {
		return
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	if named, _ := metadata["name"].(string); named != "" && named != name {
		f.refuse(w, "invalid_field", "metadata.name")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, taken := f.names[name]; taken {
		f.write(w, http.StatusOK, f.objects[id])
		return
	}
	f.write(w, http.StatusCreated, f.store(body, name))
}

// store puts one object away and returns it as the answer carries it. The
// caller holds the lock.
func (f *fake) store(body map[string]any, name string) map[string]any {
	f.counter++
	id := fmt.Sprintf("sbx_fake%022d", f.counter)
	if name == "" {
		name = id
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	metadata["name"] = name
	resolveDefaults(body, f.driftedDefault)
	body["status"] = map[string]any{
		"id": id, "owner": "fake", "environment": "default",
		"driver": "fake", "isolation": "none", "phase": "Running",
	}
	if f.wrongValues {
		// A status that leaves out what every answer carries, and a
		// specification the server rewrote behind the caller's back.
		delete(body["status"].(map[string]any), "driver")
		if spec, ok := body["spec"].(map[string]any); ok {
			spec["command"] = []any{"another", "command"}
		}
	}
	f.objects[id], f.names[name] = body, id
	f.files[id] = map[string]string{}
	f.record(id, "sandbox.created")
	return body
}

// resolveDefaults fills the literal defaults of design 003 into a sandbox's
// specification where the manifest left them out, which is the part of a
// resolve a case reads. drift resolves the spawn budget one unit off.
func resolveDefaults(body map[string]any, drift bool) {
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		body["spec"] = spec
	}
	workspace, _ := spec["workspace"].(map[string]any)
	if workspace == nil {
		workspace = map[string]any{}
		spec["workspace"] = workspace
	}
	if _, ok := workspace["path"]; !ok {
		workspace["path"] = "/workspace"
	}
	if _, ok := workspace["source"]; !ok {
		workspace["source"] = "empty"
	}
	if _, ok := spec["workdir"]; !ok {
		spec["workdir"] = workspace["path"]
	}
	network, _ := spec["network"].(map[string]any)
	if network == nil {
		network = map[string]any{}
		spec["network"] = network
	}
	egress, _ := network["egress"].(map[string]any)
	if egress == nil {
		egress = map[string]any{}
		network["egress"] = egress
	}
	if _, ok := egress["mode"]; !ok {
		egress["mode"] = "open"
	}
	if !drift {
		return
	}
	mesh, _ := spec["mesh"].(map[string]any)
	if mesh == nil {
		mesh = map[string]any{}
		spec["mesh"] = mesh
	}
	spawn, _ := mesh["spawn"].(map[string]any)
	if spawn == nil {
		spawn = map[string]any{}
		mesh["spawn"] = spawn
	}
	if _, ok := spawn["budget"]; !ok {
		spawn["budget"] = 1
	}
}

func (f *fake) read(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	f.write(w, http.StatusOK, obj)
}

func (f *fake) remove(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	id := idOf(obj)
	f.deleted = append(f.deleted, id)
	delete(f.objects, id)
	status(obj)["phase"] = "Deleting"
	f.record(id, "sandbox.deleted")
	f.write(w, http.StatusAccepted, obj)
}

// verb is start, stop and exec: the phase rules of design 005 and the two
// forms of the exec stream of design 008.
func (f *fake) verb(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		f.mu.Unlock()
		f.refuse(w, "not_found")
		return
	}
	phase, _ := status(obj)["phase"].(string)
	switch r.PathValue("verb") {
	case "start":
		if phase != "Stopped" {
			f.mu.Unlock()
			f.refuse(w, "phase_conflict")
			return
		}
		status(obj)["phase"] = "Running"
		f.record(idOf(obj), "sandbox.started")
		f.mu.Unlock()
		f.write(w, http.StatusOK, obj)
	case "stop":
		if phase != "Running" {
			f.mu.Unlock()
			f.refuse(w, "phase_conflict")
			return
		}
		status(obj)["phase"] = "Stopped"
		f.record(idOf(obj), "sandbox.stopped")
		f.mu.Unlock()
		f.write(w, http.StatusOK, obj)
	case "exec":
		f.record(idOf(obj), "sandbox.exec")
		f.mu.Unlock()
		f.exec(w, r)
	default:
		f.mu.Unlock()
		f.refuse(w, "not_found")
	}
}

// exec answers both forms: the bounded result and the framed stream.
func (f *fake) exec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command []string `json:"command"`
		Timeout string   `json:"timeout"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		f.refuse(w, "bad_request")
		return
	}
	command := strings.Join(req.Command, " ")
	stdout, stderr, code := "", "", 0
	switch {
	case strings.Contains(command, "CELLA_TOKEN_FILE"):
		stdout = workloadToken
	case req.Timeout != "":
		code = 124
	case strings.Contains(command, "exit 7"):
		stdout, stderr, code = "out", "err", 7
	case strings.Contains(command, "exit 5"):
		stdout, stderr, code = "out", "err", 5
	}
	f.mu.Lock()
	wrong := f.wrongValues
	f.mu.Unlock()
	if wrong {
		stdout, stderr, code = "another command's output", "", 0
	}
	if r.URL.Query().Get("wait") == "1" {
		f.write(w, http.StatusOK, map[string]any{
			"exitCode": code, "stdout": stdout, "stderr": stderr, "truncated": wrong, "durationMs": 1,
		})
		return
	}
	f.mu.Lock()
	mode := f.execStream
	f.mu.Unlock()
	if mode == "status" {
		f.refuse(w, "driver_unavailable")
		return
	}
	contentType := "application/vnd.cella.exec-stream"
	if mode == "type" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	frame := func(channel byte, payload string) {
		header := make([]byte, 5)
		header[0] = channel
		binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
		_, _ = w.Write(append(header, payload...))
	}
	switch mode {
	case "oversize":
		header := make([]byte, 5)
		header[0] = 1
		binary.BigEndian.PutUint32(header[1:], (1<<20)+1)
		_, _ = w.Write(header)
		return
	case "short":
		// A header whose length names more than the body carries.
		header := make([]byte, 5)
		header[0] = 1
		binary.BigEndian.PutUint32(header[1:], 64)
		_, _ = w.Write(append(header, []byte("a few bytes")...))
		return
	case "channel":
		frame(9, "a channel design 008 does not name")
		return
	case "truncated":
		frame(1, stdout)
		return
	case "error":
		frame(4, `{"error":{"code":"driver_unavailable"}}`)
		return
	case "exit":
		frame(1, stdout)
		frame(2, stderr)
		frame(3, "not a number")
		return
	case "wrongexit":
		frame(1, stdout)
		frame(2, stderr)
		frame(3, "0")
		return
	case "wrongoutput":
		frame(1, "another command's output")
		frame(2, stderr)
		frame(3, strconv.Itoa(code))
		return
	}
	frame(1, stdout)
	frame(2, stderr)
	frame(3, strconv.Itoa(code))
}

func (f *fake) logs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	_, ok := f.lookup(r.PathValue("id"))
	f.mu.Unlock()
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	f.mu.Lock()
	wrong := f.wrongValues
	f.mu.Unlock()
	if wrong {
		f.write(w, http.StatusOK, map[string]any{"lines": []string{"another sandbox's output"}})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "conformance-line\n")
}

func (f *fake) egress(w http.ResponseWriter, _ *http.Request) {
	f.write(w, http.StatusOK, map[string]any{"items": []any{}, "next": ""})
}

func (f *fake) display(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	wrong := f.wrongValues
	obj, _ := f.lookup(r.PathValue("id"))
	desktopOnly := f.desktopOnly
	f.mu.Unlock()
	if spec, _ := obj["spec"].(map[string]any); desktopOnly && spec["display"] == nil {
		f.refuse(w, "not_found")
		return
	}
	if wrong {
		f.write(w, http.StatusOK, map[string]any{"width": 640, "height": 480, "ready": false})
		return
	}
	f.write(w, http.StatusOK, map[string]any{"width": 1280, "height": 800, "ready": true})
}

// pngHeader is the first bytes of a PNG, which is what a screenshot case
// reads to know it was handed a frame and not a page.
var pngHeader = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

func (f *fake) screenshot(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	wrong := f.wrongValues
	f.mu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	if wrong {
		_, _ = io.WriteString(w, "no frame")
		return
	}
	_, _ = w.Write(pngHeader)
}

// exportTar answers the archive of the paths named, which is the whole of
// what the archive case reads.
func (f *fake) exportTar(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.idFor(r)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	paths := r.URL.Query()["path"]
	for _, p := range paths {
		if !contained(p) {
			f.refuse(w, "invalid_field")
			return
		}
	}
	name, content := "suite.txt", ""
	for _, p := range paths {
		if stored, ok := f.files[id][p]; ok {
			name, content = path.Base(p), stored
		}
	}
	archive := tarOf(name, content)
	if f.wrongValues {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/x-tar")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

// writeFile is the two writes of design 008, told apart by the selector.
func (f *fake) writeFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.idFor(r)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		f.refuse(w, "body_too_large")
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("dest") != "":
		if r.Header.Get("Content-Type") != "application/x-tar" {
			f.refuse(w, "unsupported_media_type")
			return
		}
		if !contained(q.Get("dest")) {
			f.refuse(w, "invalid_field")
			return
		}
		name, content, err := firstEntry(body)
		if err != nil {
			f.refuse(w, "bad_request")
			return
		}
		f.files[id][path.Join(q.Get("dest"), name)] = content
	case q.Get("path") != "":
		if !contained(q.Get("path")) {
			f.refuse(w, "invalid_field")
			return
		}
		f.files[id][q.Get("path")] = string(body)
	default:
		f.refuse(w, "invalid_field")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fake) removeFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.idFor(r)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	target := r.URL.Query().Get("path")
	if !contained(target) {
		f.refuse(w, "invalid_field")
		return
	}
	for p := range f.files[id] {
		if p == target || strings.HasPrefix(p, target+"/") {
			delete(f.files[id], p)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// readFile is stat, list and content.
func (f *fake) readFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.idFor(r)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	paths := r.URL.Query()["path"]
	if len(paths) != 1 || !contained(paths[0]) {
		f.refuse(w, "invalid_field")
		return
	}
	target := paths[0]
	switch r.PathValue("op") {
	case "stat":
		content, ok := f.files[id][target]
		if !ok {
			f.refuse(w, "not_found")
			return
		}
		f.write(w, http.StatusOK, f.entryOf(target, content))
	case "list":
		items := []any{}
		for p, content := range f.files[id] {
			if path.Dir(p) == target {
				items = append(items, f.entryOf(p, content))
			}
		}
		f.write(w, http.StatusOK, map[string]any{"items": items, "next": ""})
	case "content":
		content, ok := f.files[id][target]
		if !ok {
			f.refuse(w, "not_found")
			return
		}
		if f.wrongValues {
			content = "another file's bytes"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, content)
	default:
		f.refuse(w, "not_found")
	}
}

// changeFiles is mkdir and move.
func (f *fake) changeFiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		f.refuse(w, "bad_request")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.idFor(r)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	switch r.PathValue("op") {
	case "mkdir":
		if !contained(body.Path) {
			f.refuse(w, "invalid_field")
			return
		}
	case "move":
		if !contained(body.From) || !contained(body.To) {
			f.refuse(w, "invalid_field")
			return
		}
		content, ok := f.files[id][body.From]
		if !ok {
			f.refuse(w, "not_found")
			return
		}
		delete(f.files[id], body.From)
		f.files[id][body.To] = content
	default:
		f.refuse(w, "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// objectFeed is the journal of one object, newest first.
func (f *fake) objectFeed(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records := slices.Clone(f.events[r.URL.Query().Get("object")])
	if !f.wrongValues {
		slices.Reverse(records)
	}
	items := make([]any, 0, len(records))
	for _, item := range records {
		items = append(items, item)
	}
	f.write(w, http.StatusOK, map[string]any{"items": items, "next": ""})
}

// sinkRecords is what an operator's sink holds, in sequence order.
func (f *fake) sinkRecords(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records := slices.Clone(f.events[r.URL.Query().Get("object")])
	if f.wrongValues {
		slices.Reverse(records)
	}
	items := []any{}
	for _, record := range records {
		items = append(items, record)
	}
	f.write(w, http.StatusOK, items)
}

func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			f.refuse(w, "invalid_field")
			return
		}
		limit = parsed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := keys(f.objects)
	items, next := []any{}, ""
	for _, id := range ids {
		obj := f.objects[id]
		if id <= q.Get("cursor") || (!f.wrongValues && !selected(obj, q)) {
			continue
		}
		if len(items) == limit {
			next = idOf(items[len(items)-1].(map[string]any))
			break
		}
		items = append(items, obj)
	}
	f.write(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// selected is every list selector of design 008, intersected.
func selected(obj map[string]any, q map[string][]string) bool {
	metadata, _ := obj["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	for _, selector := range q["label"] {
		key, value, ok := strings.Cut(selector, "=")
		if !ok {
			return false
		}
		if got, _ := labels[key].(string); got != value {
			return false
		}
	}
	if phase := first(q["phase"]); phase != "" {
		if got, _ := status(obj)["phase"].(string); got != phase {
			return false
		}
	}
	if environment := first(q["environment"]); environment != "" {
		if got, _ := status(obj)["environment"].(string); got != environment {
			return false
		}
	}
	return true
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// applySecret writes a secret and answers it without the value it carried.
func (f *fake) applySecret(w http.ResponseWriter, r *http.Request) {
	body, ok := f.decode(w, r, "Secret")
	if !ok {
		return
	}
	if f.noSecretKey {
		f.refuse(w, "capability_unsupported")
		return
	}
	name := r.PathValue("name")
	f.mu.Lock()
	defer f.mu.Unlock()
	spec, _ := body["spec"].(map[string]any)
	if spec != nil && !f.wrongValues {
		delete(spec, "value")
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	metadata["name"] = name
	if existing, ok := f.secrets[name]; ok {
		body["status"] = existing["status"]
		f.secrets[name] = body
		f.write(w, http.StatusOK, body)
		return
	}
	f.counter++
	body["status"] = map[string]any{"id": fmt.Sprintf("sec_fake%022d", f.counter), "owner": "fake"}
	f.secrets[name] = body
	f.write(w, http.StatusCreated, body)
}

func (f *fake) readSecret(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, obj, ok := f.secretFor(r.PathValue("name"))
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	f.write(w, http.StatusOK, obj)
}

func (f *fake) deleteSecret(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, obj, ok := f.secretFor(r.PathValue("name"))
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	delete(f.secrets, name)
	f.deleted = append(f.deleted, idOf(obj))
	f.write(w, http.StatusOK, obj)
}

// secretFor resolves a secret by its name or by its id, which is the
// addressing every kind of design 008 shares. The caller holds the lock.
func (f *fake) secretFor(ref string) (string, map[string]any, bool) {
	if obj, ok := f.secrets[ref]; ok {
		return ref, obj, true
	}
	for name, obj := range f.secrets {
		if idOf(obj) == ref {
			return name, obj, true
		}
	}
	return "", nil, false
}

func (f *fake) listSecrets(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []any{}
	for _, name := range keys(f.secrets) {
		items = append(items, f.secrets[name])
	}
	f.write(w, http.StatusOK, map[string]any{"items": items, "next": ""})
}

// applyKind, readKind and deleteKind are the other kinds the suite reaches,
// which share the grammar every kind of design 008 shares.
func (f *fake) applyKind(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wanted := map[string]string{"volumes": "Volume", "sandboxsets": "SandboxSet"}[kind]
		body, ok := f.decode(w, r, wanted)
		if !ok {
			return
		}
		name := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		store := f.store4(kind)
		if _, exists := store[name]; exists {
			f.write(w, http.StatusOK, store[name])
			return
		}
		f.counter++
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			body["metadata"] = metadata
		}
		metadata["name"] = name
		body["status"] = map[string]any{"id": fmt.Sprintf("%s_fake%018d", kind[:3], f.counter), "owner": "fake", "phase": "Ready"}
		store[name] = body
		f.write(w, http.StatusCreated, body)
	}
}

func (f *fake) readKind(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		store := f.store4(kind)
		ref := r.PathValue("name")
		if obj, ok := store[ref]; ok {
			f.write(w, http.StatusOK, obj)
			return
		}
		for _, obj := range store {
			if idOf(obj) == ref {
				f.write(w, http.StatusOK, obj)
				return
			}
		}
		f.refuse(w, "not_found")
	}
}

func (f *fake) deleteKind(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		store := f.store4(kind)
		ref := r.PathValue("name")
		for name, obj := range store {
			if name == ref || idOf(obj) == ref {
				delete(store, name)
				f.deleted = append(f.deleted, idOf(obj))
				f.write(w, http.StatusOK, obj)
				return
			}
		}
		f.refuse(w, "not_found")
	}
}

// store4 is the map one kind lives in. The caller holds the lock.
func (f *fake) store4(kind string) map[string]map[string]any {
	if kind == "volumes" {
		return f.volumes
	}
	return f.sets
}

// environments answers the one environment this fake drives, with the
// capabilities it declares.
func (f *fake) environments(w http.ResponseWriter, _ *http.Request) {
	f.write(w, http.StatusOK, map[string]any{"items": []any{map[string]any{
		"apiVersion": APIVersion, "kind": "Environment",
		"metadata": map[string]any{"name": "default"},
		"status": map[string]any{"id": "env_fake", "capabilities": map[string]any{
			"files": true, "attach": true, "display": true, "input": true, "dial": true,
		}},
	}}, "next": ""})
}

// mint is the issuer route a tier takes every subject's token from.
func (f *fake) mint(w http.ResponseWriter, r *http.Request) {
	var asked struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&asked); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch f.mintFailure {
	case "status":
		w.WriteHeader(http.StatusServiceUnavailable)
	case "body":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "this is no token")
	case "empty":
		f.write(w, http.StatusOK, map[string]any{"token": ""})
	default:
		f.write(w, http.StatusOK, map[string]any{"token": "fake-token-" + asked.Sub})
	}
}

// lookup resolves an id or a name. The caller holds the lock.
func (f *fake) lookup(ref string) (map[string]any, bool) {
	if obj, ok := f.objects[ref]; ok {
		return obj, true
	}
	if id, ok := f.names[ref]; ok {
		obj, ok := f.objects[id]
		return obj, ok
	}
	return nil, false
}

// idFor is the id of the object one operation route names. The caller holds
// the lock.
func (f *fake) idFor(r *http.Request) (string, bool) {
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		return "", false
	}
	return idOf(obj), true
}

// status and idOf read the two members every object answer carries.
func status(obj map[string]any) map[string]any {
	s, _ := obj["status"].(map[string]any)
	return s
}

func idOf(obj map[string]any) string {
	id, _ := status(obj)["id"].(string)
	return id
}

// contained is the containment rule of design 004 as a path test.
func contained(p string) bool {
	return p != "" && path.Clean(p) == p && (p == "/workspace" || strings.HasPrefix(p, "/workspace/"))
}

// entryOf is one file entry as design 008 renders it.
func (f *fake) entryOf(p, content string) map[string]any {
	name, size, mode, dir := path.Base(p), len(content), "0644", false
	if f.wrongValues {
		name, size, mode, dir = "another.txt", 0, "0600", true
	}
	return map[string]any{
		"name": name, "path": p, "size": size,
		"mode": mode, "modTime": time.Now().UTC().Format(time.RFC3339), "isDir": dir,
	}
}

// firstEntry reads the first file out of an archive.
func firstEntry(archive []byte) (string, string, error) {
	content, err := tarEntry(archive, "suite.txt")
	if err != nil {
		return "", "", err
	}
	return "suite.txt", content, nil
}

// config is a run against the fake with no capability and no control, which
// is the smallest configuration the suite takes. The case deadline is short
// on purpose: a case whose state the fake never reaches waits for its
// deadline by design, and these tests are about the harness and not about
// how long a case may wait.
func (f *fake) config() Config {
	return Config{URL: f.server.URL, Caller: "fake-token", Timeout: 2 * time.Second}
}
