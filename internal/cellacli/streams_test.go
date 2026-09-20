// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/cellacli"
)

// socketPlane is a server speaking the exec and attach sockets of design
// 008: the first text frame is the request, binary frames are bytes both
// ways, and an exit frame ends the session.
type socketPlane struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	path   string
	req    map[string]any
	input  []byte
	resize []map[string]int
}

func newSocketPlane(t *testing.T, session func(*socketPlane, *websocket.Conn)) *socketPlane {
	t.Helper()
	p := &socketPlane{t: t}
	upgrader := websocket.Upgrader{Subprotocols: []string{"cella.exec.v1"}, CheckOrigin: func(*http.Request) bool { return true }}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.path = r.URL.Path
		p.req = map[string]any{}
		_ = json.Unmarshal(data, &p.req)
		p.mu.Unlock()
		session(p, conn)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// read records what the command sends until it stops.
func (p *socketPlane) read(conn *websocket.Conn) {
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		p.mu.Lock()
		if kind == websocket.BinaryMessage {
			p.input = append(p.input, data...)
		} else {
			var frame struct {
				Resize map[string]int `json:"resize"`
			}
			if json.Unmarshal(data, &frame) == nil && frame.Resize != nil {
				p.resize = append(p.resize, frame.Resize)
			}
		}
		p.mu.Unlock()
	}
}

// waitFor polls a condition the server goroutine fills, so an assertion
// reads what arrived rather than what raced.
func (p *socketPlane) waitFor(ready func() bool) bool {
	for range 200 {
		p.mu.Lock()
		done := ready()
		p.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (p *socketPlane) run(t *testing.T, stdin string, terminal cellacli.Terminal, args ...string) result {
	t.Helper()
	return runWith(t, cellacli.Env{
		Args:     args,
		Stdin:    strings.NewReader(stdin),
		Terminal: terminal,
		Getenv:   environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "caller-token"}),
	})
}

// fakeTerminal stands in for the caller's own terminal, so the rules a
// session follows are tested without one.
type fakeTerminal struct {
	mu       sync.Mutex
	cols     int
	rows     int
	raw      int
	restored int
	failRaw  error
	changed  chan struct{}
	stopped  bool
}

func newFakeTerminal(cols, rows int) *fakeTerminal {
	return &fakeTerminal{cols: cols, rows: rows, changed: make(chan struct{}, 1)}
}

func (f *fakeTerminal) Size() (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows, nil
}

func (f *fakeTerminal) MakeRaw() (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRaw != nil {
		return nil, f.failRaw
	}
	f.raw++
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.restored++
		return nil
	}, nil
}

func (f *fakeTerminal) Resized() (<-chan struct{}, func()) {
	return f.changed, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = true
	}
}

// resize reports a window change and what the new window is.
func (f *fakeTerminal) resize(cols, rows int) {
	f.mu.Lock()
	f.cols, f.rows = cols, rows
	f.mu.Unlock()
	f.changed <- struct{}{}
}

func (f *fakeTerminal) counts() (raw, restored int, stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.raw, f.restored, f.stopped
}

// TestExecWithInputIsTheSocket: -i opens the exec socket, what the caller
// types reaches the command inside, and the exit frame is the exit code.
func TestExecWithInputIsTheSocket(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("from inside\n"))
		p.waitFor(func() bool { return len(p.input) >= len("typed in") })
		_ = conn.WriteJSON(map[string]int{"exit": 5})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	got := p.run(t, "typed in", nil, "exec", "dev", "-i", "--", "cat")
	if got.code != 5 {
		t.Fatalf("exit %d, want the exit frame's 5: %q", got.code, got.stderr)
	}
	if got.stdout != "from inside\n" {
		t.Fatalf("stdout is %q", got.stdout)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path != "/v1/sandboxes/dev/exec" {
		t.Errorf("the session opened %s", p.path)
	}
	if string(p.input) != "typed in" {
		t.Errorf("what the caller typed arrived as %q", p.input)
	}
	if command, _ := p.req["command"].([]any); len(command) != 1 || command[0] != "cat" {
		t.Errorf("the request frame is %v", p.req)
	}
	if _, ok := p.req["cols"]; ok {
		t.Errorf("a command without -t asked for a terminal: %v", p.req)
	}
}

