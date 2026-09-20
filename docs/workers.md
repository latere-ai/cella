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
  cluster, or nothing at all for `native`.
- The `cellad` binary from the release archive.

## Mint the key

A worker authenticates with an **environment key**: a credential that
names one environment and authorizes that environment's registration and
its stream. Nothing else. It cannot create a sandbox, read one, or reach
any route that decides on a person.

Mint one as an administrator:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/default/keys
```

```json
{
  "token": "eyJhbGciOi...",
  "jti": "01JBQ7...",
  "exp": "2027-09-20T10:00:00Z"
}
```

The key is shown **once**. The control plane signs it and keeps no copy,
so a key that is lost is replaced rather than retrieved. Keep the `jti`:
it is what ends the key.

An environment holds several keys, so each worker and each gateway
carries its own and one is revoked without ending the others.

Revoke one:

```sh
curl -sS -X DELETE \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/default/keys/01JBQ7...
```

The key stops working at once, on its next request and on its next frame.

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

The worker checks its own driver first. A host whose engine does not
answer exits rather than registering, because a control plane that
believed the registration would place sandboxes on a machine that cannot
run them.

Then it registers what its driver provides, opens its stream, and starts
taking work. Read the environment to see it arrive:

```sh
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://cella.example.com/v1/environments/default | jq .status
```

```json
{
  "phase": "Ready",
  "driver": "native",
  "isolation": "none",
  "workers": 1,
  "lastHeartbeat": "2026-09-20T14:31:02Z"
}
```

## Its variables

A worker reads its own variables and none of the control plane's. It
holds no database, no issuer and no authorizer.

| Variable | Required | Default | What it is |
|---|---|---|---|
| `CELLA_URL` | yes | none | the control plane's public URL, which the worker connects outbound to |
| `CELLA_ENVIRONMENT_KEY` | yes | none | the key that authenticates the registration and the stream |
| `CELLA_RUNTIME` | no | `k8s` | the driver this worker runs: `k8s`, `podman` or `native` |
| `CELLA_DATA_DIR` | no | `/var/lib/cella` | where the native driver keeps its sandboxes |
| `CELLA_PODMAN_SOCKET` | no | the rootless socket, then the system one | the Podman API to drive |
| `CELLA_K8S_*` | no | in-cluster | the cluster to drive, read only with `CELLA_RUNTIME=k8s` |
| `CELLA_ALLOW_UNSAFE_NATIVE` | with `native` | `false` | explicit consent to run without isolation |
| `CELLA_CAPACITY_CPU`, `_MEMORY`, `_DISK`, `_SANDBOXES` | no | unset | what this worker declares it can hold |
| `CELLA_WORKER_LABELS` | no | unset | `key=value` pairs describing this worker, comma separated |
| `CELLA_INSECURE_CONTROL_PLANE` | no | unset | `1` admits an `http://` control plane that is not on loopback |

A key travels on every request, so `CELLA_URL` must be `https://` unless
it is a loopback address. Setting `CELLA_INSECURE_CONTROL_PLANE=1` is the
explicit consent to send it in the clear; a deployment should not.

## What happens when it goes away

A worker that loses its connection keeps the sandboxes it is already
running. A sandbox does not stop because the control plane stopped
watching it.

The worker reconnects with a backoff that starts at one second and
doubles to thirty. Each connection begins with a registration, so the
control plane always knows which process is claiming work.

The control plane stops counting a worker after
`CELLA_ENVIRONMENT_OFFLINE` (two minutes by default) without a heartbeat.
Operations that were in flight on a stream that dropped are failed, and
the caller retries.

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
```

## Running a gateway beside it

Sandboxes on your infrastructure reach the egress gateway on your
infrastructure. Run `cellad egress` beside the worker with an
environment key of its own, and point the environment at it. The gateway
follows the same rule: it connects outbound and nothing dials it.
