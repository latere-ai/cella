// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// worker is one environment a data plane serves, as a controller test builds
// it: the driver behind it and the registrations the phase loop reads.
type worker struct {
	driver        *fakeDriver
	registrations []Registration
}

// twoEnvironments opens a controller over the environment it drives itself and
// one a worker serves, with a fake driver behind each. It is the fixture of
// every routing case: the two drivers are distinct objects, so which one a
// call reached is a fact and not an inference.
func twoEnvironments(t *testing.T, o Options) (*Controller, *fakeDriver, *worker, *fakeClock) {
	t.Helper()
	clock := newClock()
	here, there := newDriver(clock), newDriver(clock)
	w := &worker{driver: there}
	o.Driver, o.Clock, o.Environment, o.Log = here, clock, "default", slog.New(slog.DiscardHandler)
	if o.Store == nil && o.DataDir == "" {
		o.DataDir = t.TempDir()
	}
	o.NewDriver = func(v1.Environment) (driver.Driver, error) { return there, nil }
	o.Registrations = func(string) []Registration { return w.registrations }
	c, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, here, w, clock
}

// applyWorker registers one worker environment through the apply route's own
// entry point, which is what an administrator's PUT reaches.
func applyWorker(t *testing.T, c *Controller, name string) v1.Environment {
	t.Helper()
	obj, _, err := c.ApplyEnvironment(t.Context(), v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: name},
		Spec: v1.EnvironmentSpec{
			Mode: v1.EnvironmentWorker, Isolation: v1.IsolationNone,
			Capacity: v1.Capacity{CPU: "8", Memory: "16Gi", Disk: "100Gi", Sandboxes: 10},
		},
		Status: v1.EnvironmentStatus{Owner: "alice"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// ready puts one environment in the phase that places, which the phase loop
// writes from a live registration.
func ready(t *testing.T, c *Controller, w *worker, name string) {
	t.Helper()
	w.registrations = []Registration{{Worker: "wrk_1", LastHeartbeat: c.clock.Now(), Connected: true}}
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	obj, err := c.GetEnvironment(name)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != v1.EnvironmentReady {
		t.Fatalf("the environment is %s, want Ready", obj.Status.Phase)
	}
}

// TestDefaultEnvironment: the environment this cellad drives itself is written
// from its variables at first start, is authoritative afterwards, fixes the
// two fields the variables set, and is never deleted.
func TestDefaultEnvironment(t *testing.T) {
	dir := t.TempDir()
	c, here, _, _ := twoEnvironments(t, Options{
		DataDir: dir, Capacity: 12, SchedulingMode: v1.SchedulingDirect,
		Pool:               v1.PoolSpec{Size: 0},
		CapacityQuantities: v1.Capacity{CPU: "4", Memory: "8Gi"},
		Gateway:            GatewayAddresses{Proxy: "egress.example.internal:8080"},
	})
	obj, err := c.GetEnvironment("")
	if err != nil {
		t.Fatalf("the default environment was not seeded: %v", err)
	}
	switch {
	case obj.Metadata.Name != "default" || obj.Status.ID != "default":
		t.Errorf("the default is named %q with id %q", obj.Metadata.Name, obj.Status.ID)
	case obj.Spec.Mode != v1.EnvironmentInprocess:
		t.Errorf("the default's mode is %q", obj.Spec.Mode)
	case obj.Spec.Isolation != here.Isolation():
		t.Errorf("the default declares isolation %q and its driver %q", obj.Spec.Isolation, here.Isolation())
	case obj.Spec.Capacity.CPU != "4" || obj.Spec.Capacity.Sandboxes != 12:
		t.Errorf("the capacity is %+v, want the variables' figures", obj.Spec.Capacity)
	case obj.Spec.Gateway != "egress.example.internal:8080":
		t.Errorf("the gateway is %q", obj.Spec.Gateway)
	case obj.Status.Owner != EnvironmentOwner:
		t.Errorf("the default is owned by %q", obj.Status.Owner)
	}

	// An administrator's edit is what the object holds afterwards, and a
	// second start with other variables does not overwrite it.
	obj.Spec.Capacity = v1.Capacity{CPU: "64", Memory: "128Gi", Sandboxes: 99}
	if _, _, err = c.ApplyEnvironment(t.Context(), obj, obj.Status.Version); err != nil {
		t.Fatalf("the default could not be edited: %v", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	again, _, _, _ := twoEnvironments(t, Options{DataDir: dir, Capacity: 3})
	edited, err := again.GetEnvironment("default")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Spec.Capacity.Sandboxes != 99 {
		t.Errorf("a changed variable overwrote the edit: %+v", edited.Spec.Capacity)
	}

	// The two fields the driver fixes do not move, and the object is not
	// deletable: it describes the process that holds it.
	edited.Spec.Mode = v1.EnvironmentWorker
	if _, _, err = again.ApplyEnvironment(t.Context(), edited, edited.Status.Version); !errors.Is(err, ErrEnvironmentReserved) {
		t.Errorf("the default's mode moved: %v", err)
	}
	if err = again.DeleteEnvironment(t.Context(), "default"); !errors.Is(err, ErrEnvironmentReserved) {
		t.Errorf("the default was deletable: %v", err)
	}
}

// TestDefaultEnvironmentDeclaresAutoWithNoFigures: an operator who declared no
// capacity gets the word auto, which is spec 021's ceiling read from the
// cluster or the host rather than from the object.
func TestDefaultEnvironmentDeclaresAutoWithNoFigures(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{})
	obj, err := c.GetEnvironment("default")
	if err != nil {
		t.Fatal(err)
	}
	if !obj.Spec.Capacity.Auto {
		t.Errorf("the capacity is %+v, want auto", obj.Spec.Capacity)
	}
}

// TestControllerRoutesByEnvironment: a sandbox on a worker's environment
// reaches that environment's driver at every call site, and one on the
// default reaches the driver this process opened.
func TestControllerRoutesByEnvironment(t *testing.T) {
	c, here, w, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")

	obj := workspace()
	obj.Metadata.Name = "there"
	obj.Spec.Environment = "eu-gpu"
	created, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatalf("the create on the worker's environment failed: %v", err)
	}
	switch {
	case created.Status.Environment != "eu-gpu":
		t.Errorf("the sandbox names environment %q", created.Status.Environment)
	case len(w.driver.sandboxes()) != 1:
		t.Errorf("the worker's driver holds %d sandboxes, want one", len(w.driver.sandboxes()))
	case len(here.sandboxes()) != 0:
		t.Errorf("the create reached the environment this process drives: %+v", here.sandboxes())
	}

	// A create with no environment at all is the one cellad drives itself,
	// which is the contract's own default.
	local := workspace()
	local.Metadata.Name = "here"
	local.Spec.Environment = ""
	if _, err = c.Create(t.Context(), local, "alice", 0); err != nil {
		t.Fatalf("the create on the default failed: %v", err)
	}
	if len(here.sandboxes()) != 1 {
		t.Errorf("the default's driver holds %d sandboxes, want one", len(here.sandboxes()))
	}

	// Every act after the create reads status.environment, so the stop, the
	// touch and the delete all land on the same driver as the create.
	id := created.Status.ID
	if err = c.Touch(t.Context(), id); err != nil {
		t.Fatalf("the touch failed: %v", err)
	}
	if _, _, touches := w.driver.acted(); !slices.Contains(touches, id) {
		t.Errorf("the touch went to %v, not the worker's driver", touches)
	}
	if _, err = c.Act(t.Context(), id, "stop"); err != nil {
		t.Fatalf("the stop failed: %v", err)
	}
	if stops, _, _ := w.driver.acted(); !slices.Contains(stops, id) {
		t.Errorf("the stop went to %v, not the worker's driver", stops)
	}
	if _, err = c.Act(t.Context(), id, "delete"); err != nil {
		t.Fatalf("the delete failed: %v", err)
	}
	if _, deletes, _ := w.driver.acted(); !slices.Contains(deletes, id) {
		t.Errorf("the delete went to %v, not the worker's driver", deletes)
	}
	if _, deletes, _ := here.acted(); len(deletes) != 0 {
		t.Errorf("the delete also reached the default's driver: %v", deletes)
	}
}

// TestARestartKeepsSandboxesOnEveryEnvironment: one control plane holds every
// environment it serves, so a restart reads back the sandboxes of all of them
// and each is still routed to the driver of its own. Without that a cellad
// that had placed one sandbox on a worker's environment would refuse to open
// its own store.
func TestARestartKeepsSandboxesOnEveryEnvironment(t *testing.T) {
	dir := t.TempDir()
	c, _, w, _ := twoEnvironments(t, Options{DataDir: dir})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	obj := workspace()
	obj.Metadata.Name = "there"
	obj.Spec.Environment = "eu-gpu"
	created, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}

	again, here, back, _ := twoEnvironments(t, Options{DataDir: dir})
	read, err := again.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatalf("the sandbox on the worker's environment did not survive the restart: %v", err)
	}
	if read.Status.Environment != "eu-gpu" {
		t.Errorf("the sandbox came back on environment %q", read.Status.Environment)
	}
	// The environment came back with it, and the acts still reach its own
	// driver rather than the one this process opened.
	if phase(t, again, "eu-gpu") == "" {
		t.Fatal("the environment did not come back")
	}
	ready(t, again, back, "eu-gpu")
	if _, err = again.Act(t.Context(), created.Status.ID, "delete"); err != nil {
		t.Fatalf("the delete after the restart failed: %v", err)
	}
	if _, deletes, _ := back.driver.acted(); len(deletes) != 1 {
		t.Errorf("the delete went to %v on the worker's driver", deletes)
	}
	if _, deletes, _ := here.acted(); len(deletes) != 0 {
		t.Errorf("the delete reached the default's driver: %v", deletes)
	}
}

