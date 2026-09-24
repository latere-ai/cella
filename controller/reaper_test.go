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
	"latere.ai/x/cella/runtime/runtimetest"
)

// epoch is the instant every fake clock starts at.
var epoch = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// fakeClock moves only when a test advances it, and hands the loop the tick
// channel the test fires, so no reaper test sleeps for a deadline.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	stopped bool
}

func newClock() *fakeClock { return &fakeClock{now: epoch, ticks: make(chan time.Time)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func (c *fakeClock) Ticker(time.Duration) (<-chan time.Time, func()) {
	return c.ticks, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopped = true
	}
}
func (c *fakeClock) tickerStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// fakeDriver keeps one map of states that List, Inspect, Stop and Delete all
// read and write, so a state a test changes between the list and the action is
// exactly what the reaper's revalidation sees.
type fakeDriver struct {
	runtimetest.Nop
	clock                                             *fakeClock
	mu                                                sync.Mutex
	states                                            map[string]driver.State
	order                                             []string
	listErr, inspectErr, stopErr, deleteErr, touchErr error
	createErr, updateErr, startErr                    error
	stops, deletes, touches, starts                   []string
	onList                                            func()
	// projected is what the driver holds inside each sandbox: the token the
	// create carried, replaced by every re-projection an update makes.
	projected map[string]string
	// pool says whether this fake declares the capability, adoptions counts
	// the entries taken, and onAdopt runs inside the adoption under the
	// driver's lock, which is where a test makes one adopter lose.
	pool      bool
	adoptions int
	onAdopt   func(id string)
	// specs is the create spec each sandbox was made from, so a test reads
	// back what the refill loop asked for.
	specs map[string]driver.CreateSpec
	// readyErr is what this driver answers its readiness probe with, which
	// is what the phase of an in-process environment is computed from.
	readyErr error
}

// Ready answers what a test set, so a case drives the phase machine of spec
// 021 without an environment that really failed.
func (d *fakeDriver) Ready(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.readyErr
}

// failReady sets what the readiness probe answers from now on.
func (d *fakeDriver) failReady(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readyErr = err
}

// declarePool makes this driver one that can keep prewarmed entries.
func (d *fakeDriver) declarePool() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pool = true
}

// sandboxes is every state this driver holds that is not a pool entry.
func (d *fakeDriver) sandboxes() []driver.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []driver.State
	for _, id := range d.order {
		if s, ok := d.states[id]; ok && !s.Pool {
			out = append(out, s)
		}
	}
	return out
}

func newDriver(clock *fakeClock) *fakeDriver {
	return &fakeDriver{clock: clock, states: map[string]driver.State{},
		projected: map[string]string{}, specs: map[string]driver.CreateSpec{}}
}

// Capabilities declares the pool where a test asked for one, so the controller
// branches on the declaration the way it does on a real driver.
func (d *fakeDriver) Capabilities() driver.Capabilities {
	d.mu.Lock()
	defer d.mu.Unlock()
	return driver.Capabilities{Pool: d.pool}
}
func (d *fakeDriver) Create(_ context.Context, s driver.CreateSpec) (driver.Ref, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return driver.Ref{}, d.createErr
	}
	if err := s.CheckPrewarm(); err != nil {
		return driver.Ref{}, err
	}
	now := d.clock.Now()
	state := driver.State{ID: s.ID, Name: s.Name, Owner: s.Owner, Phase: driver.Running, Isolation: driver.IsolationNone,
		Labels: maps.Clone(s.Labels), CreatedAt: now, StartedAt: now, LastActivityAt: now,
		AutoStop: s.Lifecycle.AutoStop, AutoDelete: s.Lifecycle.AutoDelete, Pool: s.Prewarm}
	d.specs[s.ID] = s
	if s.Lifecycle.TTL > 0 {
		state.ExpiresAt = now.Add(s.Lifecycle.TTL)
	}
	d.states[s.ID] = state
	d.order = append(d.order, s.ID)
	if len(s.Token) > 0 {
		d.projected[s.ID] = string(s.Token)
	}
	return driver.Ref{ID: s.ID}, nil
}
func (d *fakeDriver) List(_ context.Context, f driver.Filter) ([]driver.State, error) {
	d.mu.Lock()
	if d.listErr != nil {
		d.mu.Unlock()
		return nil, d.listErr
	}
	out := make([]driver.State, 0, len(d.states))
	for _, id := range d.order {
		if s, ok := d.states[id]; ok && f.Selects(s) {
			out = append(out, s)
		}
	}
	hook := d.onList
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}
func (d *fakeDriver) Inspect(_ context.Context, id string) (driver.State, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inspectErr != nil {
		return driver.State{}, d.inspectErr
	}
	s, ok := d.states[id]
	if !ok {
		return driver.State{}, driver.ErrNotFound
	}
	return s, nil
}
func (d *fakeDriver) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopErr != nil {
		return d.stopErr
	}
	s, ok := d.states[id]
	if !ok {
		return driver.ErrNotFound
	}
	s.Phase, s.StoppedAt = driver.Stopped, d.clock.Now()
	d.states[id] = s
	d.stops = append(d.stops, id)
	return nil
}
func (d *fakeDriver) Delete(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deleteErr != nil {
		return d.deleteErr
	}
	delete(d.states, id)
	delete(d.projected, id)
	d.deletes = append(d.deletes, id)
	return nil
}
func (d *fakeDriver) Touch(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.touches = append(d.touches, id)
	if d.touchErr != nil {
		return d.touchErr
	}
	s, ok := d.states[id]
	if !ok {
		return driver.ErrNotFound
	}
	s.LastActivityAt = d.clock.Now()
	d.states[id] = s
	return nil
}

