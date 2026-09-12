---
title: "Runtime contract: the Driver interface, isolation classes, capabilities, the six drivers, conformance"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
affects: [runtime/, runtime/k8s/, runtime/podman/, runtime/native/, runtime/local/, runtime/vm/, runtime/remote/, runtime/runtimetest/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Runtime contract

## Overview

A driver turns a resolved manifest into a running environment on one
substrate and answers questions about it. The substrate may be a
container on a cluster, a container on a machine, a virtual machine, a
confined process on the operator's own laptop, or a worker somewhere
the control plane cannot reach. The contract is one interface,
`runtime.Driver`, one declaration of what a driver can enforce,
`Capabilities`, and one conformance suite, `runtimetest`, so the
controller and the API never branch on a driver's name, and a new
isolation technology is a package that passes the suite.

## Current state

Not built. The interface descends from one that three container
drivers have implemented in the hosted platform; the changes are that
optional behaviour is declared rather than discovered by type
assertion, that the k8s driver takes decorators instead of importing
the platform's clients, that volumes and ports and display are first
class, and that the isolation class is part of the declaration. A
sibling project has a built and tested OS-level host sandbox driver on
the same seam shape, which the `local` driver here descends from.

## Design

### Isolation classes

| Class | Boundary | Drivers | Start | Suits |
|---|---|---|---|---|
| `container` | kernel namespaces and cgroups, an OCI image | `k8s`, `podman` | seconds, or milliseconds from a pool | the server-side default: agents, applications, rollouts |
| `vm` | a hardware virtual machine per sandbox, an OCI image or a root file system | `k8s` with a runtime class (Kata, Firecracker through a class), `vm` (a direct microVM driver) | seconds | code from an untrusted source, kernel-level isolation, an operator's compliance floor |
| `process` | an OS sandbox around a host process: Seatbelt on macOS, bubblewrap with seccomp on Linux, an allow-only proxy for the network | `local` | milliseconds | a developer's own machine, a harness already installed and logged in, no engine |
| `none` | a directory and a process, no boundary | `native` | milliseconds | the suite and `make run`; its documentation says so in the first sentence |

A manifest asks for a class through its environment: an `Environment`
declares the class its driver provides ([[021-data-plane-workers]]), a
platform's admission step may route a manifest to an environment by
class, and `status.driver` and `status.isolation` say what a sandbox
got. A caller that needs a floor names an environment that provides it;
the control plane never silently downgrades.

### The interface

```go
// Driver is what every substrate implements. A method a driver cannot
// serve is not on this interface; it is a capability below.
type Driver interface {
	Name() string
	Isolation() Isolation
	Capabilities() Capabilities
	Preflight(ctx context.Context) error // *NotReadyError with remediation when the host cannot run it
	Ready(ctx context.Context) error

	Create(ctx context.Context, spec CreateSpec) (Ref, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	Update(ctx context.Context, id string, change Change) error

	Inspect(ctx context.Context, id string) (State, error)
	List(ctx context.Context, filter Filter) ([]State, error)
	Watch(ctx context.Context) (<-chan Event, error)

	Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error)
	Attach(ctx context.Context, id string, req AttachRequest) (Session, error)
	ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error
	ImportTar(ctx context.Context, id string, dest string, src io.Reader) error
	Logs(ctx context.Context, id string, req LogsRequest) (io.ReadCloser, error)
	Dial(ctx context.Context, id string, port int) (net.Conn, error) // a connection to a port inside
}
```

`CreateSpec` is what [[005-lifecycle-controller]] derives from a
resolved manifest: image, command, user, workdir, resources, the
workspace (size, tier, source), the volumes to attach with their modes,
env with the placeholders already in it, the egress rule and the
gateway address, the ports, the mesh membership, the labels the driver
must stamp, the workload token and the CA to project under
`/run/cella/`, and the display request. `State` is the driver's truth:
id, phase, the stamped labels, timestamps, the resource shape granted,
volume attachment, and conditions. `Change` carries the mutable fields
of [[003-manifest-contract]].

### Capabilities

```go
type Capabilities struct {
	Egress   bool // the egress rule is enforced, not advisory
	Mesh     bool // peers in one mesh reach each other's mesh ports and nothing else does
	Ingress  bool // a public port gets an endpoint
	Volumes  bool // Volume objects attach and detach
	Display  bool // a GUI desktop can be attached (023)
	Input    bool // keyboard and pointer events are accepted (023)
	Resize   bool // resources change on a running sandbox
	Persist  bool // tier persistent survives Stop
	Pool     bool // Create accepts a Prewarm and Adopt pair (020)
	Files    bool // ExportTar and ImportTar work while Stopped
	Detach   bool // a handle survives the process that made it (local, native)
}
```

| Capability | k8s | podman | vm | local | native | remote |
|---|---|---|---|---|---|---|
| Egress | yes: NetworkPolicy to the gateway, DNS, `cellad` only | yes: per-sandbox network, gateway the only route | yes: the VM's single NIC routes to the gateway | yes: the OS sandbox's allow-only proxy | no; warning | the worker's driver's |
| Mesh | yes: a policy selecting peers by mesh label | yes: one network per mesh | yes, as k8s | no | no | the worker's |
| Ingress | yes, through an Ingress or Gateway API decorator | no | as k8s | no | no | the worker's |
| Volumes | yes: PVCs | yes: named volumes | yes: block devices or virtiofs | yes: host directories under the data dir, bind-mounted read-only or read-write | yes: directories | the worker's |
| Display | yes: an Xvfb sidecar | yes: in-container | yes | no | no | the worker's |
| Input | as Display | as Display | as Display | no | no | the worker's |
| Resize | CPU and memory in place where the cluster allows | CPU and memory | memory balloon | no | no | the worker's |
| Persist | yes | yes | yes | yes | yes | the worker's |
| Pool | yes | no | yes | no | no | the worker's |
| Files | yes, through a helper Pod | yes, through the volume | yes | yes | yes | the worker's |
| Detach | not applicable | not applicable | not applicable | yes | yes | not applicable |

Every driver enforces the workload token projection, the labels, the
env, the workdir, and the lifecycle timestamps, so these are not
capabilities.

### Labels are the observed truth

Each driver stamps the sandbox's identity into the substrate under the
`cella.latere.ai/` prefix: `id`, `name`, `owner`, `tier`, `mesh`,
`parent`, `created-at`, `last-activity-at`, `expires-at`, `stopped-at`,
and the user's labels under `label/<key>`. `List` and `Inspect` read
these back, and nothing else, so a control plane that starts against a
substrate holding sandboxes lists them before the store is consulted
([[010-state]]). k8s uses Pod and PVC labels and annotations; podman
uses container and volume labels; `vm` uses the hypervisor's metadata;
`local` and `native` write a JSON record beside the sandbox directory.

### Phases

Every driver reports one of `Pending`, `Starting`, `Running`,
`Stopping`, `Stopped`, `Deleting`, `Failed`, `Lost`, derived from the
substrate. The mapping per driver is a table in its package
documentation and a case in the conformance suite. `Queued` and
`Recovering` are the controller's, never a driver's.

### The k8s driver

One namespace, one PVC and one Pod per sandbox, the Pod owned by
nothing so that a Stop deletes the Pod and keeps the PVC. Exec and
Attach go through the SPDY exec subresource; `Dial` through port
forwarding. The egress rule is a NetworkPolicy selecting the Pod; the
mesh rule a second policy selecting peers by the mesh label; the
workload token and CA are a projected Secret. `runtimeClassName` is
how the class becomes `vm`: the driver reports the class its
configured `CELLA_K8S_RUNTIME_CLASS` provides, `container` when unset.
Two hooks let a platform decorate without a fork:

```go
type Decorator interface {
	Pod(ctx context.Context, spec CreateSpec, pod *corev1.Pod) error
	Claim(ctx context.Context, spec CreateSpec, pvc *corev1.PersistentVolumeClaim) error
}
```

A decorator adds a sidecar, a volume, an annotation, a scheduling
constraint, an ingress. It may not remove the token, the labels, the
network policy, or the security context; the driver re-checks those
after the decorators run and refuses the create with
`decorator_violation` if one is gone. The driver uses upstream
`client-go` against `CELLA_KUBECONFIG` or the in-cluster configuration
and assumes nothing about the cloud.

### The podman driver

Drives the libpod REST API over `CELLA_PODMAN_SOCKET`. One named volume
and one container per sandbox, rootless when the socket is a user's.
Exec and Attach over hijacked connections. Network is a per-sandbox
network whose only route is the gateway, and one network per mesh.

### The vm driver

Room, not code, in the first release: the package exists with the
interface stubbed and `Preflight` reporting not ready, and its spec is
the microVM spec that follows this deck. What is fixed now: a `vm`
driver takes an OCI image or a root file system, boots a microVM per
sandbox with one NIC routed to the gateway, exposes exec and attach
through a guest agent over vsock, and attaches volumes as block devices
or virtiofs. Its conformance run is the same suite.

### The local driver

The operator's own machine, the harness they already have installed
and logged in, and an OS sandbox around the process: Seatbelt on macOS,
bubblewrap with a seccomp filter on Linux, an allow-only network
through a host proxy. It descends from a built and tested driver in a
sibling project, whose seam is this one: a stage spec of argv, env,
readable and writable paths, denied paths, and a network policy,
rendered into the sandbox runtime's settings; a table of paths always
denied (`~/.ssh`, `~/.aws`, `~/.kube`, `~/.gnupg`, `~/.netrc`,
`~/.git-credentials`, `~/.docker/config.json`, `~/.npmrc`,
`~/.pypirc`, every `.env`), checked by a test per entry; a detached
process handle of pid plus start time that outlives the process that
made it; a typed not-ready error with remediation per platform. Two
facts that driver learned the hard way are inherited: the sandbox
runtime is allow-only and refuses a wildcard, so `mode: open` is not a
capability here and resolve says so; and TLS termination must be
declared for the injected trust store to be read, with package indexes
tunnelled because pip ignores every certificate variable.

The parts that are not Cella's, the settings renderer, the deny table,
the preflight, the detached handle, are a package with two consumers
and belong in `latere.ai/x/pkg` once extracted; `runtime/local` here
adds the `Driver` mapping, volumes as bind mounts under the data dir,
and the gateway as the proxy the sandbox runtime allows. The extraction
is a decision for the sibling project's owner and is recorded in the
index's decisions table as open.

### The native driver

A directory under `CELLA_DATA_DIR/sandboxes/<id>` and a process started
from the image's command, where the image is a local root file system
or a plain command name. No boundary is enforced. It exists for `make
run`, for the suite, and for a laptop with nothing installed, and its
package documentation says so in the first sentence.

### The remote driver

`runtime/remote` implements `Driver` for an environment a worker
serves: every method becomes an operation on the environment's queue,
claimed by the worker, executed with the worker's own driver, and
answered; streams are relayed over the worker's outbound connection.
Its capabilities and isolation are the worker's, reported at
registration. [[021-data-plane-workers]] owns the queue and the
worker.

### Conformance

`runtimetest.Run(t, func() runtime.Driver)` runs the contract against a
driver: create, list, inspect, exec with stdin and exit codes, attach
with resize, tar out and in, logs, dial, stop and start with the
workspace kept on a persistent tier and gone on an ephemeral one,
volumes attached and detached, update of every mutable field, delete,
the labels read back as written, the phase mapping, and each
capability the driver declares. A declared capability the suite finds
not to hold fails; an undeclared one the suite skips. `native` runs it
in the unit suite; `local` runs it where the sandbox runtime is
installed; `podman`, `k8s`, and `remote` run it in the tiers of
[[012-test-stubs-and-tiers]].

### Selection

`CELLA_RUNTIME` picks the in-process driver of the default environment.
A driver's own variables (`CELLA_KUBECONFIG`, `CELLA_NAMESPACE`,
`CELLA_K8S_RUNTIME_CLASS`, `CELLA_PODMAN_SOCKET`, `CELLA_LOCAL_SRT`)
are read only when that driver is selected. Further environments are
registered objects.

