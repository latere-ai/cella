// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/controller"
)

// writerStub is a writer's public listener as a forwarder sees it: it echoes
// what reached it, streams on one route, echoes a WebSocket on another, and
// on a third takes the request and goes away without answering.
type writerStub struct {
	server  *httptest.Server
	reached atomic.Int64
	next    chan struct{}
}

func newWriterStub(t *testing.T) *writerStub {
	t.Helper()
	w := &writerStub{next: make(chan struct{})}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/environments/echo", func(rw http.ResponseWriter, r *http.Request) {
		w.reached.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]string{
			"method": r.Method, "uri": r.RequestURI, "host": r.Host, "body": string(body),
			"forwarded": r.Header.Get("X-Forwarded-For"), "request": r.Header.Get(RequestIDHeader),
			"auth": r.Header.Get("Authorization"),
		})
	})
	mux.HandleFunc("/v1/environments/stream", func(rw http.ResponseWriter, r *http.Request) {
		flusher := rw.(http.Flusher)
		_, _ = io.WriteString(rw, "first\n")
		flusher.Flush()
		select {
		case <-w.next:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(rw, "second\n")
	})
	mux.HandleFunc("/v1/environments/socket", func(rw http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(kind, append([]byte("echo: "), message...))
	})
	mux.HandleFunc("/v1/environments/vanish", func(rw http.ResponseWriter, r *http.Request) {
		w.reached.Add(1)
		conn, _, err := rw.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	w.server = httptest.NewServer(mux)
	t.Cleanup(w.server.Close)
	return w
}

// fronted serves a forwarder the way cellad mounts the API: under a base the
// public listener strips before the handler sees the request.
func fronted(t *testing.T, f *Forwarder) *httptest.Server {
	t.Helper()
	front := httptest.NewServer(http.StripPrefix("/v1/environments", f))
	t.Cleanup(front.Close)
	return front
}

func fixedWriter(url string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return url, nil }
}

// TestAStandbyForwardsEveryRoute: a standby hands the writer the request as
// the caller sent it, the path under the base, the query, the body, the
// bearer, the caller's Host and a request id, with X-Forwarded-For added; a
// stream reaches the caller write by write; and a WebSocket is carried both
// ways.
func TestAStandbyForwardsEveryRoute(t *testing.T) {
	writer := newWriterStub(t)
	var counted sync.Map
	f := NewForwarder(ForwarderOptions{Writer: fixedWriter(writer.server.URL), Hold: time.Second,
		Counted: func(outcome string) {
			n, _ := counted.LoadOrStore(outcome, new(atomic.Int64))
			n.(*atomic.Int64).Add(1)
		}})
	front := fronted(t, f)

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/environments/echo?limit=2", strings.NewReader("the body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer the-callers-token")
	req.Host = "api.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var got map[string]string
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"method": "POST", "uri": "/v1/environments/echo?limit=2", "host": "api.example.com",
		"body": "the body", "auth": "Bearer the-callers-token"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the writer read %s %q, want %q", k, got[k], v)
		}
	}
	if got["forwarded"] == "" || got["request"] == "" || got["request"] != res.Header.Get(RequestIDHeader) {
		t.Errorf("the writer read X-Forwarded-For %q and request id %q, the caller %q", got["forwarded"], got["request"], res.Header.Get(RequestIDHeader))
	}

	stream, err := http.Get(front.URL + "/v1/environments/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	lines := bufio.NewReader(stream.Body)
	if first, err := lines.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("the stream's first write reached the caller as %q, %v", first, err)
	}
	close(writer.next)
	if second, err := lines.ReadString('\n'); err != nil || second != "second\n" {
		t.Fatalf("the stream's second write reached the caller as %q, %v", second, err)
	}

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/v1/environments/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, message, err := conn.ReadMessage(); err != nil || string(message) != "echo: hello" {
		t.Fatalf("the socket answered %q, %v", message, err)
	}
	if n, _ := counted.Load(ForwardForwarded); n == nil || n.(*atomic.Int64).Load() < 3 {
		t.Errorf("the forwards were not counted as forwarded")
	}
}

