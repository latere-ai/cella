// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The pool's own names and bounds (spec 020).
const (
	// PoolLeasePrefix names the lease the refill loop of one environment
	// runs under, so a replica set prewarms one environment from one
	// replica while every replica still adopts.
	PoolLeasePrefix = "pool:"
	// PoolShapeLabel carries the hash of the shape an entry was made for.
	// The driver's State reports no image and no resources, so the stamp is
	// what the drift rule compares; an adoption rewrites the labels and the
	// stamp goes with them.
	PoolShapeLabel = "cella.latere.ai/pool-shape"
	// DefaultPoolInFlight bounds how many entries one tick prewarms. A cold
	// start of a large pool would otherwise open every image pull at once.
	DefaultPoolInFlight = 2
	// DefaultPoolGrace is how long an entry is left alone before the
	// deletion rules read it. A driver reports a container between its
	// create and its first running status as Pending, and an entry deleted
	// in that window would be made and unmade on alternating ticks.
	DefaultPoolGrace = 5 * time.Minute
)

// PoolLease is the lease name of one environment's refill loop.
func PoolLease(environment string) string { return PoolLeasePrefix + environment }

// countsAgainstCapacity reports whether a sandbox in this phase holds a slot
// of the environment's ceiling. It is the count form of spec 020's capacity:
// the resource form needs the granted resources of every sandbox, which no
// driver reports yet.
func countsAgainstCapacity(phase string) bool {
	switch phase {
	case PhaseDeleting, PhaseFailed, PhaseLost, driver.Stopped:
		return false
	}
	return true
}

// RunPool ticks the refill loop until ctx ends. A driver that declares no Pool
// runs no loop at all: it can hold no entry, so there is nothing to keep and
// nothing to clean up.
func (c *Controller) RunPool(ctx context.Context) {
	if !c.driver.Capabilities().Pool {
		return
	}
	ticks, stop := c.clock.Ticker(c.reapInterval)
	defer stop()
	c.poolTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.poolTick(ctx)
		}
	}
}

// poolTick runs one pass while this replica holds the environment's pool lease.
func (c *Controller) poolTick(ctx context.Context) {
	lease := PoolLease(c.environment)
	held, err := c.lease.Acquire(ctx, lease, LeaseTTL)
	if err != nil {
		c.log.WarnContext(ctx, "pool lease unavailable", "lease", lease, "err", err)
		return
	}
	if !held {
		return
	}
	if acted, err := c.Refill(ctx); err != nil {
		c.log.WarnContext(ctx, "pool tick incomplete", "acted", acted, "err", err)
	}
}

// Refill brings the environment's pool to its target once and reports how many
// entries it made or deleted.
//
// A list the driver refuses ends the tick without acting. A partial view of
// the environment reads as a pool that is short, and the answer to that would
// be creates: the one thing a tick must not do on no information.
func (c *Controller) Refill(ctx context.Context) (int, error) {
	if !c.driver.Capabilities().Pool {
		return 0, nil
	}
	states, err := c.driver.List(ctx, driver.Filter{})
	if err != nil {
		return 0, fmt.Errorf("pool: reading the environment: %w", err)
	}
	now := c.clock.Now()
	var entries []driver.State
	live := 0
	for _, s := range states {
		switch {
		case s.Pool:
			entries = append(entries, s)
		case countsAgainstCapacity(s.Phase):
			live++
		}
	}
	keep, drop := c.sortPool(entries, now)
	target := c.poolTarget(live)
	keep, drop = takeSurplus(keep, drop, target)
	acted := 0
	var failed error
	shape := c.poolShape()
	for _, entry := range drop {
		if err := c.driver.Delete(ctx, entry.ID); err != nil && !errors.Is(err, driver.ErrNotFound) {
			failed = errors.Join(failed, fmt.Errorf("pool: deleting the entry %s: %w", entry.ID, err))
			continue
		}
		c.log.InfoContext(ctx, "the pool deleted an entry", "sandbox", entry.ID,
			"reason", cmp.Or(poolDropReason(entry, shape, now, c.poolGrace), ReasonSurplus))
		acted++
	}
	for range min(target-len(keep), c.poolInFlight) {
		id, err := c.prewarm(ctx)
		if err != nil {
			// One refusal ends this tick's prewarming. An environment that
			// cannot take a create is asked twice and not fifty times.
			return acted, errors.Join(failed, fmt.Errorf("pool: prewarming: %w", err))
		}
		c.log.InfoContext(ctx, "the pool prewarmed an entry", "sandbox", id, "image", c.pool.Image)
		acted++
	}
	return acted, failed
}

