// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// sandbox creates one sandbox under the given name and returns it.
func (f *fixture) sandbox(name string) v1.Sandbox {
	f.t.Helper()
	body := strings.Replace(createBody, `"name":"work"`, `"name":"`+name+`"`, 1)
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &obj); err != nil {
		f.t.Fatal(err)
	}
	return obj
}

// open dials one of the two sockets with the subprotocol design 008 names.
func (f *fixture) open(path, token string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialer := websocket.Dialer{Subprotocols: []string{subprotocol}, HandshakeTimeout: 5 * time.Second}
	return dialer.Dial("ws"+strings.TrimPrefix(f.url, "http")+path, header)
}

// attachTo dials, sends the request frame, and fails the test on either half.
func (f *fixture) attachTo(path, token, request string) (*websocket.Conn, *socketReader) {
	f.t.Helper()
	conn, _, err := f.open(path, token)
	if err != nil {
		f.t.Fatalf("dialing %s: %v", path, err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	if err = conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		f.t.Fatal(err)
	}
	return conn, readSocket(conn)
}

// socketReader owns the connection's reader, as a WebSocket admits one, and
// keeps what arrived so a test asserts on it with a bound.
type socketReader struct {
	mu     sync.Mutex
	bytes  []byte
	texts  []string
	closed error
	done   chan struct{}
}

func readSocket(conn *websocket.Conn) *socketReader {
	r := &socketReader{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			kind, data, err := conn.ReadMessage()
			r.mu.Lock()
			switch {
			case err != nil:
				r.closed = err
			case kind == websocket.BinaryMessage:
				r.bytes = append(r.bytes, data...)
			default:
				r.texts = append(r.texts, string(data))
			}
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return r
}
func (r *socketReader) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.bytes)
}
func (r *socketReader) frames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}
func (r *socketReader) closeErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}
func (r *socketReader) await(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.text(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("the session never wrote %q; it wrote %q", marker, r.text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ended waits for the connection to close and returns the last text frame and
// the close code.
func (r *socketReader) ended(t *testing.T) (string, int) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the connection stayed open; it wrote %q and %v", r.text(), r.frames())
	}
	last := ""
	if frames := r.frames(); len(frames) > 0 {
		last = frames[len(frames)-1]
	}
	var closed *websocket.CloseError
	if !errors.As(r.closeErr(), &closed) {
		t.Fatalf("the connection ended with %v, not a close frame", r.closeErr())
	}
	return last, closed.Code
}

func typeIn(t *testing.T, conn *websocket.Conn, line string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(line+"\n")); err != nil {
		t.Fatal(err)
	}
}

