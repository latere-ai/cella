---
title: "The local driver: process isolation on a person's own computer through hostsandbox, a granted host folder bound in place as the workspace and never deleted, exec with stdin as an attached stage, files confined in the driver to the same grants, and network from the host's own list"
status: complete
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
updated: 2026-10-19
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

## Outcome

Built on 2026-10-18. `runtime/local` is the driver, `CELLA_RUNTIME=local`
selects it in both roles, and `docs/local.md` is the walkthrough. Every
criterion has its named test in `runtime/local`, and
`TestLocalWorkerEndToEnd` in `cmd/cellad` drives it through every layer:
`cellad worker` on the local runtime registering its host, a sandbox on a
granted folder resolving with its egress narrowed, a command reading and
writing the folder under the real sandbox, and the folder whole after the
delete.

Where the tests ran. The stand-in for `srt` in the package's tests is a
script that drops srt's settings and runs the command, so
`TestLocalConformance` and every bookkeeping test run on every machine,
the gate's stripped `PATH` included. What only the real sandbox proves is
run where `srt` is installed and skipped with the remediation printed
where it is not: `TestLocalConformanceUnderSRT`, the confinement halves of
`TestLocalGrantsNeverReachTheDenyTable` and
`TestLocalReadsOutsideGrantsAreRefused`, and `TestLocalWorkerEndToEnd`.
They ran on macOS under Seatbelt. Criterion 1's Linux half under
bubblewrap is unrun until a machine with `srt` runs the suite.

Where the code diverges from the design:

- **`Options` gains `Home` and `Lookup`.** `Home` is the home directory
  the deny table and the grants are read against, the process's own when
  empty; `Lookup` reads the environment a stage inherits, `PATH` among
  it. An embedder whose process environment is not the person's sets
  them, and the tests do.
- **A new capability, `ExecStdin`.** The conformance suite ran its stdin
  cases under `Attach` alone, and this driver declares no `Attach`.
  `ExecStdin` says an exec with no terminal takes stdin; the suite runs
  the stdin cases under `Attach` or `ExecStdin` and expects a terminal
  refused without `Attach`; the API's exec socket opens for a command with
  input under it while the attach socket still needs `Attach`; and the
  remote hub intersects it across workers.
- **`status.paths` reaches the file routes.** The API admitted a path at
  or below the workspace alone, so a caller could not place a file in the
  scratch directories the design reports for that purpose. The file routes
  now admit a path below `status.paths.home` or `status.paths.tmp`, and
  the driver holds it to the same containment. An environment with
  `process` isolation runs no image at resolve, as a `native` one does.
- **A path outside every root is `invalid_field`.** The runtime contract
  has no `ErrOutside`; the driver answers `ErrInvalid` as the other
  drivers do for a path outside the workspace. A path on the deny table
  answers the same.
- **The scratch directory holds more.** Beside `home/`, `tmp/` and
  `work/`, a sandbox's directory holds `record.json`, `logs/` (the main
  stage's log), `streams/` (the error output of detached commands) and
  `run/cella/token`, the workload token. `Delete` removes all of it and
  nothing else.
- **`TMPDIR` reaches the command, not srt.** srt places its own socket
  under `TMPDIR`, and a path under the data directory is longer than a
  Unix socket's path may be on macOS, so the stage keeps the worker's
  `TMPDIR` and the command is started through `/usr/bin/env
  TMPDIR=<tmp>`.
- **A detached command's error output is a file of its own.** A stage has
  one log, so a detached exec runs under a shell that appends its
  standard error to a file in `streams/`, read as it grows and removed
  when the command ends.
- **A detached command ends with its caller.** The runtime contract has an
  exec whose request context ends, or whose timeout passes, end its
  command, and a worker closes every exec whose operation ended. So a
  worker that stops on purpose ends every exec stage whatever
  `KeepOnExit` says, and `KeepOnExit` keeps the main stage, which is the
  detached stage of spec 082's criterion 8; what survives the worker's
  process ending unexpectedly is every detached stage, which a driver
  opened over the same data directory still finds running
  (`TestLocalRunStopsItsSandboxes`).
- **An attached command ends with the driver, not with a killed
  process.** `Close` ends every attached command and reports it with exit
  `-1` and `ErrLost`, which the API answers as `phase_conflict`;
  `TestLocalExecWithStdin` proves that half. An attached stage runs in a
  session of its own, so a worker process killed outright leaves it
  running until its closed pipes end it: end of input on its next read,
  `SIGPIPE` on its next write. A command that does neither runs until it
  ends, and is not in the sandbox's record, so a later `Stop` does not
  reach it. Recording attached stages so a restarted driver ends them is
  left for later. Built on 2026-10-19: the record names each attached
  stage by its process group, the start time of the group's leader and
  the process holding its pipes, and a driver ends every one whose holder
  is gone when it starts, and every one when it stops or deletes the
  sandbox (`TestLocalEndsAnOrphanedAttachedCommand` and its `UnderSRT`
  variant).
- **Input reaches an attached command through its own pipe.** `os/exec`
  copies a reader given as `Stdin` in a goroutine `Wait` waits on, so a
  killed command whose input never ended would never report its exit.
- **`Logs` reads the main stage's log file.** The log carries no times,
  so `Since` answers the whole log or nothing by the log's last write;
  `TailLines` and `Follow` are honored.
- **A grant is reported as the folder it resolves to.** A grant is
  checked under the name given and under its target, and stored and
  reported as the target, so a grant of `~/code` that links to
  `/Volumes/work/code` is reported as `/Volumes/work/code`. A `ReadPaths` entry that does not exist
  yet is admitted, and linked into a sandbox's home once it does.
- **`local_runs_as_root` is not an API response.** A worker stops on it
  before it registers, so `docs/api.md` lists it in a table of its own
  beside the refusal table rather than with a status; the error's text
  carries the code, and an embedder tests for `local.ErrRunsAsRoot` with
  `errors.Is`.
- **`CELLA_LOCAL_SRT` is not read.** Spec 004 planned it; `srt` is found
  on `PATH`, as `hostsandbox` finds it, and spec 002's table lists the
  five variables of this spec in its place.
- **The `engines` row admits `latere.ai/x/pkg/uniq` beside
  `hostsandbox`.** The test reads the whole build list, and
  `hostsandbox` imports that helper.
