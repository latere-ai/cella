// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/server"

	"latere.ai/x/cella/authorizer"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// applyAs applies one Environment as a subject and answers the status.
func applyAs(t *testing.T, p *plane, bearer, name, spec string) (int, string) {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"` + name + `"},"spec":` + spec + `}`
	return p.doAs(t, bearer, http.MethodPut, "/v1/environments/"+name, strings.NewReader(body))
}

// mintAs mints one key of an environment as a subject and returns it and its
// jti, failing the test on any status but 201.
func mintAs(t *testing.T, p *plane, bearer, name, body string) (string, string) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var status int
	var answer string
	if reader == nil {
		status, answer = p.doAs(t, bearer, http.MethodPost, "/v1/environments/"+name+"/keys", nil)
	} else {
		status, answer = p.doAs(t, bearer, http.MethodPost, "/v1/environments/"+name+"/keys", reader)
	}
	if status != http.StatusCreated {
		t.Fatalf("the key mint for %s answered %d: %s", name, status, answer)
	}
	var minted struct {
		Token string `json:"token"`
		JTI   string `json:"jti"`
	}
	if err := json.Unmarshal([]byte(answer), &minted); err != nil {
		t.Fatal(err)
	}
	return minted.Token, minted.JTI
}

// nativeHost is a native driver this test closes with it.
func nativeHost(t *testing.T) *native.Driver {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// runEmbedded runs a program's embedded worker with a key, and returns the
// stop that ends it and the channel Run's answer arrives on.
func runEmbedded(t *testing.T, p *plane, key string, o worker.Options) (func(), <-chan error) {
	t.Helper()
	if o.Driver == nil {
		o.Driver = nativeHost(t)
	}
	o.URL, o.Key, o.Version = p.url, func(context.Context) (string, error) { return key, nil }, "test"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, o) }()
	t.Cleanup(cancel)
	return cancel, done
}

// runConnected is runEmbedded that returns once the worker's stream is open,
// its upgrade authenticated, which is what a test of how that stream closes
// acts on: a registration alone counts in status.workers before the stream
// opens.
func runConnected(t *testing.T, p *plane, key string, o worker.Options) <-chan error {
	t.Helper()
	connected := make(chan struct{})
	var once sync.Once
	o.State = func(s worker.State) {
		if s.Connected {
			once.Do(func() { close(connected) })
		}
	}
	_, done := runEmbedded(t, p, key, o)
	select {
	case <-connected:
	case err := <-done:
		t.Fatalf("Run returned %v before its stream opened", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the worker's stream never opened")
	}
	return done
}

// heldAnswer is a worker's transport that holds the answer to its
// registration until release is closed, so a test acts after the control
// plane took the registration and before the worker opens its stream.
type heldAnswer struct{ release <-chan struct{} }

func (h heldAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/workers") {
		return res, err
	}
	select {
	case <-h.release:
		return res, nil
	case <-r.Context().Done():
		_ = res.Body.Close()
		return nil, r.Context().Err()
	}
}

// refusedWith waits for Run to return and reads the refusal it returned.
func refusedWith(t *testing.T, done <-chan error, within time.Duration) string {
	t.Helper()
	select {
	case err := <-done:
		var refused *worker.Refusal
		if !errors.As(err, &refused) {
			t.Fatalf("Run returned %v, want a refusal", err)
		}
		return refused.Code
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return ""
	}
}

// registerRaw registers a worker with a key as a worker would, and answers
// the status and the error code.
func registerRaw(t *testing.T, p *plane, key string, r remote.Registration) (int, string) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	status, answer := p.doAs(t, key, http.MethodPost, "/v1/environments/self/workers", bytes.NewReader(body))
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(answer), &envelope)
	return status, envelope.Error.Code
}

// workerSpec is a worker environment's spec with room for a few sandboxes.
const workerSpec = `{"mode":"worker","isolation":"none","capacity":{"sandboxes":4}}`

// TestOwnedEnvironmentUnderOwnerPolicy: with CELLA_OWNED_ENVIRONMENTS=worker
// a subject creates a worker environment, mints, lists and revokes its keys,
// and is refused another subject's environment, another environment's key,
// and any in-process environment; without the setting both are an
// administrator's alone.
func TestOwnedEnvironmentUnderOwnerPolicy(t *testing.T) {
	p := startPlaneWith(t, "", "", map[string]string{"CELLA_OWNED_ENVIRONMENTS": "worker"})
	bob, carol := p.mint("bob"), p.mint("carol")
	if status, answer := applyAs(t, p, bob, "bob-laptop", workerSpec); status != http.StatusCreated {
		t.Fatalf("bob's worker environment answered %d: %s", status, answer)
	}
	_, jti := mintAs(t, p, bob, "bob-laptop", "")
	if status, answer := p.doAs(t, bob, http.MethodGet, "/v1/environments/bob-laptop/keys", nil); status != http.StatusOK || !strings.Contains(answer, jti) {
		t.Errorf("bob's key list answered %d: %s", status, answer)
	}
	if status, answer := p.doAs(t, bob, http.MethodGet, "/v1/environments", nil); status != http.StatusOK ||
		!strings.Contains(answer, "bob-laptop") || strings.Contains(answer, `"name":"default"`) {
		t.Errorf("bob's environment list answered %d: %s", status, answer)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/environments/bob-laptop"},
		{http.MethodPost, "/v1/environments/bob-laptop/keys"},
		{http.MethodDelete, "/v1/environments/bob-laptop/keys/" + jti},
		{http.MethodDelete, "/v1/environments/bob-laptop"},
	} {
		if status, _ := p.doAs(t, carol, c.method, c.path, nil); status != http.StatusForbidden {
			t.Errorf("carol's %s %s answered %d, want 403", c.method, c.path, status)
		}
	}
	// A key of bob's is no key of carol's environment, even named through
	// a route carol may use.
	if status, answer := applyAs(t, p, carol, "carol-desk", workerSpec); status != http.StatusCreated {
		t.Fatalf("carol's worker environment answered %d: %s", status, answer)
	}
	if status, answer := p.doAs(t, carol, http.MethodDelete, "/v1/environments/carol-desk/keys/"+jti, nil); status != http.StatusNotFound {
		t.Errorf("revoking another environment's key answered %d: %s", status, answer)
	}
	if status, _ := p.doAs(t, bob, http.MethodDelete, "/v1/environments/bob-laptop/keys/"+jti, nil); status != http.StatusNoContent {
		t.Errorf("bob's own revocation answered %d", status)
	}
	if status, _ := applyAs(t, p, bob, "bob-inproc", `{"mode":"inprocess","isolation":"none","capacity":{"sandboxes":1}}`); status != http.StatusForbidden {
		t.Errorf("bob's in-process environment answered %d, want 403", status)
	}
	if status, _ := p.doAs(t, bob, http.MethodDelete, "/v1/environments/bob-laptop", nil); status != http.StatusNoContent {
		t.Errorf("bob deleting his own environment answered %d", status)
	}

	closed := startPlane(t, "", "")
	if status, _ := applyAs(t, closed, closed.mint("bob"), "bob-laptop", workerSpec); status != http.StatusForbidden {
		t.Errorf("without the setting bob's worker environment answered %d, want 403", status)
	}
}

// recordingDecider allows everything and keeps every request, except that
// environment.serve answers what serve holds: allowed with a TTL of a
// second, or refused.
type recordingDecider struct {
	mu       sync.Mutex
	requests []authz.Request
	refuse   atomic.Bool
}

func (d *recordingDecider) Decide(_ context.Context, req authz.Request) (authz.Decision, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.mu.Unlock()
	if req.Action == authorizer.ActionEnvironmentServe {
		if d.refuse.Load() {
			return authz.Decision{Reason: "removed"}, nil
		}
		return authz.Decision{Allow: true, TTL: time.Second}, nil
	}
	return authz.Decision{Allow: true}, nil
}

func (d *recordingDecider) of(action string) []authz.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []authz.Request
	for _, r := range d.requests {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// startAuthorized is a plane whose authorizer is an endpoint over d, so every
// question crosses the wire as JSON.
func startAuthorized(t *testing.T, d *recordingDecider) *plane {
	t.Helper()
	const token = "a-bearer-for-the-endpoint"
	endpoint := httptest.NewServer(server.New(server.Options{
		Bearer: token, Vocabulary: authorizer.Vocabulary(), Decider: d,
	}))
	t.Cleanup(endpoint.Close)
	return startPlaneWith(t, "", "", map[string]string{
		"CELLA_AUTHORIZER_URL": endpoint.URL, "CELLA_AUTHORIZER_TOKEN": token,
	})
}

// TestAuthorizerSeesModeWorkersAndTTL: every environment.* question carries
// the environment's mode and its worker limit, a key mint the lifetime the
// key gets, and environment.serve the worker's registration, read back from
// the JSON the endpoint received.
func TestAuthorizerSeesModeWorkersAndTTL(t *testing.T) {
	d := &recordingDecider{}
	p := startAuthorized(t, d)
	if status, answer := applyAs(t, p, p.bearer, "two-workers",
		`{"mode":"worker","isolation":"none","capacity":{"sandboxes":4},"workers":{"max":2}}`); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "two-workers", `{"ttl":"1h"}`)
	if status, _ := p.do(t, http.MethodGet, "/v1/environments/two-workers", nil); status != http.StatusOK {
		t.Fatalf("the read answered %d", status)
	}
	if status, code := registerRaw(t, p, key, remote.Registration{Driver: "native", Isolation: "none",
		Host: &v1.HostInfo{OS: "linux", Arch: "amd64"}}); status != http.StatusCreated {
		t.Fatalf("the registration answered %d %s", status, code)
	}
	for _, action := range []string{authorizer.ActionEnvironmentCreate, authorizer.ActionEnvironmentKey,
		authorizer.ActionEnvironmentRead, authorizer.ActionEnvironmentServe} {
		asked := d.of(action)
		if len(asked) == 0 {
			t.Errorf("%s was never asked", action)
			continue
		}
		fields := asked[len(asked)-1].Resource.Fields
		if fields["mode"] != v1.EnvironmentWorker || fields["workers"] != float64(2) {
			t.Errorf("%s carried mode %v and workers %v", action, fields["mode"], fields["workers"])
		}
	}
	if ttl := d.of(authorizer.ActionEnvironmentKey)[0].Resource.Fields["ttl"]; ttl != float64(3600) {
		t.Errorf("the key mint carried the ttl %v, want 3600", ttl)
	}
	serve := d.of(authorizer.ActionEnvironmentServe)[0]
	registration, _ := serve.Resource.Fields["registration"].(map[string]any)
	host, _ := registration["host"].(map[string]any)
	if !strings.HasPrefix(serve.Subject, "environment:") || registration["driver"] != "native" || host["os"] != "linux" {
		t.Errorf("environment.serve was asked as %s with %v", serve.Subject, registration)
	}
	if status, answer := p.do(t, http.MethodPost, "/v1/environments/two-workers/keys", strings.NewReader(`{"ttl":"100000h"}`)); status != http.StatusUnprocessableEntity ||
		!strings.Contains(answer, "ceiling_exceeded") {
		t.Errorf("a key longer than the configured lifetime answered %d: %s", status, answer)
	}
	for _, body := range []string{`{"ttl":"soon"}`, `{"ttl":"-1h"}`, `{"lifetime":"1h"}`} {
		if status, _ := p.do(t, http.MethodPost, "/v1/environments/two-workers/keys", strings.NewReader(body)); status != http.StatusBadRequest {
			t.Errorf("the key body %s answered %d, want 400", body, status)
		}
	}
}

// TestServeRefusedAtRegistration: a worker whose environment.serve the
// authorizer refuses is answered serve_refused, and an embedded worker stops
// on it rather than retrying.
func TestServeRefusedAtRegistration(t *testing.T) {
	d := &recordingDecider{}
	d.refuse.Store(true)
	p := startAuthorized(t, d)
	if status, answer := applyAs(t, p, p.bearer, "refused", workerSpec); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "refused", "")
	if status, code := registerRaw(t, p, key, remote.Registration{Driver: "native", Isolation: "none"}); status != http.StatusForbidden || code != remote.ReasonServeRefused {
		t.Errorf("a refused registration answered %d %s", status, code)
	}
	_, done := runEmbedded(t, p, key, worker.Options{})
	if code := refusedWith(t, done, 20*time.Second); code != remote.ReasonServeRefused {
		t.Errorf("the embedded worker stopped on %s", code)
	}
}

// TestServeRefusedOnRecheck: a live stream is asked environment.serve again
// on its decision's TTL, and closes with serve_refused within one TTL once
// the authorizer refuses; the worker stops and deletes what it ran.
func TestServeRefusedOnRecheck(t *testing.T) {
	d := &recordingDecider{}
	p := startAuthorized(t, d)
	if status, answer := applyAs(t, p, p.bearer, "rechecked", workerSpec); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "rechecked", "")
	host := nativeHost(t)
	_, done := runEmbedded(t, p, key, worker.Options{Driver: host})
	waitFor(t, "the environment to report the worker", func() bool { return p.environmentNamed(t, "rechecked").Status.Workers == 1 })
	id := sandboxOn(t, p, "rechecked", "doomed")
	waitFor(t, "the sandbox to run", func() bool { return sandboxNow(t, p, id).Status.Phase == driver.Running })
	asked := len(d.of(authorizer.ActionEnvironmentServe))
	waitFor(t, "a recheck", func() bool { return len(d.of(authorizer.ActionEnvironmentServe)) > asked })
	d.refuse.Store(true)
	refusedAt := time.Now()
	if code := refusedWith(t, done, 10*time.Second); code != remote.ReasonServeRefused {
		t.Fatalf("the stream closed with %s", code)
	}
	if took := time.Since(refusedAt); took > 3*time.Second {
		t.Errorf("the stream closed %v after the authorizer began refusing, past one TTL", took)
	}
	if states, err := host.List(context.Background(), driver.Filter{}); err != nil || len(states) != 0 {
		t.Errorf("after serve_refused the worker's driver holds %d sandboxes, %v", len(states), err)
	}
}

// TestRevokedKeyClosesItsStream: revoking the key a worker's stream
// registered with closes that stream with key_revoked, within the heartbeat
// interval. A key revoked before the stream opens refuses the stream's
// upgrade instead, which the worker retries, so the revocation waits for the
// stream.
func TestRevokedKeyClosesItsStream(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "revoked", workerSpec); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, jti := mintAs(t, p, p.bearer, "revoked", "")
	done := runConnected(t, p, key, worker.Options{})
	if status, _ := p.do(t, http.MethodDelete, "/v1/environments/revoked/keys/"+jti, nil); status != http.StatusNoContent {
		t.Fatalf("the revocation answered %d", status)
	}
	if code := refusedWith(t, done, remote.HeartbeatInterval); code != remote.ReasonKeyRevoked {
		t.Errorf("the stream closed with %s, want key_revoked", code)
	}
}

// TestDeleteWorkerEnvironmentWithSandboxes: deleting a worker environment a
// sandbox runs on succeeds, revokes its keys, closes its stream, drops the
// sandbox's record, and the worker's driver holds nothing once Run returns;
// the control plane's own environment with a sandbox on it is refused with
// phase_conflict.
func TestDeleteWorkerEnvironmentWithSandboxes(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "short-lived", workerSpec); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "short-lived", "")
	host := nativeHost(t)
	_, done := runEmbedded(t, p, key, worker.Options{Driver: host})
	waitFor(t, "the environment to report the worker", func() bool { return p.environmentNamed(t, "short-lived").Status.Workers == 1 })
	id := sandboxOn(t, p, "short-lived", "on-it")
	waitFor(t, "the sandbox to run", func() bool { return sandboxNow(t, p, id).Status.Phase == driver.Running })

	if status, answer := p.do(t, http.MethodDelete, "/v1/environments/short-lived", nil); status != http.StatusNoContent {
		t.Fatalf("deleting the worker environment answered %d: %s", status, answer)
	}
	if code := refusedWith(t, done, 20*time.Second); code != remote.ReasonServeRefused {
		t.Errorf("the stream closed with %s, want serve_refused", code)
	}
	if states, err := host.List(context.Background(), driver.Filter{}); err != nil || len(states) != 0 {
		t.Errorf("after the delete the worker's driver holds %d sandboxes, %v", len(states), err)
	}
	if status, _ := p.do(t, http.MethodGet, "/v1/sandboxes/"+id, nil); status != http.StatusNotFound {
		t.Errorf("the sandbox's record outlived its environment: %d", status)
	}
	if status, _ := registerRaw(t, p, key, remote.Registration{Driver: "native", Isolation: "none"}); status != http.StatusUnauthorized {
		t.Errorf("a key of the deleted environment registered with %d", status)
	}

	here := create(t, p.url, p.bearer, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"here"},"spec":{"command":["sleep","60"]}}`)
	if status, answer := p.do(t, http.MethodDelete, "/v1/environments/default", nil); status != http.StatusConflict || !strings.Contains(answer, "phase_conflict") {
		t.Errorf("deleting the in-process environment with a sandbox answered %d: %s", status, answer)
	}
	if status, _ := p.do(t, http.MethodDelete, "/v1/sandboxes/"+here.Status.ID, nil); status != http.StatusNoContent && status != http.StatusAccepted {
		t.Errorf("the sandbox on the default environment was not deleted: %d", status)
	}
}

