// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// hostDriver is a driver on a machine somebody owns: the native driver with
// a host it reports, the folders it grants, and the host workspace declared.
type hostDriver struct {
	driver.Driver
	host v1.HostInfo
}

func (h hostDriver) Host() v1.HostInfo { return h.host }

func (h hostDriver) Capabilities() driver.Capabilities {
	c := h.Driver.Capabilities()
	c.HostWorkspace = true
	return c
}

// testHost grants one folder writable and one read-only, and allows two
// hosts.
var testHost = v1.HostInfo{
	OS: "linux", Arch: "arm64",
	Grants: []v1.HostGrant{
		{Path: "/home/dev/project", Write: true},
		{Path: "/home/dev/project/vendor"},
	},
	Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com", "proxy.golang.org"}},
}

// policy is a HostPolicy a case scripts: what it answers a create and an
// operation, and how long it waits before answering a create.
type policy struct {
	admit Verdict
	check Verdict
	wait  time.Duration
	// block makes Admit wait for its context, which is a person who never
	// answers.
	block bool

	mu      sync.Mutex
	creates []worker.Create
	ops     []worker.Operation
}

// Verdict is worker.Verdict under a short name for the tables below.
type Verdict = worker.Verdict

func (p *policy) Admit(ctx context.Context, c worker.Create) worker.Verdict {
	p.mu.Lock()
	p.creates = append(p.creates, c)
	p.mu.Unlock()
	if p.block {
		<-ctx.Done()
		return worker.Verdict{Reason: "the question was withdrawn"}
	}
	select {
	case <-time.After(p.wait):
	case <-ctx.Done():
		return worker.Verdict{Reason: "the question was withdrawn"}
	}
	return p.admit
}

func (p *policy) Check(_ context.Context, op worker.Operation) worker.Verdict {
	p.mu.Lock()
	p.ops = append(p.ops, op)
	p.mu.Unlock()
	return p.check
}

func (p *policy) asked() ([]worker.Create, []worker.Operation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]worker.Create(nil), p.creates...), append([]worker.Operation(nil), p.ops...)
}

// records keeps every Record a worker hands over.
type records struct {
	mu  sync.Mutex
	all []worker.Record
}

func (r *records) add(rec worker.Record) {
	r.mu.Lock()
	r.all = append(r.all, rec)
	r.mu.Unlock()
}

func (r *records) list() []worker.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]worker.Record(nil), r.all...)
}

// of is every record of one operation type.
func (r *records) of(opType string) []worker.Record {
	var out []worker.Record
	for _, rec := range r.list() {
		if rec.Type == opType {
			out = append(out, rec)
		}
	}
	return out
}

