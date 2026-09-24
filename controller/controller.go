// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package controller coordinates durable desired state with one direct driver.
package controller

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The phases the controller writes that no driver reports, from the machine of
// design 005. Lost is a desired sandbox with no observed counterpart, and
// Recovering is one being recreated from its desired state.
const (
	PhaseFailed     = "Failed"
	PhaseDeleting   = "Deleting"
	PhaseLost       = "Lost"
	PhaseRecovering = "Recovering"
	// PhaseQueued is a sandbox a queued environment holds until it fits. No
	// driver holds it (spec 057).
	PhaseQueued = "Queued"
)

var (
	ErrNotFound  = errors.New("sandbox not found")
	ErrNameTaken = errors.New("sandbox name is already in use")
	ErrQuota     = errors.New("sandbox count limit reached")
	ErrPhase     = errors.New("sandbox phase does not allow this operation")
	// ErrVersionConflict is a conditional write whose row moved since it was
	// read. Design 008 answers it with 409 version_conflict.
	ErrVersionConflict = errors.New("the object changed since it was read")
)

type Options struct {
	DataDir     string
	Store       Store
	Driver      driver.Driver
	Environment string
	// Clock, Lease and Log are the reaper's collaborators of design 005.
	// Each is optional: the wall clock, the in-process lease and the
	// default logger are what one cellad runs on.
	Clock Clock
	Lease Lease
	Log   *slog.Logger
	// ReapInterval is how often the lifecycle rules run and TouchInterval
	// how often one sandbox's activity reaches the driver. Zero takes the
	// default; CELLA_REAP_INTERVAL and CELLA_TOUCH_INTERVAL set them.
	ReapInterval  time.Duration
	TouchInterval time.Duration
	// Lifecycle is the environment's default deadline set, applied to a
	// sandbox whose manifest sets no lifecycle field at all.
	Lifecycle driver.Lifecycle
	// LostGrace is how long a sandbox the driver no longer has is held as
	// Lost before it is deleted, where the store is not durable. Zero takes
	// the default; CELLA_LOST_GRACE sets it. Where the store is durable the
	// sandbox is recovered instead and no grace is counted.
	LostGrace time.Duration
	// RecoveryAttempts is how many times a lost sandbox is recreated before
	// it is Failed with reason RecoveryExhausted. Zero takes the default.
	RecoveryAttempts int
	// Egress is the environment's connected gateways. It is optional: with
	// none, a sandbox whose boundary needs a gateway is refused and one
	// that needs none is created with EgressEnforced false (spec 018).
	Egress Egress
	// Gateway is where sandboxes of this environment reach the gateway's
	// two doors, from CELLA_GATEWAY and CELLA_GATEWAY_REVERSE.
	Gateway GatewayAddresses
	// Events is design 009's emission seam, set only where the store keeps
	// no journal of its own. See the Events interface.
	Events Events
	// Tokens mints and revokes the identity every sandbox carries (spec
	// 006). It is optional: with none, no sandbox is given a token and no
	// driver projects one.
	Tokens Tokens
	// Retention forgets what the store keeps only for a window: finished
	// journal records and answered worker operations. The reaper's tick
	// runs it; nil prunes nothing.
	Retention Retention
	// Metrics is design 017's recorder. It is optional: with none the
	// controller counts nothing and behaves the same.
	Metrics Metrics
	// Pool is the environment's prewarmed set: how many entries to keep and
	// what shape they are (spec 020). Size zero runs no pool, which is
	// every environment until an operator asks for one. A size above zero
	// needs a driver that declares the Pool capability.
	Pool v1.PoolSpec
	// Capacity is the ceiling on sandboxes of this environment, which pool
	// entries count against. Zero is no ceiling.
	Capacity int
	// PoolInFlight is how many entries one refill tick prewarms and
	// PoolGrace how long an entry is left alone before the deletion rules
	// read it. Zero takes the defaults.
	PoolInFlight int
	PoolGrace    time.Duration
	// CapacityQuantities are the cpu, memory and disk the environment this
	// cellad drives declares beside Capacity's count, from CELLA_CAPACITY_*.
	// An empty triple with a zero count seeds the word auto: the ceiling is
	// the cluster's or the host's and this control plane does not read it.
	CapacityQuantities v1.Capacity
	// ScheduleInterval is how often the scheduler loop passes over the
	// queued environments when nothing wakes it; zero is the default.
	ScheduleInterval time.Duration
	// SchedulingMode is that environment's placement mode, from
	// CELLA_SCHEDULING_MODE. Empty takes direct.
	SchedulingMode string
	// MaxPreemptions is how many times one sandbox may be stopped to place
	// one of higher priority before it is no longer a victim, which bounds
	// how long a preemptible sandbox can be kept from running. Zero takes
	// DefaultMaxPreemptions and NoPreemptions makes no sandbox a victim;
	// CELLA_MAX_PREEMPTIONS sets it.
	MaxPreemptions int
	// NewDriver builds the driver of an environment a worker serves (spec
	// 021). It is optional: with none, a control plane serves the
	// environment it drives itself and refuses to apply another.
	NewDriver NewDriverFunc
	// ReleaseDriver ends what a deleted environment's driver held, which is
	// the streams its workers opened. It is optional and pairs with
	// NewDriver.
	ReleaseDriver func(obj v1.Environment)
	// Registrations reports the workers holding one environment's stream,
	// which is what the phase loop computes from. It is optional: with
	// none, no environment reports a worker.
	Registrations func(environment string) []Registration
	// EnvironmentOffline is how long an environment is held at its phase
	// with nothing answering before it is Offline. Zero takes the default;
	// CELLA_ENVIRONMENT_OFFLINE sets it.
	EnvironmentOffline time.Duration
}
type Controller struct {
	mu          sync.Mutex
	store       Store
	durable     Durable
	recovers    bool
	environment string
	// envMu guards the registry below: the environments this control plane
	// holds and the driver serving each. It is taken after the controller's
	// lock and never before it, so every call site reaches one lookup
	// whether it already holds the controller's lock or not.
	envMu         sync.RWMutex
	environments  map[string]v1.Environment
	drivers       map[string]driver.Driver
	objects       map[string]v1.Sandbox
	clock         Clock
	lease         Lease
	log           *slog.Logger
	reapInterval  time.Duration
	touchInterval time.Duration
	lifecycle     driver.Lifecycle
	touchMu       sync.Mutex
	touched       map[string]time.Time
	// The lost rule's own state, which lives in the process because the
	// grace it counts protects an intent that also lives in the process:
	// when the sandbox was first seen missing, how many recreations it has
	// had, and when the next one is due.
	lostGrace        time.Duration
	recoveryAttempts int
	lost             map[string]time.Time
	attempts         map[string]int
	retry            map[string]time.Time
	egress           Egress
	gateway          GatewayAddresses
	events           Events
	tokens           Tokens
	retention        Retention
	metrics          Metrics
	// secrets is the Secret kind's store, and secretObjects this process's
	// copy of the collection. Neither holds a value: what is here is what a
	// read returns, and a plaintext is read per compile through the seam.
	secrets       Secrets
	secretObjects map[string]v1.Secret
	// spawner is the spawn ledger of design 022 and journaller the seam a
	// record with a payload of its own is appended through. Both are the
	// store's, taken where it has them: a store that counts no budget
	// creates no child.
	spawner    Spawner
	journaller Journaller
	// The environment's pool: its shape and size, the ceiling entries count
	// against, and the two bounds of the refill loop (spec 020).
	pool         v1.PoolSpec
	capacity     int
	poolInFlight int
	poolGrace    time.Duration
	// The scheduler loop's tick and the wake a released sandbox sends it
	// (spec 057), and how many times one sandbox may be preempted.
	scheduleInterval time.Duration
	wake             chan struct{}
	maxPreemptions   int
	// environmentStore is the Environment kind's store, taken where the
	// store has it; newDriver builds the driver of an environment a worker
	// serves, and registrations reports what its workers sent.
	environmentStore Environments
	newDriver        NewDriverFunc
	releaseDriver    func(v1.Environment)
	registrations    func(string) []Registration
	// offline is how long an environment is held at its phase with nothing
	// answering, and answered when each last answered. The instant lives in
	// the process because the window it bounds is this replica's own view of
	// a data plane.
	offline  time.Duration
	answerMu sync.Mutex
	answered map[string]time.Time
	// realizing is every sandbox whose driver create the scheduler loop has
	// in flight with the controller's lock released. An act that would reach
	// the driver for one of them waits for the settle instead of crossing it.
	realizing map[string]struct{}
}

