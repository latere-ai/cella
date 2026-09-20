// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// adoptLocked is the fake driver's half of spec 020's exclusive act, called
// with the driver's lock held. The hook runs first, which is where a test
// takes the entry from under an adopter that has already matched it.
func (d *fakeDriver) adoptLocked(id string, s driver.State, a driver.Adoption) error {
	if hook := d.onAdopt; hook != nil {
		d.mu.Unlock()
		hook(id)
		d.mu.Lock()
		s = d.states[id]
	}
	if !s.Pool {
		return driver.ErrNotFound
	}
	if a.Workspace.Path != "" && a.Workspace.Path != driver.DefaultWorkdir {
		return driver.ErrInvalid
	}
	now := d.clock.Now()
	s.Pool = false
	s.Owner, s.Name, s.Labels = a.Owner, a.Name, maps.Clone(a.Labels)
	s.CreatedAt, s.LastActivityAt = now, now
	s.AutoStop, s.AutoDelete, s.ExpiresAt = a.Lifecycle.AutoStop, a.Lifecycle.AutoDelete, time.Time{}
	if a.Lifecycle.TTL > 0 {
		s.ExpiresAt = now.Add(a.Lifecycle.TTL)
	}
	d.states[id] = s
	d.adoptions++
	if len(a.Token) > 0 {
		d.projected[id] = string(a.Token)
	}
	return nil
}

func (d *fakeDriver) adopted() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.adoptions
}

// entries is every prewarmed entry the fake holds, in creation order.
func (d *fakeDriver) entries() []driver.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []driver.State
	for _, id := range d.order {
		if s, ok := d.states[id]; ok && s.Pool {
			out = append(out, s)
		}
	}
	return out
}

// poolOptions is a controller with a pool of the given size over the fake
// driver, with the loop's two bounds left at their defaults.
func poolOptions(size int) Options { return Options{Pool: v1.PoolSpec{Size: size}} }

// newPool builds a controller whose fake driver declares the capability.
func newPool(t *testing.T, o Options) (*Controller, *fakeDriver, *fakeClock) {
	t.Helper()
	clock := newClock()
	d := newDriver(clock)
	d.pool = true
	return newFakeOver(t, o, d, clock)
}

// TestPoolNeedsTheCapability is the refusal at start-up: a pool on a driver
// that cannot hold one would never accelerate a create and never say why.
func TestPoolNeedsTheCapability(t *testing.T) {
	clock := newClock()
	d := newDriver(clock)
	o := poolOptions(2)
	o.Driver, o.Clock, o.Environment, o.DataDir = d, clock, "default", t.TempDir()
	if _, err := Open(o); err == nil {
		t.Fatal("a pool opened on a driver that declares none")
	}
}