// TestCreateOnAnEnvironmentBelowReady: an environment whose data plane is not
// there takes no sandbox and keeps the ones it has. The refusal is the
// controller's own, because a driver with no worker reports that nothing is
// running, which a caller would read as a conflict.
func TestCreateOnAnEnvironmentBelowReady(t *testing.T) {
	c, _, w, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")

	obj := workspace()
	obj.Metadata.Name = "before"
	obj.Spec.Environment = "eu-gpu"
	running, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}

	// The worker goes away and the window passes: the environment is Offline
	// and the sandbox it already holds is untouched.
	w.registrations = nil
	clock.Advance(2 * time.Minute)
	if err = c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	env, err := c.GetEnvironment("eu-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if env.Status.Phase != v1.EnvironmentOffline || env.Status.Reason != v1.ReasonHeartbeatLost {
		t.Fatalf("the environment is %s/%s, want Offline/HeartbeatLost", env.Status.Phase, env.Status.Reason)
	}
	next := workspace()
	next.Metadata.Name = "after"
	next.Spec.Environment = "eu-gpu"
	if _, err = c.Create(t.Context(), next, "alice", 0); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Errorf("a create on an offline environment answered %v", err)
	}
	if _, err = c.Get(t.Context(), running.Status.ID, "alice"); err != nil {
		t.Errorf("the sandbox already placed did not survive the environment going offline: %v", err)
	}
	// A manifest naming an environment this control plane does not hold is
	// not found rather than placed anywhere.
	absent := workspace()
	absent.Metadata.Name = "nowhere"
	absent.Spec.Environment = "us-east"
	if _, err = c.Create(t.Context(), absent, "alice", 0); !errors.Is(err, ErrNoEnvironment) {
		t.Errorf("a create on an absent environment answered %v", err)
	}
}

