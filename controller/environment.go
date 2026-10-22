// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The mutations the controller appends for the Environment kind, which are
// the rows design 009's table gives it. The two the phase loop writes are
// transitions of the machine of spec 021 and not writes of the object.
const (
	MutationEnvironmentCreated    = "environment.created"
	MutationEnvironmentUpdated    = "environment.updated"
	MutationEnvironmentRegistered = "environment.registered"
	MutationEnvironmentOffline    = "environment.offline"
	MutationEnvironmentDeleted    = "environment.deleted"
)

// The refusals an environment answers, each a code of design 008's table.
var (
	// ErrEnvironmentReserved is a caller applying the control plane's own
	// environment, or deleting it. Both are 400 reserved_prefix.
	ErrEnvironmentReserved = errors.New("this environment is the control plane's own")
	// ErrEnvironmentInUse is a delete of an environment a sandbox is placed
	// on, which is 409 phase_conflict.
	ErrEnvironmentInUse = errors.New("sandboxes are placed on this environment")
	// ErrEnvironmentUnavailable is a create on an environment whose phase is
	// not Ready, which is 503 driver_unavailable. It is the controller's own
	// answer rather than the driver's: a driver with no worker reports that
	// nothing is running, which reads as a conflict and is not one.
	ErrEnvironmentUnavailable = errors.New("this environment has no data plane that can take a sandbox")
	// ErrNoEnvironmentStore is an apply on a control plane whose store holds
	// no Environment kind, which is 422 capability_unsupported.
	ErrNoEnvironmentStore = errors.New("this control plane stores no environment of its own")
)

// Environments is the Environment kind's store as the controller reads it:
// the objects beside the sandboxes, with one conditional write per object and
// the status the phase loop writes beside it.
type Environments interface {
	// LoadEnvironments reads every live Environment with its row version.
	LoadEnvironments() (map[string]v1.Environment, error)
	// WriteEnvironment stores one Environment at the version this process
	// last saw and records the mutation. It returns the version the row now
	// holds; a row that moved is ErrVersionConflict.
	WriteEnvironment(ctx context.Context, obj v1.Environment, ifVersion int64, mutation string) (int64, error)
	// WriteEnvironmentStatus writes the status the phase loop computed
	// without touching the object or its version, and records the mutation
	// where the transition has one.
	WriteEnvironmentStatus(ctx context.Context, obj v1.Environment, mutation string) error
	// RemoveEnvironment deletes one Environment and records the mutation.
	RemoveEnvironment(ctx context.Context, name, mutation string) error
}

// NewDriverFunc builds the driver serving one registered environment. It is
// the seam `runtime/remote` reaches the controller through: the controller
// holds no transport and opens no connection, so what a worker environment is
// driven by is the caller's to supply (spec 021).
type NewDriverFunc func(obj v1.Environment) (driver.Driver, error)

// Registration is one data plane worker as the control plane sees it: the id
// it claims under, when it was last heard from, and whether it holds a stream
// open now. It is what an environment's phase is computed from.
type Registration struct {
	Worker string
	// Driver is what this worker's own driver is called, which is the
	// environment's recorded driver rather than the remote one the control
	// plane reaches it through.
	Driver        string
	LastHeartbeat time.Time
	Connected     bool
	// Pending is a worker whose registration was just taken and whose stream
	// has not opened yet. It counts as a live worker, so the environment
	// reads Ready with the worker's host from the registration on, not from
	// the next tick after its stream opened.
	Pending bool
	// Host is the machine the worker runs on as its driver reports it, nil
	// where the driver reports none (spec 082).
	Host *v1.HostInfo
}

// EnvironmentsLease is the lease name design 010 gives the phase loop and
// EnvironmentInterval how often it runs.
const (
	EnvironmentsLease   = "environments"
	EnvironmentInterval = 5 * time.Second
)

// DefaultEnvironmentOffline is how long an environment is held Ready without
// a heartbeat, which CELLA_ENVIRONMENT_OFFLINE sets.
const DefaultEnvironmentOffline = 2 * time.Minute