// Open restores desired state from an operator-supplied store, or the provisional
// single-process local file store when DataDir is set. Store is the seam for
// the transactional memory and Postgres adapters specified by design 010.
func Open(ctx context.Context, o Options) (*Controller, error) {
	if o.Driver == nil || o.Environment == "" || (o.Store == nil && o.DataDir == "") {
		return nil, errors.New("controller requires a driver, store or data directory, and environment")
	}
	store := o.Store
	var err error
	if store == nil {
		store, err = OpenFileStore(o.DataDir)
		if err != nil {
			return nil, err
		}
	}
	objects, err := store.Load()
	if err == nil {
		for id, obj := range objects {
			// The environment is not compared to this process's own: one
			// control plane holds every environment it serves and routes
			// each sandbox to the driver of its own. An environment the
			// registry does not hold is ErrNoEnvironment at the act that
			// reaches for its driver, not a snapshot this process refuses
			// to open (spec 021).
			if id == "" || obj.Status.ID != id || obj.Status.Owner == "" || obj.Spec.Environment == "" {
				err = errors.New("invalid controller snapshot object")
				break
			}
		}
	}
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if objects == nil {
		objects = map[string]v1.Sandbox{}
	}
	c := &Controller{
		store: store, environment: o.Environment, objects: objects,
		environments: map[string]v1.Environment{},
		drivers:      map[string]driver.Driver{o.Environment: o.Driver},
		clock:        o.Clock, lease: o.Lease, log: logger(o.Log),
		reapInterval: o.ReapInterval, touchInterval: o.TouchInterval,
		lifecycle: o.Lifecycle, touched: map[string]time.Time{},
		lostGrace: o.LostGrace, recoveryAttempts: o.RecoveryAttempts,
		lost: map[string]time.Time{}, attempts: map[string]int{}, retry: map[string]time.Time{},
		egress: o.Egress, gateway: o.Gateway,
		events: o.Events, tokens: o.Tokens, retention: o.Retention, metrics: cmp.Or(o.Metrics, Metrics(nopMetrics{})),
		secretObjects: map[string]v1.Secret{},
		pool:          o.Pool, capacity: o.Capacity,
		poolInFlight: o.PoolInFlight, poolGrace: o.PoolGrace,
		scheduleInterval: cmp.Or(o.ScheduleInterval, DefaultScheduleInterval), wake: make(chan struct{}, 1), maxPreemptions: cmp.Or(o.MaxPreemptions, DefaultMaxPreemptions),
		newDriver: o.NewDriver, releaseDriver: o.ReleaseDriver, registrations: o.Registrations,
		offline: o.EnvironmentOffline, answered: map[string]time.Time{},
		realizing: map[string]struct{}{},
	}
	// A store of design 010 takes one conditional write per object and
	// carries the journal; one that is also durable is what lets the lost
	// rule recover a sandbox rather than reap it.
	if d, ok := store.(Durable); ok {
		c.durable = d
		c.recovers = d.Durable()
	}
	// A store that counts the spawn budget is what makes a sandbox able to
	// create one; one that does not serves every other act.
	if sp, ok := store.(Spawner); ok {
		c.spawner = sp
	}
	if j, ok := store.(Journaller); ok {
		c.journaller = j
	}
	// A store that holds the Secret kind is what makes /v1/secrets and
	// substitution possible; one that does not serves everything else.
	if s, ok := store.(Secrets); ok {
		c.secrets = s
		if c.secretObjects, err = s.LoadSecrets(); err != nil {
			_ = store.Close()
			return nil, err
		}
		if c.secretObjects == nil {
			c.secretObjects = map[string]v1.Secret{}
		}
	}
	if c.clock == nil {
		c.clock = wallClock{}
	}
	if c.lease == nil {
		c.lease = LocalLease{}
	}
	if c.reapInterval <= 0 {
		c.reapInterval = DefaultReapInterval
	}
	if c.touchInterval <= 0 {
		c.touchInterval = DefaultTouchInterval
	}
	if c.lostGrace <= 0 {
		c.lostGrace = DefaultLostGrace
	}
	if c.recoveryAttempts <= 0 {
		c.recoveryAttempts = DefaultRecoveryAttempts
	}
	if c.poolInFlight <= 0 {
		c.poolInFlight = DefaultPoolInFlight
	}
	if c.poolGrace <= 0 {
		c.poolGrace = DefaultPoolGrace
	}
	if c.offline <= 0 {
		c.offline = DefaultEnvironmentOffline
	}
	// A store that holds the Environment kind is what makes an environment
	// desired state; one that does not serves the environment this cellad
	// drives itself and refuses to apply another.
	if e, ok := store.(Environments); ok {
		c.environmentStore = e
	}
	if err := c.openEnvironments(ctx, o); err != nil {
		_ = store.Close()
		return nil, err
	}
	// A pool on a driver that cannot hold one is a deployment that would
	// never accelerate a create and never say why, so it is refused here
	// rather than logged once a tick.
	if c.pool.Size > 0 && !o.Driver.Capabilities().Pool {
		_ = store.Close()
		return nil, fmt.Errorf("the %s driver declares no Pool capability, so this environment keeps no prewarmed entries", o.Driver.Name())
	}
	return c, nil
}

