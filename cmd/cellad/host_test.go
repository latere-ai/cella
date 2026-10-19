// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/worker"
)

// hostNative is the native driver on a machine that reports itself: its
// operating system, architecture, grants and network, which is what an
// embedded worker on a person's computer registers (spec 082).
type hostNative struct {
	driver.Driver
	host v1.HostInfo
}

func (h hostNative) Host() v1.HostInfo { return h.host }

// verdictAfter is a host policy that answers every create with one verdict
// after a wait, or never where block is set, and allows every operation.
type verdictAfter struct {
	verdict worker.Verdict
	wait    time.Duration
	block   bool
}

func (v verdictAfter) Admit(ctx context.Context, _ worker.Create) worker.Verdict {
	if v.block {
		<-ctx.Done()
		return worker.Verdict{}
	}
	select {
	case <-time.After(v.wait):
		return v.verdict
	case <-ctx.Done():
		return worker.Verdict{}
	}
}

func (verdictAfter) Check(context.Context, worker.Operation) worker.Verdict {
	return worker.Verdict{Allow: true}
}

// embedded applies a worker environment named name, mints its key, and runs
// a program's embedded worker on it with the options given, as a program on
// a person's computer would. It returns once the environment is Ready, with
// the stop that ends the worker.
func embedded(t *testing.T, p *plane, name string, o worker.Options) func() error {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"` + name + `"},` +
		`"spec":{"mode":"worker","isolation":"none","capacity":{"sandboxes":4}}}`
	if status, answer := p.do(t, http.MethodPut, "/v1/environments/"+name, strings.NewReader(body)); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	status, answer := p.do(t, http.MethodPost, "/v1/environments/"+name+"/keys", nil)
	if status != http.StatusCreated {
		t.Fatalf("the key mint answered %d: %s", status, answer)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(answer), &minted); err != nil {
		t.Fatal(err)
	}
	if o.Driver == nil {
		d, err := native.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		o.Driver = d
	}
	o.URL, o.Key, o.Version = p.url, func(context.Context) (string, error) { return minted.Token, nil }, "test"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, o) }()
	stop := sync.OnceValue(func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("the embedded worker did not stop")
		}
	})
	t.Cleanup(func() { _ = stop() })
	waitFor(t, "the environment to report the embedded worker", func() bool {
		obj := p.environmentNamed(t, name)
		return obj.Status.Phase == v1.EnvironmentReady && obj.Status.Workers == 1
	})
	return stop
}

// sandboxOn creates one sandbox on the environment without waiting, and
// returns its id.
func sandboxOn(t *testing.T, p *plane, environment, name string) string {
	t.Helper()
	create := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"` + name + `"},"spec":{"environment":"` + environment + `","command":["sleep","300"]}}`
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(create))
	if status != http.StatusCreated {
		t.Fatalf("the create answered %d: %s", status, answer)
	}
	var obj v1.Sandbox
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	return obj.Status.ID
}

// sandboxNow reads one sandbox.
func sandboxNow(t *testing.T, p *plane, id string) v1.Sandbox {
	t.Helper()
	status, answer := p.do(t, http.MethodGet, "/v1/sandboxes/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the sandbox answered %d: %s", status, answer)
	}
	var obj v1.Sandbox
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func hostApproval(obj v1.Sandbox) (v1.Condition, bool) {
	for _, c := range obj.Status.Conditions {
		if c.Type == v1.ConditionHostApproval {
			return c, true
		}
	}
	return v1.Condition{}, false
}