// TestEnvironmentPhases drives the machine of spec 021 under a fake clock:
// applied and waiting, Ready on the first heartbeat, Offline once the window
// has passed with nothing answering, and Ready again when a worker returns.
func TestEnvironmentPhases(t *testing.T) {
	events := &actRecorder{}
	c, _, w, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute, Events: events})
	applied := applyWorker(t, c, "eu-gpu")
	if applied.Status.Phase != v1.EnvironmentPending {
		t.Fatalf("an applied environment is %s, want Pending", applied.Status.Phase)
	}

	// Nothing has registered, so there is no heartbeat to have lost: the
	// environment waits rather than reporting a data plane that failed.
	clock.Advance(2 * time.Minute)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentPending {
		t.Errorf("an environment nothing ever registered on is %s", phase(t, c, "eu-gpu"))
	}

	w.registrations = []Registration{{Worker: "wrk_1", LastHeartbeat: clock.Now(), Connected: true}}
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentReady {
		t.Errorf("a registered environment is %s, want Ready", phase(t, c, "eu-gpu"))
	}
	if got := events.environmentsOf(MutationEnvironmentRegistered); len(got) != 1 || got[0].Workers != 1 {
		t.Errorf("the transition into Ready recorded %+v", got)
	}

	// A tick that found nothing new writes nothing, so a reader of the feed
	// sees one record per transition and not one per tick.
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := events.environmentsOf(MutationEnvironmentRegistered); len(got) != 1 {
		t.Errorf("a tick that changed nothing recorded %d times", len(got))
	}

	// One missed heartbeat is not an outage: the environment stays Ready
	// until the window has passed.
	w.registrations = nil
	clock.Advance(30 * time.Second)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentReady {
		t.Errorf("one missed heartbeat made the environment %s", phase(t, c, "eu-gpu"))
	}
	clock.Advance(time.Minute)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentOffline {
		t.Errorf("the environment is %s after the window, want Offline", phase(t, c, "eu-gpu"))
	}
	if got := events.environmentsOf(MutationEnvironmentOffline); len(got) != 1 || got[0].Reason != v1.ReasonHeartbeatLost {
		t.Errorf("the transition into Offline recorded %+v", got)
	}

	// The worker comes back and the environment is placeable once more.
	w.registrations = []Registration{{Worker: "wrk_2", LastHeartbeat: clock.Now(), Connected: true}}
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentReady {
		t.Errorf("the environment did not return to Ready: %s", phase(t, c, "eu-gpu"))
	}
	// An environment that comes back is an update, not a second
	// registration, which is what spec 021's event table says.
	if got := events.environmentsOf(MutationEnvironmentUpdated); len(got) != 1 {
		t.Errorf("the return to Ready recorded %d updates, want one", len(got))
	}
	if got := events.environmentsOf(MutationEnvironmentRegistered); len(got) != 1 {
		t.Errorf("the return to Ready recorded a second registration: %d", len(got))
	}

	// A worker that registered and dropped its stream is not counted: the
	// environment cannot be placed on through it.
	w.registrations = []Registration{{Worker: "wrk_2", LastHeartbeat: clock.Now(), Connected: false}}
	clock.Advance(2 * time.Minute)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentOffline {
		t.Errorf("a registration with no stream held the environment at %s", phase(t, c, "eu-gpu"))
	}
}

