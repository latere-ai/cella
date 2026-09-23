// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// scheduled is a controller over the fake driver whose own environment runs
// in the mode given with the capacity given. Its clock starts at the wall
// clock, because a sandbox's createdAt is the wall clock's and a start
// deadline is read against the controller's.
func scheduled(t *testing.T, mode string, capacity v1.Capacity, o Options) (*Controller, *fakeDriver, *fakeClock) {
	t.Helper()
	o.SchedulingMode = mode
	o.Capacity, o.CapacityQuantities = capacity.Sandboxes, capacity
	clock := &fakeClock{now: time.Now().UTC(), ticks: make(chan time.Time)}
	return newFakeOver(t, o, newDriver(clock), clock)
}

// asking is a manifest as Resolve hands it on: the queue defaulted on a
// queued environment and the resources filled.
func asking(name, owner, cpu string, priority int) (v1.Sandbox, string) {
	obj := workspace()
	obj.Metadata.Name = name
	obj.Spec.Resources = v1.Resources{CPU: v1.Quantity(cpu), Memory: "1Gi", Disk: "1Gi"}
	obj.Spec.Scheduling = v1.Scheduling{Queue: "default", Priority: priority}
	return obj, owner
}

func mustCreate(t *testing.T, c *Controller, obj v1.Sandbox, owner string) v1.Sandbox {
	t.Helper()
	got, err := c.Create(t.Context(), obj, owner, 0)
	if err != nil {
		t.Fatalf("creating %s: %v", obj.Metadata.Name, err)
	}
	return got
}

func sandboxPhase(t *testing.T, c *Controller, id string) (string, string) {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "")
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return obj.Status.Phase, obj.Status.Reason
}

func deleteSandbox(t *testing.T, c *Controller, id string) {
	t.Helper()
	if _, err := c.Act(t.Context(), id, "delete"); err != nil {
		t.Fatalf("deleting %s: %v", id, err)
	}
}

func conditionScheduled(obj v1.Sandbox) v1.Condition {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionScheduled {
			return cond
		}
	}
	return v1.Condition{}
}

// issued is every sandbox a token was minted for, read under the fake's lock.
func (f *fakeTokens) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sandbox)
}

// named is the lease the last ask named, read under the fake's lock.
func (l *fakeLease) named() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.name
}

// TestCapacityInUse is spec 057's derivation: cpu, memory and the count are
// held while the driver runs a sandbox, disk while its workspace stays, a
// quantity the environment does not declare bounds nothing, and auto bounds
// nothing either.
func TestCapacityInUse(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingDirect, v1.Capacity{CPU: "2", Disk: "3Gi"}, Options{})
	obj, owner := asking("a", "alice", "1", 0)
	obj.Spec.Resources.Memory = "900Ti" // undeclared, so it bounds nothing
	a := mustCreate(t, c, obj, owner)
	c.mu.Lock()
	used, err := c.usageOn("default")
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.cpu != 1000 || used.sandboxes != 1 || used.disk != 1<<30*1000 {
		t.Fatalf("a running sandbox holds %+v", used)
	}
	if _, err := c.Act(t.Context(), a.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	used, err = c.usageOn("default")
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.cpu != 0 || used.sandboxes != 0 || used.disk != 1<<30*1000 {
		t.Fatalf("a stopped sandbox holds %+v, want its disk alone", used)
	}
	// Disk is what is short now: two more would hold three disks with the
	// stopped one's, and the third would not fit.
	b := mustCreate(t, c, func() v1.Sandbox { o, _ := asking("b", "alice", "1", 0); return o }(), "alice")
	d := mustCreate(t, c, func() v1.Sandbox { o, _ := asking("d", "alice", "1", 0); return o }(), "alice")
	e := mustCreate(t, c, func() v1.Sandbox { o, _ := asking("e", "alice", "0", 0); return o }(), "alice")
	for _, got := range []v1.Sandbox{b, d} {
		if got.Status.Phase != driver.Running {
			t.Fatalf("%s is %s, want Running", got.Metadata.Name, got.Status.Phase)
		}
	}
	if e.Status.Phase != PhaseFailed || e.Status.Reason != ReasonNoCapacity {
		t.Fatalf("a fourth disk on three gibibytes is %s %s", e.Status.Phase, e.Status.Reason)
	}

	auto, _, _ := scheduled(t, v1.SchedulingDirect, v1.Capacity{Auto: true}, Options{})
	big, owner := asking("big", "alice", "900", 0)
	if got := mustCreate(t, auto, big, owner); got.Status.Phase != driver.Running {
		t.Fatalf("auto bounded a create: %s %s", got.Status.Phase, got.Status.Reason)
	}
}

