---
title: "Kubernetes attach: a terminal and exec with stdin over the pods/exec subresource"
status: complete
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/008-api.md
  - specs/012-test-stubs-and-tiers.md
  - specs/014-release-and-installation.md
  - specs/015-conformance-suite.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/034-terminal-attach.md
  - specs/.archive/036-k8s-driver.md
affects: [runtime/k8s/, deploy/, deploy_test.go, .github/workflows/, docs/, specs/, CHANGELOG.md]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Kubernetes attach

## Overview

The Kubernetes driver is the driver a hosted deployment runs, and it
declares no `Attach`. `GET /v1/sandboxes/{id}/attach` and the exec socket
of [[008-api]] are therefore refused with `capability_unsupported` on every
sandbox it holds, and an `Exec` with `Stdin` or `TTY` is refused with
`ErrUnsupported`. The native and podman drivers implement the terminal
([[034-terminal-attach]]); this slice brings `runtime/k8s` to the same
contract of [[004-runtime-contract]], over the `pods/exec` subresource the
driver already opens for its commands, archives and file operations
([[036-k8s-driver]]).

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| The exec stream | `runtime/k8s/exec.go`: the fallback executor, WebSocket first and SPDY on an upgrade failure | no `TTY`, no terminal size queue; `Exec` refuses `Stdin` and `TTY` with "stdin and a tty need Attach" |
| The declaration | `Capabilities()` in `runtime/k8s/k8s.go` | `Files`, `Pool`, `Mesh`, and `Display` and `Input` with a display image; no `Attach` |
| The verb table and the Role | `verbs` in `runtime/k8s/k8s.go`, `deploy/base/rbac.yaml`, `driverVerbs` in `deploy_test.go` | `create` on `pods/exec` only, while the WebSocket executor upgrades with `GET` |
| The kind stack's conformance run | `-capabilities files,pool,mesh` in `verify.yml`'s install job and `release.yml`'s conformance job | `attach` is not declared, so `case008ExecSocket` and `case008AttachSocket` skip and the capability case holds the server to refusing both sockets |

## Design

### The terminal on the exec subresource

`Attach(ctx, id, req)` reads the sandbox the way `Exec` does (the claim,
a Pod that exists, is not terminating and has not terminated) and opens
one exec in the workload's container with `stdin`, `stdout` and `tty`
set and `stderr` unset: a terminal has one output stream, and the
kubelet drops `stderr` under a TTY in any case. The command is wrapped
with the sandbox's environment, the request's environment over it, and
the working directory, as `Exec` wraps it, because the subresource
carries neither. An empty command runs `sh`, which the wrapper's
`exec "$@"` resolves on the image's `PATH`.

A window with a negative side or a side above 65535 is `ErrInvalid`, a
command whose first word is blank is `ErrInvalid`, a sandbox with no
running Pod is `ErrNotRunning`, an unknown one `ErrNotFound`, and a
driver built without a cluster connection refuses with `ErrUnsupported`.

### The session

```mermaid
sequenceDiagram
  participant C as caller
  participant S as session
  participant X as executor
  participant A as API server
  C->>S: Attach(ctx, id, {Cols: 80, Rows: 24})
  S->>X: StreamWithContext(Stdin, Stdout, Tty, TerminalSizeQueue)
  X->>A: GET pods/{name}/exec?tty=true&stdin=true (v5.channel.k8s.io)
  alt the upgrade is refused
    X->>A: POST pods/{name}/exec (SPDY, v4.channel.k8s.io)
  end
  S-->>X: Next() = 80x24
  C->>S: Write(keystrokes)
  S-->>C: Read(terminal output)
  C->>S: Resize(120, 40)
  S-->>X: Next() = 120x40
  A-->>X: status on the error channel (exit code)
  X-->>S: StreamWithContext returns
  S-->>C: Read = io.EOF, Wait = exit code
```

