---
title: "Scheduling and sets: environment modes, queues, capacity, pools, the SandboxSet kind for rollouts"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
affects: [controller/, manifest/v1/, internal/api/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Scheduling and sets

## Overview

When and where a sandbox runs is a decision, and the control plane
owns it. Three strategies cover the shapes a platform meets: start it
now and fail if there is no room; take it from a pool of pre-started
sandboxes of a common shape, which is what an interactive agent wants
when it waits on a create; or queue it against an environment's
capacity with a priority and a fair share, which is what a training
loop wants when it asks for a thousand diverse environments it does not
need at once and cannot afford to keep warm. A `SandboxSet` is the unit
such a loop asks for: one template, many replicas with per-replica
variants, a completion policy, and the results collected. This spec
fixes the strategies, the queue, capacity, preemption, and the set.

## Current state

Not built. The hosted platform has a warm pool and nothing else; a
create either hits it or runs the slow path. A pool assumes a common
shape, and the environments a reinforcement learning rollout or an
evaluation matrix asks for are diverse by design, so keeping them warm
is keeping the wrong thing warm.

## Design

### Scheduling is the environment's

An `Environment` runs in one of two modes, declared in
`spec.scheduling.mode` ([[021-data-plane-workers]]):

| Mode | On create | On no capacity | Suits |
|---|---|---|---|
| `direct` | the controller calls `Create` at once | `Failed` with reason `NoCapacity` | a laptop, a small team, a platform that fronts its own queue |
| `queued` | the sandbox enters one of the environment's queues and starts when capacity allows | waits until `startDeadline`, then `Failed` with `StartDeadline` | rollouts, evaluations, batch work, any shared cluster |

A manifest never chooses the mode. On a `queued` environment it may
set `scheduling.priority`, `.queue`, `.startDeadline`, and
`.preemptible`; on a `direct` one every `scheduling` field is
`capability_unsupported` ([[003-manifest-contract]]). `status.phase` is
`Queued` while waiting, with `conditions[Scheduled]` carrying the
position and the reason.

### Capacity

An `Environment` declares capacity as the sum of resources it will
grant at once (`cpu`, `memory`, `disk`, and `sandboxes`), or `auto`
for a k8s environment that reads its cluster's allocatable and a
headroom fraction. The scheduler admits a sandbox when its resolved
resources fit the remaining capacity, and releases them at `Stopped`,
`Failed`, or `Deleting`. The authorizer's `limits.max_sandboxes` and
the admission count ceiling are per subject; capacity is per
environment; both hold.

### The queue

One queue per name per environment, `default` when unnamed. Ordering
is by `priority` descending, then by a fair share across subjects
(the subject with the smallest running resource sum goes first), then
by arrival. `preemptible: true` marks a running sandbox that the
scheduler may `Stop` to admit a higher priority when nothing else fits; the preempted one
returns to the queue with `conditions[Scheduled]: Preempted` and its
place by its own priority. A subject's `limits.max_priority` from the
authorizer caps what it may ask.

### Pools

A pool is the environment's acceleration of either mode, never a
caller's choice. With `spec.pool.size` above zero on an environment
whose driver declares `Pool`, the controller keeps that many sandboxes
created from `spec.pool.image` with the environment's default resources
and no owner, in `Running`, labelled `cella.latere.ai/pool: "true"`. A
create whose resolved shape (image, resources, egress mode, display)
matches an entry adopts it: the driver applies the caller's labels,
env, volumes, secrets map, and token through `Update` with `Adopt`,
and the pool refills. In manifest terms adoption is a create, not an
update: the `Change` the driver receives carries fields
[[003-manifest-contract]] marks immutable, because the object never
existed for the caller. A create that matches nothing goes the slow
path of its mode. Adoption is exclusive in the driver, proved by the
conformance case `PrewarmAndAdoptIsExclusive`. A pool never serves a
create the authorizer refused, because the authorizer decides before
the scheduler is asked.

### The SandboxSet kind

```yaml
apiVersion: cella.latere.ai/v1
kind: SandboxSet
metadata:
  name: swe-rollout-42
spec:
  replicas: 256
  parallelism: 32                  # at most this many Running at once
  completions: 256                 # done when this many finished; default replicas
  template:                        # a Sandbox spec; the set's environment must be queued
    image: ghcr.io/example/swe-env:2.1
    resources: {cpu: "2", memory: 4Gi}
    scheduling: {queue: rollouts, priority: 5, preemptible: true}
    lifecycle: {ttl: 2h}
  variants:                        # per-replica overrides, applied by index; cycles when shorter than replicas
    - {env: {TASK_ID: "django-1234"}, workspace: {source: git, git: {url: https://github.com/example/django, ref: a1b2c3}}}
    - {env: {TASK_ID: "flask-77"}, workspace: {source: git, git: {url: https://github.com/example/flask, ref: d4e5f6}}}
  completion:
    when: exit                     # exit | ttl | manual
    command: ["/workspace/run.sh"] # run in each sandbox once Running; its exit ends the replica
    collect:                       # what the control plane keeps per replica before delete
      paths: ["/workspace/result.json", "/workspace/trajectory.jsonl"]
      volume: rollout-42-results   # a shared-read Volume the collected tars land in, one directory per replica
  onFailure: continue              # continue | stop
status:
  phase: Running                   # Pending | Running | Succeeded | Failed | Stopped
  counts: {queued: 180, running: 32, succeeded: 40, failed: 4}
  replicas:
    - {index: 0, sandbox: 01J9..., phase: Succeeded, exitCode: 0}
```

Each replica is an ordinary `Sandbox` owned by the set, resolved from
`template` with its variant applied and the boundary check of
[[003-manifest-contract]] run against the template as `Parent`, with a
synthetic status (`expiresAt` from the set's creation and the
template's `ttl`, `spawn.used` zero, the template's environment), so a
variant cannot widen what the template declared. `parallelism` bounds the set's own concurrency inside the
queue's capacity. `completion.command` runs through `Exec` once the
replica is `Running` and its exit code decides `Succeeded` or `Failed`;
`collect.paths` are exported as a tar into the results volume under
`<index>/` before the replica is deleted, so a rollout's trajectories
outlive its sandboxes without a second service. `onFailure: stop`
stops admitting replicas after the first failure; `continue` runs the
set to completion and reports the counts. Deleting a set deletes its
replicas.

A set is what a training loop or an evaluation matrix applies: one
document, a thousand environments, the results in one volume, the
sandboxes gone. The same document with `replicas: 1` and no
`completion` is a plain sandbox created through the queue.

### Package layout

`controller` gains `Scheduler` with the three strategies as
implementations of one interface, the queue, capacity accounting, and
the set reconciler. `controller.Options` carries the strategies, so a
platform importing the package registers its own.

## Not in this spec

The phase machine of one sandbox ([[005-lifecycle-controller]]); the
routes for sets ([[008-api]]); capacity reporting by a worker
([[021-data-plane-workers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A `direct` environment fails at once without capacity; a `queued` one waits and starts when capacity frees; `startDeadline` fails it with the reason | `TestModes` under a fake clock | not built |
| Queue order is priority, then fair share, then arrival, proved with three subjects and mixed priorities | `TestQueueOrder` | not built |
| A preemptible running sandbox is stopped for a higher priority and requeued with `Preempted`; a non-preemptible one is not | `TestPreemption` | not built |
| Capacity is released at `Stopped`, `Failed`, and `Deleting`, and never double counted across a restart | `TestCapacityAccounting` | not built |
| Two concurrent creates matching one pool entry yield one adoption and one slow path | `TestAdoptIsExclusive` | not built |
| A pool never serves a create the authorizer refused | `TestPoolIsBehindTheAuthorizer` | not built |
| A set of 64 with parallelism 8 runs at most 8 at once, collects every replica's paths into the results volume under its index, and reports the counts | `TestSetRunsToCompletion` on the native driver | not built |
| A variant that widens the template's boundary is `boundary_exceeded` at set apply, naming the index | `TestVariantsCannotWiden` | not built |
| `onFailure: stop` admits no replica after the first failure; `continue` finishes the set | `TestOnFailure` | not built |
| Deleting a set deletes its replicas and releases their capacity | `TestSetDeleteCascades` | not built |