// TestCapacitySurvivesARestart: in use is read from desired state, so a
// control plane that restarts holds the sum the last one held.
func TestCapacitySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	c, d, clock := scheduled(t, v1.SchedulingDirect, v1.Capacity{CPU: "3"}, Options{DataDir: dir})
	for _, name := range []string{"a", "b"} {
		obj, owner := asking(name, "alice", "1", 0)
		mustCreate(t, c, obj, owner)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again, _, _ := newFakeOver(t, Options{DataDir: dir, SchedulingMode: v1.SchedulingDirect, CapacityQuantities: v1.Capacity{CPU: "3"}}, d, clock)
	again.mu.Lock()
	used, err := again.usageOn("default")
	again.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.cpu != 2000 || used.sandboxes != 2 {
		t.Fatalf("after a restart the environment holds %+v, want two sandboxes of one cpu", used)
	}
	third, owner := asking("c", "alice", "2", 0)
	if got := mustCreate(t, again, third, owner); got.Status.Reason != ReasonNoCapacity {
		t.Fatalf("two cpu on the one left is %s %s", got.Status.Phase, got.Status.Reason)
	}
}

// TestDirectFailsWhatDoesNotFit: a direct environment starts a sandbox now or
// fails it, by any quantity it declares and by the count, and a failed one
// pushed no boundary, minted no token and asked no driver.
func TestDirectFailsWhatDoesNotFit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity v1.Capacity
		first    v1.Resources
		second   v1.Resources
	}{
		{"cpu", v1.Capacity{CPU: "1"}, v1.Resources{CPU: "1"}, v1.Resources{CPU: "500m"}},
		{"memory", v1.Capacity{Memory: "2Gi"}, v1.Resources{Memory: "1Gi"}, v1.Resources{Memory: "1536Mi"}},
		{"disk", v1.Capacity{Disk: "10Gi"}, v1.Resources{Disk: "6Gi"}, v1.Resources{Disk: "6Gi"}},
		{"the count", v1.Capacity{Sandboxes: 1}, v1.Resources{}, v1.Resources{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Now().UTC(), ticks: make(chan time.Time)}
			tokens := newTokens(clock, time.Hour)
			c, d, _ := scheduled(t, v1.SchedulingDirect, tc.capacity, Options{Tokens: tokens})
			first := workspace()
			first.Metadata.Name, first.Spec.Resources = "first", tc.first
			if got := mustCreate(t, c, first, "alice"); got.Status.Phase != driver.Running {
				t.Fatalf("the first create is %s %s", got.Status.Phase, got.Status.Reason)
			}
			second := workspace()
			second.Metadata.Name, second.Spec.Resources = "second", tc.second
			got := mustCreate(t, c, second, "alice")
			if got.Status.Phase != PhaseFailed || got.Status.Reason != ReasonNoCapacity {
				t.Fatalf("a create past the %s is %s %s", tc.name, got.Status.Phase, got.Status.Reason)
			}
			if cond := conditionScheduled(got); cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonNoCapacity {
				t.Fatalf("the Scheduled condition is %+v", cond)
			}
			d.mu.Lock()
			created := len(d.specs)
			d.mu.Unlock()
			if created != 1 {
				t.Fatalf("the driver was asked for %d sandboxes, want the first alone", created)
			}
			if minted := len(tokens.issued()); minted != 1 {
				t.Fatalf("%d tokens were minted, want the first sandbox's alone", minted)
			}
			// The failed sandbox holds its name and nothing of the
			// environment, so it deletes and the name is free again.
			deleteSandbox(t, c, got.Status.ID)
		})
	}
}

