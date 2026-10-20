// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// testLabels are the keys a test's plane names its sessions under on a
// sandbox, as a hosted plane's would.
var testLabels = SessionLabels{
	Session: "example.com/session", Agent: "example.com/agent", AgentVersion: "example.com/agent-version",
	Trigger: "example.com/trigger", TriggerFiresOn: "example.com/trigger-fires-on", AgentName: "example.com/agent-name",
}

// testPlatformAudience is the audience of a test's platform token.
const testPlatformAudience = "api.example.com"

// testEngine is New with the test's label keys when a test names none.
func testEngine(c Config) *Engine {
	if c.Labels == (SessionLabels{}) {
		c.Labels = testLabels
	}
	return New(c)
}

// testCredentials are a store's options for a test: the wait given, the
// test's platform audience, and readTestToken.
func testCredentials(wait time.Duration) CredentialsOptions {
	return CredentialsOptions{Wait: wait, PlatformAudience: testPlatformAudience, Read: readTestToken}
}

// readTestToken reads the claims of a test's unsigned JWT, the way an
// embedder's reader reads its plane's: aud, sub, exp, and org_id for an
// organization's context.
func readTestToken(token string) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return TokenClaims{}, errors.New("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, err
	}
	var c struct {
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
		Sub string          `json:"sub"`
		Org string          `json:"org_id"`
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return TokenClaims{}, err
	}
	var auds []string
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		auds = []string{one}
	} else if err = json.Unmarshal(c.Aud, &auds); err != nil {
		return TokenClaims{}, errors.New("no audience")
	}
	claims := TokenClaims{Audiences: auds, Subject: c.Sub, Personal: c.Org == "", Context: c.Org}
	if c.Exp > 0 {
		claims.Expiry = time.Unix(c.Exp, 0)
	}
	return claims, nil
}

// testEnv is the environment every engine test serves.
const testEnv = "dev-abcdefghijklmnop"

// realSRT is the sandbox runtime on this machine's own PATH, read once
// before any test puts a stand-in in front of it.
var realSRT = func() string {
	path, err := exec.LookPath("srt")
	if err != nil {
		return ""
	}
	return path
}()

// realSandbox is whether commands run confined here: on macOS always, since
// TestMain calls hostsandbox.Main and the local driver then confines with
// Seatbelt, part of the system; elsewhere when the real srt is installed.
var realSandbox = goruntime.GOOS == "darwin" || realSRT != ""

// requireSandbox skips a test that proves confinement where commands do not
// run confined, as on the Linux CI runners without srt, and names why.
func requireSandbox(t *testing.T) {
	t.Helper()
	if !realSandbox {
		t.Skip("srt, the sandbox runtime, is not on PATH here, so confinement is unchecked on this machine; install @anthropic-ai/sandbox-runtime to run this test")
	}
}

// sandboxRuntime makes the local driver runnable for a test of the engine's
// own behavior: the real srt where it is installed, and elsewhere a stand-in
// that runs each command unconfined, so the test runs on every runner. It
// reports whether the real one runs.
func sandboxRuntime(t *testing.T) bool {
	t.Helper()
	if realSandbox {
		return true
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "srt"), []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// What srt itself needs, by platform: ripgrep, and on Linux bubblewrap
	// and socat. The stand-in runs none of them.
	for _, name := range []string{"rg", "bwrap", "socat"} {
		if err := os.Symlink("/bin/echo", filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	return false
}

// testPaths are the engine's paths in temporary directories. The state
// directory is short, since a Unix socket's path is.
func testPaths(t *testing.T) Paths {
	t.Helper()
	home := resolvedTemp(t)
	state, err := os.MkdirTemp("/tmp", "ld")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	config := filepath.Join(home, ".config", "latere")
	return Paths{
		Home: home, Config: config, Token: filepath.Join(config, "auth-token.json"),
		State: filepath.Join(state, "latere"), Cache: filepath.Join(home, ".cache", "latere"),
	}
}

// resolvedTemp is a temporary directory with its links resolved, which is
// how the operating system's sandbox names it.
func resolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// connected writes a host policy that names testEnv and grants project, and
// keeps a fresh key for it. Its session_start is ask, since the engine's
// tests go through the person's answers; session_start allow has tests of
// its own.
func connected(t *testing.T, paths Paths, keys KeyStore) (project string) {
	t.Helper()
	project = filepath.Join(paths.Home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SavePolicy(paths.PolicyFile(), Policy{
		Device: testEnv, Name: "Test computer", SessionStart: SessionStartAsk,
		Folders: []Folder{{Path: "~/project", Write: true}},
		Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: now, ExpiresAt: now.Add(KeyTTL)}); err != nil {
		t.Fatal(err)
	}
	return project
}

// recorder is one record of what happened in what order, shared by a
// plane and an account.
type recorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *recorder) add(step string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.steps, ", ")
}

// fakeAccount is the person's side of the control plane, in memory.
type fakeAccount struct {
	mu      sync.Mutex
	minted  int
	revoked []string
	exists  bool
	mintErr error
	// order records mints and revocations as they happen.
	order *recorder
}

func (a *fakeAccount) MintKey(_ context.Context, env string, ttl time.Duration) (Key, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mintErr != nil {
		return Key{}, a.mintErr
	}
	if ttl != KeyTTL {
		return Key{}, fmt.Errorf("a key was minted with ttl %s, want %s", ttl, KeyTTL)
	}
	a.minted++
	n := a.minted + 1
	a.order.add(fmt.Sprintf("mint key-%d", n))
	now := time.Now().UTC()
	return Key{Environment: env, Token: fmt.Sprintf("key-%d", n), JTI: fmt.Sprintf("jti-%d", n), MintedAt: now, ExpiresAt: now.Add(ttl)}, nil
}

func (a *fakeAccount) RevokeKey(_ context.Context, _, jti string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked = append(a.revoked, jti)
	a.order.add("revoke " + jti)
	return nil
}

func (a *fakeAccount) Exists(context.Context, string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exists, nil
}

func (a *fakeAccount) revokedNow() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.revoked...)
}

