# Manifests

Every field of the four kinds this release serves, what it means, and
what it takes when you leave it out. A manifest is JSON or YAML, one
object per request, and always carries

```json
{"apiVersion": "cella.latere.ai/v1beta1", "kind": "Sandbox", "metadata": {...}, "spec": {...}}
```

The server decodes strictly: a field it does not know is refused with its
path, and so is an `apiVersion` other than `cella.latere.ai/v1beta1` or a
kind it does not serve. What you read back is the manifest as it runs, with
every default written in, and a `status` the server owns. A `status` you
send is ignored.

A manifest is resolved in one pass: the server fills defaults, runs your
admission endpoint if one is configured, checks ceilings, and checks every
field against what the target environment can do. A field the environment
cannot honor is refused with `capability_unsupported` and its path; a
field it records but does not enforce is accepted with a warning in the
answer.

## Metadata

The same three fields on every kind.

| Field | Rule |
|---|---|
| `name` | a DNS label of at most 63 characters: lower-case letters, digits, and hyphens. A sandbox created without one is given one. A name is unique among its owner's objects of that kind |
| `labels` | keys are label names, optionally prefixed by a DNS subdomain and a slash; values are at most 63 characters. Keys under `cella.latere.ai/` are the control plane's own and refused |
| `annotations` | the same keys; each value at most 4 KiB and all of them at most 64 KiB |

## Sandbox

```json
{
  "apiVersion": "cella.latere.ai/v1beta1",
  "kind": "Sandbox",
  "metadata": {"name": "research", "labels": {"team": "research"}},
  "spec": {
    "image": "registry.example/agent:2",
    "command": ["/bin/sh", "-c"],
    "args": ["sleep 3600"],
    "resources": {"cpu": "2", "memory": "4Gi", "disk": "20Gi"},
    "env": {"MODEL": "small"},
    "secrets": [{"name": "vendor", "env": "VENDOR_TOKEN"}],
    "network": {
      "egress": {"allowedHosts": ["api.vendor.example", "*.docs.example"]},
      "ports": [{"name": "http", "port": 8080}]
    },
    "mesh": {"enabled": true, "spawn": {"budget": 4, "depth": 2}},
    "lifecycle": {"autoStop": "30m", "ttl": "4h", "autoDelete": "24h"},
    "display": {"width": 1280, "height": 720}
  }
}
```

### What runs

| Field | Default | Meaning |
|---|---|---|
| `spec.environment` | `default`, the control plane's own | the environment the sandbox runs on. Asks `environment.use` of your authorizer |
| `spec.image` | `CELLA_DEFAULT_IMAGE`, or what admission supplies | the image to run. Required on an environment that runs images, and refused on the native one, which runs host processes |
| `spec.command` | the image's entrypoint | the main process. On the native runtime, a sandbox with no command runs nothing until you exec |
| `spec.args` | the image's arguments | arguments to the command |
| `spec.workdir` | the workspace path | the working directory, a clean absolute path |
| `spec.user` | the image's user, or `CELLA_K8S_RUN_AS_USER` on Kubernetes | a uid, a `uid:gid` pair, or a user name. The native runtime runs as the server's own user and warns |
| `spec.env` | none | environment variables, POSIX names, at most 32 KiB in total. A name starting with `CELLA_`, and the proxy and certificate variables the gateway sets, are refused |

### Resources and the workspace

