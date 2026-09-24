// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// DefaultScheduleInterval is how often the scheduler loop passes over the
// queued environments when nothing wakes it sooner (spec 020).
const DefaultScheduleInterval = 5 * time.Second

// SchedulerLease is the lease the scheduler loop runs under, so one replica
// places from a queue at a time (spec 010).
const SchedulerLease = "scheduler"

// MetricLeaseScheduler is the scheduler's lease by kind, for LeaseHeld.
const MetricLeaseScheduler = "scheduler"

// The reasons a create that never reached a driver fails with. A direct
// environment fails a create it cannot fit, and a queued one fails a sandbox
// that waited past its start deadline.
const (
	ReasonNoCapacity    = v1.ReasonNoCapacity
	ReasonStartDeadline = "StartDeadline"
)

// placement is what step 2 of the create order decided.
type placement int

const (
	// placeNow continues the create at step 3.
	placeNow placement = iota
	// placeQueued holds the sandbox in its queue until it fits.
	placeQueued
	// placeNoCapacity fails a create a direct environment cannot fit.
	placeNoCapacity
)

// schedulingOf is the environment whose mode and queues a placement reads.
// One the controller does not hold is read as direct.
func (c *Controller) schedulingOf(environment string) v1.Environment {
	obj, _ := c.environmentOf(environment)
	return obj
}

// place is step 2 of design 005's create order for a create that adopts no
// entry. A queued environment places a sandbox now only when nothing waits
// ahead of it in its queue and it fits; a direct one places it when it fits
// and fails it otherwise.
func (c *Controller) place(ctx context.Context, environment string, obj v1.Sandbox, entries []driver.State) (placement, error) {
	queued := manifest.Queued(c.schedulingOf(environment))
	if queued && c.waiting(environment, obj.Spec.Scheduling.Queue) > 0 {
		return placeQueued, nil
	}
	fits, err := c.makeRoom(ctx, environment, obj, entries)
	switch {
	case err != nil:
		return placeNow, err
	case fits:
		return placeNow, nil
	case queued:
		return placeQueued, nil
	}
	return placeNoCapacity, nil
}

// waiting is how many sandboxes wait in one queue of one environment.
func (c *Controller) waiting(environment, queue string) int {
	n := 0
	for _, obj := range c.objects {
		if obj.Status.Phase == PhaseQueued && obj.Status.Environment == environment && obj.Spec.Scheduling.Queue == queue {
			n++
		}
	}
	return n
}

// line is one queue in the order spec 020 admits it: priority descending,
// then the subject whose requested cpu on the environment is smallest, then
// arrival. createdAt is a sandbox's arrival: it is written once, at step 1, so
// a place in line is never recomputed. The id breaks what is left, so two
// passes over one state read one order.
func (c *Controller) line(environment, queue string) ([]v1.Sandbox, error) {
	return c.ordered(environment, func(obj v1.Sandbox) bool { return obj.Spec.Scheduling.Queue == queue })
}

// ordered is the sandboxes waiting on one environment that keep selects, in
// the order line gives one queue. Every queue of an environment is ordered by
// the same comparison, so the order of all of them is each queue's own order
// merged, which is how one pass reads them.
func (c *Controller) ordered(environment string, keep func(v1.Sandbox) bool) ([]v1.Sandbox, error) {
	share := map[string]int64{}
	var out []v1.Sandbox
	for _, obj := range c.objects {
		if obj.Status.Environment != environment {
			continue
		}
		if obj.Status.Phase == PhaseQueued {
			if keep(obj) {
				out = append(out, obj)
			}
			continue
		}
		if !holdsCompute(obj.Status.Phase) {
			continue
		}
		request, err := requestOf(obj.Spec.Resources)
		if err != nil {
			return nil, fmt.Errorf("the resources of %s: %w", obj.Status.ID, err)
		}
		share[obj.Status.Owner] += request.cpu
	}
	slices.SortFunc(out, func(a, b v1.Sandbox) int {
		return cmp.Or(
			cmp.Compare(b.Spec.Scheduling.Priority, a.Spec.Scheduling.Priority),
			cmp.Compare(share[a.Status.Owner], share[b.Status.Owner]),
			a.Status.CreatedAt.Compare(b.Status.CreatedAt),
			cmp.Compare(a.Status.ID, b.Status.ID),
		)
	})
	return out, nil
}

