// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// durableStore is design 010's store as the controller reads it: one row per
// object with the mutation that wrote it, and the observed index a rebuild
// replaces. The adapters of internal/store are the real ones; this is the
// contract, so a controller test states what it needs from a store and not how
// Postgres holds it.
type durableStore struct {
	mu       sync.Mutex
	objects  map[string]v1.Sandbox
	journal  []journalRow
	observed map[string][]driver.State
	rebuilds int

	durable                         bool
	writeErr, removeErr, rebuildErr error
	// live refuses a write under an ended context, as a database connection
	// does, where the zero store takes any context.
	live bool
}

type journalRow struct{ object, mutation string }

func newDurable(durable bool) *durableStore {
	return &durableStore{objects: map[string]v1.Sandbox{}, observed: map[string][]driver.State{}, durable: durable}
}

func (s *durableStore) Load() (map[string]v1.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.objects), nil
}

func (s *durableStore) Save(objects map[string]v1.Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = maps.Clone(objects)
	return nil
}

func (s *durableStore) Close() error  { return nil }
func (s *durableStore) Durable() bool { return s.durable }
func (s *durableStore) Write(ctx context.Context, obj v1.Sandbox, mutation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.live && ctx.Err() != nil {
		return ctx.Err()
	}
	s.objects[obj.Status.ID] = obj
	s.journal = append(s.journal, journalRow{obj.Status.ID, mutation})
	return nil
}

func (s *durableStore) Remove(ctx context.Context, id, mutation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removeErr != nil {
		return s.removeErr
	}
	if s.live && ctx.Err() != nil {
		return ctx.Err()
	}
	delete(s.objects, id)
	s.journal = append(s.journal, journalRow{id, mutation})
	return nil
}

func (s *durableStore) Rebuild(_ context.Context, environment string, states []driver.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rebuildErr != nil {
		return s.rebuildErr
	}
	s.rebuilds++
	s.observed[environment] = slices.Clone(states)
	return nil
}

// mutations is one object's journal in the order it was written.
func (s *durableStore) mutations(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, row := range s.journal {
		if row.object == id {
			out = append(out, row.mutation)
		}
	}
	return out
}

// index is what the last rebuild left for one environment.
func (s *durableStore) index(environment string) ([]driver.State, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.observed[environment]), s.rebuilds
}

// recovering opens a controller over the durable store and a fake driver.
func recovering(t *testing.T, durable bool, o Options) (*Controller, *fakeDriver, *fakeClock, *durableStore) {
	t.Helper()
	st := newDurable(durable)
	o.Store = st
	c, d, clock := newFake(t, o)
	return c, d, clock, st
}

// vanish removes the driver's object without telling the controller, which is
// a node drain, an operator with a shell, or a cluster that evicted a pod.
func vanish(d *fakeDriver, id string) {
	d.set(func(d *fakeDriver) {
		delete(d.states, id)
		d.order = slices.DeleteFunc(d.order, func(other string) bool { return other == id })
	})
}

