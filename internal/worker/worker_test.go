// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/worker"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
)

// plane is a control plane's half of the worker seam over loopback: the
// registration route and the stream, served by the hub of spec 021. It is the
// same hub cellad serves, so what the worker proves here it proves against
// the server.
type plane struct {
	hub    *remote.Hub
	server *httptest.Server
	driver *remote.Driver

	mu         sync.Mutex
	keys       []string
	refuse     bool
	registered int
}

func newPlane(t *testing.T) *plane {
	t.Helper()
	p := &plane{hub: remote.NewHub(remote.HubOptions{Offline: time.Minute})}
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/workers", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.keys = append(p.keys, r.Header.Get("Authorization"))
		refuse := p.refuse
		p.registered++
		p.mu.Unlock()
		if refuse {
			http.Error(w, `{"error":{"code":"unauthenticated"}}`, http.StatusUnauthorized)
			return
		}
		var registration remote.Registration
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		registered, err := p.hub.Register("env_test", registration)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		registered.Environment = "env_test"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(registered)
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = p.hub.Serve(r.Context(), "env_test", worker.NewSocket(conn))
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	d, err := remote.New(remote.Options{Environment: "env_test", Transport: p.hub.Transport("env_test")})
	if err != nil {
		t.Fatal(err)
	}
	p.driver = d
	return p
}

// start runs one worker against the plane and returns the function that stops
// it, so a test takes a worker away and puts it back.
func (p *plane) start(t *testing.T, host driver.Driver) (*worker.Worker, func()) {
	t.Helper()
	w, err := worker.New(worker.Options{
		URL: p.server.URL, Key: "an-environment-key", Driver: host, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the worker did not stop inside the deadline")
		}
	})
	t.Cleanup(stop)
	return w, stop
}

func nativeDriver(t *testing.T) driver.Driver {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// waitWorkers polls until the environment holds the wanted number of
// connected workers, which is what its phase is computed from.
func (p *plane) waitWorkers(t *testing.T, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connected := 0
		for _, w := range p.hub.Workers("env_test") {
			if w.Connected {
				connected++
			}
		}
		if connected == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: the environment never held %d connected workers", what, want)
}

// TestWorkerRegistersAndHoldsTheStream is spec 021's worker: it connects
// outbound with its key, registers what its driver provides, opens the
// stream, and the environment becomes one a sandbox can be placed on.
func TestWorkerRegistersAndHoldsTheStream(t *testing.T) {
	p := newPlane(t)
	host := nativeDriver(t)
	w, _ := p.start(t, host)
	p.waitWorkers(t, 1, "after the worker started")

	if err := p.driver.Ready(t.Context()); err != nil {
		t.Errorf("the environment is not ready with a worker on it: %v", err)
	}
	if registration, held := p.hub.Transport("env_test").Registration(); !held {
		t.Errorf("the environment holds no registration")
	} else if registration.Driver != host.Name() || registration.Isolation != host.Isolation() {
		t.Errorf("the registration reports %s/%s, want the worker's %s/%s",
			registration.Driver, registration.Isolation, host.Name(), host.Isolation())
	}
	if !strings.HasPrefix(w.ID(), "wrk_") {
		t.Errorf("the worker claims under %q, want a wrk_ id", w.ID())
	}
	if w.Environment() != "env_test" {
		t.Errorf("the worker serves %q", w.Environment())
	}
	p.mu.Lock()
	keys := append([]string(nil), p.keys...)
	p.mu.Unlock()
	if len(keys) == 0 || keys[0] != "Bearer an-environment-key" {
		t.Errorf("the registration carried %v, want the environment key as the bearer", keys)
	}

	// The driver over the stream runs a real operation on the worker's own
	// driver, which is what makes this environment a place sandboxes run.
	ref, err := p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_over_the_wire", Name: "over-the-wire", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create over the stream failed: %v", err)
	}
	state, err := p.driver.Inspect(t.Context(), ref.ID)
	if err != nil {
		t.Fatalf("the inspect failed: %v", err)
	}
	if state.Owner != "ops" {
		t.Errorf("the sandbox reads back as %+v", state)
	}
	if err = p.driver.Delete(t.Context(), ref.ID); err != nil {
		t.Errorf("the delete over the stream failed: %v", err)
	}
}