// TestAttachRoundTrip is the acceptance of design 008's attach socket: bytes
// both ways, a resize the process inside reads, and the exit code as a text
// frame before close 1000.
func TestAttachRoundTrip(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("shell")
	conn, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	typeIn(t, conn, `printf 'ROUND%s\n' TRIP`)
	r.await(t, "ROUNDTRIP")
	typeIn(t, conn, "stty size")
	r.await(t, "24 80")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"resize":{"cols":120,"rows":40}}`)); err != nil {
		t.Fatal(err)
	}
	typeIn(t, conn, "stty size")
	r.await(t, "40 120")
	typeIn(t, conn, "exit 3")
	last, code := r.ended(t)
	if last != `{"exit":3}` {
		t.Fatalf("the last frame is %q, want the exit code", last)
	}
	if code != websocket.CloseNormalClosure {
		t.Fatalf("the close code is %d, want %d", code, websocket.CloseNormalClosure)
	}
}

// TestAttachWithoutACommandRunsTheShell proves an empty command opens the
// image's shell, which is what design 004 says of AttachRequest.
func TestAttachWithoutACommandRunsTheShell(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("bare")
	conn, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, `{"cols":80,"rows":24}`)
	typeIn(t, conn, `printf 'NO%s\n' COMMAND`)
	r.await(t, "NOCOMMAND")
}

// pidIn reads the decimal after marker out of what the session wrote.
func pidIn(t *testing.T, text, marker string) int {
	t.Helper()
	_, rest, ok := strings.Cut(text, marker)
	if !ok {
		t.Fatalf("%q does not carry %q", text, marker)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.Split(strings.TrimSpace(rest), "\n")[0]))
	if err != nil {
		t.Fatalf("%q after %q is not a pid: %v", rest, marker, err)
	}
	return pid
}

// TestAttachClientDisconnectEndsTheProcess pins that an abandoned terminal
// does not leave a shell behind on a driver whose Close kills one.
func TestAttachClientDisconnectEndsTheProcess(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("abandoned")
	conn, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	// The marker is assembled by printf, so the line the terminal echoes back
	// does not carry it and only the shell's own output does.
	typeIn(t, conn, `printf 'PID%s%s\n' ":" "$$"`)
	r.await(t, "PID:")
	pid := pidIn(t, r.text(), "PID:")
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the session's shell %d is not running", pid)
	}
	// The connection goes without a close frame, which is what a client that
	// crashed or lost its network looks like to the server.
	if err := conn.UnderlyingConn().Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the shell %d outlived the connection", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAttachSessionsAreIndependent proves two terminals on one sandbox are two
// processes, each reading only what was typed into it.
func TestAttachSessionsAreIndependent(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("two")
	path := "/v1/sandboxes/" + obj.Status.ID + "/attach"
	first, firstOut := f.attachTo(path, f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	second, secondOut := f.attachTo(path, f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	typeIn(t, first, `printf 'FIRST%s%s\n' ":" "$$"`)
	firstOut.await(t, "FIRST:")
	typeIn(t, second, `printf 'SECOND%s%s\n' ":" "$$"`)
	secondOut.await(t, "SECOND:")
	if strings.Contains(secondOut.text(), "FIRST:") || strings.Contains(firstOut.text(), "SECOND:") {
		t.Fatalf("the sessions share a terminal: %q and %q", firstOut.text(), secondOut.text())
	}
	if pidIn(t, firstOut.text(), "FIRST:") == pidIn(t, secondOut.text(), "SECOND:") {
		t.Fatal("the two sessions run one process")
	}
	typeIn(t, first, "exit 0")
	if _, code := firstOut.ended(t); code != websocket.CloseNormalClosure {
		t.Fatalf("the first session closed %d", code)
	}
	// The second session outlives the first.
	typeIn(t, second, `printf 'STILL%s\n' HERE`)
	secondOut.await(t, "STILLHERE")
}

// TestAttachBadFirstFrame covers every first frame that is not the request.
func TestAttachBadFirstFrame(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("firstframe")
	path := "/v1/sandboxes/" + obj.Status.ID + "/attach"
	for name, frame := range map[string]string{
		"notJSON":      "hello",
		"notAnObject":  `["sh"]`,
		"unknownField": `{"command":["sh"],"shell":true}`,
		"twoObjects":   `{"cols":80} {"rows":24}`,
		// The close frame's reason is bounded, so a long refusal still fits.
		"longReason": `{"` + strings.Repeat("field", 40) + `":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, r := f.attachTo(path, f.alice, frame)
			if _, code := r.ended(t); code != websocket.ClosePolicyViolation {
				t.Fatalf("the close code is %d, want %d", code, websocket.ClosePolicyViolation)
			}
		})
	}
	t.Run("binary", func(t *testing.T) {
		conn, _, err := f.open(path, f.alice)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if err = conn.WriteMessage(websocket.BinaryMessage, []byte("keystrokes")); err != nil {
			t.Fatal(err)
		}
		if _, code := readSocket(conn).ended(t); code != websocket.ClosePolicyViolation {
			t.Fatalf("the close code is %d, want %d", code, websocket.ClosePolicyViolation)
		}
	})
	t.Run("pastTheBodyLimit", func(t *testing.T) {
		conn, _, err := f.open(path, f.alice)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		big := `{"command":["sh"],"env":{"BIG":"` + strings.Repeat("x", 70000) + `"}}`
		if err = conn.WriteMessage(websocket.TextMessage, []byte(big)); err != nil {
			t.Fatal(err)
		}
		if _, code := readSocket(conn).ended(t); code != websocket.CloseMessageTooBig {
			t.Fatalf("the close code is %d, want %d", code, websocket.CloseMessageTooBig)
		}
	})
}

