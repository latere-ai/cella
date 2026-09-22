// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellaclient_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/cellaclient"
)

// dialPlane speaks the dial socket of internal/api/dial.go: the subprotocol,
// binary frames of bytes both ways, and a close with the code the session
// ended on.
func dialPlane(t *testing.T, session func(*websocket.Conn)) (*httptest.Server, *string) {
	t.Helper()
	upgrader := websocket.Upgrader{Subprotocols: []string{"cella.dial.v1"}, CheckOrigin: func(*http.Request) bool { return true }}
	var mu sync.Mutex
	path := new(string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/dial/9") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"phase_conflict","message":"The sandbox is not in a state that allows this."}}`)
			return
		}
		mu.Lock()
		*path = r.URL.Path
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		session(conn)
	}))
	t.Cleanup(server.Close)
	return server, path
}

func dialClient(t *testing.T, server *httptest.Server) *cellaclient.Client {
	t.Helper()
	client, err := cellaclient.New(cellaclient.Config{URL: server.URL, Token: "caller-token", UserAgent: "cella-test"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestADialStreamCarriesBytesBothWays: the stream writes frames of bounded
// size, reads the binary frames back, passes over a frame that carries no
// bytes, and reads a normal close as the end of the stream.
func TestADialStreamCarriesBytesBothWays(t *testing.T) {
	server, path := dialPlane(t, func(conn *websocket.Conn) {
		var got []byte
		for len(got) < 100<<10 {
			kind, data, err := conn.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage || len(data) > 32<<10 {
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "bad frame"))
				return
			}
			got = append(got, data...)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("not bytes"))
		_ = conn.WriteMessage(websocket.BinaryMessage, nil)
		_ = conn.WriteMessage(websocket.BinaryMessage, got[:5])
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "the port closed"))
	})
	stream, err := dialClient(t, server).Dial(t.Context(), "dev box", 5432)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	payload := []byte(strings.Repeat("abcde", 20<<10))
	if n, err := stream.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("a normal close is the end of the stream, not %v", err)
	}
	if string(got) != "abcde" {
		t.Fatalf("read back %q", got)
	}
	if *path != "/v1/sandboxes/dev%20box/dial/5432" && *path != "/v1/sandboxes/dev box/dial/5432" {
		t.Fatalf("the socket was opened at %q", *path)
	}
}

// TestADialStreamNamesTheCodeItClosedWith: a close that is not normal is the
// refusal the reason names, and a refusal before the upgrade is the HTTP one.
func TestADialStreamNamesTheCodeItClosedWith(t *testing.T) {
	for _, tc := range []struct {
		reason, message string
		status          int
	}{
		{"upstream_unavailable", "Nothing is listening on that port.", http.StatusBadGateway},
		{"not_found", "The connection inside the sandbox ended: not_found.", http.StatusInternalServerError},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			server, _ := dialPlane(t, func(conn *websocket.Conn) {
				_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, tc.reason))
			})
			stream, err := dialClient(t, server).Dial(t.Context(), "dev", 5432)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			_, err = stream.Read(make([]byte, 8))
			var refusal *cellaclient.Error
			if !errors.As(err, &refusal) || refusal.Code != tc.reason || refusal.Message != tc.message || refusal.Status != tc.status {
				t.Fatalf("the close read as %#v", err)
			}
		})
	}
	server, _ := dialPlane(t, func(*websocket.Conn) {})
	_, err := dialClient(t, server).Dial(t.Context(), "dev", 9)
	if cellaclient.CodeOf(err) != "phase_conflict" {
		t.Fatalf("a refusal before the upgrade read as %v", err)
	}
	server.Close()
	stream, err := dialClient(t, server).Dial(t.Context(), "dev", 5432)
	if err == nil {
		_ = stream.Close()
		t.Fatal("a socket to a closed server opened")
	}
}

// TestADialStreamWriteFailsOnAClosedSocket: a write after the socket ended
// answers the failure rather than a count.
func TestADialStreamWriteFailsOnAClosedSocket(t *testing.T) {
	server, _ := dialPlane(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
	})
	stream, err := dialClient(t, server).Dial(t.Context(), "dev", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := stream.Write([]byte("late")); err == nil || n != 0 {
		t.Fatalf("a write after close answered %d, %v", n, err)
	}
	if _, err = stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("a read after close answered no error")
	}
}
