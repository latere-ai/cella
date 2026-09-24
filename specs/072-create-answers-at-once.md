---
title: "A create answers at once: 201 with the sandbox Pending, the create order finished by the scheduler loop, ?wait=1 for the held answer, and no gateway refused at admission"
status: drafted
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/011-agent-client.md
  - specs/018-egress-and-secrets.md
  - specs/020-scheduling-and-sets.md
  - specs/022-mesh-and-spawn.md
  - specs/.archive/038-environment-pools.md
  - specs/.archive/057-scheduling-queue.md
  - specs/.archive/069-client-package.md
affects: [controller/, internal/api/, client/, internal/cellacli/, test/conformance/, cmd/cellad/, api/openapi.yaml, docs/, CHANGELOG.md]
effort: large
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# A create answers at once

## Overview

`POST /v1/sandboxes` answers once the driver's create has returned and
the first read of the sandbox is written. On the Kubernetes driver that
read waits for the Pod to be ready, and a create that provisions a
volume and attaches it to a node takes as long as the two together: one
create measured 15.7 seconds, 3 of them provisioning and 9 attaching. A
console holds its page on a spinner the whole time, and a client whose
timeout is shorter than that fails a create that succeeds.

A create now answers `201` as soon as its desired state is written, with
the sandbox `Pending`. The steps of the create order after the write run
in the controller's scheduler loop, the one that already places a queued
sandbox: the boundary push, the identity mint, the driver's create and
the first read. The sandbox reaches `Running`, or `Failed` with its
reason, through the same writes and the same journal as before, and a
reader of the object or of its feed sees it happen. A caller that wants
the answer held until then asks for it with `?wait=1`.

A create whose boundary needs an egress gateway while none is connected
is refused on the request, as before, and now with a code of its own,
`egress_gateway_unavailable`, rather than `driver_unavailable`, which
told the caller the environment was down.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| The create order | `create`, `createLocked` and `realize` in `controller/controller.go`: steps 1 and 2 under the controller's lock, then `realize` on the same call, under the same lock | an answer before `realize`, and a lock that is not held across the driver's create |
| The queued path | `placeQueued` in `controller/scheduler.go`: the loop writes `Pending` and runs `realize` under the lock of the pass | the direct path using it |
| A crash between step 1 and the driver's write | the row stays `Pending`; `canBeLost` excludes `Pending`, and nothing else reads it again | a pass that realizes it |
| `ErrAlreadyExists` from a create | `recoverLocked` takes it as the driver holding the object; `realize` takes it as a failure | the create order's idempotence of design 005 on the create path |
| The boundary and the identity in the store before the driver call | written at the end of `realize`, after the driver's create | a write before the driver call, so a second run of `realize` pushes the same map and revokes the token the first minted |
| No gateway connected | `pushEgress` returns `ErrNoGateway`; `errorEnvelope` has no case for it and answers `503 driver_unavailable` | a refusal before step 1 and a code of its own |
| Waiting for `Running` | `cella apply -w` reads the object every 250 ms from the client, with `--timeout` | a server-side hold, the client's option, and the `--wait` spelling |

## Design

### The answer

`POST /v1/sandboxes`, and `PUT /v1/sandboxes/{name}` where it creates,
answer `201` with `Location` and the object, as before. What changes is
the object's phase and when the answer is written:

| The create | The answer | The rest |
|---|---|---|
| placed now, no pool entry adopted | `201`, phase `Pending`, no `Scheduled` condition | the loop runs steps 3 to 7 and the first read |
| placed now by adopting a pool entry | `201`, phase as the first read reports it, `Scheduled True FromPool`, after the adoption, as before | nothing |
| queued | `201`, phase `Queued`, as before | the loop places it, then runs steps 3 to 7 |
| a direct environment that cannot fit it | `201`, phase `Failed`, reason `NoCapacity`, as before | nothing |

Everything that refuses a create still refuses it on the request, with
nothing written: the decode and resolve of the manifest, the authorizer's
`sandbox.create` and `environment.use`, the admission webhook, the
owner's count, a taken name, a spent spawn budget, an environment below
`Ready`, and a boundary that needs a gateway while none is connected.

A pool adoption stays on the request. An adopted sandbox takes the
entry's id, and an adoption another adopter wins between the match and
the adoption falls back to a create under a fresh id. That fallback is
possible only before the caller has read an id. The adoption is the fast
path the pool exists for, so answering after it costs a caller nothing
the pool did not already save.

### The loop finishes the create

A sandbox is placed and not yet realized when its phase is `Pending` and
its `Scheduled` condition is not `True`. `Scheduled True` is written
once the driver's create or adoption has returned, and no driver reports
a `Scheduled` condition, so the predicate separates a sandbox the
control plane has not handed to a driver from one a driver reported as
`Pending` itself. `placeQueued` writes `Pending` while the object still
carries `Scheduled False Queued`, so the predicate covers a dequeued
sandbox as well, and one that was placed by a process that stopped
before handing it on.