// TestAStandbyHoldsUntilAWriterIsPromoted: a request that reaches a standby
// while no replica holds the writer lease waits, and is answered by the
// writer once one is promoted; a dial the old writer refuses sent nothing, and
// the request goes whole to the successor the lease names next.
func TestAStandbyHoldsUntilAWriterIsPromoted(t *testing.T) {
	writer := newWriterStub(t)
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + gone.Addr().String()
	if err := gone.Close(); err != nil {
		t.Fatal(err)
	}
	promoted := time.Now().Add(300 * time.Millisecond)
	var reads atomic.Int64
	f := NewForwarder(ForwarderOptions{Hold: 5 * time.Second, Writer: func(context.Context) (string, error) {
		switch {
		case time.Now().Before(promoted):
			return "", nil
		case reads.Add(1) == 1:
			// The first address read after the gap is a writer that has
			// already gone: its dial is refused.
			return refused, nil
		}
		return writer.server.URL, nil
	}})
	front := fronted(t, f)
	started := time.Now()
	res, err := http.Post(front.URL+"/v1/environments/echo", "text/plain", strings.NewReader("held"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the held request was answered %d", res.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["body"] != "held" {
		t.Errorf("the writer read the body %q after a refused dial, want it whole", got["body"])
	}
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Errorf("the request was answered after %v, before a writer was promoted", waited)
	}
	if n := writer.reached.Load(); n != 1 {
		t.Errorf("the writer was reached %d times, want once", n)
	}
}

// TestAHoldPastTheBoundIsRefused: a request that finds no writer for the
// whole hold is refused with control_plane_unavailable and its fixed
// sentence, and a forward that reached a writer which then went away is
// refused the same way and never sent twice.
func TestAHoldPastTheBoundIsRefused(t *testing.T) {
	t.Run("no writer", func(t *testing.T) {
		f := NewForwarder(ForwarderOptions{Hold: 200 * time.Millisecond, Writer: fixedWriter("")})
		front := fronted(t, f)
		started := time.Now()
		res, err := http.Get(front.URL + "/v1/environments/echo")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		assertControlPlaneUnavailable(t, res)
		if waited := time.Since(started); waited < 150*time.Millisecond {
			t.Errorf("the request was refused after %v, before its hold ended", waited)
		}
	})
	t.Run("the writer went away with the request", func(t *testing.T) {
		writer := newWriterStub(t)
		f := NewForwarder(ForwarderOptions{Hold: time.Second, Writer: fixedWriter(writer.server.URL)})
		front := fronted(t, f)
		res, err := http.Post(front.URL+"/v1/environments/vanish", "text/plain", strings.NewReader("once"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		assertControlPlaneUnavailable(t, res)
		if n := writer.reached.Load(); n != 1 {
			t.Errorf("a request that reached the writer was sent %d times", n)
		}
	})
}

func assertControlPlaneUnavailable(t *testing.T, res *http.Response) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable || envelope.Error.Code != "control_plane_unavailable" ||
		envelope.Error.Message != "The control plane is unavailable; retry shortly." {
		t.Errorf("the refusal is %d %+v", res.StatusCode, envelope.Error)
	}
	if res.Header.Get(RequestIDHeader) == "" || envelope.Error.Details["request_id"] != res.Header.Get(RequestIDHeader) {
		t.Errorf("the refusal carries request id %q and %v", res.Header.Get(RequestIDHeader), envelope.Error.Details["request_id"])
	}
}

// TestANotWriterRefusalIsControlPlaneUnavailable: a write a demoted writer's
// fence refused reaches the caller as the same 503 a standby answers when it
// finds no writer, so the caller retries and reaches the next writer.
func TestANotWriterRefusalIsControlPlaneUnavailable(t *testing.T) {
	for _, err := range []error{fmt.Errorf("persist: %w", controller.ErrNotWriter), ErrNoWriter} {
		status, envelope := errorEnvelope(err, "req_one")
		if status != http.StatusServiceUnavailable || envelope.Code != "control_plane_unavailable" ||
			envelope.Message != "The control plane is unavailable; retry shortly." {
			t.Errorf("%v answers %d %+v", err, status, envelope)
		}
	}
}

// TestAHeldRequestIsServedHereOnceThisProcessPromotes: a request a standby
// holds while no other replica is the writer is answered by this process the
// moment it promotes, not forwarded to itself and not refused at the hold.
func TestAHeldRequestIsServedHereOnceThisProcessPromotes(t *testing.T) {
	var serving atomic.Bool
	changed := make(chan struct{})
	f := NewForwarder(ForwarderOptions{
		Hold:   5 * time.Second,
		Writer: fixedWriter(""),
		Local: func(w http.ResponseWriter, _ *http.Request) bool {
			if !serving.Load() {
				return false
			}
			_, _ = io.WriteString(w, "local")
			return true
		},
		Changed: func() <-chan struct{} { return changed },
	})
	front := fronted(t, f)
	time.AfterFunc(200*time.Millisecond, func() {
		serving.Store(true)
		close(changed)
	})
	started := time.Now()
	res, err := http.Get(front.URL + "/v1/environments/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || string(body) != "local" {
		t.Errorf("the held request was answered %d %q, want this process's answer", res.StatusCode, body)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("the held request waited %v after the promotion", waited)
	}
}
