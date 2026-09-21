---
title: "Data plane workers: the Environment kind, the default environment, registration, the worker stream and its operations, self-hosted sandboxes"
status: in-progress
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/008-api.md
  - specs/018-egress-and-secrets.md
affects: [manifest/v1/, runtime/remote/, internal/worker/, internal/api/, internal/auth/, internal/config/, controller/, internal/store/]
effort: large
created: 2026-09-12
updated: 2026-09-21
author: changkun
---

# Data plane workers

## Overview

An `Environment` is a place sandboxes run. The default one is the
driver `cellad` runs in-process. Every other one is registered: an
operator applies it, mints it a key, and a worker on the operator's
infrastructure connects outbound with that key, reports what its
driver can do, and claims operations. The control plane never dials
in. This is how a customer runs sandboxes on their own cluster or
their own machines against a control plane someone else operates, and
how one control plane spans regions.

## Current state

Not built. The hosted platform drives one cluster in-process. The
split-plane design it drafted put a remote driver behind the same
interface, which is what `runtime/remote` is, with the direction of
connection reversed so that the data plane needs no inbound route.

## Design

### The Environment kind

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: Environment
metadata:
  name: eu-gpu
  labels: {region: eu, gpu: "true"}
spec:
  mode: worker                     # inprocess | worker
  isolation: container             # what the driver provides; a worker reporting otherwise is refused
  capacity: {cpu: "512", memory: 2Ti, disk: 20Ti, sandboxes: 400}   # the ceiling; or auto on an inprocess k8s environment
  scheduling:
    mode: queued                   # direct | queued; a manifest never chooses
    queues: [default, rollouts]
    defaultQueue: default
  pool:
    size: 4                        # 0 disables; needs the Pool capability
    image: ghcr.io/example/sandbox:1.4
    resources: {cpu: "1", memory: 2Gi, disk: 10Gi}
    display: null
  workspaceClass: ""               # the storage class of every managed workspace; the driver's default when empty
  gateway: egress.eu.example.internal   # the host sandboxes reach the gateway at; empty on an environment whose driver enforces no egress
status:
  id: env_01J9...
  owner: https://login.example.com|ops
  phase: Ready                     # Pending | Ready | Degraded | Offline
  reason: ""                       # for Degraded: NoGateway, NoWorker; for Offline: HeartbeatLost, DriverNotReady
  driver: k8s
  isolation: container
  capabilities: {egress: [none, allowlist, open], mesh: true, volumes: true, display: true, pool: true}
  workers: 3
  gateways: 2
  lastHeartbeat: ...
  used: {cpu: "120", memory: 500Gi, disk: 4Ti, sandboxes: 88}
