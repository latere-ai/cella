// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
)

// stubTransport is one environment answered by a test rather than by a
// worker. It is what proves the driver's own refusals: the branches a real
// worker never produces because a real worker answers correctly.
type stubTransport struct {
	registration remote.Registration
	registered   bool
	live         bool
	observed     map[string]driver.State
	reported     bool
	openErr      error

	mu   sync.Mutex
	last string
	res  remote.Response
	err  error
}

func (s *stubTransport) Registration() (remote.Registration, bool) {
	return s.registration, s.registered
}
func (s *stubTransport) Live() bool { return s.live }

func (s *stubTransport) Observed(id string) (driver.State, bool) {
	if !s.reported {
		return driver.State{}, false
	}
	state, held := s.observed[id]
	return state, held
}

func (s *stubTransport) ObservedList() ([]driver.State, bool) {
	if !s.reported {
		return nil, false
	}
	out := make([]driver.State, 0, len(s.observed))
	for _, state := range s.observed {
		out = append(out, state)
	}
	return out, true
}

func (s *stubTransport) Watch(context.Context) (<-chan remote.Event, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	events := make(chan remote.Event)
	close(events)
	return events, nil
}

func (s *stubTransport) Open(_ context.Context, opType string, _ remote.Request) (remote.Stream, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.mu.Lock()
	s.last = opType
	s.mu.Unlock()
	return &stubStream{res: s.res, err: s.err}, nil
}

func (s *stubTransport) issued() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// stubStream answers one operation with what a test chose, with both
// sub-stream directions ending at once.
type stubStream struct {
	res    remote.Response
	err    error
	closed bool
}

func (s *stubStream) Down(byte) io.WriteCloser { return nopWriter{} }
func (s *stubStream) Up(byte) io.Reader        { return strings.NewReader("") }
func (s *stubStream) Accepted(context.Context) error {
	return s.err
}
func (s *stubStream) Resize(int, int) error { return nil }
func (s *stubStream) Result(context.Context) (remote.Response, error) {
	return s.res, s.err
}
func (s *stubStream) Close() error { s.closed = true; return nil }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriter) Close() error                { return nil }

