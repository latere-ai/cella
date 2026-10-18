---
title: "The worker as a library: an exported `worker` package a program embeds to serve an environment, a host policy that admits, holds or refuses each create and checks every operation, built-in checks against the host's own grants that the control plane cannot widen, a record of every operation for the host, and the host's OS, architecture, grants and network in the registration"
status: drafted
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