// openEnvironments fills the registry at start: the stored objects, a driver
// for each, and the control plane's own object where the store holds none.
func (c *Controller) openEnvironments(ctx context.Context, o Options) error {
	if c.environmentStore != nil {
		stored, err := c.environmentStore.LoadEnvironments()
		if err != nil {
			return err
		}
		for name, obj := range stored {
			if name == c.environment {
				// The driver of the control plane's own environment is the
				// one this process opened; the object only describes it.
				c.environments[name] = obj
				continue
			}
			if c.newDriver == nil {
				// A control plane built with no seam for a worker's driver
				// holds the object and drives nothing through it, so every
				// act on a sandbox of that environment is ErrNoEnvironment
				// rather than a nil driver somewhere below.
				c.environments[name] = obj
				continue
			}
			d, err := c.newDriver(obj)
			if err != nil {
				return fmt.Errorf("the driver of the environment %s: %w", name, err)
			}
			c.environments[name] = obj
			c.drivers[name] = d
		}
	}
	if _, held := c.environments[c.environment]; held {
		return nil
	}
	return c.seedDefault(ctx, o)
}

// seedDefault writes the object of the environment this cellad drives itself,
// from the variables that describe the driver it opened. The variables seed it
// once: afterwards the stored object is what a read answers and an
// administrator's edit is never overwritten by a changed variable (spec 021).
//
// The seed is not validated. It is not a caller's manifest but a description
// of a driver this process has already opened and run a preflight against, so
// a field rule meant for input would refuse a start that is otherwise sound.
func (c *Controller) seedDefault(ctx context.Context, o Options) error {
	capacity := o.CapacityQuantities
	capacity.Sandboxes = o.Capacity
	if capacity.IsZero() {
		// The ceiling is the cluster's or the host's, which nothing here
		// reads. Spec 021 spells that auto; CELLA_CAPACITY_* declares
		// figures instead.
		capacity = v1.Capacity{Auto: true}
	}
	// The object is seeded Ready. It describes a driver this process has
	// already opened, and a driver that then fails its probe reaches Offline
	// the way every other environment does: through the offline window the
	// phase loop counts, rather than by never having been placeable at all.
	c.markAnswered(c.environment)
	queue := v1.DefaultQueueName
	obj := v1.Environment{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindEnvironment,
		Metadata:   v1.Metadata{Name: c.environment},
		Spec: v1.EnvironmentSpec{
			Mode:      v1.EnvironmentInprocess,
			Isolation: o.Driver.Isolation(),
			Capacity:  capacity,
			Scheduling: v1.SchedulingSpec{
				Mode: cmp.Or(o.SchedulingMode, v1.SchedulingDirect), Queues: []string{queue}, DefaultQueue: queue,
			},
			Pool:    o.Pool,
			Gateway: o.Gateway.Proxy,
		},
		Status: v1.EnvironmentStatus{
			ID: c.environment, Owner: EnvironmentOwner, Phase: v1.EnvironmentReady,
			Driver: o.Driver.Name(), Isolation: o.Driver.Isolation(),
			Capabilities: o.Driver.Capabilities(), CreatedAt: c.clock.Now(), UpdatedAt: c.clock.Now(),
		},
	}
	c.environments[c.environment] = obj
	if c.environmentStore == nil {
		return nil
	}
	version, err := c.environmentStore.WriteEnvironment(ctx, obj, 0, MutationEnvironmentCreated)
	if err != nil {
		return err
	}
	obj.Status.Version = version
	c.environments[c.environment] = obj
	return nil
}

// EnvironmentOwner is the subject the control plane's own environment is
// owned by. It is no person: the object describes the process, and every
// mutation of it is an administrator's.
const EnvironmentOwner = "controller"

