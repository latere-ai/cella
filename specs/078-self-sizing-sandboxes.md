---
title: "Self-sizing sandboxes: a ceiling in place of a fixed size, a workspace that grows before it fills, memory raised in place or at a move, CPU bounded by the ceiling, and the status and events that record each change"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/.archive/035-podman-driver.md
  - specs/.archive/036-k8s-driver.md
  - specs/.archive/047-admission-client.md
affects: [manifest/v1/, manifest/, autosize/, runtime/, runtime/k8s/, runtime/podman/, controller/, internal/events/, internal/store/, internal/config/, internal/api/, cmd/cellad/, api/openapi.yaml, client/, test/conformance/, deploy/, docs/, CHANGELOG.md]
effort: large
created: 2026-09-30
updated: 2026-10-11
author: changkun
---

# Self-sizing sandboxes

## Overview

A sandbox takes one size at create: `resources.cpu`, `.memory` and
`.disk`, defaulting to 1, 1Gi and 5Gi. A client that cannot know what
its workload will need picks wrong both ways. Asking too much reserves
capacity the workload never uses, which packs nodes loosely and costs
whoever pays for it. Asking too little fails the workload at the worst
time: a full workspace or an out-of-memory kill in the third hour of an
agent's task, with the work that led there lost from memory.

This spec lets a manifest name a ceiling instead. With `resources.max`
set, the sandbox starts at `resources` and is sized inside the ceiling
by what the workload uses: the workspace claim grows before it fills,
memory grows when the working set nears its limit or the kernel kills a
process for memory, and CPU runs up to the ceiling whenever the node has
it idle. The workspace is a claim that outlives the workload's process
and its Pod, so the one growth that needs a restart, moving to a node
with room, keeps every file.

## Current state

`resources` is fixed after create unless the driver declares `Resize`,
and no driver does: the Kubernetes driver declares `Files`, `Pool`,
`Mesh`, `Attach`, `Dial`, and the display and egress capabilities. It
sets each limit from `resources` and each request as a fraction of the
limit (`CPURequestRatio`, `MemoryRequestRatio`), and claims the
workspace at `resources.disk` from its configured storage class.
Nothing reads how full the workspace is, and nothing reads how much
memory the workload uses.

What running out of memory does today depends on what the kernel kills.
The Pod's restart policy is `Never`, so a kill that ends the workload
container ends the sandbox: it is `Failed` with reason `OOMKilled`, and
a start of a failed sandbox is refused. On a node whose kubelet kills
the whole container cgroup together, which is its default on cgroup v2,
any process over the limit ends the sandbox this way. On a node that
kills one process (cgroup v1, or a kubelet set to `singleProcessOOMKill`),
a child over the limit dies alone: the container keeps running, the
client's command exits 137, and a retry meets the same limit.

Kubernetes resizes a running container's CPU and memory in place
through the Pod's `resize` subresource, on by default since 1.33, and
expands a bound claim online when its storage class sets
`allowVolumeExpansion`. Podman updates a running container's memory
limit in place.

Built: CPU and memory on the Kubernetes driver, end to end. The manifest
fields with their refusals, `capabilities.autosize`, the driver's half
(`Resources.CPULimit`, `Change.Resources`, `State.Resources` and
`State.Resize`, `MemoryReader`), the `autosize` package with every figure
of the table below, the controller's reading loop under the `autosize`
lease with the three memory signals, the raise in place and the move,
`status.resources`, `.usage` and `.moves`, the `MemoryAtCeiling`
condition, the two events, `CELLA_AUTOSIZE_INTERVAL`, the two optional
rules of the base Role, and the conformance case. The cluster tests ran
against a single-node kind cluster on Kubernetes 1.37 with a
metrics-server.

Open: the workspace's growth, so `max.disk` is refused everywhere
because no driver declares `autosize.disk`; the Podman driver, which
declares none of the three flags; and `Resize`, with the questions under
Not in this spec.

## Design

### The fields

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `resources.cpu`, `.memory`, `.disk` | quantity | `Defaults` | yes, with `Resize` | unchanged: the size the sandbox starts at, and with `max` its floor |
| `resources.max.cpu`, `.memory`, `.disk` (added) | quantity | absent: fixed at `resources` | yes, with `Autosize` | each at least its `resources` value (`invalid_field`), at most the environment's `Ceilings` (`ceiling_exceeded`); a resource with no `max` stays fixed |