// TestQueuedCreateWaits: a queued environment answers a create it cannot fit
// with the sandbox Queued, and the Scheduled condition says where in its
// queue it waits.
func TestQueuedCreateWaits(t *testing.T) {
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	obj, owner := asking("a", "alice", "1", 0)
	if got := mustCreate(t, c, obj, owner); got.Status.Phase != driver.Running {
		t.Fatalf("a create on an empty queue that fits is %s", got.Status.Phase)
	}
	var waiting []v1.Sandbox
	for _, name := range []string{"b", "c"} {
		obj, owner := asking(name, "bob", "1", 0)
		got := mustCreate(t, c, obj, owner)
		if got.Status.Phase != PhaseQueued {
			t.Fatalf("%s is %s, want Queued", name, got.Status.Phase)
		}
		waiting = append(waiting, got)
	}
	for i, want := range []string{"Position 1 of 2 in the queue default.", "Position 2 of 2 in the queue default."} {
		got, err := c.Get(t.Context(), waiting[i].Status.ID, "bob")
		if err != nil {
			t.Fatal(err)
		}
		if cond := conditionScheduled(got); cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonQueued || cond.Message != want {
			t.Fatalf("%s carries %+v, want %q", got.Metadata.Name, cond, want)
		}
	}
	d.mu.Lock()
	created := len(d.specs)
	d.mu.Unlock()
	if created != 1 {
		t.Fatalf("the driver was asked for %d sandboxes while two wait", created)
	}
	// A create that would fit does not pass the queue ahead of it.
	c.envMu.Lock()
	env := c.environments["default"]
	env.Spec.Capacity.Sandboxes = 5
	c.environments["default"] = env
	c.envMu.Unlock()
	late, owner := asking("late", "carol", "1", 0)
	if got := mustCreate(t, c, late, owner); got.Status.Phase != PhaseQueued {
		t.Fatalf("a create behind a waiting queue is %s", got.Status.Phase)
	}
}

// TestSchedulerLoop: the loop places only while it holds the scheduler lease,
// on its tick and on the wake a released sandbox sends.
func TestSchedulerLoop(t *testing.T) {
	lease := &fakeLease{}
	c, _, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{Lease: lease})
	a := func() v1.Sandbox { o, w := asking("a", "alice", "1", 0); return mustCreate(t, c, o, w) }()
	b := func() v1.Sandbox { o, w := asking("b", "alice", "1", 0); return mustCreate(t, c, o, w) }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunScheduler(ctx) }()
	defer func() { cancel(); <-done }()

	// Without the lease the delete's wake and a tick place nothing.
	deleteSandbox(t, c, a.Status.ID)
	clock.ticks <- clock.Now()
	waitFor(t, "the loop to ask for its lease", func() bool { return lease.asked() >= 1 })
	clock.ticks <- clock.Now()
	if phase, _ := sandboxPhase(t, c, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("a replica without the lease placed %s", phase)
	}
	// With it, the tick places the head.
	lease.answer(true, nil)
	clock.ticks <- clock.Now()
	waitFor(t, "the tick to place the head", func() bool { p, _ := sandboxPhase(t, c, b.Status.ID); return p == driver.Running })
	if name := lease.named(); name != SchedulerLease {
		t.Fatalf("the loop asked for the lease %q", name)
	}
	// A sandbox that gives its slot back wakes the loop, which places the
	// next without waiting for a tick.
	next := func() v1.Sandbox { o, w := asking("next", "alice", "1", 0); return mustCreate(t, c, o, w) }()
	deleteSandbox(t, c, b.Status.ID)
	waitFor(t, "the wake to place the next", func() bool { p, _ := sandboxPhase(t, c, next.Status.ID); return p == driver.Running })
}