// Recovers reports what a sandbox the data plane lost gets: recreation from
// desired state, or the grace and then a delete. The start-up log says it, and
// design 001's State section is why it is said out loud.
func (c *Controller) Recovers() bool      { return c.recovers }
func (c *Controller) Close() error        { c.mu.Lock(); defer c.mu.Unlock(); return c.store.Close() }
func (c *Controller) Environment() string { return c.environment }

// persist records one object and the mutation that produced it: one
// conditional write and one journal row where the store is a Durable, the
// whole snapshot where it is not. A write that fails leaves the controller's
// map as it was, so what this process holds and what the store holds do not
// disagree.
func (c *Controller) persist(ctx context.Context, obj v1.Sandbox, mutation string) error {
	id := obj.Status.ID
	previous, held := c.objects[id]
	c.objects[id] = clone(obj)
	var err error
	if c.durable != nil {
		err = c.durable.Write(ctx, clone(obj), mutation)
	} else {
		err = c.store.Save(c.objects)
	}
	if err != nil {
		if held {
			c.objects[id] = previous
		} else {
			delete(c.objects, id)
		}
		return err
	}
	// A durable store journaled the record inside the write above. A
	// snapshot store has no journal, so the emitter takes the act here.
	if c.durable == nil {
		c.emit(ctx, mutation, clone(obj))
	}
	if held && releases(previous, &obj) {
		c.wakeScheduler()
	}
	return nil
}

// forget drops one object and records the mutation that ended it.
func (c *Controller) forget(ctx context.Context, id, mutation string) error {
	previous, held := c.objects[id]
	delete(c.objects, id)
	var err error
	if c.durable != nil {
		err = c.durable.Remove(ctx, id, mutation)
	} else {
		err = c.store.Save(c.objects)
	}
	if err != nil {
		if held {
			c.objects[id] = previous
		}
		return err
	}
	if c.durable == nil {
		c.emit(ctx, mutation, previous)
	}
	if held && releases(previous, nil) {
		c.wakeScheduler()
	}
	return nil
}

// Create writes one sandbox's desired state and answers with it, which is
// steps 1 and 2 of design 005's create order: the owner's name and count, the
// placement, and the write. A sandbox placed now answers Pending, and the
// scheduler loop runs steps 3 onward for it, the boundary, the identity, the
// driver's create and the first read, so the answer does not wait for a
// substrate that provisions and attaches storage before a workload starts.
// A queued one answers Queued and a direct environment that cannot fit it
// answers Failed NoCapacity, as before.
//
// Where the environment keeps a pool and one of its entries can carry this
// manifest, the create adopts it on this call and answers after the
// adoption: every step of design 005's order is the same act on the same id,
// and only the driver call at the end differs ([[020-scheduling-and-sets]]).
// An entry another adopter took between the match and the adoption leaves
// nothing behind, and the create takes the slow path under an id of its own,
// which it can only do before the caller has read one.
func (c *Controller) Create(ctx context.Context, obj v1.Sandbox, owner string, max int) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.create(ctx, obj, owner, max, nil)
}

// create is the body of one create under the controller's lock, with the
// spawning parent where the actor was a workload and nil where it was a
// subject. Spawn is its other caller.
func (c *Controller) create(ctx context.Context, obj v1.Sandbox, owner string, max int, parent *v1.Sandbox) (v1.Sandbox, error) {
	if owner == "" {
		return obj, errors.New("sandbox owner is required")
	}
	// The resolver's warnings say what this environment could not honor. They
	// are the caller's answer and outlive the status the controller writes.
	warnings := slices.Clone(obj.Status.Warnings)
	lifecycle, err := c.lifecycleFor(obj)
	if err != nil {
		return obj, err
	}
	started := c.clock.Now()
	environment := cmp.Or(obj.Spec.Environment, c.environment)
	if parent != nil {
		// A child runs where its parent runs. Boundary rule 8 of design 022
		// is read through the environment as well: a sandbox cannot place
		// its children anywhere its own boundary was not decided against.
		environment = parent.Status.Environment
		obj.Spec.Environment = environment
	}
	// An environment below Ready takes no sandbox and keeps the ones it
	// holds, which is the gate of spec 021's phase table.
	if err := c.admits(environment); err != nil {
		return obj, err
	}
	// Whether a gateway is connected is known now, so a boundary that needs
	// one while none is connected is refused on the request, with nothing
	// written, rather than failed once the create has answered.
	if needsGatewayFor(obj.Spec) && (c.egress == nil || c.egress.Connected() == 0) {
		return obj, ErrNoGateway
	}
	pool, _ := c.poolOf(environment)
	entries := c.poolEntries(ctx, environment)
	entry := c.matchEntry(pool, entries, obj)
	// A sandbox's place in its tree is create-time identity: the driver
	// stamps the mesh and the parent on objects it cannot rewrite, and an
	// adoption writes only the half of a sandbox a mutation may reach. A
	// spawn and a mesh root therefore take the slow path, so an adopted
	// member is never a member the substrate does not know about.
	if parent != nil || obj.Spec.Mesh.Enabled {
		entry = nil
	}
	// A pool entry is taken only by a create placed at once: one that would
	// wait behind others in its queue does not pass them by adopting.
	if c.waiting(environment, obj.Spec.Scheduling.Queue) > 0 {
		entry = nil
	}
	out, err := c.createLocked(ctx, obj, owner, max, warnings, lifecycle, entries, entry, parent)
	if entry != nil && err != nil && adoptionLost(err) {
		c.log.InfoContext(ctx, "the pool entry could not be adopted; this create takes the slow path",
			"entry", entry.ID, "err", err)
		remaining := without(entries, entry.ID)
		entry = nil
		out, err = c.createLocked(ctx, obj, owner, max, warnings, lifecycle, remaining, nil, parent)
	}
	// The pool is counted once the outcome is known. A create refused before
	// step 1 of the create order, for its owner's count, a name already
	// taken or a spent spawn budget, asked nothing of the pool or of a
	// driver, so it is neither an adoption nor a miss and has no duration;
	// one whose adoption was lost took the slow path and is a miss.
	if refused(err) {
		return out, err
	}
	c.countAdoption(pool, entry)
	c.observeCreate(out, poolResult(entry), started)
	return out, err
}