```yaml
resources:
  cpu: 500m
  memory: 1Gi
  disk: 5Gi
  max:
    cpu: "2"
    memory: 6Gi
    disk: 50Gi
```

Every refusal names the one field, `spec.resources.max.cpu`,
`.memory` or `.disk`. A manifest with no `max` resolves, renders and
runs exactly as it did before this spec, on every driver.

An admission step ([[047-admission-client]]) may set or narrow `max`
the way it narrows any field: a hosted platform gives each workload the
ceiling the person's plan allows, and the client names no size at all.
What the step returns passes the rules above after it, so a ceiling it
adds is held to `resources`, to `Ceilings` and to the capability. The
step reads the capability from the environment its request carries.

On an update, raising `max` takes effect at the next reading. Lowering
`max.memory` or `max.disk` below the size the sandbox has now, or
removing it while the sandbox is above `resources`, refuses the update
as `ceiling_exceeded`, since a claim cannot shrink and a running
process's memory cannot be taken back. `max.cpu` moves freely down to
`resources.cpu`: CPU is compressible, and the limit follows the field.

A spawned child's ceiling is held to its parent's by the boundary check
of [[003-manifest-contract]]: each `max` is at most the parent's `max`
for that resource, or the parent's `resources` value where the parent
has none. An environment's pool is a fixed shape: `spec.pool.resources`
takes no `max` (`invalid_field`), and a manifest with `max` matches no
pool entry and is created on the slow path.

`status.resources` (added) is the size the sandbox has now, `{cpu,
memory, disk}`: the CPU limit, the memory limit and the workspace
claim, in quantity syntax. `status.usage` (added) is the last reading,
`{memory, readAt}`, the working set in bytes and when it was sampled;
`disk` joins it with the workspace's growth. `status.moves` (added)
counts the moves below. All three are absent without `resources.max`.
`spec.resources` stays what the client asked for; only status changes.
The reading is held by the control plane and written with the
sandbox's next status write, so a reading costs no write of its own.

Against an environment's declared capacity
([[020-scheduling-and-sets]]) a sandbox counts at the CPU it is
guaranteed, `resources.cpu`, and at the memory it holds now. Growth is
bounded by `max` and is not refused for capacity.

### The capability

`autosize` is a member of the driver's capabilities, an object of three
flags, one per resource:

```json
{"autosize": {"cpu": true, "memory": true, "disk": false}}
```

It is `Capabilities.Autosize` in `manifest/v1` and so
`status.capabilities.autosize` of an environment. A manifest whose
`max` names a resource whose flag is false is `capability_unsupported`
at that field's path.

| Driver | `cpu` | `memory` | `disk` |
|---|---|---|---|
| Kubernetes | always | when the API server serves the `pods/resize` subresource and the metrics API, and the driver's service account may patch the first and read the second | when the workspace's storage class sets `allowVolumeExpansion` |
| Podman | always | always, through a container update | never: a local volume takes no size |
| others | never | never | never |

The Kubernetes driver checks each condition at `Preflight`, through
discovery and an access review, and declares only what holds. The two
accesses are not required: a service account without them starts as
before and declares no memory autosize. The `remote` driver declares
none of the three, because no frame of the worker stream carries a
reading or a size.

### The driver's half

The policy below is the controller's, decided once for every driver. A
driver supplies the mechanisms ([[004-runtime-contract]]):

| Addition | Meaning |
|---|---|
| `Resources.CPULimit` | the most CPU the sandbox may use; empty is `Resources.CPU` |
| `Change.Resources` | the sizes the sandbox is to hold: applied in place where the substrate changes a running sandbox, recorded for the next start otherwise; a disk change is `ErrUnsupported`; a driver that changes no size refuses the field |
| `State.Resources` | the sizes the sandbox holds now, as the substrate reports them |
| `State.Resize` | a size asked for and not held: the sizes, a reason (`InProgress`, `Deferred`, `Infeasible`, or `Restart` where only a restart applies it) and the substrate's message |
| `MemoryReader` | `MemoryUsage(ctx, id)`, the workload's working set and its sample time; `MemoryKills(ctx, id)`, how many processes of the workload the kernel has killed for memory, with the identity of the workload instance the count belongs to |

