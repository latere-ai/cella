// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// scripted is a policy whose Admit answers what a case sets after a wait, or
// waits for its context.
type scripted struct {
	verdict Verdict
	wait    time.Duration
	block   bool
}

func (s scripted) Admit(ctx context.Context, _ Create) Verdict {
	if s.block {
		<-ctx.Done()
		return Verdict{}
	}
	time.Sleep(s.wait)
	return s.verdict
}

func (s scripted) Check(context.Context, Operation) Verdict { return s.verdict }

// gateOver is the gate a worker over d with policy p and the admit timeout
// builds.
func gateOver(d runtime.Driver, p HostPolicy, timeout time.Duration) *gate {
	return newGate(Options{Driver: d, Policy: p, AdmitTimeout: timeout}, slog.Default())
}

// TestAskHoldsThenAnswers is the hold of spec 082 at the gate: a policy that
// has not answered within the held delay sends the held frame once, and its
// answer, allow or refuse, is what the create gets; a refusal carries the
// host's reason.
func TestAskHoldsThenAnswers(t *testing.T) {
	defer ShortenHeldAfter(20 * time.Millisecond)()
	var holds atomic.Int32
	hold := func() error { holds.Add(1); return nil }
	allow := gateOver(runtimetest.Nop{}, scripted{verdict: Verdict{Allow: true}, wait: 100 * time.Millisecond}, time.Minute)
	if err := allow.ask(t.Context(), Create{}, hold); err != nil {
		t.Fatalf("an allowed create answered %v", err)
	}
	if holds.Load() != 1 {
		t.Errorf("a create the policy took a while over was held %d times, want once", holds.Load())
	}
	refuse := gateOver(runtimetest.Nop{}, scripted{verdict: Verdict{Reason: "not today"}}, time.Minute)
	err := refuse.ask(t.Context(), Create{}, func() error { return errors.New("the stream is gone") })
	if !errors.Is(err, runtime.ErrHostRefused) || !strings.Contains(err.Error(), "not today") {
		t.Errorf("a refused create answered %v", err)
	}
}

