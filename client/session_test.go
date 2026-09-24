// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/client"
)

// The socket fixture speaks the frames of internal/api/attach.go: the
// subprotocol, the first text frame carrying the request, binary frames of
// bytes both ways, a resize as text from the client, and an exit or an error
// frame followed by a close. It is written here rather than imported
// because the server's own is unexported, and every assertion below is
// against the shape that file writes.
const subprotocol = "cella.exec.v1"

// socketRequest is the first text frame, as the server decodes it.
type socketRequest struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// socketFixture is a server that upgrades and then runs the case's own
// session.
type socketFixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	// request is the first frame the client sent, and resizes every window
	// frame after it.
	request socketRequest
	resizes []resizeFrame
	input   []byte
}

type resizeFrame struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func newSocketFixture(t *testing.T, session func(*socketFixture, *websocket.Conn)) *socketFixture {
	t.Helper()
	f := &socketFixture{t: t}
	upgrader := websocket.Upgrader{
		Subprotocols: []string{subprotocol},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"error":{"code":"unauthenticated","message":"Sign in and send a valid token."}}`, http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		kind, data, err := conn.ReadMessage()
		if err != nil || kind != websocket.TextMessage {
			return
		}
		f.mu.Lock()
		_ = json.Unmarshal(data, &f.request)
		f.mu.Unlock()
		session(f, conn)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// client is a client pointed at the socket fixture.
func (f *socketFixture) client() *client.Client {
	f.t.Helper()
	c, err := client.New(client.Config{URL: f.server.URL, Token: client.StaticToken("caller-token")})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// readClient reads what the client sends until it stops, recording the
// bytes and the windows separately.
func (f *socketFixture) readClient(conn *websocket.Conn) {
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		switch kind {
		case websocket.BinaryMessage:
			f.input = append(f.input, data...)
		case websocket.TextMessage:
			var frame struct {
				Resize *resizeFrame `json:"resize"`
			}
			if json.Unmarshal(data, &frame) == nil && frame.Resize != nil {
				f.resizes = append(f.resizes, *frame.Resize)
			}
		}
		f.mu.Unlock()
	}
}

// exit sends the exit frame and the close that follows it.
func exit(conn *websocket.Conn, code int) {
	_ = conn.WriteJSON(map[string]int{"exit": code})
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// TestASessionCarriesBytesBothWaysAndEndsWithTheExitFrame is design 008's
// socket: the request as the first text frame, output as binary frames, the
// caller's input as binary frames, and the exit code before the close.
func TestASessionCarriesBytesBothWaysAndEndsWithTheExitFrame(t *testing.T) {
	f := newSocketFixture(t, func(f *socketFixture, conn *websocket.Conn) {
		go f.readClient(conn)
		for _, line := range []string{"first\n", "second\n"} {
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte(line)); err != nil {
				return
			}
		}
		// Wait for what the client types before ending, so the assertion
		// below reads a stream that arrived and not one that raced.
		for range 50 {
			f.mu.Lock()
			got := len(f.input)
			f.mu.Unlock()
			if got >= len("typed") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		exit(conn, 7)
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{
		Command: []string{"sh"}, Env: map[string]string{"K": "V"}, Workdir: "/workspace", Cols: 80, Rows: 24, Timeout: "1m",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.Write([]byte("typed")); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("the output ended with %v", err)
	}
	if string(output) != "first\nsecond\n" {
		t.Fatalf("the output is %q", output)
	}
	code, err := s.Wait()
	if err != nil || code != 7 {
		t.Fatalf("Wait() = %d, %v; the exit frame carried 7", code, err)
	}
	if !s.Started() {
		t.Error("a session that wrote output reports that it never started")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.input) != "typed" {
		t.Errorf("the input that arrived is %q", f.input)
	}
	if strings.Join(f.request.Command, " ") != "sh" || f.request.Cols != 80 || f.request.Rows != 24 {
		t.Errorf("the request frame is %+v", f.request)
	}
	if f.request.Env["K"] != "V" || f.request.Workdir != "/workspace" || f.request.Timeout != "1m" {
		t.Errorf("the request frame is %+v", f.request)
	}
}

// TestAResizeReachesTheServer: SIGWINCH is a text frame, which is what the
// command sends when the window changes.
func TestAResizeReachesTheServer(t *testing.T) {
	done := make(chan struct{})
	f := newSocketFixture(t, func(f *socketFixture, conn *websocket.Conn) {
		go f.readClient(conn)
		for range 100 {
			f.mu.Lock()
			got := len(f.resizes)
			f.mu.Unlock()
			if got > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(done)
		exit(conn, 0)
	})
	s, err := f.client().AttachSession(t.Context(), "dev", client.ExecRequest{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err = s.Wait(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resizes) != 1 || f.resizes[0].Cols != 120 || f.resizes[0].Rows != 40 {
		t.Fatalf("the windows that arrived are %+v", f.resizes)
	}
}

// TestAnErrorFrameIsTheSameErrorAnHTTPRefusalIs: a failure after the
// upgrade carries the envelope of design 008, so one error shape reaches
// the caller either way.
func TestAnErrorFrameIsTheSameErrorAnHTTPRefusalIs(t *testing.T) {
	f := newSocketFixture(t, func(_ *socketFixture, conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"error": map[string]any{
			"code":    "capability_unsupported",
			"message": "The environment cannot provide this.",
			"details": map[string]any{"request_id": "req_theservers", "detail": "no terminal", "paths": []any{"spec.image"}},
		}})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "capability_unsupported"))
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = io.ReadAll(s); err == nil {
		t.Fatal("the output ended cleanly after an error frame")
	}
	_, err = s.Wait()
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("Wait() = %T: %v", err, err)
	}
	if refusal.Code != "capability_unsupported" || refusal.RequestID != "req_theservers" {
		t.Fatalf("the refusal is %+v", refusal)
	}
	if refusal.Detail != "no terminal" || strings.Join(refusal.Paths, ",") != "spec.image" || refusal.Details["detail"] != "no terminal" {
		t.Fatalf("the refusal is %+v", refusal)
	}
	if s.Started() {
		t.Error("a session that wrote no byte reports that it started")
	}
}

// TestARefusalBeforeTheUpgradeIsAnHTTPStatus: the bearer travels in the
// handshake, so a capability gate or a denial answers with a status and an
// envelope and never with a close code.
func TestARefusalBeforeTheUpgradeIsAnHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, 422, "capability_unsupported", "The environment cannot provide this.", nil)
	}))
	defer server.Close()
	c, err := client.New(client.Config{URL: server.URL, Token: client.StaticToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal is %T: %v", err, err)
	}
	if refusal.Status != 422 || refusal.Code != "capability_unsupported" {
		t.Fatalf("the refusal is %d %s", refusal.Status, refusal.Code)
	}
}

// TestAServerThatIsNoWebSocketIsRefused: an answer that is 101 without the
// handshake's own proof is not this protocol.
func TestAServerThatIsNoWebSocketIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n"))
	}))
	defer server.Close()
	c, err := client.New(client.Config{URL: server.URL, Token: client.StaticToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AttachSession(t.Context(), "dev", client.ExecRequest{}); err == nil {
		t.Fatal("a handshake the server did not answer was accepted")
	}
}

// TestASocketOnAnAddressNothingAnswersIsUnreachable: the socket separates a
// refusal from an address that is not there, as every other call does.
func TestASocketOnAnAddressNothingAnswersIsUnreachable(t *testing.T) {
	c, err := client.New(client.Config{URL: "http://127.0.0.1:1", Token: client.StaticToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	var gone *client.Unreachable
	if !errors.As(err, &gone) {
		t.Fatalf("the failure is %T: %v", err, err)
	}
}

// TestTheClientAnswersThePingsTheServerSends: the server pings every thirty
// seconds and drops a connection that answers nothing within ninety, so a
// client that never ponged would lose an idle terminal.
func TestTheClientAnswersThePingsTheServerSends(t *testing.T) {
	var mu sync.Mutex
	payload := ""
	seen := make(chan struct{})
	f := newSocketFixture(t, func(f *socketFixture, conn *websocket.Conn) {
		var once sync.Once
		conn.SetPongHandler(func(data string) error {
			mu.Lock()
			payload = data
			mu.Unlock()
			once.Do(func() { close(seen) })
			return nil
		})
		go f.readClient(conn)
		if err := conn.WriteMessage(websocket.PingMessage, []byte("alive")); err != nil {
			return
		}
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
		}
		exit(conn, 0)
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	default:
		t.Fatal("the client answered no pong, and an idle terminal would be dropped")
	}
	mu.Lock()
	defer mu.Unlock()
	if payload != "alive" {
		t.Fatalf("the pong carried %q, want the ping's own payload", payload)
	}
}

// TestAMessagePastOneFrameIsCarriedWhole: a payload longer than a short
// frame's length field travels in the extended forms, both ways.
func TestAMessagePastOneFrameIsCarriedWhole(t *testing.T) {
	big := strings.Repeat("x", 70000)
	f := newSocketFixture(t, func(f *socketFixture, conn *websocket.Conn) {
		go f.readClient(conn)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(strings.Repeat("y", 200)))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(big))
		for range 200 {
			f.mu.Lock()
			got := len(f.input)
			f.mu.Unlock()
			if got >= len(big) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		exit(conn, 0)
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 200+len(big) {
		t.Fatalf("the output is %d bytes, want %d", len(output), 200+len(big))
	}
	if _, err = s.Wait(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.input) != len(big) {
		t.Fatalf("the input that arrived is %d bytes, want %d", len(f.input), len(big))
	}
}

// TestASessionEndedWithoutAnExitFrameFails: a connection that goes away
// mid-command is a failure and not an exit code of zero.
func TestASessionEndedWithoutAnExitFrameFails(t *testing.T) {
	f := newSocketFixture(t, func(_ *socketFixture, conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("partial"))
		_ = conn.Close()
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, _ = io.ReadAll(s)
	if _, err = s.Wait(); err == nil {
		t.Fatal("a connection that went away reported a clean exit")
	}
	if !s.Started() {
		t.Error("a session that wrote a byte reports that it never started")
	}
}