The Kubernetes driver keeps the sizes in the record on the workspace
claim, the one a start renders the Pod from, so a start takes the last
size with no store behind it.

### CPU

CPU is compressible: a workload short of it slows and never fails. So
CPU takes no steps. A sandbox with `max.cpu` runs with its limit at
`max.cpu` and its request at `resources.cpu` times the request ratio.
The workload is guaranteed its request and bursts to the ceiling when
the node has CPU idle. `status.resources.cpu` is the limit. An update
of `max.cpu` reaches a running sandbox in place where the cluster
serves `pods/resize`, and at its next start otherwise.

### Disk

The controller reads the workspace's used and total bytes every
`CELLA_AUTOSIZE_INTERVAL` while the sandbox runs, by `statfs` on the
workspace path through the driver's exec path, the one a client's
command takes. It never reads the workspace's contents.

| Reading | Action |
|---|---|
| used at or above `autosize.DiskGrowAt` of the claim, and the claim below `max.disk` | expand the claim to the lesser of `max.disk` and the claim plus the greater of half the claim and `autosize.DiskStepMin`; the filesystem grows online, and `status.resources.disk` is the claim's new capacity once the storage reports it |
| used at or above `autosize.CeilingWarnAt` of `max.disk`, with the claim at `max.disk` | emit `sandbox.at_ceiling` once for this crossing |

A claim never shrinks. An expansion the storage refuses leaves the
claim at its size, sets the condition `WorkspaceResized` `False` with
the storage's reason, and is retried at the next reading. The
additional volumes of `volumes[]` keep their own `Volume.spec.size`
([[019-volumes]]).

### Memory

The controller reads every running sandbox that has `max.memory` once
per `CELLA_AUTOSIZE_INTERVAL`, under the `autosize` lease so one
replica reads. Three signals say the workload needs more:

| Signal | Meaning | Source |
|---|---|---|
| the working set at or above `autosize.MemoryGrowAt` of the limit at `autosize.MemoryGrowReadings` readings in a row | the workload is about to run out | the metrics API's sample for the workload container; a sample read twice is one reading |
| the sandbox ended `Failed` with reason `OOMKilled` | it ran out, and the kill took the container | the driver's own read of the sandbox |
| the count of processes the kernel killed for memory rose since the last reading | a child ran out and the container lives on | the `oom_kill` and `oom_group_kill` counters of the workload's cgroup |

The third signal is the common one for a shell that runs a package
install or a browser where the node kills one process at a time: the
child dies, the shell that started it survives, the container never
terminates, and a burst that ends in a kill within seconds shows as no
two readings near the limit. The count is read inside the sandbox
through the driver's exec path, as the sandbox's own user, from
`memory.events` of cgroup v2 or `memory.oom_control` of cgroup v1,
which are the kernel's files and none of the workload's. On Kubernetes
no other source carries it within the driver's namespace: the metrics
API serves usage alone, the Pod's status says nothing of a kill the
container survived, and the kubelet's own counter is served from a
node endpoint that needs access across the cluster. The count belongs
to one workload instance. The first reading of an instance, after a
start, a move or a change of the replica that reads, is the baseline
and raises nothing by itself, and one reading that finds the count
higher raises memory by one step however many kills it counted.

The interval is too slow for a client that retries a killed command
at once, and reading the count more often would cost one command in
every sandbox every few seconds, each a stream through the API server
and a process in the container. The end of the client's own command is
the moment that matters and costs nothing when no command fails. So a
command that ends with exit code 137, the code of a process a signal
ended, or whose stream breaks, has the sandbox's state and its kill
count read before its exit is reported, and a raise that reading
decides is asked of the driver before the client reads the exit. A
command that ends with any other non-zero code asks the loop for the
same reading and its exit does not wait: a parent that reports its
killed child in its own way is caught a moment later. A command that
exits zero costs nothing, and neither does one in a sandbox with no
memory ceiling. A sandbox that has just started is read the same way,
so its baseline is taken before its first interval ends. A kill no
command reports, a child of a command that goes on, or one in a
terminal session, is found at the next interval.

