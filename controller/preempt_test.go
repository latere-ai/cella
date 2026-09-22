// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Start is the fake's half of a resume: a stopped sandbox runs again, and one
// the driver no longer holds is ErrNotFound.
func (d *fakeDriver) Start(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.startErr != nil {
		return d.startErr
	}
	s, ok := d.states[id]
	if !ok {
		return driver.ErrNotFound
	}
	s.Phase, s.StartedAt, s.StoppedAt = driver.Running, d.clock.Now(), time.Time{}
	d.states[id] = s
	d.starts = append(d.starts, id)
	return nil
}

// started is every sandbox the fake was asked to start.
func (d *fakeDriver) started() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.starts)
}

// createdCount is how many sandboxes the fake was asked to create.
func (d *fakeDriver) createdCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.specs)
}

// preemptible is a running candidate for a victim: asking with the flag set.
func preemptible(name, cpu string, priority int) v1.Sandbox {
	obj, _ := asking(name, "alice", cpu, priority)
	obj.Spec.Scheduling.Preemptible = true
	return obj
}

// read is one sandbox as a caller reads it, failing the test where it cannot.
func read(t *testing.T, c *Controller, id string) v1.Sandbox {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "")
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return obj
}

// schedule runs one pass and fails the test on an error.
func schedule(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestPreemption is spec 020's choice of victims: among the running,
// preemptible sandboxes of lower priority, lowest priority first, then the
// largest cpu, then the newest, and only as many as the head needs. Each is
// stopped through its driver, written Queued with Scheduled Preempted, its
// count and its createdAt, keeps its disk counted, and is recorded as a stop
// with that reason.
func TestPreemption(t *testing.T) {
	rec, acts := &recorder{}, &actRecorder{}
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{CPU: "7"}, Options{Metrics: rec, Events: acts})
	running := map[string]v1.Sandbox{}
	for _, s := range []struct {
		name, cpu   string
		priority    int
		preemptible bool
	}{
		{"z", "2", 1, true},     // the largest of the lowest priority, and the oldest
		{"x", "1", 1, true},     // older than y
		{"y", "1", 1, true},     // the newest of the lowest priority
		{"w", "1", 2, true},     // a higher priority, so after every priority 1
		{"keep", "1", 0, false}, // the lowest priority, and not preemptible
		{"equal", "1", 5, true}, // the head's own priority is not below it
	} {
		obj := preemptible(s.name, s.cpu, s.priority)
		obj.Spec.Scheduling.Preemptible = s.preemptible
		got := mustCreate(t, c, obj, "alice")
		if got.Status.Phase != driver.Running {
			t.Fatalf("%s is %s, want Running", s.name, got.Status.Phase)
		}
		running[s.name] = got
		time.Sleep(2 * time.Millisecond)
	}
	obj, owner := asking("head", "bob", "3", 5)
	head := mustCreate(t, c, obj, owner)
	if head.Status.Phase != PhaseQueued {
		t.Fatalf("the head is %s before the pass", head.Status.Phase)
	}
	schedule(t, c)

	if phase, _ := sandboxPhase(t, c, head.Status.ID); phase != driver.Running {
		t.Fatalf("the head is %s after the pass", phase)
	}
	stops, deletes, _ := d.acted()
	if want := []string{running["z"].Status.ID, running["y"].Status.ID}; !slices.Equal(stops, want) {
		t.Fatalf("the pass stopped %v, want z then y", stops)
	}
	if len(deletes) != 0 {
		t.Fatalf("a preemption deleted %v", deletes)
	}
	for _, name := range []string{"z", "y"} {
		got := read(t, c, running[name].Status.ID)
		cond := conditionScheduled(got)
		switch {
		case got.Status.Phase != PhaseQueued || got.Status.Reason != ReasonPreempted:
			t.Errorf("%s is %s %s, want Queued Preempted", name, got.Status.Phase, got.Status.Reason)
		case cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonPreempted:
			t.Errorf("%s carries %+v", name, cond)
		case got.Status.Preemptions != 1:
			t.Errorf("%s was counted %d times", name, got.Status.Preemptions)
		case !got.Status.CreatedAt.Equal(running[name].Status.CreatedAt):
			t.Errorf("%s arrived at %v and now reads %v", name, running[name].Status.CreatedAt, got.Status.CreatedAt)
		case d.state(got.Status.ID).Phase != driver.Stopped:
			t.Errorf("the driver holds %s as %s", name, d.state(got.Status.ID).Phase)
		}
	}
	// z and y wait at one priority for one subject, so their place in line
	// is their arrival and z, the older, is first.
	if msg := conditionScheduled(read(t, c, running["z"].Status.ID)).Message; msg != "Position 1 of 2 in the queue default." {
		t.Errorf("the first victim reads its place as %q", msg)
	}
	for _, name := range []string{"x", "w", "keep", "equal"} {
		if phase, _ := sandboxPhase(t, c, running[name].Status.ID); phase != driver.Running {
			t.Errorf("%s is %s, want it left running", name, phase)
		}
	}
	// Each victim keeps its disk and gives back its cpu and its slot.
	c.mu.Lock()
	used, err := c.usageOn("default")
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.cpu != 7000 || used.sandboxes != 5 || used.disk != 7*(1<<30)*1000 {
		t.Fatalf("after the preemption the environment holds %+v", used)
	}
	if rec.preemptions() != 2 {
		t.Fatalf("the counter moved %d times, want once per victim", rec.preemptions())
	}
	var preempted []string
	for _, a := range acts.of(MutationStopped) {
		if a.Object.Status.Reason == ReasonPreempted && a.Object.Status.Phase == PhaseQueued {
			preempted = append(preempted, a.Object.Status.ID)
		}
	}
	if len(preempted) != 2 {
		t.Fatalf("the stops recorded with Preempted are %v", preempted)
	}
}

