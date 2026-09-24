---
title: "A create answers at once: 201 with the sandbox Pending, the create order finished by the scheduler loop, ?wait=1 for the held answer, and no gateway refused at admission"
status: complete
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
updated: 2026-09-25
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

A read of a placed sandbox answers the row and does not ask the driver.
A driver can run the workload before its create returns: the Kubernetes
driver's create waits for the Pod to be ready, the desktop included, and
the Pod reads `Running` first. A read that answered the driver's early
`Running` would invite a stop, a dial or a screenshot that the routes,
which read the row, refuse or cannot serve yet. The sandbox reads
`Running` once the create's own read is written.

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
| A read of the object, a list | `Pending`, without asking the driver | `Pending`, without asking the driver, even where the driver already runs the workload |
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
driver reported after the create. The handler waits on
`Controller.Changed`, a channel closed at the next write of any sandbox,
and reads the object at each; once the sandbox is handed to its driver,
`Scheduled True`, it also reads the driver each 250 ms, since a driver's
own move from `Pending` to `Running` is written by nobody until a read.
The hold is bounded like `exec?wait=1`'s: `timeout` in the query, a
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
`client.Wait` with `--timeout`, and then reads the answer, and the object
after it, until it is `Running` or `--timeout` has passed since the
create, which is what a server that ignores `wait` needs. `--timeout`
stays at `2m`.

### The records of a create

The loop writes a direct create's status and its start, and design 009
names the loop's own acts `controller` with no request id. A direct
create's start was the caller's act while the request ran it, with the
caller as `subject`, the request's id, and the reason `Request` on
`sandbox.started`, which a sink's usage fold opens its interval on. The
create therefore keeps its request's context, without its cancellation,
beside the row until the loop takes it, and the loop's writes for that
sandbox carry its values while the loop's own context still cancels the
driver's call. A sandbox a restart left placed has no such context, and
its start is the control plane's act; a dequeued sandbox's start is the
scheduler's, as before.

## Not in this slice