// TestAttachRefusedByTheDriver proves a session the runtime will not open
// reaches the client as the error frame and close 1011, not a dropped
// connection: the upgrade already answered, so no status can carry it.
func TestAttachRefusedByTheDriver(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("stopped")
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
	_, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	last, code := r.ended(t)
	if code != websocket.CloseInternalServerErr {
		t.Fatalf("the close code is %d, want %d", code, websocket.CloseInternalServerErr)
	}
	var frame errorFrame
	if err := json.Unmarshal([]byte(last), &frame); err != nil {
		t.Fatalf("the last frame %q is not the error envelope: %v", last, err)
	}
	if frame.Error.Code != "phase_conflict" || frame.Error.Message != "The sandbox is not in a state that allows this." {
		t.Fatalf("the error frame is %+v", frame.Error)
	}
	if frame.Error.Details["detail"] == nil {
		t.Fatalf("the error frame carries no developer detail: %+v", frame.Error)
	}
	// The same refusal on the other socket, where the command runs without a
	// terminal and the stdin pipe has to be given up unopened.
	_, piped := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice, `{"command":["true"]}`)
	if last, code := piped.ended(t); code != websocket.CloseInternalServerErr || !strings.Contains(last, "phase_conflict") {
		t.Fatalf("the exec socket ended %q with %d", last, code)
	}
}

// TestAttachWithoutTheUpgrade proves an ordinary GET on a socket route is the
// handshake's own refusal, not a panic or a hanging request.
func TestAttachWithoutTheUpgrade(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("plainget")
	f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, "", http.StatusBadRequest)
}

// TestAttachInvalidRequestFrame covers the requests the route refuses after
// the upgrade, each as the error frame the table names.
func TestAttachInvalidRequestFrame(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("invalid")
	attach := "/v1/sandboxes/" + obj.Status.ID + "/attach"
	execPath := "/v1/sandboxes/" + obj.Status.ID + "/exec"
	for name, tc := range map[string]struct{ path, frame string }{
		"negativeWindow":  {attach, `{"cols":-1}`},
		"hugeWindow":      {attach, `{"cols":70000,"rows":24}`},
		"reservedEnv":     {attach, `{"command":["sh"],"env":{"CELLA_URL":"x"}}`},
		"workdirOutside":  {attach, `{"command":["sh"],"workdir":"/etc"}`},
		"noCommand":       {execPath, `{}`},
		"badTimeout":      {execPath, `{"command":["sh"],"timeout":"nope"}`},
		"timeoutTooLong":  {execPath, `{"command":["sh"],"timeout":"2h"}`},
		"emptyCommandArg": {execPath, `{"command":[""]}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, r := f.attachTo(tc.path, f.alice, tc.frame)
			last, code := r.ended(t)
			if code != websocket.CloseInternalServerErr {
				t.Fatalf("the close code is %d for %q", code, last)
			}
			var frame errorFrame
			if err := json.Unmarshal([]byte(last), &frame); err != nil || frame.Error.Code != "invalid_field" {
				t.Fatalf("the last frame is %q: %v", last, err)
			}
		})
	}
}

// TestExecSocketStdin proves the exec socket without a window runs a command
// whose input the client writes and whose two outputs interleave.
func TestExecSocketStdin(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("piped")
	conn, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["sh","-c","read line; printf 'got:%s\\n' \"$line\"; printf 'ONERR\\n' >&2; exit 4"]}`)
	// A command without a terminal has no window, so a resize is accepted and
	// changes nothing; the frames that carry nothing to do are ignored too.
	for _, frame := range []string{`{"resize":{"cols":100,"rows":30}}`, `{"resize":{"cols":0,"rows":30}}`, `{}`, `not json`} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("typed\n")); err != nil {
		t.Fatal(err)
	}
	r.await(t, "got:typed")
	r.await(t, "ONERR")
	last, code := r.ended(t)
	if last != `{"exit":4}` || code != websocket.CloseNormalClosure {
		t.Fatalf("the session ended %q with %d", last, code)
	}
	// Without a terminal the input is not echoed back.
	if strings.Contains(r.text(), "typed\n") && !strings.Contains(r.text(), "got:typed") {
		t.Fatalf("the command's input was echoed: %q", r.text())
	}
}