// TestWorkersMax: on an environment that takes one worker, a second key's
// registration is refused with worker_limit; a registration with the live
// worker's key and the same host replaces it, closing its stream with
// worker_replaced; one with that key and another host is refused; and
// environment.worker_replaced is recorded both times.
func TestWorkersMax(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "one-only",
		`{"mode":"worker","isolation":"none","capacity":{"sandboxes":4},"workers":{"max":1}}`); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	first, _ := mintAs(t, p, p.bearer, "one-only", "")
	second, _ := mintAs(t, p, p.bearer, "one-only", "")
	here := v1.HostInfo{OS: "linux", Arch: "amd64", Grants: []v1.HostGrant{{Path: "/home/dev", Write: true}}}
	done := runConnected(t, p, first, worker.Options{Driver: hostNative{Driver: nativeHost(t), host: here}})

	if status, code := registerRaw(t, p, second, remote.Registration{Driver: "native", Isolation: "none"}); status != http.StatusConflict || code != remote.ReasonWorkerLimit {
		t.Errorf("a second key's registration answered %d %s", status, code)
	}
	if status, code := registerRaw(t, p, first, remote.Registration{Driver: "native", Isolation: "none", Host: &here}); status != http.StatusCreated {
		t.Fatalf("the same key from the same host answered %d %s", status, code)
	}
	if code := refusedWith(t, done, 10*time.Second); code != remote.ReasonWorkerReplaced {
		t.Errorf("the replaced worker stopped on %s", code)
	}
	elsewhere := here
	elsewhere.OS = "darwin"
	if status, code := registerRaw(t, p, first, remote.Registration{Driver: "native", Isolation: "none", Host: &elsewhere}); status != http.StatusConflict || code != remote.ReasonWorkerLimit {
		t.Errorf("the same key from another host answered %d %s", status, code)
	}
	status, answer := p.do(t, http.MethodGet, "/v1/events?object=one-only&limit=100", nil)
	if status != http.StatusOK {
		t.Fatalf("the feed answered %d: %s", status, answer)
	}
	if replaced, refused := strings.Count(answer, `"outcome":"replaced"`), strings.Count(answer, `"outcome":"refused"`); replaced != 1 || refused != 1 ||
		strings.Count(answer, "environment.worker_replaced") != 2 {
		t.Errorf("the environment's records hold %d replaced and %d refused worker_replaced records: %s", replaced, refused, answer)
	}
}