// TestTheInProcessEnvironmentAnswersFromItsDriver: the environment cellad
// drives itself is Ready while its driver answers and Offline once it has
// failed for the window, which is the other half of spec 021's table.
func TestTheInProcessEnvironmentAnswersFromItsDriver(t *testing.T) {
	c, here, _, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute})
	if phase(t, c, "default") != v1.EnvironmentReady {
		t.Fatalf("the environment this process drives is %s", phase(t, c, "default"))
	}
	here.failReady(errors.New("the runtime is not answering"))
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "default") != v1.EnvironmentReady {
		t.Errorf("one failed probe made the environment %s", phase(t, c, "default"))
	}
	clock.Advance(2 * time.Minute)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	obj, err := c.GetEnvironment("default")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != v1.EnvironmentOffline || obj.Status.Reason != v1.ReasonDriverNotReady {
		t.Errorf("the environment is %s/%s, want Offline/DriverNotReady", obj.Status.Phase, obj.Status.Reason)
	}
	here.failReady(nil)
	if err = c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "default") != v1.EnvironmentReady {
		t.Errorf("the driver answered again and the environment stayed %s", phase(t, c, "default"))
	}
}

// TestTheLoopRunsUnderItsLease holds the single-writer rule of design 010: a
// replica that does not hold the environments lease computes no phase.
func TestTheLoopRunsUnderItsLease(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{Lease: refusingLease{}})
	applyWorker(t, c, "eu-gpu")
	w.registrations = []Registration{{Worker: "wrk_1", LastHeartbeat: c.clock.Now(), Connected: true}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunEnvironments(ctx) }()
	cancel()
	<-done
	if phase(t, c, "eu-gpu") != v1.EnvironmentPending {
		t.Errorf("a replica without the lease wrote %s", phase(t, c, "eu-gpu"))
	}
}

// refusingLease is a replica that holds nothing, which is every replica but
// one.
type refusingLease struct{}

func (refusingLease) Acquire(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

// TestReaperPerEnvironment: one tick passes over every placeable environment,
// so a deadline is enforced on the worker's sandboxes as well as on the
// default's, and an environment below Ready is left alone.
func TestReaperPerEnvironment(t *testing.T) {
	c, here, w, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")

	local := workspace()
	local.Metadata.Name = "here"
	local.Spec.Lifecycle = v1.Lifecycle{TTL: "1h"}
	if _, err := c.Create(t.Context(), local, "alice", 0); err != nil {
		t.Fatal(err)
	}
	remote := workspace()
	remote.Metadata.Name = "there"
	remote.Spec.Environment = "eu-gpu"
	remote.Spec.Lifecycle = v1.Lifecycle{TTL: "1h"}
	if _, err := c.Create(t.Context(), remote, "alice", 0); err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)
	acted, err := c.Reap(t.Context())
	if err != nil {
		t.Fatalf("the tick did not finish: %v", err)
	}
	if acted != 2 {
		t.Errorf("the tick acted on %d sandboxes, want one per environment", acted)
	}
	_, deletedHere, _ := here.acted()
	_, deletedThere, _ := w.driver.acted()
	if len(deletedHere) != 1 || len(deletedThere) != 1 {
		t.Errorf("the tick deleted %v here and %v there", deletedHere, deletedThere)
	}
}

// TestTheReaperHoldsOnAnEnvironmentBelowReady: an environment whose data plane
// is gone reports nothing, and reporting nothing is not reporting an empty
// world, so no rule is applied to what it holds.
func TestTheReaperHoldsOnAnEnvironmentBelowReady(t *testing.T) {
	c, _, w, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	obj := workspace()
	obj.Metadata.Name = "there"
	obj.Spec.Environment = "eu-gpu"
	obj.Spec.Lifecycle = v1.Lifecycle{TTL: "1h"}
	if _, err := c.Create(t.Context(), obj, "alice", 0); err != nil {
		t.Fatal(err)
	}
	w.registrations = nil
	clock.Advance(2 * time.Hour)
	if err := c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, deletes, _ := w.driver.acted(); len(deletes) != 0 {
		t.Errorf("the reaper ended %v on an offline environment", deletes)
	}
}

// TestPoolPerEnvironment: the refill loop keeps each environment's own pool at
// the size that environment declares, under that environment's own lease.
func TestPoolPerEnvironment(t *testing.T) {
	c, here, w, _ := twoEnvironments(t, Options{})
	here.declarePool()
	w.driver.declarePool()
	env := applyWorker(t, c, "eu-gpu")
	env.Spec.Pool = v1.PoolSpec{Size: 2, Image: "registry.example.com/base:1"}
	if _, _, err := c.ApplyEnvironment(t.Context(), env, env.Status.Version); err != nil {
		t.Fatal(err)
	}
	ready(t, c, w, "eu-gpu")
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(w.driver.entries()) != 2 {
		t.Errorf("the worker's pool holds %d entries, want two", len(w.driver.entries()))
	}
	if len(here.entries()) != 0 {
		t.Errorf("the default's pool holds %d entries and declares none", len(here.entries()))
	}
}