// queues is every queue of one environment with a sandbox waiting in it. It
// is read from what waits rather than from what the environment declares, so
// a queue an operator removed still drains what it held.
func (c *Controller) queues(environment string) []string {
	var out []string
	for _, obj := range c.objects {
		if obj.Status.Phase == PhaseQueued && obj.Status.Environment == environment && !slices.Contains(out, obj.Spec.Scheduling.Queue) {
			out = append(out, obj.Spec.Scheduling.Queue)
		}
	}
	slices.Sort(out)
	return out
}

// waitingCondition is the Scheduled condition of a sandbox no driver holds:
// one that waits in its queue, or one a direct environment could not fit.
func waitingCondition(reason string, now time.Time) v1.Condition {
	return v1.Condition{Type: v1.ConditionScheduled, Status: v1.ConditionFalse, Reason: reason, Since: now}
}

// positioned writes a queued sandbox's place in its queue into the message of
// its Scheduled condition. The place is computed on every read from what the
// controller holds, so a dequeue rewrites no other sandbox's row. It runs
// under the controller's lock.
func (c *Controller) positioned(obj v1.Sandbox) v1.Sandbox {
	if obj.Status.Phase != PhaseQueued {
		return obj
	}
	line, err := c.line(obj.Status.Environment, obj.Spec.Scheduling.Queue)
	if err != nil {
		return obj
	}
	at := slices.IndexFunc(line, func(s v1.Sandbox) bool { return s.Status.ID == obj.Status.ID })
	for i, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionScheduled {
			obj.Status.Conditions[i].Message = fmt.Sprintf("Position %d of %d in the queue %s.", at+1, len(line), obj.Spec.Scheduling.Queue)
		}
	}
	return obj
}

// wakeScheduler asks the loop for a pass before its next tick. It never
// blocks: one pending wake is as good as many.
func (c *Controller) wakeScheduler() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// releases reports whether a sandbox moving between these two records gave
// capacity back, which is what a queue waits on.
func releases(before v1.Sandbox, after *v1.Sandbox) bool {
	if after == nil {
		return holdsCompute(before.Status.Phase) || holdsDisk(before)
	}
	return (holdsCompute(before.Status.Phase) && !holdsCompute(after.Status.Phase)) ||
		(holdsDisk(before) && !holdsDisk(*after))
}

// RunScheduler ticks the scheduler loop until ctx ends: once at start, once
// every ScheduleInterval and once on every wake. Each pass runs only while this
// replica holds the scheduler lease.
func (c *Controller) RunScheduler(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.scheduleInterval)
	defer stop()
	// The first pass runs at once, as the reaper's does: a process that
	// restarts with sandboxes waiting and room free places them before the
	// first interval ends.
	c.scheduleTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-c.wake:
		}
		c.scheduleTick(ctx)
	}
}

func (c *Controller) scheduleTick(ctx context.Context) {
	held, err := c.lease.Acquire(ctx, SchedulerLease, LeaseTTL)
	c.metrics.LeaseHeld(MetricLeaseScheduler, err == nil && held)
	if err != nil {
		c.log.WarnContext(ctx, "scheduler lease unavailable", "lease", SchedulerLease, "err", err)
		return
	}
	if !held {
		return
	}
	if err := c.Schedule(ctx); err != nil {
		c.log.WarnContext(ctx, "scheduler pass incomplete", "err", err)
	}
}

// Schedule is one pass of the loop over every environment with a sandbox
// waiting. A sandbox that waited past its start deadline fails, on any
// environment; then, on an environment that is Ready, its queues are placed
// from their heads as scheduleEnvironment says.
func (c *Controller) Schedule(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	var failed error
	for _, environment := range c.environmentsWaiting() {
		failed = errors.Join(failed, c.scheduleEnvironment(ctx, environment, now))
	}
	return failed
}

