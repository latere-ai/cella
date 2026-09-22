# Capacity and queues

An environment declares how much it holds, and a scheduling mode that
decides what happens to a sandbox it cannot fit: fail it now, or hold it
in a queue until there is room. This page is for the operator who sets
both and for whoever writes the manifests that land there.

## Capacity

An environment's `spec.capacity` declares up to four quantities:

| Field | What it bounds |
|---|---|
| `cpu` | the sum of `spec.resources.cpu` over the sandboxes that run |
| `memory` | the sum of `spec.resources.memory` over the same sandboxes |
| `disk` | the sum of `spec.resources.disk` over those and the ones whose workspace is still on disk |
| `sandboxes` | how many run at once |

A quantity you leave out bounds nothing, and `capacity: auto` bounds
nothing either: the ceiling is then the cluster's or the host's, and the
control plane does not read it.

What is in use is worked out from the sandboxes the control plane holds,
every time it places one, and never stored. A sandbox holds its cpu,
memory and slot while it is `Pending`, `Starting`, `Running`, `Stopping`
or `Recovering`. It keeps holding its disk while it is `Stopped`, and
after a failure at the driver, because its workspace is still there until
you delete it. A sandbox that is `Queued`, or that failed before any
driver held it, holds nothing. The exception is a sandbox that was
[preempted](#preemption): it waits in `Queued` with its workspace kept,
so it still holds its disk. Each sandbox's resources are what its
manifest resolved to, with the operator's defaults filling what the
manifest left out.

Prewarmed pool entries count too, at the pool's own resources. When a
create does not fit because entries hold the room, the oldest entries
are deleted to make space for it.

The environment `cellad` drives itself takes its capacity from
`CELLA_CAPACITY_CPU`, `CELLA_CAPACITY_MEMORY`, `CELLA_CAPACITY_DISK` and
`CELLA_CAPACITY_SANDBOXES` when it first starts. After that, its stored
object is what counts, and you change it with `PUT /v1/environments/<name>`
like any other environment.

## The two modes

`spec.scheduling.mode` on the environment decides what happens to a
create that does not fit:

| Mode | The create fits | It does not fit |
|---|---|---|
| `direct` (the default) | the sandbox starts | the sandbox is `Failed` with the reason `NoCapacity` |
| `queued` | the sandbox starts, unless others already wait in its queue | the sandbox is `Queued`, and starts when there is room |

Either way the create answers `201` with the sandbox, and its `phase`
says what happened. A `Failed` sandbox keeps its name and counts toward
your own sandbox limit until you delete it, the same as one that failed
at the driver.

For the environment `cellad` drives itself, `CELLA_SCHEDULING_MODE=queued`
sets the mode at first start. On an environment you apply, set it in the
object along with the queues:

```json
"scheduling": {
  "mode": "queued",
  "queues": ["default", "rollouts"],
  "defaultQueue": "default"
}
```

## Waiting in a queue

On a queued environment, a manifest can say where it waits:

| Field | Default | Rule |
|---|---|---|
| `scheduling.priority` | `0` | zero or above; higher goes first; above the limit your authorizer grants you is `ceiling_exceeded` |
| `scheduling.queue` | the environment's `defaultQueue` | one of the environment's `queues`, else `invalid_field` |
| `scheduling.startDeadline` | none | a duration such as `10m`; a sandbox still waiting after that long is `Failed` with the reason `StartDeadline` |
| `scheduling.preemptible` | `false` | `true` lets the sandbox be stopped to make room for one of higher priority; see [Preemption](#preemption) |

On a `direct` environment every one of these is `capability_unsupported`,
because there is no queue to wait in. None of them can change after the
sandbox is created.

A queue starts sandboxes in this order:

1. Higher `priority` first.
2. Among equal priorities, the subject using the least cpu on the
   environment right now, so one subject cannot fill the environment
   while others wait.
3. Then the order they arrived in.

The sandbox at the front of a queue blocks the ones behind it. If it
needs more than is free, nothing behind it starts, even when it would
fit. Without this, a large request would wait forever behind a stream of
small ones. Its `startDeadline` is what bounds the wait.

A queued sandbox reports its place in the message of its `Scheduled`
condition:

```json
{
  "type": "Scheduled",
  "status": "False",
  "reason": "Queued",
  "message": "Position 3 of 12 in the queue rollouts."
}
```

The place is computed each time you read the sandbox, so it moves
forward as the queue drains. Once the sandbox starts, the condition turns
`True` with the reason `Placed`.

Nothing runs a queued sandbox yet, so `start`, `stop` and `exec` answer
`409 phase_conflict`. Delete works at once. Its `ttl` counts from when it
starts, not from when it joined the queue.

The control plane passes over its queues every `CELLA_SCHEDULE_INTERVAL`
(default `5s`). It also passes at once whenever a sandbox is queued,
stops, fails or is deleted, and whenever an environment is applied or
comes back to `Ready`, so a freed slot is taken without waiting for the
interval. With several replicas, one holds the `scheduler` lease and
places for all of them. The queue is the set of `Queued` sandboxes, so it
survives a restart unchanged.

When an environment declares more than one queue, each pass reads them
as one line in the order above, head against head, and a head that does
not fit holds back its own queue and no other.

## Preemption

A sandbox that sets `scheduling.preemptible: true` may be stopped to make
room for one of higher priority. When the front of a queue does not fit,
the control plane first gives up prewarmed pool entries, and then looks
at the running sandboxes on the same environment that are preemptible
and have a lower `priority` than the one waiting. It stops them in this
order until the waiting sandbox fits:

1. Lowest `priority` first.
2. Among equal priorities, the one using the most cpu.
3. Then the newest.

It stops only as many as it needs. If stopping all of them would still
not make room, it stops none: the waiting sandbox keeps waiting, and its
`startDeadline` is what bounds the wait. A stopped sandbox gives back its
cpu, memory and slot but keeps its disk, so preemption never makes room
on disk.

A preempted sandbox is not deleted. Its driver stops it with its
workspace kept, and it goes back into its own queue as `Queued`, at the
place its original arrival gives it among sandboxes of its priority. Its
`Scheduled` condition says why:

```json
{
  "type": "Scheduled",
  "status": "False",
  "reason": "Preempted",
  "message": "Position 1 of 4 in the queue rollouts."
}
```

`status.preemptions` counts how many times it has been stopped this way.
When there is room again it is started, not created: it runs with the
files, the identity and the boundary it had. While it waits, its
`startDeadline` no longer applies, because it has already started once,
and its `autoDelete` does not fire, because it is waiting to run again
rather than stopped for good. Its `ttl` still counts from when it was
first placed, and delete works at once.

After `CELLA_MAX_PREEMPTIONS` preemptions (default `3`, at most `100`) a
sandbox is no longer stopped for anyone, so a preemptible sandbox is not
kept from running forever. `CELLA_MAX_PREEMPTIONS=0` turns preemption off
for the whole installation. How high a caller may set `priority` is the
authorizer's decision: a priority above the limit it grants is
`ceiling_exceeded`.

## Watching it

On the scrape surface ([Observability](observability.md)):

| Metric | What it reads |
|---|---|
| `cella_queue_depth{environment, queue}` | how many sandboxes wait in each queue of a queued environment, zero for an empty one |
| `cella_capacity{environment, resource, kind}` | each quantity an environment declares (`kind="declared"`) beside what its sandboxes hold of it (`kind="used"`), in cores, bytes or sandboxes |
| `cella_preemptions_total` | how many sandboxes have been stopped to make room for one of higher priority |

Each preemption is also a `sandbox.stopped` event with the reason
`Preempted`.
