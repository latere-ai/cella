// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
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

// carrier is a caller's transport: it marks every request it carries and
// counts them, so a test can tell which calls went through it.
type carrier struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (c *carrier) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.seen = append(c.seen, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	r = r.Clone(r.Context())
	r.Header.Set("X-Carried-By", "the-caller")
	return c.base.RoundTrip(r)
}

func (c *carrier) carried() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// TestTheCallersHTTPClientCarriesEveryCall: a caller's http.Client carries
// the plain calls and the upgrade of the exec and dial sockets alike, so its
// proxy, dialer, trust and instrumentation reach all of them. A client whose
// Timeout wraps the body into one that cannot be written is refused with a
// sentence rather than a socket that fails on its first write.
func TestTheCallersHTTPClientCarriesEveryCall(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"cella.exec.v1", "cella.dial.v1"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Carried-By") != "the-caller" {
			http.Error(w, "not carried by the caller's transport", http.StatusTeapot)
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			writeObject(w, 200, "dev", "sbx_1")
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if strings.Contains(r.URL.Path, "/dial/") {
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("from inside"))
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]int{"exit": 3})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	t.Cleanup(server.Close)

	through := &carrier{base: http.DefaultTransport}
	c, err := client.New(client.Config{URL: server.URL, Token: client.StaticToken("t"), HTTPClient: &http.Client{Transport: through}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatalf("the plain call: %v", err)
	}
	session, err := c.ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatalf("the exec socket: %v", err)
	}
	if code, err := session.Wait(); err != nil || code != 3 {
		t.Fatalf("the session ended %d, %v", code, err)
	}
	_ = session.Close()
	stream, err := c.Dial(t.Context(), "dev", 8080)
	if err != nil {
		t.Fatalf("the dial socket: %v", err)
	}
	got, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil || string(got) != "from inside" {
		t.Fatalf("the dial stream read %q, %v", got, err)
	}
	want := []string{"GET /v1/sandboxes/dev", "GET /v1/sandboxes/dev/exec", "GET /v1/sandboxes/dev/dial/8080"}
	if carried := through.carried(); strings.Join(carried, ",") != strings.Join(want, ",") {
		t.Fatalf("the caller's transport carried %v, want %v", carried, want)
	}

	bounded, err := client.New(client.Config{URL: server.URL, Token: client.StaticToken("t"),
		HTTPClient: &http.Client{Transport: through, Timeout: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bounded.ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}}); err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("a socket over a client with a Timeout opened with %v", err)
	}
}