// TestWorkerReplacedBeforeItsStreamOpens: a registration with the same key
// from the same host that replaces a worker whose stream has not opened yet
// stops that worker with worker_replaced as soon as it opens its stream,
// rather than letting it register again and take the place back.
func TestWorkerReplacedBeforeItsStreamOpens(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "one-only",
		`{"mode":"worker","isolation":"none","capacity":{"sandboxes":4},"workers":{"max":1}}`); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	first, _ := mintAs(t, p, p.bearer, "one-only", "")
	here := v1.HostInfo{OS: "linux", Arch: "amd64", Grants: []v1.HostGrant{{Path: "/home/dev", Write: true}}}
	release := make(chan struct{})
	_, done := runEmbedded(t, p, first, worker.Options{
		Driver:     hostNative{Driver: nativeHost(t), host: here},
		HTTPClient: &http.Client{Transport: heldAnswer{release: release}},
	})
	// The control plane writes the environment's status before it answers
	// a registration, so the worker counts while its answer is held.
	waitFor(t, "the environment to report the worker", func() bool { return p.environmentNamed(t, "one-only").Status.Workers == 1 })
	if status, code := registerRaw(t, p, first, remote.Registration{Driver: "native", Isolation: "none", Host: &here}); status != http.StatusCreated {
		t.Fatalf("the same key from the same host answered %d %s", status, code)
	}
	close(release)
	if code := refusedWith(t, done, 10*time.Second); code != remote.ReasonWorkerReplaced {
		t.Errorf("the replaced worker stopped on %s", code)
	}
	status, answer := p.do(t, http.MethodGet, "/v1/events?object=one-only&limit=100", nil)
	if status != http.StatusOK {
		t.Fatalf("the feed answered %d: %s", status, answer)
	}
	if replaced := strings.Count(answer, `"outcome":"replaced"`); replaced != 1 {
		t.Errorf("the environment's records hold %d replacements, want the one: %s", replaced, answer)
	}
}

