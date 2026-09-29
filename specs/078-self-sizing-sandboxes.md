---
title: "Self-sizing sandboxes: a ceiling in place of a fixed size, a workspace that grows before it fills, memory raised in place or at a move, CPU bounded by the ceiling, and the status and events that record each change"
status: drafted
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
affects: [manifest/v1/, manifest/, driver/, runtime/k8s/, runtime/podman/, controller/, internal/api/, api/openapi.yaml, client/, test/conformance/, docs/, CHANGELOG.md]
effort: large
created: 2026-09-30
updated: 2026-09-30
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
set, the sandbox starts at `resources` and the driver sizes it inside
the ceiling by what the workload uses: the workspace claim grows before
it fills, memory grows when the working set nears its limit or the
workload is killed for memory, and CPU runs up to the ceiling whenever
the node has it idle. The workspace is a claim that outlives the
workload's process and its Pod, so the one growth that needs a restart,
moving to a node with room, keeps every file.

## Current state

`resources` is fixed after create unless the driver declares `Resize`,
and no driver does: the Kubernetes driver declares `Files`, `Pool`,
`Mesh`, `Attach`, `Dial`, and the display and egress capabilities, and
names `Resize` as a later slice. It sets each limit from `resources` and
each request as a fraction of the limit (`CPURequestRatio`,
`MemoryRequestRatio`), and claims the workspace at `resources.disk`
from its configured storage class. A container killed for memory
restarts at the same limit and is killed again. Nothing reads how full
the workspace is.

Kubernetes resizes a running container's CPU and memory in place
through the Pod's `resize` subresource, on by default since 1.33, and
expands a bound claim online when its storage class sets
`allowVolumeExpansion`. Podman updates a running container's memory
limit in place. Nothing in Cella uses either.

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

An admission step ([[047-admission-client]]) may set or narrow `max`
the way it narrows any field: a hosted platform gives each workload the
ceiling the person's plan allows, and the client names no size at all.
Raising `max` on a running sandbox takes effect at the next read;
lowering it below the current size refuses the update as
`ceiling_exceeded`, since a claim cannot shrink and a running process's
memory cannot be taken back.

`status.resources` (added) is the size the sandbox has now, `{cpu,
memory, disk}`, and `status.usage` (added) the last reading, `{memory,
disk, readAt}` in bytes. `spec.resources` stays what the client asked
for; the driver writes only status.

### The capability

`Autosize` is a set of flags in the driver's capabilities, one per
resource: `disk`, `memory` and `cpu`. A manifest whose `max` names a
resource the environment's driver does not autosize is
`capability_unsupported` at that field's path.

| Driver | `cpu` | `memory` | `disk` |
|---|---|---|---|
| Kubernetes | always | when the API server serves the `pods/resize` subresource and the metrics API | when the workspace's storage class sets `allowVolumeExpansion` |
| Podman | always | always, through a container update | never: a local volume takes no size |
| others | never | never | never |

The Kubernetes driver checks each condition at start, through
discovery and the storage class, and declares only what holds.

### CPU

CPU is compressible: a workload short of it slows and never fails. So
CPU takes no steps. A sandbox with `max.cpu` runs with its limit at
`max.cpu` and its request at `resources.cpu` times the request ratio.
The workload is guaranteed its request and bursts to the ceiling when
the node has CPU idle. `status.resources.cpu` is the limit.

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

| Signal | Meaning |
|---|---|
| the working set at or above `autosize.MemoryGrowAt` of the limit at two readings in a row | the workload is about to run out |
| the container's last termination was `OOMKilled` | it ran out |

On either signal, with the limit below `max.memory`, the driver asks
for the lesser of `max.memory` and the greater of the limit times
`autosize.MemoryStepFactor` and the limit plus `autosize.MemoryStepMin`,
rounded up to `autosize.MemoryQuantum`. The request follows as the
limit times the request ratio.

1. **In place.** The driver patches the Pod's `resize` subresource
   with a `NotRequired` restart policy for memory, so the process keeps
   running. Where the node has the room, the kubelet applies it and the
   sandbox is resized with nothing lost.
2. **By a move.** Where the kubelet reports the resize deferred or
   infeasible, the node lacks the room. The driver moves the sandbox:
   it deletes the Pod and creates it again at the new size, the
   scheduler places it on a node with room, and a cluster autoscaler
   adds a node when none has it. The workspace claim moves with it, so
   the files are all there; the processes start again. A move waits
   for a moment with no exec, attach or dial session open, so no
   client's command is cut, except after an `OOMKilled`, when the
   process has already died and the move goes at once. A move is a
   restart, recorded as `moved: true` on its event, and `status.moves`
   (added) counts it.