// put writes one state directly: the substrate a test wants the reaper to find,
// with or without a desired record behind it.
func (d *fakeDriver) put(s driver.State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.states[s.ID]; !ok {
		d.order = append(d.order, s.ID)
	}
	d.states[s.ID] = s
}
func (d *fakeDriver) state(id string) driver.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.states[id]
}
func (d *fakeDriver) acted() (stops, deletes, touches []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.stops), slices.Clone(d.deletes), slices.Clone(d.touches)
}
func (d *fakeDriver) set(fn func(*fakeDriver)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

// fakeLease answers what a test told it to and counts the asks.
type fakeLease struct {
	mu    sync.Mutex
	held  bool
	err   error
	calls int
	name  string
	ttl   time.Duration
}

func (l *fakeLease) Acquire(_ context.Context, name string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls, l.name, l.ttl = l.calls+1, name, ttl
	return l.held, l.err
}
func (l *fakeLease) answer(held bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held, l.err = held, err
}
func (l *fakeLease) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func newFake(t *testing.T, o Options) (*Controller, *fakeDriver, *fakeClock) {
	t.Helper()
	clock := newClock()
	return newFakeOver(t, o, newDriver(clock), clock)
}

// newFakeOver is newFake over a driver the caller already configured, which is
// how a pool case declares the capability before the controller opens.
func newFakeOver(t *testing.T, o Options, d *fakeDriver, clock *fakeClock) (*Controller, *fakeDriver, *fakeClock) {
	t.Helper()
	o.Driver, o.Clock, o.Environment, o.Log = d, clock, "default", slog.New(slog.DiscardHandler)
	if o.Store == nil && o.DataDir == "" {
		o.DataDir = t.TempDir()
	}
	c, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, d, clock
}

func created(t *testing.T, c *Controller, name string) v1.Sandbox {
	t.Helper()
	obj := workspace()
	obj.Metadata.Name = name
	got, err := realized(t.Context(), c, obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// waitFor polls until the condition holds or the test fails; the reaper's loop
// runs on its own goroutine and a fixed sleep would be a race either way.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	// Thirty seconds is far above what any case needs on an idle machine;
	// the instrumented run of the cover gate beside other suites is what
	// the bound is for.
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestReaperRules(t *testing.T) {
	now := epoch.Add(time.Hour)
	running := driver.State{ID: "sbx", Phase: driver.Running, CreatedAt: epoch, LastActivityAt: now}
	stopped := driver.State{ID: "sbx", Phase: driver.Stopped, CreatedAt: epoch, StoppedAt: now}
	with := func(s driver.State, fn func(*driver.State)) driver.State {
		fn(&s)
		return s
	}
	for _, tc := range []struct {
		name string
		s    driver.State
		want string
	}{
		{"noLifecycleIsNever", running, ""},
		{"expiredAtTheSecond", with(running, func(s *driver.State) { s.ExpiresAt = now }), ReasonExpired},
		{"expiredOneInstantBefore", with(running, func(s *driver.State) { s.ExpiresAt = now.Add(time.Nanosecond) }), ""},
		{"expiredWhileStopped", with(stopped, func(s *driver.State) { s.ExpiresAt = now }), ReasonExpired},
		{"noExpiryIsNever", with(running, func(s *driver.State) { s.ExpiresAt = time.Time{} }), ""},
		{"autoDeleteAtTheSecond", with(stopped, func(s *driver.State) { s.StoppedAt, s.AutoDelete = now.Add(-5*time.Minute), 5*time.Minute }), ReasonAutoDelete},
		{"autoDeleteOneInstantBefore", with(stopped, func(s *driver.State) {
			s.StoppedAt, s.AutoDelete = now.Add(-5*time.Minute+time.Nanosecond), 5*time.Minute
		}), ""},
		{"autoDeleteZeroIsNever", with(stopped, func(s *driver.State) { s.StoppedAt = now.Add(-time.Hour) }), ""},
		{"autoDeleteNeedsAStopInstant", with(stopped, func(s *driver.State) { s.StoppedAt, s.AutoDelete = time.Time{}, time.Second }), ""},
		{"autoDeleteOnlyWhenStopped", with(running, func(s *driver.State) { s.StoppedAt, s.AutoDelete = now.Add(-time.Hour), time.Minute }), ""},
		{"autoStopAtTheSecond", with(running, func(s *driver.State) { s.LastActivityAt, s.AutoStop = now.Add(-10*time.Minute), 10*time.Minute }), ReasonAutoStop},
		{"autoStopOneInstantBefore", with(running, func(s *driver.State) {
			s.LastActivityAt, s.AutoStop = now.Add(-10*time.Minute+time.Nanosecond), 10*time.Minute
		}), ""},
		{"autoStopZeroIsNever", with(running, func(s *driver.State) { s.LastActivityAt = now.Add(-time.Hour) }), ""},
		{"autoStopCountsFromCreationWithoutActivity", with(running, func(s *driver.State) {
			s.CreatedAt, s.LastActivityAt, s.AutoStop = now.Add(-10*time.Minute), time.Time{}, 10*time.Minute
		}), ReasonAutoStop},
		{"autoStopOnlyWhenRunning", with(stopped, func(s *driver.State) { s.LastActivityAt, s.AutoStop = now.Add(-time.Hour), time.Minute }), ""},
		{"expiredBeatsAutoStop", with(running, func(s *driver.State) {
			s.ExpiresAt, s.LastActivityAt, s.AutoStop = now, now.Add(-time.Hour), time.Minute
		}), ReasonExpired},
		{"expiredBeatsAutoDelete", with(stopped, func(s *driver.State) {
			s.ExpiresAt, s.StoppedAt, s.AutoDelete = now, now.Add(-time.Hour), time.Minute
		}), ReasonExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reapRule(tc.s, now); got != tc.want {
				t.Fatalf("rule %q, want %q, for %+v", got, tc.want, tc.s)
			}
		})
	}
}

func TestReaperActs(t *testing.T) {
	c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{AutoStop: 10 * time.Minute, AutoDelete: 5 * time.Minute}})
	expired := created(t, c, "expired")
	deletable := created(t, c, "deletable")
	idle := created(t, c, "idle")
	untracked := driver.State{ID: "sbx_untracked", Phase: driver.Running, CreatedAt: epoch, LastActivityAt: epoch, AutoStop: time.Minute}
	d.put(untracked)
	d.put(func() driver.State {
		s := d.state(expired.Status.ID)
		s.ExpiresAt = epoch.Add(time.Minute)
		return s
	}())
	d.put(func() driver.State {
		s := d.state(deletable.Status.ID)
		s.Phase, s.StoppedAt = driver.Stopped, epoch
		return s
	}())

	clock.Advance(11 * time.Minute)
	acted, err := c.Reap(t.Context())
	if err != nil || acted != 4 {
		t.Fatalf("acted on %d sandboxes: %v", acted, err)
	}
	stops, deletes, _ := d.acted()
	if !slices.Equal(deletes, []string{expired.Status.ID, deletable.Status.ID}) {
		t.Fatalf("deleted %v", deletes)
	}
	if !slices.Equal(stops, []string{idle.Status.ID, untracked.ID}) {
		t.Fatalf("stopped %v", stops)
	}
	if _, err = c.Get(t.Context(), expired.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired record kept: %v", err)
	}
	if _, err = c.Get(t.Context(), deletable.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("auto-deleted record kept: %v", err)
	}
	got, err := c.Get(t.Context(), idle.Status.ID, "alice")
	if err != nil || got.Status.Phase != driver.Stopped || got.Status.Reason != ReasonAutoStop {
		t.Fatalf("idle sandbox is %q/%q: %v", got.Status.Phase, got.Status.Reason, err)
	}
	// A second tick over a fleet that already matches nothing acts on nothing.
	if acted, err = c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("second tick acted on %d: %v", acted, err)
	}
}

