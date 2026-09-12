---
title: "Data plane workers: the Environment kind, registration, the operation queue, the worker role, self-hosted sandboxes"
status: drafted
track: core
depends_on:
  - specs/004-runtime-backend-contract.md
  - specs/006-identity.md
  - specs/018-egress-and-secrets.md
affects: [manifest/v1/, runtime/remote/, internal/worker/, internal/api/, internal/auth/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Data plane workers

## Overview

An `Environment` is a place sandboxes run. The default one is the
driver `cellad` runs in-process. Every other one is registered: an
operator names it, the control plane issues it a key, and a worker on
the operator's infrastructure connects outbound with that key, reports
what its driver can do, and claims operations. The control plane never
dials in. This is how a customer runs sandboxes on their own cluster or
their own machines against a control plane someone else operates, the
shape a managed agent service uses for customer-hosted execution, and
how one control plane spans regions.

## Current state

Not built. The hosted platform drives one cluster in-process. The
split-plane design it drafted put a remote runtime behind the same
interface, which is what `runtime/remote` is, with the direction of
connection reversed so that the data plane needs no inbound route.

## Design

### The Environment kind

```yaml
apiVersion: cella.latere.ai/v1
kind: Environment
metadata:
  name: eu-gpu
  labels: {region: eu, gpu: "true"}
spec:
  mode: worker                     # inprocess | worker
  isolation: container             # what the driver provides; refused at registration if the worker reports otherwise
  capacity: {cpu: "512", memory: 2Ti, disk: 20Ti, sandboxes: 400}   # or auto
  defaults:
    strategy: queued
    queue: default
    image: ghcr.io/example/sandbox:1.4
  queues: [default, rollouts]
  gateway: https://egress.eu.example.internal:3128   # the environment's gateway, as sandboxes reach it
status:
  phase: Ready                     # Pending | Ready | Degraded | Offline
  driver: k8s
  capabilities: {Egress: true, Mesh: true, Volumes: true, Display: true, Pool: true}
  workers: 3
  lastHeartbeat: ...
  used: {cpu: "120", memory: 500Gi, sandboxes: 88}
```

`POST /v1/environments/{name}/keys` mints an environment key: a token
signed by `cellad` with `sub: environment:<name>`, `aud: cella`, no
expiry, revocable, shown once. The operator puts it on the worker's
host. An environment with no worker heartbeat for
`CELLA_ENVIRONMENT_OFFLINE` (default `2m`) is `Offline`: new sandboxes
for it queue or fail by their strategy, running ones are `Lost` after
the grace of [[005-lifecycle-controller]] and recovered when a worker
returns.

### The worker

The `worker` role of `cellad` reads `CELLA_URL`,
`CELLA_ENVIRONMENT_KEY`, and the driver's own variables, runs
`Preflight`, registers (`POST /v1/environments/{name}/workers` with the
driver's name, isolation, capabilities, and the host's capacity), and
then loops: claim, execute, report. Several workers may serve one
environment; each claims what it can and the queue is the arbiter. The
role is `internal/worker`, with a dependency allow list that reaches
the drivers and never the Postgres driver.

The connection is one long-lived HTTP/2 stream per worker to
`GET /v1/environments/{name}/operations?claim=1`, over which the control
plane sends operations and the worker sends results and heartbeats;
a stream that drops is reconnected with backoff and operations not
acknowledged are redelivered. Streams into a sandbox (exec, attach,
logs, files, dial) are multiplexed over the same connection as
sub-streams keyed by operation id, so a self-hosted environment needs
one outbound route and no inbound one.

### Operations

Every `Driver` method is one operation type; the worker executes it
with its own driver and answers with the result or an error in the
driver's vocabulary. Two operations exist only for workers:

| Operation | Does |
|---|---|
| `egress.push` | the compiled map of [[018-egress-and-secrets]] for one sandbox; the worker `PUT`s it to its environment's gateway, so a secret value crosses one hop into the customer's plane and is never dialed in |
| `egress.purge` | the reverse, on delete |

An operation carries the sandbox's desired state hash; a worker whose
observed state disagrees answers with a conflict and the controller
reconciles rather than repeating.

### Identity across the seam

The worker authenticates to the control plane with its environment
key. A sandbox on a worker's environment authenticates to the control
plane with its workload token exactly as any sandbox does; the token
is projected by the worker's driver from the operation's payload, and
the worker never mints one. The gateway on the worker's side verifies
the same tokens against `cellad`'s key set over the worker's outbound
route.

### What the control plane keeps

Desired state for every sandbox on every environment, so a worker that
returns is told what should exist; observed state as the worker reports
it; the queue and capacity per environment. The worker keeps nothing
but what its driver stamps into its substrate, so a worker host that is
replaced loses nothing the control plane had.

### The in-process environment

`spec.mode: inprocess` is the environment `CELLA_RUNTIME` provides,
created at start as `default` when no `Environment` object names it,
and the only mode that runs a driver inside `cellad`. A control plane
may have none, and serve only workers.

## Not in this spec

The drivers a worker runs ([[004-runtime-backend-contract]]); the
queue's ordering ([[020-scheduling-and-sets]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A worker with a valid key registers, reports its capabilities, and the environment is `Ready`; a revoked key is refused | `TestWorkerRegistration` | not built |
| Every driver method, issued through `runtime/remote`, executes on a worker running `native` and returns the same result as the direct call | `TestRemoteConformance` in `runtimetest` | not built |
| An exec of 64 MiB output, an attach with resize, and a tar both ways stream through the worker's single connection without buffering | `TestRemoteStreams` | not built |
| A dropped connection redelivers unacknowledged operations exactly once | `TestRedelivery` | not built |
| `egress.push` lands the map on the worker's gateway and the value never appears in a control plane log or in the sandbox | `TestEgressPushCrossesOneHop` | not built |
| An environment with no heartbeat goes `Offline`, its running sandboxes go `Lost` after the grace, and recover when a worker returns | `TestOfflineAndRecovery` | not built |
| A worker whose driver reports a different isolation than the environment declares is refused at registration | `TestIsolationMismatch` | not built |
| The control plane makes no outbound connection to a worker's host during the whole e2e tier | `TestNoInboundToTheDataPlane` with a firewall on the worker's side | not built |
