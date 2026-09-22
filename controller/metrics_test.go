// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// recorder is design 017's seam under test: it holds what the controller
// recorded so a case reads the call rather than a registry's exposition.
type recorder struct {
	mu        sync.Mutex
	creates   [][2]any // pool label, duration
	adoptions []string
	pool      [][2]int
	actions   [][2]string
	recovery  []string
	reminted  int
	leases    [][2]any // name, held
}

func (r *recorder) SandboxCreated(pool string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, [2]any{pool, d})
}
func (r *recorder) PoolAdoption(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adoptions = append(r.adoptions, outcome)
}
func (r *recorder) PoolSize(ready, filling int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pool = append(r.pool, [2]int{ready, filling})
}
func (r *recorder) ReaperAction(rule, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, [2]string{rule, action})
}
func (r *recorder) RecoveryAttempt(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recovery = append(r.recovery, outcome)
}
func (r *recorder) TokenReminted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reminted++
}
func (r *recorder) LeaseHeld(name string, held bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases = append(r.leases, [2]any{name, held})
}

func (r *recorder) createLabels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.creates))
	for _, c := range r.creates {
		out = append(out, c[0].(string))
	}
	return out
}

// TestCreateIsMeasured is design 017's create histogram: one observation per
// create, labelled by where the environment came from, and one adoption count
// beside it on an environment that keeps a pool.
func TestCreateIsMeasured(t *testing.T) {
	rec := &recorder{}
	c, _, clock := newFake(t, Options{Metrics: rec})
	created(t, c, "one")
	if got := rec.createLabels(); !slices.Equal(got, []string{PoolMiss}) {
		t.Errorf("create labels %v, want one miss", got)
	}
	if len(rec.adoptions) != 0 {
		t.Errorf("an environment with no pool counted an adoption: %v", rec.adoptions)
	}
	// The duration is the controller's clock, so a slow machine does not
	// make the observation wrong.
	_ = clock
	if d := rec.creates[0][1].(time.Duration); d != 0 {
		t.Errorf("the create observed %v on a clock that did not move", d)
	}
}

// TestPoolCreateIsMeasured is the same create against a pool: a hit counts an
// adoption and observes under the hit label, a miss counts and observes under
// the miss label.
func TestPoolCreateIsMeasured(t *testing.T) {
	rec := &recorder{}
	c, _, _ := newPool(t, Options{Pool: v1.PoolSpec{Size: 1}, Metrics: rec})
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	created(t, c, "adopted")
	created(t, c, "slow")
	if got := rec.createLabels(); !slices.Equal(got, []string{PoolHit, PoolMiss}) {
		t.Errorf("create labels %v, want a hit then a miss", got)
	}
	if !slices.Equal(rec.adoptions, []string{MetricAdopted, MetricMiss}) {
		t.Errorf("adoptions %v, want adopted then miss", rec.adoptions)
	}
}