// refused reports whether a create ended before step 1 of the create order
// wrote anything: the owner's count, a name already taken, a spawn budget
// already spent, or a boundary no connected gateway can hold.
func refused(err error) bool {
	return errors.Is(err, ErrQuota) || errors.Is(err, ErrNameTaken) || errors.Is(err, ErrBudgetExhausted) ||
		errors.Is(err, ErrNoGateway)
}

// observeCreate records one create's duration where the driver was asked for
// the sandbox. One that was queued, placed for the loop, or refused for
// capacity asked no driver; the loop records it when it realizes it.
func (c *Controller) observeCreate(out v1.Sandbox, pool string, started time.Time) {
	if out.Status.Phase == PhaseQueued || out.Status.Reason == ReasonNoCapacity || placed(out) {
		return
	}
	c.metrics.SandboxCreated(pool, c.clock.Now().Sub(started))
}

// countAdoption records what the pool did for this create. An environment
// that keeps no pool is not a miss: there was nothing to hit.
func (c *Controller) countAdoption(pool v1.PoolSpec, entry *driver.State) {
	switch {
	case entry != nil:
		c.metrics.PoolAdoption(MetricAdopted)
	case pool.Size > 0:
		c.metrics.PoolAdoption(MetricMiss)
	}
}

// poolResult is the create's own label: what the driver was asked for.
func poolResult(entry *driver.State) string {
	if entry != nil {
		return PoolHit
	}
	return PoolMiss
}

// createLocked is design 005's create order over one placement: an entry to
// adopt, or nothing and a driver create. It runs under the controller's lock
// and is called at most twice per create, the second time with no entry.
func (c *Controller) createLocked(ctx context.Context, obj v1.Sandbox, owner string, max int,
	warnings []string, lifecycle driver.Lifecycle, entries []driver.State, entry *driver.State,
	parent *v1.Sandbox,
) (v1.Sandbox, error) {
	// An adopted sandbox takes the entry's id. The id is the object's name
	// on a container driver, which cannot be renamed, so carrying it forward
	// is what makes the boundary's principal, the token's subject and the
	// driver's stamped identity name one thing.
	// The environment the manifest names is where this sandbox is placed,
	// and the driver serving it is what every step below calls. A manifest
	// that named none is on the environment cellad drives itself.
	environment := cmp.Or(obj.Spec.Environment, c.environment)
	d, err := c.driverFor(environment)
	if err != nil {
		return obj, err
	}
	var id string
	if entry != nil {
		id = entry.ID
	} else {
		if id, err = newID(); err != nil {
			return obj, err
		}
	}
	if obj.Metadata.Name == "" {
		obj.Metadata.Name = "sandbox-" + id[len(id)-10:]
	}
	count := 0
	for _, v := range c.objects {
		if v.Status.Owner == owner {
			if v.Status.Phase != "Deleting" {
				count++
			}
			if v.Metadata.Name == obj.Metadata.Name {
				return obj, ErrNameTaken
			}
		}
	}
	if max > 0 && count >= max {
		return obj, ErrQuota
	}
	// Step 2 of the create order. An adoption always fits: it turns one
	// entry into one sandbox and moves nothing. A real create may not, and
	// where entries hold what it needs the oldest give it up.
	placement := placeNow
	if entry == nil {
		if placement, err = c.place(ctx, environment, obj, entries); err != nil {
			return obj, err
		}
	}
	now := time.Now().UTC()
	obj.Spec.Environment = environment
	obj.Status = v1.SandboxStatus{ID: id, Owner: owner, Environment: environment, Driver: d.Name(), Isolation: d.Isolation(), Phase: driver.Pending, CreatedAt: now, Warnings: warnings}
	if placement == placeQueued {
		obj.Status.Phase = PhaseQueued
		obj.Status.Conditions = setCondition(obj.Status.Conditions, waitingCondition(v1.ReasonQueued, now))
	}
	// The tree position is written before the object is: a child's parent,
	// root and mesh are what the debit, the boundary and the driver all read.
	if err := c.spawnStatus(&obj, parent); err != nil {
		return obj, err
	}
	// The debit is inside the write, so two spawns racing for one remaining
	// unit yield one child and one refusal.
	if err := c.debit(ctx, obj, parent); err != nil {
		return obj, err
	}
	switch placement {
	case placeQueued:
		// A queued sandbox stops here and resumes at step 3 when the loop
		// places it. The loop is woken rather than left to its tick, so a
		// head that preemption can place is placed now.
		c.wakeScheduler()
		return c.positioned(export(obj)), nil
	case placeNoCapacity:
		// A direct environment starts a sandbox now or fails it. Nothing
		// past step 1 ran, so the failure is the whole of the undo.
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonNoCapacity
		obj.Status.Conditions = setCondition(obj.Status.Conditions, waitingCondition(v1.ReasonNoCapacity, now))
		return export(obj), errors.Join(c.persist(ctx, obj, MutationFailed), c.credit(ctx, parent))
	}
	if entry == nil {
		// Steps 3 onward are the scheduler loop's. It is woken rather than
		// left to its tick, so the driver is asked as soon as this answer is
		// written.
		c.wakeScheduler()
		return export(obj), nil
	}
	return c.adopt(ctx, obj, d, lifecycle, entry)
}