// TestBuiltInChecksIgnoreThePolicy holds the host's last word: with a policy
// that allows everything, and a control plane that skips resolve and sends a
// create as it likes, a workspace.host outside the host's grants is refused
// with workspace_not_granted before the policy is asked, a folder the host
// grants is admitted, an isolation the host does not provide is refused, and
// a write into a folder the host grants read-only is refused. A create that
// asks for open egress is admitted: the network is the driver's to narrow,
// which the local driver's own table holds.
func TestBuiltInChecksIgnoreThePolicy(t *testing.T) {
	p := newPlane(t)
	allow := &policy{admit: Verdict{Allow: true}, check: Verdict{Allow: true}}
	host := hostDriver{Driver: nativeDriver(t), host: testHost}
	p.startWith(t, worker.Options{Driver: host, Policy: allow})
	p.waitWorkers(t, 1, "after the worker started")

	_, err := p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_outside", Name: "outside", Owner: "ops", Command: []string{"sleep", "30"},
		Workspace: driver.Workspace{Path: "/home/dev/secrets", Host: "/home/dev/secrets"},
	})
	if !errors.Is(err, driver.ErrWorkspaceNotGranted) {
		t.Fatalf("a create outside the grants answered %v, want workspace_not_granted", err)
	}
	for _, folder := range []string{"/home/dev/project/../secrets", "home/dev/project"} {
		_, err = p.driver.Create(t.Context(), driver.CreateSpec{
			ID: "sbx_unclean", Name: "unclean", Owner: "ops", Workspace: driver.Workspace{Host: folder},
		})
		if !errors.Is(err, driver.ErrWorkspaceNotGranted) {
			t.Errorf("a create naming %q answered %v, want workspace_not_granted", folder, err)
		}
	}
	_, err = p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_vm", Name: "vm", Owner: "ops", Isolation: v1.IsolationVM, Command: []string{"sleep", "30"},
	})
	if !errors.Is(err, driver.ErrHostRefused) {
		t.Errorf("a create asking for another isolation answered %v, want host_refused", err)
	}
	if creates, _ := allow.asked(); len(creates) != 0 {
		t.Errorf("the policy was asked about creates the host does not grant: %+v", creates)
	}

	ref, err := p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_inside", Name: "inside", Owner: "ops", Command: []string{"sleep", "30"},
		Labels:      map[string]string{"session": "ses_1"},
		Annotations: map[string]string{"example.com/agent-name": "Site helper"},
		Workspace:   driver.Workspace{Path: "/home/dev/project/site", Host: "/home/dev/project/site"},
		Egress:      driver.Egress{Mode: string(v1.EgressOpen)},
	})
	if err != nil {
		t.Fatalf("a create inside the grants failed: %v", err)
	}
	creates, _ := allow.asked()
	if len(creates) != 1 || creates[0].Sandbox.Spec.Workspace.Host != "/home/dev/project/site" ||
		creates[0].Labels["session"] != "ses_1" || creates[0].Sandbox.Status.ID != "sbx_inside" ||
		creates[0].Sandbox.Spec.Network.Egress.Mode != v1.EgressOpen ||
		creates[0].Sandbox.Metadata.Annotations["example.com/agent-name"] != "Site helper" {
		t.Errorf("the policy was asked %+v", creates)
	}
	for _, path := range []string{"/home/dev/project/vendor/lib.go", "/home/dev/project/vendor"} {
		_, err = p.driver.Write(t.Context(), ref.ID, driver.WriteRequest{Path: path, Body: strings.NewReader("x")})
		if !errors.Is(err, driver.ErrWorkspaceNotGranted) {
			t.Errorf("a write to %s in a read-only grant answered %v", path, err)
		}
	}
	if err = p.driver.Move(t.Context(), ref.ID, "/home/dev/project/a", "/home/dev/project/vendor/a"); !errors.Is(err, driver.ErrWorkspaceNotGranted) {
		t.Errorf("a move into a read-only grant answered %v", err)
	}
}

// TestWorkerRefusesUnknownSandbox: an operation on a sandbox the worker's
// driver does not hold is refused before the driver or the policy sees it,
// and recorded as refused. The refusal reads as not found, so a control plane
// deleting a sandbox its worker lost reads it as gone.
func TestWorkerRefusesUnknownSandbox(t *testing.T) {
	p := newPlane(t)
	check := &policy{admit: Verdict{Allow: true}, check: Verdict{Allow: true}}
	rec := &records{}
	p.startWith(t, worker.Options{Driver: nativeDriver(t), Policy: check, Record: rec.add})
	p.waitWorkers(t, 1, "after the worker started")

	_, err := p.driver.Exec(t.Context(), "sbx_nobody", driver.ExecRequest{Command: []string{"true"}})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("an exec on a sandbox the driver does not hold answered %v, want not found", err)
	}
	if err = p.driver.Delete(t.Context(), "sbx_nobody"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a delete of a sandbox the driver does not hold answered %v", err)
	}
	if _, ops := check.asked(); len(ops) != 0 {
		t.Errorf("the policy was asked about a sandbox the host does not hold: %+v", ops)
	}
	waitUntil(t, func() bool { return len(rec.of(remote.OpExec)) == 1 }, "the exec to be recorded")
	if got := rec.of(remote.OpExec)[0]; got.Outcome != worker.OutcomeRefused || got.Sandbox != "sbx_nobody" {
		t.Errorf("the refused exec was recorded as %+v", got)
	}
}