// TestACreateBeforeTheStreamWaitsForIt: a worker's registration makes its
// environment Ready before the worker opens its stream, and a sandbox created
// on it in between stays placed and runs once the stream opens, rather than
// failing with no worker to reach.
func TestACreateBeforeTheStreamWaitsForIt(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "early",
		`{"mode":"worker","isolation":"none","capacity":{"sandboxes":4}}`); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "early", "")
	release := make(chan struct{})
	runEmbedded(t, p, key, worker.Options{HTTPClient: &http.Client{Transport: heldAnswer{release: release}}})
	// The answer to the registration is held, so the worker has not dialed
	// its stream while the environment already reads Ready.
	waitFor(t, "the environment to read Ready from the registration", func() bool {
		obj := p.environmentNamed(t, "early")
		return obj.Status.Phase == v1.EnvironmentReady && obj.Status.Workers == 1
	})
	id := sandboxOn(t, p, "early", "before-the-stream")
	if obj := sandboxNow(t, p, id); obj.Status.Phase != driver.Pending {
		t.Errorf("with the stream not open the sandbox is %s %s, want Pending", obj.Status.Phase, obj.Status.Reason)
	}
	close(release)
	waitFor(t, "the sandbox to run once the stream opened", func() bool {
		obj := sandboxNow(t, p, id)
		if obj.Status.Phase == "Failed" {
			t.Fatalf("the sandbox failed with %s", obj.Status.Reason)
		}
		return obj.Status.Phase == driver.Running
	})
}