Measured on a single-node cluster over two runs each, from the start of
a command that allocates past the limit: where the node kills one
process, the command ended after 0.3 to 0.4 seconds with the raise
already asked for, and the new limit was applied and recorded at 1.3 to
1.4 seconds; where the node kills the whole container, the command
ended after 0.3 to 0.5 seconds and the sandbox was running again at the
larger size at 3.3 to 4.5 seconds.

On any signal, with the limit below `max.memory`, the next size is the
greater of the limit times `autosize.MemoryStepFactor` and the limit
plus `autosize.MemoryStepMin`, rounded up to `autosize.MemoryQuantum`,
and then held to `max.memory`. From 256Mi under a ceiling of 1Gi that
is 768Mi and then 1Gi. The request follows as the limit times the
request ratio.

1. **In place.** The driver records the size and patches the Pod's
   `resize` subresource. Memory's resize policy is `NotRequired`, the
   default, so the process keeps running. Where the node has the room,
   the kubelet applies it and the sandbox is resized with nothing lost.
   The controller waits a few seconds for the kubelet's verdict, so a
   raise that lands is recorded at once; one still being applied is
   read again at the next interval.
2. **By a move.** Where the kubelet reports the resize deferred or
   infeasible, or the cluster refuses the patch, the node lacks the
   room. The controller moves the sandbox: it stops it and starts it
   again at the new size, so the driver deletes the Pod and creates it
   from the recorded size, the scheduler places it on a node with room,
   and a cluster autoscaler adds a node when none has it. The workspace
   claim moves with it where its storage attaches on another node, so
   the files are all there; the processes start again. A move waits
   for a moment with no exec, attach, dial or archive transfer open, so
   no client's command is cut. After an `OOMKilled` the processes have
   already died and the move goes at once, from `Failed` to `Starting`
   ([[005-lifecycle-controller]]). A move is a restart, recorded as
   `moved: true` on its event, and `status.moves` counts it.

A move runs beside the loop, one sandbox at a time each, with the
controller's lock released as a create's driver call is: a start, a
stop and the deadline rules leave a moving sandbox alone, and a delete
is taken at any time, with what the move made removed when it ends. A
move whose Pod is not ready within the driver's ready timeout is
undone: the sandbox is put back at its previous size and started
there, and its memory is not raised again for `autosize.MoveBackoff`.

Memory never shrinks while the sandbox runs. A start after a stop
takes the last size, so a workload that needed 4Gi yesterday starts at
4Gi today rather than being killed again on its way there; recovery of
a lost sandbox takes `status.resources` the same way. An update of
`spec.resources.memory` sets the size explicitly and resets that
memory.

With the limit at `max.memory`, a working set at or above
`autosize.CeilingWarnAt` of it, or a kill for memory, sets the
condition `MemoryAtCeiling` `True` and emits `sandbox.at_ceiling` once
for the crossing; the condition returns to `False` when a reading
falls below the line, and the next crossing emits again. The condition
is stored, so a restart of the control plane does not repeat the
event. A sandbox killed for memory at its ceiling is `Failed` with
`OOMKilled`, as it is without `max`, and nothing moves or resizes it.

### Events

| Type | When | Data |
|---|---|---|
| `sandbox.resized` (added) | a size change applied | `{resource, from, to, reason, moved}`: `resource` is `disk` or `memory`; `from` and `to` are quantities as `status.resources` writes them; `reason` is `usage`, `oom` or `update`; `moved` is true when the change took a move |
| `sandbox.at_ceiling` (added) | usage passed `autosize.CeilingWarnAt` of `max`, or a process was killed for memory, with no room left to grow | `{resource, used, max}`: `used` in bytes as `status.usage` reports it, `max` the ceiling as the manifest wrote it |

`reason` is `oom` after either kill signal, so a client whose command
exited 137 reads on the sandbox's feed that it was killed for memory
and that the sandbox has been given more. A change of `max.cpu` is an
update and is recorded as `sandbox.updated`.

A meter that charges for a sandbox's size reads `sandbox.resized` and
`status.resources`, the size held and for how long. What a size costs
is the operator's to set.

### Limits and constants

