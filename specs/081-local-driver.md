---
title: "The local driver: process isolation on a person's own computer through hostsandbox, a granted host folder bound in place as the workspace and never deleted, exec with stdin as an attached stage, files confined in the driver to the same grants, and network from the host's own list"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/013-security-and-threat-model.md
  - specs/018-egress-and-secrets.md
  - specs/021-data-plane-workers.md
affects: [runtime/local/ (new), manifest/v1/, manifest/, runtime/runtimetest/, cmd/cellad/, internal/config/, arch_test.go, docs/local.md (new), api/openapi.yaml, CHANGELOG.md]
effort: large
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# The local driver

## Overview

Spec 004 describes a `local` driver: `process` isolation on the
operator's own machine through `latere.ai/x/pkg/hostsandbox`. It is not
built. This spec builds it for its most demanding user: a worker on a
person's own computer that an agent runtime uses to run commands and
edit files in a folder the person chose.

Four things change from spec 004's text, because the person's own
folder is the point:

1. **A granted host folder is the workspace.** `spec.workspace.host`
   names a folder on the host that the environment grants. It is bound in
   place: never copied, never moved, never deleted. Deleting the sandbox
   deletes only the sandbox's scratch directory.
2. **Exec takes stdin.** An exec with stdin runs as an attached stage
   whose pipes the driver holds. It ends if the driver's process ends.
3. **Network follows the host's list.** With no gateway, the host's own
   allowlist narrows what the sandbox asks for and never widens it, so a
   sandbox that asks for `open` gets the host's list instead of a
   refusal.
4. **No volumes.** The host's granted folders take their place, so the
   driver declares no `Volumes`.

## Current state

- `runtime/local` does not exist. `internal/config/config.go:69` lists
  `k8s`, `podman` and `native`, and `cmd/cellad/main.go`'s `openRuntime`
  and `openWorkerRuntime` fall back to `native`.
- `hostsandbox` drives srt, the sandbox runtime that renders a stage
  into a Seatbelt profile or a bubblewrap invocation. It launches
  detached stages only (`(*Driver).Launch` returns a `StageHandle` of
  pid, start time and log path). It denies the whole home directory
  except the paths a `StageSpec` lists, applies its always-deny table of
  home-relative paths, and renders an allowlisted network through srt's
  proxy. An open network renders as no egress, since srt refuses a
  wildcard (`settings.go:102-104`).
- Spec 004 refuses `Attach`, `Dial`, and `Exec` with stdin or a TTY on
  `local`, and refuses `mode: open` at resolve. It rewrites
  `workspace.path` to a directory under the data dir.
- Resolve refuses an egress mode the environment does not declare with
  `capability_unsupported` (`manifest/network.go:197-199`). The
  controller refuses any mode but `open` while no gateway is connected
  (`controller/controller.go:612`, `needsGatewayFor` in `controller/egress.go:220`),
  and marks `EgressEnforced` `False` with `ReasonNoGateway` when none
  holds the sandbox's map (`controller/egress.go:316-327`).

## Design

### Selection and preflight

`CELLA_RUNTIME=local` selects it in the `serve` and `worker` roles.
`Preflight` calls `hostsandbox`'s `Preflight` and passes its
`NotReadyError` through with the platform's remedy, for example srt not
installed, or unprivileged user namespaces blocked by AppArmor on
Ubuntu 24.04 and later. The driver runs on macOS (Seatbelt) and Linux
(bubblewrap with seccomp). On any other OS `Preflight` answers
`capability_unsupported` naming the OS.

A driver running as root refuses to start: `Preflight` answers
`local_runs_as_root`. The local driver confines a person's own account,
and root would make every grant meaningless.

### Options

```go
package local

type Options struct {
	DataDir   string   // scratch root; sandboxes/<id> below it
	Grants    []Grant  // host folders a sandbox may use
	ReadPaths []string // files or folders readable as they are, never on the deny table
	Deny      []string // paths the embedder denies on top of the always-deny table
	Network   Network
}

type Grant struct {
	Path  string // absolute, clean, an existing directory
	Write bool
}

type Network struct {
	Mode  string   // none | allowlist
	Hosts []string // for allowlist
}
```

`cellad worker` fills `Options` from `CELLA_LOCAL_GRANTS`
(`/path:rw,/path:ro`), `CELLA_LOCAL_READ_PATHS`, `CELLA_LOCAL_DENY`,
`CELLA_LOCAL_NETWORK` and `CELLA_LOCAL_HOSTS`, and spec 002's table lists
all five. An embedder
passes `Options` directly (spec 082).