// TestARefusedCreateIsNoAdoption: a create refused for its owner's count or a
// name already taken asked nothing of the pool, so it counts neither an
// adoption nor a miss and observes no duration, and the entry it matched is
// still prewarmed for the next create. One whose adoption was lost to another
// adopter took the slow path and counts a miss.
func TestARefusedCreateIsNoAdoption(t *testing.T) {
	rec := &recorder{}
	c, d, _ := newPool(t, Options{Pool: v1.PoolSpec{Size: 1}, Metrics: rec})
	refill := func() {
		t.Helper()
		if _, err := c.Refill(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	refill()
	created(t, c, "first")
	refill()
	second := workspace()
	second.Metadata.Name = "second"
	if _, err := c.Create(t.Context(), second, "alice", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("a create past the owner's count is %v", err)
	}
	taken := workspace()
	taken.Metadata.Name = "first"
	if _, err := c.Create(t.Context(), taken, "alice", 0); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a create of a name taken is %v", err)
	}
	if got := rec.createLabels(); !slices.Equal(got, []string{PoolHit}) {
		t.Errorf("create labels %v, want the first create's hit alone", got)
	}
	if !slices.Equal(rec.adoptions, []string{MetricAdopted}) {
		t.Errorf("adoptions %v, want the first create's alone", rec.adoptions)
	}
	if d.adopted() != 1 || len(d.entries()) != 1 {
		t.Fatalf("the refused creates took %d adoptions and left %d entries", d.adopted()-1, len(d.entries()))
	}
	d.set(func(f *fakeDriver) {
		f.onAdopt = func(id string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.onAdopt = nil
			s := f.states[id]
			s.Pool, s.Owner = false, "bob"
			f.states[id] = s
		}
	})
	created(t, c, "lost")
	if !slices.Equal(rec.adoptions, []string{MetricAdopted, MetricMiss}) {
		t.Errorf("adoptions %v, want the lost adoption read as a miss", rec.adoptions)
	}
	if got := rec.createLabels(); !slices.Equal(got, []string{PoolHit, PoolMiss}) {
		t.Errorf("create labels %v, want the lost adoption observed as a miss", got)
	}
}

// TestRefillReportsThePoolSize is the pushed gauge: a scrape cannot list the
// driver, so the loop reports what it found on its own tick.
func TestRefillReportsThePoolSize(t *testing.T) {
	rec := &recorder{}
	c, _, _ := newPool(t, Options{Pool: v1.PoolSpec{Size: 2}, Metrics: rec})
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rec.pool) != 2 {
		t.Fatalf("two ticks reported %d pool sizes", len(rec.pool))
	}
	if rec.pool[1] != [2]int{2, 0} {
		t.Errorf("the second tick reported %v, want two ready and none filling", rec.pool[1])
	}
}

// TestPoolTickReportsItsLease is the lease gauge from the refill loop.
func TestPoolTickReportsItsLease(t *testing.T) {
	rec := &recorder{}
	lease := &fakeLease{}
	lease.answer(true, nil)
	c, _, _ := newPool(t, Options{Pool: v1.PoolSpec{Size: 1}, Lease: lease, Metrics: rec})
	c.poolTick(t.Context())
	if len(rec.leases) != 1 || rec.leases[0] != [2]any{MetricLeasePool, true} {
		t.Errorf("the pool tick reported %v, want the pool lease held", rec.leases)
	}
}

// TestReaperActionsAreCounted is design 017's reaper counter: one count per
// rule the reaper enforced, by what it did.
func TestReaperActionsAreCounted(t *testing.T) {
	rec := &recorder{}
	c, d, clock := newFake(t, Options{Lifecycle: driver.Lifecycle{AutoStop: 10 * time.Minute}, Metrics: rec})
	idle := created(t, c, "autostop")
	expired := created(t, c, "expired")
	d.put(func() driver.State {
		s := d.state(idle.Status.ID)
		s.LastActivityAt, s.AutoStop = epoch, time.Minute
		return s
	}())
	d.put(func() driver.State {
		s := d.state(expired.Status.ID)
		s.ExpiresAt = epoch.Add(time.Minute)
		return s
	}())
	clock.Advance(2 * time.Minute)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{ReasonAutoStop, ActionStopped}, {ReasonExpired, ActionDeleted}}
	slices.SortFunc(rec.actions, func(a, b [2]string) int {
		if a[0] == b[0] {
			return 0
		}
		if a[0] < b[0] {
			return -1
		}
		return 1
	})
	if !slices.Equal(rec.actions, want) {
		t.Errorf("reaper actions %v, want %v", rec.actions, want)
	}
}

// TestReaperTickReportsItsLease is the lease gauge from the reaper: held on
// the tick it takes it, and lost where another replica holds it.
func TestReaperTickReportsItsLease(t *testing.T) {
	rec := &recorder{}
	lease := &fakeLease{}
	lease.answer(true, nil)
	c, _, _ := newFake(t, Options{Lease: lease, Metrics: rec})
	c.tick(t.Context())
	lease.answer(false, nil)
	c.tick(t.Context())
	want := [][2]any{{MetricLeaseReaper, true}, {MetricLeaseReaper, false}}
	if !slices.Equal(rec.leases, want) {
		t.Errorf("the reaper reported %v, want %v", rec.leases, want)
	}
}