// TestPlacementResumesAtTheBoundary: a queued sandbox has pushed no boundary
// and holds no token; the placement is what does both, at step 3 of the
// create order.
func TestPlacementResumesAtTheBoundary(t *testing.T) {
	clock := &fakeClock{now: time.Now().UTC(), ticks: make(chan time.Time)}
	tokens := newTokens(clock, time.Hour)
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{Tokens: tokens})
	a := func() v1.Sandbox { o, w := asking("a", "alice", "1", 0); return mustCreate(t, c, o, w) }()
	b := func() v1.Sandbox { o, w := asking("b", "alice", "1", 0); return mustCreate(t, c, o, w) }()
	if slices.Contains(tokens.issued(), b.Status.ID) {
		t.Fatal("a queued sandbox was minted a token")
	}
	if got, _ := c.Get(t.Context(), b.Status.ID, "alice"); got.Status.TokenState != nil {
		t.Fatal("a queued sandbox holds a token state")
	}
	deleteSandbox(t, c, a.Status.ID)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tokens.issued(), b.Status.ID) {
		t.Fatal("the placement minted no token")
	}
	got, err := c.Get(t.Context(), b.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if cond := conditionScheduled(got); got.Status.Phase != driver.Running || cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonPlaced {
		t.Fatalf("the placed sandbox is %s with %+v", got.Status.Phase, cond)
	}
}

// TestSchedulerHonorsQueueOrder is spec 020's order across three subjects
// and two priorities: the higher priority first, then the subject holding the
// least cpu on the environment, then arrival.
func TestSchedulerHonorsQueueOrder(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 2}, Options{})
	// alice holds two cpu for the whole case, so she is the subject with
	// the largest share whenever priorities tie.
	holder, owner := asking("holder", "alice", "2", 0)
	mustCreate(t, c, holder, owner)
	filler, owner := asking("filler", "dave", "0", 0)
	running := mustCreate(t, c, filler, owner).Status.ID
	ids := map[string]string{}
	for _, q := range []struct {
		name, owner string
		priority    int
	}{
		{"q1", "alice", 0}, {"q2", "bob", 0}, {"q3", "carol", 1}, {"q4", "bob", 0}, {"q5", "alice", 1},
	} {
		obj, _ := asking(q.name, q.owner, "1", q.priority)
		got := mustCreate(t, c, obj, q.owner)
		if got.Status.Phase != PhaseQueued {
			t.Fatalf("%s is %s", q.name, got.Status.Phase)
		}
		ids[got.Status.ID] = q.name
		// Arrival is createdAt, which is the wall clock at the create.
		time.Sleep(2 * time.Millisecond)
	}
	var order []string
	for range 5 {
		deleteSandbox(t, c, running)
		if err := c.Schedule(t.Context()); err != nil {
			t.Fatal(err)
		}
		var placed []string
		for _, obj := range c.List() {
			if name, queued := ids[obj.Status.ID]; queued && obj.Status.Phase == driver.Running {
				placed = append(placed, name)
				running = obj.Status.ID
				delete(ids, obj.Status.ID)
			}
		}
		if len(placed) != 1 {
			t.Fatalf("one slot placed %v", placed)
		}
		order = append(order, placed[0])
	}
	if want := []string{"q3", "q5", "q2", "q4", "q1"}; !slices.Equal(order, want) {
		t.Fatalf("the queue was placed in the order %v, want %v", order, want)
	}
}

// TestAHeadThatDoesNotFitBlocks: the first head that does not fit ends its
// queue's turn, so a large request is not passed by a small one behind it.
func TestAHeadThatDoesNotFitBlocks(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{CPU: "2"}, Options{})
	held, owner := asking("held", "alice", "1", 0)
	a := mustCreate(t, c, held, owner)
	large, owner := asking("large", "bob", "2", 0)
	big := mustCreate(t, c, large, owner)
	small, owner := asking("small", "bob", "1", 0)
	little := mustCreate(t, c, small, owner)
	if big.Status.Phase != PhaseQueued || little.Status.Phase != PhaseQueued {
		t.Fatalf("the two creates are %s and %s", big.Status.Phase, little.Status.Phase)
	}
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, c, little.Status.ID); phase != PhaseQueued {
		t.Fatalf("the small request passed the large head: %s", phase)
	}
	deleteSandbox(t, c, a.Status.ID)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, c, big.Status.ID); phase != driver.Running {
		t.Fatalf("the large head is %s once it fits", phase)
	}
	if phase, _ := sandboxPhase(t, c, little.Status.ID); phase != PhaseQueued {
		t.Fatalf("the small request is %s with no cpu left", phase)
	}
}