// TestASpawnRunsWhereItsParentRuns: boundary rule 8 of design 022 read
// through the environment. A child names none of its own, so a sandbox on a
// worker's environment spawns onto that environment rather than onto the one
// the control plane drives itself.
func TestASpawnRunsWhereItsParentRuns(t *testing.T) {
	c, here, w, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	obj := workspace()
	obj.Metadata.Name = "root"
	obj.Spec.Environment = "eu-gpu"
	obj.Spec.Mesh.Spawn = v1.Spawn{Budget: 2, Depth: 1}
	parent, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	child := workspace()
	child.Metadata.Name = "child"
	child.Spec.Environment = ""
	spawned, err := c.Spawn(t.Context(), child, parent, 0)
	if err != nil {
		t.Fatalf("the spawn failed: %v", err)
	}
	if spawned.Status.Environment != "eu-gpu" {
		t.Errorf("the child landed on environment %q", spawned.Status.Environment)
	}
	if len(here.sandboxes()) != 0 {
		t.Errorf("the child reached the default's driver: %+v", here.sandboxes())
	}
	if len(w.driver.sandboxes()) != 2 {
		t.Errorf("the worker's driver holds %d sandboxes, want the parent and the child", len(w.driver.sandboxes()))
	}
}

// TestDeleteEnvironment: an environment with nothing placed on it goes, and
// one that holds a sandbox is refused rather than left with sandboxes nothing
// drives.
func TestDeleteEnvironment(t *testing.T) {
	var released atomic.Bool
	c, _, w, _ := twoEnvironments(t, Options{
		ReleaseDriver: func(v1.Environment) { released.Store(true) },
	})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	obj := workspace()
	obj.Metadata.Name = "there"
	obj.Spec.Environment = "eu-gpu"
	created, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteEnvironment(t.Context(), "eu-gpu"); !errors.Is(err, ErrEnvironmentInUse) {
		t.Errorf("an environment holding a sandbox was deleted: %v", err)
	}
	if _, err = c.Act(t.Context(), created.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteEnvironment(t.Context(), "eu-gpu"); err != nil {
		t.Fatalf("an empty environment was not deleted: %v", err)
	}
	if !released.Load() {
		t.Errorf("the delete left the environment's workers holding their streams")
	}
	if _, err = c.GetEnvironment("eu-gpu"); !errors.Is(err, ErrNoEnvironment) {
		t.Errorf("the deleted environment still reads: %v", err)
	}
	if err = c.DeleteEnvironment(t.Context(), "eu-gpu"); !errors.Is(err, ErrNoEnvironment) {
		t.Errorf("a second delete answered %v", err)
	}
}

// TestApplyEnvironmentConcurrency holds design 008's rule at the controller:
// a write at the version the read returned lands, and one at a version the
// row has moved past is refused.
func TestApplyEnvironmentConcurrency(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{})
	first := applyWorker(t, c, "eu-gpu")
	if first.Status.Version == 0 {
		t.Fatal("an applied environment carries no version")
	}
	stale := first
	stale.Spec.Capacity.Sandboxes = 20
	second, _, err := c.ApplyEnvironment(t.Context(), stale, first.Status.Version)
	if err != nil {
		t.Fatalf("a write at the version the read returned was refused: %v", err)
	}
	if second.Status.Version == first.Status.Version {
		t.Errorf("the version did not move: %d", second.Status.Version)
	}
	if _, _, err = c.ApplyEnvironment(t.Context(), stale, first.Status.Version); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("a stale version answered %v", err)
	}
}

// TestResolveAgainstTheRegistry: a manifest resolves against the environment
// it names, and one naming an environment this control plane does not hold is
// not found at spec.environment.
func TestResolveAgainstTheRegistry(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	lookup := c.Lookup()
	for _, name := range []string{"", "default", "eu-gpu"} {
		obj, err := lookup.Environment(t.Context(), name)
		if err != nil {
			t.Fatalf("the lookup of %q failed: %v", name, err)
		}
		if obj.Status.Driver == "" || obj.Status.Isolation == "" {
			t.Errorf("the environment %q resolves against no driver: %+v", name, obj.Status)
		}
	}
	if _, err := lookup.Environment(t.Context(), "us-east"); err == nil {
		t.Errorf("an environment this control plane does not hold resolved")
	}
	if _, err := lookup.Secret(t.Context(), "anything"); err == nil {
		t.Errorf("the registry answered a secret")
	}
}