// adopt is steps 3 onward of design 005's create order for a create that
// takes a pool entry, on the request that made it and under the controller's
// lock: the boundary, the identity, and the adoption. It runs before the
// answer because an adoption that loses the entry is undone whole and the
// create is made again under an id of its own, which is only possible before
// the caller has read the entry's. A spawn never adopts, so there is no
// parent's unit to credit.
func (c *Controller) adopt(ctx context.Context, obj v1.Sandbox, d driver.Driver, lifecycle driver.Lifecycle,
	entry *driver.State,
) (v1.Sandbox, error) {
	id, environment, now := obj.Status.ID, obj.Status.Environment, time.Now().UTC()
	// The undo and the record of what became of the sandbox run to the end
	// whether or not the caller is still there. A caller that hangs up mid
	// create leaves a sandbox half made, and the store, the journal and the
	// token list must still say what happened to it; the driver's own work
	// stops with the caller.
	settle := context.WithoutCancel(ctx)
	// The boundary is put in a gateway before the driver is called, so a
	// sandbox never starts before a gateway knows it (spec 018). A boundary
	// that no gateway acknowledges is a refusal here, with nothing created.
	boundary, err := c.pushEgress(ctx, &obj)
	if err != nil {
		// The map may already sit in a gateway that took the put and never
		// answered, so the principal is purged with the object: every map a
		// gateway holds is a map desired state has.
		c.purgeEgress(settle, id)
		return obj, errors.Join(err, c.forget(settle, id, MutationDeleted))
	}
	obj.Status.Secrets = boundary.Secrets
	obj.Status.Conditions = setCondition(obj.Status.Conditions, c.egressCondition(environment, boundary.Map, boundary.Held, now))
	// The identity is minted after the boundary and before the driver, which
	// is step 5 of design 005's create order: a sandbox that never starts
	// leaves a token nobody holds, and the undo below ends it.
	token, tokenState, err := c.mintToken(ctx, obj)
	if err != nil {
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonCreateFailed
		c.purgeEgress(settle, id)
		return export(obj), errors.Join(err, c.persist(settle, obj, MutationFailed))
	}
	obj.Status.TokenState = tokenState
	adoption := adoptionOf(obj, lifecycle, c.egressSpec(boundary.Map), boundary.Env, token)
	if err = d.Update(ctx, id, driver.Change{Adopt: &adoption}); err != nil {
		c.purgeEgress(settle, id)
		revoked := c.revokeToken(settle, tokenState)
		obj.Status.TokenState = nil
		if adoptionLost(err) {
			// The entry is another caller's now, or it cannot carry this
			// manifest. Nothing of this attempt survives: no map, no
			// identity and no row, so the create that follows is an
			// ordinary first create under an id of its own.
			return obj, errors.Join(err, revoked, c.forget(settle, id, MutationDeleted))
		}
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonCreateFailed
		return export(obj), errors.Join(err, revoked, c.persist(settle, obj, MutationFailed))
	}
	obj.Status.Conditions = setCondition(obj.Status.Conditions, scheduledCondition(true, c.clock.Now()))
	obj, err = c.refresh(settle, obj)
	return export(obj), errors.Join(err, c.persist(settle, obj, phaseMutation(obj.Status.Phase)))
}

// realize is steps 3 onward of design 005's create order for a sandbox the
// scheduler loop took: one a create placed and answered at once, one the loop
// placed from its queue, or one a process that stopped left placed. It is
// called under the controller's lock and returns under it.
//
// The boundary, the identity and a status write come first, under the lock.
// The write puts what the driver is about to be given in the store, so a
// second run after a crash pushes the same map with the same credential and
// placeholders and ends the token the first run minted. The driver's create
// and the first read then run with the lock released: on a substrate that
// provisions and attaches a volume before the workload starts they take as
// long as that does, and every other request would otherwise wait with them.
// The result is settled against the row as it stands after the call, which a
// delete, an apply or a spawn may have changed in the meantime.
func (c *Controller) realize(ctx context.Context, id string) error {
	obj, held := c.objects[id]
	if _, busy := c.realizing[id]; !held || busy || !placed(obj) {
		return nil
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	lifecycle, err := c.lifecycleFor(obj)
	if err != nil {
		return err
	}
	started, now, environment := c.clock.Now(), time.Now().UTC(), obj.Status.Environment
	parent := c.parentOf(obj)
	// The records of what became of the sandbox are written whether or not
	// the loop is still running, so the store never holds a half-told
	// create.
	settle := context.WithoutCancel(ctx)
	// A row a previous run wrote carries the identity that run minted. It
	// is replaced below and ended once this run has settled.
	previous := obj.Status.TokenState
	boundary, err := c.pushEgress(ctx, &obj)
	if err != nil {
		// A gateway that went away between the create's admission and this
		// pass, or one that took the put and never answered. The map may sit
		// in it, so the principal is purged with the failure.
		c.purgeEgress(settle, id)
		if errors.Is(err, ErrNoGateway) {
			obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
				Type: v1.ConditionEgressEnforced, Status: v1.ConditionFalse, Reason: v1.ReasonNoGateway, Since: now,
				Message: "No gateway of this environment acknowledged the sandbox's map, so it was not created.",
			})
		}
		return errors.Join(err, c.failCreate(settle, obj, parent, nil, previous))
	}
	obj.Status.Secrets = boundary.Secrets
	obj.Status.Conditions = setCondition(obj.Status.Conditions, c.egressCondition(environment, boundary.Map, boundary.Held, now))
	token, tokenState, err := c.mintToken(ctx, obj)
	if err != nil {
		c.purgeEgress(settle, id)
		return errors.Join(err, c.failCreate(settle, obj, parent, nil, previous))
	}
	obj.Status.TokenState = tokenState
	if err := c.persist(settle, obj, MutationStatus); err != nil {
		// The row still says what the store holds, so the next pass takes
		// it again from there, with a map and an identity of its own.
		c.purgeEgress(settle, id)
		return errors.Join(err, c.revokeToken(settle, tokenState))
	}
	spec := specOf(obj, lifecycle, c.egressSpec(boundary.Map), boundary.Env, token)
	c.realizing[id] = struct{}{}
	c.mu.Unlock()
	out := drive(ctx, d, spec, token)
	c.mu.Lock()
	delete(c.realizing, id)
	if ctx.Err() != nil {
		// The loop is stopping. The row is left as the status write left
		// it, placed and unrealized, and the next process takes it from
		// there: its driver create finds the object this one made, and its
		// settle ends the token this one minted.
		return errors.Join(ctx.Err(), out.created)
	}
	return c.settleCreate(settle, id, d, out, tokenState, previous, parent, started)
}