// environmentsWaiting is every environment with a sandbox in a queue or one
// placed and not yet realized.
func (c *Controller) environmentsWaiting() []string {
	var out []string
	for _, obj := range c.objects {
		if (obj.Status.Phase == PhaseQueued || placed(obj)) && !slices.Contains(out, obj.Status.Environment) {
			out = append(out, obj.Status.Environment)
		}
	}
	slices.Sort(out)
	return out
}

// placed reports whether a sandbox is placed and not yet realized: Pending,
// with no Scheduled condition that is True. Scheduled is written True once
// the driver's create or adoption has returned, and no driver reports it, so
// this separates a sandbox the control plane has not handed to a driver from
// one a driver reported as Pending itself. A sandbox the loop dequeued still
// carries Scheduled False Queued, and one a direct create placed carries no
// Scheduled at all; both are placed.
func placed(obj v1.Sandbox) bool {
	if obj.Status.Phase != driver.Pending {
		return false
	}
	for _, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionScheduled && cond.Status == v1.ConditionTrue {
			return false
		}
	}
	return true
}

// placedOn is every placed and unrealized sandbox of one environment, oldest
// first, which is the order their creates answered in.
func (c *Controller) placedOn(environment string) []string {
	var out []v1.Sandbox
	for _, obj := range c.objects {
		if obj.Status.Environment == environment && placed(obj) {
			out = append(out, obj)
		}
	}
	slices.SortFunc(out, func(a, b v1.Sandbox) int {
		return cmp.Or(a.Status.CreatedAt.Compare(b.Status.CreatedAt), cmp.Compare(a.Status.ID, b.Status.ID))
	})
	ids := make([]string, 0, len(out))
	for _, obj := range out {
		ids = append(ids, obj.Status.ID)
	}
	return ids
}

// scheduleEnvironment is one pass over one environment's queues, read as one
// line: each step tries the head that ranks first among the heads of the
// queues still open. A head that does not fit, even with the pool's entries
// and the victims preemption may stop given up, closes its queue for the rest
// of the pass, so a large request is not passed by the small ones behind it.
// The priorities one pass tries therefore never rise, and a victim's priority
// is below its head's, so a pass never stops a sandbox it placed; passing the
// queues one after another would place a low head in one and stop it for a
// higher head of the next.
func (c *Controller) scheduleEnvironment(ctx context.Context, environment string, now time.Time) error {
	every := func(v1.Sandbox) bool { return true }
	waiting, err := c.ordered(environment, every)
	if err != nil {
		return err
	}
	var failed error
	for _, obj := range waiting {
		if expired(obj, now) {
			failed = errors.Join(failed, c.expire(ctx, obj))
		}
	}
	if c.admits(environment) != nil {
		return failed
	}
	// A placed sandbox already holds its capacity and answered its caller,
	// so it is realized before any queue is read: it is not fitted, not
	// ordered and not held to a start deadline. Each realize reads its row
	// again, since the lock is released around the driver's call.
	for _, id := range c.placedOn(environment) {
		if err := c.realize(ctx, id); err != nil {
			failed = errors.Join(failed, fmt.Errorf("realizing %s: %w", id, err))
		}
	}
	closed := map[string]bool{}
	tried := map[string]bool{}
	for {
		waiting, err := c.ordered(environment, func(obj v1.Sandbox) bool { return !closed[obj.Spec.Scheduling.Queue] })
		if err != nil {
			return errors.Join(failed, err)
		}
		if len(waiting) == 0 {
			return failed
		}
		head := waiting[0]
		queue := head.Spec.Scheduling.Queue
		// A head this pass already tried and could not move is left where
		// it is rather than tried again, so a write that keeps failing
		// cannot hold the loop.
		if tried[head.Status.ID] {
			closed[queue] = true
			continue
		}
		tried[head.Status.ID] = true
		fits, err := c.fit(ctx, environment, head)
		if err != nil {
			failed = errors.Join(failed, err)
		}
		if err != nil || !fits {
			closed[queue] = true
			continue
		}
		if err := c.placeQueued(ctx, head); err != nil {
			failed = errors.Join(failed, fmt.Errorf("placing %s: %w", head.Status.ID, err))
		}
	}
}