// recordingHost is a worker's driver that confines with its own allow list:
// it declares none and an allow list with EgressLocal, and keeps every create
// spec it is handed.
type recordingHost struct {
	*native.Driver
	mu    sync.Mutex
	specs []driver.CreateSpec
}

func (h *recordingHost) Capabilities() driver.Capabilities {
	c := h.Driver.Capabilities()
	c.Egress = []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}
	c.EgressLocal = true
	return c
}

func (h *recordingHost) Create(ctx context.Context, spec driver.CreateSpec) (driver.Ref, error) {
	h.mu.Lock()
	h.specs = append(h.specs, spec)
	h.mu.Unlock()
	spec.Egress = driver.Egress{}
	return h.Driver.Create(ctx, spec)
}

// TestEgressLocalNeedsNoGateway: with no gateway connected, a sandbox asking
// for an allow list on a worker environment whose driver enforces its own is
// created, is EgressEnforced True with DriverEnforced, and is handed no
// proxy, reverse address, credential or gateway authority; the same manifest
// on the control plane's own environment still needs a gateway.
func TestEgressLocalNeedsNoGateway(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "own-proxy", workerSpec); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "own-proxy", "")
	host := &recordingHost{Driver: nativeHost(t)}
	runEmbedded(t, p, key, worker.Options{Driver: host})
	waitFor(t, "the environment to report the worker", func() bool { return p.environmentNamed(t, "own-proxy").Status.Workers == 1 })

	manifest := func(environment, name string) string {
		return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
			`"spec":{"environment":"` + environment + `","command":["sleep","60"],"network":{"egress":{"mode":"allowlist","allowedHosts":["github.com"]}}}}`
	}
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes?wait=1", strings.NewReader(manifest("own-proxy", "confined")))
	if status != http.StatusCreated {
		t.Fatalf("the create on the driver-enforced environment answered %d: %s", status, answer)
	}
	var obj v1.Sandbox
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sandbox to run", func() bool { return sandboxNow(t, p, obj.Status.ID).Status.Phase == driver.Running })
	var enforced v1.Condition
	for _, c := range sandboxNow(t, p, obj.Status.ID).Status.Conditions {
		if c.Type == v1.ConditionEgressEnforced {
			enforced = c
		}
	}
	if enforced.Status != v1.ConditionTrue || enforced.Reason != v1.ReasonDriverEnforced {
		t.Errorf("EgressEnforced is %+v, want True with DriverEnforced", enforced)
	}
	host.mu.Lock()
	specs := append([]driver.CreateSpec(nil), host.specs...)
	host.mu.Unlock()
	if len(specs) != 1 {
		t.Fatalf("the driver was handed %d creates", len(specs))
	}
	e := specs[0].Egress
	if e.Mode != string(v1.EgressAllowlist) || len(e.AllowedHosts) != 1 || e.ProxyAddr != "" || e.ReverseAddr != "" || e.Credential != "" || e.CAPEM != "" {
		t.Errorf("the driver was handed the egress %+v, want the allow list and nothing of a gateway", e)
	}
	if status, answer := p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(manifest("default", "gatewayed"))); status != http.StatusServiceUnavailable ||
		!strings.Contains(answer, "egress_gateway_unavailable") {
		t.Errorf("an allow list on the control plane's own environment with no gateway answered %d: %s", status, answer)
	}
}