| Field | Default | Meaning |
|---|---|---|
| `spec.resources.cpu` | no limit; `CELLA_K8S_DEFAULT_CPU` on Kubernetes | a cpu limit, such as `500m` or `2` |
| `spec.resources.memory` | no limit; `CELLA_K8S_DEFAULT_MEMORY` on Kubernetes | a memory limit, such as `4Gi` |
| `spec.resources.disk` | no limit; `CELLA_K8S_DEFAULT_DISK` on Kubernetes | the workspace's size, which is the claim's size on Kubernetes. Recorded and not enforced on Podman |
| `spec.resources.max.cpu`, `.memory`, `.disk` | none: the sandbox keeps the size above | a ceiling for that resource. The sandbox starts at the amount above and is [sized inside the ceiling](#sizing-inside-a-ceiling) |
| `spec.workspace.path` | `/workspace` | where the workspace is mounted, a clean absolute path not under `/run/cella`. The native runtime keeps it at `/workspace` |
| `spec.workspace.source` | `empty` | what the workspace starts with. This release fills it from `empty` only |
| `spec.workspace.host` | none | a folder on the machine a worker runs on, bound in place as the workspace: never copied, and never deleted with the sandbox. It must be inside a folder every live worker of the environment grants, or the manifest is refused with `workspace_not_granted`, and only an environment whose driver binds host folders takes it. It takes no `source`, and the workspace path and the working directory become the folder itself ([what the host decides](workers.md#what-the-host-decides)) |

The native runtime records resources and does not limit them, with a
warning. The workspace survives a stop and a start, and is removed with
the sandbox.

### Secrets

| Field | Meaning |
|---|---|
| `spec.secrets[].name` | a `Secret` you may mount. Asks `secret.mount` of your authorizer; one you may not use answers `not_found` |
| `spec.secrets[].env` | the variable the placeholder is put in |

The sandbox holds a placeholder, never the value. The gateway swaps the
placeholder for the value on a request to a host the secret's scope
names, in the header or query parameter the secret names; anywhere else
it leaves verbatim. Where the secret goes in a header other than
`Authorization`, or in a query parameter, the sandbox also gets
`<env>_HEADER` or `<env>_QUERY` naming where to put it. Two mounted
secrets that apply to the same host are refused. A sandbox that mounts a
secret and declares no egress rule gets `allowlist`, with the secret's
hosts on it.

### Volumes

| Field | Meaning |
|---|---|
| `spec.volumes[].name` | the mount's own name, a DNS label unique in the list |
| `spec.volumes[].path` | where the volume appears, a clean absolute path. Two mounts may not share a path or sit one inside the other, and no mount may be at or inside the workspace or under `/run/cella`: `path_conflict` |
| `spec.volumes[].volume` | a [`Volume`](#volume) by name among your own, or by `vol_` id. Asks `volume.attach` of your authorizer; one you may not use answers `not_found`. It must live in the sandbox's environment |
| `spec.volumes[].readOnly` | `false` by default. A `shared-read` volume is mounted read-only only |

A volume still being filled is accepted, and the sandbox waits in
`Pending` until the fill is done; one whose fill failed makes the
sandbox fail with `VolumesAttached` `False`. A `single` volume takes one
read-write mount or any number of read-only ones, never both: a mount
the volume's attachments exclude is `volume_busy`. The mounts are fixed
when the sandbox is created. A spawned child mounts only volumes its
parent mounts, and read-only where its parent's mount is.

### Network

| Field | Default | Meaning |
|---|---|---|
| `spec.network.egress.mode` | inferred | `none`, `allowlist`, or `open`. Inferred from whichever list is set: `allowlist` from `allowedHosts`, `open` from `deniedHosts`, `allowlist` when a secret is mounted, and `open` otherwise |
| `spec.network.egress.allowedHosts` | none | with `allowlist`, the hosts the sandbox may reach: exact names, or one leading `*.` wildcard. Not addresses |
| `spec.network.egress.deniedHosts` | none | with `open`, the hosts it may not reach |
| `spec.network.ports[].name` | required | a DNS label, unique in the list, that the port proxy addresses the port by |
| `spec.network.ports[].port` | required | the port inside the sandbox, 1 to 65535, unique in the list |
| `spec.network.ports[].expose` | `none` | `none` is reachable through the control plane's own routes alone; `mesh` also from the sandbox's mesh peers; `public` is refused in this release |

A boundary other than `open` with no denied host needs a gateway in the
environment, and the create waits for the gateway to acknowledge it. The
Kubernetes runtime, once its operator names the gateway's Pods, lets a
sandbox reach the network only through the gateway, and `status.conditions`
reports `EgressEnforced` true ([Sandboxes on Kubernetes](kubernetes.md#what-a-sandbox-can-reach)).
Every other runtime records such a boundary without confining the workload
to it: `EgressEnforced` is false, and the answer carries a warning. A
sandbox may narrow its own boundary later and never widen it.

### Spawn and mesh

| Field | Default | Meaning |
|---|---|---|
| `spec.mesh.spawn.budget` | `0` | how many sandboxes this one may create, in total, through its own workload token |
| `spec.mesh.spawn.depth` | `0` | how many generations below it may exist |
| `spec.mesh.enabled` | `false` | on a sandbox a subject created, joins its whole tree to one private network, where peers reach each other at `<sandbox-name>.mesh`. A child inherits it and may not set it. Refused on the native runtime |

Every child is a subset of its parent: its egress, its secrets, its
volumes, its resources and their ceilings, and its lifetime. A child
that asks for more is refused with `boundary_exceeded` naming the
field, and one that declares no boundary runs inside its parent's. An
update to the parent is held to the same rule: one that would narrow it
below what a live child already holds is refused the same way, naming
the child. Its budget and depth are at most the parent's remainder less
one. Deleting a sandbox deletes the tree below it.

### Lifecycle

Each is a duration such as `15m` or `24h`, or `never`. With none set, a
sandbox runs until you stop or delete it.

| Field | Meaning |
|---|---|
| `spec.lifecycle.autoStop` | stop the sandbox after this much time without activity. Commands, file operations, and sessions count as activity; a command counts for as long as it runs |
| `spec.lifecycle.ttl` | delete it this long after it was created. A child's defaults to what is left of its parent's, and cannot exceed it |
| `spec.lifecycle.autoDelete` | delete it this long after it stopped |

`cellad` holds these and the resources to no ceiling of its own. An
installation that caps them does so in its admission endpoint, which
refuses with `admission_refused` and its own reason, and a platform built
on the packages passes ceilings to the resolver, which refuses with
`ceiling_exceeded` naming the field.

### Scheduling

Read only on an environment in the `queued` mode, and refused on a
`direct` one. [Capacity and queues](scheduling.md) is how a queue orders
what waits.

| Field | Default | Meaning |
|---|---|---|
| `spec.scheduling.priority` | `0` | higher starts first |
| `spec.scheduling.queue` | the environment's default queue | the queue it waits in, one the environment declares |
| `spec.scheduling.startDeadline` | none | fail the sandbox with `StartDeadline` if it has not started within this long |
| `spec.scheduling.preemptible` | `false` | may be stopped to make room for a sandbox of higher priority, and then waits again in its place |

### Display

| Field | Meaning |
|---|---|
| `spec.display.width`, `spec.display.height` | a virtual desktop of this size, both or neither: 320 to 7680 pixels wide and 240 to 4320 high. Fixed for the sandbox's life. Refused on an environment with no desktop |

With a display the sandbox gets `DISPLAY=:0`, and the screenshot, screen,
and input routes answer for it.

### Sizing inside a ceiling

A sandbox normally keeps the size it was created with. Name a ceiling and
it starts small and takes more only when its work needs it:

```json
"resources": {
  "cpu": "500m", "memory": "1Gi",
  "max": {"cpu": "2", "memory": "6Gi"}
}
```

Each ceiling is optional and stands for its own resource. A resource with
no ceiling keeps its size, and a manifest with no `max` behaves exactly as
it did before.

- **CPU.** The sandbox may use up to `max.cpu` whenever the node has it
  idle, and is guaranteed only the share of `cpu` it would have had
  anyway. Nothing is reserved for the burst, so a package install or a
  build runs at the ceiling while it lasts and costs nothing after.
- **Memory.** The sandbox starts with `memory`. It is given more, one step
  at a time and never past `max.memory`, when its workload has been using
  85% or more of what it has at 2 readings in a row, or when the kernel
  kills one of its processes for memory. A step takes it to 1.5 times
  what it has, or to 512Mi more where that is larger, rounded up to a
  multiple of 256Mi. Memory is never taken back while the sandbox runs,
  and a sandbox that is stopped and started again starts with what it had
  reached.
- **Disk.** No environment grows a workspace yet, so `max.disk` is
  refused.

A raise normally happens in place, with every process still running. Where
the node has no room for the larger size, the sandbox is restarted on a
node that has it: the workspace is kept and the processes start again.
That restart waits until no command, terminal, port connection or file
transfer is open, so it never cuts one of yours. The exception is a kill
that ended the whole sandbox, where there is nothing left to wait for.

If a command of yours ends with exit code 137, the kernel may have killed
it for memory. The sandbox is checked before that exit reaches you, and
given more where that is why, so the same command can be run again. The
[events](events.md) of the sandbox say what happened: `sandbox.resized`
with `reason` `oom`.

#### What a client can rely on

After a command is killed for memory in a sandbox with room left under
`max.memory`, read the sandbox. It is in one of three states, and in each
you wait and run the command again. You never need to delete the sandbox
and create another.

| The sandbox reads | What happened | What to do |
|---|---|---|
| `Running` | one process was killed and the rest of the sandbox went on. Its memory is being raised in place | run the command again |
| `Starting` with reason `MemoryRaised` | the kill took the whole sandbox, or the node had no room for the raise, and the control plane is starting it again at the larger size | read again until it is `Running`, then run the command again |
| `Starting` with reason `MemoryNotRaised` | the kill took the whole sandbox and no node had room for a larger size a moment ago, so the control plane is starting it again at the size it holds | read again until it is `Running`. The sandbox is yours to use again, but the command needs more memory than it can be given for now, so expect the same command to be killed again |

A sandbox on its way back never reads `Failed`, `Stopped` or not found, at
any moment between the kill and running again: not by a read of the
sandbox, not in a list, and not in its events, which carry no
`sandbox.failed` for it. The wait is a few seconds. `Starting` with either
reason is a phase the control plane is already acting on, so a start or a
stop of the sandbox in that time is refused with `phase_conflict`, and so
is a command, which needs a running sandbox.

Where no node has room for the larger size, the sandbox carries the
`MemoryUnplaced` condition, whose message names the size that found no
room. It is kept running at the size it holds: a kill is answered by a
start at that size, at once, and more memory is asked for again later
([Sandboxes on Kubernetes](kubernetes.md#sizing-a-sandbox)). That is done
3 times in a row. A sandbox killed for memory again after the last of
them reads `Failed` with reason `OOMKilled`, and its condition says
`RestartsExhausted`: its command does not fit in the memory it can have,
and whether to [start it again](#recovering-a-failed-sandbox) is yours to
decide. Its workspace is intact.

What a restart costs is what any restart costs:

- The workspace is kept, every file of it.
- Every process is gone, not only the command that was killed: a server or
  a watcher your client started in the background has to be started again.
- Files outside the workspace are gone, such as `/tmp` and anything written
  into the image's own directories, as after a stop and a start.

`Failed` means what it always meant: nothing will start the sandbox again
until you act. A sandbox killed for memory **at** its ceiling reads
`Failed` with reason `OOMKilled`, carries the `MemoryAtCeiling` condition,
and has `sandbox.at_ceiling` in its events. Its workspace is still there,
and either of two acts brings it back with every file:

- [Start it](#recovering-a-failed-sandbox). It runs again at the size it
  holds, which is its ceiling.
- Raise `spec.resources.max.memory` with an update. The sandbox has room
  again, reads `Starting` with reason `MemoryRaised` from that update on,
  and is started at the next size.

| Refusal | When |
|---|---|
| `invalid_field` at `spec.resources.max.<resource>` | the ceiling is below the amount it bounds, or is not a quantity |
| `ceiling_exceeded` at `spec.resources.max.<resource>` | the ceiling is above what this server allows, or an update puts memory's ceiling below what the sandbox already holds |
| `capability_unsupported` at `spec.resources.max.<resource>` | the sandbox's environment does not size that resource. `GET /v1/environments` shows what each one sizes, as `status.capabilities.autosize` |
| `boundary_exceeded` at `spec.resources.<resource>` or `spec.resources.max.<resource>` | an update to a parent puts either below what a live child already holds, naming the child |

Which environments size what: Kubernetes sizes CPU always, and memory on a
cluster that [serves what it needs](kubernetes.md#sizing-a-sandbox). No
other runtime sizes anything yet.

A sandbox with a ceiling reports what it holds:

| Field | Meaning |
|---|---|
| `status.resources` | the size it has now: its CPU limit, its memory limit, and its workspace's size |
| `status.usage` | the last reading of what its workload uses: `memory` in bytes and `readAt`. Readings are taken every `CELLA_AUTOSIZE_INTERVAL` |
| `status.moves` | how many times raising its memory restarted it on another node |
| the `MemoryAtCeiling` condition | `True` while its memory is at `max.memory` and its workload uses 95% or more of it, or lost a process to the limit. `sandbox.at_ceiling` is recorded once each time that begins |
| the `MemoryUnplaced` condition | `True` once the sandbox needed more memory, inside its ceiling, and no node had room for the larger size, which its `message` names. Its `reason` is `NoRoom` while the sandbox is kept running at the size it holds, and `RestartsExhausted` once it was killed for memory too often there and is left `Failed`. `False` with reason `MemoryRaised` once a later raise was given. Absent on a sandbox no raise ever failed for |

A spawned child's ceilings may not exceed its parent's, and an update
that would narrow a parent below a live child's ceiling is refused the
same way, naming the child. An environment's warm pool keeps sandboxes
of one fixed size, so a manifest with a ceiling is created fresh and
takes no pool entry.

### Recovering a failed sandbox

A sandbox is `Failed` when its main process ended in failure: it exited
with a non-zero code, the kernel killed it for memory, or the runtime ended
it. `status.reason` is the runtime's word for it, `Exited` on the native
runtime, `Error` or `OOMKilled` on Kubernetes, and `status.exitCode` is the
code where there is one. **The workspace of such a sandbox is intact.**
Nothing was lost when the process ended, and nothing is lost while the
sandbox stays `Failed`.

Start it:

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" "$CELLA/v1/sandboxes/$SANDBOX/start"
```

or `cella start <name>`, or `StartSandbox` of the [Go client](client.md).

The start runs the sandbox's own command again, from the specification it
was created with, on the same workspace, and answers the sandbox as
`Running` or `Starting`. What a start keeps and what it does not:

| | After the start |
|---|---|
| the workspace | kept, every file of it |
| processes | gone, all of them: the main process runs anew, and a server or a watcher your client had started has to be started again |
| files outside the workspace, such as `/tmp` and anything written into the image's own directories | gone on Kubernetes, where the container is made anew; do not rely on them anywhere |
| the sandbox's id, name, labels, secrets, boundary and size | unchanged |

**Deleting a failed sandbox is what loses its files.** A client that
replaces a sandbox when it reads `Failed` discards a workspace that a start
would have given back. Delete a failed sandbox only when you mean to
discard it.

A start is one start. If the process ends again, the sandbox is `Failed`
again and stays so until the next start: nothing restarts it in a loop. A
process that fails every time it runs, because its command is wrong or
because it needs more memory than the sandbox has, fails again after each
start; read `status.exitCode` and the sandbox's logs before starting it a
third time. A start counts
against your sandbox limit as any start does, and answers `quota_exceeded`
at it.

Some failed sandboxes have nothing to start, because they never ran. A
start of one answers `phase_conflict`, with the reason in the error's
`details.detail`, and a new sandbox is the way forward:

| `status.reason` | What happened |
|---|---|
| `CreateFailed` | the create could not be finished, and what it had made was removed |
| `NoCapacity` | the environment had no room for it |
| `StartDeadline` | it waited in a queue past its start deadline and was never placed |
| `RecoveryExhausted` | its environment lost it and the control plane could not make it again |
| `VolumeMissing` | its environment lost it and a volume it had is gone |

A sandbox with a [memory ceiling](#sizing-inside-a-ceiling) and room under
it does not reach `Failed` when it is killed for memory: the control plane
starts it again itself, at a larger size or, where none could be placed,
at the one it holds, and it reads `Starting` meanwhile. A start of it then
answers `phase_conflict`, because the start is already being made. It
reads `Failed` only once it was killed again after each of the starts the
control plane makes at the same size, with the `MemoryUnplaced` condition
saying `RestartsExhausted`; your start then runs it again at that size.

A failed sandbox is kept until you start or delete it. Its
`spec.lifecycle.ttl` still deletes it when it runs out; `autoDelete`,
which counts from a stop, and `autoStop` do not apply to it.

### Status

The server writes `status`; it is ignored on the way in.

| Field | Meaning |
|---|---|
| `id` | the sandbox's identifier, which never changes and addresses it in every route |
| `owner` | the subject that created it, `<issuer>|<sub>` |
| `environment`, `driver`, `isolation` | where it runs and the boundary that runtime provides: `container`, `process`, or `none` |
| `phase` | `Pending`, `Queued`, `Starting`, `Running`, `Stopping`, `Stopped`, `Failed`, `Lost`, `Recovering`, or `Deleting`. `Pending`, `Queued`, `Starting` and `Recovering` are on the way to `Running` without any act of yours; `Failed` stays until you act, and a start [brings back](#recovering-a-failed-sandbox) one whose process ended |
| `reason` | why it is in that phase, such as `NoCapacity`, `StartDeadline`, `Preempted`, `HostRefused` or `HostTimeout` when the host a worker runs on refused the create or did not answer, the lifecycle rule that stopped it, the runtime's word for how a failed sandbox's process ended (`Exited`, `Error`, `OOMKilled`), or `MemoryRaised` or `MemoryNotRaised` on one that is `Starting` because it is being [started again after a kill for memory](#what-a-client-can-rely-on), at a larger size or at the one it holds |
| `conditions` | statements with a type, a status, a reason, and a time: `Scheduled`, `EgressEnforced`, `DisplayReady`, `HostApproval` on a worker whose host decides each create, and others. A row of a list page whose environment did not answer carries `Observed` `False`: its status is the one last recorded, not a fresh read |
| `parent`, `root`, `mesh`, `spawn` | where it sits in a spawn tree, its mesh, and its budget with how much is used |
| `paths` | on a runtime that cannot move paths inside a sandbox, such as [`local`](local.md), the host paths of its own scratch directories: `home` and `tmp`. A file of your own, such as a helper program or an attachment, goes there through the file routes, and is never inside a host folder |
| `ports` | each declared port, `listening` or `closed` |
| `secrets` | which placeholders are mounted, and which the gateway will not substitute |
| `volumes` | one entry per mount: the `vol_` id it was bound to at create and whether it is attached |
| `createdAt`, `startedAt`, `stoppedAt`, `lastActivityAt`, `expiresAt` | when each happened, and when the time to live ends |
| `exitCode` | the main process's exit code once it has ended |
| `preemptions` | how often it was stopped to make room |
| `resources`, `usage`, `moves` | for a sandbox with `spec.resources.max`, the size it holds now, the last reading of what it uses, and how often a raise restarted it; see [sizing inside a ceiling](#sizing-inside-a-ceiling). Absent otherwise |
| `warnings` | what the environment recorded and does not enforce |

## Secret

```json
{
  "apiVersion": "cella.latere.ai/v1beta1",
  "kind": "Secret",
  "metadata": {"name": "vendor"},
  "spec": {
    "scope": {"hosts": ["api.vendor.example"]},
    "value": "sk-..."
  }
}
```

| Field | Default | Meaning |
|---|---|---|
| `spec.kind` | `static` | `static` substitutes the value itself; `oauth_client_credentials` treats the value as `client_id:client_secret`, mints a token at `spec.oauth.tokenUrl`, and substitutes the token |
| `spec.scope.hosts` | required | the hosts the value may be sent to: exact names or one leading `*.` wildcard. There is no scope that means every host |
| `spec.scope.ports` | `[443]` | the ports on those hosts |
| `spec.inject.header` | `Authorization`, unless a query parameter is named | the header the value goes in |
| `spec.inject.query` | none | a query parameter instead of a header. Not both |
| `spec.inject.scheme` | `bearer` in `Authorization`, `raw` elsewhere | `bearer`, `basic` for a `user:pass` value sent as base64, or `raw` for the value as it is |
| `spec.inject.body` | `false` | also substitute the placeholder in a request body |
| `spec.oauth.tokenUrl`, `.scope`, `.audience` | none | the token endpoint and what to ask it for, for `oauth_client_credentials` |
| `spec.value` | required on the first apply | the value, on one line. It is accepted and never returned: no answer, event, or log line carries it. `cella apply --value-from-env` and `--value-file` put it in for you |

Headers that frame a request, such as `Host` or `X-Forwarded-*`, are never
rewritten and refused as an injection place. A second apply with a value
rotates it, and running sandboxes use the new value on their next
request. `status` carries the id, the owner, the version, and how many
sandboxes mount it.

`status.lastUsedAt` is when an egress gateway last substituted the value
into a request, in UTC to the second. It moves at most once every 5
minutes, so the last use is at or after it and normally less than 5
minutes after. A secret without it has not been sent anywhere since it
was created, which is how a secret nothing uses any more is told apart
from one in use.

## Volume

A volume is storage that outlives a sandbox: filled once when it is
created, mounted by a sandbox's `spec.volumes`, and kept when the
sandbox ends.

```json
{
  "apiVersion": "cella.latere.ai/v1beta1",
  "kind": "Volume",
  "metadata": {"name": "grader"},
  "spec": {
    "size": "1Gi",
    "access": "shared-read",
    "source": {"kind": "image", "image": "registry.example/grader@sha256:..."}
  }
}
```

| Field | Default | Meaning |
|---|---|---|
| `spec.environment` | `default` | where the volume lives. A volume never leaves its environment, and only a sandbox of that environment mounts it. Asks `environment.use` of your authorizer |
| `spec.size` | required | the capacity, such as `1Gi`. For an image source, the most the image's files may hold. It cannot shrink, and this release does not grow it |
| `spec.access` | `single` | `single`: one read-write mount or any number of read-only ones. `shared-read`: read-only mounts only, any number |
| `spec.source.kind` | `empty` | `empty`, or `image` for the root file system of one image. `snapshot` and `archive` are refused with `capability_unsupported` in this release |
| `spec.source.image` | required for `image` | the image, pinned by digest (`name@sha256:<64 hex digits>`), or a name your admission endpoint resolves to a pinned reference. The volume stores the pinned reference, so reading it back tells you exactly which image filled it |
| `spec.retain` | `true` | `false` deletes the volume with the last sandbox that mounts it |
| `spec.class` | none | refused in this release: the operator chooses the storage |

An image source passes your admission endpoint the way a sandbox's image
does, so an installation whose sandboxes run catalog images only fills
volumes from catalog images only. An environment that keeps no volumes,
or cannot fill one from an image, refuses with `capability_unsupported`;
the native runtime keeps none.

`status.phase` is `Pending` until the volume is filled, then `Available`,
or `Failed` with `status.reason` (`SourceUnreachable`, `SourceTooLarge`,
`SourceUnsupported`) and the runtime's own `status.message`. `Deleting`
is a delete in flight, and `Lost` a volume its runtime no longer holds,
which is never recreated. `status.attachedTo` names every sandbox that
mounts it, a stopped one included; a volume that any sandbox mounts
cannot be deleted (`volume_busy`). The labels, the annotations and
`retain` change on a second apply; every other field is fixed. Where your
installation limits how many volumes you hold, a create past the limit is
refused with `volume_quota_exceeded` ([the API reference](api.md#volumes)).

## Environment

An environment is where sandboxes run. The control plane creates its own
from its configuration; an administrator applies one for each data plane
a worker serves. [Self-hosting a data plane](workers.md) walks through
one, and [Capacity and queues](scheduling.md) is what the scheduling and
pool fields do.

| Field | Meaning |
|---|---|
| `metadata.name` | required, and the environment's id |
| `spec.mode` | `worker` for one you apply. `inprocess` is the control plane's own and not applied by a caller. Cannot change |
| `spec.isolation` | required: the boundary the runtime provides, `container`, `vm`, `process`, or `none`. A worker must match it |
| `spec.capacity` | required: `cpu`, `memory`, `disk`, and `sandboxes`, the ceiling placement admits against. The control plane's own may be `auto` |
| `spec.scheduling.mode` | `direct` or `queued` |
| `spec.scheduling.queues`, `.defaultQueue` | the queues a queued environment orders, and the one a sandbox that names none waits in |
| `spec.pool.size`, `.image`, `.resources`, `.display` | the prewarmed sandboxes to keep, on a runtime that supports them |
| `spec.workspaceClass` | the storage class of every workspace, where the runtime has one |
| `spec.gateway` | the gateway's proxy door as a sandbox of this environment dials it |

`status` reports the phase (`Pending`, `Ready`, `Degraded`, or
`Offline`), the runtime and the capabilities it declared, the workers and
gateways connected, the last heartbeat, and the capacity in use.