// TestApplyEnvironmentRefusals holds what the controller refuses before the
// route ever decides: the control plane's own object applied by a caller, and
// an apply on a control plane that stores no environment at all.
func TestApplyEnvironmentRefusals(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{})
	mine := v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: "eu-gpu"},
		Spec:     v1.EnvironmentSpec{Mode: v1.EnvironmentInprocess, Isolation: v1.IsolationNone},
	}
	if _, _, err := c.ApplyEnvironment(t.Context(), mine, 0); !errors.Is(err, ErrEnvironmentReserved) {
		t.Errorf("a caller applied an in-process environment: %v", err)
	}
	named := mine
	named.Metadata.Name = "default"
	named.Spec.Mode = v1.EnvironmentWorker
	if _, _, err := c.ApplyEnvironment(t.Context(), named, 0); !errors.Is(err, ErrEnvironmentReserved) {
		t.Errorf("a caller took over the default's name: %v", err)
	}
}

// phase reads one environment's phase.
func phase(t *testing.T, c *Controller, name string) string {
	t.Helper()
	obj, err := c.GetEnvironment(name)
	if err != nil {
		t.Fatal(err)
	}
	return obj.Status.Phase
}

// TestTheRegistryAnswersForAnAbsentEnvironment: every accessor answers
// nothing rather than reaching for a driver that is not there, so a caller
// gated on a capability is refused before an act is attempted.
func TestTheRegistryAnswersForAnAbsentEnvironment(t *testing.T) {
	c, here, _, _ := twoEnvironments(t, Options{Pool: v1.PoolSpec{Size: 0}})
	switch {
	case c.DriverName() != here.Name():
		t.Errorf("the default reports the driver %q", c.DriverName())
	case c.Isolation() != here.Isolation():
		t.Errorf("the default reports isolation %q", c.Isolation())
	case !c.Capabilities().Equal(here.Capabilities()):
		t.Errorf("the default reports capabilities %+v", c.Capabilities())
	case c.DriverNameOf("us-east") != "":
		t.Errorf("an absent environment reports the driver %q", c.DriverNameOf("us-east"))
	case c.IsolationOf("us-east") != driver.IsolationNone:
		t.Errorf("an absent environment reports isolation %q", c.IsolationOf("us-east"))
	case !c.CapabilitiesOf("us-east").Equal(driver.Capabilities{}):
		t.Errorf("an absent environment declares %+v", c.CapabilitiesOf("us-east"))
	}
	if pool, capacity := c.poolOf("us-east"); pool.Size != 0 || capacity != 0 {
		t.Errorf("an absent environment keeps a pool of %+v under %d", pool, capacity)
	}
	if names := c.ListEnvironments(); len(names) != 1 || names[0].Metadata.Name != "default" {
		t.Errorf("the list answers %+v", names)
	}
}

// TestAControlPlaneThatStoresNoEnvironment: a store without the kind serves
// the environment this cellad drives itself and refuses to apply another,
// which is a capability rather than a failure somewhere below.
func TestAControlPlaneThatStoresNoEnvironment(t *testing.T) {
	clock := newClock()
	d := newDriver(clock)
	c, err := Open(Options{
		Store: &plainStore{objects: map[string]v1.Sandbox{}}, Driver: d,
		Environment: "default", Clock: clock, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err = c.GetEnvironment("default"); err != nil {
		t.Errorf("the environment this cellad drives is not readable: %v", err)
	}
	if _, _, err = c.ApplyEnvironment(t.Context(), environmentManifest("eu-gpu"), 0); !errors.Is(err, ErrNoEnvironmentStore) {
		t.Errorf("an apply answered %v", err)
	}
	if err = c.DeleteEnvironment(t.Context(), "eu-gpu"); !errors.Is(err, ErrNoEnvironmentStore) {
		t.Errorf("a delete answered %v", err)
	}
	// Placement still works: the environment this process drives is the one
	// every manifest that names none gets.
	if _, err = c.Create(t.Context(), workspace(), "alice", 0); err != nil {
		t.Errorf("a create on a control plane with no environment store failed: %v", err)
	}
}

// environmentManifest is one worker environment as a caller applies it.
func environmentManifest(name string) v1.Environment {
	return v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"region": "eu"},
			Annotations: map[string]string{"note": "kept"}},
		Spec: v1.EnvironmentSpec{
			Mode: v1.EnvironmentWorker, Isolation: v1.IsolationNone,
			Capacity:   v1.Capacity{CPU: "8", Sandboxes: 10},
			Scheduling: v1.SchedulingSpec{Mode: v1.SchedulingDirect, Queues: []string{"default"}, DefaultQueue: "default"},
			Pool:       v1.PoolSpec{Display: &v1.Display{Width: 1280, Height: 800}},
		},
	}
}

