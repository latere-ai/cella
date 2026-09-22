// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// The reasons the reaper writes, from the one transition enum of design 009.
const (
	ReasonExpired      = "Expired"
	ReasonAutoDelete   = "AutoDelete"
	ReasonAutoStop     = "AutoStop"
	ReasonLost         = "Lost"
	ReasonCreateFailed = "CreateFailed"
	// ReasonParent is a sandbox the delete of an ancestor ended, which is
	// the cascade of design 022.
	ReasonParent            = "Parent"
	ReasonRecoveryExhausted = "RecoveryExhausted"
)

// ruleToken names design 005's fifth rule in a log line. It writes no reason
// on the sandbox, because a re-minted identity ends nothing: the four rules
// above end a sandbox and this one keeps it reachable.
const ruleToken = "token"

// ReaperLease is the lease name design 010 gives the reaper and LeaseTTL its
// term. The loop acquires it once per tick instead of holding a handle, so a
// lease that moves to another writer stops this one at its next tick.
const (
	ReaperLease = "reaper"
	LeaseTTL    = 15 * time.Second
)

// The reaper's intervals and bounds when Options leaves them zero.
const (
	DefaultReapInterval  = 30 * time.Second
	DefaultTouchInterval = time.Minute
	// DefaultLostGrace is how long a sandbox the driver no longer has is
	// held as Lost before it is deleted, where no durable store can recover
	// it. CELLA_LOST_GRACE sets it.
	DefaultLostGrace = 10 * time.Minute
	// DefaultRecoveryAttempts is how many recreations a lost sandbox gets
	// before it is Failed with reason RecoveryExhausted.
	DefaultRecoveryAttempts = 5
)

// Clock is the controller's view of time. Now is read once per tick and every
// rule of that tick is evaluated against it; Ticker drives the loop, so a test
// advances time without sleeping.
type Clock interface {
	Now() time.Time
	Ticker(d time.Duration) (ticks <-chan time.Time, stop func())
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }
func (wallClock) Ticker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Lease is the single-writer seam of design 010: the reaper acts only on the
// replica that holds the named lease.
type Lease interface {
	Acquire(ctx context.Context, name string, ttl time.Duration) (held bool, err error)
}

// LocalLease grants every lease. One cellad with one in-process driver is the
// only writer of its environment; the Postgres lease of design 010 replaces
// this where a second replica runs, and renewal is that implementation's.
type LocalLease struct{}

func (LocalLease) Acquire(context.Context, string, time.Duration) (bool, error) { return true, nil }

// reapRule returns the first lifecycle rule the state matches at now, in the
// order of design 005's table, or the empty string for none. First match wins,
// so a sandbox both past its deadline and idle is deleted rather than stopped.
// A zero duration and a zero ExpiresAt are never: the driver contract refuses
// a negative duration, so zero is the only sentinel a rule reads.
func reapRule(s driver.State, now time.Time) string {
	switch {
	case !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt):
		return ReasonExpired
	case s.Phase == driver.Stopped && s.AutoDelete > 0 && !s.StoppedAt.IsZero() && !now.Before(s.StoppedAt.Add(s.AutoDelete)):
		return ReasonAutoDelete
	case s.Phase == driver.Running && s.AutoStop > 0 && !now.Before(activityOf(s).Add(s.AutoStop)):
		return ReasonAutoStop
	}
	return ""
}

// activityOf is the instant the autoStop rule counts from: the stamped
// activity, or the creation of a sandbox that has never been touched, so a
// driver that stamps nothing does not hold a sandbox open forever.
func activityOf(s driver.State) time.Time {
	if s.LastActivityAt.IsZero() {
		return s.CreatedAt
	}
	return s.LastActivityAt
}

// RunReaper ticks every ReapInterval until ctx ends. Each tick runs only while
// the reaper lease is held, so exactly one writer enforces one environment's
// lifecycle.
func (c *Controller) RunReaper(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.reapInterval)
	defer stop()
	// The first tick runs at once rather than one interval in. It is the
	// start-up reconcile of design 005: desired state has just been read from
	// the store, the observed index is whatever the last process left, and
	// everything this environment holds is compared against the driver before
	// the process serves its first request.
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.tick(ctx)
		}
	}
}

