// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a libpod engine the driver's own tests drive: the endpoints this
// driver calls, over a unix socket, with the volumes, containers, images and
// exec sessions held in memory. It runs the suite with no podman installed,
// which is what the hermetic gate asks for, and it is where every refusal the
// real engine would rarely produce is arranged on purpose.
type fake struct {
	socket string

	mu         sync.Mutex
	volumes    map[string]map[string]string
	containers map[string]*fakeContainer
	images     map[string]string
	execs      map[string]*fakeExec
	pulls      int

	// faults maps "METHOD /path-prefix" onto the refusal a matching request
	// gets, so an error branch is reached on purpose.
	faults map[string]*arranged
	// run answers an exec session. Nil runs a command that writes nothing
	// and exits 0.
	run func(cmd []string, env []string, dir string) fakeExecResult
	// special names a path the archive endpoint emits as a symbolic link, so
	// the refusal of a file an export does not carry is reachable.
	special string
}

type fakeContainer struct {
	labels     map[string]string
	command    []string
	env        map[string]string
	workdir    string
	user       string
	volumes    []namedVolume
	limits     *resourceLimits
	state      string
	exitCode   int
	startedAt  time.Time
	finishedAt time.Time
	files      map[string]fakeFile
	logs       []fakeLine
	// released is closed to end a followed log stream.
	released chan struct{}
}

type fakeFile struct {
	mode     int64
	uid, gid int
	body     []byte
	dir      bool
}

type fakeLine struct {
	at     time.Time
	stream byte
	data   string
}

type fakeExecResult struct {
	stdout, stderr string
	code           int
	block          bool
}

type fakeExec struct {
	container string
	cmd       []string
	env       []string
	dir       string
	tty       bool
	stdin     bool
	result    fakeExecResult
	running   bool
	// window is the last size a resize set, as rows then columns.
	window [2]int
	// typed is everything the client wrote into the session's input.
	typed []byte
}

// newFake starts an engine on a unix socket in a directory of its own, removed
// when the test ends. The directory is not t.TempDir: a unix socket path is
// bounded at around a hundred bytes and a test name in the path overruns it.
func newFake(t *testing.T) *fake {
	t.Helper()
	dir, err := os.MkdirTemp("", "cp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fake{
		socket:     path.Join(dir, "s"),
		volumes:    map[string]map[string]string{},
		containers: map[string]*fakeContainer{},
		images:     map[string]string{"img": "sha256:d1", "docker.io/library/alpine:latest": "sha256:a1"},
		execs:      map[string]*fakeExec{},
		faults:     map[string]*arranged{},
	}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", f.socket, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: f, ReadHeaderTimeout: time.Second}}
	srv.Start()
	t.Cleanup(srv.Close)
	return f
}

// driver opens a driver against this engine, already preflighted.
func (f *fake) driver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(Options{Socket: f.socket, PullTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// arranged is one refusal waiting to happen: skip matching requests, then
// refuse the next with status.
type arranged struct {
	status, skip int
}

// fault refuses the next request whose method and path start with key.
func (f *fake) fault(key string, status int) { f.faultAfter(key, status, 0) }

// faultAfter refuses the request after skip matching ones, which is how a step
// that repeats an endpoint, such as the second volume create of one Create, is
// singled out.
func (f *fake) faultAfter(key string, status, skip int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[key] = &arranged{status: status, skip: skip}
}

// containerCount is how many containers the engine holds.
func (f *fake) containerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

func (f *fake) container(t *testing.T, id string) *fakeContainer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[containerName(id)]
	if c == nil {
		t.Fatalf("no container for %s", id)
	}
	return c
}

func (f *fake) labelsOf(t *testing.T, volume string) map[string]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.volumes[volume]
	if !ok {
		t.Fatalf("no volume %s", volume)
	}
	return maps.Clone(l)
}

// containerOf is the container the engine holds for one sandbox.
func (f *fake) containerOf(t *testing.T, id string) *fakeContainer {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[containerName(id)]
	if !ok {
		t.Fatalf("no container for %s", id)
	}
	return c
}

// volumeNames lists every volume the engine holds, sorted.
func (f *fake) volumeNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.volumes))
}

