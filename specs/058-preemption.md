---
title: "Preemption: a higher head stops preemptible sandboxes, a victim waits again in its place, and the bound on how often"
status: in-progress
track: core
depends_on:
  - specs/020-scheduling-and-sets.md
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
  - specs/009-events.md
  - specs/017-observability.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/038-environment-pools.md
  - specs/.archive/057-scheduling-queue.md
affects: [controller/, manifest/v1/, internal/config/, internal/metrics/, internal/events/, internal/api/, cmd/cellad/, docs/, CHANGELOG.md]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Preemption

## Overview

A queued environment admits sandboxes by priority
([[057-scheduling-queue]]), but only as capacity frees: a head of
priority 10 waits behind running sandboxes of priority 0 for as long as
they run. [[020-scheduling-and-sets]] lets a manifest declare
`scheduling.preemptible: true`, which says the sandbox may be stopped to
make room for a higher one. This slice acts on it. When the head of a
queue does not fit, the loop stops preemptible sandboxes of lower
priority on the same environment until it does, requeues each with its
place in line, and counts how often one sandbox was stopped so that
none is stopped forever.

The slice also proves the pool row [[057-scheduling-queue]] left to
[[020-scheduling-and-sets]]: a prewarmed entry is never adopted by a
create the authorizer refused.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `spec.scheduling.preemptible` and `.priority` | resolved, stored and immutable in `manifest/scheduling.go`; `Limits.MaxPriority` caps the priority | nothing reads `preemptible` |
| The loop | `controller/scheduler.go`: per queue, in the order of queue names; a head that does not fit ends its queue's turn | no victim is ever chosen |
| The phase machine's `Stopped --> Queued` edge with `Scheduled Preempted` | [[005-lifecycle-controller]] | no code writes it |
| `Preempted` in the transition enum | [[009-events]] | `internal/events.Reasons` lacks it, so a record carrying it would read `DriverFailed`; `NoCapacity` and `StartDeadline` are missing the same way |
| `CELLA_MAX_PREEMPTIONS` | listed with its default `3` in [[002-repository-scaffold]] | not read |
| `cella_preemptions_total` | declared in `internal/metrics/table.go` with `Await: "020"` | registered by nothing |
| A pool behind the authorizer | holds because the API decides before `Controller.Create` | no test proves it; the adoption counter moves before the owner's count is checked |

## Design

### Victims

On a queued environment, when the head of a queue does not fit after
the pool entries have given way ([[038-environment-pools]]), the loop
looks for victims. A candidate is a desired sandbox on the same
environment that is `Running`, sets `spec.scheduling.preemptible`, has a
`spec.scheduling.priority` strictly below the head's, and has been
preempted fewer than `CELLA_MAX_PREEMPTIONS` times. Candidates are
ordered lowest priority first, then largest requested cpu, then newest
`createdAt`, then id, so two passes over one state choose the same
victims.

The loop takes the shortest prefix of that order whose cpu, memory and
slots, given back, make the head fit. A victim gives back its cpu, its
memory and its slot and keeps its disk, so a head that is short of disk
stops nobody. Where the whole candidate list would not make the head
fit, nobody is stopped and the head blocks its queue as it did before:
a stop that places nothing is a loss with no gain.

### The stop and the requeue

For each victim, in order: `Driver.Stop`, then one write that moves the
sandbox to `Queued` with `status.reason` `Preempted`, the `Scheduled`
condition `False` with reason `Preempted`, and `status.preemptions` one
higher. The write is the `sandbox.stopped` record of [[009-events]] with
reason `Preempted`. The stop and the requeue are one write rather than
two, so no crash can leave a preempted sandbox `Stopped` and outside
every queue. A crash between the driver's stop and the write leaves the
desired row `Running`; the next pass chooses it again and stops it
again, which a driver answers as a stop of a stopped sandbox, a no-op.

`createdAt` is not touched. It is the sandbox's `enqueued_at`
([[057-scheduling-queue]]), so a victim waits in its own queue at the
place its arrival gave it among sandboxes of its priority.
`cella_preemptions_total` counts each victim. Once every victim is
written, the pool entries give way as the fit needs and the head is
placed.

### A requeued sandbox

A preempted sandbox waits in `Queued` like one that never ran, and a
driver still holds it, stopped, with its workspace. `status.preemptions`
above zero on a `Queued` sandbox is exactly that case: the only way
back into `Queued` from a phase a driver holds is a preemption.