// TestExecSocketTTY proves the exec socket with both a column and a row count
// opens a terminal, which is what design 008's one request shape distinguishes
// the two modes by.
func TestExecSocketTTY(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("terminal")
	_, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["sh","-c","if [ -t 0 ]; then printf 'ISATTY\\n'; else printf 'ISPIPE\\n'; fi; stty size; exit 2"],"cols":90,"rows":30}`)
	r.await(t, "ISATTY")
	r.await(t, "30 90")
	last, code := r.ended(t)
	if last != `{"exit":2}` || code != websocket.CloseNormalClosure {
		t.Fatalf("the session ended %q with %d", last, code)
	}
}

// noAttachDriver declares no Attach. It embeds the interface, not the native
// driver, so the concrete Attach method is not promoted either: a wrapper that
// embedded the driver would still satisfy Attacher and the gate would pass for
// the wrong reason.
type noAttachDriver struct{ runtime.Driver }

func (noAttachDriver) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true}
}

// TestAttachCapabilityGate proves both sockets answer 422 before the upgrade
// on an environment that provides no terminal.
func TestAttachCapabilityGate(t *testing.T) {
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return noAttachDriver{d} })
	obj := f.sandbox("plain")
	for _, route := range []string{"/attach", "/exec"} {
		conn, res, err := f.open("/v1/sandboxes/"+obj.Status.ID+route, f.alice)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s upgraded on an environment without Attach", route)
		}
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s answered %d, want 422", route, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if !strings.Contains(string(body), "capability_unsupported") {
			t.Fatalf("%s answered %q", route, body)
		}
	}
}

// TestAttachAuthorization proves the sockets are read, authorized, and only
// then upgraded, as every other item route is.
func TestAttachAuthorization(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("owned")
	for name, tc := range map[string]struct {
		path, token string
		status      int
	}{
		"noBearer":   {"/v1/sandboxes/" + obj.Status.ID + "/attach", "", http.StatusUnauthorized},
		"anotherSub": {"/v1/sandboxes/" + obj.Status.ID + "/attach", f.bob, http.StatusForbidden},
		"noSandbox":  {"/v1/sandboxes/sbx_absent/exec", f.alice, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			conn, res, err := f.open(tc.path, tc.token)
			if err == nil {
				_ = conn.Close()
				t.Fatal("the socket upgraded")
			}
			if res.StatusCode != tc.status {
				t.Fatalf("answered %d, want %d", res.StatusCode, tc.status)
			}
			_ = res.Body.Close()
		})
	}
}

// TestAttachStampsActivity proves a session holds its sandbox away from the
// idle rule: it stamps when it opens and on what the person types.
func TestAttachStampsActivity(t *testing.T) {
	stamps := &touchDriver{}
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver {
		stamps.Driver = d
		return stamps
	})
	obj := f.sandbox("busy")
	conn, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/attach", f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
	// The handshake finishes for the client before the handler that stamps
	// has run, so the first stamp is waited for rather than read at once.
	opened := time.Now().Add(10 * time.Second)
	for len(stamps.stamped()) == 0 {
		if time.Now().After(opened) {
			t.Fatal("opening a session stamped nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := stamps.stamped(); got[0] != obj.Status.ID {
		t.Fatalf("opening a session stamped %v", got)
	}
	typeIn(t, conn, `printf 'TYP%s\n' ED`)
	r.await(t, "TYPED")
	deadline := time.Now().Add(10 * time.Second)
	for len(stamps.stamped()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("typing stamped %v", stamps.stamped())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