Realizing two placed sandboxes at once: the loop realizes one at a time,
so a second create's driver call begins after the first's ends, as
creates did while they held the lock; it answers at once either way. The
lock is still held across the boundary push, which waits for one
gateway's acknowledgment, as every push does; holding it keeps two
pushes of one sandbox from crossing. A `Ready` condition written by the
controller, which would carry a failed create's cause beyond its reason.
A watch that writes the driver's later phases without a read.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A create placed now answers `Pending` with no `Scheduled` condition before its driver's create begins, the loop the create woke takes it to `Running`, and the journal holds `sandbox.created`, `sandbox.status`, `sandbox.started` | `TestCreateAnswersBeforeTheDriver` | built |
| The loop runs the driver's create without the controller's lock: a second pass, a read, a list and a second create answer while the first driver call is held open, the second pass does not ask the driver again, and both sandboxes run | `TestTheDriverCallHoldsNoLock` | built |
| A driver create that fails leaves the sandbox `Failed CreateFailed`, the principal purged, the token revoked and a spawn parent's unit credited; the parent reads the debit from the spawn's answer on | `TestAFailedRealizeUndoes` | built |
| A manifest whose boundary needs a gateway, with none connected, is `ErrNoGateway` at the create, with no row and no name taken; a gateway connected at admission that acknowledges nothing at the loop's push fails the sandbox, with `EgressEnforced False NoGateway` where no gateway acknowledged | `TestCreateWaitsForTheGateway` | built |
| The API answers that refusal `503 egress_gateway_unavailable` with its remedy, where it answered `driver_unavailable`; the guide, the suite's table and the command's exits carry the code | `TestNoGatewayHasItsOwnCode`, `TestTheGuideCarriesTheErrorTable`, `TestEveryErrorCodeBecomesItsExit` | built |
| A fresh controller over a store holding a placed, unrealized sandbox realizes it on the loop's first pass: with no driver object it creates one; with one, `ErrAlreadyExists` takes it, projects a new token and revokes the `jti` the first process minted | `TestTheLoopRealizesWhatARestartLeft` | built |
| A loop that stops while the driver creates writes nothing after the call, the row stays placed with its boundary record and identity, and the next pass creates the sandbox and revokes the stopped pass's identity | `TestAStoppedLoopLeavesTheCreateToTheNext` | built |
| A status write the store refuses ends the pass before the driver is asked, with the identity revoked, and the next pass creates the sandbox | `TestAFailedStatusWriteLeavesTheCreatePlaced` | built |
| A delete before the pass leaves no driver object; a delete during the driver call answers `Deleting` at once and the settle removes the object the create made and revokes its token; a delete whose driver call fails leaves the row `Deleting` for the retry | `TestDeleteDuringACreate`, `TestADeleteThatFailsDuringACreateKeepsTheRow` | built |
| Stop and start are `phase_conflict` before the pass and during the driver call, and the driver is not asked | `TestStopDuringACreate` | built |
| An apply during the driver call keeps its specification, and the settle writes the phase onto it | `TestAnApplyDuringACreateIsKept` | built |
| A read during the driver call answers `Pending` where the driver already runs the workload, and `Running` once the create's own read is written | `TestAReadDuringACreateSaysPending` | built |
| A driver that does not report a sandbox it created yet leaves it `Pending` and handed on, not created twice | `TestADriverThatDoesNotReportYet` | built |
| The start of a direct create is written under the values of the request that made it, so its records name the caller and the request | `TestTheStartCarriesTheCreatesRequest`, `TestEventsEndToEnd` | built |
| A pool adoption still answers after the adoption, `Running` with `Scheduled True FromPool`, under the entry's id; an adoption that fails undoes on the request | `TestPoolAdoption`, `TestAnAdoptionThatFailsUndoes` | built |
| `POST` without a hold answers `201 Pending` with its `Location` while the driver still has the create; `?wait=1` answers the object as it stands at the bound, `not_found` for a sandbox deleted while held, and `Running` once the driver answers; an apply takes the hold; a `timeout` that is not a positive duration of at most `1h` is `invalid_field` with nothing recorded | `TestCreateWait` | built |
| `cellad serve` answers a create `Pending` and runs it with nothing else asked, answers a `?wait=1` create `Running`, and both run commands | `TestCreateAnswersAtOnceEndToEnd` | built |
| `client.Wait` sends `wait=1` and `timeout` on the create and the apply, a zero bound leaves the server's, and a call without options sends neither; the exported client's held apply runs against a node | `TestCreateSandboxWait`, `TestTheExportedClientDrivesARunningNode` | built |
| `cella apply --wait` and `-w` send the hold with the command's timeout, and a create without either sends none | `TestApplyWaits`, `TestApplyWaitsForTheSandboxToRun` | built |
| The conformance suite holds a `?wait=1` create to `Running`, against this server and the kind tier: conformance case `case008CreateWait` | `TestTheConformanceSuiteHoldsAgainstThisServer` | built |
| The browser case reads the display until `DisplayReady` before its screenshot, so a desktop that comes up a moment after the workload passes it | `TestTheBrowserCaseWaitsForTheDesktop` | built |
| The API document names `wait` and `timeout` on both create routes, and the document and the mux agree | `TestTheDocumentAndTheMuxAgree` | built |

## Outcome

A create answers at once and the scheduler loop finishes it; the hold, the
client's option, the command's flag and the gateway's code are built, each
with its test.

| Piece | Where |
|---|---|
| The deferred placement, the gateway gate at admission, the adoption on the request | `create`, `createLocked` and `adopt` in `controller/controller.go`; `needsGatewayFor` in `controller/egress.go` |
| The loop's realize in three parts, the settle against the row as it stands, the undo | `realize`, `drive`, `settleCreate`, `failCreate` in `controller/controller.go` |
| The placed predicate, and the pass that realizes before it reads a queue | `placed`, `placedOn`, `environmentsWaiting`, `scheduleEnvironment`, `placeQueued` in `controller/scheduler.go` |
| A read of a placed sandbox that answers the row | `refresh` in `controller/controller.go` |
| Stop and start refused during a create; the token rule skips it | `Act` in `controller/controller.go`, `enforceToken` in `controller/reaper.go` |
| The create's request carried to the start's records | `origins` and `carried` in `controller/controller.go` |
| The spawn's debit projected on the parent at once | `debit` in `controller/spawn.go` |
| `Controller.Changed` | `controller/controller.go` |
| `?wait=1`, `timeout`, `egress_gateway_unavailable` | `createWait`, `awaitStart`, `started`, `handedOn` and `errorEnvelope` in `internal/api/api.go` |
| `client.Wait` and `CreateOption` | `client/sandboxes.go` |
| `cella apply --wait` | `apply` and `waitForRunning` in `internal/cellacli/objects.go`, `internal/cellacli/usage.go` |
| `case008CreateWait`, and the browser case waiting for `DisplayReady` | `test/conformance/cases_lifecycle.go`, `test/conformance/cases_kinds.go` |
| The plane example running the scheduler loop | `examples/plane/main.go` |
| The API document, the guides, the changelog | `api/openapi.yaml`, `docs/api.md`, `docs/cli.md`, `docs/client.md`, `docs/plane.md`, `docs/scheduling.md`, `docs/install.md`, `docs/native.md`, `skills/cella/SKILL.md`, `CHANGELOG.md` |

