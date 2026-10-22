# Self-hosting a data plane

A control plane decides what should run. A data plane runs it. By default
they are the same process: `cellad serve` drives the driver
`CELLA_RUNTIME` names and the sandboxes live beside it.

A worker separates them. `cellad worker` runs on your own machines, with
your own container engine or cluster, and connects **outbound** to a
control plane. Sandboxes run on your infrastructure; the control plane
holds the manifests, the identities and the API.

This is how you keep workloads inside your network while somebody else
operates the control plane, and how one control plane spans regions.

## The network rule

The control plane never dials a worker. There is one connection, the
worker opens it, and everything travels on it: the work down, the results
and the state up.

```
   your network                          the control plane
 ┌────────────────┐                     ┌──────────────────┐
 │ cellad worker  │ ──── outbound ────▶ │  cellad serve    │
 │   + sandboxes  │      HTTPS/WSS      │  /v1, identity   │
 └────────────────┘                     └──────────────────┘
        no inbound port, no public address, no firewall hole
```

A worker needs outbound access to the control plane's public URL and
nothing else. It listens on no port of its own.

## What you need

- A control plane you can reach, and an administrator's bearer for it.
- A machine with the engine your driver needs: Podman, a Kubernetes
  cluster, the sandbox runtime for `local` ([on your own
  computer](local.md)), or nothing at all for `native`.
- The `cellad` binary from the release archive.

## Apply the environment

A worker serves an environment, so the environment exists first. Apply one
as an administrator. `mode: worker` says the driver lives on your machines
rather than in the control plane's process, `isolation` is the class your
driver provides, and `capacity` is the ceiling placement admits against:

```sh
curl -sS -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "apiVersion": "cella.latere.ai/v1beta1",
    "kind": "Environment",
    "metadata": {"name": "eu-gpu", "labels": {"region": "eu"}},
    "spec": {
      "mode": "worker",
      "isolation": "container",
      "capacity": {"cpu": "128", "memory": "512Gi", "disk": "5Ti", "sandboxes": 100}
    }
  }' \
  https://cella.example.com/v1/environments/eu-gpu
```

The answer carries an `ETag`. Send it back as `If-Match` on the next apply
and a write that raced yours is refused with `version_conflict` rather
than overwriting what moved.

`spec.workers.max` limits how many workers may serve the environment at
once; unset, any number may. A registration beyond it is refused with
`worker_limit`. A worker reconnecting with the same key from the same
host, before the control plane has noticed its old stream is gone,
replaces that stream rather than waiting out the lease: the old one is
closed with `worker_replaced`. An old worker that had registered and not
yet opened its stream is told the same as soon as it opens one. The same
key from a host that reports other facts is refused while the live stream
lasts, so a copied key used from another machine cannot displace the one
serving. Each meeting is an `environment.worker_replaced` record, with
both registrations and whether the new one replaced the old or was
refused.

The environment is `Pending` until a worker registers on it. Nothing is
placed there until it is `Ready`. What happens to a create it cannot fit,
and how to make it wait in a queue instead, is in
[Capacity and queues](scheduling.md).

## Mint the key

A worker authenticates with an **environment key**: a credential that
names one environment and authorizes that environment's registration and
its stream. Nothing else. It cannot create a sandbox, read one, or reach
any route that decides on a person.

