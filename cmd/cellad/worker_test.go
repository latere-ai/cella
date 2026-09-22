// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestWorkerEndToEnd runs both roles of spec 021 in one process over
// loopback: cellad serve with the native driver, and cellad worker with its
// own native driver, joined by a key minted through the route an operator
// uses. Nothing dials the worker; the worker opens the one connection, which
// is invariant 10 of spec 001.
func TestWorkerEndToEnd(t *testing.T) {
	p := startPlane(t, "", "")
	key := p.environmentKey(t)

	// The worker reads its own variables and none of the control plane's.
	first := startWorker(t, p, key, map[string]string{"CELLA_WORKER_LABELS": "region=eu"})

	// The worker announces what it runs before it connects, so an operator
	// reading the log sees the driver and the control plane it joined.
	waitFor(t, "the worker to announce itself",
		func() bool { return strings.Contains(first.out.String(), "worker driver=native") })

	// The environment reports the worker once its stream is up. This is the
	// whole of the seam: the key authenticated a registration and a stream,
	// and the control plane made no outbound connection at all.
	waitFor(t, "the environment to report the worker", func() bool { return p.environment(t).Status.Workers == 1 })
	obj := p.environment(t)
	if obj.Status.LastHeartbeat.IsZero() {
		t.Errorf("the environment reports a worker and no heartbeat: %+v", obj.Status)
	}

	// A worker that goes away leaves the environment with none, which is
	// what the phase of spec 021 is computed from.
	if code := first.stop(); code != 0 {
		t.Errorf("the worker exited %d; stderr %q", code, first.errOut.String())
	}
	waitFor(t, "the environment to report no worker", func() bool { return p.environment(t).Status.Workers == 0 })

	// And a worker that comes back registers again and the environment holds
	// one once more, without anything having dialed it.
	startWorker(t, p, key, nil)
	waitFor(t, "the environment to report the worker that came back",
		func() bool { return p.environment(t).Status.Workers == 1 })
}

// runningWorker is one cellad worker running in this process: its output, and
// the stop that ends it and reports the exit code once.
type runningWorker struct {
	out, errOut *syncBuffer
	stop        func() int
}

// startWorker runs the worker role against the plane with the key an
// administrator minted, under the variables the role reads and no others.
func startWorker(t *testing.T, p *plane, key string, extra map[string]string) *runningWorker {
	t.Helper()
	e := map[string]string{
		"CELLA_URL":                 p.url,
		"CELLA_ENVIRONMENT_KEY":     key,
		"CELLA_RUNTIME":             "native",
		"CELLA_ALLOW_UNSAFE_NATIVE": "true",
		"CELLA_DATA_DIR":            t.TempDir(),
	}
	maps.Copy(e, extra)
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	var out, errOut syncBuffer
	go func() { codec <- run(ctx, []string{"worker"}, env(e), &out, &errOut) }()
	w := &runningWorker{out: &out, errOut: &errOut}
	w.stop = sync.OnceValue(func() int {
		cancel()
		select {
		case code := <-codec:
			return code
		case <-time.After(30 * time.Second):
			t.Errorf("the worker did not stop; stdout %q stderr %q", out.String(), errOut.String())
			return -1
		}
	})
	t.Cleanup(func() { w.stop() })
	return w
}