| Name | Value | Meaning |
|---|---|---|
| `CELLA_AUTOSIZE_INTERVAL` | `1m` | how often a running sandbox's usage is read; between `1s` and `1h` |
| `autosize.DiskGrowAt` | 0.80 | the fraction of the claim used that expands it |
| `autosize.DiskStepMin` | 5Gi | the smallest expansion |
| `autosize.MemoryGrowAt` | 0.85 | the fraction of the limit that raises memory |
| `autosize.MemoryGrowReadings` | 2 | how many readings in a row at that fraction raise it |
| `autosize.MemoryStepFactor` | 1.5 | the growth factor of a memory step |
| `autosize.MemoryStepMin` | 512Mi | the smallest memory step |
| `autosize.MemoryQuantum` | 256Mi | a memory size is a multiple of this, or the ceiling |
| `autosize.CeilingWarnAt` | 0.95 | the fraction of `max` that emits `sandbox.at_ceiling` |
| `autosize.MoveBackoff` | `10m` | how long memory is left alone after a move that found no room |

Every check, schema and message that names one of these reads the
constant.

### Security

A workload can drive its own sandbox to the ceiling by filling the disk
or allocating memory: that is what the ceiling is for, and the
ceiling, not the workload, decides the most it can hold. The usage
readings run through the same exec path, as the same user, as a
client's command, and read the kernel's own files and no file of the
workload's. They stamp no activity, so a reading never holds an idle
sandbox open. A move keeps the sandbox's identity, secrets, egress
boundary and mesh membership, since it is the same object with a new
Pod.

## Not in this spec

What a size costs and who pays: a hosted platform's metering reads the
events and status above. Choosing a ceiling from a person's plan or
wallet: the platform's admission step. Growing the additional volumes
of `volumes[]`. Shrinking a workspace, which Kubernetes claims do not
support. CPU steps, which a compressible resource does not need.

Declaring `Resize`. The mechanism an explicit update of
`spec.resources` needs is this spec's `Change.Resources`, and the
controller applies such an update through it where a driver declares
the capability. No driver declares it yet, because three questions are
open: whether an update that raises a size is held to the environment's
declared capacity, and with which refusal; what becomes of an explicit
update the node defers, a wait or a move; and how a lowered memory
reaches a running sandbox, which the cluster takes in place from 1.34
only.