| Method | Behavior |
|---|---|
| `Read` | what the terminal wrote; `io.EOF` once the exec has ended and every byte was read |
| `Write` | into a pipe the executor copies to the exec's stdin; an error once the session has ended or been closed |
| `Resize(cols, rows)` | the newest window replaces one the executor has not read yet; a side outside 1 to 65535 is `ErrInvalid`; after the session ended it is `ErrNotRunning`, which the API's frame loop and the remote driver read as moot |
| `Wait(ctx)` | the exit code from the status the API server sends on the error channel; a session the caller closed or whose context ended answers with the context's error; a transport failure answers with it |
| `Close` | cancels the exec's context, which closes the connection, and closes both pipes, so a pending `Read` and every later `Read` and `Write` fail; a second `Close` is nil |

The session's context derives from the one `Attach` was given, as the
native driver's does. The terminal size queue ends when the session
ends, so the executor's resize loop does not outlive it, and the stdin
pipe is closed then too, so the executor's stdin copy returns instead of
waiting for a keystroke that will never be delivered.

Closing the connection ends the exec's stdin and its terminal. What the
process inside does then is the container runtime's: a shell reading a
closed terminal exits, a process that ignores it runs until the sandbox
stops. The contract asks that the stream be over for its caller
([[034-terminal-attach]]), and that holds here whatever the runtime does.
`Stop` deletes the Pod, which ends every exec in it.

### Exec with stdin or a TTY