`createLocked` writes step 1 for a sandbox placed now with no entry,
wakes the loop and returns. On each pass, `scheduleEnvironment` realizes
every placed and unrealized sandbox of an environment that is `Ready`
before it looks at the queues, oldest `createdAt` first. Such a sandbox
already holds its capacity, since `holdsCompute(Pending)` is true, so it
is not fitted, not ordered in a queue, and not held to a start deadline.
An environment below `Ready` keeps its placed sandboxes `Pending` until
it returns, which is the gate a queued sandbox is held to. The loop runs
under the scheduler lease, so on a replica set the replica that holds
the lease realizes, and a create another replica wrote waits for that
replica's next pass, at most `CELLA_SCHEDULE_INTERVAL`.

`realize` runs in three parts:

1. **Under the controller's lock.** Read the row again; a row that is
   gone, or is no longer placed and unrealized, is left alone. Push the
   boundary to a gateway and mint the identity, as before. Then write
   the row with its boundary record, its identity record and the
   `EgressEnforced` condition as `sandbox.status`, which is journaled
   and never delivered to the sink. After that write the store holds
   what the driver is about to be given, so a second run after a crash
   compiles the same map with the same credential and placeholders, and
   ends the token the first run minted.
2. **With the lock released.** Call the driver's `Create` and read the
   sandbox with `Inspect`. The id is marked realizing for the duration,
   in the process.
3. **Under the lock again.** Settle against the row as it stands now:

| The row | What `realize` does |
|---|---|
| gone: a delete, a cascade or a deadline ended it during the call | deletes the driver's object, purges the principal from the gateways, revokes the token it minted; nothing is written |
| `Deleting`: a delete is in flight and its driver call failed | nothing: the retried delete removes the object |
| otherwise | writes `Scheduled True Placed` and what the read reported onto the row as it stands, as `sandbox.started`, `sandbox.failed` or `sandbox.status` by the phase; records `sandbox.spawned` on the parent of a spawn; revokes the token a previous run minted |

The row as it stands keeps whatever changed during the call: an apply's
new specification, a re-pushed boundary, a parent's spawn count.

A driver `Create` that answers `ErrAlreadyExists` is a create that
reached the driver before a restart and whose result was never written.
The ids are the control plane's own and are never reused, so the object
is this sandbox's, and `realize` takes it the way recovery does: it
projects the newly minted token with `Update` and reads the sandbox. A
create that fails any other way is `Failed` with reason `CreateFailed`,
the principal purged, the new token revoked and the parent's unit
credited, which is the undo of the create order. A boundary that no
gateway acknowledges at the push, because the gateway went away between
admission and the pass, is the same failure, with the condition
`EgressEnforced False NoGateway` on the object so that a reader sees the
cause.

A pass that finds the controller closing leaves the row as the status
write left it, and the next process realizes it.

### While a create is in flight

| Act | Before the pass takes the row | During the driver call |
|---|---|---|
| `DELETE` | `202` with `Deleting`; the row is gone before the pass reads it, and no driver object is made | `202` with `Deleting`; the delete asks the driver to remove whatever exists, and the pass removes what its create made after |
| `POST .../stop`, `.../start` | `phase_conflict`: the sandbox is `Pending` | `phase_conflict`, without asking the driver |
| `PUT` of the name | applied to the row | applied to the row; the pass keeps it |
| A second create | answers at once and is realized on the same or the next pass | answers at once, since no lock is held across the call |
| The token rule | a `Pending` sandbox holds a token from the status write at most, not yet due | not asked of a realizing id |
| The deadline rules | read only a sandbox the driver lists | a deadline that passes deletes the sandbox as a `DELETE` would |

### Restart

A process that stops at any point after step 1 leaves a `Pending` row
whose `Scheduled` is not `True`. The loop's first pass, which runs as
the loop starts, realizes it, from the status write where there is one:

| Where the process stopped | What the pass finds | What it does |
|---|---|---|
| after step 1, before the status write | no boundary record, no identity, no driver object | compiles and pushes a new boundary, mints, creates |
| after the status write, before the driver call returned | the boundary record and the identity's `jti`; maybe a driver object | pushes the same map, mints, and creates, or takes the object on `ErrAlreadyExists`; revokes the first `jti` |
| after the driver call, before the settle write | the same, and the driver object | takes the object on `ErrAlreadyExists` |

Without a durable store the rows die with the process, as they always
did.

### No gateway connected

`create` refuses, beside the environment's phase gate and before step 1,
a manifest whose boundary needs a gateway, which is an egress mode other
than `open`, a denied host, or a mounted secret, while the environment
has no gateway connected: `Options.Egress` is nil or reports
`Connected() == 0`. The refusal is `ErrNoGateway`, no row is written and
no name is taken. The API answers it, and `ErrNoGateway` from an apply's
push, with a row of its own in the error table of design 008:

| Code | Status | Message |
|---|---|---|
| `egress_gateway_unavailable` | 503 | No gateway is connected to enforce this sandbox's egress boundary; connect one, or open the boundary. |