```

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `mode` | enum | `worker` | no | `inprocess` only for the default environment, which the control plane creates itself |
| `isolation` | enum | none, required | no | `container`, `vm`, `process`, `none`; a worker or driver reporting another class is refused with `environment_mismatch` |
| `capacity` | object or `auto` | none, required | yes | quantities and a count; `auto` is valid only on `mode: inprocess` with the k8s driver, where it is the cluster's allocatable minus `CELLA_CAPACITY_HEADROOM` ([[020-scheduling-and-sets]]); on `mode: worker` it is `invalid_field` |
| `scheduling.mode` | enum | `direct` | yes | `direct` or `queued`; a change applies to the next placement |
| `scheduling.queues` | []string | `[default]` | yes | DNS-1123 labels; removing a queue with queued sandboxes is `phase_conflict` |
| `scheduling.defaultQueue` | string | `default` | yes | one of `queues` |
| `pool.size` | int | `0` | yes | 0 to `capacity.sandboxes`; above 0 needs the `Pool` capability, else `capability_unsupported` |
| `pool.image`, `.resources`, `.display` | as a Sandbox's | none; `image` and `resources` required when `size` > 0 | yes | the entries' shape ([[020-scheduling-and-sets]]) |
| `workspaceClass` | string | empty | yes, for new sandboxes | a class the driver knows, else `invalid_field` at the first create that uses it |
| `gateway` | string | empty | yes | a host or `host:port` sandboxes reach the gateway at; required when the driver's `egress` list is non-empty, else `missing_field` |

`metadata.name` is a DNS-1123 label; the name `CELLA_DEFAULT_ENVIRONMENT`
names is reserved to the control plane's own object. The rules of the
`scheduling`, `pool`, and `capacity` fields are
[[020-scheduling-and-sets]]'s; this spec fixes their presence and
shape.

`status` is written by the replica holding the `environments` lease
([[010-state]]), every 5 seconds, from the `workers` rows, the
gateways connected, and the driver's `Ready`:

| Phase | When | Gates |
|---|---|---|
| `Pending` | applied, no worker has registered, or the in-process driver has not passed `Ready` yet | nothing places here |
| `Ready` | at least one live worker, or the in-process driver `Ready`, and, when `spec.gateway` is set, at least one gateway connected | placement, the reaper, the watch |
| `Degraded` | a live worker or driver but no gateway (`NoGateway`), or a gateway but no worker within the window on `mode: worker` (`NoWorker`) | creates fail with `driver_unavailable`; running sandboxes keep running; the reaper holds |
| `Offline` | no worker heartbeat for `CELLA_ENVIRONMENT_OFFLINE` (`HeartbeatLost`), or the in-process driver failing `Ready` for as long (`DriverNotReady`) | as `Degraded`, and every sandbox with no observed counterpart is held `Lost` until `Ready` returns ([[005-lifecycle-controller]]) |

`driver` is recorded from the first registration and every later
worker must report the same name; `isolation` is `spec.isolation`
confirmed; `capabilities` is the intersection of the live workers'
reports, or the in-process driver's. `used` is [[020-scheduling-and-sets]]'s
derivation.

### The default environment

At start, when `CELLA_RUNTIME` is not `none` and no `Environment`
object bears the name `CELLA_DEFAULT_ENVIRONMENT` (default `default`),
the control plane creates one: `mode: inprocess`, `isolation` from the
driver, `capacity` from `CELLA_CAPACITY` (default `auto` on k8s,
required otherwise), `scheduling.mode` from `CELLA_SCHEDULING_MODE`,
`pool` from `CELLA_POOL_*`, `gateway` from `CELLA_GATEWAY`, `owner` the
subject `controller`. The variables seed the object once; afterwards
the stored object is authoritative and an admin edits it through the
API, so a changed variable does not silently override an edit. It is
never deletable. With `CELLA_RUNTIME=none` no object is created, and
`CELLA_DEFAULT_ENVIRONMENT` names an environment the operator applies
and keys; a manifest with no `environment` field gets that name, and
the owner policy's rule that every subject may `use` the default
environment applies to it ([[003-manifest-contract]],
[[006-identity]]). A control plane whose named default does not exist
refuses every sandbox apply with `not_found` at `spec.environment`.

### Keys and registration

`POST /v1/environments/{id}/keys` mints an environment key per
[[006-identity]]: `sub: environment:<env_ id>`, an `exp` of
`CELLA_ENVIRONMENT_KEY_TTL`, a `jti` that `DELETE .../keys/{jti}`
revokes, shown once; an environment holds several so each worker and
gateway carries its own. A key authorizes the registration route, the
worker stream, and the gateway stream of its environment, and is
refused everywhere else.

```
POST /v1/environments/{id}/workers      Authorization: Bearer <environment key>
{"driver": "k8s", "isolation": "container", "capabilities": {...},
 "capacity": {"cpu": "128", "memory": "512Gi", "disk": "5Ti", "sandboxes": 100}, "version": "v0.3.0"}