// TestWorkerReconnects is spec 021's reconnect: a worker whose stream ends
// registers again and the environment becomes placeable a second time,
// without anything having dialed the worker.
func TestWorkerReconnects(t *testing.T) {
	p := newPlane(t)
	host := nativeDriver(t)
	_, stop := p.start(t, host)
	p.waitWorkers(t, 1, "after the worker started")

	stop()
	p.waitWorkers(t, 0, "after the worker stopped")
	if err := p.driver.Ready(t.Context()); !errors.Is(err, remote.ErrNoWorker) {
		t.Errorf("an environment with no connected worker answers %v, want ErrNoWorker", err)
	}
	// The registration is still recorded, so the environment still declares
	// what its driver provides: it is unreachable, not unknown.
	if _, held := p.hub.Transport("env_test").Registration(); !held {
		t.Errorf("a worker going away dropped the environment's registration")
	}

	_, _ = p.start(t, host)
	p.waitWorkers(t, 1, "after the worker came back")
	if err := p.driver.Ready(t.Context()); err != nil {
		t.Errorf("the environment did not become ready again: %v", err)
	}
	p.mu.Lock()
	registrations := p.registered
	p.mu.Unlock()
	if registrations < 2 {
		t.Errorf("the worker registered %d times, want one per connection", registrations)
	}
}

// TestWorkerRetriesARefusedRegistration holds the backoff: a control plane
// that refuses the registration is tried again rather than exited on, because
// a key that was not yet applied is the ordinary case at install time.
func TestWorkerRetriesARefusedRegistration(t *testing.T) {
	p := newPlane(t)
	p.mu.Lock()
	p.refuse = true
	p.mu.Unlock()
	host := nativeDriver(t)
	_, _ = p.start(t, host)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		attempts := p.registered
		p.mu.Unlock()
		if attempts >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the worker did not try the refused registration again")
}