// TestPoolMatch is the match rule of spec 020, one row per field the rule
// reads. Everything it reads is fixed when the entry is made and cannot be
// rewritten by an adoption.
func TestPoolMatch(t *testing.T) {
	c, _, _ := newPool(t, poolOptions(1))
	c.pool.Image = "registry.example.com/base:1"
	c.pool.Resources = v1.Resources{CPU: "1", Memory: "1Gi"}
	base := func() v1.Sandbox {
		obj := workspace()
		obj.Spec.Image = c.pool.Image
		obj.Spec.Resources = c.pool.Resources
		obj.Spec.Workdir = ""
		return obj
	}
	for _, tc := range []struct {
		name  string
		mutar func(*v1.Sandbox)
		want  bool
	}{
		{"the environment's own shape", func(*v1.Sandbox) {}, true},
		{"the default workspace path", func(o *v1.Sandbox) { o.Spec.Workspace.Path = driver.DefaultWorkdir }, true},
		{"an empty workspace", func(o *v1.Sandbox) { o.Spec.Workspace.Source = v1.WorkspaceSourceEmpty }, true},
		{"the default workdir", func(o *v1.Sandbox) { o.Spec.Workdir = driver.DefaultWorkdir }, true},
		{"another image", func(o *v1.Sandbox) { o.Spec.Image = "registry.example.com/other:1" }, false},
		{"more cpu", func(o *v1.Sandbox) { o.Spec.Resources.CPU = "2" }, false},
		{"more memory", func(o *v1.Sandbox) { o.Spec.Resources.Memory = "2Gi" }, false},
		{"a disk", func(o *v1.Sandbox) { o.Spec.Resources.Disk = "10Gi" }, false},
		{"a command", func(o *v1.Sandbox) { o.Spec.Command = []string{"sh"} }, false},
		{"args", func(o *v1.Sandbox) { o.Spec.Args = []string{"-c"} }, false},
		{"a user", func(o *v1.Sandbox) { o.Spec.User = "1001" }, false},
		{"another workspace path", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/srv" }, false},
		{"another workdir", func(o *v1.Sandbox) { o.Spec.Workdir = "/srv" }, false},
		{"a git workspace", func(o *v1.Sandbox) { o.Spec.Workspace.Source = "git" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := base()
			tc.mutar(&obj)
			if got := c.matchesPool(obj); got != tc.want {
				t.Fatalf("matchesPool = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPoolAdoption is one create served from an entry: no driver create, the
// entry's id, the adoption's instant, and the reason a caller reads.
func TestPoolAdoption(t *testing.T) {
	c, d, clock := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := d.entries()
	if len(entries) != 1 {
		t.Fatalf("the pool holds %d entries, want one", len(entries))
	}
	entry := entries[0]
	clock.Advance(time.Hour)
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case obj.Status.ID != entry.ID:
		t.Errorf("the adopted sandbox is %s, want the entry %s", obj.Status.ID, entry.ID)
	case d.adopted() != 1:
		t.Errorf("the driver took %d adoptions, want one", d.adopted())
	case reasonOf(obj, v1.ConditionScheduled) != v1.ReasonFromPool:
		t.Errorf("Scheduled is %q, want FromPool", reasonOf(obj, v1.ConditionScheduled))
	case !obj.Status.CreatedAt.After(entry.CreatedAt):
		t.Errorf("createdAt %v is not after the prewarm's %v", obj.Status.CreatedAt, entry.CreatedAt)
	}
	// The sandbox is the caller's on the driver as well as in the store.
	state := d.state(entry.ID)
	if state.Pool || state.Owner != "alice" {
		t.Errorf("the driver still holds %s as an entry: %+v", entry.ID, state)
	}
	if len(d.entries()) != 0 {
		t.Errorf("the adopted entry is still in the pool")
	}
}

// reasonOf is one condition's reason, or the empty string where the status
// carries no such condition.
func reasonOf(obj v1.Sandbox, kind string) string {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == kind {
			return cond.Reason
		}
	}
	return ""
}

// TestPoolMismatchCreatesForReal holds the other half: a manifest the entry
// cannot carry never touches it, and the sandbox says it was placed.
func TestPoolMismatchCreatesForReal(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	obj := workspace()
	obj.Spec.Command = []string{"sh", "-c", "sleep 1"}
	got, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.adopted() != 0 {
		t.Errorf("a manifest with a command adopted an entry")
	}
	if len(d.entries()) != 1 {
		t.Errorf("the pool holds %d entries, want the one it was not allowed to use", len(d.entries()))
	}
	if reasonOf(got, v1.ConditionScheduled) != v1.ReasonPlaced {
		t.Errorf("Scheduled is %q, want Placed", reasonOf(got, v1.ConditionScheduled))
	}
}

// TestPoolAdoptionFallsBack is the race the pool is built around: the entry is
// taken between the match and the adoption. The create runs again on the slow
// path under a new id, and nothing of the attempt is left: no desired row, no
// map, no identity.
func TestPoolAdoptionFallsBack(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	entry := d.entries()[0]
	// Another adopter wins inside the adoption, which is the one instant the
	// driver's exclusivity decides.
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
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.ID == entry.ID {
		t.Fatalf("the create kept the lost entry's id %s", entry.ID)
	}
	if reasonOf(obj, v1.ConditionScheduled) != v1.ReasonPlaced {
		t.Errorf("Scheduled is %q, want Placed", reasonOf(obj, v1.ConditionScheduled))
	}
	if held := c.List(); len(held) != 1 || held[0].Status.ID != obj.Status.ID {
		t.Fatalf("desired state holds %d rows, want only the sandbox that exists", len(held))
	}
	if _, err := c.Get(t.Context(), entry.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the lost attempt left a row behind: %v", err)
	}
}

// TestPoolRefill holds the loop's arithmetic: it reaches the size, stops
// there, and never prewarms more than the in-flight cap in one tick.
func TestPoolRefill(t *testing.T) {
	o := poolOptions(3)
	o.PoolInFlight = 2
	c, d, _ := newPool(t, o)
	acted, err := c.Refill(t.Context())
	if err != nil || acted != 2 {
		t.Fatalf("the first tick made %d entries (%v), want the in-flight cap", acted, err)
	}
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(d.entries()); got != 3 {
		t.Fatalf("the pool holds %d entries, want the size", got)
	}
	acted, err = c.Refill(t.Context())
	if err != nil || acted != 0 {
		t.Fatalf("a full pool acted %d times (%v), want none", acted, err)
	}
	// Every entry carries the shape it was made for and nothing of a caller.
	for _, entry := range d.entries() {
		if entry.Labels[PoolShapeLabel] != c.poolShape() {
			t.Errorf("the entry %s is stamped %q, want the environment's shape", entry.ID, entry.Labels[PoolShapeLabel])
		}
		if entry.Owner != "" || entry.Name != "" {
			t.Errorf("the entry %s carries an owner or a name: %+v", entry.ID, entry)
		}
	}
}

// TestPoolPrewarmMatchesItsOwnShape ties the two halves together: what the
// loop makes is what the match rule accepts for a manifest asking for the
// environment's own shape. Without it the two could drift apart and every
// create would take the slow path with no test failing.
func TestPoolPrewarmMatchesItsOwnShape(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(1))
	c.pool.Image = "registry.example.com/base:1"
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	obj := workspace()
	obj.Spec.Image = c.pool.Image
	obj.Spec.Resources = c.pool.Resources
	obj.Spec.Workdir = ""
	entries := c.poolEntries(t.Context())
	if c.matchEntry(entries, obj) == nil {
		t.Fatalf("the pool's own entry does not match the environment's own shape: %+v", d.entries())
	}
}

// TestPoolRefillBounds holds what a tick does on bad news: a list it cannot
// read acts on nothing, and one create that fails ends the tick's prewarming
// rather than opening the rest.
func TestPoolRefillBounds(t *testing.T) {
	o := poolOptions(4)
	o.PoolInFlight = 4
	c, d, _ := newPool(t, o)
	d.set(func(f *fakeDriver) { f.listErr = errors.New("the environment is unreachable") })
	if acted, err := c.Refill(t.Context()); err == nil || acted != 0 {
		t.Fatalf("a tick with no list acted %d times (%v)", acted, err)
	}
	if len(d.entries()) != 0 {
		t.Fatal("a tick with no list prewarmed")
	}
	d.set(func(f *fakeDriver) { f.listErr, f.createErr = nil, errors.New("the engine refuses") })
	if acted, err := c.Refill(t.Context()); err == nil || acted != 0 {
		t.Fatalf("a tick whose first create failed acted %d times (%v)", acted, err)
	}
}

// TestPoolRefillHoldsTheLease is the single-writer rule: the supply side runs
// on one replica, under the environment's own lease name.
func TestPoolRefillHoldsTheLease(t *testing.T) {
	lease := &fakeLease{}
	o := poolOptions(1)
	o.Lease = lease
	c, d, _ := newPool(t, o)
	c.poolTick(t.Context())
	if len(d.entries()) != 0 {
		t.Fatal("a replica that holds no lease prewarmed")
	}
	lease.answer(true, nil)
	c.poolTick(t.Context())
	if len(d.entries()) != 1 {
		t.Fatalf("the lease holder made %d entries, want one", len(d.entries()))
	}
	if lease.name != PoolLease("default") {
		t.Errorf("the loop asked for the lease %q, want %q", lease.name, PoolLease("default"))
	}
}

// TestPoolYieldsCapacity is spec 020's capacity rule in the count form: a real
// create that does not fit takes the oldest entries first, and one that cannot
// fit at all is refused.
func TestPoolYieldsCapacity(t *testing.T) {
	o := poolOptions(2)
	o.Capacity = 2
	c, d, clock := newPool(t, o)
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := d.entries()
	if len(entries) != 2 {
		t.Fatalf("the pool holds %d entries, want two", len(entries))
	}
	oldest := entries[0].ID
	clock.Advance(time.Minute)
	// A create the entries cannot serve needs one of their slots.
	obj := workspace()
	obj.Spec.Command = []string{"sh"}
	if _, err := c.Create(t.Context(), obj, "alice", 0); err != nil {
		t.Fatal(err)
	}
	_, deletes, _ := d.acted()
	if !slices.Contains(deletes, oldest) {
		t.Errorf("the create deleted %v, want the oldest entry %s", deletes, oldest)
	}
	if len(d.entries()) != 1 {
		t.Fatalf("the pool holds %d entries after the create, want one", len(d.entries()))
	}
	// The ceiling is reached: the second create takes the last entry's slot,
	// and the third has none left to take.
	second := workspace()
	second.Metadata.Name, second.Spec.Command = "second", []string{"sh"}
	if _, err := c.Create(t.Context(), second, "alice", 0); err != nil {
		t.Fatal(err)
	}
	third := workspace()
	third.Metadata.Name, third.Spec.Command = "third", []string{"sh"}
	if _, err := c.Create(t.Context(), third, "alice", 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("a create over the ceiling is %v, want ErrQuota", err)
	}
	// The loop keeps no entry while the sandboxes hold the whole ceiling.
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(d.entries()); got != 0 {
		t.Fatalf("the pool holds %d entries with no room for one", got)
	}
}

// TestPoolDrift deletes what the pool no longer wants: an entry stamped with a
// shape the environment has changed, and one the engine moved out of Running.
func TestPoolDrift(t *testing.T) {
	c, d, clock := newPool(t, poolOptions(2))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := d.entries()
	drifted, stopped := entries[0].ID, entries[1].ID
	d.set(func(f *fakeDriver) {
		s := f.states[drifted]
		s.Labels = map[string]string{PoolShapeLabel: "another shape"}
		f.states[drifted] = s
		s = f.states[stopped]
		s.Phase = driver.Stopped
		f.states[stopped] = s
	})
	clock.Advance(DefaultPoolGrace + time.Minute)
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, deletes, _ := d.acted()
	if !slices.Contains(deletes, drifted) || !slices.Contains(deletes, stopped) {
		t.Fatalf("the loop deleted %v, want the drifted and the stopped entry", deletes)
	}
	// It replaces them in the same tick, up to the in-flight cap.
	if got := len(d.entries()); got != 2 {
		t.Fatalf("the pool holds %d entries after the drift, want two", got)
	}
}

// TestPoolGrace holds the window the deletion rules do not read: an entry that
// is still coming up would otherwise be made and unmade on alternating ticks.
func TestPoolGrace(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := d.entries()[0].ID
	d.set(func(f *fakeDriver) {
		s := f.states[id]
		s.Phase = driver.Pending
		f.states[id] = s
	})
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, deletes, _ := d.acted(); len(deletes) != 0 {
		t.Fatalf("the loop deleted %v inside the grace", deletes)
	}
}

// TestPoolOrphans drains the pool once the environment stops declaring one,
// which top-up alone would never do: a lowered size strands every entry above
// it.
func TestPoolOrphans(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(3))
	for range 2 {
		if _, err := c.Refill(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(d.entries()); got != 3 {
		t.Fatalf("the pool holds %d entries, want three", got)
	}
	c.pool.Size = 1
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(d.entries()); got != 1 {
		t.Fatalf("the pool holds %d entries after the size fell, want one", got)
	}
	c.pool.Size = 0
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(d.entries()); got != 0 {
		t.Fatalf("the pool holds %d entries with no pool declared", got)
	}
}

// TestReaperLeavesPoolEntries is the rule the two loops share: the deadline
// rules are not asked of an entry at all, even one a driver stamped a deadline
// on, because an entry's lifecycle is the refill loop's.
func TestReaperLeavesPoolEntries(t *testing.T) {
	c, d, clock := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := d.entries()[0].ID
	d.set(func(f *fakeDriver) {
		s := f.states[id]
		s.ExpiresAt = clock.Now().Add(-time.Hour)
		s.AutoStop = time.Minute
		f.states[id] = s
	})
	clock.Advance(2 * time.Hour)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("the reaper acted %d times on a pool entry (%v)", acted, err)
	}
	stops, deletes, _ := d.acted()
	if len(stops) != 0 || len(deletes) != 0 {
		t.Fatalf("the reaper stopped %v and deleted %v, want neither", stops, deletes)
	}
	if len(d.entries()) != 1 {
		t.Fatal("the reaper ended the pool entry")
	}
}

// TestPoolLoopRunsAndStops drives the loop itself, so the lease, the ticker
// and the shutdown are exercised the way cellad runs them.
func TestPoolLoopRunsAndStops(t *testing.T) {
	c, d, clock := newPool(t, poolOptions(1))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunPool(ctx) }()
	waitFor(t, "the first tick to prewarm", func() bool { return len(d.entries()) == 1 })
	clock.ticks <- clock.Now()
	waitFor(t, "the ticker to be read", func() bool { return len(d.entries()) == 1 })
	cancel()
	<-done
	if !clock.tickerStopped() {
		t.Fatal("the loop left its ticker running")
	}
}

// TestPoolLoopIsSilentWithoutTheCapability holds the other side: a driver that
// can keep no entry runs no loop.
func TestPoolLoopIsSilentWithoutTheCapability(t *testing.T) {
	c, d, _ := newFake(t, Options{})
	done := make(chan struct{})
	go func() { defer close(done); c.RunPool(t.Context()) }()
	<-done
	if len(d.entries()) != 0 {
		t.Fatal("a driver without the capability prewarmed")
	}
	if acted, err := c.Refill(t.Context()); acted != 0 || err != nil {
		t.Fatalf("Refill acted %d times without the capability (%v)", acted, err)
	}
}

// TestPoolAdoptionIsExclusiveUnderOneController holds what the controller's
// own lock gives: two creates over one entry yield one adoption and one real
// create, whichever order they run in.
func TestPoolAdoptionIsExclusiveUnderOneController(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]v1.Sandbox, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Go(func() {
			obj := workspace()
			obj.Metadata.Name = []string{"first", "second"}[i]
			results[i], errs[i] = c.Create(t.Context(), obj, "alice", 0)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if d.adopted() != 1 {
		t.Fatalf("the driver took %d adoptions, want one", d.adopted())
	}
	pooled := 0
	for _, obj := range results {
		if reasonOf(obj, v1.ConditionScheduled) == v1.ReasonFromPool {
			pooled++
		}
	}
	if pooled != 1 {
		t.Fatalf("%d of two creates came from the pool, want one", pooled)
	}
}

// TestPoolReadFailures holds what the pool does when it cannot read or write:
// a lease that cannot be asked runs nothing, a list the create path cannot
// read is a create on the slow path rather than a create that fails, and a
// delete that fails is reported without ending the tick.
func TestPoolReadFailures(t *testing.T) {
	lease := &fakeLease{err: errors.New("the lease table is unreachable")}
	o := poolOptions(1)
	o.Lease = lease
	c, d, _ := newPool(t, o)
	c.poolTick(t.Context())
	if len(d.entries()) != 0 {
		t.Fatal("a tick whose lease could not be asked prewarmed")
	}

	c, d, clock := newPool(t, poolOptions(1))
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.set(func(f *fakeDriver) { f.listErr = errors.New("the environment is unreachable") })
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if reasonOf(obj, v1.ConditionScheduled) != v1.ReasonPlaced {
		t.Errorf("a create that could not read the pool is %q, want Placed", reasonOf(obj, v1.ConditionScheduled))
	}

	d.set(func(f *fakeDriver) {
		f.listErr, f.deleteErr = nil, errors.New("the engine refuses the delete")
		s := f.states[f.order[0]]
		s.Phase = driver.Stopped
		f.states[f.order[0]] = s
	})
	clock.Advance(DefaultPoolGrace + time.Minute)
	if _, err := c.Refill(t.Context()); err == nil {
		t.Fatal("a tick whose delete failed reported success")
	}
}

// TestPoolShapeReadsEveryField is what the drift rule compares: two shapes
// that differ in any field of the entry are different pools.
func TestPoolShapeReadsEveryField(t *testing.T) {
	c, _, _ := newPool(t, poolOptions(1))
	seen := map[string]string{}
	for _, tc := range []struct {
		name string
		pool v1.PoolSpec
	}{
		{"nothing", v1.PoolSpec{}},
		{"an image", v1.PoolSpec{Image: "registry.example.com/base:1"}},
		{"another image", v1.PoolSpec{Image: "registry.example.com/other:1"}},
		{"cpu", v1.PoolSpec{Resources: v1.Resources{CPU: "1"}}},
		{"memory", v1.PoolSpec{Resources: v1.Resources{Memory: "1Gi"}}},
		{"disk", v1.PoolSpec{Resources: v1.Resources{Disk: "10Gi"}}},
		{"a display", v1.PoolSpec{Display: &v1.Display{Width: 1280, Height: 800}}},
		{"another display", v1.PoolSpec{Display: &v1.Display{Width: 1920, Height: 1080}}},
	} {
		c.pool = tc.pool
		shape := c.poolShape()
		if other, clash := seen[shape]; clash {
			t.Fatalf("%s and %s hash to one shape", tc.name, other)
		}
		seen[shape] = tc.name
	}
}

// TestCapacityCountsThePhases is the count form of spec 020's capacity: a
// sandbox on its way out or already gone holds no slot.
func TestCapacityCountsThePhases(t *testing.T) {
	for phase, want := range map[string]bool{
		driver.Pending: true, driver.Running: true, "Starting": true, "Stopping": true,
		PhaseRecovering: true, driver.Stopped: false, PhaseFailed: false,
		PhaseLost: false, PhaseDeleting: false,
	} {
		if got := countsAgainstCapacity(phase); got != want {
			t.Errorf("%s counts %v, want %v", phase, got, want)
		}
	}
}
