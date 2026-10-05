---
title: "Running sandbox count: the count ceiling holds the sandboxes that run or will run without a start, a stopped, failed or deleting one holds no slot, and a start is checked under the controller's lock as a create is"
status: complete
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/007-admission.md
  - specs/008-api.md
affects: [controller/, internal/api/, authorizer/, test/conformance/, api/openapi.yaml, docs/api.md, docs/plane.md, docs/scheduling.md, CHANGELOG.md]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Running sandbox count

## Overview

The count ceiling of [[007-admission]] bounds how many sandboxes one
subject holds, and it counts every desired sandbox whose phase is not
`Deleting`, a stopped one included. A client that keeps one sandbox per
conversation and lets it stop when idle reaches the ceiling with
sandboxes that run nothing: each holds only its workspace volume, and
the subject is refused a new one until a lifetime rule deletes an old
one. The ceiling exists to bound what runs. This spec counts only the
sandboxes that run or will run without anyone asking for a start, and
moves the check a stopped sandbox escaped onto its start.

## Current state

| Piece | Where | Today |
|---|---|---|
| The count | `createLocked` in `controller/controller.go` | every sandbox of the owner in `c.objects` whose phase is not `Deleting`, under `c.mu`, refused with `ErrQuota` at `max` |
| The figure | `authorizer.Limits.MaxSandboxes` | read from the allow of `sandbox.create`, for a create and a spawn |
| A start | `Controller.Act(ctx, id, "start")` | reads the driver, refuses anything but `Stopped` with `ErrPhase`, starts, reads again and writes the read; no count |
| The route | `POST /v1/sandboxes/{id}/start` in `internal/api` | decided as `sandbox.update`; the decision's limits are dropped |
| The sentence | `quota_exceeded`, 422 | "You have reached your sandbox limit." |

## Design

### Which phases count

The count is over the phase this control plane last wrote for each of
the owner's sandboxes. A phase counts unless it is one of three:

| Phase | Counts | Why |
|---|---|---|
| `Pending`, `Starting`, `Running`, `Stopping` | yes | the runtime holds the workload, is bringing it up, or has not yet brought it down |
| `Queued` | yes | the scheduler places it, and resumes one it preempted, with no start anyone asks for, so it keeps the slot its create or its preemption held |
| `Lost`, `Recovering` | yes | on a durable store the reaper recreates it and it runs again with no start; on a snapshot store the lost rule deletes it |
| `Stopped` | no | nothing runs; it holds a name and a workspace, and a start, which is checked, is the only way back to running |
| `Failed` | no | nothing runs, and a start on `Failed` is `phase_conflict`: it never runs again |
| `Deleting` | no | it is on its way out |

A phase the table does not name counts, so a driver's phase this
control plane does not know errs toward refusing. `holdsCompute`, the
capacity rule of [[020-scheduling-and-sets]], answers a different
question, what the environment's resources hold now, and does not count
a queued or a lost sandbox; the count asks what will run without a check.

### Where the phase comes from

The controller writes a sandbox's phase on every transition it makes:
the create's settle, a start, a stop, `autoStop`, a preemption and its
resume, recovery, and a delete. A transition the runtime makes on its
own, a main process that exits, is not written until the next act on
the sandbox, because nothing watches the runtime yet ([[005-lifecycle-controller]]).
Such a sandbox counts as running until then. The count can therefore
hold a slot that no longer runs, never the reverse: every path to
running passes the check or keeps a slot it already held.

### The start

`Controller.Start(ctx, id, max)` is a start with the owner's ceiling, as
`Create(ctx, obj, owner, max)` is a create with it. Under the
controller's lock it:

1. refuses a sandbox whose create is in flight with `ErrPhase`, as `Act`
   does;
2. reads the runtime and refuses any phase but `Stopped` with
   `ErrPhase`;
3. counts the owner's other sandboxes by the table above, and refuses
   the start with `ErrQuota` when the count is at or above `max`, zero
   being no ceiling;
4. asks the runtime to start it;
5. reads it again and writes the read. A read that fails, or that still
   says `Stopped` after the runtime took the start, writes `Starting`,
   the phase design 005 gives a start, so the record holds the slot
   whatever the read said;
6. stops it again when the write fails, so no sandbox runs that the
   record does not count.

The lock orders two starts racing for the last slot: the first writes
its record before it releases the lock, and the second counts it.