// tick runs one pass while this replica holds the reaper lease.
func (c *Controller) tick(ctx context.Context) {
	held, err := c.lease.Acquire(ctx, ReaperLease, LeaseTTL)
	c.metrics.LeaseHeld(MetricLeaseReaper, err == nil && held)
	if err != nil {
		c.log.WarnContext(ctx, "reaper lease unavailable", "lease", ReaperLease, "err", err)
		return
	}
	if !held {
		return
	}
	if acted, err := c.Reap(ctx); err != nil {
		c.log.WarnContext(ctx, "reaper tick incomplete", "acted", acted, "err", err)
	}
}

// Reap applies the lifecycle rules once over every placeable environment and
// reports how many sandboxes it acted on. An environment below Ready is left
// alone, which is spec 021's rule: a data plane reporting nothing is not a
// data plane reporting an empty world.
func (c *Controller) Reap(ctx context.Context) (int, error) {
	acted := 0
	var failed error
	for _, environment := range c.placeable() {
		n, err := c.reapEnvironment(ctx, environment)
		acted += n
		failed = errors.Join(failed, err)
	}
	// The revocation list is swept once a tick rather than once an
	// environment: a row outlives the token it ends and nothing more, and
	// the tokens are the control plane's whichever environment ran them
	// (spec 010).
	if err := c.sweepRevocations(ctx, c.clock.Now()); err != nil {
		failed = errors.Join(failed, fmt.Errorf("reaper: %w", err))
	}
	return acted, failed
}

// reapEnvironment is one pass over one environment. A List that fails is no
// list rather than an empty one: an environment the controller cannot read
// reports nothing, and treating silence as emptiness would end every sandbox
// in it.
func (c *Controller) reapEnvironment(ctx context.Context, environment string) (int, error) {
	d, err := c.driverFor(environment)
	if err != nil {
		return 0, err
	}
	states, err := d.List(ctx, driver.Filter{})
	if err != nil {
		return 0, fmt.Errorf("reaper: reading the environment %s: %w", environment, err)
	}
	now := c.clock.Now()
	acted := 0
	var failed error
	// The observed index is what the lost rule reads, and it is rebuilt from
	// the same list the deadline rules run over, so one tick has one view of
	// the environment. A rebuild that fails holds the lost rule for that
	// tick: its input is the index, and a stale index would report a sandbox
	// lost that the driver has.
	rebuilt := true
	if c.durable != nil {
		// The index holds what desired state can be compared against, and a
		// pool entry has no desired row, so entries are left out of it.
		observed := slices.DeleteFunc(slices.Clone(states), func(s driver.State) bool { return s.Pool })
		if err := c.durable.Rebuild(ctx, environment, observed); err != nil {
			rebuilt = false
			failed = errors.Join(failed, fmt.Errorf("reaper: rebuilding the observed index: %w", err))
		}
	}
	for _, s := range states {
		if s.Pool {
			// A pool entry is not a sandbox: nobody owns it, it carries no
			// deadline, and its lifecycle is the refill loop's (spec 020).
			// The rules are not asked of it at all, so a driver that
			// stamped a deadline on an entry cannot have it ended by a rule
			// that was never meant to see it.
			continue
		}
		rule := reapRule(s, now)
		if rule == "" {
			// The token rule is last in design 005's table and first match
			// wins, so it is asked only of a sandbox no deadline rule
			// claimed: a sandbox about to be deleted is not one whose
			// identity is worth renewing.
			done, err := c.enforceToken(ctx, s, now)
			if err != nil {
				failed = errors.Join(failed, fmt.Errorf("reaper: %s on %s: %w", ruleToken, s.ID, err))
			}
			if done {
				acted++
			}
			continue
		}
		done, err := c.enforce(ctx, environment, s.ID, rule, now)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("reaper: %s on %s: %w", rule, s.ID, err))
			continue
		}
		if done {
			c.log.InfoContext(ctx, "reaper ended a sandbox", "sandbox", s.ID, "reason", rule)
			acted++
		}
	}
	if !rebuilt {
		return acted, failed
	}
	for _, id := range c.vanished(environment, states) {
		done, err := c.enforceLost(ctx, id, now)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("reaper: %s on %s: %w", ReasonLost, id, err))
			continue
		}
		if done {
			acted++
		}
	}
	return acted, failed
}

// enforceToken is design 005's token rule: a live sandbox whose token has
// passed two thirds of its lifetime is re-minted, re-projected and the token
// it held revoked, in one act.
//
// A sandbox on its way out is left alone. Deleting is this control plane's
// own act in flight, Failed never runs again, and a Lost sandbox has no
// driver object to project into; recovery mints for that one.
func (c *Controller) enforceToken(ctx context.Context, state driver.State, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, tracked := c.objects[state.ID]
	if !tracked || !rotatable(obj.Status.Phase) {
		return false, nil
	}
	if !dueForRotation(obj.Status.TokenState, state.ExpiresAt, now) {
		return false, nil
	}
	return true, c.rotateLocked(ctx, obj)
}