// TestStartDeadline: a sandbox that waits past its start deadline fails with
// the reason, and one with no deadline keeps waiting.
func TestStartDeadline(t *testing.T) {
	c, _, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	a, owner := asking("a", "alice", "1", 0)
	mustCreate(t, c, a, owner)
	bounded, owner := asking("bounded", "alice", "1", 0)
	bounded.Spec.Scheduling.StartDeadline = "10m"
	b := mustCreate(t, c, bounded, owner)
	patient, owner := asking("patient", "alice", "1", 0)
	p := mustCreate(t, c, patient, owner)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, c, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("a sandbox inside its deadline is %s", phase)
	}
	clock.Advance(11 * time.Minute)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, reason := sandboxPhase(t, c, b.Status.ID); phase != PhaseFailed || reason != ReasonStartDeadline {
		t.Fatalf("a sandbox past its deadline is %s %s", phase, reason)
	}
	if phase, _ := sandboxPhase(t, c, p.Status.ID); phase != PhaseQueued {
		t.Fatalf("a sandbox with no deadline is %s", phase)
	}
}

// TestAQueuedSandboxIsDesiredStateOnly: no driver holds a queued sandbox, so
// a read does not ask one, the lost rule and the reaper pass over it, the
// verbs that need a driver are phase_conflict, and a delete ends it.
func TestAQueuedSandboxIsDesiredStateOnly(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	a, owner := asking("a", "alice", "1", 0)
	mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)
	if got, err := c.Refresh(t.Context(), b); err != nil || got.Status.Phase != PhaseQueued {
		t.Fatalf("a read of a queued sandbox is %s, %v", got.Status.Phase, err)
	}
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, c, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("the reaper moved a queued sandbox to %s", phase)
	}
	for _, verb := range []string{"start", "stop"} {
		if _, err := c.Act(t.Context(), b.Status.ID, verb); !errors.Is(err, ErrPhase) {
			t.Fatalf("%s on a queued sandbox is %v", verb, err)
		}
	}
	if _, err := c.Exec(t.Context(), b.Status.ID, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, ErrPhase) {
		t.Fatalf("exec on a queued sandbox is %v", err)
	}
	deleteSandbox(t, c, b.Status.ID)
	if _, err := c.Get(t.Context(), b.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted queued sandbox reads %v", err)
	}
}

// TestTheQueueSurvivesARestart: the queue is desired state, so a control plane
// that restarts holds what waited and places it.
func TestTheQueueSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	c, d, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{DataDir: dir})
	a, owner := asking("a", "alice", "1", 0)
	first := mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again, _, _ := newFakeOver(t, Options{DataDir: dir, SchedulingMode: v1.SchedulingQueued, Capacity: 1, Log: slog.New(slog.DiscardHandler)}, d, clock)
	if phase, _ := sandboxPhase(t, again, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("after a restart the queued sandbox is %s", phase)
	}
	deleteSandbox(t, again, first.Status.ID)
	if err := again.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, again, b.Status.ID); phase != driver.Running {
		t.Fatalf("the restarted loop left the head %s", phase)
	}
}

// TestPoolYieldsCapacityByResource: an entry holds the pool's resources, so a
// create that needs the cpu entries hold takes the oldest of them, and one
// that would not fit with every entry gone deletes none.
func TestPoolYieldsCapacityByResource(t *testing.T) {
	o := poolOptions(2)
	o.Pool.Resources = v1.Resources{CPU: "1"}
	o.CapacityQuantities = v1.Capacity{CPU: "2"}
	c, d, clock := newPool(t, o)
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := d.entries()
	if len(entries) != 2 {
		t.Fatalf("the pool holds %d entries, want two", len(entries))
	}
	clock.Advance(time.Minute)
	large, owner := asking("large", "alice", "3", 0)
	large.Spec.Command = []string{"sh"}
	if got := mustCreate(t, c, large, owner); got.Status.Reason != ReasonNoCapacity {
		t.Fatalf("three cpu on two is %s %s", got.Status.Phase, got.Status.Reason)
	}
	if _, deletes, _ := d.acted(); len(deletes) != 0 {
		t.Fatalf("a create that cannot fit deleted %v", deletes)
	}
	obj, owner := asking("one", "alice", "1", 0)
	obj.Spec.Command = []string{"sh"}
	if got := mustCreate(t, c, obj, owner); got.Status.Phase != driver.Running {
		t.Fatalf("one cpu beside two entries of one is %s %s", got.Status.Phase, got.Status.Reason)
	}
	if _, deletes, _ := d.acted(); !slices.Equal(deletes, []string{entries[0].ID}) {
		t.Fatalf("the create deleted %v, want the oldest entry alone", deletes)
	}
}