| Concern | A sandbox that never ran | A requeued sandbox |
|---|---|---|
| Capacity | holds nothing | holds its disk, the rule of a `Stopped` sandbox |
| Placement | the create from step 3 ([[057-scheduling-queue]]) | `Driver.Start`, with the boundary, the token and the workspace it already had; `Scheduled` turns `True` with `Placed` |
| A driver that no longer has it at placement | not possible | written `Lost`, and the lost rule of [[005-lifecycle-controller]] takes it |
| `startDeadline` | fails it with `StartDeadline` | does not apply: the sandbox started once |
| The reaper | never sees it | the `expired` rule deletes it at its `ttl`, counted from its first placement; `autoDelete` does not fire, because a sandbox waiting to run again is not one stopped for good; the token rule renews it as it renews a `Stopped` one |
| `start`, `stop`, `exec` | `phase_conflict` | `phase_conflict` |
| Delete | removes the desired row | removes the desired row and the driver's object |

### The count

`status.preemptions` is how many times the loop stopped this sandbox to
place another. It is a status field because the bound has to survive
what the loop survives. The loop runs on whichever replica holds the
`scheduler` lease, so a count in process memory would reset on a
restart or a failover and a sandbox could be stopped without end. An
annotation is the caller's own field. A table of its own would have to
be written in step with the desired row on every stop, placement and
delete, which is why [[057-scheduling-queue]] dropped the queue table.
`status` is written by the controller alone, ignored on apply, and
stored whole with the desired row by every store, so the field needs no
migration.

`CELLA_MAX_PREEMPTIONS` is a whole number from `0` to `100`, default
`3`; `0` makes no sandbox a victim. `controller.Options.MaxPreemptions`
carries it: zero takes the default, as every bound of `Options` does,
and `controller.NoPreemptions` is the zero an operator asked for.

### One pass, one order

A pass merges an environment's queues by the order of
[[057-scheduling-queue]]: at each step it tries the head that ranks
first among the heads of the queues still open, and a head that does
not fit and can preempt nobody closes its queue for the rest of the
pass. Priorities tried in one pass therefore never rise, and because a
victim's priority is strictly below its head's, a pass never stops a
sandbox it placed. A pass that served queues one after another by name
would place a low head in the first queue and stop it for a higher head
of the next.

A create written `Queued` wakes the loop, so a head that preemption can
place does not wait for the next tick.

### The pool behind the authorizer

The API asks the authorizer for `sandbox.create` and `environment.use`
before it calls the controller, and the controller checks the owner's
`max_sandboxes` from that decision before step 1 of the create order; a
pool entry is adopted only after both. The adoption counter moves once
the create's outcome is known: a create refused for the owner's count or
a name already taken is no adoption and no miss, and one whose adoption
was lost to another adopter and took the slow path is a miss.

## Not in this slice

A recovery that queues on a full environment, `capacity: auto` and its
headroom, `controller.Options.Scheduler` as a seam, and the
`SandboxSet` kind stay with [[020-scheduling-and-sets]]. Preemption
across environments, and of a sandbox that is not `Running`, are not
designed: a victim is a running sandbox on the head's own environment.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Victims are the running, preemptible, lower priority sandboxes of the environment, chosen lowest priority, then largest cpu, then newest, and only as many as the head needs; none is stopped for a head the whole list would not fit, or one short of disk | `TestPreemption`, `TestPreemptionStopsOnlyWhatTheHeadNeeds` | not built |
| A victim is stopped through its driver and written `Queued` with `Scheduled Preempted`, its count and its `createdAt`; its disk stays counted; the stopped record carries `Preempted`; the counter moves per victim | `TestPreemption`, `TestReasonOfHoldsTheEnum` | not built |
| After `CELLA_MAX_PREEMPTIONS` a sandbox is no longer a victim, a bound of zero preempts nothing, and the count survives a restart | `TestPreemptionIsBounded`, `TestPreemptionSurvivesARestart` | not built |
| A requeued sandbox is placed again by `Start` with its workspace, is `Lost` when its object is gone, is passed over by `startDeadline` and `autoDelete`, is deleted at its `ttl`, and a delete removes its object | `TestARequeuedSandboxResumes`, `TestARequeuedSandboxKeepsItsDeadlines` | not built |
| One pass merges the queues by the order and never stops what it placed; a queued create wakes the loop | `TestAPassNeverPreemptsWhatItPlaced`, `TestAQueuedCreateWakesTheLoop` | not built |
| `CELLA_MAX_PREEMPTIONS` is read with its bounds | `TestLoadScheduling`, `TestPoolConfigRefusals` | not built |
| `cella_preemptions_total` is registered and moves per victim | `TestMetricsTable`, `TestAwaitingRowsAreRegisteredByNobody`, `TestCountersRecordWhatTheyOwn` | not built |
| A pool never serves a create the authorizer refused, at `sandbox.create`, at `environment.use`, or at the owner's count, and a refused create moves no adoption counter | `TestPoolIsBehindTheAuthorizer`, `TestARefusedCreateIsNoAdoption` | not built |
| `cellad serve` on a queued environment of one slot: a higher create stops a preemptible one, which runs again with its files once the higher one is deleted, and the scrape counts it | `TestPreemptionEndToEnd` | not built |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates` | not built |
