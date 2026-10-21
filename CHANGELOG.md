# Changelog

What changed for whoever writes a manifest, runs `cellad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

### Added

- `device.PathsFrom(lookup)` resolves the paths `device.DefaultPaths`
  does, with the XDG and `LATERE_*` variables read through `lookup`
  instead of the process's environment, for an application the desktop
  starts that reads them from the person's login shell.

### Fixed

- **Built with Go 1.27.3,** which fixes GO-2026-6809, a flaw in the
  standard library's certificate verification that this code can reach.

## v0.20.0 - 2026-10-21

### Added

- **`latere.ai/x/cella/device` serves a person's own computer as one
  device**, for a program to embed: the host policy file, the environment
  key in the system keychain and its rotation, the person's approval of
  each session, the activity log, the lock that keeps one engine per
  computer, and the control socket. A command-line tool and a desktop
  application that embed it over the same files serve one device, and
  either attaches to an engine the other started. What a hosted plane
  decides (its session labels, its token's audience and claims, its part
  of connecting a computer) the embedder supplies. See
  [the local runtime](docs/local.md#a-persons-computer-as-one-device).
- With `OTEL_EXPORTER_OTLP_ENDPOINT` set, `cellad` also exports the Go
  runtime's metrics: memory, goroutines, GOGC, GOMAXPROCS and scheduling
  latency, as `go.memory.*`, `go.goroutine.count`, `go.config.gogc`,
  `go.processor.limit` and `go.schedule.duration`, through
  `latere.ai/x/pkg` v0.106.0.

## v0.19.1 - 2026-10-20

### Fixed

- **A worker the control plane refuses stops, every time.** When the
  control plane closed a worker's stream with a refusal the worker stops
  on, such as a revoked key, a write racing the close could be recorded as
  the reason instead, and the worker reconnected and kept retrying. The
  close's reason now always wins.
- **Stopping `cellad` waits for an environment's status write.** A
  worker's stream that ended just before shutdown could write the
  environment's status after the store was closed; the write in flight now
  finishes first, and one asked for later is refused.

## v0.19.0 - 2026-10-20

### Changed

- **On macOS the local runtime needs nothing installed.** `cellad`
  confines each command with Seatbelt, part of macOS, through hostsandbox's
  native driver, in place of srt, Node.js and ripgrep. Linux keeps srt.
- **On macOS a network allowlist reaches a host on ports 443 and 80;**
  name another port as `host:port`, for example `github.com:22`.
- **Inside a granted folder on macOS, a command cannot write the files
  your own tools later run as code**: shell startup files, `.envrc`,
  `.gitconfig`, `.gitmodules`, `.mcp.json`, `*.code-workspace`, the
  `.claude`, `.cursor`, `.vscode` and `.idea` folders, and a repository's
  config and hooks. Everyday git works; `git init`, `clone`, `config` and
  `remote add` do not.
- A program that embeds the worker calls `hostsandbox.Main()` first in its
  `main` to use Seatbelt on macOS; without it srt stays.

## v0.18.0 - 2026-10-19

### Added

- **A program embedding the worker is told when it connects.**
  `worker.Options.State` is called with a `worker.State` each time the
  worker's connection changes: `Connected`, with the registration's answer,
  once a registration is taken and its stream is open; not connected, with
  the reason in `Err` and whether and when it tries again, when a
  registration fails or the stream ends; and a last time, with `Reconnect`
  false and the refusal when there is one, before `Run` returns. A
  registration the control plane answers with an error the worker retries
  on is now a `*worker.RegistrationError` carrying the status and the
  error code, so an embedder tells a refused key from a control plane that
  is not ready. The error's text is unchanged.
- **The local runtime is tested under bubblewrap.** A pipeline job installs
  the sandbox runtime on Linux and runs every local runtime test under it on
  each push, so the confinement cases that ran only on macOS run on Linux
  too.

### Fixed

- **An environment reads Ready as soon as its worker registers.** The
  environment's status, its phase, `workers` and `hosts`, was written by the
  phase loop every five seconds, so a read right after a worker connected
  could show it `Pending` with no host. A registration now writes the status
  before it is answered, counting the worker until its stream opens, and the
  end of a stream writes it again, so a read right after either sees it.
- **A missing file reads as a missing file.** A file operation on the local
  runtime that finds nothing now answers `not found: AGENTS.md`, the path it
  looked for, where it answered `environment not found: openat AGENTS.md: no
  such file or directory`, which a worker's record and a host's activity log
  showed as though the environment were gone. For a package importer: the
  text of `runtime.ErrNotFound`, `ErrAlreadyExists` and `ErrNotRunning` is
  now `not found`, `already exists` and `not running`, since each is said of
  a sandbox, a file, a port or a volume alike. `errors.Is` matches as before
  and the API's error codes are unchanged.
- **A command with input does not outlive a killed worker on the local
  runtime.** Such a command runs attached to the worker, and a worker
  killed outright left it running where nothing could reach it, not even a
  stop of its sandbox. The sandbox's record now names each one while it
  runs, by its process group and when that group's leader started, and the
  next worker to start over the same data directory ends every one whose
  worker is gone; a stop or a delete of the sandbox ends them too.

## v0.17.2 - 2026-10-18

### Added

- **A volume create is decided on what it would store.** `volume.create`
  now carries `resource.proposed`, `{owner, metadata, spec}` of the Volume
  the create would write, as `volume.update` already did. The body is
  resolved before the question: `spec.source.kind` is `empty` or `image`
  even where the request left it out, `spec.access`, `spec.retain` and
  `spec.environment` carry their defaults, `spec.size` is the quantity the
  request wrote, and an image source is the digest-pinned reference the
  admission step answered. An authorizer can now allow image volumes only,
  or volumes up to a size, by reading the proposal.
- **A volume limit.** An allow of `volume.create` may carry
  `max_volumes` in its `limits`, `authorizer.WireLimits.MaxVolumes`. The
  control plane counts the subject's volumes that are not being deleted,
  whatever their phase, under the lock its create writes under, so two
  concurrent creates at the limit yield one volume, and refuses a create
  past it with the new code `volume_quota_exceeded`, 422, "You have
  reached your limit of volumes. Delete one to create another.", with
  the count in `details.detail`. An allow without the figure sets no
  limit, and no variable sets one.

### Changed

- For a package importer: `controller.CreateVolume` takes the owner's
  volume ceiling as a fourth argument, `0` for none, and refuses past it
  with `controller.ErrVolumeQuota`; `authorizer.Limits` gains
  `MaxVolumes`.

### Fixed

- **An exec's workdir may be in the sandbox's own workspace.** A sandbox
  whose workspace is a host folder bound in place (the local runtime) takes
  an exec or a terminal with a workdir in that folder or in its scratch
  directories, where `/workspace` was the only root accepted before.

## v0.17.1 - 2026-10-18

### Fixed

- **A command on the local runtime reads its own output with the data
  directory inside the home directory.** Each stage is granted its sandbox's
  logs directory read-only, so `cat`, `head` and anything that stats its
  stdout no longer fail with "Operation not permitted" under the macOS
  sandbox, where a program on a person's computer keeps its data in the
  home directory.

## v0.17.0 - 2026-10-18

### Added

- **The worker is a package a program embeds.** `latere.ai/x/cella/worker`
  serves an environment from the machine a program runs on, with
  `worker.Run`: the program supplies its own key source, read once per
  connection, its own `HostPolicy`, which is asked before each create and
  each operation and may hold a create while it asks a person, and its own
  `Record` callback, told of every operation without its content.
  `cellad worker` runs on it. `examples/worker` is a complete program, and
  [Self-hosting a data plane](docs/workers.md#embedding-the-worker)
  documents it.
- **The host has the last word on a worker.** Before a create runs, the
  worker refuses a `spec.workspace.host` outside the folders its driver
  grants, and a write into a folder it grants read-only, with
  `workspace_not_granted`, and an isolation class other than its driver's,
  or a volume on a driver that keeps none, with `host_refused`, whatever
  the control plane resolved. A create the
  host has not decided within a second shows on the sandbox as the
  `HostApproval` condition, `Unknown` with reason `Pending`, with a
  `sandbox.host_pending` record; a refusal fails it with the reason
  `HostRefused` and the host's words, and no answer within
  `CELLA_HOST_ADMIT_TIMEOUT` (ten minutes) fails it with `HostTimeout`.
  An operation the host refuses answers `host_refused`, and one on a
  sandbox the worker's driver does not hold answers `not_found`.
- **A worker reports the machine it runs on.** A driver that implements
  `runtime.HostDescriber` registers its operating system, architecture,
  granted folders and network, and the environment lists each live
  worker's in `status.hosts`. `spec.workspace.host` names a granted
  folder as the workspace, bound in place; it resolves only inside a
  folder every live worker grants. On an environment whose hosts report a
  network, a manifest's egress is narrowed to what they allow, with a
  warning naming what the sandbox got, rather than refused for a mode the
  environment does not declare.
- **The local runtime.** `CELLA_RUNTIME=local` runs each sandbox as a
  host process confined by the operating system's own sandbox, Seatbelt
  on macOS and bubblewrap on Linux, through the sandbox runtime `srt`.
  `CELLA_LOCAL_GRANTS` names the folders a sandbox may use as its
  workspace, read and write or read only, bound in place and never
  deleted with the sandbox; `CELLA_LOCAL_READ_PATHS` the files and
  folders every command may read where they are; `CELLA_LOCAL_DENY`
  paths denied on top of the always-denied credential paths; and
  `CELLA_LOCAL_NETWORK` with `CELLA_LOCAL_HOSTS` the allow list a
  sandbox's egress is narrowed to. A command with input runs attached;
  one without runs detached and survives the worker's process ending
  unexpectedly. The file routes hold the same lines in the worker. A
  worker started as root refuses to start with `local_runs_as_root`.
  [Sandboxes on your own computer](docs/local.md) is the walkthrough, and
  `latere.ai/x/cella/runtime/local` opens it in a program that embeds the
  worker.
- **`status.paths`.** A sandbox on a runtime that cannot move paths
  reports the host paths of its own scratch directories, `home` and
  `tmp`, and the file routes take a path below either one, so a caller
  places a helper program or an attachment there.
- **`execStdin`.** An environment's capabilities declare it when an exec
  with no terminal takes input on a runtime that has no terminal; the
  exec socket then opens for a command with input and refuses a
  terminal.

- **A subject can own a worker environment.** With
  `CELLA_OWNED_ENVIRONMENTS=worker`, the owner policy lets any subject
  create a `mode: worker` environment and read, change, delete, use and
  key the ones it owns, and list its own; in-process environments stay an
  administrator's. With an authorization endpoint these are its decisions,
  and every `environment.*` question now carries the environment's `mode`
  and `workers`, the value of the new `spec.workers.max`, 0 when unset.
- **Keys with a lifetime.** `POST /v1/environments/{id}/keys` takes an
  optional `{"ttl": "720h"}`, at most `CELLA_ENVIRONMENT_KEY_TTL`; a longer
  one is `ceiling_exceeded`. The `environment.key` question carries the
  lifetime as `ttl` in seconds.
- **`environment.serve`.** A worker's registration asks the authorizer the
  new action with the key's own subject and the worker's driver,
  isolation and host as `registration`; a refusal is `serve_refused`
  (403). It is asked again on every decision's TTL, at most five minutes
  apart, and a refusal closes the stream with 1008 `serve_refused`. The
  owner policy allows a key to serve its own environment. An
  authorization endpoint must answer this action before a worker can
  register.
- **`spec.workers.max`.** A registration beyond the limit is
  `worker_limit` (409). One with a live worker's key from the same host
  replaces that worker, whose stream closes with `worker_replaced`; from
  another host it is refused. Both are recorded as
  `environment.worker_replaced`.
- **A driver that enforces its own allow list.** A driver declaring
  `egressLocal` needs no gateway for a sandbox asking for `none` or an
  allow list on its worker environment; the sandbox reports
  `EgressEnforced` `True` with `DriverEnforced`.

- **A worker's host policy sees a sandbox's annotations.** The create the
  control plane sends a worker carries `metadata.annotations`, so a host
  that asks a person can show what the caller wrote for people, such as an
  agent's display name.
- **`client.MintEnvironmentKeyFor`** mints an environment key with a
  lifetime, for an authorizer that refuses a long one.

### Changed

- **Revoking a key, or deleting a worker environment, reaches its live
  stream.** A revoked key's stream closes at once with 1008 `key_revoked`.
  Deleting a worker environment no longer waits for its sandboxes to go:
  its keys are revoked, its streams close with `serve_refused`, its
  sandboxes' records are dropped with the reason `EnvironmentDeleted`, and
  each worker stops and deletes what it ran. A key is revoked only through
  its own environment; another environment's key answers `not_found`.
- **No gateway reaches a worker's environment.** A sandbox on an
  environment other than the control plane's own is no longer pointed at
  the default environment's gateway: no proxy, reverse address, gateway
  credential or authority is projected, and a boundary there that needs a
  gateway is refused with `egress_gateway_unavailable`. The gateway serves
  the default environment alone, so such a boundary was not held there
  anyway.
- **A worker stops its sandboxes when it stops.** `worker.Run` stops every
  sandbox its driver lists when its context ends, unless
  `Options.KeepOnExit` is set, and stops and deletes them when the control
  plane closes its stream with `serve_refused` or `key_revoked`. `cellad
  worker` sets `KeepOnExit`, so an operator's worker behaves as before.
- `internal/worker` is gone; import `latere.ai/x/cella/worker`.
- An environment whose isolation is `process` runs the host's own tools
  and no image, as a `native` one does: `spec.image` there is
  `capability_unsupported` with the detail "This environment runs host
  processes and no image."

## v0.16.0 - 2026-10-18

### Added

- **Volumes, filled from nothing or from an image.** A `Volume` is
  storage that outlives a sandbox: `PUT /v1/volumes/{name}` creates one,
  `Pending` until it is filled and then `Available`, `GET` and `DELETE
  /v1/volumes/{name or id}` read and delete it, and `GET /v1/volumes`
  lists yours. A sandbox mounts one with `spec.volumes[] {name, path,
  volume, readOnly}`, by name among your own or by `vol_` id where your
  authorizer allows `volume.attach`. `access: single` takes one
  read-write mount or any number of read-only ones; `shared-read` takes
  read-only mounts alone, any number; a mount either excludes is
  `volume_busy`, as is a delete of a volume any sandbox mounts, a stopped
  one included. A sandbox created while its volume is being filled waits
  in `Pending`. `retain: false` deletes a volume with its last sandbox.
  `status.volumes` on a sandbox names what each mount was bound to. The
  `snapshot` and `archive` sources, a named `class`, growth, and a change
  of mounts on a stopped sandbox are refused with
  `capability_unsupported` in this release.
- **An image source is pinned by digest and admitted like a sandbox's
  image.** `spec.source.image` is stored as a reference ending in
  `@sha256:` and 64 hex digits, so a reader verifies a volume's content
  by its digest. With an admission endpoint configured, the image is
  sent to it once, at create, as a Sandbox naming the volume's
  environment, its labels and the image, so an image catalog admits a
  volume's image exactly as it admits a sandbox's and a catalog name can
  come back pinned; the envelope's new `volume` member carries the
  Volume, and is `null` on every Sandbox apply. An endpoint needs no
  change for the catalog to hold.
- **The podman and Kubernetes runtimes keep volumes.** On podman a volume
  is a named volume, and an image volume is filled by the engine from a
  never-started container's file system; its size is advisory. On
  Kubernetes a volume is a `ReadWriteOnce` claim on
  `CELLA_K8S_VOLUME_CLASS`, the workspace class when unset, and an image
  volume is filled by a short-lived Pod that runs the image with `cellad
  fill`, a new subcommand staged from `CELLA_K8S_FILL_IMAGE`: set it to
  the `cellad` image of the same release. Without it, an image source is
  refused. A `shared-read` volume is shared across nodes only on a class
  whose volumes attach to no single node, such as an NFS or CephFS
  class; on block storage its sandboxes share it on one node. The Role
  needs no new verb. The native and remote runtimes keep no volumes.
- The volume loop runs on the writer under a lease of its own,
  `volumes`, which `cella_lease_held` reports. Volume events
  (`volume.created`, `.updated`, `.attached`, `.detached`, `.failed`,
  `.deleted`) reach the sink and the feed. The database needs no
  migration: a volume is a row of the objects table.

### Changed

- For a package importer: `manifest.Lookup` gains `Volume`,
  `controller.Events` gains `EmitVolume`, `runtime.CreateSpec` gains
  `Volumes`, `runtime.VolumeDriver` is new, and `v1.Capabilities` gains
  `volumeImages`. The native driver refuses a create that mounts a
  volume.

## v0.15.1 - 2026-10-14

### Fixed

- **An operation's event is recorded when its caller has already gone.**
  A file read, a file write, a command's exit and the other operations of
  a sandbox were left out of the event feed, with "the event was not
  journaled" and `context canceled` in the log, when the caller closed
  its connection before the record was written, as a client does that
  hangs up once it has the last byte of a file or a command's exit code.
  The record is now written whether or not the caller is still there,
  bounded at 10 seconds, as display and port sessions already were.

## v0.15.0 - 2026-10-14

### Security

- **A sandbox's environment values and gateway credential are no longer
  kept in plain text in the database or in podman labels.** With
  `CELLA_DB_URL` and `CELLA_SECRET_KEY` set, the row that records a
  sandbox held the values of its environment and its gateway credential
  as written, so an account that could read the database, or a dump of
  it, could read them. Both are now sealed in the row under
  `CELLA_SECRET_KEY`, the key mounted secrets are already sealed under,
  and the row of a deleted sandbox keeps neither. The API returns what it
  returned before. When `cellad serve` becomes the writer it seals the
  rows an earlier release wrote, once, before it loads them, and logs how
  many it rewrote; a row it could not rewrite is sealed at its sandbox's
  next write. Without `CELLA_SECRET_KEY` the rows keep the values as
  before, and `cellad serve` says so once at start. On podman, the volume
  that records what changes about a sandbox carried its environment, the
  gateway credential among it, in a label; no label carries it now. The
  container keeps the environment it was created with, and what an update
  or a pool adoption sets is kept inside the container, readable by root
  alone, beside the sandbox's token. `cellad serve` and `cellad worker`
  move the environment an earlier release labeled, once, at start, and
  log how many records they moved.
- The database gains two nullable columns, `objects.sealed_key` and
  `objects.sealed` (migration 7). An earlier release cannot read a sealed
  row. During a rolling update, a replica of an earlier release that
  becomes the writer again after a replica of this release refuses the
  rows and exits rather than serve sandboxes without their values, and
  one that starts after the migration refuses the schema, as it does for
  every migration.
- **`CELLA_SECRET_KEY` is required once the database holds anything
  sealed.** `cellad serve` refuses to start without it, or with another
  key, and says how many sandbox rows it cannot open, where it used to
  start and fail when it became the writer. The Deployment and the check
  Job in `deploy/base`
  now read the key from the `cellad-db` Secret as a required key, so an
  installation that runs the base without a database creates that Secret
  with `CELLA_SECRET_KEY` before applying this release's manifests:

  ```sh
  kubectl -n cella get secret cellad-db > /dev/null 2>&1 || \
    kubectl -n cella create secret generic cellad-db \
      --from-literal=CELLA_SECRET_KEY="$(openssl rand -base64 32)"
  ```

  The install document does the same. `CELLA_DB_URL` in that Secret stays
  optional.
- Added: `cellad rotate` moves every data key the database holds, each
  secret value's and each sealed sandbox's, from `CELLA_SECRET_KEY` to
  `CELLA_SECRET_KEY_NEXT` in one transaction, without reading a value.
  Stop every replica, run it, then start the replicas with the new key
  as `CELLA_SECRET_KEY`. A rotation that meets a row written while it
  runs changes nothing and can be run again.
- Added: `cellad unseal` takes a database back to an earlier release. Stop
  every replica of this release first, since its writer seals each row
  again at its next write and every row when it takes over. Then run
  `cellad unseal` from this release, with `CELLA_DB_URL` naming the
  database's direct endpoint and `CELLA_SECRET_KEY` the key the rows are
  sealed under: it puts every sandbox's environment values and gateway
  credential back in its row and migrates the schema down to version 6.
  Then deploy the earlier release. A second run does nothing. Both
  commands refuse while a replica holds the writer lease, and print
  counts and never a value.

## v0.14.3 - 2026-10-14

### Fixed

- v0.14.1 and v0.14.2 were not published: their releases stopped where
  a check of the release pipeline read the wrong cluster. This release
  carries both: a sandbox's credentials are kept in its Secret and in no
  other object, and a long command over the exec socket keeps its
  sandbox from stopping.

## v0.14.2 - 2026-10-14

### Fixed

- **A long command run over the exec socket keeps its sandbox from
  stopping too.** v0.13.2 held a sandbox active for a command run with
  `POST /exec`; a command run over the exec socket was still marked
  active only at its start and on input, so a long one that read no
  input, such as a large download, had the sandbox stopped under it. An
  attached terminal is unchanged: an idle one still stops.

## v0.14.1 - 2026-10-14

### Fixed

- v0.14.0 was not published: its release stopped because a check of the
  release pipeline read the wrong cluster. This release carries its
  change: a sandbox's credentials are kept in its Secret and in no other
  object.

## v0.14.0 - 2026-10-14

### Security

- **On Kubernetes, a sandbox's credentials are no longer copied into object
  metadata or into its Pod.** The annotation that records a sandbox on its
  PersistentVolumeClaim held the values of the sandbox's environment and
  its gateway credential, and the sandbox's Pod carried the same values,
  the proxy variables that embed the credential among them, in its
  container's environment, so an account that could read claims or Pods
  in the sandbox namespace could read them. Both now leave the values out.
  They are kept in the sandbox's own Secret, which already held its
  workload token and is removed with the sandbox, and the Pod's container
  takes each variable from that Secret. The Secret now also names the
  claim as its owner. A command run in a sandbox no longer carries the
  sandbox's own variables in the request that starts it, only ones an
  update changed since the sandbox started and the request's own.
  `cellad serve` and `cellad worker` rewrite the annotations an earlier
  release wrote, once, when they start, and log how many they rewrote; a
  claim they could not rewrite is rewritten at its sandbox's next stop,
  start or update. A sandbox an earlier release started keeps its Pod, and
  the values in it, until its next start: a running Pod's environment
  cannot change, and stopping and starting a sandbox keeps its workspace.
  The environment a sandbox sees is the same, with one exception: a value
  holding `$(NAME)` or `$$` reached the main process rewritten by
  Kubernetes before, and now arrives as written, as it always did for a
  command. An update of the environment still reaches the main process at
  its next start and a command at once. The Role needs nothing new:
  `create`, `get`, `update` and `delete` on `secrets` are the verbs it
  already grants. `cellad check` still writes nothing.

## v0.13.2 - 2026-10-14

### Fixed

- v0.13.1 was not published: its release stopped on a data race in one
  of its own tests. This release carries its change: a running command
  keeps its sandbox from stopping.

## v0.13.1 - 2026-10-13

### Fixed

- **A running command keeps its sandbox from stopping.** A command marked
  its sandbox active only when it started, so one that ran longer than
  the sandbox's `autoStop`, such as a long download or build, had the
  sandbox stopped under it. A command now marks its sandbox active every
  30 seconds until it ends, whether it is run with `?wait=1` or streamed.

## v0.13.0 - 2026-10-12

- Changed: a start of a `Failed` sandbox whose main process ran and ended
  is taken, and keeps the workspace. `POST /v1/sandboxes/{id}/start`,
  `cella start` and `StartSandbox` answered `phase_conflict` for every
  `Failed` sandbox before, which left a client one act on a sandbox whose
  process had exited non-zero or been killed for memory: the delete that
  discards its files. The start now runs the sandbox's command again from
  the specification it was created with, on the same workspace, for any
  failure its runtime reports (`status.reason` `Exited`, `Error`,
  `OOMKilled` and the like), under the caller's sandbox limit. Every file
  of the workspace is kept; processes, and files outside the workspace,
  are gone as after a stop and a start. It is one start: a process that
  ends again leaves the sandbox `Failed` again. A `Failed` sandbox that
  never ran is still refused with `phase_conflict`, now with the reason
  in `details.detail`: `CreateFailed`, `NoCapacity`, `StartDeadline`,
  `RecoveryExhausted` and `VolumeMissing`. **A client that relied on `phase_conflict` to tell
  that a failed sandbox was beyond recovery, and deleted it, should start
  it instead**; deleting is what loses the files. The lifecycle rules are
  as they were: `ttl` deletes a failed sandbox, and `autoDelete` and
  `autoStop` do not apply to one. A runtime built on the driver contract
  is held to the same by the driver suite's new case,
  `StartAfterFailureKeepsTheWorkspace`, and a server by the conformance
  case `case005StartAfterFailure`. See `docs/manifest.md`, "Recovering a
  failed sandbox".
- Added: a manifest may name a ceiling per resource in
  `spec.resources.max`, as `{"cpu": "2", "memory": "6Gi"}`. The sandbox
  starts at `spec.resources` and is sized inside the ceiling: its CPU
  limit is `max.cpu` while its request stays the fraction of
  `resources.cpu` it was, so it bursts into idle CPU and reserves nothing
  more; its memory is raised one step at a time, never past `max.memory`,
  when its workload uses 85% or more of what it has at 2 readings in a
  row or the kernel kills one of its processes for memory. A raise is
  applied in place where the node has room, and otherwise by restarting
  the sandbox on a node that has it once no command, terminal, port
  connection or file transfer is open in it; the workspace is kept. A
  manifest with no `max` resolves, renders and runs as it did. See
  `docs/manifest.md`, "Sizing inside a ceiling".
- Added: `capabilities.autosize`, an object of three flags, `cpu`,
  `memory` and `disk`, on every environment's `status.capabilities` and in
  the environment an admission request carries. A ceiling on a resource
  whose flag is `false` is refused with `capability_unsupported` at
  `spec.resources.max.<resource>`; one below the amount it bounds is
  `invalid_field`, and one above the server's own ceiling is
  `ceiling_exceeded`. An admission endpoint may set a ceiling, and reads
  the flags first: the Kubernetes runtime declares `cpu` always, `memory`
  on a cluster that serves `pods/resize` (Kubernetes 1.33 or newer) and
  the metrics API, and `disk` not yet; no other runtime declares any.
- Added: a sandbox with a ceiling carries `status.resources`, the size it
  holds now; `status.usage`, the last reading of its working set,
  `{"memory": <bytes>, "readAt": ...}`; `status.moves`, how often a raise
  restarted it; and the `MemoryAtCeiling` condition. Two event types
  record it: `sandbox.resized` with `{resource, from, to, reason, moved}`,
  where `reason` is `usage`, `oom` or `update`, and `sandbox.at_ceiling`
  with `{resource, used, max}`, written once each time a sandbox at its
  memory ceiling reaches 95% of it or loses a process to the limit.
- Added: on Kubernetes, a sandbox that ends `Failed` with `OOMKilled` and
  has room under `max.memory` is started again at the next size instead
  of staying failed. A command that ends with exit code 137, or whose
  stream breaks, has its sandbox read before the exit is returned, so the
  raise is already asked for when the client sees the exit, and
  `sandbox.resized` with `reason: oom` says why. On a node whose kubelet
  kills one process rather than the whole container, the kill is found
  from the kernel's own count in the sandbox's cgroup, read with `cat`
  through the exec path, and memory is raised in place.
- Changed: a sandbox the control plane is starting again at a larger
  memory size never reads `Failed`, `Stopped` or not found. From the kill
  that took its workload, or from the stop of a move to a node with room,
  until it runs again, every read of it, by id or in a list, answers
  phase `Starting` with reason `MemoryRaised`; no `sandbox.failed`,
  `sandbox.stopped` or `sandbox.started` is recorded for it; and a start,
  a stop or a command in that time is `phase_conflict`. A client that
  replaces a sandbox when it reads `Failed` therefore keeps a sandbox
  that is only being given more memory, and its workspace with it. A
  sandbox killed at its ceiling is `Failed` with reason `OOMKilled`, as
  before; a start brings it back at the size it holds, and raising
  `spec.resources.max.memory` on it starts it again at the next size,
  each with its workspace. Where a restart found no node with room, the
  sandbox runs on at the size it holds and carries the new
  `MemoryUnplaced` condition, `True` with reason `NoRoom` and the size
  that found no room in its message. A kill that follows is answered at
  once by a start at that size, with no act of the client: the sandbox
  reads `Starting` with reason `MemoryNotRaised`, then `Running`, and
  more memory is asked for again later. That start is made a few times
  in a row and no more, with a short wait between two of them; a sandbox
  killed again after the last reads `Failed` with reason `OOMKilled`,
  its condition says `RestartsExhausted`, and a start or a delete of it
  is the client's to choose. See `docs/manifest.md`, "What a client can
  rely on", and `docs/kubernetes.md` for the figures.
- Added: `CELLA_AUTOSIZE_INTERVAL`, default `1m`, how often a sandbox
  with a ceiling is read. The loop runs on the writer under the
  `autosize` lease, which `cella_lease_held` now reports.
- Changed: the base Role in `deploy/base/rbac.yaml` grants `patch` on
  `pods/resize` and `get` on `pods` of `metrics.k8s.io`. Both are
  optional: `cellad serve` starts without them, logs which is missing,
  and sizes no sandbox's memory. Add the two rules to a Role you wrote
  yourself, and run a metrics-server, to turn memory sizing on.
- Changed: an environment's declared memory capacity counts a sandbox at
  the memory it holds now, which for one raised inside its ceiling is
  more than `spec.resources.memory`. A pool takes no ceiling:
  `spec.pool.resources.max` on an environment is `invalid_field`, and a
  manifest with a ceiling is created fresh rather than adopted from the
  pool.
- Changed: in the `runtime` package, `Change` gains `Resources`, the size
  a sandbox is to hold; `Resources` gains `CPULimit`; `State` gains
  `Resources` and `Resize`, the size held and a size asked for and not
  yet held; and `MemoryReader` is the optional interface a driver that
  sizes memory implements. A driver that changes no size refuses
  `Change.Resources` with `ErrUnsupported`, which `Change.FixedSize`
  answers for it, and the driver suite holds the field to the
  declaration both ways. In `manifest/v1`, `Capabilities` gains
  `Autosize` and `Capabilities.Equal` compares it. In the `controller`
  package, `Options.AutosizeInterval` and `Controller.RunAutosize` are
  the reading loop, and `Controller.Dialer` returns the driver's dialer
  behind a count of open connections.
- Fixed: on Kubernetes, a connection `Dial` opened to a port takes a
  deadline that is set after the port forwarding session has ended, as a
  socket whose peer has closed does. The session to a port nothing listens
  on ends at once, and a caller that then set a read deadline was told of
  a closed pipe in place of reading the kubelet's reason.

### Security

- Built with Go 1.27.2 and golang.org/x/net v0.60.0, which fix GO-2026-6611, GO-2026-6612, GO-2026-6613 and GO-2026-6617.

## v0.12.0 - 2026-10-05

- Changed: the sandbox limit, `max_sandboxes` on an allow, counts the
  sandboxes that run or will run without a start: pending, queued,
  starting, running, stopping, lost and recovering ones. A stopped,
  failed or deleting sandbox no longer counts, so a subject at the limit
  stops one and creates another. A stopped sandbox is now bounded by its
  `ttl` and `autoDelete` alone; set them from your admission endpoint to
  bound what a subject keeps.
- Changed: starting a stopped sandbox is checked against the limit, and
  a start past it is refused with `quota_exceeded`. The start reads the
  figure from the allow of `sandbox.update`: an authorization endpoint
  carries `max_sandboxes` on that allow as well as on `sandbox.create`,
  and an allow without it puts no ceiling on the start.
- Changed: the message of `quota_exceeded` is "You have reached your
  limit of running sandboxes. Stop one to start another." It was "You
  have reached your sandbox limit."
- Changed: in the `controller` package, `Controller.Start(ctx, id, max)`
  starts a stopped sandbox under its owner's ceiling, as `Create` creates
  one under it. `Act` takes `stop` and `delete` and refuses `start` with
  `ErrPhase`; a plane that started sandboxes through `Act` calls `Start`
  with the figure it passes to `Create`.
- Fixed: `cella help` says that inside a sandbox the token is already in
  the file `CELLA_TOKEN_FILE` names, or at `/run/cella/token`, and that the
  address is not: set `CELLA_URL` or pass `--url`. It had said the address
  was already in the sandbox's environment, which no runtime sets.
- Fixed: `deploy/examples/generic/configmap.yaml` no longer sets
  `CELLA_DEFAULT_AUTOSTOP`, `CELLA_DEFAULT_TTL` and
  `CELLA_DEFAULT_AUTODELETE`, which `cellad` does not read: a copy of the
  example gave sandboxes no lifecycle default. A manifest that names no
  lifecycle runs until it is stopped or deleted; set defaults in your
  admission endpoint.
- Fixed: the scrape carries `cella_environments`, environments by `phase`
  and `reason`, and `cella_workers_connected`, the workers of each
  environment holding a stream. Both were declared and never exported, so
  the `CelladEnvironmentOffline` alert could not fire; it now fires on an
  environment that stays `Offline` for five minutes.

## v0.11.1 - 2026-10-02

- Changed: in the API document, `api/openapi.yaml` and `GET /openapi.yaml`,
  every operation's `summary` is a short action name of at most four words,
  such as `Create a sandbox`, where it was a sentence. The sentence now
  opens the operation's `description`, except on the port proxy and redirect
  operations, whose shared description already stated it. A reference that
  lists operations by their summary can use it as a label.
- Added: the API document shows the answer of every operation that has a
  body, and every JSON request body, as a wire example: the object as the
  server encodes it, or a few lines of a log or of the event feed. Where the
  bytes are a file, an image or a stream of frames the document states a
  binary schema under the media type instead. The WebSocket routes say which
  frames travel once the connection is upgraded.
- Fixed: the API document names the status each route answers. It named
  `200` where a create answers `201` (a Secret, an Environment, an
  environment key, a worker registration, and an apply that creates a Secret
  or an Environment), where deleting a sandbox answers `202`, where deleting
  an environment and revoking a key answer `204`, and where five WebSocket
  routes answer `101`. `FileInfo.mode` is octal text such as `0644`, as the
  server writes it, and the `path` query parameters of the file routes state
  their type under `schema`. The server's answers are unchanged.

## v0.11.0 - 2026-10-01

- Added: a Secret's `status.lastUsedAt`, when an egress gateway last
  substituted its value into a request, as an RFC 3339 time in UTC, at a
  five-minute resolution: it moves at most once every five minutes per
  secret, so a busy workload does not write the secret on every request. A
  secret no gateway has substituted has no `lastUsedAt`. The gateway reports
  each use to the control plane, so the field fills once both the control
  plane and its gateways run this release.

## v0.10.1 - 2026-10-01

- Fixed: on the k8s driver, a file operation, an archive transfer, an exec
  or a desktop operation on a sandbox whose Pod is still starting answers
  `409 phase_conflict`, with what the Pod is waiting for in the detail, such
  as `container main is ContainerCreating`. It had exec'd into a container
  that did not exist yet and answered `503 driver_unavailable` with the
  kubelet's `container not found`.
- Changed: the display supervisor paints the desktop's root window
  `#3b4252` once the window manager registers, and again after the manager
  or the X server restarts, so a desktop with no window open is visible as
  an empty desktop rather than a black frame. `CELLA_DISPLAY_BACKGROUND`
  sets another color. The `cella-display` image adds `x11-xserver-utils`
  for `xsetroot`; an image without it keeps the black root.