func stubDriver(t *testing.T, transport *stubTransport) *remote.Driver {
	t.Helper()
	d, err := remote.New(remote.Options{Environment: "env_stub", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDriverRefusesAnAnswerItCannotUse holds what the driver does with a
// worker that answered without answering: a create that named no sandbox and
// a read that carried no state are refusals, not zero values handed on.
func TestDriverRefusesAnAnswerItCannotUse(t *testing.T) {
	transport := &stubTransport{registered: true, live: true}
	d := stubDriver(t, transport)
	ctx := t.Context()

	if _, err := d.Create(ctx, driver.CreateSpec{ID: "sbx_1"}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a create the worker answered without a ref is %v, want ErrInvalid", err)
	}
	if _, err := d.Inspect(ctx, "sbx_1"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an inspect the worker answered without a state is %v, want ErrNotFound", err)
	}
	if _, err := d.Stat(ctx, "sbx_1", "/workspace/a"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a stat the worker answered without an entry is %v, want ErrNotFound", err)
	}
	if _, _, err := d.Open(ctx, "sbx_1", "/workspace/a"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an open the worker answered without an entry is %v, want ErrNotFound", err)
	}
}

// TestDriverRefusesBeforeItEnqueues holds the calls the contract refuses on
// this side: an operation that could never be executed is never written for a
// worker to claim.
func TestDriverRefusesBeforeItEnqueues(t *testing.T) {
	transport := &stubTransport{registered: true, live: true}
	d := stubDriver(t, transport)
	ctx := t.Context()

	// A prewarm that carries a caller's identity is a create with a flag set
	// by mistake, and the contract refuses it wherever it is made.
	err := func() error {
		_, createErr := d.Create(ctx, driver.CreateSpec{ID: "sbx_1", Prewarm: true, Owner: "ops"})
		return createErr
	}()
	if !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a prewarm carrying an owner is %v, want ErrInvalid", err)
	}
	// An adoption is exclusive of every other change; one that mixes them is
	// one act pretending to be two.
	labels := map[string]string{"a": "1"}
	err = d.Update(ctx, "sbx_1", driver.Change{Adopt: &driver.Adoption{Owner: "ops"}, Labels: &labels})
	if !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an adoption mixed with another field is %v, want ErrInvalid", err)
	}
	if transport.issued() != "" {
		t.Errorf("a refusal on this side enqueued the operation %q", transport.issued())
	}
}

// TestDriverReadsTheWorkerOrTheReport holds the two ways a sandbox is read: a
// control plane that has been told what the worker holds answers from the
// report, and one that has not asks the worker.
func TestDriverReadsTheWorkerOrTheReport(t *testing.T) {
	reported := driver.State{ID: "sbx_1", Owner: "ops", Phase: driver.Running}
	transport := &stubTransport{
		registered: true, live: true, reported: true,
		observed: map[string]driver.State{"sbx_1": reported},
	}
	d := stubDriver(t, transport)
	ctx := t.Context()

	state, err := d.Inspect(ctx, "sbx_1")
	if err != nil || state.Owner != "ops" {
		t.Errorf("the reported state read back as %+v (%v)", state, err)
	}
	if transport.issued() != "" {
		t.Errorf("a read the report answered enqueued the operation %q", transport.issued())
	}
	// A sandbox the report does not hold is asked of the worker.
	transport.res = remote.Response{State: &driver.State{ID: "sbx_2", Owner: "other"}}
	if state, err = d.Inspect(ctx, "sbx_2"); err != nil || state.Owner != "other" {
		t.Errorf("a sandbox the report does not hold read back as %+v (%v)", state, err)
	}
	if transport.issued() != remote.OpInspect {
		t.Errorf("the operation issued is %q, want %q", transport.issued(), remote.OpInspect)
	}

	// A control plane that has been told nothing lists through the worker,
	// and narrows what comes back by the caller's own filter.
	silent := &stubTransport{registered: true, live: true, res: remote.Response{States: []driver.State{
		{ID: "sbx_1", Owner: "ops"}, {ID: "sbx_2", Owner: "other"},
	}}}
	states, err := stubDriver(t, silent).List(ctx, driver.Filter{Owner: "ops"})
	if err != nil {
		t.Fatalf("the list failed: %v", err)
	}
	if len(states) != 1 || states[0].ID != "sbx_1" {
		t.Errorf("the worker's list narrowed to %+v, want the one sandbox the filter names", states)
	}
}

// TestDriverCarriesTheTransportsRefusal holds that a transport that cannot
// open an operation is the caller's answer, not a stream that never arrives.
func TestDriverCarriesTheTransportsRefusal(t *testing.T) {
	transport := &stubTransport{registered: true, live: true, openErr: remote.ErrNoWorker}
	d := stubDriver(t, transport)
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Exec", func() error { _, err := d.Exec(ctx, "sbx_1", driver.ExecRequest{}); return err }},
		{"Attach", func() error { _, err := d.Attach(ctx, "sbx_1", driver.AttachRequest{}); return err }},
		{"Logs", func() error { _, err := d.Logs(ctx, "sbx_1", driver.LogsRequest{}); return err }},
		{"ExportTar", func() error { return d.ExportTar(ctx, "sbx_1", nil, io.Discard) }},
		{"ImportTar", func() error { return d.ImportTar(ctx, "sbx_1", "/workspace", strings.NewReader("")) }},
		{"Open", func() error { _, _, err := d.Open(ctx, "sbx_1", "/workspace/a"); return err }},
		{"Write", func() error {
			_, err := d.Write(ctx, "sbx_1", driver.WriteRequest{Path: "/workspace/a"})
			return err
		}},
		{"List", func() error { _, err := d.List(ctx, driver.Filter{}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, remote.ErrNoWorker) {
				t.Errorf("%s on a transport that cannot open is %v, want ErrNoWorker", tc.name, err)
			}
		})
	}
}