func TestReaperRevalidatesBeforeActing(t *testing.T) {
	t.Run("activity", func(t *testing.T) {
		c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{AutoStop: 10 * time.Minute}})
		obj := created(t, c, "busy")
		clock.Advance(11 * time.Minute)
		d.set(func(d *fakeDriver) {
			d.onList = func() {
				s := d.state(obj.Status.ID)
				s.LastActivityAt = clock.Now()
				d.put(s)
			}
		})
		if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
			t.Fatalf("stopped a sandbox used since the list: %d %v", acted, err)
		}
		if stops, _, _ := d.acted(); len(stops) != 0 {
			t.Fatalf("stopped %v", stops)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{TTL: time.Hour}})
		obj := created(t, c, "retimed")
		clock.Advance(2 * time.Hour)
		d.set(func(d *fakeDriver) {
			d.onList = func() {
				s := d.state(obj.Status.ID)
				s.ExpiresAt = clock.Now().Add(time.Hour)
				d.put(s)
			}
		})
		if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
			t.Fatalf("deleted a sandbox retimed since the list: %d %v", acted, err)
		}
		if _, deletes, _ := d.acted(); len(deletes) != 0 {
			t.Fatalf("deleted %v", deletes)
		}
	})
	t.Run("gone", func(t *testing.T) {
		c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{TTL: time.Hour}})
		obj := created(t, c, "gone")
		clock.Advance(2 * time.Hour)
		d.set(func(d *fakeDriver) {
			d.onList = func() {
				d.mu.Lock()
				defer d.mu.Unlock()
				delete(d.states, obj.Status.ID)
			}
		})
		if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
			t.Fatalf("acted on a sandbox the driver no longer has: %d %v", acted, err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{TTL: time.Hour}})
		created(t, c, "unreadable")
		clock.Advance(2 * time.Hour)
		d.set(func(d *fakeDriver) { d.inspectErr = errors.New("inspection failed") })
		if acted, err := c.Reap(t.Context()); err == nil || acted != 0 {
			t.Fatalf("acted on an unreadable sandbox: %d %v", acted, err)
		}
	})
}

