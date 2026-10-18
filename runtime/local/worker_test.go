// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// stubPlane is a control plane that skips resolve: it registers a worker,
// serves its stream, and hands back a driver that sends each create exactly
// as the test writes it, which is what the host's built-in checks have to
// hold against.
type stubPlane struct {
	hub    *remote.Hub
	url    string
	driver driver.Driver
}

func newStubPlane(t *testing.T) *stubPlane {
	t.Helper()
	p := &stubPlane{hub: remote.NewHub(remote.HubOptions{Offline: time.Minute})}
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/workers", func(w http.ResponseWriter, r *http.Request) {
		var registration remote.Registration
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		registered, err := p.hub.Register("env_laptop", registration)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		registered.Environment = "env_laptop"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(registered)
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = p.hub.Serve(r.Context(), "env_laptop", worker.NewSocket(conn))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	p.url = server.URL
	d, err := remote.New(remote.Options{Environment: "env_laptop", Transport: p.hub.Transport("env_laptop")})
	if err != nil {
		t.Fatal(err)
	}
	p.driver = d
	return p
}

// serve runs a worker on the local driver against the plane until the
// returned stop is called, and waits for it to connect.
func (p *stubPlane) serve(t *testing.T, o worker.Options) (stop func() error) {
	t.Helper()
	o.URL, o.Version = p.url, "test"
	o.Key = func(context.Context) (string, error) { return "an-environment-key", nil }
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- worker.Run(ctx, o) }()
	var result error
	done := false
	stop = func() error {
		if !done {
			cancel()
			select {
			case result = <-ended:
			case <-time.After(20 * time.Second):
				t.Error("the worker did not stop inside the deadline")
			}
			done = true
		}
		return result
	}
	t.Cleanup(func() { _ = stop() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if workers := p.hub.Workers("env_laptop"); len(workers) == 1 && workers[0].Connected {
			return stop
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// allowAll is a host policy that admits every create and allows every
// operation, so what is refused is refused by the built-in checks.
type allowAll struct{ asked []worker.Create }

func (a *allowAll) Admit(_ context.Context, c worker.Create) worker.Verdict {
	a.asked = append(a.asked, c)
	return worker.Verdict{Allow: true}
}

func (*allowAll) Check(context.Context, worker.Operation) worker.Verdict {
	return worker.Verdict{Allow: true}
}

// TestLocalRegistrationCarriesHost is spec 082's criterion 7 on the local
// driver: the worker registers the operating system, architecture, grants
// and network the driver reports.
func TestLocalRegistrationCarriesHost(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}, Lookup: os.LookupEnv})
	p := newStubPlane(t)
	p.serve(t, worker.Options{Driver: d})
	workers := p.hub.Workers("env_laptop")
	want := v1.HostInfo{
		OS: d.goos, Arch: d.Host().Arch,
		Grants:  []v1.HostGrant{{Path: h.project, Write: true}, {Path: h.docs}},
		Network: v1.HostNetwork{Mode: NetworkAllowlist, Hosts: []string{"github.com"}},
	}
	if workers[0].Host == nil || !workers[0].Host.Equal(want) {
		t.Errorf("the worker registered %+v, want %+v", workers[0].Host, want)
	}
}

// TestLocalBuiltInChecksIgnoreThePolicy is spec 082's criterion 4 on the
// local driver, behind a control plane that skips resolve: with a policy that
// allows everything, a workspace.host outside the grants is refused with
// workspace_not_granted before the policy is asked, and a create asking for
// open egress on a host that allows two hosts runs with the host's list.
func TestLocalBuiltInChecksIgnoreThePolicy(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "proxy.golang.org"}}, Lookup: os.LookupEnv})
	p := newStubPlane(t)
	policy := &allowAll{}
	p.serve(t, worker.Options{Driver: d, Policy: policy})

	outside := filepath.Join(h.home, "elsewhere")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_outside", Owner: "ops", Isolation: v1.IsolationProcess,
		Workspace: driver.Workspace{Path: outside, Host: outside},
	})
	if !errors.Is(err, driver.ErrWorkspaceNotGranted) {
		t.Fatalf("a create outside the grants answered %v, want workspace_not_granted", err)
	}
	if len(policy.asked) != 0 {
		t.Errorf("the policy was asked about a folder the host does not grant: %+v", policy.asked)
	}

	if _, err = p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_open", Owner: "ops", Isolation: v1.IsolationProcess,
		Workspace: driver.Workspace{Path: h.project, Host: h.project},
		Egress:    driver.Egress{Mode: string(v1.EgressOpen)},
	}); err != nil {
		t.Fatalf("a create inside the grants asking for open egress failed: %v", err)
	}
	t.Cleanup(func() { _ = d.Delete(context.Background(), "sbx_open") })
	if len(policy.asked) != 1 || policy.asked[0].Sandbox.Spec.Network.Egress.Mode != v1.EgressOpen {
		t.Errorf("the policy was asked %+v", policy.asked)
	}
	network := d.stage(mustLoad(t, d, "sbx_open"), "probe", []string{"true"}, nil, h.project).Network
	if network.Mode != "allowlist" || !slices.Equal(network.Domains, []string{"github.com", "proxy.golang.org"}) {
		t.Errorf("the sandbox runs with the network %+v, want the host's list", network)
	}
	e, err := p.driver.Exec(t.Context(), "sbx_open", driver.ExecRequest{Command: []string{"echo", "ran"}})
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	out, _ := io.ReadAll(e.Stdout())
	if code, err := e.Wait(t.Context()); err != nil || code != 0 || string(out) != "ran\n" {
		t.Errorf("the exec answered %d, %v with %q", code, err, out)
	}
}