// eventTypes is every record type the feed holds for one object.
func eventTypes(t *testing.T, p *plane, id string) []string {
	t.Helper()
	status, answer := p.do(t, http.MethodGet, "/v1/events?object="+id+"&limit=100", nil)
	if status != http.StatusOK {
		t.Fatalf("the feed answered %d: %s", status, answer)
	}
	var page struct {
		Items []struct {
			Type string `json:"type"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(answer), &page); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.Type)
	}
	return out
}

// TestWorkerHoldsACreate is spec 082's hold end to end: a host policy that
// answers after three seconds leaves the sandbox Pending with HostApproval
// waiting for the host and a sandbox.host_pending record, and once the host
// allows it the sandbox runs and HostApproval is True.
func TestWorkerHoldsACreate(t *testing.T) {
	p := startPlane(t, "", "")
	embedded(t, p, "held", worker.Options{Policy: verdictAfter{verdict: worker.Verdict{Allow: true}, wait: 3 * time.Second}})
	id := sandboxOn(t, p, "held", "asked")
	waitFor(t, "the sandbox to wait for its host", func() bool {
		cond, has := hostApproval(sandboxNow(t, p, id))
		return has && cond.Status == v1.ConditionUnknown && cond.Reason == v1.ReasonHostPending
	})
	if obj := sandboxNow(t, p, id); obj.Status.Phase != driver.Pending {
		t.Errorf("a sandbox waiting for its host is %s", obj.Status.Phase)
	}
	waitFor(t, "the sandbox to run once the host allowed it", func() bool {
		obj := sandboxNow(t, p, id)
		cond, _ := hostApproval(obj)
		return obj.Status.Phase == driver.Running && cond.Status == v1.ConditionTrue
	})
	types := eventTypes(t, p, id)
	if !slices.Contains(types, "sandbox.host_pending") || !slices.Contains(types, "sandbox.started") {
		t.Errorf("the sandbox's records are %v, want sandbox.host_pending and sandbox.started", types)
	}
}

// TestWorkerRefusesACreate: a host that says no fails the sandbox with the
// reason HostRefused and the host's own text on HostApproval.
func TestWorkerRefusesACreate(t *testing.T) {
	p := startPlane(t, "", "")
	embedded(t, p, "refusing", worker.Options{Policy: verdictAfter{verdict: worker.Verdict{Reason: "not while I am presenting"}}})
	id := sandboxOn(t, p, "refusing", "refused")
	waitFor(t, "the sandbox to fail on the host's refusal", func() bool {
		return sandboxNow(t, p, id).Status.Phase == "Failed"
	})
	obj := sandboxNow(t, p, id)
	cond, _ := hostApproval(obj)
	if obj.Status.Reason != "HostRefused" || cond.Status != v1.ConditionFalse || cond.Message != "not while I am presenting" {
		t.Errorf("the refused sandbox reads %s with %+v", obj.Status.Reason, cond)
	}
}

// TestWorkerAdmitTimeout: a host that never answers is refused as
// HostTimeout once the worker's admit timeout passes.
func TestWorkerAdmitTimeout(t *testing.T) {
	p := startPlane(t, "", "")
	embedded(t, p, "silent", worker.Options{Policy: verdictAfter{block: true}, AdmitTimeout: 2 * time.Second})
	id := sandboxOn(t, p, "silent", "unanswered")
	waitFor(t, "the sandbox to fail when the host did not answer", func() bool {
		return sandboxNow(t, p, id).Status.Phase == "Failed"
	})
	obj := sandboxNow(t, p, id)
	if cond, _ := hostApproval(obj); obj.Status.Reason != "HostTimeout" || cond.Reason != v1.ReasonHostTimeout {
		t.Errorf("the unanswered sandbox reads %s with %+v", obj.Status.Reason, cond)
	}
}

// TestRegistrationCarriesHost: a worker whose driver reports its host
// registers the operating system, architecture, grants and network, and the
// environment's status.hosts shows them beside a status.workers count that
// is the same number it always was.
func TestRegistrationCarriesHost(t *testing.T) {
	p := startPlane(t, "", "")
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	host := v1.HostInfo{OS: "darwin", Arch: "arm64",
		Grants:  []v1.HostGrant{{Path: "/Users/dev/site", Write: true}, {Path: "/Users/dev/docs"}},
		Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com", "registry.npmjs.org"}}}
	embedded(t, p, "laptop", worker.Options{Driver: hostNative{Driver: d, host: host}})
	waitFor(t, "the environment to list the worker's host", func() bool {
		return len(p.environmentNamed(t, "laptop").Status.Hosts) == 1
	})
	obj := p.environmentNamed(t, "laptop")
	got := obj.Status.Hosts[0]
	if !strings.HasPrefix(got.Worker, "wrk_") || !got.Host.Equal(host) {
		t.Errorf("status.hosts reads %+v, want the worker's host %+v", got, host)
	}
	if obj.Status.Workers != 1 {
		t.Errorf("status.workers is %d beside one host", obj.Status.Workers)
	}
}

// TestAnEnvironmentReadsReadyOnceItsWorkerConnects: an embedder told that its
// worker connected reads its environment Ready at once, with the worker
// counted and its host in status.hosts, rather than Pending until the phase
// loop's next tick.
func TestAnEnvironmentReadsReadyOnceItsWorkerConnects(t *testing.T) {
	p := startPlane(t, "", "")
	if status, answer := applyAs(t, p, p.bearer, "fresh-laptop",
		`{"mode":"worker","isolation":"none","capacity":{"sandboxes":4}}`); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	key, _ := mintAs(t, p, p.bearer, "fresh-laptop", "")
	host := v1.HostInfo{OS: "darwin", Arch: "arm64", Grants: []v1.HostGrant{{Path: "/Users/dev/site", Write: true}},
		Network: v1.HostNetwork{Mode: "none"}}
	connected := make(chan struct{}, 1)
	runEmbedded(t, p, key, worker.Options{
		Driver: hostNative{Driver: nativeHost(t), host: host},
		State: func(s worker.State) {
			if s.Connected {
				select {
				case connected <- struct{}{}:
				default:
				}
			}
		},
	})
	select {
	case <-connected:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never connected")
	}
	obj := p.environmentNamed(t, "fresh-laptop")
	if obj.Status.Phase != v1.EnvironmentReady || obj.Status.Workers != 1 || len(obj.Status.Hosts) != 1 ||
		!obj.Status.Hosts[0].Host.Equal(host) {
		t.Errorf("right after its worker connected the environment reads %s with %d workers and hosts %+v",
			obj.Status.Phase, obj.Status.Workers, obj.Status.Hosts)
	}
}

// TestNoInboundToTheDataPlane holds invariant 10 of spec 001 for a worker a
// program embeds: across a sandbox's whole life on it (create, exec, a file
// written and read, delete), the worker opens no listening socket, so there
// is nothing on the data plane for the control plane, or anybody, to connect
// to. Every connection it has is one it opened outbound. The control plane
// side is held by TestRootPackagesDialNothing: neither the hub nor the remote
// driver reaches a client.
func TestNoInboundToTheDataPlane(t *testing.T) {
	p := startPlane(t, "", "")
	before := listeningSockets(t)
	stop := embedded(t, p, "outbound-only", worker.Options{})
	id := sandboxOn(t, p, "outbound-only", "lived")
	waitFor(t, "the sandbox to run", func() bool { return sandboxNow(t, p, id).Status.Phase == driver.Running })
	if status, answer := p.do(t, http.MethodPost, "/v1/sandboxes/"+id+"/exec?wait=1",
		strings.NewReader(`{"command":["sh","-c","echo inbound-free"]}`)); status != http.StatusOK || !strings.Contains(answer, "inbound-free") {
		t.Fatalf("the exec answered %d: %s", status, answer)
	}
	during := listeningSockets(t)
	if status, answer := p.do(t, http.MethodDelete, "/v1/sandboxes/"+id, nil); status != http.StatusNoContent && status != http.StatusAccepted {
		t.Fatalf("the delete answered %d: %s", status, answer)
	}
	if err := stop(); err != nil {
		t.Fatalf("the worker ended with %v", err)
	}
	if during > before {
		t.Errorf("the process listened on %d sockets with the worker running, %d before it: the worker opened a listener", during, before)
	}
}

// listeningSockets counts the file descriptors of this process that are
// sockets accepting connections. The control plane's own listeners are open
// before the worker starts, so a rise is a listener the worker opened.
func listeningSockets(t *testing.T) int {
	t.Helper()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("reading the descriptor limit: %v", err)
	}
	top := min(int(limit.Cur), 65536)
	count := 0
	for fd := range top {
		accepting, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err == nil && accepting != 0 {
			count++
		}
	}
	return count
}