const libpod = "/v" + apiVersion + "/libpod"
const compat = "/v" + apiVersion

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if status := f.takeFault(r); status != 0 {
		refuse(w, status, "arranged fault")
		return
	}
	p := r.URL.Path
	switch {
	case p == libpod+"/info":
		writeJSON(w, map[string]string{"version": "test"})
	case p == libpod+"/volumes/create":
		f.createVolume(w, r)
	case p == libpod+"/volumes/json":
		f.listVolumes(w, r)
	case strings.HasPrefix(p, libpod+"/volumes/"):
		f.oneVolume(w, r, strings.TrimPrefix(p, libpod+"/volumes/"))
	case p == libpod+"/containers/create":
		f.createContainer(w, r)
	case p == libpod+"/containers/json":
		f.listContainers(w, r)
	case strings.HasPrefix(p, libpod+"/containers/"):
		f.oneContainer(w, r, strings.TrimPrefix(p, libpod+"/containers/"))
	case strings.HasPrefix(p, libpod+"/images/"):
		f.image(w, r, strings.TrimPrefix(p, libpod+"/images/"))
	case strings.HasPrefix(p, libpod+"/exec/"):
		f.exec(w, r, strings.TrimPrefix(p, libpod+"/exec/"))
	case strings.HasPrefix(p, compat+"/containers/") && strings.HasSuffix(p, "/archive"):
		f.archive(w, r, strings.TrimSuffix(strings.TrimPrefix(p, compat+"/containers/"), "/archive"))
	default:
		refuse(w, http.StatusNotFound, "no route for "+p)
	}
}

func (f *fake) takeFault(r *http.Request) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, a := range f.faults {
		method, prefix, _ := strings.Cut(key, " ")
		if r.Method != method || !strings.HasPrefix(r.URL.Path, prefix) {
			continue
		}
		if a.skip > 0 {
			a.skip--
			return 0
		}
		delete(f.faults, key)
		return a.status
	}
	return 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": message, "response": status})
}

// ---- volumes ----

func (f *fake) createVolume(w http.ResponseWriter, r *http.Request) {
	var in volumeCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[in.Name]; ok {
		// Podman answers a duplicate with 500 and this sentence, not 409.
		refuse(w, http.StatusInternalServerError, "volume with name "+in.Name+" already exists")
		return
	}
	f.volumes[in.Name] = maps.Clone(in.Labels)
	writeJSON(w, volumeInspect(in))
}

func (f *fake) listVolumes(w http.ResponseWriter, r *http.Request) {
	want := filterLabels(r.URL.Query().Get("filters"))
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []volumeInspect{}
	for _, name := range slices.Sorted(maps.Keys(f.volumes)) {
		labels := f.volumes[name]
		if matches(labels, want) {
			out = append(out, volumeInspect{Name: name, Labels: labels})
		}
	}
	writeJSON(w, out)
}