// TestPolicyRefusesAnOperation: a Check that says no answers the operation
// host_refused with the host's reason, and the driver never runs it.
func TestPolicyRefusesAnOperation(t *testing.T) {
	p := newPlane(t)
	no := &policy{admit: Verdict{Allow: true}, check: Verdict{Reason: "not in this folder"}}
	rec := &records{}
	p.startWith(t, worker.Options{Driver: nativeDriver(t), Policy: no, Record: rec.add})
	p.waitWorkers(t, 1, "after the worker started")
	ref, err := p.driver.Create(t.Context(), driver.CreateSpec{ID: "sbx_checked", Name: "checked", Owner: "ops", Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.driver.Exec(t.Context(), ref.ID, driver.ExecRequest{Command: []string{"touch", "/tmp/never"}})
	if !errors.Is(err, driver.ErrHostRefused) || !strings.Contains(err.Error(), "not in this folder") {
		t.Fatalf("a refused exec answered %v", err)
	}
	_, ops := no.asked()
	if len(ops) == 0 || ops[len(ops)-1].Type != remote.OpExec || ops[len(ops)-1].Command[0] != "touch" {
		t.Errorf("the policy was asked %+v", ops)
	}
	waitUntil(t, func() bool { return len(rec.of(remote.OpExec)) == 1 }, "the refusal to be recorded")
	if got := rec.of(remote.OpExec)[0]; got.Outcome != worker.OutcomeRefused || !strings.Contains(got.Reason, "not in this folder") {
		t.Errorf("the refusal was recorded as %+v", got)
	}
}

// TestRecordCarriesNoContent: the records of an exec, a write and a read name
// the operation, the sandbox, its labels, the command and the path, and carry
// no byte of what the exec read on stdin, what it printed, or what the files
// held.
func TestRecordCarriesNoContent(t *testing.T) {
	p := newPlane(t)
	rec := &records{}
	p.startWith(t, worker.Options{Driver: nativeDriver(t), Record: rec.add})
	p.waitWorkers(t, 1, "after the worker started")
	ref, err := p.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_quiet", Name: "quiet", Owner: "ops", Command: []string{"sleep", "30"},
		Labels: map[string]string{"session": "ses_quiet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec, err := p.driver.Exec(t.Context(), ref.ID, driver.ExecRequest{
		Command: []string{"tr", "a-z", "A-Z"}, Stdin: strings.NewReader("typed-on-stdin"),
	})
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	out, _ := io.ReadAll(exec.Stdout())
	_, _ = io.ReadAll(exec.Stderr())
	if _, err = exec.Wait(t.Context()); err != nil {
		t.Fatalf("the exec ended with %v", err)
	}
	if !strings.Contains(string(out), "TYPED-ON-STDIN") {
		t.Fatalf("the exec printed %q", out)
	}
	if _, err = p.driver.Write(t.Context(), ref.ID, driver.WriteRequest{
		Path: "/workspace/notes.txt", Body: strings.NewReader("written-into-the-file"),
	}); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	reader, _, err := p.driver.Open(t.Context(), ref.ID, "/workspace/notes.txt")
	if err != nil {
		t.Fatalf("the read failed: %v", err)
	}
	body, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(body) != "written-into-the-file" {
		t.Fatalf("the read returned %q", body)
	}
	waitUntil(t, func() bool {
		return len(rec.of(remote.OpExec)) == 1 && len(rec.of(remote.OpWrite)) == 1 && len(rec.of(remote.OpOpen)) == 1
	}, "the three operations to be recorded")
	whole, err := json.Marshal(rec.list())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"typed-on-stdin", "TYPED-ON-STDIN", "written-into-the-file"} {
		if strings.Contains(string(whole), content) {
			t.Errorf("a record carries %q: %s", content, whole)
		}
	}
	got := rec.of(remote.OpExec)[0]
	if got.Sandbox != ref.ID || got.Labels["session"] != "ses_quiet" || got.Command[0] != "tr" ||
		got.Outcome != worker.OutcomeOK || got.ExitCode == nil || *got.ExitCode != 0 || got.Time.IsZero() {
		t.Errorf("the exec was recorded as %+v", got)
	}
	if write := rec.of(remote.OpWrite)[0]; write.Path != "/workspace/notes.txt" || write.Outcome != worker.OutcomeOK {
		t.Errorf("the write was recorded as %+v", write)
	}
}