// TestApplyWithoutADriverSeam: a control plane that stores environments but
// has no way to build a worker's driver refuses the apply rather than storing
// an environment nothing serves.
func TestApplyWithoutADriverSeam(t *testing.T) {
	clock := newClock()
	d := newDriver(clock)
	c, err := Open(Options{
		DataDir: t.TempDir(), Driver: d, Environment: "default",
		Clock: clock, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, _, err = c.ApplyEnvironment(t.Context(), environmentManifest("eu-gpu"), 0); !errors.Is(err, ErrNoEnvironmentStore) {
		t.Errorf("an apply with no driver seam answered %v", err)
	}
}

// TestAStoredEnvironmentComesBackWithoutADriverSeam: a control plane that
// restarts without the seam holds the object and drives nothing through it,
// so an act on a sandbox of that environment names the environment rather
// than failing on a driver that is not there.
func TestAStoredEnvironmentComesBackWithoutADriverSeam(t *testing.T) {
	dir := t.TempDir()
	c, _, _, _ := twoEnvironments(t, Options{DataDir: dir})
	applyWorker(t, c, "eu-gpu")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	clock := newClock()
	again, err := Open(Options{
		DataDir: dir, Driver: newDriver(clock), Environment: "default",
		Clock: clock, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("the control plane did not open: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if _, err = again.GetEnvironment("eu-gpu"); err != nil {
		t.Errorf("the stored environment did not come back: %v", err)
	}
	// Nothing can be placed there: the environment is Pending because no
	// data plane reported, and it has no driver to report through either.
	obj := workspace()
	obj.Spec.Environment = "eu-gpu"
	if _, err = again.Create(t.Context(), obj, "alice", 0); !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Errorf("a create on an environment with no driver answered %v", err)
	}
	if _, err = again.driverFor("eu-gpu"); !errors.Is(err, ErrNoEnvironment) {
		t.Errorf("the environment answered a driver: %v", err)
	}
}

// TestTheEnvironmentLoopTicks: the loop runs its first pass at once and then
// on every tick, and stops with the context.
func TestTheEnvironmentLoopTicks(t *testing.T) {
	c, _, w, clock := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunEnvironments(ctx) }()
	w.registrations = []Registration{{Worker: "wrk_1", Driver: "native", LastHeartbeat: clock.Now(), Connected: true}}
	clock.ticks <- clock.Now()
	waitFor(t, "the loop to write the phase", func() bool {
		obj, err := c.GetEnvironment("eu-gpu")
		return err == nil && obj.Status.Phase == v1.EnvironmentReady && obj.Status.Driver == "native"
	})
	cancel()
	<-done
	if !clock.tickerStopped() {
		t.Error("the loop left its ticker running")
	}
}

// TestTheEnvironmentLoopSurvivesALeaseThatFails: a lease the store cannot
// answer holds the tick rather than ending the loop.
func TestTheEnvironmentLoopSurvivesALeaseThatFails(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{Lease: failingLease{}})
	applyWorker(t, c, "eu-gpu")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunEnvironments(ctx) }()
	cancel()
	<-done
	if phase(t, c, "eu-gpu") != v1.EnvironmentPending {
		t.Errorf("a tick that could not take the lease wrote %s", phase(t, c, "eu-gpu"))
	}
}

// failingLease is a store that cannot answer whether this replica holds one.
type failingLease struct{}

func (failingLease) Acquire(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("the lease table is unavailable")
}

// TestTheLookupCarriesWhatThePhaseLoopWrote: an environment the loop has
// reached resolves against the status it wrote, and one it has not against
// the driver behind it.
func TestTheLookupCarriesWhatThePhaseLoopWrote(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	before, err := c.Lookup().Environment(t.Context(), "eu-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if before.Status.Isolation == "" {
		t.Errorf("an environment the loop has not reached resolves against nothing: %+v", before.Status)
	}
	ready(t, c, w, "eu-gpu")
	after, err := c.Lookup().Environment(t.Context(), "eu-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != v1.EnvironmentReady {
		t.Errorf("the lookup answers phase %q", after.Status.Phase)
	}
}

// TestAnEnvironmentIsClonedOutOfTheRegistry: what a caller reads carries
// every reference the stored object holds, and writing through it changes
// nothing the registry keeps.
func TestAnEnvironmentIsClonedOutOfTheRegistry(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{})
	applied, _, err := c.ApplyEnvironment(t.Context(), environmentManifest("eu-gpu"), 0)
	if err != nil {
		t.Fatal(err)
	}
	applied.Metadata.Labels["region"] = "us"
	applied.Metadata.Annotations["note"] = "changed"
	applied.Spec.Scheduling.Queues[0] = "other"
	applied.Spec.Pool.Display.Width = 1920
	read, err := c.GetEnvironment("eu-gpu")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case read.Metadata.Labels["region"] != "eu":
		t.Errorf("a caller's write reached the registry's labels: %v", read.Metadata.Labels)
	case read.Metadata.Annotations["note"] != "kept":
		t.Errorf("a caller's write reached the annotations: %v", read.Metadata.Annotations)
	case read.Spec.Scheduling.Queues[0] != "default":
		t.Errorf("a caller's write reached the queues: %v", read.Spec.Scheduling.Queues)
	case read.Spec.Pool.Display.Width != 1280:
		t.Errorf("a caller's write reached the pool's display: %+v", read.Spec.Pool.Display)
	}
}