// TestPreemptionStopsOnlyWhatTheHeadNeeds: a head the whole list of
// candidates would not make fit stops nobody, and so does a head that is short
// of disk, which no stop gives back.
func TestPreemptionStopsOnlyWhatTheHeadNeeds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity v1.Capacity
		victim   v1.Resources
		head     v1.Resources
	}{
		{"more cpu than every candidate holds", v1.Capacity{CPU: "2"}, v1.Resources{CPU: "1"}, v1.Resources{CPU: "3"}},
		{"disk", v1.Capacity{Disk: "2Gi"}, v1.Resources{Disk: "2Gi"}, v1.Resources{Disk: "1Gi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, d, _ := scheduled(t, v1.SchedulingQueued, tc.capacity, Options{})
			victim := preemptible("victim", "", 0)
			victim.Spec.Resources = tc.victim
			v := mustCreate(t, c, victim, "alice")
			obj, owner := asking("head", "bob", "", 5)
			obj.Spec.Resources = tc.head
			head := mustCreate(t, c, obj, owner)
			schedule(t, c)
			if stops, _, _ := d.acted(); len(stops) != 0 {
				t.Fatalf("a head that cannot fit stopped %v", stops)
			}
			if phase, _ := sandboxPhase(t, c, v.Status.ID); phase != driver.Running {
				t.Fatalf("the candidate is %s", phase)
			}
			if phase, _ := sandboxPhase(t, c, head.Status.ID); phase != PhaseQueued {
				t.Fatalf("the head is %s", phase)
			}
		})
	}
}

// TestPreemptionIsBounded: after the bound a sandbox is no longer a victim,
// the bound defaults to three, and NoPreemptions makes nobody a victim.
func TestPreemptionIsBounded(t *testing.T) {
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{MaxPreemptions: 2})
	v := mustCreate(t, c, preemptible("victim", "1", 0), "alice")
	for round := 1; round <= 2; round++ {
		obj, owner := asking("head", "bob", "1", 5)
		head := mustCreate(t, c, obj, owner)
		schedule(t, c)
		if got := read(t, c, v.Status.ID); got.Status.Phase != PhaseQueued || got.Status.Preemptions != round {
			t.Fatalf("round %d: the victim is %s after %d preemptions", round, got.Status.Phase, got.Status.Preemptions)
		}
		deleteSandbox(t, c, head.Status.ID)
		schedule(t, c)
		if phase, _ := sandboxPhase(t, c, v.Status.ID); phase != driver.Running {
			t.Fatalf("round %d: the victim is %s once the head is gone", round, phase)
		}
	}
	obj, owner := asking("head", "bob", "1", 5)
	head := mustCreate(t, c, obj, owner)
	schedule(t, c)
	if phase, _ := sandboxPhase(t, c, head.Status.ID); phase != PhaseQueued {
		t.Fatalf("a head placed past the bound: %s", phase)
	}
	if stops, _, _ := d.acted(); len(stops) != 2 {
		t.Fatalf("a sandbox at its bound was stopped: %v", stops)
	}

	none, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{MaxPreemptions: NoPreemptions})
	mustCreate(t, none, preemptible("victim", "1", 0), "alice")
	obj, owner = asking("head", "bob", "1", 5)
	mustCreate(t, none, obj, owner)
	schedule(t, none)
	if stops, _, _ := d.acted(); len(stops) != 0 {
		t.Fatalf("NoPreemptions stopped %v", stops)
	}
	if def, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{}, Options{}); def.maxPreemptions != DefaultMaxPreemptions {
		t.Fatalf("the default bound is %d", def.maxPreemptions)
	}
}