// driven is what the driver's half of a realize ended with: the create's
// error, and the first read with its own.
type driven struct {
	state   driver.State
	created error
	read    error
}

// drive is the part of realize that runs without the controller's lock: the
// driver's create and its first read. ErrAlreadyExists is a create that
// reached the driver before a restart and whose result was never written; ids
// are never reused, so the object is this sandbox's, and it is taken the way
// recovery takes one, with the new identity projected into it.
func drive(ctx context.Context, d driver.Driver, spec driver.CreateSpec, token string) driven {
	_, err := d.Create(ctx, spec)
	if errors.Is(err, driver.ErrAlreadyExists) {
		err = nil
		if token != "" {
			err = d.Update(ctx, spec.ID, driver.Change{Token: []byte(token)})
		}
	}
	if err != nil {
		return driven{created: err}
	}
	state, read := d.Inspect(ctx, spec.ID)
	return driven{state: state, read: read}
}

// settleCreate writes what one realize ended with, against the row as it
// stands after the driver's call. It runs under the controller's lock.
func (c *Controller) settleCreate(ctx context.Context, id string, d driver.Driver, out driven,
	token, previous *v1.TokenState, parent *v1.Sandbox, started time.Time,
) error {
	created, read := out.created, out.read
	cur, held := c.objects[id]
	switch {
	case !held:
		// A delete, a cascade or a deadline ended the sandbox during the
		// call, and its driver call may have run before the create made
		// anything. What the create made goes now, with the map and the
		// identity it was given.
		gone := d.Delete(ctx, id)
		if errors.Is(gone, driver.ErrNotFound) {
			gone = nil
		}
		c.purgeEgress(ctx, id)
		return errors.Join(gone, c.revokeToken(ctx, token), c.revokeToken(ctx, previous))
	case cur.Status.Phase == PhaseDeleting:
		// A delete is in flight and its driver call failed. The row is
		// the delete's, and the retried delete removes the object and ends
		// the identity the row names.
		return created
	case created != nil:
		c.purgeEgress(ctx, id)
		return errors.Join(created, c.failCreate(ctx, cur, parent, token, previous))
	}
	cur.Status.Conditions = setCondition(cur.Status.Conditions, scheduledCondition(false, c.clock.Now()))
	switch {
	case read == nil:
		cur = observe(cur, out.state)
	case errors.Is(read, driver.ErrNotFound):
		// The driver took the create and does not report it yet. The
		// sandbox is written Pending and handed on, and the next read says
		// what it is.
		read = nil
	}
	err := errors.Join(read, c.persist(ctx, cur, phaseMutation(cur.Status.Phase)))
	if parent != nil {
		err = errors.Join(err, c.spawned(ctx, cur, parent.Status.ID))
	}
	c.observeCreate(cur, PoolMiss, started)
	return errors.Join(err, c.revokeToken(ctx, previous))
}

// failCreate is the undo of a create the loop could not finish: the sandbox
// is Failed with CreateFailed, every identity minted for it is ended, and a
// spawn's unit is credited back to its parent. The caller has purged the map.
func (c *Controller) failCreate(ctx context.Context, obj v1.Sandbox, parent *v1.Sandbox, tokens ...*v1.TokenState) error {
	obj.Status.Phase = PhaseFailed
	obj.Status.Reason = ReasonCreateFailed
	obj.Status.TokenState = nil
	var revoked error
	for _, token := range tokens {
		revoked = errors.Join(revoked, c.revokeToken(ctx, token))
	}
	return errors.Join(revoked, c.persist(ctx, obj, MutationFailed), c.credit(ctx, parent))
}

// phaseMutation is the act the first driver read after a create observed. A
// sandbox the driver brought up is Running, and that is the transition
// design 009 names started; one that came up Failed is that transition; one
// still coming up is a status write and no event. Without this a sandbox
// that runs from creation would never say it started, and a meter that opens
// its interval there would read it as never having run.
func phaseMutation(phase string) string {
	switch phase {
	case driver.Running:
		return MutationStarted
	case PhaseFailed:
		return MutationFailed
	}
	return MutationStatus
}

// specOf derives the driver's create spec from one resolved manifest, the
// deadline set it runs under, and the boundary its gateway holds. It is the
// one place the manifest's vocabulary meets the driver's, so a create and a
// recovery of the same sandbox ask for the same object.
func specOf(obj v1.Sandbox, lifecycle driver.Lifecycle, boundary driver.Egress, secrets map[string]string, token string) driver.CreateSpec {
	return driver.CreateSpec{
		ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner, Image: obj.Spec.Image,
		Command: obj.Spec.Command, Args: obj.Spec.Args, Env: withSecretEnv(obj.Spec.Env, secrets),
		Workdir: obj.Spec.Workdir, Labels: obj.Metadata.Labels, User: obj.Spec.User,
		Resources: driver.Resources{CPU: string(obj.Spec.Resources.CPU), Memory: string(obj.Spec.Resources.Memory), Disk: string(obj.Spec.Resources.Disk)},
		Workspace: driver.Workspace{Path: obj.Spec.Workspace.Path},
		Lifecycle: lifecycle,
		Egress:    boundary,
		Display:   geometryOf(obj.Spec.Display),
		Ports:     portsOf(obj.Spec.Network.Ports),
		Mesh:      driver.Mesh{ID: obj.Status.Mesh},
		Parent:    obj.Status.Parent,
		Token:     []byte(token),
	}
}

