---
title: "Environment pools: the prewarmed entry, the match rule, adoption as one exclusive driver act, the refill loop under its lease"
status: in-progress
track: core
depends_on:
  - specs/020-scheduling-and-sets.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/037-lifecycle-enforcement.md
  - specs/.archive/039-egress-gateway.md
  - specs/.archive/045-workload-tokens.md
affects: [runtime/, controller/, manifest/v1/, internal/config/, cmd/cellad/, specs/]
effort: large
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# Environment pools

## Overview

Slice 038 of [[031-hosted-sandbox-consolidation]]. It ports the hosted
warm pool of `sandbox/internal/sandbox/pool` into the shape
[[020-scheduling-and-sets]] fixes: the pool is the environment's, a
manifest never asks for it, and an entry is an ordinary sandbox object
with no owner that a create turns into the caller's in one exclusive
driver act.

A create that adopts skips the image pull, the workspace provision and
the container start, which is the whole of what a pool buys: seconds
become milliseconds, and nothing else about the sandbox differs. The
acceleration is transparent. The caller's manifest, its boundary, its
identity and its lifecycle are the same whether the sandbox was adopted
or created, and `status.conditions[Scheduled].reason` is the only place
the difference is visible.

What the hosted product carried and this slice drops: per-image warm
floors keyed by a bucket of image and size, since the environment
declares one shape; the admin API that resized a floor at runtime, since
[[021-data-plane-workers]] makes the size a field of the `Environment`
object; and the hosted fast path that adopted a Pod already running for
another tenant, which this contract refuses because an entry has no
owner until adoption and an owner until deletion.

## Current state

Nothing prewarms. `controller.Create` walks the seven steps of
[[005-lifecycle-controller]] and calls `Driver.Create` at the last one;
`runtime.CreateSpec` has no `Prewarm`, `runtime.Change` no `Adopt`, and
`runtime.State` no `Pool`. Every driver declares `Capabilities.Pool`
false, and [[004-runtime-contract]]'s `PrewarmAndAdoptIsExclusive` is
the one conformance case with no implementation behind it.

`manifest/v1.Environment` carries `spec.isolation` and the observed
status of the driver, which is what [[044-manifest-fields]] left it.
`spec.scheduling` and `spec.pool` are declared nowhere, so the
environment's own scheduling decision has no vocabulary yet.

## Design

### Scheduling is the environment's

`EnvironmentSpec` gains `scheduling` and `pool`:

```go
type EnvironmentSpec struct {
	Isolation  Isolation      `json:"isolation"`
	Scheduling SchedulingSpec `json:"scheduling,omitzero"`
	Pool       PoolSpec       `json:"pool,omitzero"`
}

type SchedulingSpec struct {
	Mode string `json:"mode,omitempty"` // direct; queued is 020's later work
}

type PoolSpec struct {
	Size      int       `json:"size,omitempty"`
	Image     string    `json:"image,omitempty"`
	Resources Resources `json:"resources,omitzero"`
	Display   *Display  `json:"display,omitempty"`
}
```

`mode` is `direct` here, which starts a sandbox now or fails. `queued`
is accepted vocabulary and refused as `capability_unsupported` until the
queue of [[020-scheduling-and-sets]] lands, so an operator who writes it
learns at start-up rather than watching sandboxes never start. A
manifest never chooses the mode, and this slice adds no `scheduling`
field to `SandboxSpec`.

The default environment's values come from the variables of the
configuration table below. There is no `Environment` store yet, so
`controller.Options` carries the pool of the one environment `cellad`
drives, typed as `v1.PoolSpec`: the vocabulary from the variable to the
refill loop is one type, and it is the type the stored object will hold
when [[021-data-plane-workers]] lands.

### The entry

An entry is a sandbox object the control plane made for nobody. The
driver receives `CreateSpec.Prewarm` and makes it with no owner, no
name, no token, no boundary, no user labels, the image's own entrypoint,
and an empty workspace, and stamps `cella.latere.ai/pool: "true"` on the
object it holds for that sandbox. `Inspect` and `List` report
`State.Pool` from that stamp, and `Filter.Pool` selects on it.