Mint one as an administrator, for the environment the worker will serve:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/eu-gpu/keys
```

```json
{
  "token": "eyJhbGciOi...",
  "jti": "01JBQ7...",
  "exp": "2027-09-20T10:00:00Z"
}
```

The key is shown **once**. The control plane keeps a record of every key
it mints, and never the key itself, so a key that is lost is replaced
rather than retrieved. The `jti` is what ends a key.

A key lives `CELLA_ENVIRONMENT_KEY_TTL`, a year by default. A mint may
ask for less with a body such as `{"ttl": "720h"}`; asking for more is
refused with `ceiling_exceeded`. The authorizer's `environment.key`
question carries the lifetime as `ttl` in seconds, so a policy can refuse
a long one.

An environment holds several keys, so each worker and each gateway
carries its own and one is revoked without ending the others.

List the keys an environment holds, oldest first:

```sh
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/eu-gpu/keys
```

```json
{
  "items": [
    {
      "jti": "01JBQ7...",
      "mintedAt": "2026-09-20T10:00:00Z",
      "exp": "2027-09-20T10:00:00Z",
      "revoked": false,
      "mintedBy": "https://login.example.com|ops"
    }
  ],
  "next": ""
}
```

A revoked key is listed with `"revoked": true` and the `revokedAt` it was
revoked at. A key leaves the list once its `exp` passes. The list pages
like every other: `?limit=` takes 1 to 200 and is 50 when absent, and a
non-empty `next` is the `?cursor=` of the page after. Listing the keys
needs the same right as minting one. A key minted before the control
plane kept this record is not listed, and is still revoked by its `jti`.

Revoke one:

```sh
curl -sS -X DELETE \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/eu-gpu/keys/01JBQ7...
```

The key stops working at once, on its next request and on its next frame,
and a stream it opened is closed with `key_revoked`. A key is revoked
through the environment it was minted for; another environment's key
answers `not_found`.

## Who may serve

A registration asks the authorizer `environment.serve` with the key's own
subject, `environment:<id>`, about the environment, with the worker's
driver, isolation class and host as the resource's `registration`. A
refusal answers `serve_refused`. While the stream lives the question is
asked again on every decision's TTL, at most five minutes apart, and a
refusal closes the stream with `serve_refused`. A recheck that gets no
decision keeps the stream one more TTL and then closes it with 1013, try
again later, so the worker reconnects and is decided anew. The owner
policy allows a key that is not revoked to serve its own environment.

A worker stops on `serve_refused` and `key_revoked`: it stops and deletes
every sandbox its driver holds and does not reconnect.

## Environments a subject owns

Under the owner policy environments are an administrator's. With
`CELLA_OWNED_ENVIRONMENTS=worker`, any subject may create a `mode: worker`
environment and act on the ones it owns, as it acts on its sandboxes:
read, change, delete, use and key them, and list its own. In-process
environments stay an administrator's. With an authorization endpoint,
these are its decisions, and every `environment.*` question carries the
environment's `mode` and `workers`, the value of `spec.workers.max`, 0
when unset.

Deleting a worker environment deletes it with whatever runs on it: its
keys are revoked, its workers' streams close with `serve_refused`, the
records of its sandboxes go, and each worker stops and deletes what it
ran. Any other environment a sandbox is placed on answers
`phase_conflict`.

## A driver that enforces its own allow list

The egress gateway serves the control plane's own environment, so a
sandbox on a worker's environment is pointed at no gateway: no proxy
variable, reverse address, gateway credential or authority is projected
into it. A boundary there that a gateway must hold, such as a mounted
secret, is refused with `egress_gateway_unavailable`. A driver that
declares `egressLocal` in its capabilities enforces an allow list itself,
with its own proxy as the only route out: a sandbox asking for `none` or
an allow list on its environment needs no gateway, is created, and reports
`EgressEnforced` `True` with the reason `DriverEnforced`. Such an
environment needs no `spec.gateway`.

## Run the worker

```sh
export CELLA_URL=https://cella.example.com
export CELLA_ENVIRONMENT_KEY='eyJhbGciOi...'
export CELLA_RUNTIME=podman
cellad worker
```

```
cellad: v0.5.0 worker driver=podman isolation=container control-plane=https://cella.example.com
```

The worker reads the public certificate authorities first, which every
sandbox it runs behind an egress gateway trusts beside the gateway's own,
and exits when it finds none; `SSL_CERT_FILE` names the bundle when the
system's is not where Go looks for it. Then it checks its own driver. A
host whose engine does not answer exits rather than registering, because a
control plane that believed the registration would place sandboxes on a
machine that cannot run them.

Then it registers what its driver provides, opens its stream, and starts
taking work. Read the environment to see it arrive:

```sh
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/eu-gpu | jq .status
```

```json
{
  "phase": "Ready",
  "driver": "podman",
  "isolation": "container",
  "workers": 1,
  "lastHeartbeat": "2026-09-20T14:31:02Z"
}
```

The phase follows the worker. It is `Ready` from the moment a worker's
registration is answered and while one heartbeats, `Offline` with the
reason `HeartbeatLost` once nothing has been heard for
`CELLA_ENVIRONMENT_OFFLINE`, and `Ready` again when a worker returns.
`workers` and `hosts` count a worker from its registration and stop
counting it when its stream ends, so a read right after either already
says so. A worker that stops after registering and before opening its
stream is counted for 45 seconds from its registration, because nothing
tells the control plane it went away. A sandbox created in the moment
between a registration and its stream stays `Pending` until the stream
opens. An environment below `Ready` refuses a new sandbox and leaves the
ones it already holds running.

## Run sandboxes on it

A manifest names the environment it wants:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "apiVersion": "cella.latere.ai/v1beta1",
    "kind": "Sandbox",
    "metadata": {"name": "build"},
    "spec": {"environment": "eu-gpu", "image": "registry.example.com/base:1"}
  }' \
  https://cella.example.com/v1/sandboxes
```