201 {"worker": "wrk_01J9...", "heartbeatInterval": "15s", "lease": "45s"}
```

Registration is refused with `environment_mismatch` (422) when
`isolation` differs from `spec.isolation` or `driver` differs from the
recorded one. The worker id is a `wrk_` ULID the control plane mints
per registration; a worker that restarts registers again and its old
claims lapse. A worker whose own `Preflight` fails exits 1 and never
registers. Capacity: `spec.capacity` is the ceiling; the live workers'
reports sum to what the substrate can hold; placement admits against
the lesser of the two ([[020-scheduling-and-sets]]).

### The worker

`cellad worker` reads `CELLA_URL`, which must be `https://` unless it
is a loopback address or `CELLA_INSECURE_CONTROL_PLANE=1` is set, a
hatch the stubs use and no deployment does, the same rule the gateway
and the client apply ([[018-egress-and-secrets]], [[011-agent-client]]);
`CELLA_ENVIRONMENT_KEY`; the driver's own variables; and
`CELLA_RUNTIME` for which driver to run;
runs `Preflight`; registers; and opens one WebSocket to
`GET /v1/environments/{id}/operations`, subprotocol `cella.worker.v1`
([[008-api]]), with the environment key as bearer and its worker id in
`hello`. Several workers may serve one environment; each claims what it
can and the operations table is the arbiter ([[010-state]]). The
stream drops on any error and is reopened with backoff from 1 second
doubling to 30 seconds; a heartbeat every 15 seconds, in both
directions, and a connection without one for 45 seconds is closed by
either side, which is the lease after which a worker's claims are
redelivered. The worker keeps nothing but what its driver stamps into
its substrate, so a replaced host loses nothing the control plane had.

### The stream

Every frame is a 26-byte operation id, one byte of sub-stream, and the
payload: `0` control, `1` stdin, `2` stdout, `3` stderr, `4` bytes.
Control frames are JSON:

| Message | Direction | Meaning |
|---|---|---|
| `hello {worker, versions}` | up, first | the registered worker id and the highest operation id it has acknowledged |
| `heartbeat` | both | every 15 seconds |
| `operation {id, type, sandbox, payload, desiredVersion, traceparent}` | down | one driver call, carrying the W3C trace context the worker continues ([[017-observability]]); the worker answers with a conflict when its observed state's version is not `desiredVersion` |
| `result {id, ok, value, error}` | up | the call's non-stream return, or a driver error in the driver's vocabulary |
| `cancel {id}` | down | the caller went away; the worker cancels the operation's context |
| `credit {id, bytes}` | both | flow control per operation: a side sends at most 8 MiB on an operation's sub-streams beyond what the other side has credited |
| `event {environment, state}` | up | one `runtime.Event` from the worker's `Watch`, for the long-lived `Watch` operation |

An operation type is the `Driver` or optional-interface method name;
its payload is the method's arguments less the context, JSON-encoded
with `io.Reader` and `io.Writer` arguments replaced by sub-streams; its
result is the method's non-stream returns. The exceptions:

| Method | On the stream |
|---|---|
| `Exec` | `1` stdin down, `2` and `3` up; `Wait`'s exit code in `result` |
| `Attach` | `1` down, `2` up; resize as a control `resize {id, cols, rows}`; the exit in `result` |
| `Dial` | `4` both ways |
| `Logs` | `2` up |
| `ExportTar` | `4` up; `ImportTar` `4` down |
| `Screen` | `4` up, one frame per message |
| `Watch` | one long-lived operation per connection; `event` messages up; a `relist` event tells the control plane to issue `List` |
| `Create` | the payload carries the workload token, the CA, `CELLA_URL`, and the placeholders the worker's driver projects; the worker never mints a token |

A sub-stream is closed by a zero-length frame. A caller's disconnect at
the API ([[008-api]]) sends `cancel`. Frames are at most 1 MiB. On
reconnect, `hello` names the last acknowledged operation and the
control plane redelivers, through the operations table, every claimed
operation whose worker lease lapsed, exactly once to a live worker.

### Identity across the seam

The worker authenticates to the control plane with its environment
key. A sandbox on a worker's environment authenticates to the control
plane with its workload token exactly as any sandbox does, reaching
`CELLA_URL` directly through the driver's rule; the token is projected
by the worker's driver from the `Create` payload. The gateway on the
worker's side is `cellad egress` with its own environment key on its
own stream ([[018-egress-and-secrets]]); it verifies no tokens and
authenticates sandboxes by the credential in their maps. Nothing dials
into the worker, which [[012-test-stubs-and-tiers]]'s worker and kind
tiers prove.

### Events