503 because the three `*_unavailable` codes are the ones that name a
missing collaborator; this one names what to do instead of saying retry.

### Waiting: `?wait=1`

`POST /v1/sandboxes?wait=1` and `PUT /v1/sandboxes/{name}?wait=1` hold
the answer until the sandbox has left `Queued`, `Pending` and
`Starting`: it is `Running`, `Failed` with its reason, or any phase the
driver reported after the create. The handler reads the object each
250 ms while it waits, and reads the driver once the loop has realized
it. The hold is bounded like `exec?wait=1`'s: `timeout` in the query, a
Go duration that is positive and at most `1h`, `10m` when absent, and
anything else is `invalid_field`. When the bound passes first, or the
caller hangs up, the answer is the object as it stands. The status is
`201` with `Location` in every case, because the object exists whatever
became of its create; an update answers `200` as before. A sandbox
deleted while the answer is held answers `not_found`.

This differs from the answer a create gave before on a failure: the
driver's error was the answer, a 4xx or 5xx, with a `Failed` object left
behind that the answer did not name. The object and its reason are now
the answer.

### The client and the command

`client.CreateSandbox` and `client.ApplySandbox` take options after the
manifest, `...CreateOption`, so every existing call compiles unchanged.
`client.Wait(timeout)` is the one option: it sends `wait=1` and, for a
positive duration, `timeout`. A caller whose `http.Client` has a timeout
shorter than the hold loses the answer and not the sandbox.

`cella apply` spells the flag `-w` and `--wait`, one flag. It sends
`client.Wait` with the remaining `--timeout`, and then reads the object
as before until it is `Running` or the timeout passes, which is what a
server that ignores `wait` needs. `--timeout` stays at `2m`.

## Not in this slice

Realizing two placed sandboxes at once: the loop realizes one at a time,
so a second create waits for the first's driver call before its own
begins, as creates did while they held the lock; it answers at once
either way. The lock is still held across the boundary push, which waits
for one gateway's acknowledgment, as every push does; holding it keeps
two pushes of one sandbox from crossing. A `Ready` condition written by
the controller. A watch that writes the driver's later phases without a
read.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A create placed now answers `Pending` before its driver's create begins, the loop takes it to `Running`, and the journal holds `sandbox.created`, `sandbox.status`, `sandbox.started` | `TestCreateAnswersBeforeTheDriver` | not built |
| The loop runs the driver's create without the controller's lock: a second create, a read and a list answer while the first driver call is held open | `TestTheDriverCallHoldsNoLock` | not built |
| A driver create that fails leaves the sandbox `Failed CreateFailed`, the principal purged, the token revoked and a spawn parent's unit credited | `TestAFailedRealizeUndoes` | not built |
| A gateway that acknowledges nothing at the loop's push fails the sandbox with `EgressEnforced False NoGateway` | `TestAGatewayLostAfterAdmissionFails` | not built |
| A manifest whose boundary needs a gateway, with none connected, is `ErrNoGateway` at the create, with no row and no name taken; the API answers it `503 egress_gateway_unavailable` | `TestNoGatewayIsRefusedAtAdmission`, `TestNoGatewayHasItsOwnCode` | not built |
| A fresh controller over a store holding a placed, unrealized sandbox realizes it on the loop's first pass: with no driver object it creates one; with one, `ErrAlreadyExists` takes it, projects a new token and revokes the `jti` of the status write | `TestTheLoopRealizesWhatARestartLeft` | not built |
| A delete before the pass leaves no driver object; a delete during the driver call answers `Deleting` at once and the pass removes the object it made and revokes its token | `TestDeleteDuringACreate` | not built |
| Stop and start are `phase_conflict` before the pass and during the driver call | `TestStopDuringACreate` | not built |
| An apply during the driver call keeps its specification, and the settle writes the phase onto it | `TestAnApplyDuringACreateIsKept` | not built |
| A pool adoption still answers after the adoption, `Scheduled True FromPool`, under the entry's id | `TestAdoptionAnswersAfterTheAdoption` | not built |
| `POST ?wait=1` answers `201` with `Running`; `timeout` bounds it and the answer is the object as it stands; a malformed or out of range `timeout` is `invalid_field` | `TestCreateWait` | not built |
| `cellad serve` answers a create `Pending` and a `?wait=1` create `Running`, and runs the sandbox either way | `TestCreateAnswersAtOnceEndToEnd` | not built |
| `client.Wait` sends `wait=1` and `timeout`, and calls without options send neither | `TestCreateSandboxWait` | not built |
| `cella apply --wait` and `-w` send the hold and report the sandbox once it runs | `TestApplyWaits` | not built |
| The conformance suite holds a `?wait=1` create to `Running`, against this server and the kind tier | `TestTheConformanceSuiteHoldsAgainstThisServer`, case `case008CreateWait` | not built |
| The API document names `wait` and `timeout` on both create routes and the new code, and the document and the mux agree | `TestTheDocumentAndTheMuxAgree`, `TestErrorTable` | not built |