It reads back like any other sandbox. `status.environment`,
`status.driver` and `status.isolation` are the only fields that say where
it ran. A manifest that names no environment gets the one the control
plane drives itself.

When an environment is finished with, delete it. One that still holds a
sandbox is refused, so nothing is left running with nothing driving it:

```sh
curl -sS -X DELETE \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/eu-gpu
```

## Its variables

A worker reads its own variables and none of the control plane's. It
holds no database, no issuer and no authorizer.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `CELLA_URL` | yes | none | the control plane's public URL, which the worker connects outbound to, with its path when it is served under one: `https://api.example.com/v1/environments` |
| `CELLA_ENVIRONMENT_KEY` | yes | none | the key that authenticates the registration and the stream |
| `CELLA_RUNTIME` | no | `k8s` | the driver this worker runs: `k8s`, `podman`, `native` or `local` |
| `CELLA_DATA_DIR` | no | `/var/lib/cella` | where the native and local drivers keep their sandboxes |
| `CELLA_PODMAN_SOCKET` | no | the rootless socket, then the system one | the Podman API to drive |
| `CELLA_K8S_*` | no | in-cluster | the cluster to drive, read only with `CELLA_RUNTIME=k8s` |
| `CELLA_ALLOW_UNSAFE_NATIVE` | with `native` | `false` | explicit consent to run without isolation |
| `CELLA_LOCAL_*` | no | unset | the folders, read paths, denied paths and network a `local` worker allows ([on your own computer](local.md)) |
| `CELLA_CAPACITY_CPU`, `_MEMORY`, `_DISK`, `_SANDBOXES` | no | unset | what this worker declares it can hold |
| `CELLA_WORKER_LABELS` | no | unset | `key=value` pairs describing this worker, comma separated |
| `CELLA_INSECURE_CONTROL_PLANE` | no | unset | `1` admits an `http://` control plane that is not on loopback |
| `SSL_CERT_FILE` | no | the first of Go's Linux bundle files that holds a certificate | the public roots written into each sandbox's trust file ahead of the gateway's authority. The worker refuses to start when it finds none, or more than 512 KiB. The released image ships `/etc/ssl/certs/ca-certificates.crt`; point this at a bundle of your own to add a private authority beside the public ones |

A key travels on every request, so `CELLA_URL` must be `https://` unless
it is a loopback address. Setting `CELLA_INSECURE_CONTROL_PLANE=1` is the
explicit consent to send it in the clear; a deployment should not.

## What happens when it goes away

A worker that loses its connection keeps the sandboxes it is already
running. A sandbox does not stop because the control plane stopped
watching it.

The worker reconnects with a backoff that starts at one second and
doubles to thirty. A stream the control plane accepted sets it back to one
second, so a worker whose stream ended because the control plane restarted
dials again after a second, however often that has happened before. Each
connection begins with a registration, so the control plane always knows
which process is claiming work.