// TestTokenRotationIsCounted is design 017's re-mint counter, on the one act
// design 005's token rule performs.
func TestTokenRotationIsCounted(t *testing.T) {
	rec := &recorder{}
	c, _, clock, _ := withTokens(t, 3*time.Hour, Options{Metrics: rec})
	created(t, c, "rotating")
	clock.Advance(2 * time.Hour)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the token rule did not fire: acted=%d err=%v", acted, err)
	}
	if rec.reminted != 1 {
		t.Errorf("the token rule re-minted %d times, want one", rec.reminted)
	}
}

// TestNoRecorderCountsNothing is the default: a controller built with no
// recorder behaves exactly as it did before design 017.
func TestNoRecorderCountsNothing(t *testing.T) {
	c, _, _ := newFake(t, Options{})
	if _, ok := c.metrics.(nopMetrics); !ok {
		t.Fatalf("a controller with no recorder holds %T", c.metrics)
	}
	created(t, c, "one")
	c.tick(t.Context())
}

// TestReadyFilling is the split the pool gauge carries: an entry that can be
// adopted now against one still coming up.
func TestReadyFilling(t *testing.T) {
	ready, filling := readyFilling([]driver.State{
		{Phase: driver.Running}, {Phase: driver.Pending}, {Phase: driver.Running},
	})
	if ready != 2 || filling != 1 {
		t.Errorf("readyFilling = %d ready, %d filling; want 2 and 1", ready, filling)
	}
}

// TestRecoveryIsCounted is design 017's recovery counter: one count per
// attempt that reached an answer, recovered or exhausted.
func TestRecoveryIsCounted(t *testing.T) {
	rec := &recorder{}
	c, d, _, _ := recovering(t, true, Options{Metrics: rec})
	obj := created(t, c, "work")
	vanish(d, obj.Status.ID)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the lost sandbox was not recovered: acted=%d err=%v", acted, err)
	}
	if !slices.Equal(rec.recovery, []string{MetricRecovered}) {
		t.Errorf("recovery outcomes %v, want one recovered", rec.recovery)
	}
}

// TestRecoveryExhaustionIsCounted is the other end of the same rule, and the
// one an alert reads.
func TestRecoveryExhaustionIsCounted(t *testing.T) {
	rec := &recorder{}
	clock := newClock()
	fake := newDriver(clock)
	refuses := errors.New("the driver refuses to create")
	c := withDriver(t, newDurable(true), adopting{fakeDriver: fake, err: refuses}, clock,
		Options{RecoveryAttempts: 1, Metrics: rec})
	obj := workspace()
	obj.Status = v1.SandboxStatus{ID: "sbx_exhausted", Owner: "alice", Environment: "default", Phase: driver.Running}
	c.mu.Lock()
	c.objects[obj.Status.ID] = obj
	c.mu.Unlock()

	if _, err := c.Reap(t.Context()); !errors.Is(err, refuses) {
		t.Fatalf("the first attempt returned %v, want the driver's failure", err)
	}
	clock.Advance(recoveryCeiling)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the attempts were not exhausted: acted=%d err=%v", acted, err)
	}
	if !slices.Equal(rec.recovery, []string{MetricExhausted}) {
		t.Errorf("recovery outcomes %v, want one exhausted", rec.recovery)
	}
}

// TestLostDeleteIsCountedAsAReaperAction is the lost rule without a durable
// store: the sandbox is deleted by the reaper, so it is one reaper action
// under the Lost reason.
func TestLostDeleteIsCountedAsAReaperAction(t *testing.T) {
	rec := &recorder{}
	c, d, clock, _ := recovering(t, false, Options{LostGrace: time.Minute, Metrics: rec})
	obj := created(t, c, "work")
	vanish(d, obj.Status.ID)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the lost sandbox was not reaped: acted=%d err=%v", acted, err)
	}
	if !slices.Contains(rec.actions, [2]string{ReasonLost, ActionDeleted}) {
		t.Errorf("reaper actions %v, want a Lost delete", rec.actions)
	}
}