The refusal goes. `Stdin` is handed to the executor, which copies it to
the exec's stdin and closes that stream at the reader's end (the
close signal of `v5.channel.k8s.io`, or the SPDY stream's close), so a
command reading to the end of its input sees it. Without `TTY` the two
output streams stay apart. With `TTY` the exec runs under a terminal,
everything reaches `Stdout`, and `Stderr` is at its end from the first
read.

### The verbs the terminal needs

The executor tries a WebSocket first and falls back to SPDY when the
upgrade fails. The API server resolves the verb of a request from its
HTTP method: `GET`, which the WebSocket upgrade uses, is `get`, and
`POST`, which SPDY uses, is `create`. From Kubernetes 1.35 the pod
registry also requires `create` on the WebSocket upgrade, behind the
`AuthorizePodWebsocketUpgradeCreatePermission` gate, which is on by
default. A Role therefore grants both `get` and `create` on `pods/exec`,
the verb table `Preflight` reviews names both, and `deploy/base/rbac.yaml`
and the table `deploy_test.go` holds it to follow.

A Role that grants `create` alone still works against a cluster before
1.35, because the refused upgrade falls back to SPDY, but every exec pays
a refused round trip first, and `Preflight` names the missing `get` so an
operator sees it at start rather than in latency.

### Declaring it

`Capabilities()` adds `Attach: true`. The kind stack's conformance run
declares `attach` in both workflows, so `case008ExecSocket` and
`case008AttachSocket` run against `cellad` in a cluster, through the Role
the deploy tree ships, and `case004CapabilityGates` holds both sockets to
answering rather than refusing. The driver-level suite's `ExecStdin`,
`ExecTTY`, `AttachRoundTrip`, `AttachResize` and
`AttachCloseEndsTheStream` run in `TestClusterConformance` wherever a
cluster is configured for it.

## Not in this spec

`Dial` on this driver, through port forwarding. Wiring
`TestClusterConformance` into a workflow, which runs every driver case
against the kind cluster and not the terminal alone. A replay buffer or
a fan-out of one session to several viewers, which [[034-terminal-attach]]
leaves to [[023-computer-use-operations]].

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `Attach` opens an exec with `stdin` and `tty` and no `stderr`, wraps the command with the sandbox's and the request's environment and the directory, runs `sh` when the request names no command, carries bytes both ways, and ends its stream with the exit code readable from `Wait` | `TestAttachRoundTripOverTheExecStream` | passing |
| The window the request names is the first the executor reads, a resize replaces one not yet read, a resize after the end is `ErrNotRunning`, and the size queue ends with the session | `TestAttachResizeReachesTheQueue`, `TestTheWindowKeepsTheNewestSize` | passing |
| `Close` ends the stream for its caller: `Read` and `Write` fail, `Wait` answers with the cancellation, and the exec's context is cancelled; a session ends with the context it was opened with | `TestAttachCloseEndsTheSession`, `TestAttachEndsWithItsContext` | passing |
| A bad window, a blank command, a stopped sandbox, an unknown one, a cancelled context and a driver with no cluster connection are each refused before an exec is opened | `TestAttachRefusals` | passing |
| `Exec` with `Stdin` delivers it and keeps the two streams apart; with `TTY` it runs under a terminal with `Stderr` at its end; neither is refused any longer | `TestExecStdinKeepsTheStreamsApart`, `TestExecTTYHasOneStream`, `TestExecRefusals` | passing |
| Over a served exec endpoint speaking `v5.channel.k8s.io`, the real executor sends `tty` and `stdin`, delivers stdin and its end, carries a resize on the resize channel, and reads the exit code from the error channel | `TestTheExecSubresourceCarriesATerminal` | passing |
| An endpoint that refuses the WebSocket upgrade is reached over SPDY with the terminal intact, and each HTTP method the two transports use maps to a verb the table reviews | `TestTheExecStreamFallsBackToSPDY` | passing |
| `Preflight` reviews `get` and `create` on `pods/exec`, and the Role grants exactly the table | `TestPreflightNamesWhatIsMissing`, `TestRoleMatchesTheDriversVerbs` | passing |
| The driver declares `Attach` and implements `Attacher` | `TestDeclarations` | passing |
| The driver passes the attach and stdin cases of the conformance suite against a real cluster | `TestClusterConformance` | built and skipped here: no cluster could be reached from this machine, and no workflow runs it; see the Outcome |
| The kind stack's conformance run declares `attach`, so the exec and attach sockets run against `cellad` in a cluster through the Role the deploy tree ships | `TestContract` in `verify.yml`'s install job and `release.yml`'s conformance job | built: both runs pass `attach`; the first install job after this slice lands is the run that shows it |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates` | passing |

## Outcome

`runtime/k8s` declares `Attach` and implements `Attacher`, and `Exec`
takes `Stdin` and `TTY`. Both ride the exec path the driver already had:
the fallback executor, a WebSocket first and SPDY on an upgrade failure,
now with the terminal flag and the terminal size queue passed through.

| Piece | Where |
|---|---|
| `Attach`, the session, and the one-slot size queue | `runtime/k8s/attach.go` |
| `Exec` with stdin and a TTY, the shared wrapper, and `launch` with the hook that releases a terminal's input and queue when the exec ends | `runtime/k8s/exec.go` |
| `Attach` declared, `get` on `pods/exec` in the verb table | `runtime/k8s/k8s.go` |
| The terminal against the exec double, and against a served endpoint over both transports | `runtime/k8s/attach_test.go`, `runtime/k8s/endpoint_test.go`, `runtime/k8s/exec_test.go` |
| The Role, the test holding it to the table, the operator's page | `deploy/base/rbac.yaml`, `deploy_test.go`, `deploy/README.md` |
| `attach` declared on the kind stack's conformance runs | `.github/workflows/verify.yml`, `.github/workflows/release.yml` |
| The install walk's note and the changelog | `docs/install.md`, `CHANGELOG.md` |

Coverage of `runtime/k8s` under `go test -race -cover`: 93.3%, from 93.0%
before the slice; every function of `attach.go` is at 100%, and `launch`
and `wrapped` are too. The endpoint the wire tests serve is built from
the channel server of `k8s.io/apimachinery` (`wsstream`) and its SPDY
upgrader, both already in the build list, so no module was added.

### The verbs, from the source

The WebSocket executor upgrades with `GET`, and the question was which
verb the API server authorizes that as. `k8s.io/apiserver` v0.35.4,
`pkg/endpoints/request/requestinfo.go` lines 177 to 191, maps the HTTP
method to the verb with no exception for `exec`: `GET` is `get`, `POST`
is `create`. Kubernetes v1.35.0 adds a second check in the pod registry,
`pkg/registry/core/pod/rest/subresources.go` lines 180 to 185: behind
`AuthorizePodWebsocketUpgradeCreatePermission`, beta and on by default
from 1.35 (`pkg/features/kube_features.go` lines 1152 to 1153), the
upgrade also needs `create`. A Role therefore grants `get` and `create`
on `pods/exec`, and the verb table `Preflight` reviews names both.

A Role with `create` alone is not caught by the kind tier: the refused
upgrade reaches the executor as an upgrade failure (client-go
`transport/websocket/roundtripper.go` line 156) and it falls back to
SPDY, which `create` allows. `Preflight` is what names it, and
`TestTheExecStreamFallsBackToSPDY` holds both transports' methods to
the table.

### Decisions this slice made

- **`get` on `pods/exec` is fatal at start like every other verb.** The
  table is one list and `Preflight` treats every entry alike, so a
  control plane or a worker whose own Role lacks it stops at start-up
  with the rule named. On a cluster before 1.35 such a Role would still
  run every exec over SPDY; the slice does not special-case it, and the
  changelog says to add the verb before upgrading.
- **The session belongs to the context it was opened with**, as the
  native driver's does, so a caller that goes away ends the exec.
- **`Close` promises what every driver holds.** It cancels the exec,
  which closes the connection and with it the exec's stdin and terminal.
  A shell reading a closed terminal exits; a process that ignores it is
  the container runtime's until the sandbox stops.
- **The size queue holds one window.** A resize the executor has not sent
  yet is replaced by the newer one, because the terminal only has one
  size, and the queue answers nil once the session ends so the
  executor's resize loop returns.
- **The default command is `sh`, resolved by the wrapper.** Every argv
  already ends in `exec "$@"` under `sh -c`, so the image's `PATH` finds
  the shell; podman names `/bin/sh` because its exec has no wrapper.

### What diverges

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[004-runtime-contract]] | k8s `Attach` is "SPDY exec with a TTY" | a WebSocket first and SPDY as the fallback | the driver's exec path already used the fallback executor, which is how current `kubectl` reaches the subresource; the table row now says so |
| [[036-k8s-driver]] | `Exec` refuses `Stdin` and `TTY` with `ErrUnsupported`; the Role grants `create` on `pods/exec` | both are accepted; the Role grants `get` as well | `Attach` is declared, and the WebSocket's `GET` is `get` |

### What the conformance run did

`TestClusterConformance` did not run: `kind` is not installed on this
machine and no cluster answers. It is also wired into no workflow, so the
driver-level attach cases have not yet run against a cluster anywhere.
What does run in CI is the API suite against `cellad` on the kind stack,
now with `attach` declared: `case008ExecSocket` and
`case008AttachSocket` open real sessions through the Role the deploy tree
ships, and `case004CapabilityGates` holds both sockets to answering. On a
machine with a cluster, the driver-level run is:

```
kind create cluster --name cella
CELLA_TEST_KUBECONFIG=$HOME/.kube/config go test ./runtime/k8s -run TestClusterConformance -v
```

### Left open

| Open | Why |
|---|---|
| `TestClusterConformance` in a workflow | it runs every driver case against the cluster, not the terminal alone, and has never run; wiring it is a job of its own with its own budget |
| `Dial` on this driver, through port forwarding | a slice of its own |
| A Role for a worker that drives Kubernetes in the workers guide | the guide's example shows the worker's ServiceAccount and no Role; the worker needs the control plane's Role, `get` on `pods/exec` included |