| Field | A prewarmed entry | After adoption |
|---|---|---|
| `Owner` | empty | the caller's subject |
| `Name` | empty | the manifest's `metadata.name` |
| `Labels` | the pool's shape stamp | the manifest's labels |
| `Env` | empty | the manifest's, with the boundary's placeholders |
| `Lifecycle` | none, so no deadline rule fires | the sandbox's own |
| `Token` | none | the workload token of [[045-workload-tokens]] |
| `Egress` | none | the compiled boundary of [[039-egress-gateway]] |
| `CreatedAt` | the prewarm | the adoption |
| `LastActivityAt` | the prewarm | the adoption |
| `Pool` | true | false |

`CreatedAt` and `LastActivityAt` both move, because the deadline rules
of [[005-lifecycle-controller]] count from them: an entry that sat warm
for two hours would otherwise be expired or auto-stopped on the first
tick after it was adopted.

An entry keeps the id it was prewarmed with, and the adopted sandbox
takes that id as its own `status.id`. The id is the object's name on
both container drivers, `cella-ws-<id>` on podman and the claim's name
on k8s, and neither can be renamed; carrying the entry's id forward is
what makes the boundary's principal, the token's subject and the
driver's stamped identity name one thing.

### The runtime contract

| Type | Addition |
|---|---|
| `CreateSpec` | `Prewarm bool` |
| `Change` | `Adopt *Adoption` |
| `Adoption` | `Owner, Name string`, `Labels, Env map[string]string`, `Workspace Workspace`, `Lifecycle Lifecycle`, `Token []byte`, `Egress Egress` |
| `State` | `Pool bool` |
| `Filter` | `Pool *bool` |

`Adoption` carries no `Volumes` yet: [[019-volumes]] has not landed and
`CreateSpec` has no mount list to rewrite. The field joins with that
slice.

`Adopt` is exclusive of every other field of `Change`: a call carrying
both is `ErrInvalid`, because adoption writes the same record a
mutation would and one act cannot be two. A driver that declares no
`Pool` answers `Prewarm` and `Adopt` with `ErrUnsupported`.

Adoption refuses with `ErrInvalid` when `Adoption.Workspace.Path`
differs from the entry's. The files are already provisioned at the
entry's path on every driver, and on both container drivers the path is
the mount destination the container was started with. The controller
reads that refusal as a mismatch and falls back to a real create; its
match rule holds the path as well, so the refusal is the second line of
defence and not the first.

### The match rule

A create matches an entry when the resolved manifest asks for nothing
the entry cannot carry:

| Field | Rule | Why |
|---|---|---|
| `image` | equals `spec.pool.image` | the container is already running that image |
| `resources` | equal `spec.pool.resources`, field by field, as written | the cgroup is already set |
| `display` | equals `spec.pool.display` | the desktop is already up or not |
| `command`, `args` | unset | the entry runs the image's entrypoint and a process cannot be replaced |
| `workspace.source` | unset or `empty` | the entry's workspace is empty |
| `workspace.path` | unset, or the entry's path | the files are provisioned there |
| `user` | unset | the container is already running as its user |

A quantity is compared as the caller wrote it rather than parsed,
because [[003-manifest-contract]] keeps the caller's own spelling and a
pool whose `1000m` did not match a manifest's `1` would merely be slower
and never wrong.

The environment's shape is stamped on the entry as
`cella.latere.ai/pool-shape`, the hash of the image, the resources and
the display the entry was made for. It is what the drift rule compares,
since `State` reports no image, and adoption clears it with the rest of
the labels.

### Adoption

Adoption is [[005-lifecycle-controller]]'s create from step 3 with the
driver call at step 7 replaced. Every step before it is the same act on
the same id, so an adopted sandbox and a created one hold the same
boundary, the same identity and the same record.

