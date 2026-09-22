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
	share := map[string]int64{}
	var out []v1.Sandbox
	for _, obj := range c.objects {
		if obj.Status.Environment != environment {
			continue
		}
		if obj.Status.Phase == PhaseQueued {
			if obj.Spec.Scheduling.Queue == queue {
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

// RunScheduler ticks the scheduler loop until ctx ends: once every
// ScheduleInterval and once on every wake. Each pass runs only while this
// replica holds the scheduler lease.
func (c *Controller) RunScheduler(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.scheduleInterval)
	defer stop()
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

// Schedule is one pass of the loop over every queue that holds a sandbox. A
// sandbox that waited past its start deadline fails, on any environment; then,
// on an environment that is Ready, each queue is placed from its head while
// the head fits, and the first head that does not fit ends that queue's turn,
// so a large request is not passed by the small ones behind it.
func (c *Controller) Schedule(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	var failed error
	for _, environment := range c.environmentsWaiting() {
		for _, queue := range c.queues(environment) {
			failed = errors.Join(failed, c.scheduleQueue(ctx, environment, queue, now))
		}
	}
	return failed
}

// environmentsWaiting is every environment with a sandbox in a queue.
func (c *Controller) environmentsWaiting() []string {
	var out []string
	for _, obj := range c.objects {
		if obj.Status.Phase == PhaseQueued && !slices.Contains(out, obj.Status.Environment) {
			out = append(out, obj.Status.Environment)
		}
	}
	slices.Sort(out)
	return out
}

func (c *Controller) scheduleQueue(ctx context.Context, environment, queue string, now time.Time) error {
	line, err := c.line(environment, queue)
	if err != nil {
		return err
	}
	var failed error
	for _, obj := range line {
		if expired(obj, now) {
			failed = errors.Join(failed, c.expire(ctx, obj))
		}
	}
	if c.admits(environment) != nil {
		return failed
	}
	tried := map[string]bool{}
	for {
		line, err := c.line(environment, queue)
		if err != nil {
			return errors.Join(failed, err)
		}
		if len(line) == 0 {
			return failed
		}
		head := line[0]
		// A head this pass already tried and could not move is left where
		// it is rather than tried again, so a write that keeps failing
		// cannot hold the loop.
		if tried[head.Status.ID] {
			return failed
		}
		tried[head.Status.ID] = true
		fits, err := c.makeRoom(ctx, environment, head, c.poolEntries(ctx, environment))
		if err != nil {
			return errors.Join(failed, err)
		}
		if !fits {
			return failed
		}
		if err := c.placeQueued(ctx, head); err != nil {
			failed = errors.Join(failed, fmt.Errorf("placing %s: %w", head.Status.ID, err))
		}
	}
}

// expired reports whether a queued sandbox has waited past its start
// deadline. A deadline that does not parse is one Resolve refused, so it never
// reaches here; it is read as no deadline rather than as one already passed.
func expired(obj v1.Sandbox, now time.Time) bool {
	if obj.Spec.Scheduling.StartDeadline == "" {
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
// step 3. It takes the slow path: a pool entry is adopted only by a create
// placed at once, under that create's own lock.
func (c *Controller) placeQueued(ctx context.Context, obj v1.Sandbox) error {
	started := c.clock.Now()
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	lifecycle, err := c.lifecycleFor(obj)
	if err != nil {
		return err
	}
	obj.Status.Phase = driver.Pending
	if err := c.persist(ctx, obj, MutationStatus); err != nil {
		return err
	}
	out, err := c.realize(ctx, obj, d, lifecycle, nil, c.parentOf(obj), true)
	c.observeCreate(out, PoolMiss, started)
	return err
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