// takeSurplus moves the entries above the target out of the kept set, oldest
// first and only ones that are ready.
//
// An entry that is still coming up is never the surplus. It is what the grace
// protects, and a tick that gave it up would make and unmake an entry on
// alternating ticks: the one that was just created is the youngest, and the
// one the pool wants to give up is the one that has been waiting longest.
func takeSurplus(keep, drop []driver.State, target int) ([]driver.State, []driver.State) {
	surplus := len(keep) - target
	if surplus <= 0 {
		return keep, drop
	}
	kept := make([]driver.State, 0, len(keep))
	for _, entry := range keep {
		if surplus > 0 && entry.Phase == driver.Running {
			drop = append(drop, entry)
			surplus--
			continue
		}
		kept = append(kept, entry)
	}
	return kept, drop
}

// poolTarget is how many entries this environment should hold: the declared
// size, and never more than the ceiling leaves after the sandboxes that hold
// it. Without the second term the loop and the create path would fight, one
// deleting an entry to make room and the other making it again.
func (c *Controller) poolTarget(live int) int {
	if c.pool.Size <= 0 {
		return 0
	}
	if c.capacity <= 0 {
		return c.pool.Size
	}
	return max(min(c.pool.Size, c.capacity-live), 0)
}

// sortPool splits the entries into the ones the pool still wants, oldest
// first, and the ones it does not. An entry inside the grace is always kept:
// it may still be coming up, and the deletion rules would otherwise race the
// create that made it.
func (c *Controller) sortPool(entries []driver.State, now time.Time) (keep, drop []driver.State) {
	shape := c.poolShape()
	for _, entry := range entries {
		if now.Sub(entry.CreatedAt) < c.poolGrace || poolDropReason(entry, shape, now, c.poolGrace) == "" {
			keep = append(keep, entry)
			continue
		}
		drop = append(drop, entry)
	}
	slices.SortFunc(keep, func(a, b driver.State) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return keep, drop
}

// The reasons an entry leaves the pool, which the log line carries so an
// operator reads why the pool gave one up rather than only that it did.
const (
	ReasonShapeDrift = "ShapeDrift"
	ReasonNotRunning = "NotRunning"
	ReasonSurplus    = "Surplus"
)

// poolDropReason says why an entry is no longer one the pool wants, or the
// empty string for one it does. Drift and the orphan are one rule read two
// ways: the shape changed under the entry, or the engine moved it out of
// Running. An entry inside the grace has no reason at all.
func poolDropReason(entry driver.State, shape string, now time.Time, grace time.Duration) string {
	switch {
	case now.Sub(entry.CreatedAt) < grace:
		return ""
	case entry.Labels[PoolShapeLabel] != shape:
		return ReasonShapeDrift
	case entry.Phase != driver.Running:
		return ReasonNotRunning
	}
	return ""
}

// poolShape is the hash of what an entry is made of: the image, the resources
// and the display. Two shapes that differ in any of them are different pools,
// and an entry stamped with one the environment no longer declares is deleted
// rather than served to a create that would not match it.
func (c *Controller) poolShape() string {
	parts := []string{
		c.pool.Image,
		string(c.pool.Resources.CPU), string(c.pool.Resources.Memory), string(c.pool.Resources.Disk),
	}
	if d := c.pool.Display; d != nil {
		parts = append(parts, fmt.Sprintf("%dx%d", d.Width, d.Height))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// prewarm makes one entry: the environment's shape, nobody's sandbox, and the
// stamp the drift rule reads.
func (c *Controller) prewarm(ctx context.Context) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	_, err = c.driver.Create(ctx, driver.CreateSpec{
		ID:        id,
		Image:     c.pool.Image,
		Resources: resourcesOf(c.pool.Resources),
		Labels:    map[string]string{PoolShapeLabel: c.poolShape()},
		Prewarm:   true,
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// resourcesOf is the manifest's quantities as the driver takes them, which is
// the caller's own spelling either way.
func resourcesOf(r v1.Resources) driver.Resources {
	return driver.Resources{CPU: string(r.CPU), Memory: string(r.Memory), Disk: string(r.Disk)}
}

// poolEntries is every prewarmed entry of the environment, oldest first. It is
// read under the controller's lock by the create path, which takes the first
// one that matches, so the entry that has been ready longest is the one a
// caller gets.
func (c *Controller) poolEntries(ctx context.Context) []driver.State {
	if c.pool.Size <= 0 || !c.driver.Capabilities().Pool {
		return nil
	}
	pool := true
	entries, err := c.driver.List(ctx, driver.Filter{Pool: &pool})
	if err != nil {
		// The pool is an acceleration. An environment that cannot be listed
		// is a create on the slow path, not a create that fails.
		c.log.WarnContext(ctx, "the pool could not be read; this create takes the slow path", "err", err)
		return nil
	}
	slices.SortFunc(entries, func(a, b driver.State) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return entries
}

// matchEntry is spec 020's match rule: the first entry that can carry this
// manifest, or nil. Everything the rule reads is a field the driver fixed when
// it made the entry and cannot rewrite at adoption.
//
// A quantity is compared as the caller wrote it rather than parsed: the
// manifest keeps the caller's own spelling, and a pool whose "1000m" did not
// match a manifest's "1" is slower and never wrong.
func (c *Controller) matchEntry(entries []driver.State, obj v1.Sandbox) *driver.State {
	if len(entries) == 0 || !c.matchesPool(obj) {
		return nil
	}
	shape := c.poolShape()
	for i, entry := range entries {
		if entry.Phase == driver.Running && entry.Labels[PoolShapeLabel] == shape {
			return &entries[i]
		}
	}
	return nil
}

// matchesPool reports whether the resolved manifest asks for nothing the
// environment's entries cannot carry.
func (c *Controller) matchesPool(obj v1.Sandbox) bool {
	spec := obj.Spec
	switch {
	case spec.Image != c.pool.Image:
		return false
	case spec.Resources != c.pool.Resources:
		return false
	case !sameDisplay(spec.Display, c.pool.Display):
		// The entry's X server sized its frame buffer when the entry came
		// up, and a running desktop cannot be resized into another, so an
		// entry is adopted only into the geometry it already has.
		return false
	case len(spec.Command) > 0 || len(spec.Args) > 0:
		// The entry is already running what the driver starts for a
		// sandbox with no command, and a process cannot be replaced
		// under a container that is up.
		return false
	case spec.User != "":
		return false
	case spec.Workspace.Source != "" && spec.Workspace.Source != v1.WorkspaceSourceEmpty:
		return false
	case !samePath(spec.Workspace.Path, driver.DefaultWorkdir):
		return false
	case !samePath(spec.Workdir, driver.DefaultWorkdir):
		return false
	}
	return true
}

// samePath reads an unset path as the contract's default, which is what every
// driver renders it as.
func samePath(p, fallback string) bool { return p == "" || p == fallback }

// sameDisplay reports whether a manifest asks for the desktop an entry was
// prewarmed with: both without one, or both at one geometry.
func sameDisplay(a, b *v1.Display) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// makeRoom frees a slot for a real create where the environment has a ceiling
// and entries hold it. The oldest entries go first, and a create that does not
// fit with no entry left is the quota refusal the API answers.
func (c *Controller) makeRoom(ctx context.Context, entries []driver.State) error {
	if c.capacity <= 0 {
		return nil
	}
	live := 0
	for _, obj := range c.objects {
		if countsAgainstCapacity(obj.Status.Phase) {
			live++
		}
	}
	for live+len(entries)+1 > c.capacity {
		if len(entries) == 0 {
			return ErrQuota
		}
		oldest := entries[0]
		entries = entries[1:]
		if err := c.driver.Delete(ctx, oldest.ID); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return fmt.Errorf("pool: freeing capacity by deleting %s: %w", oldest.ID, err)
		}
		c.log.InfoContext(ctx, "the pool gave up an entry so a create would fit", "sandbox", oldest.ID)
	}
	return nil
}

// adoptionOf is what one entry is turned into: every field of the sandbox the
// match rule could not carry. It is built where the create order would have
// built the driver's create spec, from the same inputs, so an adopted sandbox
// and a created one hold the same record.
func adoptionOf(obj v1.Sandbox, lifecycle driver.Lifecycle, boundary driver.Egress, secrets map[string]string, token string) driver.Adoption {
	return driver.Adoption{
		Owner:     obj.Status.Owner,
		Name:      obj.Metadata.Name,
		Labels:    obj.Metadata.Labels,
		Env:       withSecretEnv(obj.Spec.Env, secrets),
		Workspace: driver.Workspace{Path: obj.Spec.Workspace.Path},
		Lifecycle: lifecycle,
		Token:     []byte(token),
		Egress:    boundary,
	}
}

// without is the entries less the one an adoption lost, which is what the
// create that follows may still take capacity from.
func without(entries []driver.State, id string) []driver.State {
	return slices.DeleteFunc(slices.Clone(entries), func(s driver.State) bool { return s.ID == id })
}

// adoptionLost reports whether the driver's answer to an adoption means the
// entry cannot serve this create: another adopter took it, it is gone, or it
// cannot carry what the caller asked for. Each is a fall back to a real
// create, never a failure the caller sees.
func adoptionLost(err error) bool {
	return errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrInvalid) ||
		errors.Is(err, driver.ErrUnsupported)
}

// scheduledCondition says where the sandbox came from: an entry the
// environment had ready, or a create the driver ran now.
func scheduledCondition(fromPool bool, now time.Time) v1.Condition {
	cond := v1.Condition{Type: v1.ConditionScheduled, Status: v1.ConditionTrue, Reason: v1.ReasonPlaced, Since: now}
	if fromPool {
		cond.Reason = v1.ReasonFromPool
		cond.Message = "This sandbox was adopted from an entry the environment had prewarmed."
	}
	return cond
}