// TestAFailedPlacementIsRecorded: the caller of a queued create already holds
// the object, so a placement the driver refuses is written Failed rather than
// taken back, and the loop goes on to the next head.
func TestAFailedPlacementIsRecorded(t *testing.T) {
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	a, owner := asking("a", "alice", "1", 0)
	first := mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)
	deleteSandbox(t, c, first.Status.ID)
	d.set(func(d *fakeDriver) { d.createErr = errors.New("the engine is down") })
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a placement the driver refused reported nothing")
	}
	if phase, reason := sandboxPhase(t, c, b.Status.ID); phase != PhaseFailed || reason != ReasonCreateFailed {
		t.Fatalf("the refused placement is %s %s", phase, reason)
	}
}

// TestSchedulerHoldsWhatItCannotPlace: the loop places nothing on an
// environment below Ready, and a replica whose lease cannot be read places
// nothing either; a deadline passes in both.
func TestSchedulerHoldsWhatItCannotPlace(t *testing.T) {
	lease := &fakeLease{}
	lease.answer(false, errors.New("the lease table is unavailable"))
	c, _, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{Lease: lease})
	a, owner := asking("a", "alice", "1", 0)
	first := mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)
	bounded, owner := asking("bounded", "alice", "1", 0)
	bounded.Spec.Scheduling.StartDeadline = "1m"
	late := mustCreate(t, c, bounded, owner)
	deleteSandbox(t, c, first.Status.ID)

	c.scheduleTick(t.Context())
	if phase, _ := sandboxPhase(t, c, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("a replica that could not read its lease placed %s", phase)
	}

	c.envMu.Lock()
	env := c.environments["default"]
	env.Status.Phase = v1.EnvironmentOffline
	c.environments["default"] = env
	c.envMu.Unlock()
	clock.Advance(2 * time.Minute)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase, _ := sandboxPhase(t, c, b.Status.ID); phase != PhaseQueued {
		t.Fatalf("an offline environment placed %s", phase)
	}
	if phase, reason := sandboxPhase(t, c, late.Status.ID); phase != PhaseFailed || reason != ReasonStartDeadline {
		t.Fatalf("a deadline on an offline environment left the sandbox %s %s", phase, reason)
	}
}

// TestADeadlineIsReadAsResolveLeftIt: a deadline Resolve would refuse is read
// as none rather than as one already passed, and a spawned sandbox names the
// parent it was debited from while the parent exists.
func TestADeadlineIsReadAsResolveLeftIt(t *testing.T) {
	now := time.Now()
	for _, deadline := range []v1.Duration{"", "soon", v1.DurationNever} {
		obj := v1.Sandbox{Spec: v1.SandboxSpec{Scheduling: v1.Scheduling{StartDeadline: deadline}},
			Status: v1.SandboxStatus{CreatedAt: now.Add(-time.Hour)}}
		if expired(obj, now) {
			t.Errorf("the deadline %q expired", deadline)
		}
	}
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{}, Options{})
	parent := mustCreate(t, c, func() v1.Sandbox { o, _ := asking("parent", "alice", "1", 0); return o }(), "alice")
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := c.parentOf(v1.Sandbox{Status: v1.SandboxStatus{Parent: parent.Status.ID}}); got == nil || got.Status.ID != parent.Status.ID {
		t.Fatalf("the parent read back as %v", got)
	}
	for _, missing := range []string{"", "sbx_gone"} {
		if got := c.parentOf(v1.Sandbox{Status: v1.SandboxStatus{Parent: missing}}); got != nil {
			t.Fatalf("the parent %q read back as %s", missing, got.Status.ID)
		}
	}
}