// TestPreemptionSurvivesARestart: the count is status, so a control plane that
// restarts holds it, keeps the victim's disk counted, and does not stop a
// sandbox that reached its bound before the restart.
func TestPreemptionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	c, d, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{DataDir: dir, MaxPreemptions: 1})
	v := mustCreate(t, c, preemptible("victim", "1", 0), "alice")
	obj, owner := asking("head", "bob", "1", 5)
	head := mustCreate(t, c, obj, owner)
	schedule(t, c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again, _, _ := newFakeOver(t, Options{DataDir: dir, SchedulingMode: v1.SchedulingQueued, Capacity: 1, MaxPreemptions: 1}, d, clock)
	got := read(t, again, v.Status.ID)
	if got.Status.Phase != PhaseQueued || got.Status.Preemptions != 1 || got.Status.Reason != ReasonPreempted {
		t.Fatalf("after a restart the victim is %s %s with %d", got.Status.Phase, got.Status.Reason, got.Status.Preemptions)
	}
	again.mu.Lock()
	used, err := again.usageOn("default")
	again.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.disk != 2*(1<<30)*1000 {
		t.Fatalf("after a restart the disk in use is %d, want the head's and the victim's", used.disk)
	}
	deleteSandbox(t, again, head.Status.ID)
	schedule(t, again)
	obj, owner = asking("second", "bob", "1", 5)
	second := mustCreate(t, again, obj, owner)
	schedule(t, again)
	if phase, _ := sandboxPhase(t, again, second.Status.ID); phase != PhaseQueued {
		t.Fatalf("a sandbox at its bound before the restart was preempted after it: %s", phase)
	}
}

// TestARequeuedSandboxResumes: a victim is placed again by a start of the
// object its driver kept, with no create and no new token; one whose object is
// gone is Lost; a start the driver refuses leaves it waiting; and a delete
// removes its object.
func TestARequeuedSandboxResumes(t *testing.T) {
	clock := &fakeClock{now: time.Now().UTC(), ticks: make(chan time.Time)}
	tokens := newTokens(clock, time.Hour)
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{Tokens: tokens})
	preempt := func(name string) (victim, head v1.Sandbox) {
		t.Helper()
		victim = mustCreate(t, c, preemptible(name, "1", 0), "alice")
		obj, owner := asking(name+"-head", "bob", "1", 5)
		head = mustCreate(t, c, obj, owner)
		schedule(t, c)
		if got := read(t, c, victim.Status.ID); !requeued(got) {
			t.Fatalf("%s is %s after the pass", name, got.Status.Phase)
		}
		return victim, head
	}

	v, head := preempt("victim")
	creates, minted := d.createdCount(), len(tokens.issued())
	deleteSandbox(t, c, head.Status.ID)
	schedule(t, c)
	got := read(t, c, v.Status.ID)
	if cond := conditionScheduled(got); got.Status.Phase != driver.Running || got.Status.Reason != "" ||
		cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonPlaced {
		t.Fatalf("the resumed victim is %s %q with %+v", got.Status.Phase, got.Status.Reason, cond)
	}
	if !slices.Contains(d.started(), v.Status.ID) || d.createdCount() != creates || len(tokens.issued()) != minted {
		t.Fatalf("the resume started %v and created %d, minted %d", d.started(), d.createdCount()-creates, len(tokens.issued())-minted)
	}
	deleteSandbox(t, c, v.Status.ID)

	// A start the driver refuses leaves the sandbox waiting where it was.
	v, head = preempt("refused")
	deleteSandbox(t, c, head.Status.ID)
	d.set(func(d *fakeDriver) { d.startErr = errors.New("the engine is down") })
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a start the driver refused reported nothing")
	}
	if got := read(t, c, v.Status.ID); !requeued(got) {
		t.Fatalf("a refused start left the sandbox %s", got.Status.Phase)
	}
	d.set(func(d *fakeDriver) { d.startErr = nil })
	// A driver that cannot be read after the start is taken at its word.
	d.set(func(d *fakeDriver) { d.inspectErr = errors.New("the engine does not answer") })
	schedule(t, c)
	d.set(func(d *fakeDriver) { d.inspectErr = nil })
	if phase, _ := sandboxPhase(t, c, v.Status.ID); phase != driver.Running {
		t.Fatalf("a start that could not be read left the sandbox %s", phase)
	}
	deleteSandbox(t, c, v.Status.ID)

	// An object the driver lost while the sandbox waited is Lost.
	v, head = preempt("lost")
	d.set(func(d *fakeDriver) { delete(d.states, v.Status.ID) })
	deleteSandbox(t, c, head.Status.ID)
	schedule(t, c)
	if phase, reason := sandboxPhase(t, c, v.Status.ID); phase != PhaseLost || reason != ReasonLost {
		t.Fatalf("a victim whose object is gone is %s %s", phase, reason)
	}
	deleteSandbox(t, c, v.Status.ID)

	// A delete of a waiting victim removes the object its driver kept.
	v, _ = preempt("deleted")
	deleteSandbox(t, c, v.Status.ID)
	if _, deletes, _ := d.acted(); !slices.Contains(deletes, v.Status.ID) {
		t.Fatalf("the delete of a requeued sandbox left its object: %v", deletes)
	}
}