Memory never shrinks while the sandbox runs. A start after a stop
takes `status.resources`, so a workload that needed 4Gi yesterday
starts at 4Gi today rather than being killed again on its way there.
An update of `spec.resources.memory` sets the size explicitly and
resets that memory.

With the limit at `max.memory`, a working set past
`autosize.CeilingWarnAt` of it emits `sandbox.at_ceiling` once for the
crossing, and an `OOMKilled` there restarts the container at the same
limit, as today.

### Events

| Type | When | Data |
|---|---|---|
| `sandbox.resized` (added) | a size change applied | `{resource, from, to, reason, moved}`: `resource` is `disk` or `memory`; `reason` is `usage`, `oom` or `update`; `moved` is true when the change took a move |
| `sandbox.at_ceiling` (added) | usage passed `autosize.CeilingWarnAt` of `max` with no room left to grow | `{resource, used, max}` |

A meter that charges for a sandbox's size reads `sandbox.resized` and
`status.resources`, the size held and for how long. What a size costs
is the operator's to set.

### Limits and constants

| Name | Value | Meaning |
|---|---|---|
| `CELLA_AUTOSIZE_INTERVAL` | `1m` | how often a running sandbox's usage is read |
| `autosize.DiskGrowAt` | 0.80 | the fraction of the claim used that expands it |
| `autosize.DiskStepMin` | 5Gi | the smallest expansion |
| `autosize.MemoryGrowAt` | 0.85 | the fraction of the limit that raises memory, at two readings in a row |
| `autosize.MemoryStepFactor` | 1.5 | the growth factor of a memory step |
| `autosize.MemoryStepMin` | 512Mi | the smallest memory step |
| `autosize.MemoryQuantum` | 256Mi | a memory size is a multiple of this |
| `autosize.CeilingWarnAt` | 0.95 | the fraction of `max` that emits `sandbox.at_ceiling` |

Every check, schema and message that names one of these reads the
constant.

### Security

A workload can drive its own sandbox to the ceiling by filling the disk
or allocating memory: that is what the ceiling is for, and the
ceiling, not the workload, decides the most it can hold. The usage
reading runs `statfs` through the same exec path, as the same user, as
a client's command, and reads no file. A move keeps the sandbox's
identity, secrets, egress boundary and mesh membership, since it is the
same object with a new Pod.

## Not in this spec

What a size costs and who pays: a hosted platform's metering reads the
events and status above. Choosing a ceiling from a person's plan or
wallet: the platform's admission step. Growing the additional volumes
of `volumes[]`. Shrinking a workspace, which Kubernetes claims do not
support. CPU steps, which a compressible resource does not need.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `max` below `resources`, above a ceiling, or on a resource the driver does not autosize is refused with the field's path; lowering `max` below the current size is refused | `TestResolveAutosizeFields` | not built |
| A sandbox with `max.cpu` runs with its CPU limit at the ceiling and its request at `resources.cpu` times the ratio | `k8s.TestCPUAtTheCeiling` | not built |
| A workspace filled past `DiskGrowAt` is expanded by the step, never past `max.disk`, the files intact, and `status.resources.disk` reports the new capacity | `k8s.TestWorkspaceGrowsBeforeItFills` on kind with an expandable CSI class | not built |
| An expansion the storage refuses sets `WorkspaceResized` false and is retried | `TestRefusedExpansionRetries` | not built |
| A working set past `MemoryGrowAt` at two readings raises memory in place, the process still running | `k8s.TestMemoryGrowsInPlace` on kind | not built |
| A resize the node cannot hold moves the sandbox once no session is open, keeps the workspace, and records `moved: true` | `k8s.TestDeferredResizeMoves` | not built |
| After an `OOMKilled`, the sandbox moves at once to the larger size | `k8s.TestOOMKillGrowsMemory` | not built |
| A start after a stop takes `status.resources`; an update of `spec.resources.memory` resets it | `TestStartTakesTheLastSize` | not built |
| Usage past `CeilingWarnAt` at the ceiling emits one `sandbox.at_ceiling` per crossing | `TestAtCeilingOncePerCrossing` | not built |
| Podman raises a running container's memory in place and declares no disk autosize | `podman.TestMemoryUpdateInPlace` | not built |
| The Kubernetes driver declares each autosize flag only when its condition holds | `k8s.TestAutosizeCapabilities` | not built |