// TestAQuantityThatDoesNotParseStopsThePlacement: desired state and the
// environment carry quantities Resolve already read, so one that does not
// parse is a store that was written by something else. The placement reports
// it rather than reading it as zero, which would place past the ceiling.
func TestAQuantityThatDoesNotParseStopsThePlacement(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	a, owner := asking("a", "alice", "1", 0)
	running := mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)

	c.mu.Lock()
	held := c.objects[running.Status.ID]
	broken := clone(held)
	broken.Spec.Resources.CPU = "lots"
	c.objects[running.Status.ID] = broken
	c.mu.Unlock()
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a queue holding a quantity that does not parse was placed")
	}
	if got, err := c.Get(t.Context(), b.Status.ID, "alice"); err != nil || conditionScheduled(got).Message != "" {
		t.Fatalf("a position was computed over a broken queue: %v, %v", conditionScheduled(got), err)
	}
	next, owner := asking("c", "alice", "1", 0)
	next.Spec.Scheduling.Queue = "other"
	if _, err := c.Create(t.Context(), next, owner, 0); err == nil {
		t.Fatal("a create was placed over a sum that does not parse")
	}
	c.mu.Lock()
	c.objects[running.Status.ID] = held
	c.mu.Unlock()

	c.envMu.Lock()
	env := c.environments["default"]
	env.Spec.Capacity.CPU = "plenty"
	c.environments["default"] = env
	c.envMu.Unlock()
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a capacity that does not parse was read")
	}
	c.envMu.Lock()
	env.Spec.Capacity.CPU = ""
	env.Spec.Pool.Resources.CPU = "some"
	c.environments["default"] = env
	c.envMu.Unlock()
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a pool shape that does not parse was read")
	}
}

// TestSchedulerMetrics: the two gauges read the queues and the capacity from
// desired state, an empty declared queue reads zero, and a quantity the
// environment does not declare has no figure.
func TestSchedulerMetrics(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{CPU: "2", Sandboxes: 1}, Options{})
	c.envMu.Lock()
	env := c.environments["default"]
	env.Spec.Scheduling.Queues = []string{"default", "batch"}
	c.environments["default"] = env
	c.envMu.Unlock()
	a, owner := asking("a", "alice", "1500m", 0)
	mustCreate(t, c, a, owner)
	for _, name := range []string{"b", "c"} {
		obj, owner := asking(name, "alice", "1", 0)
		mustCreate(t, c, obj, owner)
	}
	depths := c.QueueDepths()
	want := []QueueDepth{{"default", "batch", 0}, {"default", "default", 2}}
	if !slices.Equal(depths, want) {
		t.Fatalf("the queues read %+v, want %+v", depths, want)
	}
	got := c.CapacityFigures(t.Context())
	wantFigures := []CapacityFigure{
		{Environment: "default", Resource: "cpu", Declared: 2, Used: 1.5},
		{Environment: "default", Resource: "sandboxes", Declared: 1, Used: 1},
	}
	if !slices.Equal(got, wantFigures) {
		t.Fatalf("the capacity reads %+v, want %+v", got, wantFigures)
	}
	c.envMu.Lock()
	env.Spec.Capacity.Memory = "a lot"
	c.environments["default"] = env
	c.envMu.Unlock()
	if got := c.CapacityFigures(t.Context()); len(got) != 0 {
		t.Fatalf("a capacity that does not parse published %+v", got)
	}
}

// TestTheLoopPassesAtStart: a control plane that restarts with a sandbox
// waiting and room free places it on the loop's first pass, with no tick and
// no wake.
func TestTheLoopPassesAtStart(t *testing.T) {
	dir := t.TempDir()
	c, d, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{DataDir: dir})
	a, owner := asking("a", "alice", "1", 0)
	first := mustCreate(t, c, a, owner)
	obj, owner := asking("b", "alice", "1", 0)
	b := mustCreate(t, c, obj, owner)
	deleteSandbox(t, c, first.Status.ID)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again, _, _ := newFakeOver(t, Options{DataDir: dir, SchedulingMode: v1.SchedulingQueued, Capacity: 1}, d, clock)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); again.RunScheduler(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the first pass to place what waited", func() bool {
		p, _ := sandboxPhase(t, again, b.Status.ID)
		return p == driver.Running
	})
}