func TestListErrorIsNotEmpty(t *testing.T) {
	c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{TTL: time.Minute}})
	obj := created(t, c, "live")
	clock.Advance(time.Hour)
	d.set(func(d *fakeDriver) { d.listErr = errors.New("environment unreachable") })
	acted, err := c.Reap(t.Context())
	if err == nil || acted != 0 {
		t.Fatalf("an errored list acted on %d sandboxes: %v", acted, err)
	}
	if _, deletes, _ := d.acted(); len(deletes) != 0 {
		t.Fatalf("an errored list deleted %v", deletes)
	}
	if got, err := c.Get(t.Context(), obj.Status.ID, "alice"); err != nil || got.Status.Phase == "Deleting" {
		t.Fatalf("an errored list changed the record: %+v %v", got.Status, err)
	}
}

func TestReaperDriverFailuresKeepTheIntent(t *testing.T) {
	c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{TTL: time.Minute, AutoStop: time.Minute}})
	expired := created(t, c, "expired")
	clock.Advance(time.Hour)
	d.set(func(d *fakeDriver) { d.deleteErr = errors.New("runtime outage") })
	if acted, err := c.Reap(t.Context()); err == nil || acted != 0 {
		t.Fatalf("a failed delete reported %d and %v", acted, err)
	}
	got, err := c.Get(t.Context(), expired.Status.ID, "alice")
	if err != nil || got.Status.Phase != "Deleting" || got.Status.Reason != ReasonExpired {
		t.Fatalf("the delete intent is %q/%q: %v", got.Status.Phase, got.Status.Reason, err)
	}
	// A stop that the driver refuses is reported and leaves the record alone.
	c2, d2, clock2 := newFake(t, Options{Lifecycle: driver.Lifecycle{AutoStop: time.Minute}})
	idle := created(t, c2, "idle")
	clock2.Advance(time.Hour)
	d2.set(func(d *fakeDriver) { d.stopErr = errors.New("runtime outage") })
	if acted, err := c2.Reap(t.Context()); err == nil || acted != 0 {
		t.Fatalf("a failed stop reported %d and %v", acted, err)
	}
	if got, err = c2.Get(t.Context(), idle.Status.ID, "alice"); err != nil || got.Status.Phase != driver.Running {
		t.Fatalf("a failed stop changed the record to %q: %v", got.Status.Phase, err)
	}
}