`environment.created` and `.updated` on apply; `.registered` per
worker registration with `{workers}`; `.offline` once per transition
into `Offline` and `.updated` once per return to `Ready`; `.keyed` and
`.key_revoked` per key with `{jti}`; `.deleted` ([[009-events]]).

### What the control plane keeps

Desired state for every sandbox on every environment, so a worker that
returns is told what should exist; observed state as workers report
it; the operations and workers tables; the queue and capacity per
environment ([[010-state]]).

## Not in this spec

The drivers a worker runs ([[004-runtime-contract]]); the rules of the
`scheduling`, `pool`, and `capacity` fields and the queue's ordering
([[020-scheduling-and-sets]]); the gateway's own stream
([[018-egress-and-secrets]]); the tiers that prove no inbound
connection ([[012-test-stubs-and-tiers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every field rule in the table has a refusing case; `auto` on `mode: worker` is `invalid_field`; a missing `gateway` with enforced egress is `missing_field` | `TestEnvironmentFieldRules` | built, [[051-environments-and-workers]] |
| Each phase transition fires on its trigger, is written by the lease holder, and gates what the table says; `Degraded NoGateway` fails a create with `driver_unavailable` | `TestEnvironmentPhases` under a fake clock | not built |
| The default environment is created from the variables at first start, is authoritative afterwards, is not deletable, and with `CELLA_RUNTIME=none` the named default is what a manifest gets and non-admins may use | `TestDefaultEnvironment`, three cases | not built |
| A worker with a valid key registers and receives a `wrk_` id; a revoked key, a mismatched isolation, and a mismatched driver are refused with their codes; a failed `Preflight` exits 1 without registering | `TestWorkerRegistrationRoute`, `TestRegistrationMismatch`, `TestRevokedKeyIsRefusedOnTheWorkerRoutes`, `TestWorkerRefusals` | built as `TestWorkerRegistrationRoute`, `TestRegistrationMismatch`, `TestRevokedKeyIsRefusedOnTheWorkerRoutes` and `TestWorkerRefusals`, [[051-environments-and-workers]] |
| Placement admits against the lesser of `spec.capacity` and the live workers' reports; `capabilities` is the intersection | `TestWorkerCapacityAndCapabilities` | not built |
| Every driver method and optional-interface method, issued through `runtime/remote`, executes on a worker running `native` and returns the same result as the direct call; the framing per row holds | `TestWorkerConformance` in `runtimetest`, `TestStreamFraming` | built, [[051-environments-and-workers]] |
| An exec of 64 MiB output, an attach with resize, a dial, a screen, and a tar both ways stream through one connection under credit without buffering more than the window | `TestRemoteStreams` | not built |
| A caller's disconnect cancels the operation on the worker within one heartbeat | `TestCancelCrossesTheSeam` | built, [[051-environments-and-workers]] |
| `Watch` events cross the seam and a `relist` triggers a `List` | `TestRemoteWatch` | not built |
| A dropped connection redelivers unacknowledged operations exactly once to a live worker after the lease | the `Redelivery` case of `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore` | built at the store as `storetest`'s `Redelivery` case over both adapters; the hub reads the live registrations from memory and a fleet reads them from the table, [[051-environments-and-workers]] |
| An environment with no heartbeat goes `Offline`, its running sandboxes are held `Lost`, and recover when a worker returns | `TestOfflineAndRecovery` | not built |
| The control plane makes no outbound connection to a worker's host during the whole worker and kind tiers | `TestNoInboundToTheDataPlane`, run as [[012-test-stubs-and-tiers]]'s `TestWorkerNoInbound` and `TestClusterWorkerNoInbound` | not built |
| A non-loopback `http://` `CELLA_URL` is refused at start unless the hatch is set | `TestLoadWorkerRefusals`, `TestWorkerRoleRefusesItsConfiguration` | built as `TestLoadWorkerRefusals` and `TestWorkerRoleRefusesItsConfiguration`, [[051-environments-and-workers]] |
| Every variable this spec names is in [[002-repository-scaffold]]'s table with the same default | `TestConfigTableAgrees` | not built: the table carries every variable, the worker's own included; the test that compares it to the code is not built, [[051-environments-and-workers]] |