// TestPreemptionReadsThroughAFailingDriver: a victim the driver cannot stop
// ends the preemption and the head waits, and one the driver cannot read
// after its stop is still requeued.
func TestPreemptionReadsThroughAFailingDriver(t *testing.T) {
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	v := mustCreate(t, c, preemptible("victim", "1", 0), "alice")
	obj, owner := asking("head", "bob", "1", 5)
	head := mustCreate(t, c, obj, owner)
	d.set(func(d *fakeDriver) { d.stopErr = errors.New("the engine is down") })
	if err := c.Schedule(t.Context()); err == nil {
		t.Fatal("a stop the driver refused reported nothing")
	}
	if phase, _ := sandboxPhase(t, c, v.Status.ID); phase != driver.Running {
		t.Fatalf("a victim the driver could not stop is %s", phase)
	}
	if phase, _ := sandboxPhase(t, c, head.Status.ID); phase != PhaseQueued {
		t.Fatalf("the head is %s", phase)
	}
	// The read after the stop is asked of the victim alone, so the requeue
	// is driven on its own rather than through a pass whose placement reads
	// the same driver.
	d.set(func(d *fakeDriver) { d.stopErr, d.inspectErr = nil, errors.New("the engine does not answer") })
	c.mu.Lock()
	err := c.requeue(t.Context(), d, c.objects[v.Status.ID], head)
	c.mu.Unlock()
	d.set(func(d *fakeDriver) { d.inspectErr = nil })
	if err != nil {
		t.Fatalf("a requeue whose read failed reported %v", err)
	}
	if got := read(t, c, v.Status.ID); !requeued(got) {
		t.Fatalf("a victim that could not be read after its stop is %s", got.Status.Phase)
	}
	schedule(t, c)
	if phase, _ := sandboxPhase(t, c, head.Status.ID); phase != driver.Running {
		t.Fatalf("the head is %s once the victim waits", phase)
	}
}

// TestARequeuedSandboxKeepsItsDeadlines: a victim started once, so its start
// deadline no longer applies; it is waiting to run again, so autoDelete does
// not end it; and its ttl still does.
func TestARequeuedSandboxKeepsItsDeadlines(t *testing.T) {
	c, d, clock := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	obj := preemptible("victim", "1", 0)
	obj.Spec.Scheduling.StartDeadline = "1m"
	obj.Spec.Lifecycle = v1.Lifecycle{AutoDelete: "1m", TTL: "10m"}
	v := mustCreate(t, c, obj, "alice")
	head, owner := asking("head", "bob", "1", 5)
	mustCreate(t, c, head, owner)
	schedule(t, c)
	clock.Advance(2 * time.Minute)
	schedule(t, c)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, c, v.Status.ID); !requeued(got) {
		t.Fatalf("past its start deadline and its autoDelete the victim is %s %s", got.Status.Phase, got.Status.Reason)
	}
	if _, deletes, _ := d.acted(); slices.Contains(deletes, v.Status.ID) {
		t.Fatal("autoDelete ended a sandbox waiting to run again")
	}
	clock.Advance(10 * time.Minute)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(t.Context(), v.Status.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a victim past its ttl reads %v", err)
	}
}