Marking the killed command itself. The exit record of an exec is its
exit code in three wire forms ([[008-api]]), and none carries a cause.
The cause is on the feed, as `sandbox.resized` with `reason: oom`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `max` below `resources`, above a ceiling, or on a resource the driver does not autosize is refused with the field's path; lowering `max` below the current size is refused | `TestResolveAutosizeFields`, `TestCeilingOnUpdate` | built, with the floor read after defaulting as `TestACeilingIsHeldToTheDefaultedAmount`, a child's ceiling inside its parent's as `TestAChildsCeilingIsInsideItsParents`, the pool's refusal as `TestAPoolTakesNoCeiling`, the native environment's as `TestNativeRefusesACeiling`, and over HTTP against any server as conformance case `case078Ceiling` |
| A manifest with no `max` resolves and renders as before, and its status carries none of the three members | `TestNoCeilingResolvesAsBefore`, `TestCPUAtTheCeiling`, `TestASandboxWithACeilingRunsAtItAndReportsItsSize` | built: the golden corpus of the manifests without a ceiling is unchanged |
| A `max` an admission step adds is validated and kept | `TestAdmissionSetsTheCeiling`, `TestAdmissionMayAddACeiling` | built, through the admission client and the whole of `Resolve`, with an endpoint that ignores the capability refused as `TestAnEndpointThatIgnoresTheCapabilityIsRefused` |
| A sandbox with `max.cpu` runs with its CPU limit at the ceiling and its request at `resources.cpu` times the ratio | `k8s.TestCPUAtTheCeiling` | built, and on a cluster as the first assertion of `TestClusterMemoryGrowsInPlace`; an update of `max.cpu` reaches the driver as `TestAnUpdateOfTheCPUCeilingReachesTheDriver` |
| A workspace filled past `DiskGrowAt` is expanded by the step, never past `max.disk`, the files intact, and `status.resources.disk` reports the new capacity | `k8s.TestWorkspaceGrowsBeforeItFills` on kind with an expandable CSI class | not built |
| An expansion the storage refuses sets `WorkspaceResized` false and is retried | `TestRefusedExpansionRetries` | not built |
| A working set past `MemoryGrowAt` at two readings raises memory in place, the process still running | `k8s.TestMemoryGrowsInPlace`, `TestClusterMemoryGrowsInPlace` on kind, `TestMemoryIsRaisedAfterReadingsInARow` | built: over the client double, and against a real kubelet on a kind cluster, where a process holding 112Mi of 128Mi was raised to 768Mi in place with the same Pod, no container restart, the process still running and the container's cgroup at the new limit |
| A resize the node cannot hold moves the sandbox once no session is open, keeps the workspace, and records `moved: true` | `k8s.TestDeferredResizeMoves`, `TestADeferredRaiseMovesOnceNoSessionIsOpen`, `TestEverySessionKindHoldsAMove` | built over the client double, with the kubelet's deferred and infeasible verdicts written by the test. Not run against a real kubelet: on one node a resize the node defers is a Pod the scheduler cannot place either, and a move across nodes needs storage that attaches on a second node, which the kind cluster's node-bound storage class is not |
| After an `OOMKilled`, the sandbox moves at once to the larger size | `k8s.TestOOMKillGrowsMemory`, `TestClusterOOMKillGrowsMemory` on kind, `TestAKillThatTookTheContainerMovesAtOnce` | built: over the client double, and on a kind cluster, where a main process that needs 200Mi under 128Mi was killed, the Pod ended `OOMKilled`, and the sandbox ran again at 768Mi with the file its first start wrote. At its ceiling it stays `Failed` and nothing restarts it |
| A child killed for memory in a container that lives on raises memory by one step, in place, and the first count of an instance raises nothing | `TestAChildKilledForMemoryGrowsTheSandbox`, `TestClusterAChildKilledForMemoryGrowsTheSandbox` on kind, `TestAChildKilledForMemoryRaisesOneStep`, `TestAFailedCommandIsReadBeforeItsExitIsReported` | built: over the client double with the exec stream answering the kernel's file, and on a kind cluster in both of the node's modes. With the kubelet set to kill one process, the command ended 137, the shell lived, the limit was raised in place and the same command then succeeded; with the kubelet's default, the kill took the container and the sandbox was restarted at the larger size |
| A start after a stop takes `status.resources`; an update of `spec.resources.memory` resets it | `TestStartTakesTheLastSize`, `TestAStartAndARecoveryTakeTheSizeHeld`, `TestRecoveryAndCapacityReadTheSizeHeld`, `TestAnUpdateOfTheAmountResetsTheSize` | built at the driver, whose record on the claim a start renders, and at the controller, whose create and recovery ask for the size held. The reset by an update is built at both and reached through the API by no driver yet, because none declares `Resize` |
| Usage past `CeilingWarnAt` at the ceiling emits one `sandbox.at_ceiling` per crossing | `TestAtCeilingOncePerCrossing` | built, with the condition stored so a restarted control plane does not repeat the event, and a raised ceiling clearing it as `TestRaisingTheCeilingMakesRoomAgain` |
| A move that finds no room is undone and the sandbox left alone for `MoveBackoff`; a raise the cluster refuses is given again and then moved | `TestAMoveThatFindsNoRoomIsUndone`, `TestARaiseTheClusterRefusesIsGivenAgainAndThenMoved`, `TestADeleteDuringAMoveRemovesWhatTheMoveMade` | built |
| A change of size and its record commit together in every store, and the two records carry the data the table states | `TestBridgeWritesASizeWithItsRecord`, the `Sizes` case of `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore`, `TestAResizeCommitsWithItsRecord`, `TestRecordShapes` | built |
| Every figure of the constants table is the constant, in the spec and on the pages | `TestTheSpecsTableIsTheConstants`, `TestThePagesStateTheConstants`, `TestNextMemory`, `TestASmallStartReachesItsCeilingInTwoSteps` | built |
| Podman raises a running container's memory in place and declares no disk autosize | `podman.TestMemoryUpdateInPlace` | not built: the Podman driver declares none of the three flags and refuses a change of size, held by the driver suite's `SizeChangeIsHonoredOrRefused` |
| The Kubernetes driver declares each autosize flag only when its condition holds | `k8s.TestAutosizeCapabilities` | built for `cpu` and `memory`, with the two accesses optional at `Preflight` and in the base Role, `TestRoleMatchesTheDriversVerbs`; `disk` is never declared |