A control plane that runs
[several replicas](kubernetes.md#running-more-than-one-replica) holds every
worker's stream on its writer, whichever replica the worker reaches. When
the writer hands off to another replica, the stream is closed and the
worker reconnects to the new writer.

The control plane stops counting a worker after
`CELLA_ENVIRONMENT_OFFLINE` (two minutes by default) without a heartbeat,
and never sooner than the 45 second lease a worker's own connection runs
under, so a short window cannot declare a healthy worker gone between two
heartbeats. Operations that were in flight on a stream that dropped are
failed, and the caller retries.

## Long streams and slow callers

Everything an operation streams travels on the worker's one connection: an
exec's output, a terminal, an archive going in or out, a file's body. Each
of those streams has a window of 8 MiB. The side sending it sends at most
that much ahead of what the other side has handed on, and then waits for
room rather than queueing more.

So a caller that stops reading holds its own stream and nothing else. A
client that pauses a long export, or reads an exec's output slowly, slows
that one stream; every other exec, terminal and lifecycle call on the same
worker keeps moving, and the connection stays up however long the caller
pauses. Memory is bounded the same way: the control plane holds at most
8 MiB of a stream whose caller is not reading, and the worker at most
8 MiB of one its driver is not reading.

The window is agreed when the worker connects, so the control plane and
its workers can be upgraded in either order. A worker and a control plane
where one side predates the window still connect and work, with the stream
as it was before: a caller that stops reading can hold that worker's
connection until it reads again, and a pause longer than ten seconds can
drop the connection, which the worker then reopens. Upgrade both sides to
have the window.

## Running several

Several workers may serve one environment. Each registers, each claims
what it can, and the work is shared.

They must agree: the environment records the driver name and the
isolation class from the first registration, and a later worker that
reports another is refused with `environment_mismatch`. One environment
is one kind of place, or a manifest that resolved against it would be
resolving against something else.

What the environment can do is what **all** of its workers can do. A
capability one worker lacks is one the environment does not declare, so
a caller is never routed to a worker that cannot serve the request.

## What a worker never does

It mints no credentials. A sandbox's identity is minted by the control
plane and arrives in the create the worker is handed; the worker's driver
projects the file and nothing more.

It keeps nothing of its own. Everything a worker knows is either in its
driver's own records or arrives on the stream, so a replaced host loses
nothing the control plane had.

## Embedding the worker

`cellad worker` is one program that runs the worker. Another program can
run it in its own process, with its own key store, its own prompts and
its own log, by importing `latere.ai/x/cella/worker`:

```go
err := worker.Run(ctx, worker.Options{
	URL:    "https://api.example.com",
	Key:    func(context.Context) (string, error) { return keychain.Read("environment-key") },
	Driver: driver,     // any runtime.Driver; its Preflight must pass
	Policy: myPolicy,   // asked before each create and each operation
	Record: myLog,      // told of each operation once it is answered
	State:  myStatus,   // told when the worker connects and when it stops being connected
})
```

`Key` is read once per connection, so a key kept in the operating
system's key store, or one rotated while the program runs, is read where
it lives. `Run` reconnects with the backoff above and returns when `ctx`
ends or when the control plane refuses the worker: a `*worker.Refusal`
whose code is `serve_refused`, `key_revoked`, `worker_replaced`,
`worker_limit` or `environment_mismatch`. It does not retry those, since
another attempt would meet the same answer.

`State` is told of every change of the connection, so a program shows
whether its machine is online without reading the worker's log. A
`worker.State` is `Connected` once a registration is taken and its
stream is open, with the control plane's answer to the registration in
`Registered`. When a registration fails or the stream ends it is not
connected, and `Err` says why: a `*worker.RegistrationError` carries the
status and the error code of a registration the control plane answered
with an error the worker retries on, such as a key it refused with 401.
`Reconnect` and `RetryIn` say when the worker tries again. The last call
before `Run` returns has `Reconnect` false, with the `*worker.Refusal`
in `Refusal` when a refusal ended it, or the context's cause in `Err`
when the context did. The calls arrive in order on `Run`'s own
goroutine, which waits for each, so a `State` that blocks holds the
worker.

When it returns, `Run` ends what it started. When `ctx` ends it stops
every sandbox its driver lists, unless `KeepOnExit` is set; `cellad
worker` sets it, because an operator's sandboxes outlive a restart of the
role. On `serve_refused` or `key_revoked` it stops and deletes every
sandbox whatever `KeepOnExit` says. On `worker_replaced` it touches
nothing: another process holding the same key now serves those sandboxes.

[`examples/worker`](../examples/worker) is a complete program that asks
on its terminal before each create and prints a line per operation.

## What the host decides

A worker on a machine somebody owns, such as a person's own computer,
must be able to say no. The host has the last word in three ways:

- **Built-in checks.** Before anything else, the worker compares each
  create with what its driver reports of the host. A `spec.workspace.host`
  outside every folder the host grants is refused with
  `workspace_not_granted`, and so is a write into a folder it grants
  read-only. A create asking for another isolation class than the
  driver's, or mounting a volume on a driver that keeps none, is refused
  with `host_refused`. These hold whatever the
  control plane resolved and whatever the policy says. The network is
  narrowed by the driver to the host's own list rather than refused.
- **The policy.** `Options.Policy` is asked before each create, and may
  take its time: a person may be asked. A create not answered within a
  second shows on the sandbox as the `HostApproval` condition, status
  `Unknown`, reason `Pending`, message "Waiting for the host", with a
  `sandbox.host_pending` record. An allowed create runs and the condition
  becomes `True`. A refused one fails the sandbox with the reason
  `HostRefused` and the host's own words in the condition's message; one
  not answered within `AdmitTimeout` (`CELLA_HOST_ADMIT_TIMEOUT`, ten
  minutes by default) fails it with `HostTimeout`. The policy is also
  asked before every other operation on a sandbox, and its refusal
  answers `host_refused` with its reason.
- **The record.** `Options.Record` is told of every operation once it is
  answered: when, what, which sandbox and its labels, the command or the
  path, and whether it ran, was refused, or failed. It carries no file
  content, no stdin and no output.

A worker also refuses any operation on a sandbox its driver does not
hold.

### The host in the registration

A driver that reports the machine it runs on registers its operating
system, architecture, granted folders and network. The environment shows
one entry per live worker in `status.hosts`, beside the `status.workers`
count:

```json
"hosts": [{"worker": "wrk_...", "host": {"os": "darwin", "arch": "arm64",
  "grants": [{"path": "/Users/dev/site", "write": true}],
  "network": {"mode": "allowlist", "hosts": ["github.com"]}}}]
```

A host whose network admits any public host but those it blocks reports
the mode `open`, its deny list as `blocked`, and the `host:port` entries
for ports beyond 443 and 80 as `hosts`. A manifest's
`spec.workspace.host` must be inside a folder every live worker grants,
and its egress is narrowed to what every live worker's network allows,
with a warning naming what the sandbox gets. A change on
the host takes effect when its program starts the worker again.

## On Kubernetes

Nothing here listens, so the workload needs no Service, no Ingress and no
inbound rule. Mint the key first and put it in the Secret.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cellad-worker
type: Opaque
stringData:
  # The environment key. It names one environment and authorizes that
  # environment's registration and its stream, and nothing else: it cannot
  # create a sandbox, read one, or reach any route that decides on a
  # person. Revoke it by the jti the mint returned.
  CELLA_ENVIRONMENT_KEY: "replace-me"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cellad-worker
  labels:
    app.kubernetes.io/name: cellad
    app.kubernetes.io/component: worker
spec:
  # Several workers may serve one environment: each registers, each claims
  # what it can, and the operations table is the arbiter. They must agree
  # on the driver and the isolation class, which one image and one
  # configuration give them.
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: cellad
      app.kubernetes.io/component: worker
  template:
    metadata:
      labels:
        app.kubernetes.io/name: cellad
        app.kubernetes.io/component: worker
    spec:
      serviceAccountName: cellad-worker
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: worker
          image: ghcr.io/example/cellad:v0.5.0
          args: ["worker"]
          env:
            # The control plane this worker joins. A key travels on every
            # request, so this is https:// unless it is loopback.
            - name: CELLA_URL
              value: "https://cella.example.com"
            - name: CELLA_ENVIRONMENT_KEY
              valueFrom:
                secretKeyRef:
                  name: cellad-worker
                  key: CELLA_ENVIRONMENT_KEY
            # The driver this worker runs, and the cluster it drives. The
            # control plane records the name from the first registration
            # and refuses a later worker that reports another.
            - name: CELLA_RUNTIME
              value: "k8s"
            - name: CELLA_K8S_NAMESPACE
              value: "cella-sandboxes"
            # What this worker declares it can hold. Placement admits
            # against the lesser of it and the environment's own ceiling.
            - name: CELLA_CAPACITY_CPU
              value: "128"
            - name: CELLA_CAPACITY_MEMORY
              value: "512Gi"
            - name: CELLA_CAPACITY_SANDBOXES
              value: "100"
            - name: CELLA_WORKER_LABELS
              value: "region=eu,gpu=true"
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: "100m"
              memory: "128Mi"
            limits:
              cpu: "1"
              memory: "512Mi"
          volumeMounts:
            - name: state
              mountPath: /var/lib/cella
      volumes:
        # The worker keeps nothing that has to outlive it: everything it
        # knows is in its driver's own records or arrives on the stream.
        - name: state
          emptyDir: {}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cellad-worker
---
# The worker's driver uses the accesses the control plane's does, and
# checks each one when it starts: apply the Role of deploy/base/rbac.yaml
# in the namespace CELLA_K8S_NAMESPACE names, and bind it here. Without it
# the worker stops at start with the missing rule named.
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cellad-worker
  namespace: cella-sandboxes
subjects:
  - kind: ServiceAccount
    name: cellad-worker
    namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cellad
```

The binding names the namespace the worker's Deployment runs in,
`default` above; change it with the Deployment's.

## Running a gateway beside it

Sandboxes on your infrastructure reach the egress gateway on your
infrastructure. Run `cellad egress` beside the worker with an
environment key of its own, and point the environment at it. The gateway
follows the same rule: it connects outbound and nothing dials it.
