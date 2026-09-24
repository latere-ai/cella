# Manifests

Every field of the three kinds this release serves, what it means, and
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
| `spec.workspace.path` | `/workspace` | where the workspace is mounted, a clean absolute path not under `/run/cella`. The native runtime keeps it at `/workspace` |
| `spec.workspace.source` | `empty` | what the workspace starts with. This release fills it from `empty` only |

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
environment, and the create waits for the gateway to acknowledge it. No
runtime in this release confines a workload to the gateway, so such a
boundary is recorded, `status.conditions` reports `EgressEnforced` false,
and the answer carries a warning. A sandbox may narrow its own boundary
later and never widen it.

### Spawn and mesh

| Field | Default | Meaning |
|---|---|---|
| `spec.mesh.spawn.budget` | `0` | how many sandboxes this one may create, in total, through its own workload token |
| `spec.mesh.spawn.depth` | `0` | how many generations below it may exist |
| `spec.mesh.enabled` | `false` | on a sandbox a subject created, joins its whole tree to one private network, where peers reach each other at `<sandbox-name>.mesh`. A child inherits it and may not set it. Refused on the native runtime |

Every child is a subset of its parent: its egress, its secrets, its
resources, and its lifetime. A child that asks for more is refused with
`boundary_exceeded` naming the field, and one that declares no boundary
runs inside its parent's. Its budget and depth are at most the parent's
remainder less one. Deleting a sandbox deletes the tree below it.

### Lifecycle

Each is a duration such as `15m` or `24h`, or `never`. With none set, a
sandbox runs until you stop or delete it.

| Field | Meaning |
|---|---|
| `spec.lifecycle.autoStop` | stop the sandbox after this much time without activity. Commands, file operations, and sessions count as activity |
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

### Status

The server writes `status`; it is ignored on the way in.

| Field | Meaning |
|---|---|
| `id` | the sandbox's identifier, which never changes and addresses it in every route |
| `owner` | the subject that created it, `<issuer>|<sub>` |
| `environment`, `driver`, `isolation` | where it runs and the boundary that runtime provides: `container`, `process`, or `none` |
| `phase` | `Pending`, `Queued`, `Running`, `Stopped`, `Failed`, `Lost`, `Recovering`, or `Deleting` |
| `reason` | why it is in that phase, such as `NoCapacity`, `StartDeadline`, `Preempted`, or the lifecycle rule that stopped it |
| `conditions` | statements with a type, a status, a reason, and a time: `Scheduled`, `EgressEnforced`, `DisplayReady`, and others |
| `parent`, `root`, `mesh`, `spawn` | where it sits in a spawn tree, its mesh, and its budget with how much is used |
| `ports` | each declared port, `listening` or `closed` |
| `secrets` | which placeholders are mounted, and which the gateway will not substitute |
| `createdAt`, `startedAt`, `stoppedAt`, `lastActivityAt`, `expiresAt` | when each happened, and when the time to live ends |
| `exitCode` | the main process's exit code once it has ended |
| `preemptions` | how often it was stopped to make room |
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