// TestTheSnapshotStoreHoldsTheEnvironmentKind: the local file store keeps the
// object, its version and the status beside it, with the same conditional
// write the durable store makes.
func TestTheSnapshotStoreHoldsTheEnvironmentKind(t *testing.T) {
	opened, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	s, ok := opened.(Environments)
	if !ok {
		t.Fatal("the snapshot store does not hold the Environment kind")
	}
	if _, err = opened.Load(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	obj := environmentManifest("eu-gpu")

	// A create at a version nobody holds is a write of a row that exists.
	if _, err = s.WriteEnvironment(ctx, obj, 7, MutationEnvironmentCreated); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("a create at a version answered %v", err)
	}
	first, err := s.WriteEnvironment(ctx, obj, 0, MutationEnvironmentCreated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WriteEnvironment(ctx, obj, 0, MutationEnvironmentUpdated); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("a second create of one name answered %v", err)
	}
	obj.Spec.Capacity.Sandboxes = 25
	second, err := s.WriteEnvironment(ctx, obj, first, MutationEnvironmentUpdated)
	if err != nil || second <= first {
		t.Fatalf("the update at %d answered %d, %v", first, second, err)
	}

	// The status write touches the status and leaves the version alone.
	obj.Status.Phase = v1.EnvironmentReady
	if err = s.WriteEnvironmentStatus(ctx, obj, MutationEnvironmentRegistered); err != nil {
		t.Fatalf("the status write failed: %v", err)
	}
	held, err := s.LoadEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	switch read := held["eu-gpu"]; {
	case read.Status.Phase != v1.EnvironmentReady:
		t.Errorf("the status write did not land: %+v", read.Status)
	case read.Status.Version != second:
		t.Errorf("the status write moved the version to %d, want %d", read.Status.Version, second)
	case read.Spec.Capacity.Sandboxes != 25:
		t.Errorf("the object is %+v", read.Spec.Capacity)
	}

	// An environment the store does not hold is not found, and a delete of
	// one it never had is not an error: the object is gone either way.
	absent := environmentManifest("us-east")
	if err = s.WriteEnvironmentStatus(ctx, absent, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a status write on an absent environment answered %v", err)
	}
	if err = s.RemoveEnvironment(ctx, "us-east", MutationEnvironmentDeleted); err != nil {
		t.Errorf("a delete of an absent environment answered %v", err)
	}
	if err = s.RemoveEnvironment(ctx, "eu-gpu", MutationEnvironmentDeleted); err != nil {
		t.Fatal(err)
	}
	if held, err = s.LoadEnvironments(); err != nil || len(held) != 0 {
		t.Errorf("the load answers %v, %v after the delete", held, err)
	}
}

// TestThePhaseLoopPassesOverAnEnvironmentItCannotDrive: an object with no
// driver has nothing to observe, so the phase it was applied with stands and
// the pass reaches every other environment.
func TestThePhaseLoopPassesOverAnEnvironmentItCannotDrive(t *testing.T) {
	c, _, _, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "eu-gpu")
	c.envMu.Lock()
	delete(c.drivers, "eu-gpu")
	c.envMu.Unlock()
	if err := c.Phases(t.Context()); err != nil {
		t.Errorf("the tick answered %v", err)
	}
	if phase(t, c, "eu-gpu") != v1.EnvironmentPending {
		t.Errorf("an environment with no driver is %s", phase(t, c, "eu-gpu"))
	}
	if phase(t, c, "default") != v1.EnvironmentReady {
		t.Errorf("the default is %s after that pass", phase(t, c, "default"))
	}
}

// TestAnOfflineEnvironmentStaysOffline: a tick that still finds nothing
// leaves the phase and its reason where they are rather than writing them
// again, so the feed carries one record per transition.
func TestAnOfflineEnvironmentStaysOffline(t *testing.T) {
	events := &actRecorder{}
	c, _, w, clock := twoEnvironments(t, Options{EnvironmentOffline: time.Minute, Events: events})
	applyWorker(t, c, "eu-gpu")
	ready(t, c, w, "eu-gpu")
	w.registrations = nil
	clock.Advance(2 * time.Minute)
	for range 3 {
		if err := c.Phases(t.Context()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	obj, err := c.GetEnvironment("eu-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != v1.EnvironmentOffline || obj.Status.Reason != v1.ReasonHeartbeatLost {
		t.Errorf("the environment is %s/%s", obj.Status.Phase, obj.Status.Reason)
	}
	if got := events.environmentsOf(MutationEnvironmentOffline); len(got) != 1 {
		t.Errorf("three ticks past the window recorded %d transitions", len(got))
	}
}
