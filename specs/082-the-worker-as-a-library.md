---
title: "The worker as a library: an exported `worker` package a program embeds to serve an environment, a host policy that admits, holds or refuses each create and checks every operation, built-in checks against the host's own grants that the control plane cannot widen, a record of every operation for the host, and the host's OS, architecture, grants and network in the registration"
status: testing
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/013-security-and-threat-model.md
  - specs/021-data-plane-workers.md
  - specs/081-local-driver.md
affects: [worker/ (new, from internal/worker/), internal/worker/ (removed), runtime/remote/, manifest/v1/, controller/, internal/api/, cmd/cellad/, arch_test.go, api/openapi.yaml, docs/workers.md, CHANGELOG.md]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# The worker as a library

## Overview

A worker is how a machine Cella does not own serves an environment. It
is `cellad worker` today, and its loop is in `internal/worker`, so no
other program can embed it. A program that connects a person's own
computer (a command-line tool, a desktop application) has to run the
loop in its own process, with its own keychain, its own prompts and its
own log. This spec exports the loop.

It also gives the host a say. Today a worker executes whatever the
control plane sends, within what its driver can do. A worker on a
person's computer must be able to refuse: the person's own policy is the
last word, and a compromised or mistaken control plane must not be able
to widen it. Three additions do that:

1. a **host policy** the embedder supplies, asked before each create (it
   may hold the create while it asks a person) and before every other
   operation;
2. **built-in checks** that compare every create with what the host
   grants, whatever the policy says;
3. a **record** of every operation, handed to the embedder so the host
   keeps its own log.

The registration also carries the host's OS, architecture, grants and
network, so the control plane resolves a manifest against them and a
caller sees them.

## Current state

- `internal/worker/worker.go` holds `Options{URL, Key, Driver, Capacity,
  ...}`. It registers at `POST /v1/environments/{id|self}/workers`, opens
  `GET .../operations`, and reconnects with backoff from 1s to 30s.
- `runtime/remote/serve.go` (`Server`) and `execute.go` (`Execute`) are
  exported and turn each operation back into a driver call.
- `runtime/remote/protocol.go`'s `Registration` has `Worker, Driver,
  Isolation, Capabilities, Capacity, Labels, Version`, and no host facts.
- Nothing between the stream and the driver can refuse an operation.

## Design

### The package

`latere.ai/x/cella/worker` is a root package. `cellad worker` calls it
directly, and `internal/worker` is removed: no wrapper is kept.

```go
package worker

type Options struct {
	URL         string                        // the control plane's public URL
	Environment string                        // id, or "self" for the key's own
	Key         func(context.Context) (string, error) // read per connection, so a keychain or a rotation works
	Driver      runtime.Driver
	Capacity    v1.Capacity
	Labels      map[string]string
	Version     string
	Policy      HostPolicy                    // nil admits everything the built-in checks admit
	Record      func(Record)                  // nil records nothing
	KeepOnExit  bool                          // leave sandboxes running when ctx ends
	AdmitTimeout time.Duration                // how long a create may be held; 10 minutes when zero
	HTTPClient  *http.Client
}

func Run(ctx context.Context, o Options) error
```

`Run` returns when `ctx` ends, closing the stream with 1000, or on a
fatal answer from the control plane (`serve_refused`, `worker_limit`,
`worker_replaced`, `environment_mismatch`, `key_revoked`), which it does
not retry. The
depcheck row for `worker` admits `runtime/...`, `manifest/...`, `client`
and the standard library, and never a store.

### The host policy

```go
type HostPolicy interface {
	// Admit is asked before a create runs. It blocks until the host
	// decides or ctx ends; a person may be asked meanwhile.
	Admit(ctx context.Context, c Create) Verdict
	// Check is asked before every other operation on a sandbox.
	Check(ctx context.Context, op Operation) Verdict
}

type Create struct {
	Sandbox v1.Sandbox        // the resolved manifest, as the control plane sent it
	Labels  map[string]string // its labels, which name the caller's session when a caller sets them
}

type Operation struct {
	Type    string   // OpExec, OpWrite, OpRemove, ...
	Sandbox string
	Command []string // for exec
	Path    string   // for file operations
}

