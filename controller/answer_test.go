// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// gated is a driver whose Create says it has been entered and then waits for
// the case to release it, which is how a case holds a create in the driver's
// hands while it acts on the sandbox.
type gated struct {
	*fakeDriver
	entered chan string
	release chan struct{}
}

func newGated(clock *fakeClock) gated {
	return gated{fakeDriver: newDriver(clock), entered: make(chan string, 8), release: make(chan struct{})}
}

func (d gated) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	d.entered <- s.ID
	select {
	case <-d.release:
	case <-ctx.Done():
		return driver.Ref{}, ctx.Err()
	}
	return d.fakeDriver.Create(ctx, s)
}

// pass runs one scheduler pass on its own goroutine, which a gated driver
// holds inside the driver's create, and returns what the pass answers once it
// has.
func pass(t *testing.T, c *Controller) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Schedule(t.Context()) }()
	return done
}

// within runs fn and fails the case when it has not returned by the bound. It
// is how a case proves an act does not wait for the driver's create: an act
// that waited would wait for a release the case never sends before it.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s waited for the driver's create", what)
	}
}

// TestCreateAnswersBeforeTheDriver: a create placed now answers Pending with
// no Scheduled condition before its driver's create has begun, the scheduler
// loop the create woke takes it to Running, and the journal says created,
// then the status written before the driver call, then started.
func TestCreateAnswersBeforeTheDriver(t *testing.T) {
	clock := newClock()
	st := newDurable(true)
	d := newGated(clock)
	c := withDriver(t, st, d, clock, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	loop := make(chan struct{})
	go func() { defer close(loop); c.RunScheduler(ctx) }()
	defer func() { cancel(); <-loop }()

	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := answered.Status.ID
	if answered.Status.Phase != driver.Pending || conditionOf(answered, v1.ConditionScheduled).Type != "" {
		t.Fatalf("the create answered %s with %+v, want Pending and no Scheduled condition",
			answered.Status.Phase, answered.Status.Conditions)
	}
	if got := <-d.entered; got != id {
		t.Fatalf("the loop created %s, want %s", got, id)
	}
	if len(d.sandboxes()) != 0 {
		t.Fatal("the driver held the sandbox before its create was released")
	}
	close(d.release)
	waitFor(t, "the sandbox to run", func() bool {
		got, err := c.Get(t.Context(), id, "alice")
		return err == nil && got.Status.Phase == driver.Running
	})
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if reasonOf(got, v1.ConditionScheduled) != v1.ReasonPlaced {
		t.Fatalf("Scheduled is %q, want Placed", reasonOf(got, v1.ConditionScheduled))
	}
	want := []string{MutationCreated, MutationStatus, MutationStarted}
	if got := st.mutations(id); !slices.Equal(got, want) {
		t.Fatalf("the journal reads %v, want %v", got, want)
	}
}

// TestTheDriverCallHoldsNoLock: while the loop's driver create is in flight,
// a read, a list and a second create all answer, the second one Pending, and
// both sandboxes run once the driver returns.
func TestTheDriverCallHoldsNoLock(t *testing.T) {
	clock := newClock()
	d := newGated(clock)
	c := withDriver(t, newDurable(true), d, clock, Options{})
	first, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := pass(t, c)
	<-d.entered

	var second v1.Sandbox
	within(t, "a second pass, a read, a list and a create", func() {
		// A second pass leaves a sandbox whose create is in flight to the
		// pass that has it, rather than asking the driver twice.
		if err := c.Schedule(t.Context()); err != nil {
			t.Error(err)
		}
		if len(d.entered) != 0 {
			t.Error("a second pass asked the driver for a sandbox already being created")
		}
		if _, err := c.Get(t.Context(), first.Status.ID, "alice"); err != nil {
			t.Error(err)
		}
		if n := len(c.List()); n != 1 {
			t.Errorf("the list holds %d sandboxes, want one", n)
		}
		next := workspace()
		next.Metadata.Name = "second"
		if second, err = c.Create(t.Context(), next, "alice", 0); err != nil {
			t.Error(err)
		}
	})
	if second.Status.Phase != driver.Pending {
		t.Fatalf("the second create answered %s, want Pending", second.Status.Phase)
	}
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The second create was written while the pass was in the driver, so the
	// pass it woke is the one that takes it.
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []v1.Sandbox{first, second} {
		got, err := c.Get(t.Context(), obj.Status.ID, "alice")
		if err != nil || got.Status.Phase != driver.Running {
			t.Fatalf("%s is %s: %v, want Running", obj.Metadata.Name, got.Status.Phase, err)
		}
	}
}

// TestAFailedRealizeUndoes is the undo of the create order on the loop: a
// driver create that fails leaves the child Failed with CreateFailed, its
// principal purged from the gateway, its identity revoked and its parent's
// unit credited back.
func TestAFailedRealizeUndoes(t *testing.T) {
	gw := &gateway{}
	clock := newClock()
	tokens := newTokens(clock, time.Hour)
	c, d, _ := newFakeOver(t, Options{Egress: gw, Tokens: tokens}, newDriver(clock), clock)
	parent, err := realized(t.Context(), c, root("planner", 2, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	d.set(func(d *fakeDriver) { d.createErr = errors.New("the volume did not attach") })
	answered, err := c.Spawn(t.Context(), child("worker", 0, 0), parent, 0)
	if err != nil || answered.Status.Phase != driver.Pending {
		t.Fatalf("the spawn answered %s and %v, want Pending", answered.Status.Phase, err)
	}
	if got, _ := c.Get(t.Context(), parent.Status.ID, "alice"); got.Status.Spawn.Used != 1 {
		t.Fatalf("the parent's budget used is %d after the debit, want 1", got.Status.Spawn.Used)
	}
	got, err := finished(t.Context(), c, answered, nil)
	if err == nil {
		t.Fatal("a driver create that failed was reported as a success")
	}
	if got.Status.Phase != PhaseFailed || got.Status.Reason != ReasonCreateFailed {
		t.Fatalf("the child is %s/%s, want Failed/CreateFailed", got.Status.Phase, got.Status.Reason)
	}
	_, revoked, _ := tokens.read()
	if len(revoked) != 1 {
		t.Fatalf("revoked %v, want the child's one identity", revoked)
	}
	gw.mu.Lock()
	purged := slices.Clone(gw.purged)
	gw.mu.Unlock()
	if !slices.Contains(purged, egress.Principal(answered.Status.ID)) {
		t.Fatalf("purged %v, want the child's principal", purged)
	}
	if got, _ := c.Get(t.Context(), parent.Status.ID, "alice"); got.Status.Spawn.Used != 0 {
		t.Fatalf("the parent's budget used is %d after the failure, want the unit credited back", got.Status.Spawn.Used)
	}
}

// existing is a driver whose Create answers ErrAlreadyExists for an id it
// already holds, which is what every real driver answers for a stamped id.
type existing struct{ *fakeDriver }

func (d existing) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	d.mu.Lock()
	_, held := d.states[s.ID]
	d.mu.Unlock()
	if held {
		return driver.Ref{ID: s.ID}, driver.ErrAlreadyExists
	}
	return d.fakeDriver.Create(ctx, s)
}

// madeThenStopped is a driver whose create makes the object and then the
// loop stops before the result is written, which is a process that ends
// between the driver's answer and the settle.
type madeThenStopped struct {
	*fakeDriver
	cancel context.CancelFunc
}

func (d madeThenStopped) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	ref, err := d.fakeDriver.Create(ctx, s)
	d.cancel()
	return ref, err
}

// TestTheLoopRealizesWhatARestartLeft: a fresh controller over a store that
// holds a placed and unrealized sandbox realizes it on the loop's first pass.
// With nothing made it creates the sandbox; with the object made and the
// result unwritten, the create answers ErrAlreadyExists, the object is taken
// with the new identity projected into it, and the identity the first process
// minted is revoked.
func TestTheLoopRealizesWhatARestartLeft(t *testing.T) {
	t.Run("nothingMade", func(t *testing.T) {
		clock := newClock()
		st := newDurable(true)
		fake := newDriver(clock)
		first := withDriver(t, st, fake, clock, Options{})
		answered, err := first.Create(t.Context(), workspace(), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		again := withDriver(t, st, existing{fake}, clock, Options{})
		ctx, cancel := context.WithCancel(t.Context())
		loop := make(chan struct{})
		go func() { defer close(loop); again.RunScheduler(ctx) }()
		defer func() { cancel(); <-loop }()
		waitFor(t, "the restarted loop to create the sandbox", func() bool {
			got, err := again.Get(t.Context(), answered.Status.ID, "alice")
			return err == nil && got.Status.Phase == driver.Running
		})
		if n := len(fake.sandboxes()); n != 1 {
			t.Fatalf("the driver holds %d sandboxes, want one", n)
		}
	})
	t.Run("objectMade", func(t *testing.T) {
		clock := newClock()
		st := newDurable(true)
		fake := newDriver(clock)
		tokens := newTokens(clock, time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		first := withDriver(t, st, madeThenStopped{fakeDriver: fake, cancel: cancel}, clock, Options{Tokens: tokens})
		answered, err := first.Create(t.Context(), workspace(), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		id := answered.Status.ID
		if err := first.Schedule(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("the stopped pass answered %v", err)
		}
		st.mu.Lock()
		left := st.objects[id]
		st.mu.Unlock()
		if !placed(left) || left.Status.TokenState == nil {
			t.Fatalf("the store holds %+v, want the sandbox placed with the identity the pass minted", left.Status)
		}
		minted := left.Status.TokenState.JTI

		again := withDriver(t, st, existing{fake}, clock, Options{Tokens: tokens})
		if err := again.Schedule(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, err := again.Get(t.Context(), id, "alice")
		if err != nil || got.Status.Phase != driver.Running {
			t.Fatalf("the restarted pass left %s: %v, want Running", got.Status.Phase, err)
		}
		if n := len(fake.sandboxes()); n != 1 {
			t.Fatalf("the driver holds %d sandboxes, want the one the first process made", n)
		}
		_, revoked, _ := tokens.read()
		if !slices.Equal(revoked, []string{minted}) {
			t.Fatalf("revoked %v, want the first process's %s", revoked, minted)
		}
		if held := fake.tokenOf(id); held == "" || held == "token-"+minted {
			t.Fatalf("the sandbox holds %q, want the identity the restarted pass minted", held)
		}
	})
}

// TestDeleteDuringACreate: a delete before the pass takes the sandbox leaves
// no driver object ever made; a delete while the driver creates answers
// Deleting at once, and the settle removes the object the create made, purges
// its map and revokes its identity.
func TestDeleteDuringACreate(t *testing.T) {
	t.Run("beforeThePass", func(t *testing.T) {
		c, d, _ := newFake(t, Options{})
		answered, err := c.Create(t.Context(), workspace(), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		gone, err := c.Act(t.Context(), answered.Status.ID, "delete")
		if err != nil || gone.Status.Phase != PhaseDeleting {
			t.Fatalf("the delete answered %s and %v, want Deleting", gone.Status.Phase, err)
		}
		if err := c.Schedule(t.Context()); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		made := len(d.specs)
		d.mu.Unlock()
		if made != 0 {
			t.Fatalf("the driver was asked for %d sandboxes, want none", made)
		}
	})
	t.Run("duringTheDriverCall", func(t *testing.T) {
		clock := newClock()
		st := newDurable(true)
		gw := &gateway{}
		tokens := newTokens(clock, time.Hour)
		d := newGated(clock)
		c := withDriver(t, st, d, clock, Options{Egress: gw, Tokens: tokens})
		answered, err := c.Create(t.Context(), workspace(), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		id := answered.Status.ID
		done := pass(t, c)
		<-d.entered
		within(t, "the delete", func() {
			gone, err := c.Act(t.Context(), id, "delete")
			if err != nil || gone.Status.Phase != PhaseDeleting {
				t.Errorf("the delete answered %s and %v, want Deleting", gone.Status.Phase, err)
			}
		})
		close(d.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, err := d.Inspect(t.Context(), id); !errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("the object the create made outlived the delete: %v", err)
		}
		if _, err := c.Get(t.Context(), id, "alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the deleted sandbox reads %v", err)
		}
		if _, revoked, _ := tokens.read(); !slices.Contains(revoked, "jti-1") {
			t.Fatalf("revoked %v, want the create's identity", revoked)
		}
		if got := st.mutations(id); got[len(got)-1] != MutationDeleted {
			t.Fatalf("the journal reads %v, want it to end deleted", got)
		}
	})
}

// TestStopDuringACreate: stop and start are phase_conflict for a sandbox the
// loop has not taken and for one whose driver create is in flight, the
// driver is never asked, and both work once the create settles.
func TestStopDuringACreate(t *testing.T) {
	clock := newClock()
	d := newGated(clock)
	c := withDriver(t, newDurable(true), d, clock, Options{})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := answered.Status.ID
	refused := func(when string) {
		for _, verb := range []string{"stop", "start"} {
			if _, err := c.Act(t.Context(), id, verb); !errors.Is(err, ErrPhase) {
				t.Fatalf("%s %s answered %v, want %v", verb, when, err, ErrPhase)
			}
		}
	}
	refused("before the pass")
	done := pass(t, c)
	<-d.entered
	within(t, "the stop", func() { refused("during the driver call") })
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stops, _, _ := d.acted(); len(stops) != 0 {
		t.Fatalf("the driver was asked to stop %v during the create", stops)
	}
	if got, err := c.Act(t.Context(), id, "stop"); err != nil || got.Status.Phase != driver.Stopped {
		t.Fatalf("the stop after the create answered %s and %v", got.Status.Phase, err)
	}
}

// TestAnApplyDuringACreateIsKept: an apply while the driver creates writes
// its specification, and the settle writes the phase onto the row as the
// apply left it rather than over it.
func TestAnApplyDuringACreateIsKept(t *testing.T) {
	clock := newClock()
	d := newGated(clock)
	c := withDriver(t, newDurable(true), d, clock, Options{})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := answered.Status.ID
	done := pass(t, c)
	<-d.entered
	within(t, "the apply", func() {
		next, err := c.Get(t.Context(), id, "alice")
		if err != nil {
			t.Error(err)
			return
		}
		next.Metadata.Labels = map[string]string{"team": "b"}
		if _, err := c.Update(t.Context(), next); err != nil {
			t.Error(err)
		}
	})
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil || got.Status.Phase != driver.Running || got.Metadata.Labels["team"] != "b" {
		t.Fatalf("the sandbox is %s with %v: %v, want Running with the applied label", got.Status.Phase, got.Metadata.Labels, err)
	}
}

// TestAFailedStatusWriteLeavesTheCreatePlaced: a status write the store
// refuses ends the pass before the driver is asked, with the identity it
// minted revoked and the principal purged, and the next pass creates the
// sandbox from the row the store still holds.
func TestAFailedStatusWriteLeavesTheCreatePlaced(t *testing.T) {
	gw := &gateway{}
	clock := newClock()
	tokens := newTokens(clock, time.Hour)
	store := &memoryStore{saveErr: errors.New("write failed"), failOn: 2}
	c, d, _ := newFakeOver(t, Options{Store: store, Egress: gw, Tokens: tokens}, newDriver(clock), clock)
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a status write the store refused was reported as a success")
	}
	got, err := c.Get(t.Context(), answered.Status.ID, "alice")
	if err != nil || !placed(got) {
		t.Fatalf("the sandbox is %s: %v, want it placed for the next pass", got.Status.Phase, err)
	}
	if len(d.sandboxes()) != 0 {
		t.Fatal("the driver was asked for a sandbox whose status write failed")
	}
	if _, revoked, _ := tokens.read(); !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("revoked %v, want the identity of the failed pass", revoked)
	}
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err = c.Get(t.Context(), answered.Status.ID, "alice"); err != nil || got.Status.Phase != driver.Running {
		t.Fatalf("the next pass left %s: %v, want Running", got.Status.Phase, err)
	}
}

// TestADeleteThatFailsDuringACreateKeepsTheRow: a delete whose driver call
// fails while the create is in flight leaves the row Deleting, the settle
// leaves the object to the delete, and the retried delete removes it.
func TestADeleteThatFailsDuringACreateKeepsTheRow(t *testing.T) {
	clock := newClock()
	d := newGated(clock)
	c := withDriver(t, newDurable(true), d, clock, Options{})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := answered.Status.ID
	done := pass(t, c)
	<-d.entered
	d.set(func(d *fakeDriver) { d.deleteErr = errors.New("the runtime is restarting") })
	within(t, "the delete", func() {
		if _, err := c.Act(t.Context(), id, "delete"); err == nil {
			t.Error("a delete the driver refused was reported as a success")
		}
	})
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), id, "alice")
	if err != nil || got.Status.Phase != PhaseDeleting {
		t.Fatalf("the sandbox is %s: %v, want the delete's Deleting", got.Status.Phase, err)
	}
	d.set(func(d *fakeDriver) { d.deleteErr = nil })
	if _, err := c.Act(t.Context(), id, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(t.Context(), id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the retried delete left the object: %v", err)
	}
}

// TestADriverThatDoesNotReportYet: a create the driver took and does not yet
// report is written Pending with Scheduled True, so the loop hands it on
// rather than creating it again, and a read says what it is once the driver
// does.
func TestADriverThatDoesNotReportYet(t *testing.T) {
	c, d, _ := newFake(t, Options{})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	d.set(func(d *fakeDriver) { d.inspectErr = driver.ErrNotFound })
	got, err := finished(t.Context(), c, answered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != driver.Pending || placed(got) {
		t.Fatalf("the sandbox is %s with %+v, want Pending and handed on", got.Status.Phase, got.Status.Conditions)
	}
	d.set(func(d *fakeDriver) { d.inspectErr = nil })
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := d.createdCount(); n != 1 {
		t.Fatalf("the driver was asked %d times, want once", n)
	}
	if read, err := c.Refresh(t.Context(), got); err != nil || read.Status.Phase != driver.Running {
		t.Fatalf("the read says %s: %v, want Running", read.Status.Phase, err)
	}
}

// TestAnAdoptionThatFailsUndoes holds the adoption's undo on the request: a
// boundary no gateway acknowledges leaves no row, and an identity the mint
// refuses or an adoption the driver fails for another reason leaves the
// sandbox Failed with CreateFailed.
func TestAnAdoptionThatFailsUndoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(c *Controller, d *fakeDriver, gw *gateway, tokens *fakeTokens)
		row   bool
	}{
		{"noGatewayAcknowledges", func(_ *Controller, _ *fakeDriver, gw *gateway, _ *fakeTokens) { gw.sendOnly = true }, false},
		{"theMintRefuses", func(_ *Controller, _ *fakeDriver, _ *gateway, tokens *fakeTokens) {
			tokens.fail(errors.New("the key is unavailable"), nil)
		}, true},
		{"theAdoptionFails", func(_ *Controller, d *fakeDriver, _ *gateway, _ *fakeTokens) {
			d.set(func(d *fakeDriver) { d.updateErr = errors.New("the entry did not answer") })
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &gateway{}
			clock := newClock()
			tokens := newTokens(clock, time.Hour)
			d := newDriver(clock)
			d.pool = true
			o := poolOptions(1)
			o.Egress, o.Tokens = gw, tokens
			c, _, _ := newFakeOver(t, o, d, clock)
			if _, err := c.Refill(t.Context()); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Hour)
			tc.setUp(c, d, gw, tokens)
			// A bounded manifest is one whose map a gateway must hold, and the
			// pool's match reads nothing of the boundary.
			got, err := c.Create(t.Context(), bounded(), "alice", 0)
			if err == nil {
				t.Fatal("a failed adoption was reported as a success")
			}
			if d.adopted() != 0 && !tc.row {
				t.Fatal("the driver adopted an entry whose map no gateway held")
			}
			c.mu.Lock()
			_, held := c.objects[got.Status.ID]
			c.mu.Unlock()
			if held != tc.row {
				t.Fatalf("a row is held: %v, want %v", held, tc.row)
			}
			if tc.row && (got.Status.Phase != PhaseFailed || got.Status.Reason != ReasonCreateFailed) {
				t.Fatalf("the sandbox is %s/%s, want Failed/CreateFailed", got.Status.Phase, got.Status.Reason)
			}
		})
	}
}

// originKey is a value a request carries, which the records of its create's
// start read.
type originKey struct{}

// originRecorder notes the value each act's context carried.
type originRecorder struct {
	actRecorder
	seen map[string]any
}

func (r *originRecorder) Emit(ctx context.Context, a Act) {
	r.mu.Lock()
	r.seen[a.Type] = ctx.Value(originKey{})
	r.mu.Unlock()
	r.actRecorder.Emit(ctx, a)
}

// TestTheStartCarriesTheCreatesRequest: the loop writes a direct create's
// start under the values of the request that made it, so its records name
// the caller and the request as they did when the request ran the create;
// the loop's own cancellation still reaches the driver.
func TestTheStartCarriesTheCreatesRequest(t *testing.T) {
	events := &originRecorder{seen: map[string]any{}}
	c, _, _ := newFake(t, Options{Events: events})
	request := context.WithValue(t.Context(), originKey{}, "req_1")
	if _, err := c.Create(request, workspace(), "alice", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if got := events.seen[MutationStarted]; got != "req_1" {
		t.Fatalf("the start was written under %v, want the create's request", got)
	}
}

// madeThenHeld is a driver whose create makes the object, running, and then
// waits for the case to release it, which is a Kubernetes create whose Pod
// runs before the driver's own wait for readiness has returned.
type madeThenHeld struct{ gated }

func (d madeThenHeld) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	ref, err := d.fakeDriver.Create(ctx, s)
	if err != nil {
		return ref, err
	}
	d.entered <- s.ID
	select {
	case <-d.release:
	case <-ctx.Done():
		return driver.Ref{}, ctx.Err()
	}
	return ref, nil
}

// TestAReadDuringACreateSaysPending: while the loop's create is in the
// driver's hands, a read answers the sandbox Pending even where the driver
// already reports it running, so no reader acts on a sandbox whose create
// has not settled: a stop, a dial or a screenshot the read would invite is
// refused or unready until the create's own read is written. Once it is,
// the read says Running.
func TestAReadDuringACreateSaysPending(t *testing.T) {
	clock := newClock()
	d := madeThenHeld{newGated(clock)}
	c := withDriver(t, newDurable(true), d, clock, Options{})
	answered, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := pass(t, c)
	<-d.entered
	within(t, "the read", func() {
		read, err := c.Refresh(t.Context(), answered)
		if err != nil || read.Status.Phase != driver.Pending {
			t.Errorf("a read during the create answered %s and %v, want Pending", read.Status.Phase, err)
		}
	})
	close(d.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	settled, err := c.Get(t.Context(), answered.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read, err := c.Refresh(t.Context(), settled); err != nil || read.Status.Phase != driver.Running {
		t.Fatalf("a read after the create answered %s and %v, want Running", read.Status.Phase, err)
	}
}