func TestReaperStoreFailuresKeepTheRecord(t *testing.T) {
	// A create writes three times, the row, the status before the driver
	// call and the settle, so the reaper's intent and its removal are the
	// fourth and the fifth.
	for _, failOn := range []int{4, 5} {
		store := &memoryStore{saveErr: errors.New("write failed"), failOn: failOn}
		c, _, clock := newFake(t, Options{Store: store, Lifecycle: driver.Lifecycle{TTL: time.Minute}})
		obj := created(t, c, "expired")
		clock.Advance(time.Hour)
		if acted, err := c.Reap(t.Context()); err == nil || acted != 0 {
			t.Fatalf("a failed save reported %d and %v", acted, err)
		}
		if _, err := c.Get(t.Context(), obj.Status.ID, "alice"); err != nil {
			t.Fatalf("a failed save lost the record: %v", err)
		}
	}
}

func TestReaperNeedsTheLease(t *testing.T) {
	lease := &fakeLease{}
	c, d, clock := newFake(t, Options{Lease: lease, Lifecycle: driver.Lifecycle{AutoStop: time.Minute}})
	created(t, c, "idle")
	clock.Advance(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunReaper(ctx) }()
	defer func() { cancel(); <-done }()

	for _, tc := range []struct {
		name string
		held bool
		err  error
	}{
		{"notHeld", false, nil},
		{"unavailable", false, errors.New("lease store unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease.answer(tc.held, tc.err)
			asked := lease.asked()
			clock.ticks <- clock.Now()
			waitFor(t, "the lease to be asked", func() bool { return lease.asked() > asked })
			if stops, _, _ := d.acted(); len(stops) != 0 {
				t.Fatalf("a replica without the lease stopped %v", stops)
			}
		})
	}
	lease.answer(true, nil)
	clock.ticks <- clock.Now()
	waitFor(t, "the reaper to stop the idle sandbox", func() bool {
		stops, _, _ := d.acted()
		return len(stops) == 1
	})
	if lease.name != ReaperLease || lease.ttl != LeaseTTL {
		t.Fatalf("asked for lease %q with ttl %v", lease.name, lease.ttl)
	}
}

func TestRunReaperTicksAndStops(t *testing.T) {
	c, d, clock := newFake(t, Options{ReapInterval: time.Second, Lifecycle: driver.Lifecycle{AutoStop: time.Minute}})
	first := created(t, c, "first")
	second := created(t, c, "second")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunReaper(ctx) }()

	clock.Advance(time.Minute)
	clock.ticks <- clock.Now()
	waitFor(t, "both sandboxes to be stopped", func() bool {
		stops, _, _ := d.acted()
		return len(stops) == 2
	})
	stops, _, _ := d.acted()
	if !slices.Equal(stops, []string{first.Status.ID, second.Status.ID}) {
		t.Fatalf("stopped %v", stops)
	}
	// A tick over a fleet with nothing to do is not an error and the loop
	// keeps running; the listing error is what a later tick reports.
	d.set(func(d *fakeDriver) { d.listErr = errors.New("environment unreachable") })
	clock.ticks <- clock.Now()
	waitFor(t, "the loop to survive a failed tick", func() bool {
		select {
		case clock.ticks <- clock.Now():
			return true
		case <-time.After(5 * time.Millisecond):
			return false
		}
	})
	cancel()
	<-done
	if !clock.tickerStopped() {
		t.Fatal("the loop returned without stopping its ticker")
	}
}

