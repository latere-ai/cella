---
title: "Scheduling queue: the queued mode, capacity by resource, the queue and its loop, the scheduling fields"
status: complete
track: core
depends_on:
  - specs/020-scheduling-and-sets.md
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
  - specs/010-state.md
  - specs/017-observability.md
  - specs/021-data-plane-workers.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/038-environment-pools.md
  - specs/.archive/054-environments-desired-state.md
affects: [manifest/, manifest/v1/, controller/, internal/config/, internal/metrics/, cmd/cellad/, docs/, CHANGELOG.md]
effort: large
created: 2026-09-22
updated: 2026-09-23
author: changkun
---

# Scheduling queue

## Overview

An environment runs in one of two modes ([[020-scheduling-and-sets]]).
`direct` starts a sandbox now or refuses it; `queued` holds a sandbox
the environment cannot fit yet and starts it when capacity frees, in
priority and fair-share order. Today only `direct` exists, and only in
its count form: an environment declares a ceiling on the number of
sandboxes, [[038-environment-pools]] enforces it, and an environment
applied with `mode: queued` is refused.

This slice builds the queued mode and the resource form of capacity.
A manifest on a queued environment may set `spec.scheduling`; the
controller places a sandbox now when its queue is empty and its
resources fit, and otherwise writes it `Queued` and stops the create
after step 2 of [[005-lifecycle-controller]]'s order. A loop under the
`scheduler` lease resumes each head at step 3 when it fits, and fails
one that waited past its `startDeadline`. A direct environment that
cannot fit a create writes it `Failed` with reason `NoCapacity`, which
is the transition [[005-lifecycle-controller]]'s machine names, in
both the count form and the resource form.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `Environment.spec.scheduling` with `mode`, `queues`, `defaultQueue` | `manifest/v1/environment.go`, validated in `manifest/environment.go` | `mode: queued` is `capability_unsupported` |
| Capacity, count form | `countsAgainstCapacity` and `makeRoom` in `controller/pool.go` | cpu, memory and disk are declared and never read; a create past the count is refused `quota_exceeded`, whose sentence is about a caller's own limit, rather than written `Failed NoCapacity` |
| `CELLA_SCHEDULING_MODE` | `internal/config/pool.go` | `queued` falls back to `direct` |
| `spec.scheduling` on a Sandbox | nowhere | the field, its rules, and `Limits.MaxPriority` beyond the zero case |
| The `Queued` phase and the `Scheduled` condition's `Queued` reason | the vocabulary is in [[003-manifest-contract]] and [[005-lifecycle-controller]] | no code writes either |
| `store.Queue` and the `queue` table | an interface in `internal/store/store.go` and a table in migration 1 | no adapter implements either, and this slice replaces them |
| `cella_queue_depth`, `cella_capacity` | declared in `internal/metrics/table.go` | awaiting [[020-scheduling-and-sets]] |

## Design

### The scheduling fields

`spec.scheduling` is `{priority, queue, startDeadline, preemptible}`,
the rows of [[003-manifest-contract]]'s field table, resolved against
the environment the manifest names:

| Field | Rule |
|---|---|
| any | on a `direct` environment, a set field is `capability_unsupported` at its own path |
| `priority` | an integer, `0` or above, else `invalid_field`; above a positive `Limits.MaxPriority` is `ceiling_exceeded` |
| `queue` | defaults to the environment's `defaultQueue`; one of its `queues`, else `invalid_field` |
| `startDeadline` | a duration, not `never`, else `invalid_field`; absent waits without bound |
| `preemptible` | a boolean; carried and not yet acted on, preemption is its own slice |

Every field is immutable on an update ([[003-manifest-contract]]). The
golden corpus resolves against a queued environment, so the fields
are covered by an accepted entry and an unknown queue by a refusal.

### Capacity by resource

`spec.capacity` declares `cpu`, `memory`, `disk` and `sandboxes`; an
undeclared quantity bounds nothing. In use is derived from the desired
sandboxes on the environment, never stored:

