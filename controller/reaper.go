// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The reasons the reaper writes, from the one transition enum of design 009.
const (
	ReasonExpired    = "Expired"
	ReasonAutoDelete = "AutoDelete"
	ReasonAutoStop   = "AutoStop"
)

// ReaperLease is the lease name design 010 gives the reaper and LeaseTTL its
// term. The loop acquires it once per tick instead of holding a handle, so a
// lease that moves to another writer stops this one at its next tick.
const (
	ReaperLease = "reaper"
	LeaseTTL    = 15 * time.Second
)

// The reaper's intervals when Options leaves them zero.
const (
	DefaultReapInterval  = 30 * time.Second
	DefaultTouchInterval = time.Minute
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			held, err := c.lease.Acquire(ctx, ReaperLease, LeaseTTL)
			if err != nil {
				c.log.WarnContext(ctx, "reaper lease unavailable", "lease", ReaperLease, "err", err)
				continue
			}
			if !held {
				continue
			}
			if acted, err := c.Reap(ctx); err != nil {
				c.log.WarnContext(ctx, "reaper tick incomplete", "acted", acted, "err", err)
			}
		}
	}
}

// Reap applies the lifecycle rules once over the environment and reports how
// many sandboxes it acted on. A List that fails is no list rather than an
// empty one: an environment the controller cannot read reports nothing, and
// treating silence as emptiness would end every sandbox in it.
func (c *Controller) Reap(ctx context.Context) (int, error) {
	states, err := c.driver.List(ctx, driver.Filter{})
	if err != nil {
		return 0, fmt.Errorf("reaper: reading the environment: %w", err)
	}
	now := c.clock.Now()
	acted := 0
	var failed error
	for _, s := range states {
		rule := reapRule(s, now)
		if rule == "" {
			continue
		}
		done, err := c.enforce(ctx, s.ID, rule, now)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("reaper: %s on %s: %w", rule, s.ID, err))
			continue
		}
		if done {
			c.log.InfoContext(ctx, "reaper ended a sandbox", "sandbox", s.ID, "reason", rule)
			acted++
		}
	}
	return acted, failed
}

// enforce re-reads the candidate under the controller's lock and acts only
// while the same rule still matches the fresh state, so a sandbox touched or
// retimed between the list and the action keeps running.
func (c *Controller) enforce(ctx context.Context, id, rule string, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.driver.Inspect(ctx, id)
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
		return true, c.stopLocked(ctx, id, rule)
	}
	return true, c.deleteLocked(ctx, id, rule)
}

// stopLocked stops the sandbox and records the phase the driver reports with
// the rule's reason. A driver object with no desired record is stopped all the
// same: the substrate is what the rules read, and there is no status to write.
func (c *Controller) stopLocked(ctx context.Context, id, reason string) error {
	if err := c.driver.Stop(ctx, id); err != nil {
		return err
	}
	obj, tracked := c.objects[id]
	if !tracked {
		return nil
	}
	obj, err := c.refresh(ctx, obj)
	if err != nil {
		return err
	}
	obj.Status.Reason = reason
	c.objects[id] = clone(obj)
	return c.save()
}

// deleteLocked writes the Deleting intent with its reason before it calls the
// driver, so a crash between the two leaves a record that names why, and
// removes the record once the object is gone.
func (c *Controller) deleteLocked(ctx context.Context, id, reason string) error {
	obj, tracked := c.objects[id]
	if tracked {
		intent := clone(obj)
		intent.Status.Phase = "Deleting"
		intent.Status.Reason = reason
		c.objects[id] = intent
		if err := c.save(); err != nil {
			c.objects[id] = obj
			return err
		}
		obj = intent
	}
	if err := c.driver.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return err
	}
	c.forgetTouch(id)
	if !tracked {
		return nil
	}
	delete(c.objects, id)
	if err := c.save(); err != nil {
		c.objects[id] = obj
		return err
	}
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
	return c.driver.Touch(ctx, id)
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

// lifecycleOf is the one place the manifest's lifecycle meets the driver's.
// manifest/v1 carries spec.lifecycle from slice 044 of design 031; until it
// does, every sandbox of this environment takes the environment's default.
func (c *Controller) lifecycleOf(_ v1.Sandbox) driver.Lifecycle { return c.lifecycle }

// logger is the reaper's log: an operator sees every sandbox it ends and every
// tick it could not finish.
func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