// withSecretEnv is the manifest's own environment with each mounted secret's
// placeholder beside it, and the companion key that says where to put it.
// The placeholder is a projection and never a field of the manifest, so what
// a caller reads back is what it wrote and what the workload holds is what
// the boundary minted.
func withSecretEnv(env, secrets map[string]string) map[string]string {
	if len(secrets) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(secrets))
	maps.Copy(out, env)
	maps.Copy(out, secrets)
	return out
}

// purgeEgress drops a principal from every gateway of the environment. It
// runs on a delete and on a create whose driver refused, so no gateway holds
// a map for an object that does not exist.
func (c *Controller) purgeEgress(ctx context.Context, id string) {
	if c.egress == nil {
		return
	}
	c.egress.Purge(ctx, egress.Principal(id))
}
func (c *Controller) Get(ctx context.Context, key, owner string) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[key]
	if !ok {
		for _, v := range c.objects {
			if v.Status.Owner == owner && v.Metadata.Name == key {
				obj = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return v1.Sandbox{}, ErrNotFound
	}
	return c.positioned(export(obj)), nil
}

// List returns desired records. Runtime refresh is deferred until after API authorization.
func (c *Controller) Refresh(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	return c.refresh(ctx, clone(obj))
}
func (c *Controller) List() []v1.Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]v1.Sandbox, 0, len(c.objects))
	for _, obj := range c.objects {
		out = append(out, c.positioned(export(obj)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}
func (c *Controller) refresh(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	// Deleting is this control plane's own act in flight, and no driver
	// holds a Queued sandbox: asking one would read its absence as a loss.
	if obj.Status.Phase == PhaseDeleting || obj.Status.Phase == PhaseQueued {
		return obj, nil
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return obj, err
	}
	state, err := d.Inspect(ctx, obj.Status.ID)
	if errors.Is(err, driver.ErrNotFound) {
		if obj.Status.Phase != driver.Pending && obj.Status.Phase != "Failed" {
			obj.Status.Phase = "Lost"
		}
		return obj, nil
	}
	if err != nil {
		return obj, err
	}
	return observe(obj, state), nil
}

// observe writes one driver read onto the sandbox: the phase and its reason,
// the conditions the driver owns, the ports it probed, and its instants.
func observe(obj v1.Sandbox, state driver.State) v1.Sandbox {
	// A driver names the reason for a transition it made itself and names
	// none for one it was told to make, so a reason the controller wrote
	// stands until the phase it was written for changes. Without that the
	// first read after an autoStop would erase AutoStop.
	if state.Reason != "" || state.Phase != obj.Status.Phase {
		obj.Status.Reason = state.Reason
	}
	obj.Status.Phase = state.Phase
	obj.Status.ExitCode = state.ExitCode
	// The driver owns the conditions it writes and the probe of every
	// declared port, so a read of the sandbox carries what the environment
	// actually built rather than what the manifest asked for.
	for _, condition := range state.Conditions {
		obj.Status.Conditions = setCondition(obj.Status.Conditions, condition)
	}
	obj.Status.Ports = portStatusOf(state.Ports)
	obj.Status.StartedAt = state.StartedAt
	obj.Status.StoppedAt = state.StoppedAt
	obj.Status.LastActivityAt = state.LastActivityAt
	obj.Status.ExpiresAt = state.ExpiresAt
	return obj
}

// lifecycleFor is the deadline set one sandbox runs under: its own manifest's,
// or the environment default where the manifest names none. An explicit never
// is a set field and stays the zero duration the drivers read as no bound.
func (c *Controller) lifecycleFor(obj v1.Sandbox) (driver.Lifecycle, error) {
	lifecycle, err := lifecycleOf(obj.Spec.Lifecycle)
	if err != nil {
		return lifecycle, err
	}
	if obj.Spec.Lifecycle == (v1.Lifecycle{}) {
		return c.lifecycle, nil
	}
	return lifecycle, nil
}

// lifecycleOf turns the resolved manifest's durations into the driver's, where
// never and absent are both the zero duration the drivers read as no bound.
func lifecycleOf(l v1.Lifecycle) (driver.Lifecycle, error) {
	var out driver.Lifecycle
	for _, field := range []struct {
		value v1.Duration
		into  *time.Duration
	}{{l.AutoStop, &out.AutoStop}, {l.TTL, &out.TTL}, {l.AutoDelete, &out.AutoDelete}} {
		if field.value == "" {
			continue
		}
		value, never, err := manifest.ParseDuration(field.value)
		if err != nil {
			return out, err
		}
		if !never {
			*field.into = value
		}
	}
	return out, nil
}

// Act serializes lifecycle changes; failed deletes retain the desired record.
func (c *Controller) Act(ctx context.Context, id, verb string) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[id]
	if !ok {
		return obj, ErrNotFound
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return obj, err
	}
	// A sandbox whose create the loop has not finished has nothing a driver
	// can start or stop yet, and one whose driver call is in flight must not
	// be crossed by another: both are phase_conflict until the create
	// settles. A delete is accepted in every phase, and the create's settle
	// removes what it made.
	if _, busy := c.realizing[id]; (busy || placed(obj)) && (verb == "start" || verb == "stop") {
		return obj, ErrPhase
	}
	switch verb {
	case "start":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Stopped {
			return obj, ErrPhase
		}
		err = d.Start(ctx, id)
	case "stop":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Running {
			return obj, ErrPhase
		}
		err = d.Stop(ctx, id)
	case "delete":
		old := obj
		// The tree below this sandbox goes first, deepest generation
		// before the one above it, so no sandbox outlives the ancestor
		// that bounded it (design 022).
		if err = c.cascade(ctx, id); err != nil {
			return old, err
		}
		obj.Status.Phase = PhaseDeleting
		if err = c.persist(ctx, obj, MutationDeleting); err != nil {
			return old, err
		}
		err = c.deleteOne(ctx, &obj)
		if err == nil {
			return export(obj), nil
		}
	default:
		return obj, ErrPhase
	}
	if err != nil {
		return obj, err
	}
	obj, err = c.refresh(ctx, obj)
	if err != nil {
		return obj, err
	}
	return export(obj), c.persist(ctx, obj, verbMutation(verb))
}

// deleteOne ends one sandbox: the driver's object, the touch and lost
// bookkeeping, the gateway's map, the identity, the ledger row and the
// desired row. A sandbox the driver no longer has is already ended by the
// driver's lights, so its absence is not an error.
func (c *Controller) deleteOne(ctx context.Context, obj *v1.Sandbox) error {
	id := obj.Status.ID
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	err = d.Delete(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return err
	}
	c.forgetTouch(id)
	c.forgetLost(id)
	c.purgeEgress(ctx, id)
	// The token dies with the sandbox rather than with its own exp, so a
	// deleted sandbox's identity is refused at once (spec 006).
	revoked := c.revokeToken(ctx, obj.Status.TokenState)
	var forgotten error
	if c.spawner != nil {
		forgotten = c.spawner.ForgetSpawns(ctx, id)
	}
	return errors.Join(revoked, forgotten, c.forget(ctx, id, MutationDeleted))
}

// cascade deletes every descendant of one sandbox, deepest generation first,
// each with reason Parent. The sandbox the request named keeps the request's
// own reason and is deleted by the caller after this returns.
func (c *Controller) cascade(ctx context.Context, id string) error {
	for _, child := range c.descendants(id) {
		child.Status.Phase = PhaseDeleting
		child.Status.Reason = ReasonParent
		if err := c.persist(ctx, child, MutationDeleting); err != nil {
			return err
		}
		if err := c.deleteOne(ctx, &child); err != nil {
			return err
		}
	}
	return nil
}

// verbMutation is the act one API verb performed, which is the type of the
// record it produces.
func verbMutation(verb string) string {
	if verb == "start" {
		return MutationStarted
	}
	return MutationStopped
}
func (c *Controller) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	return d.Exec(ctx, id, req)
}

