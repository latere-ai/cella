// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package controller coordinates durable desired state with one direct driver.
package controller

import (
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
)

var (
	ErrNotFound  = errors.New("sandbox not found")
	ErrNameTaken = errors.New("sandbox name is already in use")
	ErrQuota     = errors.New("sandbox count limit reached")
	ErrPhase     = errors.New("sandbox phase does not allow this operation")
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
}
type Controller struct {
	mu            sync.Mutex
	store         Store
	durable       Durable
	recovers      bool
	driver        driver.Driver
	environment   string
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
	// secrets is the Secret kind's store, and secretObjects this process's
	// copy of the collection. Neither holds a value: what is here is what a
	// read returns, and a plaintext is read per compile through the seam.
	secrets       Secrets
	secretObjects map[string]v1.Secret
	// The environment's pool: its shape and size, the ceiling entries count
	// against, and the two bounds of the refill loop (spec 020).
	pool         v1.PoolSpec
	capacity     int
	poolInFlight int
	poolGrace    time.Duration
}

// Open restores desired state from an operator-supplied store, or the provisional
// single-process local file store when DataDir is set. Store is the seam for
// the transactional memory and Postgres adapters specified by design 010.
func Open(o Options) (*Controller, error) {
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
			if id == "" || obj.Status.ID != id || obj.Status.Owner == "" || obj.Spec.Environment != o.Environment {
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
		store: store, driver: o.Driver, environment: o.Environment, objects: objects,
		clock: o.Clock, lease: o.Lease, log: logger(o.Log),
		reapInterval: o.ReapInterval, touchInterval: o.TouchInterval,
		lifecycle: o.Lifecycle, touched: map[string]time.Time{},
		lostGrace: o.LostGrace, recoveryAttempts: o.RecoveryAttempts,
		lost: map[string]time.Time{}, attempts: map[string]int{}, retry: map[string]time.Time{},
		egress: o.Egress, gateway: o.Gateway,
		events: o.Events, tokens: o.Tokens,
		secretObjects: map[string]v1.Secret{},
		pool:          o.Pool, capacity: o.Capacity,
		poolInFlight: o.PoolInFlight, poolGrace: o.PoolGrace,
	}
	// A store of design 010 takes one conditional write per object and
	// carries the journal; one that is also durable is what lets the lost
	// rule recover a sandbox rather than reap it.
	if d, ok := store.(Durable); ok {
		c.durable = d
		c.recovers = d.Durable()
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
	// A pool on a driver that cannot hold one is a deployment that would
	// never accelerate a create and never say why, so it is refused here
	// rather than logged once a tick.
	if c.pool.Size > 0 && !c.driver.Capabilities().Pool {
		_ = store.Close()
		return nil, fmt.Errorf("the %s driver declares no Pool capability, so this environment keeps no prewarmed entries", c.driver.Name())
	}
	return c, nil
}

// Recovers reports what a sandbox the data plane lost gets: recreation from
// desired state, or the grace and then a delete. The start-up log says it, and
// design 001's State section is why it is said out loud.
func (c *Controller) Recovers() bool      { return c.recovers }
func (c *Controller) Close() error        { c.mu.Lock(); defer c.mu.Unlock(); return c.store.Close() }
func (c *Controller) Environment() string { return c.environment }
func (c *Controller) Isolation() string   { return c.driver.Isolation() }

// DriverName is the name of the driver serving this controller's
// environment, which an environment's status reports.
func (c *Controller) DriverName() string { return c.driver.Name() }

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
	return nil
}

// Create atomically reserves the owner's name and quota before calling runtime.
//
// Where the environment keeps a pool and one of its entries can carry this
// manifest, the create adopts it: every step of design 005's order is the
// same act on the same id, and only the driver call at the end differs
// ([[020-scheduling-and-sets]]). An entry another adopter took between the
// match and the adoption leaves nothing behind, and the create runs again on
// the slow path.
func (c *Controller) Create(ctx context.Context, obj v1.Sandbox, owner string, max int) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner == "" {
		return obj, errors.New("sandbox owner is required")
	}
	// The resolver's warnings say what this environment could not honour. They
	// are the caller's answer and outlive the status the controller writes.
	warnings := slices.Clone(obj.Status.Warnings)
	lifecycle, err := c.lifecycleFor(obj)
	if err != nil {
		return obj, err
	}
	entries := c.poolEntries(ctx)
	entry := c.matchEntry(entries, obj)
	out, err := c.createLocked(ctx, obj, owner, max, warnings, lifecycle, entries, entry)
	if entry != nil && err != nil && adoptionLost(err) {
		c.log.InfoContext(ctx, "the pool entry could not be adopted; this create takes the slow path",
			"entry", entry.ID, "err", err)
		return c.createLocked(ctx, obj, owner, max, warnings, lifecycle, without(entries, entry.ID), nil)
	}
	return out, err
}