A grant is refused at `New` when it is `/`, the home directory itself, a
system directory (`/System`, `/usr`, `/bin`, `/sbin`, `/etc`, `/var`,
`/Library`, `/Applications`, `/opt` on macOS; `/usr`, `/bin`, `/sbin`,
`/etc`, `/var`, `/boot`, `/proc`, `/sys`, `/dev` on Linux), or a folder
the process's user does not own. Granting a home subfolder is allowed.
The deny table names paths in the home directory (`~/.ssh`, `~/.env`,
and the rest), and no grant reaches them, through `..` or a link. A
secret file inside a granted folder, such as a project's own `.env`, is
part of what was granted and is readable; an embedder that wants more
withheld names it in `Deny`.

### Capabilities

`Isolation()` is `process`. Declared: `Files`, `Detach`, `HostWorkspace`
and `EgressLocal` (both new, below), and `Egress` of `none` and
`allowlist`. Not declared: `Attach`, `Dial`, `Display`, `Input`,
`Resize`, `Pool`, `Mesh`, `Ingress`, `Volumes`, `Snapshots`, `Autosize`.
Spec 004's volumes as directories under the data dir are left out: a
person's computer has its granted folders instead. A TTY on exec is
refused with `capability_unsupported`.

The driver also implements a new optional interface, so a worker can
report what the host grants (spec 082 carries it in the registration):

```go
type HostDescriber interface {
	Host() v1.HostInfo
}

// in manifest/v1
type HostInfo struct {
	OS      string      `json:"os"`
	Arch    string      `json:"arch"`
	Grants  []HostGrant `json:"grants,omitempty"`
	Network HostNetwork `json:"network"`
}
type HostGrant struct {
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}
type HostNetwork struct {
	Mode  string   `json:"mode"`
	Hosts []string `json:"hosts,omitempty"`
}
```

Two new members of `Capabilities`: `HostWorkspace`, the driver binds a
host folder named by `workspace.host`; and `EgressLocal`, the driver
enforces an allowlist itself, with no gateway (spec 083 reads it).

### The host workspace

A new manifest field, `spec.workspace.host`, holds an absolute, clean
host path. Resolve admits it only when the environment's capabilities
declare `HostWorkspace` and the path is inside one of the grants the
environment reports, and otherwise refuses with `workspace_not_granted`
(422) naming the path. `workspace.host` and `workspace.source` are
exclusive. The driver checks again at `Create`, against its own grants
and not against what the control plane sent: the host has the last word.

A sandbox is `DataDir/sandboxes/<id>/` with `home/`, `tmp/` and, when no
host workspace is named, `work/`. The workdir is the host folder when
there is one, and `work/` otherwise.

`Inspect` reports the scratch directories' host paths as
`status.paths.home` and `status.paths.tmp`. The OS sandbox cannot remap
paths, so a caller that places files of its own (a helper program, an
attachment) needs the real ones, and they are never inside the host
folder.

`Delete` removes `DataDir/sandboxes/<id>/` and nothing else. It never
opens, walks or removes the host folder. A conformance case proves it:
a sandbox on a host folder of known files, written to and then deleted,
leaves every file in the folder as the sandbox last wrote it.

### What a stage sees

Every exec is a stage with these grants:

- the host folder, read-write when its grant is `Write`, read-only
  otherwise;
- the sandbox's `home/`, `tmp/` and `work/`, read-write;
- every path of `ReadPaths`, read-only where it is. A file directly in
  the home directory (for example `~/.gitconfig`) is also symlinked into
  `home/` so tools find it; the OS sandbox checks the link's target, so
  the deny table still applies to it;
- reads of the system paths, and of each `PATH` directory itself,
  needed to run the person's own tools. A `PATH` directory grants exactly
  that directory, never its parent: `~/.cargo/bin` grants
  `~/.cargo/bin`, and a toolchain that needs more (`~/.rustup`) is named
  in `ReadPaths`. A `PATH` entry that is the home directory itself is
  skipped;
- the always-deny table and `Deny`, which hold inside every grant.

`New` refuses a `ReadPaths` or `Grants` entry that is on or under the
deny table or `Deny`, and refuses a `ReadPaths` entry that is the home
directory or a system directory.

`HOME` is the sandbox's `home/`. `TMPDIR` is its `tmp/`. `XDG_CACHE_HOME`,
`GOCACHE`, `GOMODCACHE`, `npm_config_cache` and `PIP_CACHE_DIR` point into
`home/.cache`. `PATH` is the worker's own.

### Exec

- **No stdin, no TTY:** a detached stage through `Launch`, so a
  background command survives a crash of the worker. This is spec 004's
  shape. A worker that stops on purpose stops it (spec 082).
