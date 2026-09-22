// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// usage is an amount of an environment's capacity: requested cpu, memory and
// disk in the milli-units manifest.ParseQuantity returns, and a count of
// sandboxes.
type usage struct {
	cpu, memory, disk int64
	sandboxes         int
}

func (u usage) plus(o usage) usage {
	return usage{cpu: u.cpu + o.cpu, memory: u.memory + o.memory, disk: u.disk + o.disk, sandboxes: u.sandboxes + o.sandboxes}
}

// requestOf is what one sandbox of these resources asks of its environment. A
// resolved manifest always carries its resources, because Defaults fills the
// ones it leaves out, so the sum over desired state is the sum of what was
// granted and no driver needs to report it.
func requestOf(r v1.Resources) (usage, error) {
	u := usage{sandboxes: 1}
	for _, q := range []struct {
		value v1.Quantity
		into  *int64
	}{{r.CPU, &u.cpu}, {r.Memory, &u.memory}, {r.Disk, &u.disk}} {
		if q.value == "" {
			continue
		}
		n, err := manifest.ParseQuantity(q.value)
		if err != nil {
			return usage{}, err
		}
		*q.into = n
	}
	return u, nil
}

// askOf is what placing one sandbox adds to what its environment holds: its
// request, less the disk of a sandbox the loop preempted, which it has held
// all the while it waited and must not be asked for twice.
func askOf(obj v1.Sandbox) (usage, error) {
	request, err := requestOf(obj.Spec.Resources)
	if err != nil {
		return usage{}, err
	}
	if requeued(obj) {
		request.disk = 0
	}
	return request, nil
}

// holdsCompute reports whether a sandbox in this phase holds its cpu, its
// memory and its slot of the count: every phase in which the driver runs it or
// is bringing it up or down. A queued sandbox holds nothing yet, and a lost
// one holds nothing the control plane can see.
func holdsCompute(phase string) bool {
	switch phase {
	case PhaseQueued, PhaseDeleting, PhaseFailed, PhaseLost, driver.Stopped:
		return false
	}
	return true
}

// holdsDisk reports whether a sandbox holds its disk. A stopped sandbox's
// workspace stays on the substrate until it is deleted, and so does a failed
// one's, except where it failed before any driver held it: a create a direct
// environment could not fit and a queued sandbox whose deadline passed. A
// queued sandbox holds its disk only where the loop preempted it, because
// its driver keeps it stopped with its workspace while it waits.
func holdsDisk(obj v1.Sandbox) bool {
	switch obj.Status.Phase {
	case PhaseQueued:
		return requeued(obj)
	case PhaseDeleting, PhaseLost:
		return false
	case PhaseFailed:
		return obj.Status.Reason != ReasonNoCapacity && obj.Status.Reason != ReasonStartDeadline
	}
	return true
}

// usageOn is what the sandboxes on one environment hold. It is derived from
// desired state on every call and never stored, so a restart reads the same
// sum a running process held and cannot count a sandbox twice.
func (c *Controller) usageOn(environment string) (usage, error) {
	var total usage
	for _, obj := range c.objects {
		if obj.Status.Environment != environment {
			continue
		}
		request, err := requestOf(obj.Spec.Resources)
		if err != nil {
			return usage{}, fmt.Errorf("the resources of %s: %w", obj.Status.ID, err)
		}
		if holdsCompute(obj.Status.Phase) {
			total.cpu += request.cpu
			total.memory += request.memory
			total.sandboxes++
		}
		if holdsDisk(obj) {
			total.disk += request.disk
		}
	}
	return total, nil
}

// capacityOf is what one environment declares it holds. The environment
// cellad drives itself is seeded at Open, so every environment a sandbox can
// name has an object; one that has none declares nothing.
func (c *Controller) capacityOf(environment string) v1.Capacity {
	obj, _ := c.environmentOf(environment)
	return obj.Spec.Capacity
}

// ceiling is a declared capacity in the units usage counts in. A quantity the
// environment does not declare is -1, which bounds nothing; so is every
// quantity of an environment that declares auto, whose ceiling is the
// cluster's and is not read here.
type ceiling struct {
	cpu, memory, disk int64
	sandboxes         int
}