`Act` takes `stop` and `delete`. A `start` passed to it is refused with
`ErrPhase` naming `Start`, so no caller of the package starts a sandbox
past a ceiling by leaving the figure out.

### The route

`POST /v1/sandboxes/{id}/start` is decided as `sandbox.update`, as
before, and passes that allow's `limits.max_sandboxes` to `Start`. An
authorizer carries the figure on the allow of `sandbox.update` as well
as on the allow of `sandbox.create`; an allow of `sandbox.update`
without it is no ceiling on the start. The count is the sandbox's
owner's, which for a spawned child is the root's owner
([[022-mesh-and-spawn]]).

### The error

A refused start is `quota_exceeded`, 422, as a refused create is. The
code keeps one fixed sentence, now true of both: "You have reached your
limit of running sandboxes. Stop one to start another." The developer
detail names the count and the ceiling: for a create, how many the
owner runs of the ceiling; for a start, the sandbox and the count the
start would make.

### What bounds a stopped sandbox

The count no longer does. A stopped sandbox is bounded by its lifecycle
alone: `ttl`, whose `Expired` rule deletes a sandbox in any phase, and
`autoDelete`, which deletes one stopped that long ([[005-lifecycle-controller]]).
The manifest sets them, or an installation's admission endpoint does,
which is where a plan's lifetime belongs ([[007-admission]]);
`CELLA_MAX_TTL` is loaded by nothing yet. Where neither is set, a
stopped sandbox stays until it is deleted and keeps its workspace
volume. An installation that bounds what its subjects keep sets `ttl` or
`autoDelete` through admission.

## Not in this spec

| Item | Why |
|---|---|
| A second ceiling on stopped sandboxes | the lifetime rules bound them; a ceiling on what does not run is a disk policy an admission endpoint already expresses through `ttl` and `autoDelete` |
| Writing a runtime's own transitions | the watch of [[005-lifecycle-controller]]; until it is built a self-exited sandbox counts until its next act |
| The environment's capacity | [[020-scheduling-and-sets]] counts resources, not subjects, and is unchanged |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A create counts the owner's `Pending`, `Queued`, `Starting`, `Running`, `Stopping`, `Lost` and `Recovering` sandboxes and an unknown phase, and not the `Stopped`, `Failed` and `Deleting` ones | `TestTheCountHoldsWhatRunsOrWillRun` | built |
| A start past the ceiling is refused with `ErrQuota` and leaves the sandbox stopped; a start under it runs and holds a slot | `TestAStartPastTheCeilingIsRefused` | built |
| A stop frees a slot for a create and for a start | `TestAStopFreesASlot` | built |
| Two starts racing for the last slot start one, on a runtime whose read still says `Stopped` after it took the start | `TestTwoStartsRaceForOneSlot` | built |
| A start whose record cannot be written is stopped again and stays `Stopped` in desired state | `TestAStartThatCannotBeRecordedIsStoppedAgain` | built |
| `Act` refuses a start | `TestActTakesNoStart` | built |
| Over the API a start reads the figure from the allow of `sandbox.update`, and a refused start is 422 `quota_exceeded` with the fixed sentence and a detail naming the count; a stopped sandbox no longer counts toward a create | `TestCountCeilingCountsRunningSandboxes`, `TestAStartPastTheCeilingIsQuotaExceeded` | built |

## Outcome

Built as designed on 2026-10-05. `holdsSlot` is the phase table and
`countLocked` the count, shared by the create and by
`Controller.Start`; the route passes the allow of `sandbox.update` to
the start. Every criterion has its test, and each new controller test
fails against the count and the start before this change.

Two points the design fixed while it was built. A start whose read
after the runtime took it still says `Stopped`, or fails, is written
`Starting`, because the record is what the next count reads: without
it two starts racing for the last slot on such a runtime both pass,
which `TestTwoStartsRaceForOneSlot` shows with a runtime that lags. The
start's own read failing is no longer the caller's error once the
runtime took the start; it is logged and the record written as above,
as the scheduler's resume of a preempted sandbox already did.

Removing `start` from `Act` changes the `controller` package's surface
for an importer: a start through `Act` is `ErrPhase` naming `Start`.
The change log says so. The user sentence of `quota_exceeded` changed
with it, and a plane that shows Cella's messages, or quotes the table
in its own documents, carries the new one.