// TestWorkerRefusals holds what a worker refuses to start on: the two
// variables it cannot run without, and a driver whose own preflight fails,
// which must not register because the control plane would place work on it.
func TestWorkerRefusals(t *testing.T) {
	host := nativeDriver(t)
	for _, tc := range []struct {
		name string
		o    worker.Options
	}{
		{"no control plane URL", worker.Options{Key: "k", Driver: host}},
		{"no environment key", worker.Options{URL: "https://control.example.test", Driver: host}},
		{"no driver", worker.Options{URL: "https://control.example.test", Key: "k"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := worker.New(tc.o); err == nil {
				t.Errorf("this worker was built and cannot run")
			}
		})
	}
	w, err := worker.New(worker.Options{
		URL: "https://control.example.test", Key: "k", Driver: refusingDriver{Driver: host},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Run(t.Context()); err == nil {
		t.Errorf("a worker whose driver fails preflight registered")
	}
}

// refusingDriver is a driver whose preflight fails, which is a host missing
// what its driver needs.
type refusingDriver struct{ driver.Driver }

func (refusingDriver) Preflight(context.Context) error {
	return errors.New("this host has no container engine")
}

// TestStreamURL holds how the control plane's URL becomes the stream's: the
// scheme's WebSocket equivalent, the environment in the path, and whatever
// base path the control plane is served under kept.
func TestStreamURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		base string
		env  string
		want string
	}{
		{"https becomes wss", "https://cella.example.test", "env_a",
			"wss://cella.example.test/v1/environments/env_a/operations"},
		{"http on loopback becomes ws", "http://127.0.0.1:8080", "env_a",
			"ws://127.0.0.1:8080/v1/environments/env_a/operations"},
		{"a trailing slash is not a path segment", "https://cella.example.test/", "env_a",
			"wss://cella.example.test/v1/environments/env_a/operations"},
		{"a base path is kept", "https://example.test/cella", "env_a",
			"wss://example.test/cella/v1/environments/env_a/operations"},
		{"no environment is the one the key names", "https://cella.example.test", "",
			"wss://cella.example.test/v1/environments/self/operations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := worker.StreamURL(tc.base, tc.env)
			if err != nil {
				t.Fatalf("the stream URL was not built: %v", err)
			}
			if got != tc.want {
				t.Errorf("the stream URL is %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := worker.StreamURL("://not a url", "env_a"); err == nil {
		t.Errorf("a base that is not a URL built a stream URL")
	}
}

// TestWorkerRefusesAControlPlaneThatDoesNotSpeakTheProtocol holds the
// subprotocol rule: a control plane of another release refuses the upgrade or
// answers another vocabulary, and either way the worker does not go on.
func TestWorkerRefusesAControlPlaneThatDoesNotSpeakTheProtocol(t *testing.T) {
	registered := 0
	mute := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/workers") {
			registered++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"worker":"wrk_1","environment":"env_a"}`))
			return
		}
		// An upgrade that never happens: the worker cannot open a stream.
		http.Error(w, "no websocket here", http.StatusNotFound)
	}))
	t.Cleanup(mute.Close)

	w, err := worker.New(worker.Options{URL: mute.URL, Key: "k", Driver: nativeDriver(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && registered < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if registered < 2 {
		t.Errorf("the worker tried %d times against a control plane with no stream, want a retry", registered)
	}
}

// TestWorkerRefusesAnAnswerItCannotRead holds that a registration answered
// with something that is not one is a failure rather than a worker that
// claims under no id.
func TestWorkerRefusesAnAnswerItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a refusal", http.StatusForbidden, `{"error":{"code":"forbidden"}}`},
		{"an answer that is not JSON", http.StatusCreated, `not json`},
		{"an answer that names no worker", http.StatusCreated, `{"environment":"env_a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			w, err := worker.New(worker.Options{URL: server.URL, Key: "k", Driver: nativeDriver(t)})
			if err != nil {
				t.Fatal(err)
			}
			if w.ID() != "" {
				t.Errorf("a worker that has not registered claims under %q", w.ID())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_ = w.Run(ctx)
			if w.ID() != "" {
				t.Errorf("a worker whose registration was not read claims under %q", w.ID())
			}
		})
	}
}

// TestSocketRefusesAnotherFraming holds that every frame of this protocol is
// binary: a peer sending text is speaking something else, and a stream that
// half understands is worse than one that closes.
func TestSocketRefusesAnotherFraming(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	read := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		socket := worker.NewSocket(conn)
		_, err = socket.ReadFrame()
		read <- err
		_ = socket.Close()
	}))
	t.Cleanup(server.Close)

	dialer := &websocket.Dialer{Subprotocols: []string{remote.Protocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("the stream did not open: %v", err)
	}
	if res != nil {
		_ = res.Body.Close()
	}
	if err = conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("the text frame was not sent: %v", err)
	}
	select {
	case err = <-read:
		if err == nil || !strings.Contains(err.Error(), "binary") {
			t.Errorf("a text frame was read as one of this protocol: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the socket never answered the text frame")
	}
	_ = conn.Close()
}

// TestSocketRoundTripsAFrame holds the other half: a whole frame travels both
// ways, and a write on a closed socket is refused rather than lost.
func TestSocketRoundTripsAFrame(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	echoed := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		socket := worker.NewSocket(conn)
		raw, err := socket.ReadFrame()
		if err == nil {
			echoed <- raw
			_ = socket.WriteFrame(raw)
		}
	}))
	t.Cleanup(server.Close)

	dialer := &websocket.Dialer{Subprotocols: []string{remote.Protocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("the stream did not open: %v", err)
	}
	if res != nil {
		_ = res.Body.Close()
	}
	socket := worker.NewSocket(conn)
	sent, err := remote.EncodeMessage(remote.NoOperation, remote.Message{Type: remote.MessageHeartbeat})
	if err != nil {
		t.Fatal(err)
	}
	if err = socket.WriteFrame(sent); err != nil {
		t.Fatalf("the frame was not written: %v", err)
	}
	select {
	case got := <-echoed:
		if string(got) != string(sent) {
			t.Errorf("the frame arrived as %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the frame never arrived")
	}
	back, err := socket.ReadFrame()
	if err != nil {
		t.Fatalf("the echoed frame was not read: %v", err)
	}
	if string(back) != string(sent) {
		t.Errorf("the echoed frame is %q", back)
	}
	if err = socket.Close(); err != nil {
		t.Errorf("closing the socket is %v", err)
	}
	if err = socket.WriteFrame(sent); err == nil {
		t.Errorf("a frame was written on a closed socket")
	}
	if _, err = socket.ReadFrame(); err == nil {
		t.Errorf("a frame was read from a closed socket")
	}
}