```mermaid
sequenceDiagram
    participant R as refill loop
    participant C as controller.Create
    participant G as gateway
    participant D as driver

    R->>D: Create{Prewarm, image, resources, display}
    D-->>R: entry, pool=true, no owner
    Note over R: repeats to min(size, capacity - live)

    C->>C: match the resolved manifest against an entry
    C->>C: id := entry.ID, write desired state
    C->>G: Compile and Send the boundary
    G-->>C: one gateway acknowledged
    C->>C: Mint the workload token
    C->>D: Update{Adopt{owner, name, labels, env, lifecycle, token, egress}}
    D->>D: claim: swap the record under a compare-and-swap
    D->>D: project: the token and the gateway's authority
    D-->>C: adopted
    C->>C: createdAt = now, Scheduled = FromPool, sandbox.created
```

The driver claims before it projects. The claim is the one act two
adopters race on, and the loser must not already have written its
caller's token into a container the winner now owns. A projection that
fails after a won claim deletes the entry and answers the error, so the
controller falls back to a real create rather than handing a caller a
sandbox with no identity.

A lost adoption is `ErrNotFound`: the entry the caller asked for is no
longer an entry. The controller undoes what it wrote under that id, the
purge of the boundary, the revocation of the token and the removal of
the desired row, and creates for real under a new id.

| Driver | The claim | Exclusivity |
|---|---|---|
| podman | the record volume swap of [[035-podman-driver]], `cella-rec-<id>-<n+1>` | the engine refuses a second volume of that name, so a generation is written once |
| k8s | the guarded JSON patch of [[036-k8s-driver]] | `test` on `resourceVersion` and on the pool label, both of which the winner changes |
| native | the record rewrite under the driver's lock | one process owns the directory, which is what the driver already requires |

The environment a workload reads is the environment the entry started
with, and the manifest's own reaches it through `Exec`, which is the
rule both container drivers already follow for `Change.Env`: podman and
k8s fix a process's environment at create. This is why the match rule
refuses a `command`. A sandbox with no command runs the entrypoint the
image chose and the caller's work arrives through `Exec`, which carries
the adopted record.

### Capacity

Capacity is a count here. [[020-scheduling-and-sets]]'s resource form
needs the granted resources of every sandbox, which no driver reports
yet, so `Options.Capacity` is the ceiling on sandboxes of the
environment and zero is no ceiling.

With ceiling `C`, owned sandboxes in a counted phase `N`, and entries
`P`, a real create fits when

$$N + P + 1 \le C$$

and an adoption always fits, because it turns one entry into one
sandbox and moves nothing. A create that does not fit while `P > 0`
deletes entries, oldest first, until it does. A create that does not fit
with no entry left is `ErrQuota`.

The refill loop's target is

$$T = \min(S, \max(0, C - N))$$

with `S` the declared `spec.pool.size`. Without the second term the loop
and the create path would fight: a create would evict an entry to make
room and the next tick would make it again.

### The refill loop

One loop per environment under the lease `pool:<environment>`, acquired
per tick the way the reaper acquires its own, ticking on the same
interval. Each tick lists the driver, counts what it holds, and:

| Condition | Act |
|---|---|
| entries below `T` | prewarm one at a time, up to `T` |
| entries above `T` | delete the oldest, down to `T` |
| an entry whose shape stamp is not the environment's | delete it |
| an entry that is not `Running` | delete it |
| `size` zero, or the driver declares no `Pool` | delete every entry and prewarm none |

Drift and the orphan are one rule read two ways: an entry the pool no
longer wants is deleted, whether the shape changed under it, the engine
moved it out of `Running`, or the size fell. The loop creates one entry
per tick per missing slot rather than all of them at once, so a pool of
fifty does not open fifty image pulls on the first tick of a cold start.

The reaper never ends an entry. An entry carries no owner and no
lifecycle, so `expired`, `autoDelete` and `autoStop` cannot fire on it
by construction; the loop states it as a rule anyway, because a driver
that stamped a deadline on an entry would otherwise have it reaped by a
rule that was never meant to see it. The lost rule does not see an entry
either: entries are not desired state, and the controller's map holds
one row per sandbox a caller asked for.

### Events and conditions

An adopted sandbox emits `sandbox.created`, the same record a created
one emits ([[042-events]]), because in manifest terms adoption is a
create. A prewarm and a deleted entry emit nothing: an entry is the
control plane's own machinery, not an act on a caller's object.