// rotatable reports whether a sandbox in this phase is one whose identity is
// renewed. A stopped sandbox is: its files and its record are intact, it
// starts again with what it holds, and a token that expired while it was
// stopped would leave it unable to call back.
func rotatable(phase string) bool {
	switch phase {
	case PhaseDeleting, PhaseFailed, PhaseLost, PhaseRecovering:
		return false
	}
	return true
}

// enforce re-reads the candidate under the controller's lock and acts only
// while the same rule still matches the fresh state, so a sandbox touched or
// retimed between the list and the action keeps running.
func (c *Controller) enforce(ctx context.Context, environment, id, rule string, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, err := c.driverFor(environment)
	if err != nil {
		return false, err
	}
	state, err := d.Inspect(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if reapRule(state, now) != rule {
		return false, nil
	}
	if rule == ReasonAutoStop {
		c.metrics.ReaperAction(rule, ActionStopped)
		return true, c.stopLocked(ctx, environment, id, rule)
	}
	c.metrics.ReaperAction(rule, ActionDeleted)
	return true, c.deleteLocked(ctx, environment, id, rule)
}

// stopLocked stops the sandbox and records the phase the driver reports with
// the rule's reason. A driver object with no desired record is stopped all the
// same: the substrate is what the rules read, and there is no status to write.
func (c *Controller) stopLocked(ctx context.Context, environment, id, reason string) error {
	d, err := c.driverFor(environment)
	if err != nil {
		return err
	}
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	obj, tracked := c.objects[id]
	if !tracked {
		return nil
	}
	obj, err = c.refresh(ctx, obj)
	if err != nil {
		return err
	}
	obj.Status.Reason = reason
	return c.persist(ctx, obj, MutationStopped)
}

// deleteLocked writes the Deleting intent with its reason before it calls the
// driver, so a crash between the two leaves a record that names why, and
// removes the record once the object is gone.
//
// A deadline ends a tree the way a request does: the descendants go first,
// deepest generation before the one above it, each with reason Parent, so a
// sandbox never outlives the ancestor whose boundary it ran inside (spec 022).
func (c *Controller) deleteLocked(ctx context.Context, environment, id, reason string) error {
	obj, tracked := c.objects[id]
	if tracked {
		if err := c.cascade(ctx, id); err != nil {
			return err
		}
		intent := clone(obj)
		intent.Status.Phase = PhaseDeleting
		intent.Status.Reason = reason
		if err := c.persist(ctx, intent, MutationDeleting); err != nil {
			return err
		}
		obj = intent
		return c.deleteOne(ctx, &obj)
	}
	d, err := c.driverFor(environment)
	if err != nil {
		return err
	}
	if err := d.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return err
	}
	c.forgetTouch(id)
	c.forgetLost(id)
	return nil
}

// Touch stamps activity on a sandbox, coalesced per sandbox: the first call
// reaches the driver and the calls inside TouchInterval after it do not, so an
// interactive session writes the substrate once an interval rather than once a
// keystroke. A suppressed call is dropped and not deferred, so the stamped
// activity lags the real activity by up to one interval.
func (c *Controller) Touch(ctx context.Context, id string) error {
	if !c.openTouchWindow(id) {
		return nil
	}
	d, err := c.driverOf(id)
	if err != nil {
		return err
	}
	return d.Touch(ctx, id)
}

// openTouchWindow reports whether this touch is the one that reaches the
// driver and, when it is, closes the window for TouchInterval. The window
// closes before the driver call, so a driver that fails every touch is asked
// no more often than one that succeeds.
func (c *Controller) openTouchWindow(id string) bool {
	c.touchMu.Lock()
	defer c.touchMu.Unlock()
	now := c.clock.Now()
	if next, ok := c.touched[id]; ok && now.Before(next) {
		return false
	}
	c.touched[id] = now.Add(c.touchInterval)
	return true
}

// forgetTouch drops a deleted sandbox's window, so the map holds one entry per
// sandbox that still exists.
func (c *Controller) forgetTouch(id string) {
	c.touchMu.Lock()
	defer c.touchMu.Unlock()
	delete(c.touched, id)
}

// logger is the reaper's log: an operator sees every sandbox it ends and every
// tick it could not finish.
func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