## Not in this spec

The reconciliation that calls these methods ([[005-lifecycle-controller]]);
the worker and the queue ([[021-data-plane-workers]]); the gateway
([[018-egress-and-secrets]]); the display and input operations
([[023-computer-use-operations]]); the microVM driver's own spec.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `native` passes the whole conformance suite in the unit suite | `TestNativeConformance` | not built |
| `local` passes it on a machine with the sandbox runtime installed, and is skipped with the remediation printed where it is not | `TestLocalConformance` | not built |
| `podman` passes it in the podman tier; `k8s` against kind; `remote` through a worker running `native` | `TestPodmanConformance`, `TestK8sConformance`, `TestRemoteConformance` | not built |
| A driver that declares a capability it does not honour fails the suite | `TestConformanceCatchesAFalseCapability` | not built |
| After the substrate holds three sandboxes, a fresh driver lists three with their labels intact | conformance case `ListReadsLabelsBack` | not built |
| A decorator that removes the token mount, the network policy, or the security context is refused with `decorator_violation` | `TestDecoratorCannotWeakenTheBoundary` | not built |
| On k8s with a runtime class configured, `Isolation()` reports `vm` and the Pod carries the class | `TestK8sRuntimeClass` | not built |
| In `local`, a stage cannot read any path in the always-deny table, asserted per entry inside a real sandbox | `TestLocalDenyTable` | not built |
| `Exec` streams a 64 MiB output without buffering and returns the exit code; `Dial` reaches a port inside | conformance cases `ExecStreams`, `DialReachesAPort` | not built |
| `vm` exists, declares its class, and reports not ready with a message naming the spec that builds it | `TestVMIsAStub` | not built |