// TestATerminalSessionSetsRawModeAndRestoresIt: -t asks the server for a
// terminal, the caller's own is set raw for the session, the window follows
// it, and it is restored on the way out.
func TestATerminalSessionSetsRawModeAndRestoresIt(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		p.waitFor(func() bool { return len(p.resize) > 0 })
		_ = conn.WriteJSON(map[string]int{"exit": 0})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	terminal := newFakeTerminal(100, 40)
	done := make(chan result, 1)
	go func() { done <- p.run(t, "", terminal, "exec", "dev", "-t", "--", "sh") }()
	// The window changes once the session has asked for its own, which is
	// what a SIGWINCH during a session is.
	if !p.waitFor(func() bool { return p.req != nil }) {
		t.Fatal("the session sent no request frame")
	}
	terminal.resize(120, 50)
	got := <-done
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	raw, restored, stopped := terminal.counts()
	if raw != 1 || restored != 1 {
		t.Fatalf("the terminal was set raw %d time(s) and restored %d", raw, restored)
	}
	if !stopped {
		t.Error("the window watch was not ended")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req["cols"] != float64(100) || p.req["rows"] != float64(40) {
		t.Errorf("the session asked for the window %v", p.req)
	}
	if len(p.resize) == 0 || p.resize[0]["cols"] != 120 || p.resize[0]["rows"] != 50 {
		t.Errorf("the windows that arrived are %v", p.resize)
	}
}

// TestAttachIsATerminalAndCarriesACommand: attach always opens a terminal,
// and the command after -- is the one it runs.
func TestAttachIsATerminalAndCarriesACommand(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("a prompt"))
		_ = conn.WriteJSON(map[string]int{"exit": 0})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	got := p.run(t, "", newFakeTerminal(90, 30), "attach", "dev", "--", "bash", "-l")
	if got.code != 0 || got.stdout != "a prompt" {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path != "/v1/sandboxes/dev/attach" {
		t.Errorf("attach opened %s", p.path)
	}
	if p.req["cols"] != float64(90) || p.req["rows"] != float64(30) {
		t.Errorf("attach asked for the window %v", p.req)
	}
	command, _ := p.req["command"].([]any)
	if len(command) != 2 || command[0] != "bash" || command[1] != "-l" {
		t.Errorf("attach carried the command %v", p.req["command"])
	}
}

// TestASessionWithNoTerminalOfItsOwnStillRunsUnderOne: -t on a pipe asks
// the server for a terminal with a fixed window, because the command inside
// is what -t is about.
func TestASessionWithNoTerminalOfItsOwnStillRunsUnderOne(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteJSON(map[string]int{"exit": 0})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	got := p.run(t, "", nil, "exec", "dev", "-t", "--", "sh")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req["cols"] != float64(80) || p.req["rows"] != float64(24) {
		t.Fatalf("the session asked for the window %v", p.req)
	}
}

// TestATerminalThatCannotBeSetRawEndsTheCommand: a session that could not
// take the terminal does not run half configured.
func TestATerminalThatCannotBeSetRawEndsTheCommand(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteJSON(map[string]int{"exit": 0})
	})
	terminal := newFakeTerminal(80, 24)
	terminal.failRaw = errors.New("this terminal cannot be set raw")
	got := p.run(t, "", terminal, "attach", "dev")
	if got.code != 125 {
		t.Fatalf("exit %d, want 125", got.code)
	}
	if !strings.Contains(got.stderr, "cannot be set raw") {
		t.Fatalf("stderr is %q", got.stderr)
	}
}

// TestASessionUnderJSONWritesItsExitCode: every command answers --json, and
// a session's answer is the code the command inside ended with.
func TestASessionUnderJSONWritesItsExitCode(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteJSON(map[string]int{"exit": 9})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	})
	got := p.run(t, "", nil, "exec", "dev", "-i", "--json", "--", "sh")
	if got.code != 9 {
		t.Fatalf("exit %d, want 9", got.code)
	}
	if strings.TrimSpace(got.stdout) != `{"exitCode":9}` {
		t.Fatalf("stdout is %q", got.stdout)
	}
}

// TestASessionThatFailsMidStreamIs125: a connection that went away after
// the command had begun is not a clean exit.
func TestASessionThatFailsMidStreamIs125(t *testing.T) {
	p := newSocketPlane(t, func(p *socketPlane, conn *websocket.Conn) {
		go p.read(conn)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("half a line"))
		_ = conn.Close()
	})
	got := p.run(t, "", nil, "exec", "dev", "-i", "--", "sh")
	if got.code != 125 {
		t.Fatalf("exit %d, want 125: %q", got.code, got.stderr)
	}
	if got.stdout != "half a line" {
		t.Fatalf("what arrived before the failure is %q", got.stdout)
	}
}