// TestWorkerRefusesARevokedKey holds spec 021's rule at the seam: the key is
// what authenticates a worker, and revoking it ends the registration route
// for that worker at once.
func TestWorkerRefusesARevokedKey(t *testing.T) {
	p := startPlane(t, "", "")
	status, body := p.do(t, http.MethodPost, "/v1/environments/default/keys", nil)
	if status != http.StatusCreated {
		t.Fatalf("the key mint answered %d: %s", status, body)
	}
	var minted struct {
		Token string `json:"token"`
		JTI   string `json:"jti"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	status, body = p.do(t, http.MethodDelete, "/v1/environments/default/keys/"+minted.JTI, nil)
	if status != http.StatusNoContent {
		t.Fatalf("the revocation answered %d: %s", status, body)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		p.url+"/v1/environments/self/workers", strings.NewReader(`{"driver":"native","isolation":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked key registered a worker with %s", res.Status)
	}
}

// TestWorkerRoleRefusesItsConfiguration holds that the role fails to start
// rather than running half configured, which is spec 002's rule for a role.
func TestWorkerRoleRefusesItsConfiguration(t *testing.T) {
	var out, errOut syncBuffer
	code := run(t.Context(), []string{"worker"}, env(map[string]string{
		"CELLA_URL": "http://control.example.test", "CELLA_ENVIRONMENT_KEY": "k",
	}), &out, &errOut)
	if code != 1 {
		t.Errorf("a worker with an http control plane off loopback exited %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "CELLA_INSECURE_CONTROL_PLANE") {
		t.Errorf("the refusal does not name the hatch: %q", errOut.String())
	}
	var vOut, vErr syncBuffer
	if code = run(t.Context(), []string{"worker", "-version"}, env(nil), &vOut, &vErr); code != 0 {
		t.Errorf("cellad worker -version exited %d: %q", code, vErr.String())
	}
	if strings.TrimSpace(vOut.String()) == "" {
		t.Errorf("cellad worker -version printed nothing")
	}
}

// environment reads the environment object the control plane serves.
func (p *plane) environment(t *testing.T) v1.Environment {
	t.Helper()
	status, body := p.do(t, http.MethodGet, "/v1/environments/default", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the environment answered %d: %s", status, body)
	}
	var obj v1.Environment
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("the environment did not decode: %v", err)
	}
	return obj
}

// TestWorkerSelectsItsDriver proves each of the three drivers is reached from
// the worker's own configuration: the role fails at that driver's preflight,
// naming what it was pointed at, rather than at the selection.
func TestWorkerSelectsItsDriver(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	kubeconfig := filepath.Join(t.TempDir(), "absent")
	for _, tc := range []struct {
		name  string
		env   map[string]string
		names string
	}{
		{"podman", map[string]string{"CELLA_RUNTIME": "podman", "CELLA_PODMAN_SOCKET": socket}, socket},
		{"k8s", map[string]string{"CELLA_RUNTIME": "k8s", "CELLA_K8S_KUBECONFIG": kubeconfig}, "kubeconfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := map[string]string{
				"CELLA_URL":             "http://127.0.0.1:1",
				"CELLA_ENVIRONMENT_KEY": "a-key",
				"CELLA_DATA_DIR":        t.TempDir(),
			}
			maps.Copy(e, tc.env)
			var out, errOut syncBuffer
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			code := run(ctx, []string{"worker"}, env(e), &out, &errOut)
			if code != 1 {
				t.Fatalf("a worker whose driver cannot work exited %d; stderr %q", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.names) {
				t.Errorf("the refusal does not name %s: %q", tc.names, errOut.String())
			}
		})
	}
}