// export is the object as a caller reads it: everything clone carries, less
// the control plane's own record of the boundary. The credential in it is
// what both gateway doors authenticate, so it leaves the process only toward
// a gateway and inside the sandbox it belongs to, never in an API response.
func export(obj v1.Sandbox) v1.Sandbox {
	out := clone(obj)
	out.Status.EgressState = nil
	// The identity's record is the control plane's own, like the boundary's:
	// a caller reads what the sandbox is, not the key by which its token is
	// ended (spec 006).
	out.Status.TokenState = nil
	return out
}
func clone(obj v1.Sandbox) v1.Sandbox {
	obj.Metadata.Labels = maps.Clone(obj.Metadata.Labels)
	obj.Metadata.Annotations = maps.Clone(obj.Metadata.Annotations)
	obj.Spec.Command = slices.Clone(obj.Spec.Command)
	obj.Spec.Args = slices.Clone(obj.Spec.Args)
	obj.Spec.Env = maps.Clone(obj.Spec.Env)
	obj.Spec.Network.Egress.AllowedHosts = slices.Clone(obj.Spec.Network.Egress.AllowedHosts)
	obj.Spec.Network.Egress.DeniedHosts = slices.Clone(obj.Spec.Network.Egress.DeniedHosts)
	obj.Spec.Network.Ports = slices.Clone(obj.Spec.Network.Ports)
	obj.Spec.Secrets = slices.Clone(obj.Spec.Secrets)
	if obj.Spec.Display != nil {
		display := *obj.Spec.Display
		obj.Spec.Display = &display
	}
	obj.Status.Conditions = slices.Clone(obj.Status.Conditions)
	obj.Status.Ports = slices.Clone(obj.Status.Ports)
	obj.Status.Secrets.Mounted = slices.Clone(obj.Status.Secrets.Mounted)
	obj.Status.Secrets.NotInjectable = slices.Clone(obj.Status.Secrets.NotInjectable)
	obj.Status.Warnings = slices.Clone(obj.Status.Warnings)
	if obj.Status.EgressState != nil {
		state := *obj.Status.EgressState
		state.Secrets = slices.Clone(obj.Status.EgressState.Secrets)
		obj.Status.EgressState = &state
	}
	if obj.Status.TokenState != nil {
		state := *obj.Status.TokenState
		obj.Status.TokenState = &state
	}
	if obj.Status.ExitCode != nil {
		code := *obj.Status.ExitCode
		obj.Status.ExitCode = &code
	}
	return obj
}

// newID mints one sandbox's id: the prefix of design 001 and a ULID, so ids
// sort by the instant the sandbox was made.
func newID() (string, error) {
	raw, err := newULID()
	if err != nil {
		return "", err
	}
	return "sbx_" + crockfordULID(raw), nil
}

// newULID is the 128 bits of a ULID: the millisecond of the mint in the first
// six bytes and eighty random bits behind them.
func newULID() ([16]byte, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return b, err
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	return b, nil
}

// crockfordULID renders those bits the way every prefixed id of design 001 is
// rendered: twenty-six lowercase Crockford characters, which sort as the bits
// sort.
func crockfordULID(b [16]byte) string {
	n := new(big.Int).SetBytes(b[:])
	mask := big.NewInt(31)
	var out [26]byte
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return string(out[:])
}

// Update replaces one sandbox's desired state with what an apply resolved.
// The status is the controller's and is carried over whole: an apply writes
// the specification and never the record of what the sandbox is.
//
// The boundary reaches a gateway before desired state claims it, which is the
// order design 018 fixes for a create and holds for an update: a map no
// gateway will hold is a refusal, and the boundary the object already had is
// put back so that every map a gateway holds is a map desired state has. The
// driver is not called, because design 003 marks every field a running
// sandbox would have to be recreated for immutable.
func (c *Controller) Update(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.objects[obj.Status.ID]
	if !ok {
		return v1.Sandbox{}, ErrNotFound
	}
	obj.Status = held.Status
	boundary, err := c.pushEgress(ctx, &obj)
	if err != nil {
		previous := clone(held)
		if _, restore := c.pushEgress(ctx, &previous); restore != nil {
			err = errors.Join(err, restore)
		}
		return v1.Sandbox{}, err
	}
	obj.Status.Secrets = boundary.Secrets
	obj.Status.Conditions = setCondition(obj.Status.Conditions, c.egressCondition(obj.Status.Environment, boundary.Map, boundary.Held, time.Now().UTC()))
	if err := c.persist(ctx, obj, MutationUpdated); err != nil {
		return v1.Sandbox{}, err
	}
	return export(obj), nil
}