// TestAskTimesOutAndFollowsItsCaller: a policy that never answers is refused
// as host_timeout after the admit timeout, and a caller that went away ends
// the question with its own error.
func TestAskTimesOutAndFollowsItsCaller(t *testing.T) {
	never := gateOver(runtimetest.Nop{}, scripted{block: true}, 50*time.Millisecond)
	if err := never.ask(t.Context(), Create{}, func() error { return nil }); !errors.Is(err, runtime.ErrHostTimeout) {
		t.Errorf("an unanswered create answered %v, want host_timeout", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	patient := gateOver(runtimetest.Nop{}, scripted{block: true}, time.Minute)
	if err := patient.ask(ctx, Create{}, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a create whose caller left answered %v", err)
	}
}

// TestRefusedKeepsTheReasonShort: a reason past MaxReasonBytes is cut at a
// character boundary, and no reason at all is the bare refusal.
func TestRefusedKeepsTheReasonShort(t *testing.T) {
	long := strings.Repeat("a", MaxReasonBytes-1) + "é and more"
	err := refused(long)
	text := strings.TrimSuffix(err.Error(), " ("+runtime.ErrHostRefused.Error()+")")
	if len(text) > MaxReasonBytes || !strings.HasPrefix(long, text) || strings.ContainsRune(text, '�') {
		t.Errorf("the reason was cut to %q (%d bytes)", text, len(text))
	}
	if err := refused(""); err != runtime.ErrHostRefused { //nolint:errorlint // the bare sentinel is the answer
		t.Errorf("no reason gives %v", err)
	}
}

// TestChecksOfAHostThatGrantsNothing: a driver that reports no host binds no
// folder and refuses a workspace.host, and its writes are the driver's own to
// confine; operations that are not about one sandbox pass through.
func TestChecksOfAHostThatGrantsNothing(t *testing.T) {
	g := gateOver(runtimetest.Nop{}, nil, time.Minute)
	err := g.checkCreate(runtime.CreateSpec{Workspace: runtime.Workspace{Host: "/home/dev/project"}})
	if !errors.Is(err, runtime.ErrWorkspaceNotGranted) {
		t.Errorf("a host folder on a driver that binds none answered %v", err)
	}
	if err := g.checkWrite(Operation{Type: remote.OpWrite, Path: "/anywhere"}); err != nil {
		t.Errorf("a write on a driver that reports no host answered %v", err)
	}
	for _, opType := range []string{remote.OpList, remote.OpWatch} {
		if err := g.admit(t.Context(), opType, remote.Request{}, nil); err != nil {
			t.Errorf("%s answered %v, want it passed through", opType, err)
		}
	}
	if err := g.admit(t.Context(), remote.OpCreate, remote.Request{}, nil); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("a create with no spec answered %v", err)
	}
	if err := g.admit(t.Context(), remote.OpCreate, remote.Request{Spec: &runtime.CreateSpec{ID: "sbx_a"}}, nil); err != nil {
		t.Errorf("a plain create with no policy answered %v", err)
	}
}

// failingInspect is a driver whose reads fail for a reason other than a
// missing sandbox, which reaches the caller as that failure.
type failingInspect struct{ runtimetest.Nop }

func (failingInspect) Inspect(context.Context, string) (runtime.State, error) {
	return runtime.State{}, errors.New("the engine is not answering")
}

func TestAnInspectThatFailsIsTheAnswer(t *testing.T) {
	g := gateOver(failingInspect{}, nil, time.Minute)
	err := g.admit(t.Context(), remote.OpExec, remote.Request{ID: "sbx_a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Errorf("an exec whose read failed answered %v", err)
	}
}

// TestOperationOfNamesWhatThePolicyChecks: each operation tells the policy
// the path it acts on, a move its destination, an archive its first path.
func TestOperationOfNamesWhatThePolicyChecks(t *testing.T) {
	for _, c := range []struct {
		opType string
		req    remote.Request
		want   Operation
	}{
		{remote.OpMove, remote.Request{ID: "s", Path: "/a", To: "/b"}, Operation{Type: remote.OpMove, Sandbox: "s", Path: "/a", To: "/b"}},
		{remote.OpImportTar, remote.Request{ID: "s", Dest: "/d"}, Operation{Type: remote.OpImportTar, Sandbox: "s", Path: "/d"}},
		{remote.OpExportTar, remote.Request{ID: "s", Paths: []string{"/e", "/f"}}, Operation{Type: remote.OpExportTar, Sandbox: "s", Path: "/e"}},
		{remote.OpExec, remote.Request{ID: "s"}, Operation{Type: remote.OpExec, Sandbox: "s"}},
	} {
		got := operationOf(c.opType, c.req)
		if got.Type != c.want.Type || got.Sandbox != c.want.Sandbox || got.Path != c.want.Path || got.To != c.want.To {
			t.Errorf("%s gives %+v, want %+v", c.opType, got, c.want)
		}
	}
	// A sandbox with no id is remembered nowhere, so a record of an
	// operation with none carries no labels.
	g := gateOver(runtimetest.Nop{}, nil, time.Minute)
	g.remember("", map[string]string{"a": "b"})
	if len(g.labelsOf("")) != 0 {
		t.Error("labels were remembered for no sandbox")
	}
}

// TestRefusalReadsTheEnvelope: a registration refused with a code the worker
// stops on is a Refusal carrying the detail, and any other answer is retried.
func TestRefusalReadsTheEnvelope(t *testing.T) {
	r, fatal := refusalOf([]byte(`{"error":{"code":"worker_limit","details":{"detail":"two live workers"}}}`))
	if !fatal || r.Code != remote.ReasonWorkerLimit || !strings.Contains(r.Error(), "two live workers") {
		t.Errorf("a worker_limit refusal reads as %v %v", r, fatal)
	}
	for _, body := range []string{`{"error":{"code":"unauthenticated"}}`, `not json`} {
		if r, fatal := refusalOf([]byte(body)); fatal {
			t.Errorf("%s reads as the refusal %v", body, r)
		}
	}
	if !strings.HasSuffix((&Refusal{Code: "serve_refused"}).Error(), "serve_refused") {
		t.Error("a refusal with no detail does not name its code")
	}
}

// listFails is a driver that cannot list what it holds, which release logs
// and survives.
type listFails struct{ runtimetest.Nop }

func (listFails) List(context.Context, runtime.Filter) ([]runtime.State, error) {
	return nil, errors.New("the engine is gone")
}

// stopFails is a driver holding one sandbox it can neither stop nor delete.
type stopFails struct{ runtimetest.Nop }

func (stopFails) List(context.Context, runtime.Filter) ([]runtime.State, error) {
	return []runtime.State{{ID: "sbx_stuck"}}, nil
}
func (stopFails) Stop(context.Context, string) error   { return errors.New("it will not stop") }
func (stopFails) Delete(context.Context, string) error { return errors.New("it will not go") }

func TestReleaseLogsWhatItCannotEnd(t *testing.T) {
	for _, d := range []runtime.Driver{listFails{}, stopFails{}} {
		w, err := newWorker(Options{URL: "http://127.0.0.1:1", Key: func(context.Context) (string, error) { return "k", nil }, Driver: d})
		if err != nil {
			t.Fatal(err)
		}
		w.release(t.Context(), true, true)
		w.release(t.Context(), false, false)
	}
}