Coverage on `go test -cover`: `controller` 91.3%, `internal/api` 93.0%,
`client` 95.3%, `internal/cellacli` 90.5%; `go tool lateregate` holds every
package above 90%. The end-to-end runs were `TestCreateAnswersAtOnceEndToEnd`
against `cellad serve`, the conformance suite against this server with
`case008CreateWait`, and, on a kind cluster of the stack in
`deploy/examples/kind-stubs`, the five cluster tests of the kind tier and the
conformance suite with every capability the workflow declares.
On the kind stack the kind tier's five cluster tests passed and the suite
answered 41 passed, 0 failed and 13 skipped, the skips being the groups
whose input the stack does not give.

The kind run found two defects the unit tiers could not. A read of a
sandbox whose create was still in the driver's hands asked the driver, and
the Kubernetes driver reports the Pod `Running` before its create's wait for
readiness returns: the suite's await saw `Running`, and its stop, which
reads the row, was `phase_conflict`, and its dial was refused as `Pending`.
A read of a placed sandbox now answers the row, as
`TestAReadDuringACreateSaysPending` holds. The browser case asked for a
screenshot at `Running`, while the desktop on Kubernetes comes up beside the
workload and may follow it; it passed against the old create by timing. It
now reads the display until `DisplayReady`, which is what its own statement
says, as `TestTheBrowserCaseWaitsForTheDesktop` holds.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| This slice's brief | the boundary push, the mint, the driver's create or the pool adoption, and the first read run after the answer | a pool adoption runs on the request and answers after it | the adoption takes the entry's id, and a lost adoption falls back under a new id, which is possible only before the caller has read one |
| [[005-lifecycle-controller]] | a create that crashes before the observed write is repaired by the next reconcile | the scheduler loop's pass over placed sandboxes repairs it, from a status write made before the driver call | no reconcile loop exists; the loop that realizes a dequeued sandbox owns every placed one |
| [[005-lifecycle-controller]] | `Pending --> Starting: driver has the object`, seen by a watch | a placed sandbox reads `Pending` until its create's own read is written, whatever the driver reports | a read that answered the driver's early `Running` invited acts the routes, which read the row, refuse |
| [[009-events]] | `subject` is `controller` for the reaper and the scheduler | a direct create's start, written by the loop, names the create's caller and request, with reason `Request` | the start is the caller's act, as it was while the request ran it, and a sink's usage fold opens on it |
| [[008-api]] | the create's answer is the manifest as it runs, and a runtime failure is the answer | `201` with the sandbox `Pending`, or held; a failure is the object's `Failed` and its reason | the answer comes before the runtime is asked |
| [[022-mesh-and-spawn]] | `status.spawn.used` tracks the ledger once the child is created | it is projected at the debit, when the child's create answers | the child's start is written by the loop later |

### What this leaves open

| Open | Why |
|---|---|
| Realizing two placed sandboxes at once | the loop takes one at a time; a second create answers at once and its driver call begins after the first's ends |
| The boundary push outside the lock | two pushes of one sandbox must not cross, and a push waits for one gateway's acknowledgment |
| A create another replica wrote waits for the lease holder's next pass | the loop runs under the scheduler lease, at most `CELLA_SCHEDULE_INTERVAL` |
| A failed create's cause on the object beyond its reason | no `Ready` condition is written by the controller; the cause is in the control plane's log |
| A driver's later phase written without a read | there is no watch |