// TestLostForVanishedSandbox is the hosted reaper's
// TestReaperEmitsLostForVanishedSandbox restated: there, a sandbox missing from
// this sweep and present in the last one was lost; here the desired record is
// the baseline, which is what a store buys and why the rule waited for one.
func TestLostForVanishedSandbox(t *testing.T) {
	c, d, _, st := recovering(t, false, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(d, id)

	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatalf("the tick failed: %v", err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatalf("the desired record went with the driver's object: %v", err)
	}
	if got.Status.Phase != PhaseLost || got.Status.Reason != ReasonLost {
		t.Fatalf("the vanished sandbox is %s/%s, want Lost/Lost", got.Status.Phase, got.Status.Reason)
	}
	if last := st.mutations(id); len(last) == 0 || last[len(last)-1] != MutationLost {
		t.Fatalf("the journal of the vanished sandbox is %v", last)
	}
}

// TestLostSuppressedForEndedSandbox is the hosted reaper's
// TestReaperSuppressesLostWhenProbeMatches restated: there, a sandbox with a
// recent terminal audit was not reported lost; here a phase the control plane
// itself wrote is what says the sandbox was ended rather than lost.
func TestLostSuppressedForEndedSandbox(t *testing.T) {
	for _, phase := range []string{driver.Pending, PhaseFailed, PhaseDeleting} {
		t.Run(phase, func(t *testing.T) {
			c, d, _, st := recovering(t, false, Options{})
			obj := created(t, c, "work")
			id := obj.Status.ID
			vanish(d, id)
			c.mu.Lock()
			ended := c.objects[id]
			ended.Status.Phase = phase
			c.objects[id] = ended
			c.mu.Unlock()

			if _, err := c.Reap(t.Context()); err != nil {
				t.Fatalf("the tick failed: %v", err)
			}
			got, err := c.Get(t.Context(), id, "alice")
			if err != nil {
				t.Fatalf("the record went: %v", err)
			}
			if got.Status.Phase != phase {
				t.Fatalf("a %s sandbox was moved to %s", phase, got.Status.Phase)
			}
			if slices.Contains(st.mutations(id), MutationLost) {
				t.Errorf("a %s sandbox was reported lost", phase)
			}
		})
	}
}

// TestLostRevalidatesBeforeActing: the list is read without the controller's
// lock, so a create can land between the list and the rule. Only a driver that
// reports the object gone confirms a sandbox is lost; anything else leaves it,
// because recreating a sandbox that exists would run it twice.
func TestLostRevalidatesBeforeActing(t *testing.T) {
	c, d, _, st := recovering(t, true, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	// The object is not in the list this tick and is there when the rule
	// re-reads it, which is what a create finishing in between looks like.
	d.set(func(d *fakeDriver) { d.order = nil })

	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatalf("the tick failed: %v", err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == PhaseLost || got.Status.Phase == PhaseRecovering {
		t.Fatalf("a sandbox the driver still has was declared %s", got.Status.Phase)
	}
	if m := st.mutations(id); slices.Contains(m, MutationLost) || slices.Contains(m, MutationRecovering) {
		t.Fatalf("the journal reports a sandbox that never went: %v", m)
	}
}

// TestLostRevalidationReportsADriverFailure: an Inspect that fails for any
// other reason is not evidence of anything, so the tick reports it and acts on
// nothing.
func TestLostRevalidationReportsADriverFailure(t *testing.T) {
	c, d, _, st := recovering(t, false, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(d, id)
	boom := errors.New("the driver is unreachable")
	d.set(func(d *fakeDriver) { d.inspectErr = boom })

	if _, err := c.Reap(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("the tick returned %v, want the driver's failure", err)
	}
	if slices.Contains(st.mutations(id), MutationLost) {
		t.Error("a sandbox was reported lost on a driver that did not answer")
	}
}

// TestLostGraceReaps is the rule without a durable store: the sandbox is Lost
// at once and deleted once the grace has passed, and not one tick before.
func TestLostGraceReaps(t *testing.T) {
	const grace = 10 * time.Minute
	c, d, clock, st := recovering(t, false, Options{LostGrace: grace})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(d, id)

	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(grace - time.Second)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("the sandbox was ended one second before its grace: acted=%d err=%v", acted, err)
	}
	if _, err := c.Get(t.Context(), id, "alice"); err != nil {
		t.Fatalf("the record went inside the grace: %v", err)
	}
	clock.Advance(time.Second)
	acted, err := c.Reap(t.Context())
	if err != nil || acted != 1 {
		t.Fatalf("the grace passed and the sandbox was not ended: acted=%d err=%v", acted, err)
	}
	if _, err := c.Get(t.Context(), id, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the lost record outlived its grace: %v", err)
	}
	want := []string{MutationCreated, MutationStatus, MutationStarted, MutationLost, MutationDeleting, MutationDeleted}
	if got := st.mutations(id); !slices.Equal(got, want) {
		t.Fatalf("the journal reads %v, want %v", got, want)
	}
}

// TestRecoveryRecreates is the rule with a durable store: the sandbox is Lost,
// then Recovering, then recreated with the same id, name and labels.
func TestRecoveryRecreates(t *testing.T) {
	c, d, _, st := recovering(t, true, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(d, id)

	acted, err := c.Reap(t.Context())
	if err != nil || acted != 1 {
		t.Fatalf("the lost sandbox was not recovered: acted=%d err=%v", acted, err)
	}
	state, err := d.Inspect(t.Context(), id)
	if err != nil {
		t.Fatalf("the driver does not have the recovered sandbox: %v", err)
	}
	if state.ID != id || state.Name != obj.Metadata.Name || state.Owner != "alice" ||
		!maps.Equal(state.Labels, obj.Metadata.Labels) {
		t.Fatalf("the recovered object is %+v, want the id, name, owner and labels of %+v", state, obj)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != driver.Running {
		t.Fatalf("the recovered sandbox is %s, want Running", got.Status.Phase)
	}
	want := []string{MutationLost, MutationRecovering, MutationRecovered}
	if got := st.mutations(id); !slices.Equal(got[len(got)-3:], want) {
		t.Fatalf("the journal reads %v, want it to end %v", got, want)
	}
}

// adopting is a driver whose Create reports that the object is already there,
// which is a second writer that got to it first. Design 005's create order
// adopts such an object rather than making a second one. It answers that way
// once armed, so the sandbox the case starts from is created normally.
type adopting struct {
	*fakeDriver
	err   error
	armed *bool
}

func (d adopting) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if d.err != nil {
		return driver.Ref{}, d.err
	}
	if _, err := d.fakeDriver.Create(ctx, s); err != nil {
		return driver.Ref{}, err
	}
	if d.armed != nil && *d.armed {
		return driver.Ref{ID: s.ID}, driver.ErrAlreadyExists
	}
	return driver.Ref{ID: s.ID}, nil
}

// withDriver opens a controller over one driver of the test's choosing.
func withDriver(t *testing.T, st Store, d driver.Driver, clock *fakeClock, o Options) *Controller {
	t.Helper()
	o.Store, o.Driver, o.Clock, o.Environment, o.Log = st, d, clock, "default", slog.New(slog.DiscardHandler)
	c, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestRecoveryAdopts: a create that finds the object already stamped is an
// adoption, so a recovery racing a second writer leaves one sandbox running
// and not two.
func TestRecoveryAdopts(t *testing.T) {
	clock := newClock()
	fake := newDriver(clock)
	st := newDurable(true)
	armed := false
	c := withDriver(t, st, adopting{fakeDriver: fake, armed: &armed}, clock, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(fake, id)
	armed = true

	acted, err := c.Reap(t.Context())
	if err != nil || acted != 1 {
		t.Fatalf("an adopted object was not a recovery: acted=%d err=%v", acted, err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil || got.Status.Phase != driver.Running {
		t.Fatalf("the adopted sandbox is %+v: %v", got.Status, err)
	}
}

// TestRecoveryExhausts: recreations back off and end in Failed with reason
// RecoveryExhausted, so a driver that refuses every create is not a loop.
func TestRecoveryExhausts(t *testing.T) {
	clock := newClock()
	fake := newDriver(clock)
	st := newDurable(true)
	refuses := errors.New("the driver refuses to create")
	c := withDriver(t, st, adopting{fakeDriver: fake, err: refuses}, clock, Options{RecoveryAttempts: 2})
	// The sandbox is desired and the driver never had it, which is what a
	// recovery against a failing driver looks like from the second tick on.
	obj := workspace()
	obj.Status = v1.SandboxStatus{ID: "sbx_exhausted", Owner: "alice", Environment: "default", Phase: driver.Running}
	c.mu.Lock()
	c.objects[obj.Status.ID] = obj
	c.mu.Unlock()

	for attempt := range 2 {
		if _, err := c.Reap(t.Context()); !errors.Is(err, refuses) {
			t.Fatalf("attempt %d returned %v, want the driver's failure", attempt+1, err)
		}
		got, err := c.Get(t.Context(), obj.Status.ID, "alice")
		if err != nil || got.Status.Phase != PhaseRecovering {
			t.Fatalf("attempt %d left %s: %v", attempt+1, got.Status.Phase, err)
		}
		// The next attempt waits out the backoff of design 005.
		clock.Advance(recoveryCeiling)
	}
	acted, err := c.Reap(t.Context())
	if err != nil || acted != 1 {
		t.Fatalf("the attempts were not exhausted: acted=%d err=%v", acted, err)
	}
	got, err := c.Get(t.Context(), obj.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != PhaseFailed || got.Status.Reason != ReasonRecoveryExhausted {
		t.Fatalf("the exhausted sandbox is %s/%s, want Failed/RecoveryExhausted", got.Status.Phase, got.Status.Reason)
	}
}

// TestRecoveryWaitsOutTheBackoff: a tick inside the backoff does not ask the
// driver again, which is what keeps a failing environment from being asked
// once per tick forever.
func TestRecoveryWaitsOutTheBackoff(t *testing.T) {
	clock := newClock()
	fake := newDriver(clock)
	st := newDurable(true)
	refuses := errors.New("the driver refuses to create")
	c := withDriver(t, st, adopting{fakeDriver: fake, err: refuses}, clock, Options{})
	obj := workspace()
	obj.Status = v1.SandboxStatus{ID: "sbx_backoff", Owner: "alice", Environment: "default", Phase: driver.Running}
	c.mu.Lock()
	c.objects[obj.Status.ID] = obj
	c.mu.Unlock()

	if _, err := c.Reap(t.Context()); !errors.Is(err, refuses) {
		t.Fatalf("the first attempt returned %v", err)
	}
	before := len(st.mutations(obj.Status.ID))
	clock.Advance(recoveryFloor - time.Second)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatalf("a tick inside the backoff returned %v", err)
	}
	if after := len(st.mutations(obj.Status.ID)); after != before {
		t.Fatalf("a tick inside the backoff wrote %d mutation(s)", after-before)
	}
	if got := recoveryBackoff(9); got != recoveryCeiling {
		t.Fatalf("the ninth backoff is %s, want the ceiling %s", got, recoveryCeiling)
	}
}

// TestRebuildPerTick: the observed index is replaced from the same list the
// deadline rules run over, and a rebuild that fails holds the lost rule for
// that tick rather than reporting a sandbox the driver has.
func TestRebuildPerTick(t *testing.T) {
	c, d, _, st := recovering(t, true, Options{})
	obj := created(t, c, "work")
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	states, rebuilds := st.index("default")
	if rebuilds == 0 || len(states) != 1 || states[0].ID != obj.Status.ID {
		t.Fatalf("the index holds %+v after %d rebuild(s)", states, rebuilds)
	}

	boom := errors.New("the store is unreachable")
	st.mu.Lock()
	st.rebuildErr = boom
	st.mu.Unlock()
	vanish(d, obj.Status.ID)
	if _, err := c.Reap(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("the tick returned %v, want the store's failure", err)
	}
	if got, _ := c.Get(t.Context(), obj.Status.ID, "alice"); got.Status.Phase == PhaseLost {
		t.Error("a sandbox was reported lost on a tick whose index was not rebuilt")
	}
}

// TestRebuildIsNotAskedOfASnapshotStore: a store that is only a Store has no
// observed index, and the lost rule still runs from the driver's list.
func TestRebuildIsNotAskedOfASnapshotStore(t *testing.T) {
	c, d, _ := newFake(t, Options{LostGrace: time.Minute})
	obj := created(t, c, "work")
	vanish(d, obj.Status.ID)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), obj.Status.ID, "alice")
	if err != nil || got.Status.Phase != PhaseLost {
		t.Fatalf("the vanished sandbox is %+v: %v", got.Status, err)
	}
}

// TestRecoveryEndToEndOverNative drives the whole rule over the native driver
// with no fake in the way: a sandbox's workspace is deleted behind the
// controller's back, and the next tick has it running again under the same id.
func TestRecoveryEndToEndOverNative(t *testing.T) {
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st := newDurable(true)
	c, err := Open(t.Context(), Options{
		Store: st, Driver: d, Environment: "default",
		ReapInterval: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	obj, err := realized(t.Context(), c, workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := obj.Status.ID
	if err := d.Delete(t.Context(), id); err != nil {
		t.Fatalf("deleting the driver's object: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunReaper(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the lost sandbox to be recreated on the driver", func() bool {
		return slices.Contains(st.mutations(id), MutationRecovered)
	})
	state, err := d.Inspect(t.Context(), id)
	if err != nil {
		t.Fatalf("the driver does not have the recovered sandbox: %v", err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != state.Phase {
		t.Fatalf("the record says %s and the driver says %s", got.Status.Phase, state.Phase)
	}
	if state.ID != id || state.Name != obj.Metadata.Name || !maps.Equal(state.Labels, obj.Metadata.Labels) {
		t.Fatalf("the recovered object is %+v, want the id, name and labels of %+v", state, obj)
	}
	want := []string{MutationLost, MutationRecovering, MutationRecovered}
	if got := st.mutations(id); len(got) < 3 || !slices.Equal(got[len(got)-3:], want) {
		t.Fatalf("the journal reads %v, want it to end %v", got, want)
	}
}

// hangUp is a driver whose create is where the loop stops: it ends the
// loop's context and answers what the Kubernetes driver answers then.
type hangUp struct {
	*fakeDriver
	cancel context.CancelFunc
}

func (d hangUp) Create(ctx context.Context, _ driver.CreateSpec) (driver.Ref, error) {
	d.cancel()
	return driver.Ref{}, ctx.Err()
}

// TestAStoppedLoopLeavesTheCreateToTheNext: a loop that stops while the
// driver creates writes nothing after the call. The row stays as the status
// write left it, placed with its boundary record and its identity, rather
// than Failed, and the next pass creates the sandbox from there and ends the
// identity the stopped pass minted.
func TestAStoppedLoopLeavesTheCreateToTheNext(t *testing.T) {
	clock := newClock()
	st := newDurable(true)
	st.live = true
	tokens := newTokens(clock, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	fake := newDriver(clock)
	c := withDriver(t, st, hangUp{fakeDriver: fake, cancel: cancel}, clock, Options{Tokens: tokens})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := answered.Status.ID
	if _, err := finished(ctx, c, answered, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass the loop stopped answered %v", err)
	}
	st.mu.Lock()
	stored, ok := st.objects[id]
	st.mu.Unlock()
	if !ok || !placed(stored) || stored.Status.EgressState == nil || stored.Status.TokenState == nil {
		t.Fatalf("the store holds %+v (present %v), want the sandbox placed with its boundary and identity", stored.Status, ok)
	}
	if got := st.mutations(id); slices.Contains(got, MutationFailed) {
		t.Fatalf("the journal holds %v, a failure for a create nobody refused", got)
	}
	first := stored.Status.TokenState.JTI

	// The next pass runs over a driver that creates.
	c.setDriver("default", fake)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil || got.Status.Phase != driver.Running {
		t.Fatalf("the next pass left %s: %v", got.Status.Phase, err)
	}
	if _, revoked, _ := tokens.read(); !slices.Contains(revoked, first) {
		t.Fatalf("the identity the stopped pass minted, %s, is still live: revoked %v", first, revoked)
	}
}
