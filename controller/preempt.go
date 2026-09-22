// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// DefaultMaxPreemptions is how many times one sandbox may be stopped to place
// one of higher priority when Options leaves the bound zero (spec 020).
const DefaultMaxPreemptions = 3

// NoPreemptions is the bound that makes no sandbox a victim. Options reads
// zero as its default, so the zero an operator asks for is carried as this.
const NoPreemptions = -1

// ReasonPreempted is the reason a sandbox the scheduler stopped carries, on
// its status and on its Scheduled condition, while it waits to run again.
const ReasonPreempted = v1.ReasonPreempted

// requeued reports whether a queued sandbox is one a driver still holds: a
// sandbox the loop preempted, stopped with its workspace, waiting to be
// started again. A preemption is the only way back into Queued from a phase a
// driver holds, so a count above zero on a Queued sandbox is exactly that.
func requeued(obj v1.Sandbox) bool {
	return obj.Status.Phase == PhaseQueued && obj.Status.Preemptions > 0
}

// candidate is a sandbox the head may stop, with what it asks of the
// environment.
type candidate struct {
	obj     v1.Sandbox
	request usage
}

// victims is the sandboxes to stop so that a head that does not fit does:
// among the running, preemptible sandboxes of the environment whose priority
// is below the head's and which have been preempted fewer times than the
// bound, lowest priority first, then largest requested cpu, then newest, the
// shortest run whose cpu, memory and slots make the head fit. A victim keeps
// its disk, so a head short of disk stops nobody, and a head that would not
// fit with every candidate stopped stops nobody either: a stop that places
// nothing is a loss with no gain. It runs under the controller's lock.
func (c *Controller) victims(environment string, head v1.Sandbox) ([]v1.Sandbox, error) {
	if c.maxPreemptions < 0 {
		return nil, nil
	}
	capacity, err := ceilingOf(c.capacityOf(environment))
	if err != nil {
		return nil, fmt.Errorf("the capacity of %s: %w", environment, err)
	}
	used, err := c.usageOn(environment)
	if err != nil {
		return nil, err
	}
	need, err := askOf(head)
	if err != nil {
		return nil, fmt.Errorf("the resources of %s: %w", head.Status.ID, err)
	}
	var candidates []candidate
	for _, obj := range c.objects {
		if obj.Status.Environment != environment || obj.Status.Phase != driver.Running ||
			!obj.Spec.Scheduling.Preemptible || obj.Spec.Scheduling.Priority >= head.Spec.Scheduling.Priority ||
			obj.Status.Preemptions >= c.maxPreemptions {
			continue
		}
		request, err := requestOf(obj.Spec.Resources)
		if err != nil {
			return nil, fmt.Errorf("the resources of %s: %w", obj.Status.ID, err)
		}
		candidates = append(candidates, candidate{obj: obj, request: request})
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(a.obj.Spec.Scheduling.Priority, b.obj.Spec.Scheduling.Priority),
			cmp.Compare(b.request.cpu, a.request.cpu),
			b.obj.Status.CreatedAt.Compare(a.obj.Status.CreatedAt),
			cmp.Compare(b.obj.Status.ID, a.obj.Status.ID),
		)
	})
	total := used.plus(need)
	var chosen []v1.Sandbox
	for _, cand := range candidates {
		if capacity.holds(total) {
			break
		}
		total.cpu -= cand.request.cpu
		total.memory -= cand.request.memory
		total.sandboxes--
		chosen = append(chosen, cand.obj)
	}
	if !capacity.holds(total) {
		return nil, nil
	}
	return chosen, nil
}

// preempt stops the victims a head needs and requeues each, and reports
// whether it stopped any. A victim that cannot be stopped ends the
// preemption: the head is not placed on this pass, and the victims already
// requeued stay requeued, which is capacity the next pass finds free.
func (c *Controller) preempt(ctx context.Context, environment string, head v1.Sandbox) (bool, error) {
	chosen, err := c.victims(environment, head)
	if err != nil || len(chosen) == 0 {
		return false, err
	}
	d, err := c.driverFor(environment)
	if err != nil {
		return false, err
	}
	for _, victim := range chosen {
		if err := c.requeue(ctx, d, victim, head); err != nil {
			return false, fmt.Errorf("preempting %s for %s: %w", victim.Status.ID, head.Status.ID, err)
		}
	}
	return true, nil
}

// requeue stops one victim through its driver and writes it back into its
// queue. The stop and the requeue are one write, the sandbox.stopped record
// with reason Preempted, so no crash leaves a preempted sandbox Stopped and
// outside every queue; a crash before the write leaves it Running in desired
// state, and the next pass stops it again, which a driver answers as the stop
// of a stopped sandbox. createdAt is its place in line and is not touched.
func (c *Controller) requeue(ctx context.Context, d driver.Driver, obj, head v1.Sandbox) error {
	id := obj.Status.ID
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	// The driver's read carries what the stop changed, stoppedAt among it.
	// The requeue does not depend on it, so a read that fails is logged and
	// the record goes on as it was.
	if read, err := c.refresh(ctx, obj); err != nil {
		c.log.WarnContext(ctx, "a preempted sandbox could not be read after its stop", "sandbox", id, "err", err)
	} else {
		obj = read
	}
	obj.Status.Phase = PhaseQueued
	obj.Status.Reason = ReasonPreempted
	obj.Status.Preemptions++
	obj.Status.Conditions = setCondition(obj.Status.Conditions, waitingCondition(v1.ReasonPreempted, c.clock.Now()))
	if err := c.persist(ctx, obj, MutationStopped); err != nil {
		return err
	}
	c.metrics.SandboxPreempted()
	c.log.InfoContext(ctx, "the scheduler stopped a sandbox to place one of higher priority",
		"sandbox", id, "for", head.Status.ID, "preemptions", obj.Status.Preemptions)
	return nil
}

// resume places a requeued sandbox again: the driver starts the object it
// kept, with the boundary, the token and the workspace it already had, so
// nothing of the create order runs a second time. A driver that no longer has
// the object has lost it, and the sandbox is written Lost for the lost rule of
// design 005 to take.
func (c *Controller) resume(ctx context.Context, obj v1.Sandbox) error {
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	err = d.Start(ctx, obj.Status.ID)
	if errors.Is(err, driver.ErrNotFound) {
		obj.Status.Phase = PhaseLost
		obj.Status.Reason = ReasonLost
		return c.persist(ctx, obj, MutationLost)
	}
	if err != nil {
		return err
	}
	// The driver held it stopped, and that is the phase its read is compared
	// against: a queued one is never asked of a driver.
	obj.Status.Phase = driver.Stopped
	read, err := c.refresh(ctx, obj)
	if err != nil {
		// The driver took the start and could not be read after it. The
		// sandbox is written as the driver answered, started, and the next
		// read corrects whatever it finds.
		c.log.WarnContext(ctx, "a resumed sandbox could not be read after its start", "sandbox", obj.Status.ID, "err", err)
		read = obj
		read.Status.Phase, read.Status.Reason = driver.Running, ""
	}
	read.Status.Conditions = setCondition(read.Status.Conditions, scheduledCondition(false, c.clock.Now()))
	return c.persist(ctx, read, phaseMutation(read.Status.Phase))
}