func (f *fake) oneVolume(w http.ResponseWriter, r *http.Request, rest string) {
	name := strings.TrimSuffix(rest, "/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	labels, ok := f.volumes[name]
	if !ok {
		refuse(w, http.StatusNotFound, "no such volume "+name)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(f.volumes, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, volumeInspect{Name: name, Labels: labels})
	}
}

// filterLabels reads the list endpoints' {"label":["k","k=v"]} selector.
func filterLabels(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	var in map[string][]string
	if json.Unmarshal([]byte(raw), &in) != nil {
		return nil
	}
	out := map[string]string{}
	for _, entry := range in["label"] {
		k, v, _ := strings.Cut(entry, "=")
		out[k] = v
	}
	return out
}

// matches applies a selector where an empty value means the key alone.
func matches(labels, want map[string]string) bool {
	for k, v := range want {
		got, ok := labels[k]
		if !ok || v != "" && got != v {
			return false
		}
	}
	return true
}

// ---- containers ----

func (f *fake) createContainer(w http.ResponseWriter, r *http.Request) {
	var sg specGenerator
	if err := json.NewDecoder(r.Body).Decode(&sg); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[sg.Image]; !ok {
		refuse(w, http.StatusNotFound, "no such image "+sg.Image)
		return
	}
	if _, ok := f.containers[sg.Name]; ok {
		refuse(w, http.StatusConflict, "container "+sg.Name+" already exists")
		return
	}
	f.containers[sg.Name] = &fakeContainer{
		labels: maps.Clone(sg.Labels), command: sg.Command, env: maps.Clone(sg.Env),
		workdir: sg.WorkDir, user: sg.User, volumes: sg.Volumes, limits: sg.ResourceLimits,
		state: "created", files: map[string]fakeFile{}, released: make(chan struct{}),
	}
	writeJSON(w, map[string]string{"Id": sg.Name})
}

func (f *fake) listContainers(w http.ResponseWriter, r *http.Request) {
	want := filterLabels(r.URL.Query().Get("filters"))
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []containerListItem{}
	for _, name := range slices.Sorted(maps.Keys(f.containers)) {
		c := f.containers[name]
		if !matches(c.labels, want) {
			continue
		}
		item := containerListItem{State: c.state, ExitCode: c.exitCode, Labels: c.labels}
		if !c.startedAt.IsZero() {
			item.StartedAt = c.startedAt.Unix()
		}
		if !c.finishedAt.IsZero() {
			item.Exited, item.ExitedAt = true, c.finishedAt.Unix()
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}

func (f *fake) oneContainer(w http.ResponseWriter, r *http.Request, rest string) {
	name, verb, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	c := f.containers[name]
	if c == nil {
		f.mu.Unlock()
		refuse(w, http.StatusNotFound, "no such container "+name)
		return
	}
	switch {
	case verb == "json":
		var ci containerInspect
		ci.State.Status, ci.State.ExitCode = c.state, c.exitCode
		ci.State.StartedAt, ci.State.FinishedAt = c.startedAt, c.finishedAt
		ci.Config.User = c.user
		f.mu.Unlock()
		writeJSON(w, ci)
	case verb == "start":
		if c.state == "running" {
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.state, c.startedAt, c.finishedAt, c.exitCode = "running", time.Now().UTC(), time.Time{}, 0
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(verb, "stop"):
		if c.state != "running" {
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.state, c.exitCode, c.finishedAt = "exited", 137, time.Now().UTC()
		close(c.released)
		c.released = make(chan struct{})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case verb == "exec":
		f.mu.Unlock()
		f.createExec(w, r, name)
	case strings.HasPrefix(verb, "logs"):
		f.mu.Unlock()
		f.logs(w, r, name)
	case r.Method == http.MethodDelete:
		delete(f.containers, name)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Unlock()
		refuse(w, http.StatusNotFound, "no verb "+verb)
	}
}

// exit ends a container as its own process would, which is how the tests
// reach the phases a stop does not produce.
func (f *fake) exit(t *testing.T, id string, code int) {
	t.Helper()
	c := f.container(t, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	c.state, c.exitCode, c.finishedAt = "exited", code, time.Now().UTC()
	close(c.released)
	c.released = make(chan struct{})
}

// ---- images ----

func (f *fake) image(w http.ResponseWriter, r *http.Request, rest string) {
	if strings.HasPrefix(rest, "pull") {
		f.mu.Lock()
		f.pulls++
		ref := r.URL.Query().Get("reference")
		if ref != "unpullable" {
			f.images[ref] = "sha256:pulled"
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"done"}`)
		return
	}
	name := strings.TrimSuffix(rest, "/json")
	f.mu.Lock()
	digest, ok := f.images[name]
	f.mu.Unlock()
	if !ok {
		refuse(w, http.StatusNotFound, "no such image "+name)
		return
	}
	writeJSON(w, map[string]string{"Digest": digest})
}

// ---- exec ----

func (f *fake) createExec(w http.ResponseWriter, r *http.Request, container string) {
	var in execCreate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	result := fakeExecResult{}
	if f.run != nil {
		result = f.run(in.Cmd, in.Env, in.WorkingDir)
	}
	id := "exec" + strconv.Itoa(len(f.execs)+1)
	f.execs[id] = &fakeExec{container: container, cmd: in.Cmd, env: in.Env, dir: in.WorkingDir, tty: in.Tty, stdin: in.AttachStdin, result: result, running: true}
	writeJSON(w, map[string]string{"Id": id})
}

func (f *fake) exec(w http.ResponseWriter, r *http.Request, rest string) {
	id, verb, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	e := f.execs[id]
	f.mu.Unlock()
	if e == nil {
		refuse(w, http.StatusNotFound, "no such exec "+id)
		return
	}
	switch verb {
	case "start":
		if e.tty || e.stdin {
			f.hijackExec(w, r, e)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(frame(1, e.result.stdout))
		_, _ = w.Write(frame(2, e.result.stderr))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if e.result.block {
			<-r.Context().Done()
			return
		}
		f.mu.Lock()
		e.running = false
		f.mu.Unlock()
	case "json":
		f.mu.Lock()
		out := execInspect{ExitCode: e.result.code, Running: e.running}
		f.mu.Unlock()
		writeJSON(w, out)
	case "resize":
		rows, _ := strconv.Atoi(r.URL.Query().Get("h"))
		cols, _ := strconv.Atoi(r.URL.Query().Get("w"))
		f.mu.Lock()
		e.window = [2]int{rows, cols}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		refuse(w, http.StatusNotFound, "no verb "+verb)
	}
}

// hijackExec answers an upgrade with the raw stream the engine speaks a
// terminal over. A session the arrangement blocks is interactive: it echoes
// what is typed, as a terminal does, until the client goes away. One that does
// not block runs once: it takes the input the client half-closed, writes the
// arranged output, and ends. A TTY stream is unframed, as podman's is; without
// one the framing stays.
func (f *fake) hijackExec(w http.ResponseWriter, r *http.Request, e *fakeExec) {
	// The request body is drained before the connection is taken over, or the
	// bytes still in the reader would be read back as the session's input.
	_, _ = io.Copy(io.Discard, r.Body)
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		refuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	if err = brw.Flush(); err != nil {
		return
	}
	if e.result.block {
		f.echo(e, brw)
		return
	}
	if e.stdin {
		typed, _ := io.ReadAll(brw)
		f.mu.Lock()
		e.typed = typed
		f.mu.Unlock()
	}
	if e.tty {
		_, _ = brw.WriteString(e.result.stdout + e.result.stderr)
	} else {
		_, _ = brw.Write(frame(1, e.result.stdout))
		_, _ = brw.Write(frame(2, e.result.stderr))
	}
	_ = brw.Flush()
	f.mu.Lock()
	e.running = false
	f.mu.Unlock()
}

// echo is the interactive half: every byte typed is recorded and written back.
func (f *fake) echo(e *fakeExec, brw *bufio.ReadWriter) {
	b := make([]byte, 4096)
	for {
		n, err := brw.Read(b)
		if n > 0 {
			f.mu.Lock()
			e.typed = append(e.typed, b[:n]...)
			f.mu.Unlock()
			if _, werr := brw.Write(b[:n]); werr != nil {
				break
			}
			if brw.Flush() != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	f.mu.Lock()
	e.running = false
	f.mu.Unlock()
}

// typedInto is everything the client wrote into a session.
func (f *fake) typedInto(e *fakeExec) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(e.typed)
}

// windowOf is the last size a resize set on a session, as rows then columns.
func (f *fake) windowOf(e *fakeExec) [2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return e.window
}

// session is the one exec session the engine holds, for a case that asserts
// what was created, resized or typed.
func (f *fake) session(t *testing.T) *fakeExec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execs) != 1 {
		t.Fatalf("the engine holds %d exec sessions, want one", len(f.execs))
	}
	for _, e := range f.execs {
		return e
	}
	return nil
}

// frame renders one payload in podman's stream framing.
func frame(stream byte, body string) []byte {
	if body == "" {
		return nil
	}
	out := make([]byte, 8, 8+len(body))
	out[0] = stream
	binary.BigEndian.PutUint32(out[4:], uint32(len(body)))
	return append(out, body...)
}

// ---- logs ----

func (f *fake) logs(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	f.mu.Lock()
	c := f.containers[name]
	lines := slices.Clone(c.logs)
	released := c.released
	running := c.state == "running"
	f.mu.Unlock()
	if since := q.Get("since"); since != "" {
		at, err := strconv.ParseInt(since, 10, 64)
		if err != nil {
			refuse(w, http.StatusBadRequest, "bad since")
			return
		}
		lines = slices.DeleteFunc(lines, func(l fakeLine) bool { return l.at.Unix() < at })
	}
	if tail := q.Get("tail"); tail != "" {
		n, err := strconv.Atoi(tail)
		if err != nil {
			refuse(w, http.StatusBadRequest, "bad tail")
			return
		}
		if n < len(lines) {
			lines = lines[len(lines)-n:]
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	written := len(lines)
	for _, l := range lines {
		_, _ = w.Write(frame(l.stream, l.data))
	}
	if q.Get("follow") != "true" || !running {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	select {
	case <-released:
	case <-r.Context().Done():
		return
	}
	f.mu.Lock()
	rest := slices.Clone(c.logs[min(written, len(c.logs)):])
	f.mu.Unlock()
	for _, l := range rest {
		_, _ = w.Write(frame(l.stream, l.data))
	}
}

// ---- archive ----

func (f *fake) archive(w http.ResponseWriter, r *http.Request, name string) {
	target := r.URL.Query().Get("path")
	f.mu.Lock()
	c := f.containers[name]
	f.mu.Unlock()
	if c == nil {
		refuse(w, http.StatusNotFound, "no such container "+name)
		return
	}
	switch r.Method {
	case http.MethodPut:
		f.mu.Lock()
		defer f.mu.Unlock()
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				w.WriteHeader(http.StatusOK)
				return
			}
			if err != nil {
				refuse(w, http.StatusBadRequest, err.Error())
				return
			}
			full := path.Join(target, strings.TrimSuffix(h.Name, "/"))
			if h.Typeflag == tar.TypeDir {
				c.files[full] = fakeFile{mode: h.Mode, dir: true}
				continue
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				refuse(w, http.StatusBadRequest, err.Error())
				return
			}
			c.files[full] = fakeFile{mode: h.Mode, uid: h.Uid, gid: h.Gid, body: body}
		}
	case http.MethodGet:
		f.mu.Lock()
		defer f.mu.Unlock()
		names := []string{}
		for n := range c.files {
			if n == target || strings.HasPrefix(n, target+"/") {
				names = append(names, n)
			}
		}
		if len(names) == 0 && target != rootOf(c) {
			refuse(w, http.StatusNotFound, "no such path "+target)
			return
		}
		slices.Sort(names)
		w.Header().Set("Content-Type", "application/x-tar")
		tw := tar.NewWriter(w)
		base := path.Base(target)
		// Podman emits the asked-for path as its own first entry, named after
		// its base, and every entry below it under that name.
		_ = tw.WriteHeader(&tar.Header{Name: base + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		for _, n := range names {
			file := c.files[n]
			entry := path.Join(base, strings.TrimPrefix(strings.TrimPrefix(n, target), "/"))
			if file.dir {
				_ = tw.WriteHeader(&tar.Header{Name: entry + "/", Mode: file.mode, Typeflag: tar.TypeDir})
				continue
			}
			if n == f.special {
				_ = tw.WriteHeader(&tar.Header{Name: entry, Mode: file.mode, Typeflag: tar.TypeSymlink, Linkname: "elsewhere"})
				continue
			}
			_ = tw.WriteHeader(&tar.Header{Name: entry, Mode: file.mode, Size: int64(len(file.body)), Typeflag: tar.TypeReg})
			_, _ = tw.Write(file.body)
		}
		_ = tw.Close()
	default:
		refuse(w, http.StatusMethodNotAllowed, r.Method)
	}
}

// rootOf is where the container's workspace volume is mounted, which always
// exists whether or not a file has been written to it.
func rootOf(c *fakeContainer) string {
	if len(c.volumes) > 0 {
		return c.volumes[0].Dest
	}
	return ""
}

// line records one log entry the container wrote.
func line(at time.Time, stream byte, text string) fakeLine {
	return fakeLine{at: at, stream: stream, data: text}
}