// TestUnknownSubcommand holds spec 002's usage rule: a name that is not a
// role is a usage error naming every role, and never a silent start.
func TestUnknownSubcommand(t *testing.T) {
	var out, errOut syncBuffer
	if code := run(t.Context(), []string{"teleport"}, env(nil), &out, &errOut); code != 2 {
		t.Errorf("an unknown subcommand exited %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "worker") {
		t.Errorf("the usage error does not name the worker role: %q", errOut.String())
	}
}

// TestWorkerRoleRefusesADataDirItCannotMake holds that the role fails at
// start rather than running with nowhere to keep what its driver needs.
func TestWorkerRoleRefusesADataDirItCannotMake(t *testing.T) {
	// A file where the directory should be: the role cannot make one there.
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut syncBuffer
	code := run(t.Context(), []string{"worker"}, env(map[string]string{
		"CELLA_URL":             "http://127.0.0.1:1",
		"CELLA_ENVIRONMENT_KEY": "a-key",
		"CELLA_RUNTIME":         "podman",
		"CELLA_DATA_DIR":        filepath.Join(blocked, "under"),
	}), &out, &errOut)
	if code != 1 {
		t.Errorf("a worker with no data directory exited %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "CELLA_DATA_DIR") {
		t.Errorf("the refusal does not name the directory: %q", errOut.String())
	}
}

// TestWorkerRoleRefusesAFlagItDoesNotHave holds spec 002's usage rule for the
// role's own flags: a flag outside its set is a usage error, not a start.
func TestWorkerRoleRefusesAFlagItDoesNotHave(t *testing.T) {
	var out, errOut syncBuffer
	if code := run(t.Context(), []string{"worker", "-teleport"}, env(nil), &out, &errOut); code != 2 {
		t.Errorf("a flag the role does not have exited %d, want 2", code)
	}
}

// TestWorkerEnvironmentEndToEnd is the whole of slice 054 over loopback:
// `cellad serve` with its own driver, an environment an administrator applies
// and keys, `cellad worker` that registers on it, a sandbox created on that
// environment through the API and an exec that runs on the worker, and the
// phase that follows the worker away and back.
func TestWorkerEnvironmentEndToEnd(t *testing.T) {
	// The offline window is short here so the case observes the transition
	// rather than the two minutes a deployment holds an environment for.
	p := startPlaneWith(t, "", "", map[string]string{"CELLA_ENVIRONMENT_OFFLINE": "2s"})

	// The operator applies the environment and mints it a key. Nothing is
	// placed on it yet: no worker has registered, so it is Pending.
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment",` +
		`"metadata":{"name":"eu-gpu"},` +
		`"spec":{"mode":"worker","isolation":"none","capacity":{"cpu":"8","memory":"16Gi","sandboxes":10}}}`
	status, answer := p.do(t, http.MethodPut, "/v1/environments/eu-gpu", strings.NewReader(body))
	if status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	if got := p.environmentNamed(t, "eu-gpu").Status.Phase; got != v1.EnvironmentPending {
		t.Errorf("an environment nothing has registered on is %q", got)
	}
	status, answer = p.do(t, http.MethodPost, "/v1/environments/eu-gpu/keys", nil)
	if status != http.StatusCreated {
		t.Fatalf("the key mint answered %d: %s", status, answer)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(answer), &minted); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}

	// The worker connects outbound with that key and the environment becomes
	// placeable. Nothing dialed the worker.
	first := startWorker(t, p, minted.Token, nil)
	waitFor(t, "the environment to report the worker", func() bool {
		obj := p.environmentNamed(t, "eu-gpu")
		return obj.Status.Phase == v1.EnvironmentReady && obj.Status.Workers == 1
	})
	// The driver an environment reports is the one its workers run, which
	// spec 021 records from the first registration; `remote` is only how the
	// control plane reaches them.
	if got := p.environmentNamed(t, "eu-gpu").Status.Driver; got != "native" {
		t.Errorf("the environment reports the driver %q, want native", got)
	}

	// A sandbox named onto that environment is created on the worker's own
	// driver, and an exec runs there.
	create := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"there"},"spec":{"environment":"eu-gpu","command":["sleep","300"]}}`
	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(create))
	if status != http.StatusCreated {
		t.Fatalf("the create on the worker's environment answered %d: %s", status, answer)
	}
	var sandbox v1.Sandbox
	if err := json.Unmarshal([]byte(answer), &sandbox); err != nil {
		t.Fatalf("the sandbox did not decode: %v", err)
	}
	if sandbox.Status.Environment != "eu-gpu" {
		t.Fatalf("the sandbox names environment %q", sandbox.Status.Environment)
	}
	waitFor(t, "the sandbox to run on the worker", func() bool {
		status, answer := p.do(t, http.MethodGet, "/v1/sandboxes/"+sandbox.Status.ID, nil)
		if status != http.StatusOK {
			return false
		}
		var obj v1.Sandbox
		return json.Unmarshal([]byte(answer), &obj) == nil && obj.Status.Phase == "Running"
	})
	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes/"+sandbox.Status.ID+"/exec?wait=1",
		strings.NewReader(`{"command":["sh","-c","echo across the seam"]}`))
	if status != http.StatusOK {
		t.Fatalf("the exec on the worker answered %d: %s", status, answer)
	}
	if !strings.Contains(answer, "across the seam") {
		t.Errorf("the exec's output is %s", answer)
	}

	// The worker goes away and the environment reports it: the phase loop
	// writes Offline once the window has passed with nothing answering.
	if code := first.stop(); code != 0 {
		t.Errorf("the worker exited %d; stderr %q", code, first.errOut.String())
	}
	waitFor(t, "the environment to go offline", func() bool {
		obj := p.environmentNamed(t, "eu-gpu")
		return obj.Status.Phase == v1.EnvironmentOffline && obj.Status.Reason == v1.ReasonHeartbeatLost
	})
	// An offline environment takes no new sandbox and keeps the one it has.
	again := strings.Replace(create, `"name":"there"`, `"name":"after"`, 1)
	if status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(again)); status != http.StatusServiceUnavailable {
		t.Errorf("a create on an offline environment answered %d: %s", status, answer)
	}
	if status, _ = p.do(t, http.MethodGet, "/v1/sandboxes/"+sandbox.Status.ID, nil); status != http.StatusOK {
		t.Errorf("the sandbox already placed did not survive the environment going offline")
	}

	// And a worker that comes back makes the environment placeable again.
	startWorker(t, p, minted.Token, nil)
	waitFor(t, "the environment to return to Ready", func() bool {
		return p.environmentNamed(t, "eu-gpu").Status.Phase == v1.EnvironmentReady
	})
	if status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(again)); status != http.StatusCreated {
		t.Errorf("a create on the environment that returned answered %d: %s", status, answer)
	}
}

// environmentNamed reads one environment object the control plane serves.
func (p *plane) environmentNamed(t *testing.T, name string) v1.Environment {
	t.Helper()
	status, body := p.do(t, http.MethodGet, "/v1/environments/"+name, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the environment %s answered %d: %s", name, status, body)
	}
	var obj v1.Environment
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("the environment did not decode: %v", err)
	}
	return obj
}
