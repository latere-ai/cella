---
title: "Scheduling and sets: environment modes, capacity, the queue, preemption, pools, the SandboxSet kind for rollouts"
status: validated
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/007-admission.md
  - specs/010-state.md
  - specs/019-volumes.md
affects: [controller/, manifest/v1/, internal/api/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-13
author: changkun
---

# Scheduling and sets

## Overview

When and where a sandbox runs is a decision, and the control plane
owns it on the operator's behalf. An environment runs in one of two
modes: `direct`, which starts a sandbox now or fails, and `queued`,
which admits sandboxes against the environment's capacity by priority
and fair share, which is what a training loop wants when it asks for a
thousand diverse environments it does not need at once and cannot
afford to keep warm. A pool is an acceleration of either mode, never a
caller's choice. A `SandboxSet` is the unit such a loop asks for: one
template, many replicas with per-replica variants, completion by exit,
and the results collected into a volume. This spec fixes the modes,
capacity, the queue and its loop, preemption, pools, and the set.

## Current state

Not built. The hosted platform has a warm pool and nothing else: a
create either hits it or runs the slow path. A pool assumes a common
shape, and the environments a reinforcement learning rollout or an
evaluation matrix asks for are diverse by design.

## Design

### Scheduling is the environment's

An `Environment` declares `spec.scheduling.mode`
([[021-data-plane-workers]]):

| Mode | On create | On no capacity | Suits |
|---|---|---|---|
| `direct` | the controller calls `Create` at once | `Failed` with reason `NoCapacity` | a laptop, a small team, a platform that fronts its own queue |
| `queued` | the sandbox enters one of the environment's queues and starts when capacity allows | waits until `startDeadline`, then `Failed` with `StartDeadline` | rollouts, evaluations, batch work, any shared cluster |

A manifest never chooses the mode. On a `queued` environment it may
set `scheduling.priority`, `.queue`, `.startDeadline`, and
`.preemptible`; on a `direct` one every `scheduling` field is
`capability_unsupported` ([[003-manifest-contract]]). `scheduling.queue`
defaults to the environment's `spec.scheduling.defaultQueue` and must
be one of `spec.scheduling.queues`, else `invalid_field`.
`status.phase` is `Queued` while waiting, with `conditions[Scheduled]`
carrying a reason from `Queued`, `Preempted`, `NoCapacity`, `Placed`,
and a message with the position.

### The contract

```go
// Scheduler decides when and where a sandbox runs. controller calls
// Place at create step 2 and Release when a sandbox leaves a counted
// phase (005).
type Scheduler interface {
	Place(ctx context.Context, desired *v1.Sandbox) (Placement, error)
	Release(ctx context.Context, id string) error
}

type Placement struct {
	Kind     PlacementKind // Now, Adopt, or Queued
	AdoptID  string        // the pool entry to adopt, for Adopt
	Position int           // the position in its queue, for Queued
}
```

`Release` frees nothing itself, since capacity in use is derived
([[010-state]]); it wakes the loop so a queued head is tried at once.

### Capacity

An `Environment` declares capacity as `spec.capacity{cpu, memory,
disk, sandboxes}`, or `auto` on k8s, which reads the cluster's
allocatable and keeps `CELLA_CAPACITY_HEADROOM` (default `0.1`) of it
free. Capacity in use is derived, never stored: `cpu`, `memory`, and
`sandboxes` are summed over the sandboxes in `Pending`, `Starting`,
`Running`, `Stopping`, and `Recovering` on the environment; `disk` is
summed over those and over `Stopped` and `Failed` as well, because a
stopped sandbox's managed workspace and attached volumes stay on the
substrate until `Delete` ([[005-lifecycle-controller]], [[010-state]]).
`Lost` frees everything; `Recovering` re-takes it, and a recovery on a
full environment queues on a `queued` one at the sandbox's priority
and is `Failed NoCapacity` on a `direct` one. A pool entry counts. The
scheduler admits a sandbox when its resolved resources fit the
remainder. The authorizer's `max_sandboxes` and the count ceiling of
[[007-admission]] are per subject and are checked at apply; capacity is
per environment and is checked at placement; both hold.

### The queue and its loop

One queue per name per environment. Ordering, computed by
[[010-state]]'s `Dequeue` per call: `priority` descending; then fair
share, the subject whose sum of requested CPU in millicores over the
counted phases on that environment is smallest goes first; then
`enqueued_at`. The loop runs on the replica holding the `scheduler`
lease, one tick per `CELLA_SCHEDULE_INTERVAL` (default `5s`) and on
every `Release`: for every `queued` environment and every queue, it
dequeues while the head fits the remaining capacity, moves the
sandbox `Queued` to `Pending`, and resumes its create at step 3
([[005-lifecycle-controller]]). A head that does not fit blocks its
queue, so a large request is not starved by small ones behind it;
`startDeadline` bounds how long it may block.

### Preemption

When the head does not fit and is of higher priority than a running
`preemptible` sandbox, the loop stops victims until it fits: lowest
priority first, then largest requested CPU, then newest. A victim is
`Stopped` with `Scheduled: Preempted`, requeued with its original
`enqueued_at` so it keeps its place among equals, and its disk stays
counted. After `CELLA_MAX_PREEMPTIONS` (default `3`) a sandbox is no
longer a victim, which bounds starvation. `Limits.MaxPriority` from
the authorizer caps what a subject may ask ([[006-identity]]).

### Pools

With `spec.pool.size` above zero on an environment whose driver
declares `Pool`, the refill loop, under the `pool:<environment>` lease,
keeps that many sandboxes prewarmed: `CreateSpec.Prewarm` with
`spec.pool.image`, `spec.pool.resources`, `spec.pool.display`
([[021-data-plane-workers]]), no owner, no map, no credential, no
token, the image's entrypoint, an empty workspace, `Running`, labelled
`cella.latere.ai/pool: "true"`. A create matches an entry when its
resolved manifest equals the entry on every field `Adoption` cannot
carry: `image`, `resources`, `display`, `command` and `args` unset,
`ports` empty, `mesh.enabled` false. Adoption is then create step 3
onward with the driver call replaced: `Compile` mints the credential,
`Send` waits for the gateway's acknowledgement, volumes attach, the
token is minted, and `Update` with `Adopt{Owner, Name, Labels, Env,
Workspace, Lifecycle, Volumes, Token, Egress}` turns the entry into
the caller's sandbox in one exclusive act; the driver performs a git
workspace clone at adoption. The adopted sandbox's `createdAt` is the
adoption time. When a real create does not fit and pool entries hold
the capacity, the oldest entries are deleted first. In manifest terms
adoption is a create, not an update. Adoption is exclusive in the
driver, proved by `PrewarmAndAdoptIsExclusive` ([[004-runtime-contract]]),
and a pool never serves a create the authorizer refused, because the
authorizer decides before the scheduler is asked.

### The SandboxSet kind

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: SandboxSet
metadata:
  name: swe-rollout-42
spec:
  environment: rollouts           # a queued environment; the replicas' environment
  replicas: 256
  parallelism: 32                 # at most this many admitted at once
  completions: 256                # done when this many succeeded; default replicas
  template:                       # a Sandbox spec without environment
    image: ghcr.io/example/swe-env:2.1
    resources: {cpu: "2", memory: 4Gi}
    scheduling: {queue: rollouts, priority: 5, preemptible: true}
    lifecycle: {ttl: 2h}
  variants:                       # per-replica overrides, by index, cycling when shorter than replicas
    - {env: {TASK_ID: "django-1234"}, workspace: {source: git, git: {url: https://github.com/example/django, ref: a1b2c3}}}
    - {env: {TASK_ID: "flask-77"}, workspace: {source: git, git: {url: https://github.com/example/flask, ref: d4e5f6}}}
  completion:
    command: ["/workspace/run.sh"]   # optional; run once Running; its exit ends the replica; else the main process's exit does
    collect:
      paths: ["/workspace/result.json", "/workspace/trajectory.jsonl"]
      volume: rollout-42-results     # a Volume in the same environment, Available; the control plane writes <index>/ into it
  onFailure: continue                # continue | stop
status:
  id: set_01J9...
  owner: https://login.example.com|alice
  phase: Running                     # Pending | Running | Succeeded | Failed | Stopped
  counts: {pending: 180, running: 32, succeeded: 40, failed: 4, stopped: 0, collectFailed: 0}
  replicas:
    - {index: 0, sandbox: sbx_01J9..., phase: Succeeded, exitCode: 0, collected: true}
```

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `environment` | string | the default environment | no | a `queued` environment the caller may use; `direct` is `capability_unsupported` |
| `replicas` | int | none, required | no | 1 to `CELLA_MAX_SET_REPLICAS` (default `4096`) |
| `parallelism` | int | `replicas` | yes | 1 to `replicas` |
| `completions` | int | `replicas` | no | 1 to `replicas`; the set succeeds when this many replicas have |
| `template` | Sandbox spec | none, required | no | every `Sandbox` field but `environment`, which is the set's; resolved per replica |
| `variants[]` | list of partial Sandbox specs | empty | no | applied by index, cycling; a variant may set any template field; the merged result is what the boundary check below holds |
| `completion.command` | []string | none | no | run through `Exec` once the replica is `Running`; its exit code is the replica's; absent, the main process's exit is |
| `completion.collect.paths` | []string | empty | no | absolute paths exported from each replica after it ends |
| `completion.collect.volume` | string | none; required with `paths` | no | a `Volume` by name or `vol_` id, `Available`, in the set's environment; the control plane writes `<index>/` into it through `VolumeDriver.Write` ([[019-volumes]]) |
| `onFailure` | enum | `continue` | yes | `continue` admits every replica and reports; `stop` admits no replica after the first failure and lets running ones finish |

Each replica is an ordinary `Sandbox`, named `<set>-<index>`, owned by
the set's owner, carrying `status.set{name, index}`
([[003-manifest-contract]]), resolved from `template` with its variant
merged. The boundary check of [[003-manifest-contract]] holds every
replica to the template at apply, so a variant cannot widen what the
template declared, with the template read as a parent this way: rules
1 to 5 and 8 apply as written; rule 7 applies without the decrement,
so a replica's spawn budget and depth are at most the template's; rule
6 does not apply, a replica's `ttl` is its own; rule 9 does not apply,
and a template with `mesh.enabled` puts every replica in one mesh. The
count ceiling of [[007-admission]] is checked at apply against
`replicas`, so a set the owner could not hold is refused whole with
`quota_exceeded`; admission runs per replica as it is admitted, with
`Set{Name, Index}` in the request.

Replica phases are the set's own vocabulary, derived from the
sandbox's: `Pending` (not yet admitted, `Pending` or `Queued`),
`Running`, `Succeeded` (the completion exit code is 0), `Failed` (any
other exit, `Failed` for any reason, or `Lost` past recovery),
`Stopped` (the set was stopped). A sandbox whose main process exits 0
is `Stopped` with reason `Exited` ([[005-lifecycle-controller]]), which
the set reads as `Succeeded` when no `completion.command` is set.

Set phases: `Pending` until the first replica is admitted; `Running`;
`Succeeded` when `completions` replicas have succeeded; `Failed` when
`onFailure: stop` has fired and every admitted replica has ended, or
when `completions` can no longer be reached; `Stopped` after `POST
.../stop`, which stops every running replica and admits no more. On
`Succeeded` or `Failed`, replicas that have not been collected stay
until they are; every other replica is deleted. Deleting a set deletes
its replicas.

### Results collection

When a replica ends, the controller runs `ExportTar` over
`collect.paths` on the sandbox, which needs the driver's `Files`
capability since the sandbox is no longer `Running`, and streams it
into `VolumeDriver.Write` under `<index>/` in the results volume, then
deletes the replica. A replica is never deleted by the reaper before
collection: its `ttl` is extended by the controller while collection
is pending. A failed export or write is retried three times with
backoff, then the replica's row carries `collected: false`, the set's
`collectFailed` count moves, and `set.collect_failed` is emitted
([[009-events]]); the replica is then deleted. At apply the volume must
exist, be `Available`, and be in the set's environment, else
`invalid_field` at `spec.completion.collect.volume`.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `CELLA_SCHEDULING_MODE`, `CELLA_POOL_SIZE`, `CELLA_POOL_IMAGE` | `direct`, `0`, unset | the default environment's `spec.scheduling.mode`, `spec.pool.size`, `spec.pool.image` |
| `CELLA_SCHEDULE_INTERVAL` | `5s` | the scheduler loop's tick |
| `CELLA_CAPACITY_HEADROOM` | `0.1` | the fraction of allocatable an `auto` capacity keeps free |
| `CELLA_MAX_PREEMPTIONS` | `3` | how many times one sandbox may be preempted |
| `CELLA_MAX_SET_REPLICAS` | `4096` | the largest `replicas` |

### Package layout

`controller` gains one `Scheduler` implementation with the two modes,
the queue loop, capacity accounting, preemption, the pool refill loop,
and the set reconciler. `controller.Options.Scheduler` carries it, so a
platform importing the package supplies its own.

## Not in this spec

The phase machine of one sandbox ([[005-lifecycle-controller]]); the
routes for sets ([[008-api]]); the `Environment` fields
([[021-data-plane-workers]]); the store's `Dequeue` ordering test,
which [[010-state]] owns.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A `direct` environment fails at once without capacity; a `queued` one waits and starts when capacity frees; `startDeadline` fails it with the reason; a `scheduling` field on a `direct` environment is `capability_unsupported`; an unknown queue is `invalid_field` | `TestModes` under a fake clock | not built |
| The loop runs only on the lease holder, on the tick and on `Release`, and resumes a dequeued sandbox at create step 3 | `TestSchedulerLoop` | not built |
| The scheduler admits in priority, then smallest CPU sum per subject, then arrival, with three subjects and mixed priorities | `TestSchedulerHonoursQueueOrder` | not built |
| Capacity in use follows the two derivations, `Lost` frees, `Recovering` re-takes or queues, a pool entry counts, `auto` keeps the headroom, and a restart does not double count | `TestCapacityAccounting` | not built |
| Victims are chosen lowest priority, largest CPU, newest; a victim is `Stopped`, requeued with its `enqueued_at`, and after the cap is no longer a victim | `TestPreemption`, `TestPreemptionIsBounded` | not built |
| Two concurrent creates matching one pool entry yield one adoption and one slow path; the adopted sandbox has its own credential, map, token, and `createdAt`; a create with a `command` or `ports` does not adopt; oldest entries are deleted when a create does not fit | `TestPoolAdoption`, `TestPoolYieldsCapacity` | not built |
| A pool never serves a create the authorizer refused | `TestPoolIsBehindTheAuthorizer` | not built |
| Every field rule in the set table has a refusing case; `parallelism` and `onFailure` change mid-run and nothing else does | `TestSetFieldRules`, `TestSetUpdate` | not built |
| A set of 64 with parallelism 8 runs at most 8 at once, collects every replica's paths under its index, deletes replicas after collection, and ends `Succeeded` with the counts | `TestSetRunsToCompletion` on the native driver | not built |
| A replica with no command whose process exits 0 is `Succeeded`; exit 3 is `Failed`; `onFailure: stop` admits no more and lets running ones finish; `POST .../stop` stops them | `TestReplicaPhases`, `TestOnFailure`, `TestSetStop` | not built |
| A failed export is retried, then recorded as `collected: false` with the event, and the replica is then deleted; the reaper does not delete an uncollected replica | `TestCollectionFailure` | not built |
| A variant that widens the template's boundary is `boundary_exceeded` at set apply naming the index; a template with `mesh.enabled` yields replicas in one mesh | `TestVariantsCannotWiden`, `TestSetMesh` | not built |
| A set whose `replicas` exceed the owner's remaining count ceiling is refused whole with `quota_exceeded` | `TestSetCountCeiling` | not built |
| Deleting a set deletes its replicas and frees their capacity | `TestSetDeleteCascades` | not built |