`status.conditions[Scheduled]` carries the placement:

| Reason | When |
|---|---|
| `FromPool` | the sandbox was adopted from an entry |
| `Placed` | the sandbox was created for real |

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `CELLA_SCHEDULING_MODE` | `direct` | the default environment's `spec.scheduling.mode`; `queued` is refused at start-up until the queue lands |
| `CELLA_POOL_SIZE` | `0` | `spec.pool.size`; zero runs no pool |
| `CELLA_POOL_IMAGE` | unset | `spec.pool.image` |
| `CELLA_POOL_CPU`, `CELLA_POOL_MEMORY`, `CELLA_POOL_DISK` | unset | `spec.pool.resources` |

A size above zero on a driver that declares no `Pool` is a start-up
problem naming the driver, which is [[021-data-plane-workers]]'s
`capability_unsupported` where the field is applied through the API. The
image is not required here: the native driver runs no image, and an
environment whose driver needs one refuses the prewarm itself.

## Not in this spec

The queue, its ordering, preemption, the `SandboxSet` kind and the
`Scheduler` interface ([[020-scheduling-and-sets]]); the resource form of
capacity, which waits on drivers reporting granted resources; the
`Environment` object's store, its API and the rest of its field table
([[021-data-plane-workers]]); `spec.display` on a sandbox, which
[[023-computer-use-operations]] owns, so `PoolSpec.Display` is declared
and carried and no driver reads it yet; the git workspace clone at
adoption, which waits on a workspace source the manifest does not have.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `Prewarm` makes an entry with no owner, no token and the pool stamp; `Inspect` reports `Pool`; an owner filter does not select it and does after adoption | `PrewarmIsNotOwned` in `runtimetest` | not built |
| Adoption rewrites owner, name, labels, env, lifecycle, token and the boundary, and moves `createdAt` and `lastActivityAt` to the adoption | `AdoptRewritesTheRecord` in `runtimetest` | not built |
| Two concurrent adoptions of one entry yield one success and one `ErrNotFound`, and the winner's record is whole | `PrewarmAndAdoptIsExclusive` in `runtimetest` | not built |
| A differing workspace path is `ErrInvalid` and the entry is untouched; `Adopt` beside another `Change` field is `ErrInvalid`; a driver without `Pool` answers `ErrUnsupported` | `AdoptRefusals` in `runtimetest` | not built |
| The match rule accepts an equal manifest and refuses each of image, resources, display, command, args, user, workspace source and workspace path | `TestPoolMatch` | not built |
| A matching create adopts: one `Update{Adopt}`, no `Create`, `Scheduled` is `FromPool`, `createdAt` is the adoption, the boundary and the token are the sandbox's own | `TestPoolAdoption` | not built |
| An adoption lost to a race falls back to a real create under a new id, with no leaked map, token or desired row | `TestPoolAdoptionFallsBack` | not built |
| A non-matching create never touches an entry and is `Scheduled: Placed` | `TestPoolMismatchCreatesForReal` | not built |
| The refill loop reaches `spec.pool.size` and stops, runs only under its lease, and holds `min(size, capacity - live)` | `TestPoolRefill`, `TestPoolRefillHoldsTheLease` | not built |
| A real create that does not fit deletes the oldest entries first, and is `ErrQuota` with none left | `TestPoolYieldsCapacity` | not built |
| An entry whose shape stamp differs, one that left `Running`, and every entry once the size is zero are deleted | `TestPoolDrift`, `TestPoolOrphans` | not built |
| The reaper ends no entry under any rule, with a deadline stamped on one | `TestReaperLeavesPoolEntries` | not built |
| A node on the native driver with `CELLA_POOL_SIZE=2` holds two entries, adopts a matching create with `FromPool` and a fresh `createdAt`, and creates a non-matching one for real | `TestPoolEndToEnd` in `cmd/cellad` | not built |
| `CELLA_SCHEDULING_MODE=queued` and a size above zero on a driver without `Pool` are start-up problems | `TestPoolConfig` | not built |

## Outcome

Written at completion.