// stubPlane is a control plane that registers one worker and serves its
// stream, and hands back a driver that sends each create as the test writes
// it. register, when set, answers the registration instead.
type stubPlane struct {
	hub    *remote.Hub
	url    string
	server *httptest.Server
	driver runtime.Driver

	mu       sync.Mutex
	bearers  []string
	register func(w http.ResponseWriter, bearer string) bool
	order    *recorder
	// closeWith, when set, closes every stream at once with this reason,
	// as cellad closes the stream of a worker it stops.
	closeWith atomic.Pointer[string]
}

func newStubPlane(t *testing.T) *stubPlane {
	t.Helper()
	p := &stubPlane{hub: remote.NewHub(remote.HubOptions{Offline: time.Minute})}
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/workers", func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p.mu.Lock()
		p.bearers = append(p.bearers, bearer)
		register := p.register
		p.mu.Unlock()
		p.order.add("register " + bearer)
		if register != nil && register(w, bearer) {
			return
		}
		var registration remote.Registration
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The key's token stands in for its jti, so a test revokes a key
		// by the token it handed the engine.
		registered, _, err := p.hub.RegisterWith(testEnv, registration, remote.Admission{JTI: bearer})
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		registered.Environment = testEnv
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(registered)
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if reason := p.closeWith.Load(); reason != nil {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(remote.ClosePolicy, *reason), time.Now().Add(time.Second))
			_ = conn.Close()
			return
		}
		_ = p.hub.Serve(r.Context(), testEnv, worker.NewSocket(conn))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p.url, p.server = server.URL, server
	d, err := remote.New(remote.Options{Environment: testEnv, Transport: p.hub.Transport(testEnv)})
	if err != nil {
		t.Fatal(err)
	}
	p.driver = d
	return p
}

// refuse has every registration answered with code, as the control plane
// answers a worker it stops.
func (p *stubPlane) refuse(status int, code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.register = func(w http.ResponseWriter, _ string) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"refused","details":{"detail":"the stub refused"}}}`, code)
		return true
	}
}

func (p *stubPlane) bearersNow() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bearers...)
}

// endStream ends the stream the worker holds, and has the next one closed
// with reason when it is not empty, which is how a stream closed with a
// refusal reaches the worker.
func (p *stubPlane) endStream(reason string) {
	if reason != "" {
		p.closeWith.Store(&reason)
	}
	p.hub.Release(testEnv)
}

// live is the host of the worker holding a stream now, and whether one
// does.
func (p *stubPlane) live() (*v1.HostInfo, bool) {
	for _, w := range p.hub.Workers(testEnv) {
		if w.Connected {
			return w.Host, true
		}
	}
	return nil, false
}

// waitConnected waits for the worker's stream.
func (p *stubPlane) waitConnected(t *testing.T) {
	t.Helper()
	waitFor(t, "the worker to connect", func() bool {
		_, ok := p.live()
		return ok
	})
}

// waitFor polls cond for up to fifteen seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// running starts the engine and answers a function that stops it and
// returns what Run returned, and a channel closed once Run returned.
func running(t *testing.T, e *Engine) (stop func() error, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var result error
	go func() {
		result = e.Run(ctx)
		close(finished)
	}()
	stop = func() error {
		cancel()
		select {
		case <-finished:
			return result
		case <-time.After(30 * time.Second):
			return errors.New("the engine did not stop inside the deadline")
		}
	}
	t.Cleanup(func() { _ = stop() })
	return stop, finished
}

// asked is what an answering client saw, read under its lock.
type asked struct {
	mu   sync.Mutex
	list []Approval
}

func (a *asked) all() []Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Approval(nil), a.list...)
}

// answering attaches a client that gives answer to every create asked, and
// records each approval it saw.
func answering(t *testing.T, e *Engine, answer string) *asked {
	t.Helper()
	events, unsubscribe := e.Subscribe()
	detach := e.Attach()
	seen := &asked{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Kind != EventAsked {
				continue
			}
			seen.mu.Lock()
			seen.list = append(seen.list, *ev.Approval)
			seen.mu.Unlock()
			_ = e.Answer(ev.Approval.ID, answer, "the test")
		}
	}()
	t.Cleanup(func() {
		detach()
		unsubscribe()
		<-done
	})
	return seen
}

// deviceCreate is a create as Topos sends one for a device session.
func deviceCreate(id, folder string, labels map[string]string) runtime.CreateSpec {
	return runtime.CreateSpec{
		ID: id, Name: id, Owner: "alice", Isolation: v1.IsolationProcess,
		Labels: labels, Annotations: map[string]string{testLabels.AgentName: "Site helper"},
		Workspace: runtime.Workspace{Host: folder, Path: folder},
	}
}

// run executes a command in a sandbox through the plane and answers its exit
// code and output.
func run(t *testing.T, d runtime.Driver, sandbox string, command ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	x, err := d.Exec(ctx, sandbox, runtime.ExecRequest{Command: command})
	if err != nil {
		t.Fatalf("exec %v: %v", command, err)
	}
	var out, errOut strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&out, x.Stdout()) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errOut, x.Stderr()) }()
	wg.Wait()
	code, err := x.Wait(ctx)
	if err != nil {
		t.Fatalf("exec %v: %v", command, err)
	}
	if code != 0 {
		t.Logf("exec %v exited %d; stderr %q", command, code, errOut.String())
	}
	return code, out.String()
}
