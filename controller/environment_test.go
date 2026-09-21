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
	if got := events.environmentsOf(MutationEnvironmentRegistered); len(got) != 2 {
		t.Errorf("the return to Ready recorded %d times, want a second", len(got))
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

// TestDeleteEnvironment: an environment with nothing placed on it goes, and
// one that holds a sandbox is refused rather than left with sandboxes nothing
// drives.
func TestDeleteEnvironment(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{})
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