- **With stdin:** an attached stage. The driver runs the rendered srt
  command under `os/exec` and holds its pipes. It ends with the
  driver's process and is reported with exit `-1` and reason `lost`.
  This needs an attached form in `hostsandbox`, `(*Driver).Command(ctx,
  StageSpec) (*exec.Cmd, func(), error)`, returning the command and a
  cleanup for its settings file. That change lands in pkg first (spec
  004's import stays `latere.ai/x/pkg/hostsandbox`).

A sandbox with no `command` runs no main process. It is `Running` while
its directory exists and is not stopped, and `Stopped` after `Stop`,
which ends its detached stages.

### Files

`FileStore` operations run in the driver's own process, outside the OS
sandbox, so the driver enforces the same boundary in Go:

- each grant and the sandbox's directory is opened with `os.OpenRoot`,
  and a path that resolves outside every root is `ErrOutside`;
- a path on the deny table is refused even inside a root;
- a write to a read-only grant is refused.

`ExportTar` and `ImportTar` follow the same rules. `ImportTar` into a
host folder is refused with `capability_unsupported`: a tar never
overwrites a person's folder in one call.

### Network

The host narrows what a sandbox asks for and never widens it:

| Host `Network.Mode` | Sandbox asks `none` | Sandbox asks `allowlist` | Sandbox asks `open` |
|---|---|---|---|
| `none` | no network | no network | no network |
| `allowlist` | no network | the sandbox's hosts that are also on the host's list | the host's list |

An open network is not offered: srt cannot render one (see Current
state), and a person's computer has no reason to need it.

Resolve changes to match. For an environment that reports a host
network (spec 082), resolve narrows the asked mode and hosts to it as the
table says, instead of refusing an undeclared mode with
`capability_unsupported`, and the resolved manifest carries a warning
naming the effective mode and hosts, so a caller sees what its sandbox
got. The driver narrows again at `Create`, against its own network.

When the environment has a gateway, the gateway is the one allowed
domain, as spec 004 describes. A worker on a person's computer has none,
and spec 083 lets `EgressLocal` stand in for the gateway: srt's proxy is
the only route out, and `EgressEnforced` is `True`.

### Logs and records

`Logs` is the main stage's `Output` from an offset. Exec stages' output
streams back to the caller and is not kept by the driver.

## Not in this spec

- Reporting the grants to the control plane, and the host's own
  approvals: spec 082.
- Owned environments, revocation, and no gateway projection: spec 083.
- A TTY, `Attach` and `Dial`; Windows; display and input.
- Keeping toolchain caches across sandboxes.

## Acceptance criteria

1. `runtimetest.Run` passes against `local` on macOS and on Linux, with
   every undeclared capability's case reported skipped.
2. `TestLocalDeleteLeavesTheHostFolder`: write, rename and remove files
   through exec and `FileStore` in a host folder, delete the sandbox, and
   the folder holds exactly what the sandbox last wrote.
3. `TestLocalGrantsNeverReachTheDenyTable`: with a grant over a home
   subfolder, a stage's `cat` and a `FileStore` read of `~/.ssh/id_ed25519`
   and `~/.env`, through `..`, an absolute path and a symlink placed in
   the grant, are refused; and a `Deny` entry inside the grant is
   refused the same way.
4. `TestLocalReadsOutsideGrantsAreRefused`: a file in the home directory
   outside every grant is unreadable from a stage and from `FileStore`,
   including with `~/bin` and `~/.local/bin` on `PATH`.
5. `TestLocalReadPathsRespectTheDenyTable`: a `ReadPaths` entry under
   `~/.ssh` is refused at `New`, and a `Deny` entry inside a granted
   folder is unreadable.
6. `TestLocalWorkspaceNotGranted`: resolve refuses `workspace.host`
   outside the reported grants with `workspace_not_granted`, and the
   driver refuses it at `Create` when the control plane's view is stale.
7. `TestLocalExecWithStdin`: stdin reaches an attached stage and its exit
   code returns, and killing the driver's process reports exit `-1` with
   reason `lost`.
8. `TestLocalNetworkTable`: one case per cell of the network table, at
   resolve (narrowed with a warning, never `capability_unsupported`) and
   at the driver.
9. `TestLocalRefusesRoot` and `TestLocalRefusesSystemGrants`.
10. `docs/api.md`'s code table lists `workspace_not_granted` and
   `local_runs_as_root`, and spec 002's table lists the five
   `CELLA_LOCAL_*` variables.
11. `arch_test.go` has an `engines` row for `runtime/local` that admits
   only `hostsandbox` and the standard library.