func ceilingOf(capacity v1.Capacity) (ceiling, error) {
	out := ceiling{cpu: -1, memory: -1, disk: -1, sandboxes: -1}
	if capacity.Auto {
		return out, nil
	}
	if capacity.Sandboxes > 0 {
		out.sandboxes = capacity.Sandboxes
	}
	for _, q := range []struct {
		value v1.Quantity
		into  *int64
	}{{capacity.CPU, &out.cpu}, {capacity.Memory, &out.memory}, {capacity.Disk, &out.disk}} {
		if q.value == "" {
			continue
		}
		n, err := manifest.ParseQuantity(q.value)
		if err != nil {
			return ceiling{}, err
		}
		*q.into = n
	}
	return out, nil
}

// holds reports whether this much fits under the ceiling, quantity by
// quantity.
func (c ceiling) holds(u usage) bool {
	return (c.cpu < 0 || u.cpu <= c.cpu) &&
		(c.memory < 0 || u.memory <= c.memory) &&
		(c.disk < 0 || u.disk <= c.disk) &&
		(c.sandboxes < 0 || u.sandboxes <= c.sandboxes)
}

// makeRoom reports whether one more sandbox of these resources fits on the
// environment, and where pool entries hold what it needs, deletes the oldest
// until it does. An entry holds the pool's resources and one slot. Nothing is
// deleted for a create that would not fit with every entry gone.
func (c *Controller) makeRoom(ctx context.Context, environment string, obj v1.Sandbox, entries []driver.State) (bool, error) {
	capacity, err := ceilingOf(c.capacityOf(environment))
	if err != nil {
		return false, fmt.Errorf("the capacity of %s: %w", environment, err)
	}
	used, err := c.usageOn(environment)
	if err != nil {
		return false, err
	}
	request, err := askOf(obj)
	if err != nil {
		return false, fmt.Errorf("the resources of %s: %w", obj.Metadata.Name, err)
	}
	pool, _ := c.poolOf(environment)
	entry, err := requestOf(pool.Resources)
	if err != nil {
		return false, fmt.Errorf("the pool resources of %s: %w", environment, err)
	}
	held := func(n int) usage {
		return usage{cpu: entry.cpu * int64(n), memory: entry.memory * int64(n), disk: entry.disk * int64(n), sandboxes: n}
	}
	if !capacity.holds(used.plus(request)) {
		return false, nil
	}
	evict := 0
	for !capacity.holds(used.plus(request).plus(held(len(entries) - evict))) {
		evict++
	}
	if evict == 0 {
		return true, nil
	}
	d, err := c.driverFor(environment)
	if err != nil {
		return false, err
	}
	for _, oldest := range entries[:evict] {
		if err := d.Delete(ctx, oldest.ID); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return false, fmt.Errorf("pool: freeing capacity by deleting %s: %w", oldest.ID, err)
		}
		c.log.InfoContext(ctx, "the pool gave up an entry so a create would fit", "sandbox", oldest.ID)
	}
	return true, nil
}

// CapacityFigure is one quantity an environment declares beside what its
// sandboxes hold of it, in the quantity's own unit: cores, bytes, or
// sandboxes.
type CapacityFigure struct {
	Environment, Resource string
	Declared, Used        float64
}

// CapacityFigures is every declared quantity of every environment, which is
// what cella_capacity publishes. A quantity an environment does not declare,
// and every quantity of one that declares auto, bounds nothing and has no
// figure.
func (c *Controller) CapacityFigures(ctx context.Context) []CapacityFigure {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CapacityFigure
	for _, environment := range c.environmentsHeld() {
		declared, err := ceilingOf(c.capacityOf(environment))
		if err == nil {
			var used usage
			if used, err = c.usageOn(environment); err == nil {
				out = append(out, figures(environment, declared, used)...)
				continue
			}
		}
		c.log.WarnContext(ctx, "the capacity of an environment could not be read", "environment", environment, "err", err)
	}
	return out
}

func figures(environment string, declared ceiling, used usage) []CapacityFigure {
	var out []CapacityFigure
	for _, f := range []struct {
		resource string
		declared int64
		used     int64
		scale    float64
	}{
		{"cpu", declared.cpu, used.cpu, 1000},
		{"memory", declared.memory, used.memory, 1000},
		{"disk", declared.disk, used.disk, 1000},
		{"sandboxes", int64(declared.sandboxes), int64(used.sandboxes), 1},
	} {
		if f.declared < 0 {
			continue
		}
		out = append(out, CapacityFigure{Environment: environment, Resource: f.resource,
			Declared: float64(f.declared) / f.scale, Used: float64(f.used) / f.scale})
	}
	return out
}