// TestLocalRunStopsItsSandboxes is spec 082's criterion 8 on the local
// driver. When the worker's context ends it stops the sandbox: its main
// stage and the stage of an exec started through it are gone. With
// KeepOnExit the main stage, a detached stage, keeps running; the exec's own
// stage ends with its operation, because the seam closes every exec whose
// stream ended. What survives a worker that does not stop on purpose is
// every detached stage: a driver opened over the same data directory after
// the first was dropped still finds the exec's stage running.
func TestLocalRunStopsItsSandboxes(t *testing.T) {
	start := func(t *testing.T, keep bool) (*Driver, func() error, hostsandbox.StageHandle, hostsandbox.StageHandle) {
		t.Helper()
		shim(t)
		h := newHost(t)
		d := h.open(t, Options{Lookup: os.LookupEnv})
		p := newStubPlane(t)
		stop := p.serve(t, worker.Options{Driver: d, KeepOnExit: keep})
		if _, err := p.driver.Create(t.Context(), driver.CreateSpec{
			ID: "sbx_left", Owner: "ops", Isolation: v1.IsolationProcess, Command: []string{"sleep", "300"},
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Delete(context.Background(), "sbx_left") })
		if _, err := p.driver.Exec(t.Context(), "sbx_left", driver.ExecRequest{Command: []string{"sleep", "300"}}); err != nil {
			t.Fatal(err)
		}
		r := mustLoad(t, d, "sbx_left")
		if r.Main == nil || len(r.Stages) != 1 {
			t.Fatalf("the sandbox holds the main stage %v and the stages %v", r.Main, r.Stages)
		}
		return d, stop, *r.Main, r.Stages[0]
	}
	running := func(t *testing.T, d *Driver, handle hostsandbox.StageHandle) bool {
		t.Helper()
		status, err := d.sandbox.Observe(context.Background(), handle)
		if err != nil {
			t.Fatal(err)
		}
		return status.Running
	}

	t.Run("a context that ends stops the stages", func(t *testing.T) {
		d, stop, main, exec := start(t, false)
		if err := stop(); err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if running(t, d, main) || running(t, d, exec) {
			t.Error("a stage runs after the worker stopped")
		}
		if state, err := d.Inspect(context.Background(), "sbx_left"); err != nil || state.Phase != driver.Stopped {
			t.Errorf("the sandbox reads %s, %v, want Stopped", state.Phase, err)
		}
	})
	t.Run("KeepOnExit leaves the main stage running", func(t *testing.T) {
		d, stop, main, exec := start(t, true)
		if err := stop(); err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if !running(t, d, main) {
			t.Error("the main stage ended with the worker")
		}
		if running(t, d, exec) {
			t.Error("the exec's stage outlived its operation")
		}
	})
	t.Run("a dropped driver leaves every detached stage running", func(t *testing.T) {
		shim(t)
		h := newHost(t)
		data := tempDir(t)
		first := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
		sandbox(t, first, driver.CreateSpec{ID: "sbx_crash"})
		if _, err := first.Exec(context.Background(), "sbx_crash", driver.ExecRequest{Command: []string{"sleep", "300"}}); err != nil {
			t.Fatal(err)
		}
		stages := mustLoad(t, first, "sbx_crash").Stages
		second := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
		if len(stages) != 1 || !running(t, second, stages[0]) {
			t.Fatalf("a second driver finds the stages %v not running", stages)
		}
		if err := second.Stop(context.Background(), "sbx_crash"); err != nil {
			t.Fatal(err)
		}
		if running(t, second, stages[0]) {
			t.Error("the stage runs after the second driver stopped the sandbox")
		}
	})
}