func TestTouchCoalesces(t *testing.T) {
	c, d, clock := newFake(t, Options{TouchInterval: time.Minute})
	first := created(t, c, "first").Status.ID
	second := created(t, c, "second").Status.ID
	touches := func() []string {
		_, _, touches := d.acted()
		return touches
	}
	for range 3 {
		if err := c.Touch(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	}
	if got := touches(); !slices.Equal(got, []string{first}) {
		t.Fatalf("a burst inside the interval reached the driver as %v", got)
	}
	if err := c.Touch(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if got := touches(); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("each sandbox has its own window: %v", got)
	}
	clock.Advance(time.Minute)
	if err := c.Touch(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if got := touches(); !slices.Equal(got, []string{first, second, first}) {
		t.Fatalf("the window did not open at the interval: %v", got)
	}
	clock.Advance(time.Minute)
	d.set(func(d *fakeDriver) { d.touchErr = errors.New("runtime outage") })
	if err := c.Touch(t.Context(), first); err == nil {
		t.Fatal("a failed touch was swallowed")
	}
	if _, err := c.Act(t.Context(), first, "delete"); err != nil {
		t.Fatal(err)
	}
	c.touchMu.Lock()
	defer c.touchMu.Unlock()
	if _, ok := c.touched[first]; ok {
		t.Fatal("a deleted sandbox kept its touch window")
	}
	if _, ok := c.touched[second]; !ok {
		t.Fatal("a live sandbox lost its touch window")
	}
}

func TestRefreshKeepsTheControllerReason(t *testing.T) {
	c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{AutoStop: time.Minute}})
	obj := created(t, c, "idle")
	clock.Advance(time.Hour)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("acted on %d: %v", acted, err)
	}
	got, err := c.Get(t.Context(), obj.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got, err = c.Refresh(t.Context(), got); err != nil || got.Status.Reason != ReasonAutoStop {
		t.Fatalf("a refresh erased the reason: %q %v", got.Status.Reason, err)
	}
	// The reason goes when the phase it was written for does.
	s := d.state(obj.Status.ID)
	s.Phase, s.StoppedAt = driver.Running, time.Time{}
	d.put(s)
	if got, err = c.Refresh(t.Context(), got); err != nil || got.Status.Reason != "" || got.Status.Phase != driver.Running {
		t.Fatalf("a restarted sandbox kept %q/%q: %v", got.Status.Phase, got.Status.Reason, err)
	}
}

// TestReaperEndToEndOverNative drives the loop over the native driver with no
// fakes: a sandbox goes idle, is stopped with reason AutoStop, and is deleted
// once it has been stopped for longer than its autoDelete.
//
// The stopped state lasts one autoDelete, after which the next rule ends the
// record, so a probe that arrives later than that never sees it. The test
// therefore waits only for the record to be gone, which stays true once
// reached, and reads the transitions from the acts the controller emitted,
// which keep every one of them whatever the scheduling. Its first read comes
// after the delete on every run.
func TestReaperEndToEndOverNative(t *testing.T) {
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	acts := &actRecorder{}
	c, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), Driver: d, Environment: "default", Events: acts,
		ReapInterval: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler),
		Lifecycle: driver.Lifecycle{AutoStop: 40 * time.Millisecond, AutoDelete: 40 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	obj, err := realized(t.Context(), c, workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunReaper(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the idle sandbox to be stopped and then deleted", func() bool {
		_, err := c.Get(t.Context(), obj.Status.ID, "alice")
		return errors.Is(err, ErrNotFound)
	})
	stopped, deleting := -1, -1
	for i, a := range acts.all() {
		if a.Object.Status.ID != obj.Status.ID {
			continue
		}
		switch {
		case a.Type == MutationStopped && stopped < 0:
			stopped = i
			if a.Object.Status.Reason != ReasonAutoStop {
				t.Errorf("the sandbox was stopped with reason %q, want %s", a.Object.Status.Reason, ReasonAutoStop)
			}
			if a.Object.Status.Phase != driver.Stopped {
				t.Errorf("the stop recorded phase %s, want %s", a.Object.Status.Phase, driver.Stopped)
			}
		case a.Type == MutationDeleting && deleting < 0:
			deleting = i
			if a.Object.Status.Reason != ReasonAutoDelete {
				t.Errorf("the sandbox was deleted with reason %q, want %s", a.Object.Status.Reason, ReasonAutoDelete)
			}
		}
	}
	if stopped < 0 || deleting < 0 || stopped > deleting {
		t.Fatalf("the acts were %v, want the stop before the delete", acts.types())
	}
	if _, err = d.Inspect(t.Context(), obj.Status.ID); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the workspace outlived its record: %v", err)
	}
}