- `sandboxes`, `cpu` and `memory` sum over `Pending`, `Starting`,
  `Running`, `Stopping` and `Recovering`;
- `disk` sums over those and over `Stopped` and `Failed`, because a
  stopped sandbox's workspace stays on the substrate until `Delete`;
- `Queued`, `Lost` and `Deleting` hold nothing.

A pool entry counts at `spec.pool.resources` and one sandbox. A create
fits when every declared quantity holds its request on top of what is
in use; where entries hold what it needs, the oldest give it up, the
rule [[038-environment-pools]] already applies to the count. The sum
is over resolved manifests, which always carry `resources` because
`Defaults` fills them, so a driver reporting its grant is not needed.
`capacity: auto` bounds nothing in this slice.

### Placement

`Place` is step 2 of the create order, run under the controller's lock
after the desired state is written:

| Mode | Queue empty and fits | Otherwise |
|---|---|---|
| `direct` | continue at step 3 | written `Failed` with reason `NoCapacity`; the create answers 201 with that object |
| `queued` | continue at step 3 | written `Queued`; the create answers 201 with that object |

Both answers are the object, because both are states the sandbox is in
rather than refusals of the request: the caller reads the phase, and a
`Failed` sandbox holds its name and counts toward its owner's count
until it is deleted, as one that failed at the driver does. It holds
no capacity. Nothing past step 1 ran, so there is no map, no token and
no driver object to undo. The count form of [[038-environment-pools]]
follows the same rule, so a direct environment answers the same way
whichever quantity is short.

A sandbox joins the queue its `spec.scheduling.queue` names, and a
non-empty queue places nothing ahead of it, so an arrival that would
fit cannot pass a head that does not. A spawned child runs where its
parent runs and joins that environment's queue like any create. A pool
entry is adopted only when a create is placed at once; a sandbox the
loop places takes the slow path, which [[020-scheduling-and-sets]]'s
`Place` does not require and which keeps the adoption under the
create's own lock.

A `Queued` sandbox carries `conditions[Scheduled]` with status `False`
and reason `Queued`. Its message is the position in its queue,
computed on every read from what the controller holds, so a dequeue
rewrites no other row.

### The loop

`RunScheduler` ticks every `CELLA_SCHEDULE_INTERVAL` (default `5s`) and
on every wake, and acts only while this replica holds the `scheduler`
lease. A sandbox leaving a phase that holds capacity wakes it: a stop,
a delete, a failure, a loss, and an environment whose capacity was
raised by an apply. Each tick passes over every `Ready` environment in
the `queued` mode and every queue it declares:

1. A waiting sandbox older than its `startDeadline` is written `Failed`
   with reason `StartDeadline`.
2. The rest are ordered by `priority` descending, then by the smallest
   sum of requested CPU its subject holds on the environment in the
   phases that count, then by `createdAt`.
3. While the head fits, it moves `Queued` to `Pending` and the create
   resumes at step 3: the boundary, the token and the driver's create,
   each undone as [[005-lifecycle-controller]] states when a later one
   fails. The first head that does not fit ends the queue's turn.

### The queue is desired state

The queue is the set of desired sandboxes in `Queued`, ordered by the
controller on each tick. [[010-state]] specified a `queue` table read
by `Dequeue`, and [[020-scheduling-and-sets]] put the order in that
call; this slice changes both. A second row per waiting sandbox would
have to be written in step with the desired row on every create,
placement, deadline and delete, and a restart or a failover would have
to reconcile the two. Capacity in use is already derived rather than
stored for the same reason. `createdAt` is the sandbox's `enqueued_at`:
it is written once at step 1, so a place in line is never recomputed.
`store.Queue`, `store.QueueItem` and the `queue` table are removed, the
table by a migration of its own, and 010 and 020 are amended to say
the queue is the `Queued` rows.

### What a queued sandbox is not