// TestRunStopsItsSandboxes holds what a worker leaves behind when it stops:
// when its context ends it stops every sandbox its driver lists; with
// KeepOnExit it leaves them running; when the control plane closes the
// stream with key_revoked or serve_refused it stops and deletes every one
// and returns the refusal; and when it closes it with worker_replaced it
// touches nothing, because another process holding the key serves them now.
func TestRunStopsItsSandboxes(t *testing.T) {
	create := func(t *testing.T, p *plane) string {
		t.Helper()
		ref, err := p.driver.Create(t.Context(), driver.CreateSpec{
			ID: "sbx_" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_"), Name: "left", Owner: "ops",
			Command: []string{"sleep", "300"},
		})
		if err != nil {
			t.Fatalf("the create failed: %v", err)
		}
		return ref.ID
	}
	phase := func(t *testing.T, host driver.Driver, id string) string {
		t.Helper()
		state, err := host.Inspect(context.Background(), id)
		if errors.Is(err, driver.ErrNotFound) {
			return "gone"
		}
		if err != nil {
			t.Fatalf("the inspect failed: %v", err)
		}
		return state.Phase
	}

	t.Run("a context that ends stops them", func(t *testing.T) {
		p, host := newPlane(t), nativeDriver(t)
		stop, _ := p.startWith(t, worker.Options{Driver: host})
		p.waitWorkers(t, 1, "after the worker started")
		id := create(t, p)
		if err := stop(); err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if got := phase(t, host, id); got != driver.Stopped {
			t.Errorf("after the worker stopped the sandbox is %s, want Stopped", got)
		}
	})
	t.Run("KeepOnExit leaves them running", func(t *testing.T) {
		p, host := newPlane(t), nativeDriver(t)
		stop, _ := p.startWith(t, worker.Options{Driver: host, KeepOnExit: true})
		p.waitWorkers(t, 1, "after the worker started")
		id := create(t, p)
		if err := stop(); err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if got := phase(t, host, id); got != driver.Running {
			t.Errorf("with KeepOnExit the sandbox is %s, want Running", got)
		}
	})
	for _, c := range []struct {
		reason string
		want   string
	}{
		{remote.ReasonKeyRevoked, "gone"},
		{remote.ReasonServeRefused, "gone"},
		{remote.ReasonWorkerReplaced, driver.Running},
	} {
		t.Run("a stream closed with "+c.reason, func(t *testing.T) {
			p, host := newPlane(t), nativeDriver(t)
			_, done := p.startWith(t, worker.Options{Driver: host, KeepOnExit: true})
			p.waitWorkers(t, 1, "after the worker started")
			id := create(t, p)
			// The next stream is refused with the reason, and the one held
			// now ends, so the worker reconnects into the refusal.
			reason := c.reason
			p.closeWith.Store(&reason)
			p.hub.Release("env_test")
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("Run did not return on the refusal")
			}
			if got := phase(t, host, id); got != c.want {
				t.Errorf("after %s the sandbox is %s, want %s", c.reason, got, c.want)
			}
		})
	}
}

// TestRunReturnsTheRefusal: a stream the control plane closed with a refusal
// ends Run with that Refusal, which an embedder reads to tell a removed
// environment from a replaced worker.
func TestRunReturnsTheRefusal(t *testing.T) {
	p := newPlane(t)
	reason := remote.ReasonWorkerReplaced
	p.closeWith.Store(&reason)
	err := worker.Run(t.Context(), worker.Options{URL: p.url, Key: key("k"), Driver: nativeDriver(t)})
	var refusal *worker.Refusal
	if !errors.As(err, &refusal) || refusal.Code != remote.ReasonWorkerReplaced || !strings.Contains(refusal.Error(), "worker_replaced") {
		t.Fatalf("Run returned %v, want the worker_replaced refusal", err)
	}
}

// TestKeyIsReadPerConnection: the key source is asked once per connection,
// so a key rotated in the store it lives in is used at the next one, and a
// source that fails or answers nothing is retried rather than registered
// with.
func TestKeyIsReadPerConnection(t *testing.T) {
	p := newPlane(t)
	p.drop.Store(true)
	var mu sync.Mutex
	asked := 0
	source := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		switch asked {
		case 1:
			return "", errors.New("the key store is locked")
		case 2:
			return " ", nil
		}
		return "key-" + string(rune('0'+asked)), nil
	}
	w, err := worker.New(worker.Options{URL: p.url, Key: source, Driver: nativeDriver(t)})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &waitRecorder{}
	w.SetAfter(recorder.after)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	waitUntil(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.keys) >= 2
	}, "two registrations")
	cancel()
	<-done
	p.mu.Lock()
	keys := append([]string(nil), p.keys...)
	p.mu.Unlock()
	if keys[0] != "Bearer key-3" || keys[1] != "Bearer key-4" {
		t.Errorf("the registrations carried %v, want the key read anew for each", keys)
	}
}