// ListEnvironments is every environment this control plane holds, ordered by
// name.
func (c *Controller) ListEnvironments() []v1.Environment {
	c.envMu.RLock()
	defer c.envMu.RUnlock()
	out := make([]v1.Environment, 0, len(c.environments))
	for _, obj := range c.environments {
		out = append(out, cloneEnvironment(obj))
	}
	slices.SortFunc(out, func(a, b v1.Environment) int {
		return cmp.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	return out
}

// GetEnvironment reads one environment by name. The empty name asks for the
// one a manifest with no spec.environment resolves against.
func (c *Controller) GetEnvironment(name string) (v1.Environment, error) {
	obj, held := c.environmentOf(cmp.Or(name, c.environment))
	if !held {
		return v1.Environment{}, ErrNoEnvironment
	}
	return cloneEnvironment(obj), nil
}

// ApplyAttempts is how many times an apply that named no version re-reads and
// writes again before it reports a conflict. Design 008's rule: a caller that
// does not care about concurrency never sees one in practice.
const ApplyAttempts = 3

// ApplyEnvironment writes one resolved Environment and returns it with
// whether it was created. ifVersion is the version an If-Match named; zero is
// a read-modify-write, retried up to ApplyAttempts times against the version
// the store holds.
func (c *Controller) ApplyEnvironment(ctx context.Context, obj v1.Environment, ifVersion int64) (v1.Environment, bool, error) {
	if ifVersion > 0 {
		return c.applyEnvironment(ctx, obj, ifVersion)
	}
	var (
		out     v1.Environment
		created bool
		err     error
	)
	for range ApplyAttempts {
		out, created, err = c.applyEnvironment(ctx, obj, 0)
		if !errors.Is(err, ErrVersionConflict) {
			return out, created, err
		}
	}
	return out, created, err
}

// applyEnvironment is one attempt: the driver is built before the write, so
// an environment whose driver cannot be made is refused rather than stored
// with nothing serving it.
func (c *Controller) applyEnvironment(ctx context.Context, obj v1.Environment, ifVersion int64) (v1.Environment, bool, error) {
	if c.environmentStore == nil {
		return obj, false, ErrNoEnvironmentStore
	}
	if obj.Spec.Mode == v1.EnvironmentInprocess || obj.Metadata.Name == c.environment {
		// The in-process environment is the control plane's own object: it
		// describes the driver this process opened, and no caller applies
		// one. Its own fields are edited through the same route, which is
		// the branch below.
		if obj.Metadata.Name != c.environment || obj.Spec.Mode != v1.EnvironmentInprocess {
			return obj, false, ErrEnvironmentReserved
		}
	}
	c.envMu.Lock()
	defer c.envMu.Unlock()
	existing, held := c.environments[obj.Metadata.Name]
	now := c.clock.Now()
	obj.Status = v1.EnvironmentStatus{
		ID: obj.Metadata.Name, Owner: obj.Status.Owner, Phase: v1.EnvironmentPending,
		CreatedAt: now, UpdatedAt: now,
	}
	mutation := MutationEnvironmentCreated
	if held {
		obj.Status = existing.Status
		obj.Status.UpdatedAt = now
		mutation = MutationEnvironmentUpdated
	}
	if ifVersion == 0 {
		ifVersion = obj.Status.Version
	}
	d, err := c.driverOfEnvironment(obj)
	if err != nil {
		return obj, false, err
	}
	version, err := c.environmentStore.WriteEnvironment(ctx, obj, ifVersion, mutation)
	if err != nil {
		return obj, false, err
	}
	obj.Status.Version = version
	c.environments[obj.Metadata.Name] = obj
	if d != nil {
		c.drivers[obj.Metadata.Name] = d
	}
	c.emitEnvironment(ctx, mutation, obj)
	// An apply may raise the capacity a queue waits on or change the mode,
	// which the next pass reads.
	c.wakeScheduler()
	return cloneEnvironment(obj), !held, nil
}

// driverOfEnvironment is the driver one applied environment is served by: the
// one this process opened for its own, and a new one from the caller's seam
// for a worker's. A driver already registered is kept, so an edit of an
// environment does not drop the workers connected to it.
func (c *Controller) driverOfEnvironment(obj v1.Environment) (driver.Driver, error) {
	if held := c.drivers[obj.Metadata.Name]; held != nil {
		return held, nil
	}
	if c.newDriver == nil {
		return nil, ErrNoEnvironmentStore
	}
	return c.newDriver(obj)
}

// DeleteEnvironment drops one environment and the driver serving it. One a
// sandbox is placed on is refused rather than left with sandboxes nothing
// drives, unless it is a worker environment: its sandboxes' records are
// dropped with it, and the worker, told that the environment may not be
// served, stops and deletes what its driver holds (spec 083). The control
// plane's own environment is never deleted.
func (c *Controller) DeleteEnvironment(ctx context.Context, name string) error {
	if c.environmentStore == nil {
		return ErrNoEnvironmentStore
	}
	obj, held := c.environmentOf(name)
	worker := held && obj.Spec.Mode == v1.EnvironmentWorker
	// The controller's lock comes before the registry's, which is the order
	// every call site takes them in.
	c.mu.Lock()
	var placed []v1.Sandbox
	for _, o := range c.objects {
		if o.Status.Environment == name {
			placed = append(placed, clone(o))
		}
	}
	if len(placed) > 0 && !worker {
		c.mu.Unlock()
		return ErrEnvironmentInUse
	}
	if name == c.environment {
		c.mu.Unlock()
		return ErrEnvironmentReserved
	}
	for i := range placed {
		if err := c.dropLocked(ctx, &placed[i]); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	c.envMu.Lock()
	defer c.envMu.Unlock()
	obj, held = c.environments[name]
	if !held {
		return ErrNoEnvironment
	}
	if err := c.environmentStore.RemoveEnvironment(ctx, name, MutationEnvironmentDeleted); err != nil {
		return err
	}
	delete(c.environments, name)
	delete(c.drivers, name)
	// The streams the environment's workers opened end with the object, so a
	// worker of an environment that no longer exists is not left holding one.
	if c.releaseDriver != nil {
		c.releaseDriver(obj)
	}
	c.emitEnvironment(ctx, MutationEnvironmentDeleted, obj)
	return nil
}

// ReasonEnvironmentDeleted is a sandbox whose worker environment was deleted
// with it on: its record is gone, and its worker deletes what it ran.
const ReasonEnvironmentDeleted = "EnvironmentDeleted"

// dropLocked ends one sandbox's record without asking its driver, which is
// its worker's to delete once it learns its environment is gone. It runs
// under the controller's lock.
func (c *Controller) dropLocked(ctx context.Context, obj *v1.Sandbox) error {
	obj.Status.Phase = PhaseDeleting
	obj.Status.Reason = ReasonEnvironmentDeleted
	if err := c.persist(ctx, *obj, MutationDeleting); err != nil {
		return err
	}
	return c.forgetOne(ctx, obj)
}

// admits reports whether one environment takes a sandbox now. Spec 021 places
// on Ready alone: an environment below it refuses a create and leaves the
// sandboxes it already holds running.
func (c *Controller) admits(environment string) error {
	obj, held := c.environmentOf(environment)
	if !held {
		// A control plane that stores no environment of its own places on
		// the driver it opened, which is every deployment until an
		// operator applies one.
		if environment == c.environment {
			return nil
		}
		return ErrNoEnvironment
	}
	if obj.Status.Phase != v1.EnvironmentReady {
		return fmt.Errorf("%w: %s is %s", ErrEnvironmentUnavailable, environment, obj.Status.Phase)
	}
	return nil
}

// RunEnvironments ticks the phase loop until ctx ends. Each tick runs only
// while this replica holds the environments lease, so one writer computes one
// environment's phase (spec 010).
func (c *Controller) RunEnvironments(ctx context.Context) {
	ticks, stop := c.clock.Ticker(EnvironmentInterval)
	defer stop()
	c.environmentTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.environmentTick(ctx)
		}
	}
}

func (c *Controller) environmentTick(ctx context.Context) {
	held, err := c.lease.Acquire(ctx, EnvironmentsLease, LeaseTTL)
	c.metrics.LeaseHeld(MetricLeaseEnvironments, err == nil && held)
	if err != nil {
		c.log.WarnContext(ctx, "environments lease unavailable", "lease", EnvironmentsLease, "err", err)
		return
	}
	if !held {
		return
	}
	if err := c.Phases(ctx); err != nil && !errors.Is(err, ErrClosed) {
		c.log.WarnContext(ctx, "environment phase tick incomplete", "err", err)
	}
}

// Phases computes and writes every environment's phase once. It is one pass
// of the loop, called directly by the tests that drive the machine under a
// fake clock.
func (c *Controller) Phases(ctx context.Context) error {
	if !c.observe() {
		return ErrClosed
	}
	defer c.observing.Done()
	var failed error
	for _, name := range c.environmentsHeld() {
		if err := c.phaseOf(ctx, name); err != nil {
			failed = errors.Join(failed, fmt.Errorf("the phase of %s: %w", name, err))
		}
	}
	return failed
}

// ObserveEnvironment writes one environment's observed half now, as a tick of
// the phase loop would: a worker's registration and the end of its stream
// change what the environment reports, and a reader right after either must
// not wait for the next tick to see it. Like the loop, it writes only while
// this replica holds the environments lease, and an environment this
// control plane holds no object of is left as it is.
func (c *Controller) ObserveEnvironment(ctx context.Context, name string) error {
	if !c.observe() {
		return ErrClosed
	}
	defer c.observing.Done()
	held, err := c.lease.Acquire(ctx, EnvironmentsLease, LeaseTTL)
	if err != nil {
		return fmt.Errorf("the environments lease: %w", err)
	}
	if !held {
		return nil
	}
	return c.phaseOf(ctx, name)
}

// observe admits one environment status write, or refuses it once Close has
// begun. A write it admits is counted until the caller marks it done, and
// Close waits for it.
func (c *Controller) observe() bool {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.closed {
		return false
	}
	c.observing.Add(1)
	return true
}

// phaseOf writes one environment's observed half: what its data plane
// reports, the phase that follows, and the record of a transition.
func (c *Controller) phaseOf(ctx context.Context, name string) error {
	c.phaseMu.Lock()
	defer c.phaseMu.Unlock()
	obj, held := c.environmentOf(name)
	if !held {
		return nil
	}
	d, err := c.driverFor(name)
	if errors.Is(err, ErrNoEnvironment) {
		// The object is held and nothing drives it, which is a control plane
		// built with no seam for a worker's driver. There is nothing to
		// observe, so the phase the object was applied with stands and every
		// act on a sandbox of it is ErrNoEnvironment.
		return nil
	}
	if err != nil {
		return err
	}
	next := obj
	next.Status.Driver = d.Name()
	next.Status.Isolation = d.Isolation()
	next.Status.Capabilities = d.Capabilities()
	workers, last, reported := c.registrationsOf(name)
	next.Status.Workers, next.Status.LastHeartbeat = workers, last
	next.Status.Hosts = c.hostsOf(name)
	// Spec 021 records the driver from the first registration: what an
	// environment runs is what its workers run, and `remote` is only how the
	// control plane reaches them.
	if reported != "" {
		next.Status.Driver = reported
	}
	// The gateways are the ones connected to this control plane's hub, which
	// serves the environment cellad drives itself (spec 018).
	if c.egress != nil && name == c.environment {
		next.Status.Gateways = c.egress.Connected()
	}
	next.Status.Phase, next.Status.Reason = c.phase(ctx, obj, d, workers)
	if sameObserved(obj.Status, next.Status) {
		return nil
	}
	next.Status.UpdatedAt = c.clock.Now()
	mutation := transitionMutation(obj.Status.Phase, next.Status.Phase)
	c.envMu.Lock()
	c.environments[name] = next
	c.envMu.Unlock()
	if c.environmentStore != nil {
		if err := c.environmentStore.WriteEnvironmentStatus(ctx, next, mutation); err != nil {
			return err
		}
	}
	if mutation != "" {
		c.emitEnvironment(ctx, mutation, next)
	}
	// An environment that came back to Ready places again, and what waits
	// in its queues need not wait for the next tick to learn it.
	if next.Status.Phase == v1.EnvironmentReady && obj.Status.Phase != v1.EnvironmentReady {
		c.wakeScheduler()
	}
	return nil
}

// phase is the machine of spec 021 read at one instant. The in-process
// environment answers from its driver's own readiness and a worker's from the
// heartbeats its workers sent; either is Offline once nothing has answered
// for the offline window.
func (c *Controller) phase(ctx context.Context, obj v1.Environment, d driver.Driver, workers int) (string, string) {
	if obj.Spec.Mode == v1.EnvironmentInprocess {
		if err := d.Ready(ctx); err != nil {
			return c.lapsed(obj, v1.EnvironmentReady, v1.ReasonDriverNotReady)
		}
		c.markAnswered(obj.Metadata.Name)
		return v1.EnvironmentReady, ""
	}
	if workers > 0 {
		c.markAnswered(obj.Metadata.Name)
		return v1.EnvironmentReady, ""
	}
	if obj.Status.Phase == "" || obj.Status.Phase == v1.EnvironmentPending {
		// Nothing has ever registered, so there is no heartbeat to have
		// lost: the environment is applied and waiting for its data plane.
		return v1.EnvironmentPending, ""
	}
	return c.lapsed(obj, v1.EnvironmentPending, v1.ReasonHeartbeatLost)
}

// lapsed holds an environment at its phase until the offline window has
// passed with no answer, then moves it to Offline with the reason given. The
// window is what keeps one missed heartbeat from emptying an environment.
func (c *Controller) lapsed(obj v1.Environment, pending, reason string) (string, string) {
	name := obj.Metadata.Name
	c.answerMu.Lock()
	since, seen := c.answered[name]
	if !seen {
		since = c.clock.Now()
		c.answered[name] = since
	}
	c.answerMu.Unlock()
	if c.clock.Now().Before(since.Add(c.offline)) {
		if obj.Status.Phase == v1.EnvironmentOffline {
			return v1.EnvironmentOffline, obj.Status.Reason
		}
		return cmp.Or(obj.Status.Phase, pending), obj.Status.Reason
	}
	return v1.EnvironmentOffline, reason
}

// markAnswered records the instant an environment last reported, which the
// offline window is counted from.
func (c *Controller) markAnswered(name string) {
	c.answerMu.Lock()
	c.answered[name] = c.clock.Now()
	c.answerMu.Unlock()
}

// registrationsOf is how many workers of one environment hold a stream open
// or have just registered to open one, when the most recent was heard from,
// and the driver they run. A worker that registered and dropped its stream is
// not counted: nothing can be placed through it.
func (c *Controller) registrationsOf(name string) (int, time.Time, string) {
	if c.registrations == nil {
		return 0, time.Time{}, ""
	}
	count := 0
	var last time.Time
	driverName := ""
	for _, w := range c.registrations(name) {
		if !w.Connected && !w.Pending {
			continue
		}
		count++
		if driverName == "" {
			driverName = w.Driver
		}
		if w.LastHeartbeat.After(last) {
			last = w.LastHeartbeat
		}
	}
	return count, last, driverName
}

// streamOpening reports whether a worker environment is served by nothing
// but registrations whose streams have not opened yet: it reads Ready, and
// no operation can reach a worker of it until one of those streams opens. A
// registration stops being pending once its window passes with no stream,
// so this ends either way.
func (c *Controller) streamOpening(name string) bool {
	obj, held := c.environmentOf(name)
	if !held || obj.Spec.Mode != v1.EnvironmentWorker || c.registrations == nil {
		return false
	}
	pending := false
	for _, w := range c.registrations(name) {
		if w.Connected {
			return false
		}
		pending = pending || w.Pending
	}
	return pending
}

// streamRetry is how soon the scheduler takes again a sandbox it kept placed
// while its environment's worker was opening its stream.
const streamRetry = 100 * time.Millisecond

// hostsOf is the host of every worker of one environment that registrationsOf
// counts and that reports one, in the order the registrations list them,
// which is status.hosts (spec 082).
func (c *Controller) hostsOf(name string) []v1.HostStatus {
	if c.registrations == nil {
		return nil
	}
	var out []v1.HostStatus
	for _, w := range c.registrations(name) {
		if (!w.Connected && !w.Pending) || w.Host == nil {
			continue
		}
		out = append(out, v1.HostStatus{Worker: w.Worker, Host: cloneHost(*w.Host)})
	}
	return out
}

// cloneHost copies one host's lists, so the registry never shares them.
func cloneHost(h v1.HostInfo) v1.HostInfo {
	h.Grants = slices.Clone(h.Grants)
	h.Network.Hosts = slices.Clone(h.Network.Hosts)
	return h
}

// sameHosts reports whether two status.hosts lists say the same thing.
func sameHosts(a, b []v1.HostStatus) bool {
	return slices.EqualFunc(a, b, func(x, y v1.HostStatus) bool { return x.Worker == y.Worker && x.Host.Equal(y.Host) })
}

// sameObserved reports whether the loop found nothing new, so a tick that
// changed nothing writes nothing.
func sameObserved(a, b v1.EnvironmentStatus) bool {
	return a.Phase == b.Phase && a.Reason == b.Reason && a.Workers == b.Workers && sameHosts(a.Hosts, b.Hosts) &&
		a.Gateways == b.Gateways && a.LastHeartbeat.Equal(b.LastHeartbeat) &&
		a.Driver == b.Driver && a.Isolation == b.Isolation && a.Capabilities.Equal(b.Capabilities)
}

// transitionMutation is the record one phase change produces, and the empty
// string for a status write design 009 names no type for.
func transitionMutation(from, to string) string {
	switch {
	case from == to:
		return ""
	case to == v1.EnvironmentReady && from == v1.EnvironmentOffline:
		// Spec 021's table: an environment that comes back is an update,
		// and only a data plane arriving for the first time is a
		// registration.
		return MutationEnvironmentUpdated
	case to == v1.EnvironmentReady:
		return MutationEnvironmentRegistered
	case to == v1.EnvironmentOffline:
		return MutationEnvironmentOffline
	}
	return ""
}

// emitEnvironment hands one act on an environment to the emitter, where there
// is one. A store that journals inside its own transaction records it there
// instead, which is the rule every other kind follows.
func (c *Controller) emitEnvironment(ctx context.Context, mutation string, obj v1.Environment) {
	if c.events == nil {
		return
	}
	c.events.EmitEnvironment(ctx, EnvironmentAct{
		Type: mutation, Object: obj,
		Workers: obj.Status.Workers, Reason: obj.Status.Reason,
	})
}

// EnvironmentAct is one thing the control plane did to one environment: the
// type of design 009's record, the object as it stands, and the two fields
// spec 021 gives the record beyond the object.
type EnvironmentAct struct {
	Type    string
	Object  v1.Environment
	Workers int
	Reason  string
}

// environmentLookup is the controller as design 003's resolver reads it: the
// environments it holds, answered by name, with the empty name asking for the
// one a manifest that names none resolves against.
type environmentLookup struct{ c *Controller }

// Lookup is the resolver's view of this control plane's environments. The
// secret half is the caller's, composed with manifest.WithSecrets.
func (c *Controller) Lookup() manifest.Lookup { return environmentLookup{c} }

func (l environmentLookup) Environment(_ context.Context, name string) (*v1.Environment, error) {
	obj, err := l.c.GetEnvironment(name)
	if err != nil {
		return nil, manifest.ErrNotFound
	}
	// The resolver reads the isolation class and the capabilities off the
	// status, which the phase loop writes. An environment the loop has not
	// reached yet answers from its driver, so the first apply after a start
	// resolves against what the driver provides rather than against nothing.
	if obj.Status.Isolation == "" {
		obj.Status.Isolation = l.c.IsolationOf(obj.Metadata.Name)
		obj.Status.Driver = l.c.DriverNameOf(obj.Metadata.Name)
		obj.Status.Capabilities = l.c.CapabilitiesOf(obj.Metadata.Name)
	}
	// The hosts are read live rather than from the last tick, so a manifest
	// naming a host folder resolves against the workers connected now: a
	// worker that just registered grants its folders at once, and one that
	// left grants nothing (spec 082).
	if l.c.registrations != nil {
		obj.Status.Hosts = l.c.hostsOf(obj.Metadata.Name)
	}
	return &obj, nil
}

func (environmentLookup) Secret(context.Context, string) (*v1.Secret, error) {
	return nil, manifest.ErrNotFound
}

func (environmentLookup) Volume(context.Context, string) (*v1.Volume, error) {
	return nil, manifest.ErrNotFound
}

// cloneEnvironment copies every reference one environment holds, so a caller
// never writes through the registry's own object.
func cloneEnvironment(obj v1.Environment) v1.Environment {
	obj.Metadata.Labels = maps.Clone(obj.Metadata.Labels)
	obj.Metadata.Annotations = maps.Clone(obj.Metadata.Annotations)
	obj.Spec.Scheduling.Queues = slices.Clone(obj.Spec.Scheduling.Queues)
	if obj.Spec.Pool.Display != nil {
		display := *obj.Spec.Pool.Display
		obj.Spec.Pool.Display = &display
	}
	obj.Status.Capabilities.Egress = slices.Clone(obj.Status.Capabilities.Egress)
	if obj.Status.Hosts != nil {
		hosts := make([]v1.HostStatus, len(obj.Status.Hosts))
		for i, h := range obj.Status.Hosts {
			hosts[i] = v1.HostStatus{Worker: h.Worker, Host: cloneHost(h.Host)}
		}
		obj.Status.Hosts = hosts
	}
	return obj
}