// TestAPassNeverPreemptsWhatItPlaced: one pass reads an environment's queues
// as one line, so a low head of one queue is not placed only to be stopped
// for a higher head of another queue in the same pass.
func TestAPassNeverPreemptsWhatItPlaced(t *testing.T) {
	c, d, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{})
	c.envMu.Lock()
	env := c.environments["default"]
	env.Spec.Scheduling.Queues = []string{"a", "b"}
	c.environments["default"] = env
	c.envMu.Unlock()
	blocker, owner := asking("blocker", "alice", "1", 0)
	blocker.Spec.Scheduling.Queue = "a"
	b := mustCreate(t, c, blocker, owner)
	low := preemptible("low", "1", 1)
	low.Spec.Scheduling.Queue = "a"
	l := mustCreate(t, c, low, "alice")
	high, owner := asking("high", "bob", "1", 5)
	high.Spec.Scheduling.Queue = "b"
	h := mustCreate(t, c, high, owner)
	creates := d.createdCount()
	deleteSandbox(t, c, b.Status.ID)
	schedule(t, c)
	if phase, _ := sandboxPhase(t, c, h.Status.ID); phase != driver.Running {
		t.Fatalf("the higher head is %s", phase)
	}
	got := read(t, c, l.Status.ID)
	if got.Status.Phase != PhaseQueued || got.Status.Preemptions != 0 || d.createdCount() != creates+1 {
		t.Fatalf("the lower head is %s after %d preemptions and %d creates", got.Status.Phase, got.Status.Preemptions, d.createdCount()-creates)
	}
}

// TestAQueuedCreateWakesTheLoop: a create written Queued wakes the loop, so a
// head preemption can place is placed without a tick.
func TestAQueuedCreateWakesTheLoop(t *testing.T) {
	lease := &fakeLease{}
	lease.answer(true, nil)
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Sandboxes: 1}, Options{Lease: lease})
	v := mustCreate(t, c, preemptible("victim", "1", 0), "alice")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunScheduler(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the loop's first pass", func() bool { return lease.asked() >= 1 })
	obj, owner := asking("head", "bob", "1", 5)
	head := mustCreate(t, c, obj, owner)
	waitFor(t, "the wake to place the head", func() bool {
		p, _ := sandboxPhase(t, c, head.Status.ID)
		return p == driver.Running
	})
	if got := read(t, c, v.Status.ID); !requeued(got) {
		t.Fatalf("the victim is %s", got.Status.Phase)
	}
}

// TestARequeuedSandboxHoldsItsDiskOnce: a preempted sandbox holds its disk
// while it waits, so the fit that places it again asks for its cpu, memory and
// slot and not for that disk a second time. Counting it twice would keep a
// victim on a full disk from ever running again.
func TestARequeuedSandboxHoldsItsDiskOnce(t *testing.T) {
	c, _, _ := scheduled(t, v1.SchedulingQueued, v1.Capacity{Disk: "1Gi", Sandboxes: 1}, Options{})
	victim := preemptible("victim", "1", 0)
	victim.Spec.Resources.Disk = "1Gi"
	v := mustCreate(t, c, victim, "alice")
	obj, owner := asking("head", "bob", "1", 5)
	obj.Spec.Resources.Disk = ""
	head := mustCreate(t, c, obj, owner)
	schedule(t, c)
	if got := read(t, c, v.Status.ID); !requeued(got) {
		t.Fatalf("the victim is %s", got.Status.Phase)
	}
	deleteSandbox(t, c, head.Status.ID)
	schedule(t, c)
	if phase, _ := sandboxPhase(t, c, v.Status.ID); phase != driver.Running {
		t.Fatalf("a victim whose disk is all the environment has is %s once the head is gone", phase)
	}
}