// createLocked is design 005's create order over one placement: an entry to
// adopt, or nothing and a driver create. It runs under the controller's lock
// and is called at most twice per create, the second time with no entry.
func (c *Controller) createLocked(ctx context.Context, obj v1.Sandbox, owner string, max int,
	warnings []string, lifecycle driver.Lifecycle, entries []driver.State, entry *driver.State,
) (v1.Sandbox, error) {
	// An adopted sandbox takes the entry's id. The id is the object's name
	// on a container driver, which cannot be renamed, so carrying it forward
	// is what makes the boundary's principal, the token's subject and the
	// driver's stamped identity name one thing.
	var id string
	if entry != nil {
		id = entry.ID
	} else {
		var err error
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
	// An adoption always fits: it turns one entry into one sandbox and moves
	// nothing. A real create may not, and where entries hold the ceiling the
	// oldest give it up.
	if entry == nil {
		if err := c.makeRoom(ctx, entries); err != nil {
			return obj, err
		}
	}
	now := time.Now().UTC()
	obj.Status = v1.SandboxStatus{ID: id, Owner: owner, Environment: c.environment, Driver: c.driver.Name(), Isolation: c.driver.Isolation(), Phase: driver.Pending, CreatedAt: now, Warnings: warnings}
	if err := c.persist(ctx, obj, MutationCreated); err != nil {
		return obj, err
	}
	// The boundary is put in a gateway before the driver is called, so a
	// sandbox never starts before a gateway knows it (spec 018). A boundary
	// that no gateway will hold is a refusal here, with nothing created.
	boundary, err := c.pushEgress(ctx, &obj)
	if err != nil {
		// The map may already sit in a gateway that took the put and never
		// answered, so the principal is purged with the object: every map a
		// gateway holds is a map desired state has.
		c.purgeEgress(ctx, id)
		return obj, errors.Join(err, c.forget(ctx, id, MutationDeleted))
	}
	obj.Status.Secrets = boundary.Secrets
	obj.Status.Conditions = setCondition(obj.Status.Conditions, c.egressCondition(boundary.Map, boundary.Held, now))
	// The identity is minted after the boundary and before the driver, which
	// is step 5 of design 005's create order: a sandbox that never starts
	// leaves a token nobody holds, and the undo below ends it.
	token, tokenState, err := c.mintToken(ctx, obj)
	if err != nil {
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonCreateFailed
		c.purgeEgress(ctx, id)
		return export(obj), errors.Join(err, c.persist(ctx, obj, MutationFailed))
	}
	obj.Status.TokenState = tokenState
	if entry != nil {
		adoption := adoptionOf(obj, lifecycle, c.egressSpec(boundary.Map), boundary.Env, token)
		err = c.driver.Update(ctx, id, driver.Change{Adopt: &adoption})
	} else {
		_, err = c.driver.Create(ctx, specOf(obj, lifecycle, c.egressSpec(boundary.Map), boundary.Env, token))
	}
	if err != nil {
		c.purgeEgress(ctx, id)
		revoked := c.revokeToken(ctx, tokenState)
		obj.Status.TokenState = nil
		if entry != nil && adoptionLost(err) {
			// The entry is another caller's now, or it cannot carry this
			// manifest. Nothing of this attempt survives: no map, no
			// identity and no row, so the create that follows is an
			// ordinary first create under an id of its own.
			return obj, errors.Join(err, revoked, c.forget(ctx, id, MutationDeleted))
		}
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonCreateFailed
		return export(obj), errors.Join(err, revoked, c.persist(ctx, obj, MutationFailed))
	}
	obj.Status.Conditions = setCondition(obj.Status.Conditions, scheduledCondition(entry != nil, c.clock.Now()))
	obj, err = c.refresh(ctx, obj)
	return export(obj), errors.Join(err, c.persist(ctx, obj, phaseMutation(obj.Status.Phase)))
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
	return export(obj), nil
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
		out = append(out, export(obj))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}
func (c *Controller) refresh(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	if obj.Status.Phase == "Deleting" {
		return obj, nil
	}
	state, err := c.driver.Inspect(ctx, obj.Status.ID)
	if errors.Is(err, driver.ErrNotFound) {
		if obj.Status.Phase != driver.Pending && obj.Status.Phase != "Failed" {
			obj.Status.Phase = "Lost"
		}
		return obj, nil
	}
	if err != nil {
		return obj, err
	}
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
	return obj, nil
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
	var err error
	switch verb {
	case "start":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Stopped {
			return obj, ErrPhase
		}
		err = c.driver.Start(ctx, id)
	case "stop":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Running {
			return obj, ErrPhase
		}
		err = c.driver.Stop(ctx, id)
	case "delete":
		old := obj
		obj.Status.Phase = PhaseDeleting
		if err = c.persist(ctx, obj, MutationDeleting); err != nil {
			return old, err
		}
		err = c.driver.Delete(ctx, id)
		if errors.Is(err, driver.ErrNotFound) {
			err = nil
		}
		if err == nil {
			c.forgetTouch(id)
			c.forgetLost(id)
			c.purgeEgress(ctx, id)
			// The token dies with the sandbox rather than with its own
			// exp, so a deleted sandbox's identity is refused at once
			// (spec 006).
			revoked := c.revokeToken(ctx, obj.Status.TokenState)
			return export(obj), errors.Join(revoked, c.forget(ctx, id, MutationDeleted))
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

// verbMutation is the act one API verb performed, which is the type of the
// record it produces.
func verbMutation(verb string) string {
	if verb == "start" {
		return MutationStarted
	}
	return MutationStopped
}
func (c *Controller) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	return c.driver.Exec(ctx, id, req)
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
	obj.Spec.Secrets = slices.Clone(obj.Spec.Secrets)
	obj.Status.Conditions = slices.Clone(obj.Status.Conditions)
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
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	n := new(big.Int).SetBytes(b[:])
	mask := big.NewInt(31)
	var out [26]byte
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return "sbx_" + string(out[:]), nil
}