## v0.10.0 - 2026-09-29

- Fixed: an apply over an existing Sandbox asks `sandbox.update` about the
  Sandbox as stored, its name and labels included, with the body only in
  `resource.proposed`. The resource had carried the body's labels, so an
  authorization endpoint that reads tenancy from labels decided on the
  labels the request asked for rather than those the Sandbox had.
- Added: `secret.update` carries `resource.proposed`, the `owner`,
  `metadata` and `spec` of the Secret the request would write, as
  `sandbox.update` does, so an authorization endpoint can refuse a change of
  labels or scope it does not allow. The rest of the resource is the stored
  Secret, as before, and the proposed `spec` never carries the value.
  [Building a plane](docs/plane.md#who-may-do-what) says how to read it.
- Changed: `PUT /v1/secrets/{name}` on a Secret you hold reads and validates
  its body before it asks `secret.update`, as a sandbox apply does. A body
  that does not resolve now answers its own refusal, such as `400
  invalid_field` or `413 body_too_large`, where a caller the endpoint
  refuses was answered `403 forbidden` before.

## v0.9.1 - 2026-09-27

### Security

- The egress gateway refuses a request inside a terminated tunnel whose
  `Host`, or `:authority` over HTTP/2, names another authority than the
  tunnel's, answering `421 Misdirected Request`. Before, such a request was
  sent to the tunnel's host with that host's credentials swapped in, and a
  front that routes by `Host` could deliver them to another service. From
  `latere.ai/x/pkg` v0.89.1.

## v0.9.0 - 2026-09-27

- Changed: `cella_requests_total`, `cella_request_duration_seconds` and the
  request log line name a request refused before it reached an endpoint
  `unmatched` instead of leaving `route` empty. Every Latere service uses
  that label for such a request.
- Changed: the server span and the OpenTelemetry request metrics take the
  route from the shared handler in `latere.ai/x/pkg/otel`, which now decides
  one route per request. The values are unchanged: the span is named
  `GET /v1/sandboxes/{id}` and `http.route` is `/v1/sandboxes/{id}`.
- Changed: with no authorization endpoint configured, a token minted from a
  service account's key is narrowed by the grants it carries, as a personal
  access token's is.
- Added: with `CELLA_DB_URL`, `cellad serve` runs as several replicas that
  roll with no gap. One replica is the writer: it holds a lease in the
  database, runs the background loops, holds the gateways' and the workers'
  streams, and answers the API. The others are standbys that forward every
  API request to the writer, WebSockets and streams included. A writer that
  is stopped finishes its requests for at most `CELLA_HANDOFF_TIMEOUT`
  (default `10s`) and hands the lease to a standby, which takes over within
  about a second; a request that arrives meanwhile is held for at most
  `CELLA_FORWARD_HOLD` (default `15s`) and then answered. Each replica sets
  `CELLA_ADVERTISE_URL`, the address the others reach its public listener
  at, such as `http://$(POD_IP):8080`. Readiness does not depend on the
  lease. [Running more than one replica](docs/kubernetes.md#running-more-than-one-replica)
  lists what a Deployment changes: `RollingUpdate` with `maxSurge: 1` and
  `maxUnavailable: 0`, a disruption budget, a network policy between the
  replicas, the grace period, and the database pool.
- The first rollout onto this release is a `Recreate`: an older process runs
  no standby, and a new replica beside it would become a second writer. Set
  `CELLA_ADVERTISE_URL` in that rollout already, so the writer it starts
  advertises where the next rollout's standbys forward to. Rollouts after
  it can be rolling.
- Added: `503 control_plane_unavailable`, "The control plane is unavailable;
  retry shortly.", for a request no replica could take within its hold,
  which happens only while one replica hands off to another. Retry it.
- Changed: with `CELLA_DB_URL`, a process waits for the writer lease before
  it opens its controller. A process that crashed without handing off holds
  the lease until its 15 second term lapses, so the next one waits up to
  that long before it serves.
- Changed: `cella_lease_held` carries the name `writer`, and the
  `CelladLeaseNotHeld` alert fires when no replica holds a lease rather than
  when one replica does not, since a standby holds none. New:
  `cella_forwarded_requests_total{outcome}` counts what a standby forwarded.
- Changed: when a writer hands off, or a single process stops, every
  terminal, screen and dial WebSocket is closed with `1001` and a create
  held under `?wait=1` answers the sandbox as it stands, rather than
  waiting out the shutdown's grace period.
- Fixed: a gateway or a worker whose stream to the control plane had
  dropped a few times over its life waited up to 30 seconds before dialing
  again, because its backoff never reset after a stream that worked. It now
  dials again at once after a stream the control plane accepted.
- Fixed: a delete whose runtime call failed, or that a restart cut short,
  left the sandbox `Deleting` until someone deleted it again. The reaper now
  finishes such a delete at its next tick.
- Fixed: a create or a spawn whose name another replica had just taken, or
  a write over a row another replica had changed, answered
  `driver_unavailable`. They answer `name_taken` and `version_conflict`.
- The database gains one nullable column, `leases.address` (migration 6),
  which an older release ignores.

## v0.8.0 - 2026-09-27

- Fixed: the OpenTelemetry request metrics (`http.server.request.duration`
  and the rest) carried no `http.route`, so every request fell into one
  series whatever endpoint it reached. They now carry the route's path
  template, such as `/v1/sandboxes/{id}`; a path no route serves still has
  none. The server span's `http.route` is the same template, without the
  method it used to start with; the span's name keeps the method.

## v0.7.1 - 2026-09-26

- v0.7.0 was tagged but not published: the release runner's race tier
  found the environment phase loop and a sandbox delete writing the local
  snapshot store at once. This release carries the same binaries, packages
  and API as the v0.7.0 notes below describe, with that fixed.
- Fixed: with no `CELLA_DB_URL`, the snapshot under `CELLA_DATA_DIR` could
  be written from two goroutines at once, since an environment's phase is
  written outside the lock a sandbox write holds, and the write of the
  environment read the sandboxes while the other changed them. Under load
  that is a crash on concurrent map access or a snapshot that misses one of
  the two writes. The store now serializes its own writes and keeps its own
  copy of the sandboxes. A deployment with Postgres was not affected.

## v0.7.0 - 2026-09-26

- `GET /v1/secrets` takes `owner` and `label`, as `GET /v1/sandboxes`
  does: `?owner=<status.owner>` answers that owner's secrets and
  `?label=key=value`, repeatable, the secrets carrying every label. Both
  narrow what the caller may already read and never widen it, so naming
  another subject's owner answers what the caller could already read of
  that subject's secrets, which under the owner policy is an empty page.
  `ListSecrets` in the client package and `cella get secrets --owner` send
  it. A server before this release ignores both on the secret list and
  answers every secret the caller may read, so a program that acts on the
  answer against an older server, such as one that deletes a person's
  secrets, should still compare `status.owner`.
- Fixed: `GET /v1/sandboxes` failed the whole page with
  `driver_unavailable`, or with `not_found` for a sandbox on an
  environment the server holds no driver for, when reading one sandbox
  from its environment failed. The page now answers every sandbox: a row
  that could not be read carries the status last recorded and the
  condition `Observed` with `status` `False` and the reason
  `DriverUnavailable` or `EnvironmentNotHeld`, and the failure is logged
  at warn with the sandbox and the error. `phase` filters such a row by
  the phase last recorded. Reading that sandbox by id is still refused
  with `driver_unavailable`, and a request its caller closed still ends
  the page.
- Fixed: with `CELLA_DB_POOL_URL` naming a pooled endpoint whose URL
  carries `default_query_exec_mode=exec`, the mode a transaction pooler
  needs, every write of an object or an observed state failed with `cannot
  find encode plan` for its labels, and every journal append and operation
  enqueue failed with SQLSTATE 22P02, as did a list filtered by labels. In
  that mode the server describes no parameter, so the driver encoded the
  label maps from their Go type, which it cannot, and sent each JSON
  document as `bytea`, which a `jsonb` column refuses. The store now binds
  every JSON value as text, which each connection mode sends as it is. A
  direct endpoint, or a pooled one in a mode that describes each statement,
  was not affected.

## v0.6.3 - 2026-09-25

- Fixed: a request its caller closed while `cellad` was still answering,
  such as a browser tab closed during a log read, was counted and logged
  as a `5xx` `driver_unavailable` or `authorizer_unavailable`, and an
  authorization question it left unanswered counted as the authorizer
  being `unavailable`, which the alert on that outcome reads. Such a
  request is now `4xx` `client_closed` in `cella_requests_total` and the
  request log, and the unanswered question is not counted.

## v0.6.2 - 2026-09-25

- Fixed: inside a sandbox behind an egress gateway, HTTPS to any host no
  secret is bound to failed certificate verification. `pip install`, `curl
  https://github.com`, `git clone` and `go get` all failed. The trust
  variables (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`,
  `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`) named a file that held only the
  gateway's own authority. The gateway passes such a host through untouched,
  so the workload is shown the host's real certificate, and OpenSSL, curl,
  git, Python and Go read that file as their whole trust store. On
  Kubernetes this broke every sandbox with a gateway in v0.6.0 and v0.6.1.
  On Podman and the native runtime it has been broken since v0.2.0, the
  first release that pointed a sandbox at a gateway. Node was unaffected,
  because `NODE_EXTRA_CA_CERTS` adds to its own roots. The file now holds
  the public roots and then the gateway's authority, so a host the gateway
  passes through verifies against the first and a host it substitutes a
  secret for verifies against the second.
- The public roots are the system's, read once at start: the file
  `SSL_CERT_FILE` names in the environment of `cellad serve` or `cellad
  worker`, or else the first of Go's Linux bundle files that holds a
  certificate. The released image ships
  `/etc/ssl/certs/ca-certificates.crt`. To add roots of your own, for hosts
  whose certificates come from a private authority, point `SSL_CERT_FILE`
  at a bundle that holds them and the public ones. `cellad serve` with
  `CELLA_GATEWAY` set, and every `cellad worker`, now refuse to start when
  they find no roots, or more than 512 KiB of them, and the message names
  `SSL_CERT_FILE` and the files they tried. Without `CELLA_GATEWAY`, `cellad
  serve` reads no roots, since no sandbox is pointed at a gateway. `cellad
  check` reads them on its `gateway` line and fails that line where `cellad
  serve` would refuse.
- Sandboxes created before this release keep the old file until it is
  written again. On Kubernetes that happens at the sandbox's next token
  rotation, at two thirds of the token's life, without a restart, and at
  its next start. A sandbox whose token lasts as long as the sandbox itself
  is never rotated, so stop it and start it, or wait for the rotation. On
  Podman and the native runtime nothing rewrites the file, so delete the
  sandbox and create it again.
- For a program that builds a driver itself: `k8s.Options.TrustRoots`,
  `podman.Options.TrustRoots` and `(*native.Driver).SetTrustRoots` take the
  public roots, `egress.LoadRoots` reads them the way `cellad` does, and
  `egress.TrustBundle` composes the file. A driver given no roots writes
  the gateway's authority alone, as before. `runtime.Egress.CAPEM` is still
  the authority alone.

## v0.6.1 - 2026-09-25

- v0.6.0 was tagged but not published: two of its own tests read a sandbox
  straight after a create and failed under the race detector on the release
  runner. This release carries the same binaries, packages and API as the
  v0.6.0 notes below describe. The base path test and the client's example
  now hold the create with `?wait=1` and `client.Wait` before they run a
  command in the sandbox, as every caller that acts on a new sandbox has to
  since v0.6.0, and the egress subcommand test creates once the control
  plane counts its gateway rather than when the gateway's doors are bound.

## v0.6.0 - 2026-09-25

- A create answers at once, which changes what every caller of `POST
  /v1/sandboxes`, and of `PUT /v1/sandboxes/{name}` when it creates, reads.
  The answer is `201` with `Location` and the sandbox as soon as it is
  recorded, in phase `Pending`, before its workload runs. The control plane
  then pushes the boundary to a gateway, mints the identity and asks the
  runtime for the sandbox, and the sandbox moves to `Running`, or to
  `Failed` with its reason, which a read or the events feed shows. On a
  runtime that provisions and attaches storage before a workload starts,
  that no longer holds the request. A caller that runs a command in the
  sandbox or copies files into it straight after the create now waits for
  `Running` first, or asks for the old answer with `?wait=1`: it holds the
  answer until the sandbox has left `Queued`, `Pending` and `Starting`, for
  at most `timeout` (`10m` when absent, at most `1h`), and answers `201` in
  every case, with the sandbox as it stands. A runtime failure used to be
  the answer, an error that did not name the sandbox it left `Failed`; the
  sandbox and its reason are the answer now. A sandbox the environment
  adopts from its pool still answers after the adoption, already running,
  and a queued create still answers `Queued`.
- `cella apply --wait` is the long form of `-w`, and both now ask the server
  to hold the create's answer, then read the sandbox until it runs as
  before. `client.CreateSandbox` and `client.ApplySandbox` take
  `...client.CreateOption`, and `client.Wait(timeout)` is the hold; a call
  without options compiles and answers as the server now does.
- A create whose egress boundary needs a gateway, an egress mode other than
  `open`, a denied host or a mounted secret, while no gateway is connected,
  is refused with `503 egress_gateway_unavailable` and a message that says
  to connect one or open the boundary. It was `driver_unavailable`, which
  read as the runtime being down. A gateway that stops answering after the
  create is accepted fails the sandbox with `CreateFailed` and the
  condition `EgressEnforced False NoGateway`.
- The conformance suite's browser case reads a sandbox's display until it
  reports ready before it asks for a screenshot, since a desktop may come
  up a moment after the workload, and it adds `case008CreateWait`, which
  holds a create with `?wait=1` and expects `Running`.
- For a program that imports `controller`: `Create` and `Spawn` answer once
  the desired state is written, and `RunScheduler` is what finishes the
  create, so a server of its own runs it beside `RunReaper`; without it a
  created sandbox stays `Pending`. The driver's create runs with the
  controller's lock released, so reads, lists and other creates answer
  while a runtime brings a sandbox up. A create a restart interrupted is
  finished by the next process's first scheduler pass, and `Changed`
  returns a channel closed at the next write of any sandbox.
- Sandboxes on Kubernetes can be confined to the egress gateway. Set
  `CELLA_K8S_GATEWAY_SELECTOR` to the labels of the gateway's Pods, with
  `CELLA_K8S_GATEWAY_NAMESPACE` and `CELLA_K8S_GATEWAY_PORTS` where they
  differ from the sandbox namespace and `3128,8080`, and every sandbox runs
  under a NetworkPolicy of its own that admits cluster DNS, the gateway and
  the members of its own mesh, and nothing else. The environment then
  enforces the `none`, `allowlist` and `open` boundaries: a manifest that
  declares one no longer carries the warning, and `EgressEnforced` is true
  once the gateway holds the sandbox's boundary. The workload container
  carries `HTTPS_PROXY`, `HTTP_PROXY`, `CELLA_GATEWAY_URL` and the trust
  variables, and the gateway's authority is at `/run/cella/egress-ca.pem`,
  as on Podman. `CELLA_K8S_DNS_SELECTOR` and `CELLA_K8S_DNS_NAMESPACE` name
  a cluster's DNS Pods where they are not `k8s-app=kube-dns` in
  `kube-system`. The selector is refused at start without `CELLA_GATEWAY`,
  and the other four without the selector. The cluster's network plugin has
  to enforce NetworkPolicy, egress included.
- Every sandbox on Kubernetes now runs under its own NetworkPolicy, gateway
  or not: nothing in the cluster opens a connection to a sandbox's Pod
  except the other members of its mesh. Commands, files, logs and ports are
  unaffected; they reach the sandbox through the API server. The Role needs
  no new access, since a policy is made with `create` and replaced or
  removed with `delete` on `networkpolicies`, which it already grants. A
  sandbox created before this release takes its policy at its next start.
- Under that confinement a sandbox does not reach the control plane from
  inside, so `cella` inside it cannot call the API, and a sandbox taken from
  a warm pool carries none of the proxy variables until its next start,
  because its container started before it had a gateway.
- The conformance suite holds a declared egress boundary: with `egress` in
  `-capabilities` and an `-upstream` host:port, it checks from inside a
  sandbox that the upstream is reached through the sandbox's gateway, a
  host off its allow list is refused, nothing is reached around the
  gateway, and the refusal is in the sandbox's records.
- The kind stack in `deploy/examples/kind-stubs` runs `cellad egress`, with
  the environment key `up.sh` mints at the control plane, and an upstream
  a sandbox may be allowed to reach.

## v0.5.0 - 2026-09-24

- `CELLA_BASE_PATH` serves the control plane under a path of an API
  address it shares with other services, such as `/v1/environments`. The
  path takes the place of `/v1`: `/v1/sandboxes` is answered at
  `/v1/environments/sandboxes`, and the key set, the OpenAPI document and
  `/version` move under it. Nothing is answered outside it, and the probes
  stay on the internal listener. It must be the path of
  `CELLA_PUBLIC_URL`, and the start-up line names it as `base=`. Unset,
  nothing changes. `docs/install.md` has a section on serving behind a
  shared origin.
- Every path the control plane writes is under the path of
  `CELLA_PUBLIC_URL`: the `Location` of a create or an apply, the
  `X-Forwarded-Prefix` a server inside a sandbox receives from the port
  proxy, and the paths of the served OpenAPI document. A public URL with a
  path and no `CELLA_BASE_PATH` is a control plane behind a proxy that
  rewrites the path away. A `CELLA_PUBLIC_URL` with a query or a fragment,
  or with a path that is not a plain path such as `/v1/environments`, now
  stops `cellad` at start.
- A path on the address a client is given now takes the place of `/v1`
  rather than coming before it, in the Go client, `cella`, `cellad
  worker`, `cellad egress` and the conformance suite's `-url`:
  `CELLA_URL=https://api.example.com/v1/environments` reaches
  `/v1/environments/sandboxes`. A client behind a proxy that strips a path
  of its own before the control plane names the control plane's `/v1` in
  its address, `https://example.com/cella/v1`.
- `client.ListEnvironmentKeys` reads an environment's key list: each key's
  jti, when it was minted and by whom, when it expires, and whether it was
  revoked. The token is never part of it.
- The Go client is public: `latere.ai/x/cella/client`, the one the `cella`
  command is built on. A program outside this module can now create,
  apply, list, start, stop and delete sandboxes, secrets and environments,
  run commands and sessions, move files, dial a port, mint and revoke
  environment keys, and read or follow the events feed. You pass the
  address, a token source (a fixed token, a file read on every request,
  or your own function) and, if you want, your own `http.Client`, which
  also carries the exec, attach and dial sockets. `client.Environment`
  reads `CELLA_URL` and the token the way the command does, for code
  running inside a sandbox. Manifests go in JSON or YAML. A refusal's
  whole `details` object is on the error. `docs/client.md` is the guide.
- A name or id that needs escaping in a URL path is escaped once. The
  client escaped it twice, so the server looked up a different name.
- `GET /v1/environments` now applies your authorizer's answer. The list
  used to return every environment to anyone allowed to list, ignoring the
  `filter` the `environment.list` decision carried. It now holds each
  environment to the filter's owners and labels and asks `environment.read`
  for each one, as the sandbox list does, leaving out the ones the read
  refuses. The default environment is listed whenever `environment.read` on
  it is allowed, whatever the filter names, because every subject may use
  it and no filter over owners and labels can name it. An authorizer that
  narrows `environment.list` to a tenant keeps showing the default by
  allowing that read, as it already does for a read by name.
- `GET /v1/secrets` holds each secret to the `labels` of the list
  decision's filter as well as its owners; the labels were ignored.
- A port path without its trailing slash, `/v1/sandboxes/{id}/ports/{name}`,
  now answers `307` with a relative `Location`, the port's name and a slash
  with the query kept. The redirect used to name this server's absolute
  path, which sent a client behind a proxy that serves the control plane
  under a path of its own out of that path.
- `GET /v1/environments/{id}/keys` lists the keys minted for an
  environment, oldest first and a page at a time: each key's `jti`, when it
  was minted, its `exp`, whether and when it was revoked, and who minted
  it, never the key itself. It needs the right a mint needs,
  `environment.key`. The control plane now keeps a record of each key from
  its mint until its `exp` passes; with Postgres the record is in a new
  table, `environment_keys`, which the migration at start creates. Keys
  minted by an earlier release are not listed and are still revoked by
  their `jti`.

## v0.4.0 - 2026-09-24

- The file routes accept paths in the sandbox's own workspace. A sandbox
  whose manifest set `spec.workspace.path` to something other than
  `/workspace` had every file route refused with `invalid_field`, although
  its driver keeps the files at that path.
- `unsupported_media_type` now reads "Send the body in a media type this
  route accepts." An archive upload sent with the wrong type used to be
  told to send a manifest. The detail still names the type the route
  takes.
- The API document describes each file route's parameters, bodies and
  answers, including the `204` the writes return.
- A create whose caller disconnects while the sandbox is starting is now
  recorded as `Failed` with its `sandbox.failed` record, and its identity
  token is revoked. The record and the state used to be written under the
  caller's request, so a disconnect could leave the sandbox shown as still
  being created, with no record of the failure.
- The conformance suite deletes what each case created as the case ends,
  so a run holds one case's sandboxes at a time. A server with a
  per-subject sandbox limit, or a small cluster, no longer fills up over
  the course of a run.
- A desktop on Kubernetes. Each release publishes
  `ghcr.io/<owner>/cella-display:<tag>` beside `cellad`, built for amd64
  and arm64, signed, and with its bill of materials attached to the
  release. Set `CELLA_K8S_DISPLAY_IMAGE` to it and a sandbox whose
  manifest names `display` gets a screen, screenshots and input;
  `CELLA_K8S_DISPLAY_CPU` and `CELLA_K8S_DISPLAY_MEMORY` size the desktop
  container. Unset, the environment declares no desktop, as before.
- `CELLA_K8S_DEFAULT_CPU`, `CELLA_K8S_DEFAULT_MEMORY` and
  `CELLA_K8S_DEFAULT_DISK` are checked when `cellad` starts. A value that
  is not a quantity used to start cleanly and fail every create that
  relied on it; it now stops the start with the variable named.
- On Kubernetes, a sandbox now takes a terminal. `cella attach`, the
  attach and exec sockets, and `cella exec -i` and `-t` work there as they
  do on Podman and the native runtime, where a Kubernetes environment
  answered `capability_unsupported` before. The exit code, a resize of
  your window, and the end of the session when the shell exits or you
  disconnect all carry through.
- The Role in `deploy/base` now grants `get` on `pods/exec` beside
  `create`. The control plane opens each exec over a WebSocket, which the
  API server authorizes as `get` (and from Kubernetes 1.35 as `create` as
  well), and falls back to the older SPDY upgrade when that is refused.
  An installation that wrote its own Role, for the control plane or for a
  worker that drives Kubernetes, adds `get` on `pods/exec` before it
  upgrades: `cellad` checks every access its driver uses at start-up and
  stops with the missing rule named, as `cellad check` does.
- On Kubernetes, a sandbox's declared ports are now reachable: the port
  listing, the HTTP proxy at `/v1/sandboxes/{id}/ports/{name}/`, the dial
  socket and `cella port-forward` work there as they do on the native and
  podman runtimes, where before each answered `capability_unsupported`.
  The connection goes through the API server's port forwarding, so no
  NetworkPolicy has to admit the control plane, a server that listens only
  on loopback inside the sandbox is reached too, and a port the manifest
  does not declare stays unreachable.
- Upgrading on Kubernetes: the control plane's Role needs `get` and
  `create` on `pods/portforward`. `cellad serve`, `cellad worker` on the
  Kubernetes runtime, and `cellad check` name the rule when it is missing,
  and `cellad serve` does not start without it, so add it before moving to
  this release. The Role in `deploy/base` carries it.
- `GET /v1/events` follows. With `follow=1&object=<id>&cursor=<seq>` the
  feed sends the object's records after `cursor`, the newest `seq` you
  hold, and then each new one as it happens, as newline-delimited JSON,
  with nothing sent twice and nothing skipped; without `cursor` it starts
  from now, and it ends after the object's delete. With `follow=1` and no
  `object` it sends every record you may read from now, filtered the way
  listing your sandboxes is. An idle feed writes an empty line every 15
  seconds, a feed that ends on a failure writes the error envelope as its
  last line, and a shutdown ends every feed at once rather than waiting
  out the grace period. A cursor whose records are no longer kept is 410
  `cursor_expired`, and a server holding 256 feeds refuses the next with
  429 `rate_limited`. A proxy in front of the server must not buffer the
  feed and must allow an idle read of more than 15 seconds.
- The journal's retention now keeps each object's newest record whatever
  its age. With a database, a prune that removed every record of an object
  used to start that object's `seq` again at 1, so a number a reader held
  could name a different record.
- The release pipeline reads a release's files from the release's assets
  endpoint, by id, rather than from the list the release object carries,
  which GitHub has answered empty for a release whose every file was
  attached and served.

## v0.3.1 - 2026-09-23

- `v0.3.1` carries the same code as `v0.3.0`. Both releases hold every
  file: the binaries, the deploy archive, the checksums and their
  signature. When they were published, GitHub's API answered each release
  with an empty list of files, which is what `gh release download` reads,
  so the pipeline's last check could not find them. A release now stays a
  draft until it holds every file it was built with.

## v0.3.0 - 2026-09-23

- On Kubernetes, `cellad` now asks the API server at up to 50 requests a
  second with bursts of 100, instead of the client library's default of 5
  and 10. Under load the old default queued every sandbox read behind the
  control plane's own limiter, and a short exec timeout could fail with
  `driver_unavailable` before the command ran. A platform that hands the
  driver its own client configuration keeps the rate it set there.
- A sandbox that sets `scheduling.preemptible: true` on a queued
  environment can now be stopped to make room for one of higher priority.
  When the front of a queue does not fit, the lowest priority preemptible
  sandboxes are stopped first, the largest and then the newest among
  equals, and only as many as it takes; if stopping all of them would not
  be enough, none is stopped. A preempted sandbox keeps its workspace,
  waits in its queue again at its original place with the `Scheduled`
  reason `Preempted`, and is started with its files once there is room.
  `status.preemptions` counts how often that happened, and after
  `CELLA_MAX_PREEMPTIONS` (default `3`; `0` turns preemption off) it is no
  longer stopped for anyone. `cella_preemptions_total` counts the stops,
  and each one is a `sandbox.stopped` event with the reason `Preempted`.
  The `sandbox.failed` event of a create that did not fit, or that waited
  past its `startDeadline`, now carries `NoCapacity` or `StartDeadline`
  rather than `DriverFailed`, and `cella_pool_adoptions_total` no longer
  counts a create refused for your sandbox limit or a name already taken.
  [Capacity and queues](docs/scheduling.md) has the whole of it.
- The event journal no longer grows for as long as the process or the
  database lives. Delivered and dropped records, and answered worker
  operations, older than `CELLA_JOURNAL_RETENTION` (default `720h`, at
  least `1h`) are forgotten on the reaper's tick; a record still waiting
  for your sink is kept whatever its age. Without a database the journal
  keeps the newest `CELLA_JOURNAL_CAP` records per object (default
  `1000`), and the per-object feed reads that many.
- The conformance suite holds a server to more of the contract. A sandbox
  applied without them must come back with the defaults the manifest contract
  states as fixed values: the workspace at `/workspace`, starting empty and
  used as the working directory, open egress, and no mesh or spawn rights. The
  spawn case now sends fields the schema knows and runs on every environment,
  not only one that declares a mesh. The documented command carries
  `-count=1`, so a run always asks the server instead of replaying an earlier
  pass from the build cache, and `-timeout 30m`, so a run against a cluster is
  not cut off at `go test`'s ten minute default. A new `conformance` workflow
  runs that command from GitHub Actions against an address you give it, with
  the bearer taken from the repository secret `CONFORMANCE_TOKEN`.
  [Conformance](docs/conformance.md) has both.
- `CELLA_TEST_DRIFT_DEFAULT` makes a development `cellad` resolve one spawn
  default wrongly, so a test can show the conformance suite catches a server
  that does. It takes `spec.mesh.spawn.budget` or `spec.mesh.spawn.depth`, and
  `cellad` refuses to start with it unless `CELLA_RUNTIME=native`, so an
  installation that isolates its sandboxes cannot run with it.
- A server running inside a sandbox can be reached from outside it. Declare
  the port by name under `spec.network.ports`, and
  `/v1/sandboxes/{id}/ports/{name}/` forwards any HTTP request to it under
  your bearer: every method, the path, the query, the body and a WebSocket
  upgrade, with your bearer itself never passed on. A port nothing listens
  on, or a stopped sandbox, answers `502` with the new code
  `upstream_unavailable`. `GET /v1/sandboxes/{id}/dial/{port}` is now a live
  byte stream to a port inside rather than a refusal, and
  `cella port-forward <ref> <local>:<port>` carries a loopback port on your
  machine to it, one connection at a time. The `native` and `podman`
  runtimes both provide this: `native` reaches the port on the host's own
  loopback, and `podman` publishes every declared port on `127.0.0.1` when
  the sandbox is created. An environment a self-hosted worker serves does
  not offer it yet, and its routes answer `capability_unsupported`. A
  manifest that declares ports is no longer
  served from a prewarmed pool entry, because an entry's published ports
  are fixed when it is made. Each dial session writes a `sandbox.dial`
  event with its duration and the bytes each way.
- A worker's connection no longer stalls behind a caller that stops
  reading. Each stream an operation carries on it, an exec's output, a
  terminal, an archive or a file body, now has a window of 8 MiB: the
  sender waits for room rather than queueing more, so a slow or paused
  caller holds its own stream and every other operation on that worker
  keeps moving. Before, one caller that stopped reading an exec's output
  held the worker's whole connection, and after ten seconds the
  connection dropped with every operation on it. The window is agreed
  when the worker connects, so a worker and a control plane can be
  upgraded in either order and keep working meanwhile, without the window
  until both have it. Three fixes came with it: a control plane no longer
  crashes when an operation is issued at the instant a worker's
  connection ends, a file write whose caller gave up releases the worker
  at once rather than when the connection ends, and a worker no longer
  logs a warning for every heartbeat of its control plane.
  [Self-hosting a data plane](docs/workers.md) has the whole of it.
- For whoever writes a driver: `runtime/remote` declares `Watcher` and
  `Event`, the watch of the runtime contract. A worker whose driver
  implements `Watcher` sends each change up as it happens, and the remote
  driver's own `Watch` delivers it on the control plane, with what
  `Inspect` and `List` answer already updated. A `relist` makes the
  control plane list the worker's sandboxes again before it passes the
  `relist` on, and a consumer that falls behind receives a `relist` in
  place of what it missed. No driver in this repository watches yet.
- An environment can hold what it cannot fit yet. Apply one with
  `scheduling.mode: queued`, or start `cellad` with
  `CELLA_SCHEDULING_MODE=queued`, and a create past its capacity answers
  `201` with the sandbox `Queued` instead of failing. It starts on its own
  once there is room, by priority, then by which subject uses the least cpu
  there, then by arrival. A manifest on such an environment can set
  `spec.scheduling.priority`, `.queue`, `.startDeadline` and
  `.preemptible`, and a queued sandbox reports its place in the queue on
  every read. Capacity now counts cpu, memory and disk as well as the
  number of sandboxes. A create that does not fit on a `direct`
  environment is now `201` with the sandbox `Failed` and the reason
  `NoCapacity`, where before it was refused with `quota_exceeded`, a code
  that reads as your own limit. `cella_queue_depth` and `cella_capacity`
  are on the scrape surface. [Capacity and queues](docs/scheduling.md)
  has the whole of it.
- Two pages for whoever builds on Cella rather than only runs it.
  `docs/plane.md` is how a platform sells sandboxes on top of the control
  plane: the two doors, the three endpoints you write, what each concern
  costs through either door, and a complete server under `examples/plane/`
  that composes the packages behind an API of its own and compiles on every
  push. `SECURITY.md` now carries what the project protects and what proves
  it: the four commitments, one row per asset with the control that answers
  it and the tests that hold the control to its word, and what is out of
  scope.
- The `/v1` API answers every rule its own conformance suite reads. A manifest
  may now be written in YAML and sent as `application/yaml`,
  `application/x-yaml` or `text/yaml`, and a field the schema does not know is
  refused with the path it sits at rather than only its name. `Accept` decides
  the syntax of an answer: JSON by default, YAML where you ask for it, and a
  406 where you ask for neither; `cella get -o yaml` reads that. A sandbox is
  applied by name with `PUT /v1/sandboxes/{name}`, which creates the object
  when the name is free and updates it when you already hold it, and refuses a
  body naming another object instead of quietly renaming it. `POST
  /v1/sandboxes/{id}/exec` without `?wait=1` now streams the command's output
  as it is produced and ends with its exit code, so a long run no longer waits
  for the whole answer. `GET /v1/events?object=<id>` reads any object's event
  history, newest first and paged. `GET /openapi.yaml` serves the API
  description beside the key set, with no credential, for a client generator
  to build from. A request id is `req_` and a sortable identifier, and your own
  `X-Request-Id` is carried through the answer, the events and the logs.
- Environments you apply, and sandboxes that run on them. An `Environment` is
  now an object: `PUT /v1/environments/{name}` creates or updates one,
  `GET /v1/environments` lists what a control plane holds, and `DELETE`
  removes one nothing is placed on. Every read carries an `ETag` and every
  apply may carry `If-Match`, so two administrators editing one environment
  cannot silently overwrite each other. A sandbox whose `spec.environment`
  names one of them runs there: the control plane routes every act to the
  driver that environment declares, which is the driver `cellad` opened for
  itself on its own environment and a `cellad worker`'s on yours. The
  environment cellad drives itself is written once from the `CELLA_*`
  variables that describe its driver, and your edits are what it holds
  afterwards. Each environment reports what its data plane is doing:
  `Ready` while a worker heartbeats, `Offline` with the reason
  `HeartbeatLost` once it has heard nothing for `CELLA_ENVIRONMENT_OFFLINE`,
  and `Ready` again when the worker returns. An environment below `Ready`
  takes no new sandbox and leaves the ones it holds running.
- The API is now an executable contract. `test/conformance` runs one case per
  rule the `/v1` API states against any server that claims to serve it, and
  prints what held, what failed with the request and the answer that
  disagreed, and what was skipped and why. Run it against an installation of
  your own with `go test -tags=e2e -run '^TestContract$' ./test/conformance
  -args -url <address> -token <bearer>`; it creates objects under a prefix of
  its own, deletes the ids it made and nothing else, and leaves a server
  carrying other work alone. A server that does not answer a case yet can
  declare it, with the reason, and the run stays green while the report names
  each declaration. Every push runs the suite against this server, and every
  tag runs it against the images it published. `docs/conformance.md` is the
  page for whoever runs it or writes a server of their own.
- A self-hosted data plane. `cellad worker` runs beside your own sandboxes,
  on your own engine or cluster, and connects outbound to a control plane
  somebody else operates; nothing ever dials it, so it needs no inbound
  port and no public address. It authenticates with an environment key,
  which an administrator now mints through `POST
  /v1/environments/{id}/keys` (shown once) and ends through `DELETE
  /v1/environments/{id}/keys/{jti}`; the same key is what `cellad egress`
  carries, so a gateway no longer needs one signed by hand. The worker
  checks its own driver before it registers, declares what that driver
  provides, and reconnects with a backoff when the connection drops,
  keeping the sandboxes it is already running. The `Environment` kind
  gains the full shape an operator declares, with a refusal per field, and
  `/v1/environments` serves it. [Self-hosting a data plane](docs/workers.md)
  is the walkthrough, with the Kubernetes manifest to apply.

## v0.2.1 - 2026-09-20

- A mesh works on a rootless Podman engine. A sandbox in a mesh is created in
  the bridge network namespace, which is the only one the engine attaches a
  network to; a rootless engine's default of slirp4netns or pasta refused the
  join, so every mesh create on such an engine failed. A sandbox outside any
  mesh keeps the engine's default. This is the first published release of
  the v0.2 line: the v0.2.0 tag exists, but its release pipeline stopped at
  this failure and published neither archives nor an image tag.
- `cellad` reports what it is doing. `GET /metrics` on the internal listener
  serves the Prometheus exposition: requests and their latency by route,
  sandboxes by phase, create time by driver and whether a prewarmed entry was
  adopted, reaper actions, recovery attempts, token re-mints, authorization
  and admission decisions with their latency, the event backlog and what
  delivery did with each record, store operation latency, which loop holds
  which lease, exec sessions by exit, and the boundary's connections, bytes
  and connected gateways. Labels are bounded: no series carries a sandbox id,
  a subject, a name or a path. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` also
  exports traces and logs, one span per request named after its route with
  the authorizer and admission calls as children on the same trace.
  Every log line passes one redacting handler on both destinations, so a
  secret value, a token, a credential or an egress placeholder cannot reach
  a log. The start-up line says `telemetry=otlp` or `telemetry=off`, the
  `egress` role serves no scrape surface of its own, and
  `deploy/base/prometheusrule.yaml` now carries alert rules over the metrics
  that are emitted rather than placeholders. `docs/observability.md` is the
  page.

## v0.2.0 - 2026-09-20

- A sandbox can create sandboxes. Give one spawn rights with
  `spec.mesh.spawn.budget` and `spec.mesh.spawn.depth`, and a process inside
  it applies manifests to the same `POST /v1/sandboxes` with the token it
  finds at `CELLA_TOKEN_FILE`. Every child is held to the sandbox that made
  it: its egress, its secrets, its resources and its life are a subset of its
  parent's, checked before anything is created and refused with
  `boundary_exceeded` naming the field. A child that declares no boundary of
  its own runs inside its parent's. The budget counts children created in
  total and is decremented in the same write that stores the child, so two
  children racing for the last one yield one sandbox and one
  `spawn_budget_exhausted`; the token carries what was granted at mint and
  raising that claim raises nothing. `status.parent`, `status.root` and
  `status.spawn` say where a sandbox sits in its tree, `GET
  /v1/sandboxes?root=<id>` returns the whole tree, and deleting any sandbox
  deletes the tree below it. With `spec.mesh.enabled` on the sandbox at the
  top, the tree shares one private network: on podman and Kubernetes peers
  reach each other at `<sandbox-name>.mesh`, nothing outside the tree reaches
  those ports, and traffic to a peer never leaves the environment. The native
  environment connects no peers, so it refuses `spec.mesh.enabled` and still
  spawns.

- A second binary, `cella`, speaks the API from a shell and from inside a
  sandbox: apply a Sandbox or a Secret, list and read objects, exec with or
  without a terminal, attach, follow logs, move files whole or one at a
  time, read the gateway's records, start, stop and delete. It reads
  `CELLA_URL` and `CELLA_TOKEN`, and inside a sandbox it needs neither: the
  address is injected and the token is read from `/run/cella/token` per
  request, so no flag and no login are involved. Every command answers
  columns or `--json`, a refusal is one sentence with the code behind `-v`,
  and the exit code says what happened without reading it: 2 a bad call, 3
  refused, 4 not found, 5 the wrong state, 7 unreachable, and under `exec`
  the code of the command that ran inside. It never retries. The release
  carries it as `cella_<tag>_<os>_<arch>.tar.gz` for the same four
  platforms as the server, `docs/cli.md` is the page for a person and
  `skills/cella/SKILL.md` the one an agent reads.

- `CELLA_DB_POOL_URL` names a pooled Postgres endpoint for the serving path,
  which falls back to `CELLA_DB_URL`; migrations keep the direct endpoint,
  because they hold a session-scoped lock a transaction-mode pooler drops.
  The pooled name without the direct one is a start-up failure.

- An agent that uses a computer gets one. A manifest asks for a virtual
  desktop with `spec.display: {width, height}`, and the environment runs an X
  server, a window manager and the capture and input tools beside the
  workload: a second container on Kubernetes sharing the sandbox's `/tmp`, a
  second process in the container on podman. `GET /v1/sandboxes/{id}/display`
  answers the geometry and whether the desktop is up, which
  `status.conditions` carries as `DisplayReady`;
  `GET /v1/sandboxes/{id}/screenshot` answers one frame as a PNG or a JPEG at
  a scale you choose; `GET /v1/sandboxes/{id}/screen` is a WebSocket of frames
  paced up to ten a second, where a viewer that falls behind loses the newest
  frame rather than a growing backlog; and `POST /v1/sandboxes/{id}/input`
  takes a batch of pointer and keyboard events, moves, clicks, drags, scrolls,
  keys, typed text and waits, validated whole against your own desktop before
  the first one runs, so a coordinate off the screen or a key that is not a
  key refuses the batch and changes nothing. A batch the desktop stops part
  way through answers with how much of it landed and where it stopped. A
  manifest also declares what runs inside with `spec.network.ports`, and
  `GET /v1/sandboxes/{id}/ports` and `status.ports` say which of them
  something is listening on. No screenshot, keystroke or character of typed
  text reaches an event or a log line: the records carry a frame's size, a
  batch's count and a session's duration and nothing else.

- `make run` needs no issuer of your own. It starts `cella-stubs`, a test
  binary that serves an OpenID Connect issuer, an authorization endpoint, an
  admission endpoint and an event sink on loopback, then starts `cellad serve`
  wired to them, and prints the one command that mints a token to call it
  with. `make test`, `make tier-podman` and `make tier-kind` are the tiers,
  and `deploy/examples/kind-stubs` runs the same stubs beside the control
  plane in a kind cluster, which is what lets
  [`docs/install.md`](docs/install.md) walk green on every push and on every
  tag rather than waiting for an issuer somebody has to supply.

- An environment can keep sandboxes ready, so a create that matches one
  starts in milliseconds instead of waiting for an image pull and a container
  start. `CELLA_POOL_SIZE` with `CELLA_POOL_IMAGE`, `CELLA_POOL_CPU`,
  `CELLA_POOL_MEMORY` and `CELLA_POOL_DISK` says how many to keep and what
  shape they are, with `CELLA_POOL_IN_FLIGHT` and `CELLA_POOL_GRACE` bounding
  how fast the pool fills and how long an entry is left alone before it is
  read as one to give up; nothing is kept without a size. The acceleration is
  transparent: a manifest never asks for it and never refuses it, and a
  sandbox that came from a prewarmed one is the caller's own in every respect,
  with its own boundary, its own identity, its own labels and a creation time
  of its own. `status.conditions[Scheduled]` reads `FromPool` where it
  happened and `Placed` where the sandbox was created outright. A manifest
  that names a command, a user, another image, other resources or a workspace
  of its own is created outright, because none of those can be changed under a
  container that is already running. `CELLA_SCHEDULING_MODE` accepts `direct`,
  which is what this control plane does: a create starts now or fails.

- Your own policy decides every create. Point `CELLA_ADMISSION_URL` at an
  endpoint you write, give it the bearer in `CELLA_ADMISSION_TOKEN`, and
  `cellad` sends it the defaulted manifest, the verified caller and the
  environment on every apply; it answers with the manifest to run or with
  a refusal. That is where an image catalogue pins an alias to a digest,
  where a plan fills in resources and lifetimes and refuses what exceeds
  them, and where a boundary is narrowed before anything starts. What the
  endpoint returns is what the caller reads back and what the sandbox
  runs, and it is validated the same way a caller's manifest is, so an
  endpoint cannot write a field the schema does not have. A refusal is a
  422 carrying your endpoint's own reason; an endpoint that times out,
  answers anything else, or cannot be reached fails the create with a 503
  and never lets one through, and nothing is ever resent. Leave the URL
  unset and the step is the identity, as before. `spec.image` moves with
  it: it is now required after your endpoint has run rather than before,
  so a catalogue can supply it, and `CELLA_DEFAULT_IMAGE` fills it in on
  an installation with no endpoint. A manifest that names no image after
  both is refused with `missing_field`, and an environment that runs no
  image, such as the native one, still refuses an image whoever named it.

- A `v*` tag is a release. It publishes `ghcr.io/<owner>/cellad:<tag>`, a
  multi-architecture image built from the binary the pipeline compiled, signed
  with cosign and carrying an SPDX bill of materials and a build-provenance
  attestation; `cellad_<tag>_<os>_<arch>.tar.gz` for linux and darwin on amd64
  and arm64, with `checksums.txt` and a signature over it; and
  `deploy-<tag>.tar.gz`, the kustomize tree with that image pinned by digest.
  The namespace is the account that pushed the tag, so a fork's tag publishes
  under the fork. The release page's body is the CHANGELOG section for the
  tag, and a clean runner verifies every signature, checksum and attestation
  after it is published.

- `deploy/` is the installation: a base with the control plane's Deployment,
  its Service, the account and the Role the Kubernetes driver needs and
  nothing more, a network policy, a disruption budget and a check Job;
  `deploy/bootstrap` with the namespace and one Secret per dependency; and two
  overlays, a laptop cluster and a cluster an installation runs on. The base
  pins no namespace and no image registry. [`docs/install.md`](docs/install.md)
  walks it from an empty cluster to a running sandbox.

- `cellad check` reads the whole configuration and prints one line per
  requirement of an installation, exiting 1 on any failure: the configuration
  loads, every issuer answers and the signing key parses, the authorization
  endpoint denies the reserved probe, the backend answers with the accesses
  the driver uses, and the data directory is writable. Each optional
  dependency is answered for when its variable is set and reported as not
  configured otherwise. It is the start-up path of `cellad serve` asked one
  question at a time, so an installation that passes it is one that would have
  started. `cellad version` prints the same build identity as `cellad
  -version`, for a Job that is one image and one argument.

- A sandbox reaches the services it needs and holds none of their
  credentials. The new `Secret` kind at `/v1/secrets` takes a value, the hosts
  that value may be sent to, and where in a request it goes: a header, a query
  parameter, verbatim or base64 of a `user:pass` pair, or an OAuth 2.0
  client-credentials grant the gateway mints a token from. The value is
  write-only: it is accepted on apply and absent from every read, every list,
  every event and every log line, and it is stored under envelope encryption
  with the key in `CELLA_SECRET_KEY`. A manifest mounts one with
  `spec.secrets: [{name, env}]`, and what the sandbox's environment carries is
  an opaque placeholder, plus `<env>_HEADER` or `<env>_QUERY` where the secret
  injects somewhere a client would not look by itself. The gateway swaps the
  placeholder for the value on the way out, toward a host the secret's own
  owner named and nowhere else: sent anywhere else it leaves verbatim, so an
  exfiltration attempt carries an opaque token. Two mounted secrets that apply
  to one host are refused rather than silently collapsed, and a mount with no
  allowed host of its own puts the secret's scope on the sandbox's allow list.
  Rotating a value reaches a running sandbox's next request without restarting
  it; deleting the secret marks the sandbox `notInjectable` and its next
  request leaves unauthenticated. `status.secrets` says which placeholders are
  mounted and which of them the gateway will not substitute.

- Every sandbox carries an identity of its own. The control plane mints a
  workload token at create, the driver projects it read-only at
  `/run/cella/token` and names the path in `CELLA_TOKEN_FILE`, and a process
  inside the sandbox calls `/v1` back with it: it reads and execs the sandbox
  it belongs to and reaches nothing else. The token expires with its sandbox
  or a day after it was minted, whichever is sooner, and the control plane
  re-mints and re-projects it once two thirds of that has passed, without
  restarting the workload. A token that is replaced, and one whose sandbox is
  deleted, is refused from that moment rather than when it expires. Any
  service can verify one offline against `/.well-known/jwks.json`.

- A sandbox's workspace answers one file at a time, not only whole archives.
  `GET /v1/sandboxes/{id}/files/content?path=` streams one file,
  `.../files/stat?path=` describes it, `.../files/list?path=` lists a
  directory sorted by name, `PUT /v1/sandboxes/{id}/files?path=[&mode=]`
  writes one file of any content type, `DELETE .../files?path=` removes a file
  or a tree, and `POST .../files/mkdir` and `POST .../files/move` make a
  directory and rename one path onto another. A write is staged and renamed,
  so a body that ends early or passes `CELLA_MAX_UPLOAD_BYTES` leaves the
  previous file as it was, and a move takes the exact destination rather than
  nesting the source inside an existing directory. Every path is absolute and
  inside the workspace: one that leaves it, by traversal or through a symbolic
  link the workload planted, is refused. Names arrive as they are, spaces,
  unicode, percent signs, pipes and newlines included. The native, podman and
  k8s drivers all serve the routes; the podman driver needs the sandbox
  running and answers `phase_conflict` while it is stopped.

- Every change to a sandbox and every operation on one produces a signed
  record, delivered to the endpoint `CELLA_EVENTS_URL` names. A record says
  who did what to which object, when, and why, with the object's labels and a
  sequence per object; it never carries a command line, a file's content, an
  environment or secret value, or a token. `CELLA_EVENTS_SECRET` holds one
  secret or two separated by a comma, and each signs the delivery, so a secret
  is rotated without losing a record. Delivery is at least once and in order
  per object: a sink that fails is retried with a growing wait for
  `CELLA_EVENTS_RETRY_WINDOW` (default `24h`), a sink that refuses a record
  ends it, and a sink that rejects the signature holds it until the two ends
  agree again. A record commits with the change it describes, so a sandbox
  that started has a record that says so. With the URL unset nothing is
  delivered and the journal still holds the history.

- A manifest declares the network boundary its sandbox lives inside, and
  `cellad egress` enforces it. `spec.network.egress` takes a `mode` of `none`,
  `allowlist` or `open`, with `allowedHosts` for the first and `deniedHosts`
  for the last, each an exact name or one leading `*.` wildcard; the mode is
  inferred from whichever list is set. A sandbox may narrow its own boundary
  and never widen it. At create, the control plane compiles the boundary into
  the map its gateway holds and waits for a gateway to acknowledge it before
  the sandbox exists, so nothing runs outside the boundary it declared; a
  boundary of `open` with no denied host needs no gateway. The gateway is the
  new `cellad egress` role: it connects outbound to `CELLA_URL` with
  `CELLA_ENVIRONMENT_KEY`, receives every map of its environment and every
  change after, and serves two doors, a CONNECT proxy on
  `CELLA_EGRESS_PROXY_ADDR` and a reverse door on `CELLA_EGRESS_REVERSE_ADDR`,
  each authenticated by the sandbox's own credential. Every connection is
  decided before any dial and reported back as one record, which
  `GET /v1/sandboxes/{id}/egress` serves newest first. The native and podman
  drivers point the workload at the gateway through the proxy and trust
  variables and project the gateway's authority into the sandbox. Secrets and
  their values are not in this release: a boundary is enforced, and nothing is
  substituted yet.

- A terminal inside a sandbox. `GET /v1/sandboxes/{id}/attach` is a WebSocket
  carrying a shell: the first text frame says what to run and how big the
  window is, binary frames are bytes both ways, `{"resize":{"cols","rows"}}`
  changes the window, and the session ends with `{"exit": n}` and a normal
  close. `GET /v1/sandboxes/{id}/exec` is the same socket for one command: give
  it a column and a row count for a terminal, or neither to write the command's
  input and read its output. Both need the environment to provide a terminal
  and answer `capability_unsupported` where it does not. The native runtime
  opens a real pseudo-terminal on Linux and macOS, and the podman runtime runs
  the session in the container; `POST /v1/sandboxes/{id}/exec?wait=1` is
  unchanged. Closing the connection ends the process on the native runtime; on
  podman the engine keeps the session until the sandbox stops.

- `CELLA_RUNTIME=k8s` runs each sandbox on a Kubernetes cluster: one
  PersistentVolumeClaim and one Pod per sandbox, where a stop deletes the Pod
  and keeps the claim, and a start renders a new Pod against it. Every Pod runs
  as a non-root uid with a read-only root file system, dropped capabilities, the
  default seccomp profile, no privilege escalation, no host namespaces and no
  service account token, with the workspace and `/tmp` as the only writable
  mounts. Identity, the deadlines and the activity stamp live on the two
  objects, so listing a namespace is enough to see every sandbox after a
  restart. Exec, logs and archive transfer go through the cluster, and a
  transfer into a stopped sandbox runs in a short-lived helper Pod. The cluster
  is named by `CELLA_K8S_KUBECONFIG` (empty means in-cluster) and
  `CELLA_K8S_NAMESPACE` (default `cella`); `CELLA_K8S_STORAGE_CLASS`,
  `CELLA_K8S_NODE_SELECTOR`, `CELLA_K8S_TOLERATIONS`,
  `CELLA_K8S_IMAGE_PULL_SECRETS`, `CELLA_K8S_RUN_AS_USER`,
  `CELLA_K8S_RUN_AS_GROUP`, `CELLA_K8S_CPU_REQUEST_RATIO`,
  `CELLA_K8S_MEMORY_REQUEST_RATIO`, `CELLA_K8S_DEFAULT_CPU`,
  `CELLA_K8S_DEFAULT_MEMORY`, `CELLA_K8S_DEFAULT_DISK`,
  `CELLA_K8S_READY_TIMEOUT` and `CELLA_K8S_GRACE_PERIOD` are what one
  installation supplies. Egress rules, the mesh, attach, ports, display and a
  warm pool are not on this driver yet.
- Desired state can live in Postgres. `CELLA_DB_URL` selects it, bounded by
  `CELLA_DB_MAX_CONNS` (default `4`), with the schema applied at start-up and
  a schema this binary does not know refused rather than served. With it, a
  sandbox the runtime lost is recreated from its desired state with the same
  id, name, labels and lifecycle; without it, a lost sandbox is reported and
  deleted after `CELLA_LOST_GRACE` (default `10m`), which the start-up line
  says out loud. `CELLA_SECRET_KEY` is read where it is set: 32 bytes, base64,
  the key every secret value's data key is sealed under. Several `cellad`
  replicas on one database take one writer's lease each.
- `CELLA_RUNTIME=podman` runs each sandbox as a container on a podman engine,
  reached over the libpod API at `CELLA_PODMAN_SOCKET`. Unset, the socket is
  the rootless one and then the system one, and a start-up with neither
  answering says which it tried. A sandbox is a container with its workspace on
  a named volume at `workspace.path`, `resources.cpu` and `resources.memory` as
  cgroup limits, `user`, `env` and the command as written, and its identity and
  lifecycle stamped on the engine's own objects, so `cellad` reads every
  sandbox back after a restart. Exec, logs and file transfer work, and file
  transfer works while the sandbox is stopped. `resources.disk` is recorded and
  not enforced, and attach, a desktop, egress rules and a mesh are not in this
  backend yet.

- `cellad` enforces the lifecycle its runtime reports. A sandbox past its
  expiry is deleted, one stopped for longer than its auto-delete window is
  deleted, and one idle for longer than its auto-stop window is stopped, each
  with the reason in `status.reason`. The exec and file routes stamp activity,
  coalesced per sandbox so a busy session does not write the substrate per
  request. `CELLA_REAP_INTERVAL` (default `30s`) sets how often the rules run
  and `CELLA_TOUCH_INTERVAL` (default `1m`) how often one sandbox's activity
  reaches the runtime. A sandbox with no deadline is never ended.

- A manifest carries the fields that decide what a sandbox gets and how long
  it lives: `spec.user`, `spec.resources` with `cpu`, `memory` and `disk`,
  `spec.workspace.path`, and `spec.lifecycle` with `ttl`, `autoStop` and
  `autoDelete`, each a duration or `never`. A resolved manifest is returned in
  the caller's own spelling, with `status.expiresAt` and a warning for every
  field the environment records and does not enforce. The operator's defaults
  and ceilings apply in one staged resolve, so a manifest above a ceiling is
  refused with `ceiling_exceeded` and the path that exceeded it, and a field
  the contract fixes is refused with `immutable_field`.

- `runtime/runtimetest` is the conformance suite a driver passes: `Run` drives
  every `Driver` method, skips an operation the driver does not declare, and
  fails one it declares but refuses. `Nop` is a driver for embedders' fakes.
  `runtime/native` passes it. The tree refuses Latere coordinates under
  `runtime/` and the exported packages are checked to import no client.
- Native workloads can start a main command, report its exit, restart it, and
  stream timestamp-filtered or tailed logs. File routes import and export tar
  archives with authorization, upload bounds, and path containment.
- The first native workspace API is executable: create, list, inspect, execute,
  stop, start, and delete through authenticated `/v1/sandboxes` routes. The
  controller preserves ownership and desired objects across a server restart.
  This is a single-node development store; replicated state and recovery remain
  unimplemented.
- `runtime/native` provides process-group cancellation, durable workspace
  metadata, and confined archive paths. Native execution has no isolation and
  now requires `CELLA_ALLOW_UNSAFE_NATIVE=true`. Unimplemented runtime selections
  fail startup. `make run` explicitly opts into trusted native execution.
- JSON request bodies default to 64 KiB and tar uploads to 1 GiB, configurable
  with `CELLA_MAX_BODY_BYTES` and `CELLA_MAX_UPLOAD_BYTES`.

- `CELLA_OIDC_AUDIENCE` accepts a comma-separated set for external callers.
  The first entry remains the audience of locally signed workload and
  environment tokens. Empty and repeated entries fail startup.

## v0.1.0 - 2026-09-17

- A personal access key can now be narrower than the person who holds
  it, and `cellad` holds it to that. When you create a key you choose
  what it may do: read these two sandboxes, run anything in this
  environment, and nothing else. Those grants ride on every token the
  key mints, and `cellad` applies them on top of the answer its
  authorizer already gave. A key narrowed to some sandboxes is refused
  everywhere else, and the refusal names the reason `grant`.

  A grant only ever takes away. It cannot reach a sandbox you could not
  reach yourself, because the authorizer answers first and the grants
  narrow that answer: a key on an object you do not own reaches nothing,
  and a key can never do more than you can. A token that is not from a
  personal access key is unaffected, whatever it carries.

  Two things to know before you point a narrowed key at `cellad`. A key
  that carries no grants at all is refused everywhere: the grants are
  what a key may do, so a key that says nothing may do nothing. And a
  narrowed key reaching a service that does not read grants is refused
  at the door with a 401 rather than quietly given the person's full
  reach; `cellad` reads them, so a key narrowed for Cella works against
  Cella.

  **If you run your own authorizer endpoint, read this one.** From this
  version a narrowed key reaches `cellad`, and `cellad` asks your
  endpoint what the caller may do. An endpoint written on
  `latere.ai/x/pkg/authz/server` applies the grants for you: bump the
  package to v0.75.0 or later and there is nothing else to do. An
  endpoint written by hand applies them itself, `authz.Restrict` over
  its own answer. An endpoint that does neither answers about the person
  and not about the key, and gives a narrowed key the person's full
  reach. Point `CELLA_TEST_AUTHORIZER_URL` at your endpoint and run the
  conformance suite to see which one yours is. The built-in owner
  policy, which is what you run with `CELLA_AUTHORIZER_URL` unset,
  applies them already.

- `cellad` reads every issuer's keys into its verifier at start, so the
  first request after a start waits for no fetch. It already read them
  to check the issuer; now it keeps them. An issuer that answers that
  check and then stops answering refuses the start, naming
  `CELLA_OIDC_ISSUERS`, which is the same rule as before: at start every
  issuer answers, or `cellad` does not start.

- `latere.ai/x/cella/authorizer` publishes a heading for each resource
  kind beside the table, read with `Vocabulary().Label(kind)`:
  Sandboxes, Secrets, Volumes, Sandbox sets, Environments. Anything that
  offers a person a choice of what a key may do groups the actions by
  kind and takes the heading from here, rather than spelling
  `SandboxSet` at somebody.

- A caller's token is refused once it is more than 24 hours old, even
  when its `exp` is still in the future, so a caller that works for days
  re-mints at its own issuer instead of holding one token. A token that
  does not say when it was minted, carrying no `iat`, is refused for the
  same reason. This is the same rule Origo and Lux apply, one age bound
  across the three. The keys `cellad` mints for environments are
  unaffected: one lives `CELLA_ENVIRONMENT_KEY_TTL`, a year by default,
  and its `exp` is its only bound.
- `cellad` knows who is calling. `CELLA_OIDC_ISSUERS` lists the OpenID
  Connect issuers you trust, any of them; at start `cellad` reads each
  one's discovery document and key set and refuses to start when one does
  not answer, names another issuer, or publishes no `RS256` or `ES256`
  key. A bearer is accepted when a listed issuer signed it, its `aud`
  contains `CELLA_OIDC_AUDIENCE` (default `cella`), it has not expired,
  and it was minted less than 24 hours ago; nothing else about it is
  interpreted. A subject is the issuer and the `sub` joined,
  `https://login.example.com|alice`, so two issuers that agree on a `sub`
  are two subjects, and every claim of the token reaches your authorizer
  verbatim. There is no anonymous access and no
  API key.
- `cellad` signs the identities it hands out. `CELLA_TOKEN_KEY` is one or
  two PEM RSA private keys of at least 2048 bits: the first signs a
  sandbox's token and an environment's key, every one is published at
  `/.well-known/jwks.json`, and each carries its RFC 7638 thumbprint as
  its `kid`, so anything verifies a sandbox's identity offline with a
  stock JWT library. Rotation is yours and lives in the configuration:
  put a new key in front, and remove the old block once the tokens it
  signed have expired. `CELLA_PUBLIC_URL` is now required, because it is
  the issuer those tokens name.
- `cellad` asks somebody else what a caller may do.
  `CELLA_AUTHORIZER_URL` and `CELLA_AUTHORIZER_TOKEN` point at the
  endpoint you write, with `CELLA_AUTHORIZER_TIMEOUT` (`5s`) bounding one
  call and `CELLA_AUTHORIZER_CACHE` (`60s`) holding an allow whose answer
  names no `ttl`. An allow may carry the three ceilings and, on a list, a
  filter. Anything that is not a decision fails the request and never
  allows it, and a connection that died before a response line is retried
  once and nothing else is. With the URL unset, the built-in owner policy
  decides from `CELLA_ADMIN_SUBJECTS` and the owner of each object: you
  create anything but an environment, you act on what you own, a list
  returns your own, admins act on everything and alone make environments,
  and a sandbox's own identity reads and execs itself, reads the tree
  below it, creates a child and nothing else.
- Running `cellad` therefore needs three variables it did not need
  before, `CELLA_OIDC_ISSUERS`, `CELLA_PUBLIC_URL` and `CELLA_TOKEN_KEY`,
  and `make run` needs an issuer named on the command line until the test
  stubs land. The README has the whole of it.

- The vocabulary an authorization endpoint for Cella speaks is an
  importable package, `latere.ai/x/cella/authorizer`. Write the endpoint
  `CELLA_AUTHORIZER_URL` points at in Go and take the thirty-two actions
  of the identity spec's table as constants, the resource kind each one
  acts on from `Kind`, the whole table from `Vocabulary()`, and the three
  ceilings an allow may carry from `WireLimits`, rather than keeping a
  copy of the strings. The envelope on the wire is
  `latere.ai/x/pkg/authz`'s, and so are the stub and the conformance
  suite an endpoint passes; an endpoint built on
  `latere.ai/x/pkg/authz/server` with nothing behind it but a decider
  passes that suite with this vocabulary. `cellad` asks no authorizer
  yet: the configuration, the client and the guard are the identity
  spec's and are not built.

- The repository: the `cellad` binary serving its probes on two listeners,
  typed configuration from `CELLA_*` variables, the quality gate, and the
  design specs. Nothing creates a sandbox yet; the specs say what will.
- The design, revised after review: Cella is a control plane whose data
  plane is a driver in-process or a worker on your own infrastructure;
  the API group is `cella.latere.ai/v1`; five kinds (`Sandbox`, `Secret`,
  `Volume`, `SandboxSet`, `Environment`); an egress gateway that keeps
  secret values out of sandboxes; volumes for persistent state; mesh and
  spawn with a boundary a child cannot widen; three scheduling
  strategies and sets for rollouts; desired state that recovers a lost
  sandbox; six drivers across four isolation classes.
