---
title: "Runtime contract: the Driver interface, optional interfaces, isolation classes, capabilities, the six drivers, conformance"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
affects: [runtime/, runtime/k8s/, runtime/podman/, runtime/native/, runtime/local/, runtime/vm/, runtime/remote/, runtime/runtimetest/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-23
author: changkun
---

# Runtime contract

## Overview

A driver turns a resolved manifest into a running environment on one
substrate and answers questions about it. The substrate may be a
container on a cluster, a container on a machine, a virtual machine, a
confined process on the operator's own laptop, or a worker somewhere
the control plane cannot reach. The contract is one interface every
driver implements, `runtime.Driver`, a set of optional interfaces a
driver implements when it declares the matching capability, one
declaration of what it can enforce, `Capabilities`, and one conformance
suite, `runtimetest`, so the controller and the API never branch on a
driver's name and a new isolation technology is a package that passes
the suite.

## Current state

The initial native implementation is in `runtime/native` ([[025-native-runtime-migration]]), and the container driver of the server-side default is in `runtime/k8s` ([[036-k8s-driver]]) with the lifecycle, execution, logs, archive transfer and stamped identity of this contract. `DisplayDriver` and `InputDriver` are implemented by `runtime/podman` and `runtime/k8s`, with `State.Ports` and the `DisplayReady` condition ([[041-display-and-input]]); `Dialer` is implemented by `runtime/native`, on the host's loopback, and by `runtime/podman`, through the engine's publication of each declared port on loopback, with the dial socket and the port proxy of [[008-api]] behind it ([[060-dial-and-port-proxy]]); `runtime/remote` relays no dial yet. The exported package and capability types follow this design; the other drivers and remaining native capabilities below are not complete.

Design provenance: The interface descends from one that three container
drivers have implemented in the hosted platform; the changes are that
optional behaviour is declared and typed rather than discovered by
assertion alone, that the k8s driver takes decorators instead of
importing the platform's clients, that volumes, ports, display, and
snapshots are first class, and that the isolation class is part of the
declaration. The `local` driver builds on `latere.ai/x/pkg/hostsandbox`
v0.60.1, a package with one other consumer, and this spec claims of it
only what that version does.

## Design

### Isolation classes

| Class | Boundary | Drivers | Start | Suits |
|---|---|---|---|---|
| `container` | kernel namespaces and cgroups, an OCI image | `k8s`, `podman` | seconds, or milliseconds from a pool | the server-side default: agents, applications, rollouts |
| `vm` | a hardware virtual machine per sandbox | `k8s` with a runtime class the operator declares as `vm`, `vm` ([[024-vm-driver]]) | seconds | code from an untrusted source, a compliance floor |
| `process` | an OS sandbox around a host process: Seatbelt on macOS, bubblewrap with seccomp on Linux, an allow-only proxy for the network | `local` | milliseconds | a developer's own machine, a harness already installed and logged in, no engine |
| `none` | a directory and a process, no boundary | `native` | milliseconds | the suite and `make run`; its documentation says so in the first sentence |
| the worker's | whatever the worker's driver declares | `remote` | the worker's | a self-hosted environment ([[021-data-plane-workers]]) |

A manifest asks for a class through its environment: an `Environment`
declares the class its driver provides, a worker whose driver reports
another class is refused at registration ([[021-data-plane-workers]]),
and `status.isolation` says what a sandbox got. The control plane never
infers a class and never downgrades one.

### The interface

```go
// Driver is what every substrate implements.
type Driver interface {
	Name() string
	Isolation() v1.Isolation           // container | vm | process | none
	Capabilities() v1.Capabilities
	Preflight(ctx context.Context) error // *NotReadyError when the host cannot run it
	Ready(ctx context.Context) error

	Create(ctx context.Context, spec CreateSpec) (Ref, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	Update(ctx context.Context, id string, change Change) error

	Inspect(ctx context.Context, id string) (State, error)
	List(ctx context.Context, filter Filter) ([]State, error)
	Watch(ctx context.Context) (<-chan Event, error)

	Exec(ctx context.Context, id string, req ExecRequest) (Exec, error)
	Logs(ctx context.Context, id string, req LogsRequest) (io.ReadCloser, error)
	ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error
	ImportTar(ctx context.Context, id string, dest string, src io.Reader) error
	Touch(ctx context.Context, id string) error // stamps last-activity-at; coalesced by the caller
}
```

The optional interfaces, each implemented if and only if the matching
capability is declared; the suite fails a driver that declares one
without the other:

| Capability | Interface | Methods |
|---|---|---|
| `Attach` | `Attacher` | `Attach(ctx, id, AttachRequest) (Session, error)`: a PTY, bytes both ways, resize |
| `Dial` | `Dialer` | `Dial(ctx, id, port int) (net.Conn, error)`: a connection to a port inside |
| `Volumes` | `VolumeDriver` | `CreateVolume(ctx, VolumeSpec) (Ref, error)` (fills from the source; `Pending` until done), `DeleteVolume(ctx, id)`, `ResizeVolume(ctx, id, size)`, `InspectVolume(ctx, id) (VolumeState, error)`, `ListVolumes(ctx) ([]VolumeState, error)`, `Write(ctx, id, dest string, src io.Reader) error` (the control plane's own path in, [[019-volumes]]) |
| `Snapshots` | `Snapshotter` | `Snapshot(ctx, volumeID) (snapshotID string, err error)` (crash-consistent), `DeleteSnapshot(ctx, snapshotID)`, `ListSnapshots(ctx, volumeID) ([]string, error)` |
| `Display` | `DisplayDriver` | `Display(ctx, id) (Geometry, error)`, `Screenshot(ctx, id, ScreenshotRequest) (io.ReadCloser, error)`, `Screen(ctx, id, fps int, format string) (<-chan Frame, error)` |
| `Input` | `InputDriver` | `Input(ctx, id, []InputEvent) error` |
| `Files` | `FileStore` | `Stat(ctx, id, path) (FileInfo, error)`, `ReadDir(ctx, id, path) ([]FileInfo, error)` (sorted by name), `Open(ctx, id, path) (io.ReadCloser, FileInfo, error)`, `Write(ctx, id, WriteRequest) (int64, error)` (a body, a mode and a bound; staged and renamed, so a body that ends early or passes the bound leaves the previous file whole), `Mkdir`, `Remove`, `Move(ctx, id, from, to)` (the exact destination; onto a directory is refused) ([[033-file-operations]]) |
| `Pool` | none | `CreateSpec.Prewarm` and adoption through `Update` are accepted |
| `Egress`, `Mesh`, `Ingress`, `Resize`, `Detach` | none | declarations about what the core methods enforce |

### Types

Every type is in `runtime`; the enumerations and `Capabilities` are in
`manifest/v1`, which `runtime` aliases.

| Type | Fields |
|---|---|
| `Ref` | `ID string`, the `sbx_` id every other call takes |
| `CreateSpec` | `ID`, `Name`, `Owner`, `Image`, `Command`, `Args`, `Workdir`, `User`, `Resources{CPU, Memory, Disk}`, `Workspace{Path, Source, Git{URL, Ref, TokenEnv}, VolumeID}`, `Volumes []Mount{Name, VolumeID, Path, ReadOnly}`, `Env map[string]string` (placeholders already in it), `Egress{Mode, AllowedHosts, DeniedHosts, GatewayURL, CAPEM, Credential}`, `Ports []Port{Name, Port, Expose}`, `Mesh{ID, Enabled}`, `Parent`, `Labels map[string]string` (the user's), `Token []byte` (the workload token to project), `Lifecycle{AutoStop, TTL, AutoDelete}`, `Display *Geometry`, `Prewarm bool` |
| `Change` | pointers, nil meaning unchanged: `Labels`, `Annotations`, `Lifecycle`, `Egress{Mode, AllowedHosts, DeniedHosts}`, `Resources`, `Volumes []Mount` (the full desired list; the driver attaches and detaches the difference, while `Stopped`), `Env`, `Token []byte` (a re-projection before expiry), `Adopt *Adoption{Owner, Name, Labels, Env, Workspace, Lifecycle, Volumes, Token, Egress}` (turns a prewarmed entry into the caller's sandbox in one call, exclusively; the driver performs a git workspace clone here) |
| `State` | `ID`, `Name`, `Owner`, `Phase`, `Isolation`, `Labels` (the user's), `MeshID`, `Parent`, `Pool bool`, `Resources` (granted), `Volumes []VolumeAttachment{Name, VolumeID, Attached bool}`, `Ports []PortState{Name, Port, State}` (`listening` or `closed`, probed by the driver at inspect), `Conditions []Condition` (the driver's: `Ready`, `WorkspaceReady`, `EgressEnforced`, `VolumesAttached`, `DisplayReady`), `CreatedAt`, `StartedAt`, `StoppedAt`, `LastActivityAt`, `ExpiresAt`, `AutoStop`, `AutoDelete` |
| `Filter` | `Owner`, `Phase`, `MeshID`, `Parent`, `Pool *bool`, `IDs []string`; selects on the label half of the stamped identity only, so it is what a substrate can answer without reading every object |
| `Event` | `Type` (`added`, `modified`, `deleted`, `lost`, `relist`), `State` (the observed state after the change; empty for `relist`, which tells the consumer to `List`) |
| `ExecRequest` | `Command []string`, `Env`, `Workdir`, `Stdin io.Reader` (needs `Attach`; nil otherwise), `TTY bool` (needs `Attach`), `Timeout` |
| `Exec` | `Stdout`, `Stderr io.Reader`, `Wait(ctx) (exitCode int, err error)`, `Close() error` |
| `AttachRequest` | `Command []string` (the image's shell when empty), `Env`, `Workdir`, `Cols`, `Rows` |
| `Session` | `io.ReadWriteCloser`, `Resize(cols, rows int) error`, `Wait(ctx) (exitCode int, err error)` |
| `LogsRequest` | `Follow bool`, `Since time.Time`, `TailLines int` |
| `VolumeSpec` | `ID`, `Name`, `Owner`, `Size`, `Class`, `Access` (`single`, `shared-read`), `Source{Kind, SnapshotID, Image, ArchiveURL, AllowedHosts, MaxBytes, SHA256}` (the control plane validated the URL; the driver re-applies the hosts to redirects and the caps to the fetch) |
| `VolumeState` | `ID`, `Phase` (`Pending`, `Available`, `Failed`, `Lost`), `Reason`, `Capacity`, `AttachedTo []string`, `Access` |
| `Geometry`, `ScreenshotRequest`, `Frame`, `InputEvent` | declared in `runtime/display` as [[023-computer-use-operations]] defines them: `{Width, Height}`; `{Format, Scale}`; `{At, Format, Data []byte}`; `{Type, X, Y, ToX, ToY, Button, Modifiers, Key, Text, Direction, Amount, Ms}` |
| `FileInfo` | `Name` (the base name as the filesystem holds it), `Path` (absolute, inside the workspace), `Size`, `Mode fs.FileMode`, `ModTime`, `IsDir` ([[033-file-operations]]) |
| `WriteRequest` | `Path`, `Mode fs.FileMode` (zero is `0644`), `MaxBytes` (zero is no bound), `Body io.Reader` |
| `NotReadyError` | `Driver`, `Condition`, `Components []string`, `Remediation`, `Alternative`; the `local` driver converts `hostsandbox.NotReadyError` into it so `runtime` imports no other package |

### Capabilities

```go
// In manifest/v1; runtime.Capabilities = v1.Capabilities.
type Capabilities struct {
	Egress     []EgressMode `json:"egress"` // the modes enforced: none, allowlist, open; empty means the rule is advisory
	Mesh       bool `json:"mesh"`       // peers in one mesh reach each other's mesh ports and nothing else does
	Ingress    bool `json:"ingress"`    // a public port gets an endpoint
	Volumes    bool `json:"volumes"`    // Volume objects are created, attached, detached, deleted
	Snapshots  bool `json:"snapshots"`  // a volume can be snapshotted and a snapshot used as a source
	Attach     bool `json:"attach"`     // a PTY session into a running sandbox; Exec with stdin or tty
	Dial       bool `json:"dial"`       // a connection to a port inside
	Display    bool `json:"display"`    // a GUI desktop can be attached (023)
	Input      bool `json:"input"`      // keyboard and pointer events are accepted (023)
	Resize     bool `json:"resize"`     // resources change on a running sandbox
	Pool       bool `json:"pool"`       // Prewarm and Adopt are accepted (020)
	Files      bool `json:"files"`      // the archives work while Stopped, and FileStore is implemented
	Detach     bool `json:"detach"`     // a driver built in another process recovers a sandbox from its record alone
}
```

| Capability | k8s | podman | vm | local | native | remote |
|---|---|---|---|---|---|---|
| Egress | `none, allowlist, open`: NetworkPolicy to the gateway, DNS, `cellad` only | `none, allowlist, open`: per-sandbox network, gateway the only route | `none, allowlist, open`: one NIC routed to the gateway | `none, allowlist`: the OS sandbox's allow-only proxy refuses a wildcard, so `open` is not listed | empty; `EgressEnforced` false | the worker's |
| Mesh | yes: a policy selecting peers by mesh label, a headless Service per mesh with each Pod's `hostname` and `subdomain` set so `<sandbox-name>.mesh` resolves through the installation's DNS rewrite | yes: one network per mesh with `<name>.mesh` as the container's alias | as k8s | no | no | the worker's |
| Ingress | only when an `Exposer` decorator is installed; `false` otherwise | no | as k8s | no | no | the worker's |
| Volumes | yes: PVCs | yes: named volumes | yes: block devices or virtiofs | yes: directories under the data dir, granted as readable or writable paths | yes: directories | the worker's |
| Snapshots | where the cluster has the VolumeSnapshot API | by copy | yes | by copy | by copy | the worker's |
| Attach | yes: SPDY exec with a TTY | yes: hijacked attach, with the engine keeping the session when the connection drops | yes: the guest agent | no: the sandbox runtime launches detached stages and has no PTY or stdin seam | yes: a pseudo-terminal from the host's own ioctls on Linux and macOS, and the process group killed on close ([[034-terminal-attach]]) | the worker's |
| Dial | yes: port forwarding | yes | yes | no | yes: loopback | the worker's |
| Display | yes: the `cella-display` sidecar sharing `/tmp` with the workload | yes: in-container | yes: the guest agent | no | no | the worker's |
| Input | as Display | as Display | as Display | no | no | the worker's |
| Resize | CPU and memory in place where the cluster allows | CPU and memory | memory balloon | no | no | the worker's |
| Pool | yes | no | yes | no | no | the worker's |
| Files | yes, through a helper Pod | yes, through the volume | yes | yes | yes | the worker's |
| Detach | no | yes: the container, the workspace volume and the record volume are the whole of a sandbox, so a second driver over the engine reads it back ([[035-podman-driver]]) | no | yes: the handle is a pid, a start time, and a log path | yes: a pid and a record | no |

Every driver enforces the workload token and CA projection, the
labels, the env, the workdir, and the lifecycle timestamps, so these
are not capabilities. `Exec` without stdin or a TTY is on every driver:
on `local` it is a second confined stage sharing the sandbox's paths,
not an entry into the first, and the `Exec` returned streams its log.

### Stamped identity: labels and annotations

Each driver stamps the sandbox's identity into the substrate under the
`cella.latere.ai/` prefix so that `List` and `Inspect` read it back and
nothing else, and a control plane that starts against a substrate
holding sandboxes lists them before the store is consulted
([[010-state]]). The split follows what a Kubernetes label may hold (at
most 63 characters of letters, digits, `-`, `_`, `.`) and what it may
not:

| As labels (selectable) | As annotations (read back, not selectable) |
|---|---|
| `id`, `name`, `mesh`, `parent`, `pool` | `owner`, `created-at`, `started-at`, `stopped-at`, `last-activity-at`, `expires-at`, `auto-stop`, `auto-delete`, and the user's labels as `label.<key>` |

The reaper of [[005-lifecycle-controller]] reads every instant and
duration it needs from this record, so it runs from the substrate
alone after a restart without a durable store. k8s uses Pod and PVC
labels and annotations; podman uses container and volume labels for
both halves, since its labels are unconstrained; `vm` uses the
hypervisor's metadata; `local` and `native` write a JSON record beside
the sandbox directory with the same two halves. `Filter` selects on
the label half only.

### Phases

Every driver reports one of `Pending`, `Starting`, `Running`,
`Stopping`, `Stopped`, `Deleting`, `Failed`, `Lost`, derived from the
substrate. The mapping per driver is a table in its package
documentation and a case in the conformance suite. `Queued` and
`Recovering` are the controller's, never a driver's. `Scheduled` is
the controller's condition; the five conditions in `State` are the
driver's.

### Projections

Every driver projects into the sandbox, read-only: the workload token
at `/run/cella/token` and the gateway's CA at
`/run/cella/egress-ca.pem` ([[006-identity]],
[[018-egress-and-secrets]]), and sets `CELLA_URL` to the control
plane's public URL, so a process inside reaches the API with the token
beside it ([[011-agent-client]]). `Change.Token` re-projects a token before
expiry without a restart; on k8s the projected Secret is updated, on
podman and `local` the file is rewritten.

### The k8s driver

One namespace (`CELLA_K8S_NAMESPACE`), one PVC and one Pod per sandbox, the
Pod owned by nothing so that a Stop deletes the Pod and keeps the PVC.
`Exec` and `Attach` go through the SPDY exec subresource; `Dial`
through port forwarding. The egress rule is a NetworkPolicy selecting
the Pod, created before the Pod; the mesh rule a second policy
selecting peers by the mesh label plus a headless Service per mesh; the
token and CA a projected Secret. With `CELLA_EGRESS_SIDECAR=1` the
driver adds the gateway as a native sidecar and points the proxy
variables at it; the sidecar is then part of the baseline below.

The class is declared, never inferred: `CELLA_K8S_RUNTIME_CLASS` names
the `runtimeClassName` every Pod gets, and
`CELLA_K8S_RUNTIME_CLASS_ISOLATION` (`container` or `vm`, default
`container`) is what `Isolation()` reports, set by the operator who
knows what backs the class.

Every Pod carries the security baseline of
[[013-security-and-threat-model]], as fields:

| Field | Value |
|---|---|
| `securityContext.runAsNonRoot` | `true` |
| `securityContext.capabilities.drop` | `["ALL"]` |
| `securityContext.readOnlyRootFilesystem` | `true`, with the workspace, `/tmp`, and the volumes as the only writable mounts |
| `securityContext.seccompProfile.type` | `RuntimeDefault` |
| `securityContext.allowPrivilegeEscalation` | `false` |
| `securityContext.privileged` | `false` |
| `hostNetwork`, `hostPID`, `hostIPC`, `shareProcessNamespace` | `false` |
| `automountServiceAccountToken` | `false` |
| the token and CA projected volume, the labels and annotations above, the NetworkPolicy | present |

Decorators let a platform add without a fork:

```go
type Decorator interface {
	Pod(ctx context.Context, spec CreateSpec, pod *corev1.Pod) error
	Claim(ctx context.Context, spec CreateSpec, pvc *corev1.PersistentVolumeClaim) error
}

// Exposer is a Decorator that also gives a public port an endpoint;
// the driver declares Ingress if and only if one is installed.
type Exposer interface {
	Expose(ctx context.Context, spec CreateSpec, port Port) (url string, err error)
	Unexpose(ctx context.Context, id string, port Port) error
}
```

After every decorator has run, the driver compares the object against
the baseline table field by field, and against the identity and
projections it stamped, and refuses the create with
`decorator_violation` naming the first field that differs. A decorator
therefore adds sidecars, volumes, annotations, scheduling constraints,
and endpoints, and cannot weaken the boundary by removal or by
addition. The driver uses upstream `client-go` against
`CELLA_KUBECONFIG` or the in-cluster configuration and assumes nothing
about the cloud.

### The podman driver

Drives the libpod REST API over `CELLA_PODMAN_SOCKET`. One named volume
and one container per sandbox, rootless when the socket is a user's.
`Exec` and `Attach` over hijacked connections; `Dial` through the
engine's port publishing on loopback. Network is a per-sandbox network
whose only route is the gateway, and one network per mesh with the
engine's DNS resolving peers.

`Attach` is built ([[034-terminal-attach]]): an exec session with a TTY over a
hijacked start, resized through the engine, its exit code from the session's
inspect. The engine has no exec kill, so dropping the connection leaves the
process running until the sandbox stops, which the driver's documentation
states and the conformance case allows for.

Podman fixes an object's labels at create, so the mutable half of the
stamped identity is a second, unmounted volume the driver replaces with
a generation rather than a label it edits, and the driver holds no
sandbox state in its own process ([[035-podman-driver]]). The
per-sandbox networks are not built yet; what is built is the
lifecycle, `Exec`, `Logs`, the archive transfers, the stamped identity,
`Attach`, the mesh's own network with its member aliases
([[040-mesh-and-spawn]]), and `Dial`: every declared port is published
on `127.0.0.1` at a host port the engine picks, and `Dial` reads the
mapping from the container's inspect, so a second driver over the engine
dials what the first created ([[060-dial-and-port-proxy]]). Where a
user-space forwarder stands in front of the engine, a closed port reads
as a connection the far end closes at once rather than a refused dial.

### The vm driver

A stub: the package exists, declares `Isolation() == vm`, and
`Preflight` returns a `NotReadyError` naming [[024-vm-driver]], which
holds the design open. What is fixed now: a `vm` driver takes an OCI
image or a root file system, boots a virtual machine per sandbox with
one NIC routed to the gateway, serves `Exec`, `Attach`, `Dial`, and
files through a guest agent, and attaches volumes as block devices or
a shared file system. Its conformance run is the same suite.

### The local driver

The operator's own machine, the harness they already have installed
and logged in, and an OS sandbox around the process, through
`latere.ai/x/pkg/hostsandbox` v0.60.1. What that package provides and
this driver uses: a `StageSpec` of argv, environment, granted paths,
denied paths, and network policy, rendered into the sandbox runtime's
settings; the always-deny table (`~/.ssh`, `~/.aws`, `~/.config/gcloud`,
`~/.kube`, `~/.netrc`, `~/.gnupg`, `~/.docker/config.json`,
`~/.git-credentials`, `~/.npmrc`, `~/.pypirc`, `~/.env`), checked by a
test per entry; detached launch with a handle of pid, start time, and
log path that outlives the process that made it; `Observe` from a
status file; a typed not-ready error with per-platform remediation;
and `hostsandboxtest.Run`, the contract suite of that seam.

What the driver adds: the mapping from `CreateSpec` to a `StageSpec`,
where a sandbox is a directory under `CELLA_DATA_DIR/sandboxes/<id>`
and its main process is one stage; volumes as directories under
`CELLA_DATA_DIR/volumes/<id>` granted read-only or read-write; the
gateway as the one domain the sandbox runtime's proxy allows, with the
CA and the proxy variables in the stage's environment; the token and CA
as files in a readable path; `Exec` as a second stage sharing the
sandbox's grants; `Logs` as `Output` from an offset; and Cella's own
`Alternative` text naming another environment.

What the driver cannot do, said in its capabilities and in the resolved
manifest's warnings: the sandbox runtime grants host paths and mounts
nothing, so `workspace.path` and every `volumes[].path` are rewritten
to their host paths under the data dir with a warning naming both; it
launches detached stages and has no PTY, stdin, or port seam, so
`Attach`, `Dial`, and `Exec` with stdin or a TTY are refused with
`capability_unsupported`; it is allow-only, so its `egress` list omits `open` and `mode: open` is
refused at resolve. `hostsandbox.Capabilities` is that package's own type and
is not this spec's `Capabilities`; the driver reads it at `Preflight`
and maps it.

### The native driver

A directory under `CELLA_DATA_DIR/sandboxes/<id>` and a process started
from the image's command, where the image is a local root file system
or a plain command name. No boundary is enforced. It exists for `make
run`, for the suite, and for a laptop with nothing installed, and its
package documentation says so in the first sentence.

### The remote driver

`runtime/remote` implements `Driver` and every optional interface for
an environment a worker serves: each method becomes an operation on
the environment's queue, claimed by the worker, executed with the
worker's own driver, and answered; streams are relayed over the
worker's outbound connection. `Isolation()` and `Capabilities()` are
what the worker reported at registration; `Preflight` succeeds when the
environment is registered and `Ready` when a worker has sent a
heartbeat within the offline window. `remote` is never selected by
`CELLA_RUNTIME`; it is what an `Environment` with `mode: worker` gets.
[[021-data-plane-workers]] owns the queue and the worker.

### Conformance

`runtimetest.Run(t, func(t *testing.T) runtime.Driver, runtimetest.Options)`
runs
one case per method and one per declared capability, and fails a driver
that declares a capability without its interface or whose declared
capability the case finds not to hold; an undeclared capability's case
is skipped and reported:

| Case | Proves |
|---|---|
| `NameIsolationCapabilities` | the three declarations are stable across calls and the interfaces match the capabilities |
| `PreflightAndReady` | `Preflight` on a ready host is nil; `Ready` is nil with the substrate up and an error with it down where the options can bring it down |
| `CreateInspectDelete` | the phases `Pending` to `Running` to `Deleting`, and `Inspect` after delete is not found |
| `ListReadsIdentityBack` | three sandboxes created, a fresh driver lists three with labels and annotations as written |
| `FilterSelectsOnLabels` | each `Filter` field narrows the list |
| `StopStartKeepsTheWorkspace` | the managed workspace and every attached volume survive stop and start |
| `UpdateEveryMutableField` | each `Change` field lands and is read back |
| `ExecStreamsAndExits` | exit codes 0 and 3; the first stdout byte arrives before the command writes its last (the command writes, sleeps, writes); 64 MiB of output completes with the driver process's resident set growing by less than 16 MiB |
| `LogsFollow` | lines written after `Logs` opened arrive with `Follow` |
| `TarOutAndIn` | round trip of a tree; with `Files`, while `Stopped` |
| `FilesRoundTrip`, `FilesListOrder`, `FilesMutate` | one file written, stat'd, listed, read back with its mode and its exact name; a listing sorted by name with every field; mkdir, remove and move, with a move onto a directory refused |
| `FilesContainment`, `FilesExactNames`, `FilesWriteBound`, `FilesWhileStopped` | traversal, an absolute path outside and a link out of the workspace refused; a name with spaces, unicode, a percent sign, a pipe or a newline round tripped exactly; a body past the bound refused with the previous file intact; on a stopped sandbox every operation works or answers `ErrNotRunning` |
| `TouchStampsActivity` | `last-activity-at` advances |
| `WatchDeliversEveryTransition` | every phase change of a lifecycle arrives as an `Event`; a `relist` follows a closed channel |
| `AttachRoundTrip`, `AttachResize`, `AttachCloseEndsTheStream` | bytes both ways and the exit code; a resize reaches the PTY; after `Close` the stream is over for its caller, whether or not the engine under the driver also ends the process |
| `ExecStdin`, `ExecTTY` | `Stdin` reaches a command whose two outputs stay apart; `TTY` runs one under a terminal, with `Stderr` at its end | 
| `DialReachesAPort` | a listener inside is reachable |
| `VolumeLifecycle`, `VolumeAttachDetachWhileStopped`, `AttachFailureIsClean` | `VolumeDriver`; a failing attach leaves no sandbox |
| `SnapshotAndRestore` | a snapshot becomes a new volume's source |
| `DisplayScreenshot`, `ScreenStream`, `InputAcceptsAndRefuses` | `DisplayReady`, a PNG and a JPEG of the declared geometry at the requested scale, a paced frame off the stream, a gesture inside the desktop accepted and one outside it refused whole ([[041-display-and-input]]) |
| `PortsReportListening` | a declared port a process inside holds reads `listening` and one nothing holds reads `closed` |
| `PrewarmAndAdoptIsExclusive` | two concurrent `Adopt` of one entry yield one success |
| `EgressAllowlistEnforced`, `MeshReachability`, `IngressURL` | against the gateway and upstream of [[012-test-stubs-and-tiers]]; a disallowed host fails, a peer is reachable, a public URL answers |
| `DetachRecovers` | a second driver instance recovers the sandbox from its record |
| `PhaseTableMatchesPackageDoc` | the driver's documented phase mapping equals the observed one |

`native` runs it in the unit suite; `local` runs it where the sandbox
runtime is installed; `podman`, `k8s`, and `remote` run it in the tiers
of [[012-test-stubs-and-tiers]].

The package is built ([[032-runtime-conformance-suite]]) with the cases
that today's `Driver` has an operation for; the `Attacher` cases came with
that interface ([[034-terminal-attach]]), the `Files*` cases with
`FileStore` ([[033-file-operations]]), and the display, screen, input and
port cases with `DisplayDriver` and `InputDriver`
([[041-display-and-input]]). The display cases need an image carrying a
desktop and the port case a command that binds one, both named in the
suite's options, and each skips with that reason where the caller named
none. `DialReachesAPort` came with the first `Dialer`s and needs a command
that serves one port as an echo, named in the options the same way
([[060-dial-and-port-proxy]]). `Watch`, the remaining
optional interfaces, and `PhaseTableMatchesPackageDoc` have no operation on
it yet; a declared capability among them is reported by the suite as
declared without a case, so a driver's run lists what it claims and the
suite cannot yet check. Each of those cases lands with its interface.
`NameIsolationCapabilities` checks the rule both ways for every interface
built: a declared capability without its interface fails, and an interface
without its declaration fails.

### Selection

`CELLA_RUNTIME` picks the in-process driver of the default environment:
`k8s` (default), `podman`, `native`, `local`, `vm`, or `none` for a
control plane that serves only workers. A driver's own variables
(`CELLA_K8S_KUBECONFIG`, `CELLA_K8S_NAMESPACE`, `CELLA_K8S_RUNTIME_CLASS`,
`CELLA_K8S_RUNTIME_CLASS_ISOLATION`, `CELLA_PODMAN_SOCKET`,
`CELLA_LOCAL_SRT`) are read only when that driver is selected. Further
environments are registered objects.

## Not in this spec

The reconciliation that calls these methods ([[005-lifecycle-controller]]);
the worker and the queue ([[021-data-plane-workers]]); the gateway
([[018-egress-and-secrets]]); the shapes of the display and input
requests ([[023-computer-use-operations]]); the microVM driver's design
([[024-vm-driver]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `native` passes the whole conformance suite in the unit suite | `TestNativeConformance` | passing for the cases built, [[032-runtime-conformance-suite]], including the `Attacher` cases of [[034-terminal-attach]], the `FileStore` cases of [[033-file-operations]], the `Pool` cases of [[038-environment-pools]], and `DialReachesAPort` and `PortsReportListening` with the test binary serving the port ([[060-dial-and-port-proxy]]) |
| `local` passes it on a machine with the sandbox runtime installed, with `Attach`, `Dial`, `Mesh`, `Display`, `Input`, `Resize`, `Pool` and the `open` egress case skipped as undeclared, and is skipped whole with the remediation printed where the runtime is absent | `TestLocalConformance` | not built |
| `podman` passes it in the podman tier; `k8s` against kind; `remote` through a worker running `native` | `TestPodmanConformance`, `TestClusterConformance`, `TestWorkerConformance` | `podman` passing against a real engine, the `Attacher`, `FileStore`, display, screen, input, port and dial cases included, skipped where no socket answers, [[035-podman-driver]], [[034-terminal-attach]], [[033-file-operations]], [[041-display-and-input]], [[060-dial-and-port-proxy]]; `TestClusterConformance` built and skipped where no cluster is configured, [[036-k8s-driver]]; both declare `Pool` and pass its cases, [[038-environment-pools]]; `remote` passed the whole suite against an in-process worker running `native`, [[051-environments-and-workers]], until `native` declared `Dial` ([[060-dial-and-port-proxy]]): the remote driver reports the worker's declaration and relays no dial, so its `NameIsolationCapabilities` and `DialReachesAPort` fail until the relay over the worker stream lands or the driver withholds `Dial`; it is reached as the driver of an `Environment` with `mode: worker` by `TestControllerRoutesByEnvironment` and `TestWorkerEnvironmentEndToEnd` ([[054-environments-desired-state]]) |
| A driver that declares a capability without its interface, implements one it does not declare, or declares one the suite finds not to hold, fails | `TestConformanceCatchesAFalseCapability` with nine lying wrappers | passing, [[032-runtime-conformance-suite]], [[034-terminal-attach]], [[033-file-operations]], [[045-workload-tokens]] |
| `Mesh`: podman puts each member on one network per mesh under `<name>.mesh`, k8s renders the policy admitting that mesh alone with a headless Service and each Pod's `hostname` and `subdomain`, the object ends with the last member, and `native` declares none | `TestMeshNetwork` and `TestPodmanMeshOnARealEngine`, `TestRenderMesh`, `TestMeshPolicyAdmitsTheMeshAndNothingElse`, `TestMeshLifetime`, `TestNativeDeclaresNoMesh` | built ([[040-mesh-and-spawn]]); the peer-reachability case belongs to the conformance tier and waits on [[012-test-stubs-and-tiers]] |
| Every stamped label value is a legal Kubernetes label value and every key a legal key, for an owner with `@` and a user label with a `/` | `TestStampedIdentityIsLegal` | passing, [[036-k8s-driver]] |
| The workload token a create carries is a file inside the sandbox its owner alone reads, at `/run/cella/token` or at the path `CELLA_TOKEN_FILE` names where the driver has no mount namespace of its own, and `Change.Token` is what the next read returns | the `TokenProjection` case of the conformance suite | passing on `native` and on `podman` against a real engine; on `k8s` over the client double, and in the cluster run where one is configured ([[045-workload-tokens]]) |
| A decorator that removes the token mount, sets `privileged`, adds `hostNetwork` or `shareProcessNamespace`, or mounts a service account token is refused with `decorator_violation` naming the field | `TestDecoratorCannotWeakenTheBaseline`, table-driven over the baseline | not built |
| With `CELLA_K8S_RUNTIME_CLASS_ISOLATION=vm`, `Isolation()` is `vm` and the Pod carries the class; unset, `container` regardless of the class name | `TestK8sIsolationIsDeclared` | not built |
| `Ingress` is declared only with an `Exposer` installed, and a `public` port then has a URL that answers | `TestIngressNeedsAnExposer`, e2e `IngressURL` | not built |
| In `local`, a stage cannot read any path in the always-deny table, asserted per entry inside a real sandbox; `workspace.path` is rewritten with a warning; `Attach` and stdin are `capability_unsupported` | `TestLocalDenyTable`, `TestLocalPathRewrite`, `TestLocalRefusesAttach` | not built |
| `vm` exists, declares its class, and reports not ready with a message naming [[024-vm-driver]] | `TestVMIsAStub` | not built |
| Every variable in the Selection section is in [[002-repository-scaffold]]'s table with the same default | `TestConfigTableAgrees` reading both files | not built; `CELLA_PODMAN_SOCKET` agrees with that table as of [[035-podman-driver]] |
