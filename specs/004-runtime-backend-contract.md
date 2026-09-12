---
title: "Runtime backend contract: the Runtime interface, capabilities, k8s, podman, native, conformance"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
affects: [runtime/, runtime/k8s/, runtime/podman/, runtime/native/, runtime/runtimetest/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Runtime backend contract

## Overview

A backend turns a resolved manifest into a running environment on one
substrate and answers questions about it. Three ship: `k8s`, a Pod and
a PersistentVolumeClaim per sandbox on any Kubernetes; `podman`, a
container and a named volume on a machine with Podman; `native`, a
directory and a confined host process, for development and for the
suite. All three implement one interface, `runtime.Runtime`, declare
what they can enforce through `Capabilities`, and pass one conformance
suite, `runtimetest`, so the controller and the API never branch on the
backend's name.

## Current state

Not built. The interface descends from one that three backends have
implemented in the hosted plane this core comes from; the changes are
that the optional capabilities are declared rather than discovered by
type assertion, that the k8s backend takes decorators instead of
importing the plane's clients, and that the workspace is a volume the
sandbox clones into rather than a mount of a remote file system.

## Design

### The interface

```go
// Runtime is what every backend implements. A method that a backend
// cannot serve is not on this interface; it is a capability below.
type Runtime interface {
	Name() string
	Capabilities() Capabilities
	Ready(ctx context.Context) error

	Create(ctx context.Context, spec CreateSpec) (Ref, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error

	Inspect(ctx context.Context, id string) (State, error)
	List(ctx context.Context, filter Filter) ([]State, error)
	Watch(ctx context.Context) (<-chan Event, error)

	Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error)
	Attach(ctx context.Context, id string, req AttachRequest) (Session, error)
	ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error
	ImportTar(ctx context.Context, id string, dest string, src io.Reader) error

	Update(ctx context.Context, id string, change Change) error
}
```

`CreateSpec` is what [[005-lifecycle-controller]] derives from a
resolved manifest: the image, command, user, workdir, resources, the
workspace volume (size, tier, source), env, the network rule, the
labels the backend must stamp, the workload token to project at
`/run/cella/token`, and the display request. `State` is the backend's
truth: id, phase, the stamped labels, timestamps, the resource shape
actually granted, and the condition list. `Change` carries the mutable
fields of [[003-manifest-contract]]: labels and annotations, lifecycle
values, network rule, tier, resources.

### Capabilities

```go
type Capabilities struct {
	Network  bool // network.mode and allowedHosts are enforced, not advisory
	Display  bool // a GUI desktop can be attached
	Resize   bool // resources change on a running sandbox
	Persist  bool // tier persistent survives Stop
	WarmPool bool // Create accepts a Prewarm and Adopt pair (005)
	Files    bool // ExportTar and ImportTar work while the sandbox is Stopped
}
```

| Capability | k8s | podman | native |
|---|---|---|---|
| Network | yes, NetworkPolicy plus a DNS-scoped egress rule | yes, a per-container network with an nftables allow list | no; warning |
| Display | yes, a Xvfb sidecar | yes, in-container Xvfb | no; refusal |
| Resize | CPU and memory, in place where the cluster allows | CPU and memory | no |
| Persist | yes, the PVC outlives the Pod | yes, the volume outlives the container | yes, the directory |
| WarmPool | yes | no | no |
| Files | yes, through a short-lived helper Pod | yes, through the volume | yes |

Every backend enforces the workload token projection, the labels, the
env, the workdir, and the lifecycle timestamps, so these are not
capabilities.

### Labels are truth

Each backend stamps the sandbox's identity into the substrate under
the `cella/` prefix: `cella/id`, `cella/name`, `cella/owner`,
`cella/tier`, `cella/created-at`, `cella/last-activity-at`,
`cella/expires-at`, `cella/stopped-at`, and the user's labels under
`cella/label/<key>`. `List` and `Inspect` read these back, and nothing
else, so a `cellad` that starts against a substrate holding sandboxes
lists them before any store is consulted ([[010-state]]). The native
backend stamps a JSON record in the sandbox's directory; podman uses
container labels; k8s uses Pod and PVC labels and annotations.

### Phases

Every backend reports one of `Pending`, `Starting`, `Running`,
`Stopping`, `Stopped`, `Deleting`, `Failed`, `Lost`, derived from the
substrate: a Pod that is scheduled and not ready is `Starting`; a PVC
with no Pod is `Stopped`; a record the substrate has lost the object
for is `Lost`. The mapping per backend is a table in its package
documentation and a case in the conformance suite.

### The k8s backend

One namespace (`CELLA_NAMESPACE`), one PVC and one Pod per sandbox,
the Pod owned by nothing so that a Stop deletes the Pod and keeps the
PVC. Exec and Attach go through the SPDY exec subresource. Network is a
NetworkPolicy selecting the Pod, with an egress rule per allowed host
resolved at apply and refreshed by the controller. The workload token
is a projected Secret. Two hooks let a plane decorate without a fork:

```go
type Decorator interface {
	Pod(ctx context.Context, spec CreateSpec, pod *corev1.Pod) error
	Claim(ctx context.Context, spec CreateSpec, pvc *corev1.PersistentVolumeClaim) error
}
```

A decorator adds a sidecar, a volume, an annotation, a scheduling
constraint. It may not remove the workload token, the labels, or the
network policy; the backend re-checks those after the decorators run
and refuses the create with `decorator_violation` if one is gone. The
backend uses upstream `client-go` against `CELLA_KUBECONFIG` or the
in-cluster configuration and assumes nothing about the cloud; storage
class, node selector, and runtime class are decorator or configuration
concerns.

### The podman backend

Drives the libpod REST API over `CELLA_PODMAN_SOCKET`. One named volume
and one container per sandbox, rootless when the socket is a user's.
Exec and Attach use the API's exec and attach endpoints over a hijacked
connection. Network is a per-sandbox network with an allow list applied
through the engine's firewall driver; when the driver cannot express a
host rule the backend reports `Network: false` and the resolved manifest
carries the warning.

### The native backend

A directory under `CELLA_DATA_DIR/sandboxes/<id>` and a process started
from the image's command, where the image is a local root file system
or a plain command name, since there is no engine to pull one. The
process is confined to the directory and the environment; no network
rule is enforced. It exists for `make run`, for the suite, and for a
laptop with nothing installed. Its package documentation says so in the
first sentence.

### Conformance

`runtimetest.Run(t, func() runtime.Runtime)` runs the contract against
a backend: create, list, inspect, exec with stdin and exit codes, attach
with resize, tar out and in, stop and start with the workspace kept on
a persistent tier and gone on an ephemeral one, update of every
mutable field, delete, the labels read back as written, the phase
mapping, and each capability the backend declares. A declared
capability the suite finds not to hold fails; an undeclared one the
suite skips. The native backend runs it in the unit suite; podman and
k8s run it in the tiers of [[012-test-stubs-and-tiers]].

### Selection

`CELLA_RUNTIME` picks the backend at start. The k8s backend is the
default because it is the production one. A backend's own variables
(`CELLA_KUBECONFIG`, `CELLA_NAMESPACE`, `CELLA_PODMAN_SOCKET`) are read
only when that backend is selected.

## Not in this spec

The reconciliation that calls these methods, the reaper, and the warm
pool ([[005-lifecycle-controller]]); the HTTP shape of exec and attach
([[008-api]]); the token the backend projects ([[006-identity]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The native backend passes the whole conformance suite in the unit suite | `TestNativeConformance` | not built |
| The podman backend passes it in the podman tier | `TestPodmanConformance` under the `podman` build tag | not built |
| The k8s backend passes it against a kind cluster | `TestK8sConformance` under the `e2e` build tag | not built |
| A backend that declares a capability it does not honour fails the suite | `TestConformanceCatchesAFalseCapability` with a wrapper that lies | not built |
| After the substrate holds three sandboxes, a fresh backend lists three with their labels intact | conformance case `ListReadsLabelsBack` | not built |
| A decorator that removes the workload token mount is refused with `decorator_violation` | `TestDecoratorCannotRemoveTheToken` | not built |
| A sandbox with `allowedHosts: [example.com]` on k8s cannot reach another host | e2e case `NetworkAllowlistIsEnforced` | not built |
| `Exec` returns the process exit code and streams stdin and stdout without buffering the whole output | conformance case `ExecStreams` with a 64 MiB payload | not built |