// fit reports whether a head fits once the pool's entries have given way,
// and where it does not, whether stopping preemptible sandboxes of lower
// priority makes it fit, stopping them when it does. The entries give way
// before anyone is stopped: an entry is nobody's work.
func (c *Controller) fit(ctx context.Context, environment string, head v1.Sandbox) (bool, error) {
	fits, err := c.makeRoom(ctx, environment, head, c.poolEntries(ctx, environment))
	if err != nil || fits {
		return fits, err
	}
	preempted, err := c.preempt(ctx, environment, head)
	if err != nil || !preempted {
		return false, err
	}
	return c.makeRoom(ctx, environment, head, c.poolEntries(ctx, environment))
}

// expired reports whether a queued sandbox has waited past its start
// deadline. A deadline that does not parse is one Resolve refused, so it never
// reaches here; it is read as no deadline rather than as one already passed.
func expired(obj v1.Sandbox, now time.Time) bool {
	// A sandbox the loop preempted started once, which is what the deadline
	// bounds the wait for; it waits again without one.
	if obj.Spec.Scheduling.StartDeadline == "" || obj.Status.Preemptions > 0 {
		return false
	}
	deadline, never, err := manifest.ParseDuration(obj.Spec.Scheduling.StartDeadline)
	if err != nil || never {
		return false
	}
	return now.Sub(obj.Status.CreatedAt) > deadline
}

// expire fails a sandbox that waited past its start deadline. Nothing past
// step 1 of the create order ran for it, so the failure and the spawn budget
// it took are the whole of the undo.
func (c *Controller) expire(ctx context.Context, obj v1.Sandbox) error {
	obj.Status.Phase = PhaseFailed
	obj.Status.Reason = ReasonStartDeadline
	return errors.Join(c.persist(ctx, obj, MutationFailed), c.credit(ctx, c.parentOf(obj)))
}

// parentOf is the sandbox that spawned this one, where it still exists.
func (c *Controller) parentOf(obj v1.Sandbox) *v1.Sandbox {
	if obj.Status.Parent == "" {
		return nil
	}
	parent, held := c.objects[obj.Status.Parent]
	if !held {
		return nil
	}
	out := clone(parent)
	return &out
}

// placeQueued moves one queued sandbox to Pending and resumes its create at
// step 3, which is the realize a create placed at once gets. It takes the
// slow path: a pool entry is adopted only by a create placed at once, on that
// create's own request. A sandbox the loop preempted has its object already,
// and is started rather than created.
func (c *Controller) placeQueued(ctx context.Context, obj v1.Sandbox) error {
	if requeued(obj) {
		return c.resume(ctx, obj)
	}
	if _, err := c.driverFor(obj.Status.Environment); err != nil {
		return err
	}
	if _, err := c.lifecycleFor(obj); err != nil {
		return err
	}
	obj.Status.Phase = driver.Pending
	if err := c.persist(ctx, obj, MutationStatus); err != nil {
		return err
	}
	return c.realize(ctx, obj.Status.ID)
}

// QueueDepth is how many sandboxes wait in one queue of one environment.
type QueueDepth struct {
	Environment, Queue string
	Waiting            int
}

// QueueDepths is every queue of every queued environment, an empty one
// included, beside any queue an operator removed while sandboxes still wait in
// it. It is what cella_queue_depth publishes, so a queue that drained reads
// zero rather than disappearing.
func (c *Controller) QueueDepths() []QueueDepth {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []QueueDepth
	for _, environment := range c.environmentsHeld() {
		env, _ := c.environmentOf(environment)
		queues := c.queues(environment)
		if manifest.Queued(env) {
			for _, q := range manifest.QueuesOf(env) {
				if !slices.Contains(queues, q) {
					queues = append(queues, q)
				}
			}
		}
		slices.Sort(queues)
		for _, q := range queues {
			out = append(out, QueueDepth{Environment: environment, Queue: q, Waiting: c.waiting(environment, q)})
		}
	}
	return out
}
