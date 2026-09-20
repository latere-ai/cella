// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The recovery backoff of design 005: the first recreation waits half a
// minute and each one after it waits twice as long, to eight minutes.
const (
	recoveryFloor   = 30 * time.Second
	recoveryCeiling = 8 * time.Minute
)

// recoveryBackoff is how long the nth recreation of one sandbox waits. An
// unbounded loop against a driver that refuses every create is a load
// generator, and a fixed delay is one that never gives the driver room.
func recoveryBackoff(attempt int) time.Duration {
	wait := recoveryFloor
	for range attempt - 1 {
		wait *= 2
		if wait >= recoveryCeiling {
			return recoveryCeiling
		}
	}
	return wait
}

// vanished is the lost rule's input: the desired sandboxes of this environment
// with no counterpart in what the driver just listed.
//
// A sandbox in Pending, Failed or Deleting is never lost. Pending has not
// reached the driver, Failed never will, and Deleting is this control plane's
// own act in flight. That is the hosted reaper's terminal probe restated: a
// sandbox the platform itself ended is not one the data plane lost.
func (c *Controller) vanished(states []driver.State) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	observed := make(map[string]struct{}, len(states))
	for _, s := range states {
		observed[s.ID] = struct{}{}
	}
	var out []string
	for id, obj := range c.objects {
		if _, held := observed[id]; held {
			// It is back, or it never went: whatever was counted against it
			// is not owed any more.
			delete(c.lost, id)
			delete(c.retry, id)
			delete(c.attempts, id)
			continue
		}
		if !canBeLost(obj.Status.Phase) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// canBeLost reports whether a phase can become Lost.
func canBeLost(phase string) bool {
	switch phase {
	case driver.Pending, PhaseFailed, PhaseDeleting:
		return false
	}
	return true
}

// enforceLost re-reads one candidate under the controller's lock and acts only
// while the driver still does not have it.
//
// The list is read without the lock, so a create can complete between the list
// and the rule. Only ErrNotFound confirms a sandbox is lost; anything else
// leaves it alone, because recreating a sandbox that exists would run it twice.
func (c *Controller) enforceLost(ctx context.Context, id string, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, tracked := c.objects[id]
	if !tracked || !canBeLost(obj.Status.Phase) {
		return false, nil
	}
	_, err := c.driver.Inspect(ctx, id)
	switch {
	case err == nil:
		delete(c.lost, id)
		return false, nil
	case !errors.Is(err, driver.ErrNotFound):
		return false, err
	}
	if c.recovers {
		return c.recoverLocked(ctx, obj, now)
	}
	return c.graceLocked(ctx, obj, now)
}

// graceLocked is the lost rule without a durable store: the sandbox is marked
// Lost on the tick that first missed it and deleted once the grace has passed.
//
// The grace is counted in this process because the intent it protects also
// lives in this process. A control plane that restarts has no desired state to
// recover from, which is what the start-up log says out loud.
func (c *Controller) graceLocked(ctx context.Context, obj v1.Sandbox, now time.Time) (bool, error) {
	id := obj.Status.ID
	since, marked := c.lost[id]
	if !marked {
		c.lost[id] = now
		if obj.Status.Phase == PhaseLost {
			return false, nil
		}
		obj.Status.Phase = PhaseLost
		obj.Status.Reason = ReasonLost
		return false, c.persist(ctx, obj, MutationLost)
	}
	if now.Before(since.Add(c.lostGrace)) {
		return false, nil
	}
	c.log.InfoContext(ctx, "reaper ended a lost sandbox", "sandbox", id, "reason", ReasonLost,
		"grace", c.lostGrace)
	return true, c.deleteLocked(ctx, id, ReasonLost)
}

// recoverLocked is the lost rule with a durable store: the sandbox is marked
// Lost, then Recovering, and recreated by the create order with the same id,
// name, labels, annotations and lifecycle.
//
// The annotations and the labels never left: they are the desired state this
// recreation is made from. The map, the token and the volumes of the create
// order's steps 3 to 5 are slices 039, 045 and design 019.
func (c *Controller) recoverLocked(ctx context.Context, obj v1.Sandbox, now time.Time) (bool, error) {
	id := obj.Status.ID
	if next, waiting := c.retry[id]; waiting && now.Before(next) {
		return false, nil
	}
	if obj.Status.Phase != PhaseLost && obj.Status.Phase != PhaseRecovering {
		obj.Status.Phase = PhaseLost
		obj.Status.Reason = ReasonLost
		if err := c.persist(ctx, obj, MutationLost); err != nil {
			return false, err
		}
	}
	c.attempts[id]++
	attempt := c.attempts[id]
	if attempt > c.recoveryAttempts {
		obj.Status.Phase = PhaseFailed
		obj.Status.Reason = ReasonRecoveryExhausted
		c.log.WarnContext(ctx, "recovery exhausted", "sandbox", id, "attempts", c.recoveryAttempts)
		delete(c.retry, id)
		delete(c.attempts, id)
		delete(c.lost, id)
		return true, c.persist(ctx, obj, MutationFailed)
	}
	obj.Status.Phase = PhaseRecovering
	obj.Status.Reason = ReasonLost
	if err := c.persist(ctx, obj, MutationRecovering); err != nil {
		return false, err
	}
	lifecycle, err := c.lifecycleFor(obj)
	if err != nil {
		return false, err
	}
	// A create that finds the id already stamped adopts the object rather
	// than making a second one, which is the idempotence of design 005's
	// create order against a crash between the driver call and the write.
	// The boundary is put back in a gateway before the driver is asked, as
	// on create (spec 018): a recovered sandbox runs inside the same map.
	m, held, err := c.pushEgress(ctx, &obj)
	if err != nil {
		c.retry[id] = now.Add(recoveryBackoff(attempt))
		return false, fmt.Errorf("pushing the boundary: %w", err)
	}
	obj.Status.Conditions = setCondition(obj.Status.Conditions, c.egressCondition(m, held, now))
	if _, err := c.driver.Create(ctx, specOf(obj, lifecycle, c.egressSpec(m))); err != nil && !errors.Is(err, driver.ErrAlreadyExists) {
		c.retry[id] = now.Add(recoveryBackoff(attempt))
		return false, fmt.Errorf("recreating the sandbox: %w", err)
	}
	delete(c.retry, id)
	delete(c.attempts, id)
	delete(c.lost, id)
	obj, err = c.refresh(ctx, obj)
	c.log.InfoContext(ctx, "recovered a lost sandbox", "sandbox", id, "attempt", attempt, "phase", obj.Status.Phase)
	return true, errors.Join(err, c.persist(ctx, obj, MutationRecovered))
}

// forgetLost drops a deleted sandbox's lost bookkeeping, so the maps hold one
// entry per sandbox that still exists.
func (c *Controller) forgetLost(id string) {
	delete(c.lost, id)
	delete(c.retry, id)
	delete(c.attempts, id)
}