// TestDriverRefusesAnEndedContext holds that the two calls answered from the
// registration still read the caller's context, so a caller that went away is
// told so rather than given a stale yes.
func TestDriverRefusesAnEndedContext(t *testing.T) {
	d := stubDriver(t, &stubTransport{registered: true, live: true})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Preflight(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("preflight under an ended context is %v", err)
	}
	if err := d.Ready(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ready under an ended context is %v", err)
	}
	if _, err := d.Watch(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a watch under an ended context is %v", err)
	}
}

// TestHandlesRefuseAnAnswerWithoutAnExit holds that a command the worker
// answered without an exit code is a defect on the far side and never a zero
// exit handed to a caller that would read it as success.
func TestHandlesRefuseAnAnswerWithoutAnExit(t *testing.T) {
	transport := &stubTransport{registered: true, live: true}
	d := stubDriver(t, transport)
	ctx := t.Context()

	exec, err := d.Exec(ctx, "sbx_1", driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	if _, err = exec.Wait(ctx); err == nil {
		t.Errorf("a command answered without an exit code waited successfully")
	}
	if err = exec.Close(); err != nil {
		t.Errorf("closing the command is %v", err)
	}
	if err = exec.Close(); err != nil {
		t.Errorf("closing a closed command is %v", err)
	}

	session, err := d.Attach(ctx, "sbx_1", driver.AttachRequest{})
	if err != nil {
		t.Fatalf("the attach failed: %v", err)
	}
	if _, err = session.Wait(ctx); err == nil {
		t.Errorf("a session ended without an exit code waited successfully")
	}
	if err = session.Resize(80, 24); err != nil {
		t.Errorf("resizing a live session is %v", err)
	}
	if err = session.Close(); err != nil {
		t.Errorf("closing the session is %v", err)
	}
	// A session the caller closed refuses a write and takes a resize as the
	// nothing it is, because the terminal is gone either way.
	if _, err = session.Write([]byte("typed after the close")); err == nil {
		t.Errorf("a closed session took a write")
	}
	if err = session.Resize(80, 24); err != nil {
		t.Errorf("resizing a closed session is %v, want nothing to do", err)
	}
	if err = session.Close(); err != nil {
		t.Errorf("closing a closed session is %v", err)
	}
}

// TestExecuteRefusesAnOperationThisWorkerDoesNotRun holds how a worker of an
// older release answers work it has no method for: the contract's own
// ErrUnsupported, so the operation is refused rather than dropped.
func TestExecuteRefusesAnOperationThisWorkerDoesNotRun(t *testing.T) {
	link := remote.NewLink(&nopConn{}, remote.LinkOptions{})
	channel := link.Open(remote.NewOperationID(), false)
	_, err := remote.Execute(t.Context(), runtimeNop{}, "Teleport", remote.Request{}, channel)
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an operation this worker does not run is %v, want ErrUnsupported", err)
	}
	// The calls that carry a body refuse one that is absent, because an
	// update with no change and a create with no spec are frames that lost
	// their payload.
	if _, err = remote.Execute(t.Context(), runtimeNop{}, remote.OpUpdate, remote.Request{}, channel); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an update carrying no change is %v, want ErrInvalid", err)
	}
	if _, err = remote.Execute(t.Context(), runtimeNop{}, remote.OpCreate, remote.Request{}, channel); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a create carrying no spec is %v, want ErrInvalid", err)
	}
	if _, err = remote.Execute(t.Context(), runtimeNop{}, remote.OpExec, remote.Request{}, channel); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an exec carrying no request is %v, want ErrInvalid", err)
	}
}

// nopConn is a connection that reads nothing and writes into nothing, for the
// cases that drive a channel without a peer.
type nopConn struct{ closed chan struct{} }

func (c *nopConn) ReadFrame() ([]byte, error) {
	if c.closed == nil {
		c.closed = make(chan struct{})
	}
	<-c.closed
	return nil, io.EOF
}
func (c *nopConn) WriteFrame([]byte) error { return nil }
func (c *nopConn) Close() error {
	if c.closed != nil {
		close(c.closed)
		c.closed = nil
	}
	return nil
}

// runtimeNop is a driver that runs nothing, for the cases that are about the
// executor's own table rather than about a driver.
type runtimeNop struct{ driver.Driver }