No driver holds a `Queued` sandbox. A read returns the desired record
without asking a driver, the lost rule passes over it, and the reaper
never sees it. `start`, `stop` and `exec` are `phase_conflict`; a
delete removes the desired record with nothing to undo. Its `ttl`
counts from the create that places it, which is when the driver
receives it.

### Configuration and metrics

`CELLA_SCHEDULING_MODE=queued` seeds the default environment in the
queued mode. `CELLA_SCHEDULE_INTERVAL` sets the tick. `cellad serve`
runs the loop beside the reaper and the pool loop.
`cella_queue_depth{environment, queue}` is the number waiting, and
`cella_capacity{environment, resource, kind}` is each declared
quantity (`kind="declared"`) beside its sum in use (`kind="used"`),
per [[017-observability]].

## Not in this slice

Preemption and `CELLA_MAX_PREEMPTIONS`; `preemptible` is resolved and
stored and not read. `capacity: auto` and `CELLA_CAPACITY_HEADROOM`,
which need the k8s driver to report allocatable. A recovery that
queues on a full environment: `Recovering` re-takes capacity as the
count form already does. `controller.Options.Scheduler` as a seam a
platform replaces. The `SandboxSet` kind. A pool that is never offered
a create the authorizer refused, which holds today because the
authorizer decides before the controller is called, and whose test
belongs with the pool rows of [[020-scheduling-and-sets]].

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every row of the scheduling field table has a refusing case, a queued environment defaults the queue, and each field is immutable on an update | `TestSchedulingFields`, `TestSchedulingIsImmutable` | built |
| The golden corpus sets every scheduling field on an accepted entry and refuses an unknown queue | `TestGoldenCorpus`, `TestCorpusCoversTheSchema` | built |
| An environment applied with `mode: queued` is accepted, and `CELLA_SCHEDULING_MODE=queued` seeds the default in that mode | `TestEnvironmentAcceptsTheQueuedMode`, `TestLoadScheduling` | built |
| In use follows the phases above per quantity, a pool entry counts at its resources, an undeclared quantity bounds nothing, and a restart does not count twice | `TestCapacityCountsThePhases`, `TestCapacityInUse`, `TestPoolYieldsCapacityByResource`, `TestCapacitySurvivesARestart` | built |
| A direct environment writes a create that does not fit by any quantity, the count included, `Failed` with reason `NoCapacity` and pushes no map, mints no token and calls no driver; a queued one answers 201 `Queued` with the position | `TestDirectFailsWhatDoesNotFit`, `TestQueuedCreateWaits`, `TestSchedulingOverHTTP` | built |
| The `queue` table is dropped by a migration with its reverse, and a migrated database holds no such table | `TestEveryMigrationIsReversible`, `TestTheQueueTableIsGone` | built |
| The loop acts only under the `scheduler` lease, at start, on the tick and on a wake, and a placed sandbox receives its boundary and token at placement and not before | `TestSchedulerLoop`, `TestPlacementResumesAtTheBoundary`, `TestSchedulerHoldsWhatItCannotPlace`, `TestTheLoopPassesAtStart` | built |
| The loop admits by priority, then the subject with the smallest CPU sum, then arrival, across three subjects and mixed priorities, and a head that does not fit blocks its queue | `TestSchedulerHonorsQueueOrder`, `TestAHeadThatDoesNotFitBlocks` | built |
| A sandbox waiting past its `startDeadline` is `Failed` with reason `StartDeadline` | `TestStartDeadline`, `TestADeadlineIsReadAsResolveLeftIt` | built |
| A queued sandbox is read without a driver, is never lost or reaped, refuses `start`, `stop` and `exec` with `phase_conflict`, and deletes | `TestAQueuedSandboxIsDesiredStateOnly`, `TestSchedulingOverHTTP` | built |
| A queued sandbox survives a restart and is placed by the loop afterwards | `TestTheQueueSurvivesARestart` | built |
| `cella_queue_depth` and `cella_capacity` are registered and report the queue and the sums | `TestSchedulerMetrics`, `TestPullGaugesReadTheirIndex`, `TestQueuedEnvironmentEndToEnd` | built |
| `cellad serve` with `CELLA_SCHEDULING_MODE=queued` and a capacity of one sandbox: a second create is `Queued`, and it runs once the first is deleted | `TestQueuedEnvironmentEndToEnd` | built |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates` | built |

## Outcome

The queued mode, capacity by resource and the scheduling fields are
built, and the queue is desired state.

| Piece | Where |
|---|---|
| `spec.scheduling`, its four rules, the queue default and the priority ceiling | `manifest/v1/sandbox.go`, `manifest/scheduling.go` |
| The queued mode accepted on the `Environment` kind and from `CELLA_SCHEDULING_MODE`, and `CELLA_SCHEDULE_INTERVAL` | `manifest/environment.go`, `internal/config/pool.go` |
| Capacity in use per quantity, the fit, and the pool entries that give way | `controller/capacity.go` |
| Step 2 of the create order, the split at step 3, the loop, the order, the deadline, the wake and the position | `controller/scheduler.go`, `controller/controller.go` |
| A queued sandbox kept from every driver path | `controller/controller.go` (read), `controller/registry.go` (operations), `controller/recovery.go` (lost rule) |
| `cella_queue_depth`, `cella_capacity` and the `scheduler` lease label | `internal/metrics/`, `cmd/cellad/telemetry.go` |
| The loop beside the reaper, the pool and the phase loop | `cmd/cellad/main.go` |
| The `queue` table and `store.Queue` removed | `internal/store/store.go`, migration `000004_drop_queue` |
| The operator's page | `docs/scheduling.md` |

Coverage on `go test -cover`: `manifest` 95.8%, `controller` 90.9%,
`internal/config` 91.5%, `internal/metrics` 100%, `internal/store`
90.3% with memory 93.2%, postgres 91.8% and the migrations 94.1%,
`internal/api` 91.8%, `cmd/cellad` 90.4%. `go test -race` is clean
over all of them. The end-to-end that ran is `TestQueuedEnvironmentEndToEnd`:
`cellad serve` on the native driver, queued with room for one sandbox,
a second create answered `Queued` with its place and read back from the
scrape, and placed by the wake the first one's delete sent, well inside
one tick.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[010-state]], [[020-scheduling-and-sets]] | a `queue` table read by `Store.Queue.Dequeue`, which orders | the `Queued` desired rows, ordered by the controller on each pass; the table dropped | one record per waiting sandbox; the two specs are amended |
| [[038-environment-pools]] | a direct create past the count refused `quota_exceeded` | written `Failed NoCapacity` and answered 201, in both forms | [[005-lifecycle-controller]]'s machine names the transition, and `quota_exceeded` reads as the caller's own limit |
| [[020-scheduling-and-sets]] | disk held by every `Stopped` and `Failed` sandbox | not by one that failed before any driver held it (`NoCapacity`, `StartDeadline`) | nothing of it is on the substrate |
| [[020-scheduling-and-sets]] | `Place` may answer `Adopt` for a dequeued head | a placement from the loop takes the slow path | the adoption stays under the create's own lock and its retry on a lost entry |

### What this leaves open

| Open | Why |
|---|---|
| Preemption, `CELLA_MAX_PREEMPTIONS`, and reading `preemptible` | its own slice; the field is resolved and stored |
| `capacity: auto` with `CELLA_CAPACITY_HEADROOM` | needs the k8s driver to report allocatable; `auto` bounds nothing |
| A recovery that queues on a full queued environment | `Recovering` re-takes capacity as the count form did |
| The refill loop's target by resource | it sizes by the count, so entries past a resource ceiling are prewarmed and then given up to the first create that needs the room |
| `controller.Options.Scheduler` as a seam a platform replaces | the built-in scheduler is the controller's own |
| The lesser of `spec.capacity` and what a worker environment's live workers report | [[021-data-plane-workers]]; `spec.capacity` alone is read |
| The `SandboxSet` kind and results collection | the next slice of [[020-scheduling-and-sets]], after [[019-volumes]] |