type Verdict struct {
	Allow  bool
	Reason string // shown to the caller on a refusal, at most 200 bytes
}
```

**Holding a create.** When `Admit` has not answered within one second,
the worker sends a `held` control frame for the operation. The control
plane sets the sandbox's condition `HostApproval` to `Pending` with the
reason "waiting for the host", visible on `GET /v1/sandboxes/{id}` and in
a `sandbox.host_pending` event. When `Admit` allows, the condition
becomes `True` and the create proceeds. When it refuses, the sandbox is
`Failed` with reason `host_refused` and the host's text. A create the
host has not answered within `Options.AdmitTimeout` (10 minutes when
zero; `cellad worker` reads `CELLA_HOST_ADMIT_TIMEOUT`, which spec 002's
table lists) is refused with `host_timeout`. The operation's lease does not expire
while a `held` operation's heartbeats continue.

**Check** runs before the driver call. A refusal answers the operation
with `host_refused` and the host's text. It is the embedder's place for
per-command rules; this package ships none.

### Built-in checks

Before `Admit`, the worker compares the create with the driver's own
`HostInfo` (spec 081's `HostDescriber`) and refuses it, naming the first
difference:

- `workspace.host` outside every grant, or a write to a read-only grant,
  answered `workspace_not_granted` as resolve and the driver answer it;
- an isolation class other than the driver's, answered `host_refused`;
- an image, a command or a volume the driver cannot honor, answered
  `host_refused`.

The network is not a refusal: the driver narrows whatever egress was
resolved to the host's own network (spec 081's table), so a control plane
that skipped resolve still gets no wider network than the host allows.
These hold whatever the policy answers and whatever the control plane
resolved. A worker also refuses an operation on a sandbox its driver
does not list.

### The record

`Record` is called once per operation, after it is answered:

```go
type Record struct {
	Time     time.Time
	Type     string
	Sandbox  string
	Labels   map[string]string
	Command  []string // exec
	Path     string   // files
	Outcome  string   // ok, refused, failed
	Reason   string
	ExitCode *int
}
```

It carries no file content, no stdin and no output. The embedder decides
where it goes.

### When the worker stops

Closing a stream does not end what the driver started: a detached stage
outlives it. So `Run` ends the host's work itself before it returns:

- on `ctx` ending, it stops every sandbox its driver lists, which ends
  their detached stages, unless `Options.KeepOnExit` is set (an operator's
  fleet may want its sandboxes to outlive a worker restart; a person's
  computer does not);
- on `key_revoked`, `serve_refused` or a deleted environment, it stops
  and deletes every sandbox its driver lists, whatever `KeepOnExit` says;
- on `worker_replaced`, it touches nothing and returns: another process
  holding this key now serves the same environment, possibly from the
  same data directory, and its sandboxes are that process's.

A worker that crashes cannot do either; at its next start it lists its
sandboxes and stops those the control plane no longer wants.

### The host in the registration

`Registration` gains `Host *v1.HostInfo` (spec 081 defines the type),
filled when the driver implements `HostDescriber`. A driver that does
not implement it, such as `k8s`, registers no host facts and serves no
`workspace.host`.

The control plane keeps it per worker and shows it on `GET
/v1/environments/{id}` in a new list, `status.hosts`, one entry per live
worker: `{worker, host}`. (`status.workers` stays the count it is.) It
resolves a
manifest's `workspace.host` and egress against the grants and network
that every live worker reports (the intersection, as for capabilities).
A change on the host takes effect at the next registration: the
embedder ends `Run` and starts it again.

## Not in this spec

- Who may own an environment, `environment.serve`, revocation and
  `spec.workers.max`: spec 083.
- Prompts, notifications or a policy file: the embedder's.
- Display and input over the stream.

## Acceptance criteria

1. `cellad worker` runs on `worker.Run`, `internal/worker` is gone, and
   `docs/workers.md` documents embedding with a runnable example under
   `examples/`.
2. `TestWorkerHoldsACreate`: a policy that answers after three seconds
   leaves the sandbox at `HostApproval=Pending` with a
   `sandbox.host_pending` event, then `Running`.
3. `TestWorkerRefusesACreate` and `TestWorkerAdmitTimeout`: `host_refused`
   with the host's text, and `host_timeout` after the configured time.
4. `TestBuiltInChecksIgnoreThePolicy`: with a policy that allows
   everything, a create whose `workspace.host` is outside the grants is
   refused with `workspace_not_granted`, and one that asks for `open` egress on an allowlist host runs
   with the host's list, including when the control plane is a stub that
   skips resolve.
5. `TestWorkerRefusesUnknownSandbox`: an exec for a sandbox the driver
   does not list is refused.
6. `TestRecordCarriesNoContent`: records for exec, write and read carry
   no bytes of stdin, output or file content.
7. `TestRegistrationCarriesHost`: a worker on `local` registers its OS,
   architecture, grants and network, and the environment's
   `status.hosts` shows them beside an unchanged `status.workers` count.
8. `TestRunStopsItsSandboxes`: a detached stage started through an exec
   is gone after `ctx` ends, and after `key_revoked` the sandbox is also
   deleted; with `KeepOnExit` the stage survives a `ctx` end; after
   `worker_replaced` the stage and the sandbox are untouched.
9. The depcheck row for `worker` holds, and `TestNoInboundToTheDataPlane`
   (spec 021) covers an embedded worker.

## Outcome

Built on 2026-10-18. `latere.ai/x/cella/worker` holds the loop, `cellad
worker` calls `worker.Run`, and `internal/worker` is gone. Criteria 1, 2,
3, 5, 6, 8 and 9 have their named tests (`TestWorkerHoldsACreate`,
`TestWorkerRefusesACreate` and `TestWorkerAdmitTimeout` end to end in
`cmd/cellad`; the rest in `worker/`), and `examples/worker` builds under
`TestExampleWorkerBuilds`. The spec stays at testing until spec 081 adds
the `local` variants of criteria 4 and 7.

Where the code diverges from the design:

- **Spec 081's types land here.** This spec ships before the local
  driver, so `v1.HostInfo`, `HostGrant`, `HostNetwork`,
  `runtime.HostDescriber`, `Capabilities.HostWorkspace`,
  `spec.workspace.host` and the resolve rules for it and for the host
  network (`workspace_not_granted`, narrowing with a warning) are built in
  this spec's commits. Resolve writes `workspace.path` and the default
  `workdir` as the host folder, so the file routes and the driver agree on
  where the workspace is.
- **Criteria 4 and 7 run against a test driver for now.**
  `TestBuiltInChecksIgnoreThePolicy` and `TestRegistrationCarriesHost`
  use the native driver behind a host it reports. Narrowing open egress
  to the host's list is the driver's, so the half of criterion 4 that
  reads the network is proved by the local driver's tests in spec 081.
- **`HostApproval` Pending is status `Unknown` with reason `Pending`.**
  Condition statuses are the three Kubernetes spells, so the waiting
  state is `Unknown`, message "Waiting for the host.", and becomes `True`
  with reason `Approved`, or `False` with `HostRefused` or `HostTimeout`
  and the host's own words as the message.
- **The sandbox's reason is `HostRefused` or `HostTimeout`.** Sandbox
  reasons are CamelCase like `CreateFailed`; the snake case words are the
  API's error codes, `host_refused` (403), `host_timeout` (504) and
  `workspace_not_granted` (422), each with its fixed message in
  `docs/api.md`.
- **The held frame reaches the controller through the hub.** The hub
  records each create it sends by operation id and calls the function the
  API installs with `Hub.OnHeld`, which writes the condition and the
  `sandbox.host_pending` record; a worker cannot name a sandbox of its
  own choosing. The lease needs no change: heartbeats continue on the
  stream while a create is held.
- **`Create.Sandbox` is built from what the worker receives.** A worker
  is sent a `runtime.CreateSpec`, not the manifest, so `Create.Sandbox`
  carries the id, owner and isolation in its status, the name and labels,
  and the image, command, workdir, workspace and egress in its spec. The
  workload token is never in it. `CreateSpec` gains `Isolation`, the
  class the sandbox resolved to, so the worker can compare it with its
  driver's.
- **The image and command check is the driver's.** A create mounting a
  volume on a driver that declares no `Volumes` is refused with
  `host_refused` before the policy. The runtime contract has no way to ask
  a driver which image or command it can honor short of creating, so a
  create the driver cannot honor that way is refused by the driver's own
  `Create`, after the policy.
- **`Operation` and `Record` carry `To`,** a move's destination, so a
  policy and a log see both ends of a move. `Options` also takes `Log`.
- **An operation on a sandbox the driver does not hold answers
  `not_found`,** recorded as refused, so a control plane deleting a
  sandbox its worker lost reads it as gone rather than failing the delete
  forever.
- **No separate reconcile at start.** A worker that crashed meets
  `serve_refused` at its next registration when its environment was
  deleted (spec 083), and the control plane retries the delete of a
  single sandbox until the worker answers it, so nothing beyond the
  existing paths is needed for the sandboxes the control plane no longer
  wants.
- **`cellad worker` sets `KeepOnExit`,** so an operator's worker keeps
  its sandboxes across a restart of the role as it always did.
- **The depcheck row admits more than the spec lists.** The worker
  reaches `github.com/gorilla/websocket` (its one stream),
  `latere.ai/x/pkg/httpjson` and `github.com/google/uuid` through the
  exported client, and `latere.ai/x/pkg/otel` with the OpenTelemetry SDK,
  because every outbound client of this repository carries the tracing
  transport and the `otel-client` gate holds it. The engines table of
  `arch_test.go` lists packages one by one, so the worker is held by
  `TestTheWorkerReachesNoStore` instead: no package under `internal/`, no
  driver, no store or engine client.
- **A registration refused with 401 is retried.** A key revoked while its
  worker was disconnected is refused at the verifier, before any code a
  worker stops on is chosen, so that worker keeps trying with backoff;
  a live stream is closed with `key_revoked` (spec 083).
- **`TestNoInboundToTheDataPlane`** is the embedded worker opening no
  listening socket across a sandbox's whole life, read from the process's
  own descriptors; the control plane's half is
  `TestRootPackagesDialNothing`. It leaves the threat model's pending
  list.
