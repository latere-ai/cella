// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/cellacli"
)

// lockedBuffer is a writer the command and the test read at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// forwardPlane is a server speaking the dial socket: it echoes the bytes of a
// socket to port 5432, closes one to port 6000 with upstream_unavailable, and
// refuses port 9 before the upgrade.
func forwardPlane(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"cella.dial.v1"}, CheckOrigin: func(*http.Request) bool { return true }}
	opened := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandboxes/dev/dial/9":
			refuse(w, http.StatusConflict, "phase_conflict", "The sandbox is not in a state that allows this.")
			return
		case "/v1/sandboxes/gone/dial/5432":
			refuse(w, http.StatusNotFound, "not_found", "There is no such object.")
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		opened.Add(1)
		if strings.HasSuffix(r.URL.Path, "/dial/6000") {
			// The probe of the command's start is let through; every
			// connection after it finds nothing listening.
			if opened.Load() > 1 {
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream_unavailable"))
			}
			return
		}
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err = conn.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, opened
}

// forwarding runs the command until the test cancels it, and returns the
// address it listens on as the line it printed names it.
func forwarding(t *testing.T, server *httptest.Server, args ...string) (string, *lockedBuffer, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var out lockedBuffer
	errOut := &lockedBuffer{}
	codes := make(chan int, 1)
	go func() {
		codes <- cellacli.Run(ctx, cellacli.Env{
			Args: args, Stdin: strings.NewReader(""), Stdout: &out, Stderr: errOut,
			Getenv:  environment(map[string]string{"CELLA_URL": server.URL, "CELLA_TOKEN": "caller-token"}),
			Version: "v0.0.0-test",
		})
	}()
	line := regexp.MustCompile(`^Forwarding (127\.0\.0\.1:\d+) to port \d+ of \S+\n$`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := line.FindStringSubmatch(out.String()); m != nil {
			return m[1], errOut, func() int {
				cancel()
				select {
				case code := <-codes:
					return code
				case <-time.After(5 * time.Second):
					t.Fatal("port-forward did not end with its context")
					return -1
				}
			}
		}
		select {
		case code := <-codes:
			t.Fatalf("port-forward exited %d before listening: %s", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("port-forward printed %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPortForward is design 011's port-forward: every accepted connection is
// carried both ways by its own dial socket, a connection whose socket fails
// is reported and the listener goes on, and the command ends with its
// context.
func TestPortForward(t *testing.T) {
	server, opened := forwardPlane(t)
	addr, _, stop := forwarding(t, server, "port-forward", "dev", "0:5432")
	for _, line := range []string{"first", "second"} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(conn, line+"\n"); err != nil {
			t.Fatal(err)
		}
		got, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || got != line+"\n" {
			t.Fatalf("read back %q, %v", got, err)
		}
		_ = conn.Close()
	}
	// The probe and one socket per connection.
	if got := opened.Load(); got != 3 {
		t.Errorf("%d sockets were opened, want the probe and one per connection", got)
	}
	if code := stop(); code != 0 {
		t.Fatalf("an interrupted port-forward exited %d", code)
	}
}

// TestPortForwardReportsAConnectionItCouldNotCarry: a port nothing listens on
// yet is not a refusal at start, and each connection to it is reported and
// closed while the listener stays up.
func TestPortForwardReportsAConnectionItCouldNotCarry(t *testing.T) {
	server, _ := forwardPlane(t)
	addr, errOut, stop := forwarding(t, server, "port-forward", "dev", "0:6000")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(conn); err != nil {
		t.Fatalf("the connection was not closed: %v", err)
	}
	_ = conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(errOut.String(), "Nothing is listening on that port.") {
		if time.Now().After(deadline) {
			t.Fatalf("the failed connection was not reported: %q", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := stop(); code != 0 {
		t.Fatalf("an interrupted port-forward exited %d", code)
	}
}

// TestPortForwardRefusals: what the command cannot start with is its exit,
// by design 011's scheme, and nothing listens.
func TestPortForwardRefusals(t *testing.T) {
	server, _ := forwardPlane(t)
	env := environment(map[string]string{"CELLA_URL": server.URL, "CELLA_TOKEN": "caller-token"})
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	_, busy, err := net.SplitHostPort(taken.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"port-forward", "dev"}, 2, "needs a sandbox and <local>:<port>"},
		{[]string{"port-forward", "dev", "5432"}, 2, "takes <local>:<port>"},
		{[]string{"port-forward", "dev", "x:5432"}, 2, "local port"},
		{[]string{"port-forward", "dev", "0:0"}, 2, "not between 1 and 65535"},
		{[]string{"port-forward", "dev", "0:70000"}, 2, "not between 1 and 65535"},
		{[]string{"port-forward", "--bogus", "dev", "0:5432"}, 2, "bogus"},
		{[]string{"port-forward", "dev", "0:9"}, 5, "not in a state"},
		{[]string{"port-forward", "gone", "0:5432"}, 4, "no such object"},
		{[]string{"port-forward", "dev", busy + ":5432"}, 1, "address already in use"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got := runWith(t, cellacli.Env{Args: tc.args, Getenv: env})
			if got.code != tc.code || !strings.Contains(got.stderr, tc.want) {
				t.Fatalf("exited %d with %q, want %d naming %q", got.code, got.stderr, tc.code, tc.want)
			}
			if strings.Contains(got.stdout, "Forwarding") {
				t.Fatalf("a refused port-forward listened: %q", got.stdout)
			}
		})
	}
	unset := runWith(t, cellacli.Env{Args: []string{"port-forward", "dev", "0:5432"}, Getenv: environment(nil)})
	if unset.code != 2 {
		t.Fatalf("a command with no control plane exited %d: %s", unset.code, unset.stderr)
	}
}
