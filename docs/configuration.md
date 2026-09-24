# Configuration

Every environment variable Cella reads, what it does, and its default.
`cellad` reads its configuration once, when it starts, and a value that is
missing or malformed stops it with one message naming every problem at
once, so a deployment is fixed in one round. A blank value is unset.

One binary plays several roles, and each reads its own set:

| Role | Command | Reads |
|---|---|---|
| the control plane | `cellad serve`, or `cellad` alone | every section below up to the worker |
| the installation check | `cellad check` | the same as the control plane, and asks each dependency whether it answers |
| a self-hosted data plane | `cellad worker` | [The worker](#the-worker) |
| the egress gateway | `cellad egress` | [The egress gateway](#the-egress-gateway) |
| the client | `cella` | [The cella command](#the-cella-command) |

Durations are written the way Go writes them: `500ms`, `30s`, `10m`,
`8760h`. Compute amounts are written the way Kubernetes writes them:
`500m` or `2` for cpu, `2Gi` for memory and disk. A list is comma
separated.

## Listeners and local state

| Variable | Default | What it is |
|---|---|---|
| `CELLA_PUBLIC_ADDR` | `:8080` | the listener callers reach: the `/v1` API, the key set, the OpenAPI document, and, at the root, the probes |
| `CELLA_INTERNAL_ADDR` | `:8081` | the listener for the probes and `/metrics`. It must differ from the public one. Do not publish it |
| `CELLA_BASE_PATH` | unset, the root | the path the public listener answers under when the control plane shares an origin with other services, such as `/v1/environments`. It takes the place of `/v1`: `/v1/sandboxes` is answered at `/v1/environments/sandboxes`, and the key set, the OpenAPI document and `/version` move under it too. Nothing is answered outside it, and the probes stay on the internal listener. It must be the path of `CELLA_PUBLIC_URL`. See [Serving behind a shared origin](install.md#serving-behind-a-shared-origin) |
| `CELLA_DATA_DIR` | `/var/lib/cella` | the directory `cellad` keeps local state in: the readiness write test, the native runtime's sandboxes, and, without a database, the snapshot of desired state. Created at start |
| `CELLA_MAX_BODY_BYTES` | `65536` | the largest manifest or JSON body accepted. Integer bytes, or with a `Ki`, `Mi`, or `Gi` suffix |
| `CELLA_MAX_UPLOAD_BYTES` | `1Gi` | the largest file or archive upload accepted, in the same syntax |

## Identity

Who may call, and what `cellad` signs. The first three are required.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_PUBLIC_URL` | required | the absolute URL callers reach the public listener at. It is the issuer of every token `cellad` mints, so it must be the address callers actually dial. A path on it, such as `https://api.example.com/v1/environments`, is where the control plane is served, and every path `cellad` writes, a `Location` header or the paths of the served OpenAPI document, is under it. No query and no fragment |
| `CELLA_OIDC_ISSUERS` | required | the OpenID Connect issuers whose tokens are accepted. At start `cellad` reads each one's discovery document and key set, and refuses to start when one does not answer |
| `CELLA_TOKEN_KEY` | required | one or two PEM RSA private keys of at least 2048 bits. The first signs every sandbox's workload token and every environment key; every key is published at `/.well-known/jwks.json`. To rotate, put the new key first and remove the old block once the tokens it signed have expired |
| `CELLA_OIDC_AUDIENCE` | `cella` | the audiences a caller's token may carry, one or more. The first is the audience of the tokens `cellad` mints. An empty or repeated entry is a start-up failure |
| `CELLA_OIDC_INSECURE_ISSUERS` | unset | issuers from the list that may use `http://` on a host that is not loopback. For a test issuer only |
| `CELLA_AUTHORIZER_URL` | unset | your authorization endpoint. Unset, the built-in owner policy decides. `http://` is accepted on loopback only |
| `CELLA_AUTHORIZER_TOKEN` | required with the URL | the bearer `cellad` sends your endpoint |
| `CELLA_AUTHORIZER_TIMEOUT` | `5s` | the deadline of one decision. A decision that does not arrive refuses the request |
| `CELLA_AUTHORIZER_CACHE` | `60s` | how long an allow is kept when the answer names no `ttl`. An answer's own `ttl` is capped at `600s`, and a deny is kept for five seconds |
| `CELLA_ADMIN_SUBJECTS` | unset | subjects, written `<issuer>|<sub>`, that the owner policy lets act on everything and alone lets create environments. Unused when an authorization endpoint is set |
| `CELLA_ENVIRONMENT_KEY_TTL` | `8760h` | how long an environment key, which a worker or a gateway authenticates with, lives from mint |
| `CELLA_DEFAULT_ENVIRONMENT` | `default` | the name of the environment this control plane drives itself, and the one a manifest that names none runs on |

## Admission

An endpoint you write that sees every manifest before it is created and
answers with the manifest to run or a refusal.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_ADMISSION_URL` | unset | your admission endpoint. Unset, a manifest continues as the caller wrote it. `http://` is accepted on loopback only |
| `CELLA_ADMISSION_TOKEN` | required with the URL | the bearer `cellad` sends it |
| `CELLA_ADMISSION_TIMEOUT` | `3s` | the deadline of one call, between `100ms` and `30s`. A call that does not answer fails the create |
| `CELLA_DEFAULT_IMAGE` | unset | the image a manifest gets when it names none, on an environment that runs images. With an admission endpoint, the endpoint can supply it instead |

## The runtime

The runtime is what the control plane's own environment runs sandboxes
on.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_RUNTIME` | `k8s` | `k8s`, `podman`, or `native`. Any other value stops the start; nothing falls back to another runtime |
| `CELLA_ALLOW_UNSAFE_NATIVE` | `false` | `true` is the consent the native runtime requires, because it runs sandboxes as host processes with no isolation |
| `CELLA_PODMAN_SOCKET` | the rootless socket, then the system one | the absolute path of the libpod API socket the Podman runtime drives. Unset, it tries `$XDG_RUNTIME_DIR/podman/podman.sock` and then `/run/podman/podman.sock` |

### Kubernetes

Read when `CELLA_RUNTIME` is `k8s`. Each sandbox is one
PersistentVolumeClaim and one Pod in the namespace below.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_K8S_KUBECONFIG` | in-cluster | the kubeconfig of the cluster to drive. Empty is the configuration of the Pod `cellad` runs in |
| `CELLA_K8S_NAMESPACE` | `cella` | the namespace sandboxes live in |
| `CELLA_K8S_STORAGE_CLASS` | the cluster's default | the storage class each workspace claim is provisioned from |
| `CELLA_K8S_NODE_SELECTOR` | unset | `key=value` pairs every sandbox Pod is scheduled by |
| `CELLA_K8S_TOLERATIONS` | unset | `key[=value][:effect]` entries every sandbox Pod tolerates |
| `CELLA_K8S_IMAGE_PULL_SECRETS` | unset | the Secrets in that namespace every sandbox Pod pulls with |
| `CELLA_K8S_RUN_AS_USER` | `1000` | the uid a sandbox runs as when its manifest names none. Zero is refused |
| `CELLA_K8S_RUN_AS_GROUP` | `1000` | the gid, which is also the group that owns the workspace. Zero is refused |
| `CELLA_K8S_DEFAULT_CPU` | `1` | the cpu limit of a sandbox whose manifest names none |
| `CELLA_K8S_DEFAULT_MEMORY` | `1Gi` | the memory limit of a sandbox whose manifest names none |
| `CELLA_K8S_DEFAULT_DISK` | `5Gi` | the size of the workspace claim of a sandbox whose manifest names none |
| `CELLA_K8S_CPU_REQUEST_RATIO` | `0.1` | the cpu request as a fraction of the limit, above 0 and at most 1. Lower packs idle sandboxes more densely |
| `CELLA_K8S_MEMORY_REQUEST_RATIO` | `1` | the memory request as a fraction of the limit |
| `CELLA_K8S_READY_TIMEOUT` | `90s` | how long a create or a start waits for the Pod to be ready |
| `CELLA_K8S_GRACE_PERIOD` | `10s` | the termination grace period of a sandbox Pod |
| `CELLA_K8S_DISPLAY_IMAGE` | unset | the desktop image, `ghcr.io/<owner>/cella-display:<tag>` from the same release. Set, a sandbox that asks for `display` gets a screen, screenshots, and input; unset, such a manifest is refused |
| `CELLA_K8S_DISPLAY_CPU` | the default cpu | the cpu limit of the desktop container |
| `CELLA_K8S_DISPLAY_MEMORY` | the default memory | the memory limit of the desktop container |
| `CELLA_K8S_GATEWAY_SELECTOR` | unset | `key=value` labels of the egress gateway's Pods. Set, every sandbox reaches the network only through the gateway and the environment enforces the `none`, `allowlist` and `open` boundaries; unset, a boundary is recorded and not enforced. Needs `CELLA_GATEWAY` |
| `CELLA_K8S_GATEWAY_NAMESPACE` | the sandbox namespace | the namespace the gateway's Pods run in |
| `CELLA_K8S_GATEWAY_PORTS` | `3128,8080` | the ports the gateway's Pods listen on, which are the gateway's `CELLA_EGRESS_PROXY_ADDR` and `CELLA_EGRESS_REVERSE_ADDR`, not the ports of its Service |
| `CELLA_K8S_DNS_SELECTOR` | `k8s-app=kube-dns` | `key=value` labels of the cluster's DNS Pods, which a sandbox reaches to resolve the gateway |
| `CELLA_K8S_DNS_NAMESPACE` | `kube-system` | the namespace the DNS Pods run in |

The last four are read only with `CELLA_K8S_GATEWAY_SELECTOR`, and are a
start-up problem without it.

[Sandboxes on Kubernetes](kubernetes.md) is what the Role has to allow
for each capability, and what the network rule of each sandbox admits.

## Lifecycle

| Variable | Default | What it is |
|---|---|---|
| `CELLA_REAP_INTERVAL` | `30s` | how often the lifecycle rules run: auto-stop, time to live, and auto-delete. Between `1s` and `1h` |
| `CELLA_TOUCH_INTERVAL` | `1m` | how often one sandbox's activity is written to the runtime, so a busy session does not write on every request. Between `1s` and `1h` |
| `CELLA_LOST_GRACE` | `10m` | without a database, how long a sandbox the runtime lost is reported as `Lost` before it is deleted. With one, a lost sandbox is recreated instead. Between `1s` and `1h` |
| `CELLA_ENVIRONMENT_OFFLINE` | `2m` | how long an environment goes without a worker's heartbeat, or with its own runtime not ready, before it is `Offline` and takes no new sandbox. Between `1s` and `1h` |

## State

| Variable | Default | What it is |
|---|---|---|
| `CELLA_DB_URL` | unset | a `postgres://` URL for desired state, the event journal, and secret values. Migrations run over it at start, and a schema this binary does not know stops the start. Unset, state is a snapshot under `CELLA_DATA_DIR` that one process holds, and a sandbox the runtime lost is not recovered |
| `CELLA_DB_POOL_URL` | `CELLA_DB_URL` | a pooled endpoint for the serving path, such as a transaction-mode pooler. Migrations keep the direct URL. Set without `CELLA_DB_URL`, it stops the start |
| `CELLA_DB_MAX_CONNS` | `4` | the connections one replica opens, between 1 and 32 |
| `CELLA_SECRET_KEY` | unset | 32 bytes, base64, that every secret value's data key is wrapped under. Without it, a `Secret` is refused with `capability_unsupported`. `openssl rand -base64 32` makes one |

Several replicas on one database share the work: each background loop is
held by one replica at a time under a lease.

## The default environment

The environment this control plane drives itself is written from these
variables the first time `cellad` starts on a store. After that, the
stored `Environment` is what counts, and `PUT /v1/environments/{name}` is
how it changes. [Capacity and queues](scheduling.md) is what each field
does.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_SCHEDULING_MODE` | `direct` | `direct` starts a create now or fails it; `queued` holds what does not fit until there is room |
| `CELLA_CAPACITY_CPU`, `CELLA_CAPACITY_MEMORY`, `CELLA_CAPACITY_DISK` | unset | what the environment holds. With none of the four set, the capacity is `auto`, what the cluster or the host reports |
| `CELLA_CAPACITY_SANDBOXES` | unset | how many sandboxes the environment holds, zero or above |
| `CELLA_POOL_SIZE` | `0` | how many prewarmed sandboxes to keep, up to 1024. Zero keeps none. The runtime must support pools |
| `CELLA_POOL_IMAGE` | unset | the image the prewarmed sandboxes run |
| `CELLA_POOL_CPU`, `CELLA_POOL_MEMORY`, `CELLA_POOL_DISK` | unset | the resources of a prewarmed sandbox. A create is served from the pool only when it matches what a prewarmed sandbox already is |
| `CELLA_GATEWAY` | unset | the egress gateway's proxy door as a sandbox dials it, `host` or `host:port` (port 3128 when none is given). A manifest that declares a boundary or mounts a secret needs a gateway |
| `CELLA_GATEWAY_REVERSE` | unset | the gateway's reverse door as a sandbox dials it, for tools that ignore proxy variables. Only with `CELLA_GATEWAY` |

These are read on every start, whether or not the environment exists yet:

| Variable | Default | What it is |
|---|---|---|
| `CELLA_SCHEDULE_INTERVAL` | `5s` | how often the scheduler passes over queued environments when nothing wakes it sooner. Between `1s` and `1h` |
| `CELLA_MAX_PREEMPTIONS` | `3` | how often one sandbox may be stopped to make room for one of higher priority, up to 100. `0` turns preemption off |
| `CELLA_POOL_IN_FLIGHT` | `2` | how many prewarmed sandboxes one refill starts at once, between 1 and 64 |
| `CELLA_POOL_GRACE` | `5m` | how long a new prewarmed sandbox is left alone before the pool's rules read it. Between `1s` and `1h` |
| `CELLA_EGRESS_ACK_TIMEOUT` | `5s` | how long a create waits for a gateway to acknowledge the new sandbox's boundary, up to `1m`. A create no gateway acknowledges fails |
| `CELLA_EGRESS_RECORDS_CAP` | `1000` | how many connection records are kept per sandbox |

## Events and the journal

Every change and every operation is a record in the journal. With a sink
configured, each record is also delivered, signed, to your endpoint.
[Following events](events.md) reads the journal over the API.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_EVENTS_URL` | unset | your event sink. Unset, nothing is delivered and the journal still holds every record. `http://` is accepted on loopback only, unless the next variable is set |
| `CELLA_EVENTS_SECRET` | required with the URL | one secret, or two separated by a comma, each signing every delivery, so a secret rotates without losing a record |
| `CELLA_EVENTS_INSECURE_SINK` | unset | `1` admits an `http://` sink on a host that is not loopback. For a test stack only |
| `CELLA_EVENTS_TIMEOUT` | `10s` | the deadline of one delivery, between `1s` and `1m` |
| `CELLA_EVENTS_RETRY_WINDOW` | `24h` | how long a record the sink has not taken is retried before it is dropped and counted. At least `1m` |
| `CELLA_JOURNAL_RETENTION` | `720h` | with a database, how long a delivered or dropped record is kept. At least `1h`; a record still waiting for the sink is kept whatever its age |
| `CELLA_JOURNAL_CAP` | `1000` | without a database, how many records are kept per object |

## Telemetry

`/metrics` on the internal listener is served whether or not anything is
exported. Traces and logs leave the process only when
`OTEL_EXPORTER_OTLP_ENDPOINT` is set. The standard OpenTelemetry
variables, and what `cellad` does with each, are in
[Observability](observability.md).

## The worker

`cellad worker` runs beside your own engine or cluster and connects
outbound to a control plane somebody else operates.
[Self-hosting a data plane](workers.md) is the walkthrough.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_URL` | required | the control plane's public URL, its `CELLA_PUBLIC_URL`, with the path when it has one: `https://api.example.com/v1/environments`. `http://` is accepted on loopback only, unless `CELLA_INSECURE_CONTROL_PLANE` is set |
| `CELLA_ENVIRONMENT_KEY` | required | the environment key an administrator minted for this environment |
| `CELLA_RUNTIME` | `k8s` | the runtime this worker drives, with its variables above: the Kubernetes section, `CELLA_PODMAN_SOCKET`, or `CELLA_ALLOW_UNSAFE_NATIVE` |
| `CELLA_DATA_DIR` | `/var/lib/cella` | local state, as for the control plane |
| `CELLA_CAPACITY_CPU`, `CELLA_CAPACITY_MEMORY`, `CELLA_CAPACITY_DISK`, `CELLA_CAPACITY_SANDBOXES` | unset | what this one worker declares it can hold. Placement admits against it beside the environment's own capacity |
| `CELLA_WORKER_LABELS` | unset | `key=value` pairs that describe this worker to an operator reading the fleet |
| `CELLA_INSECURE_CONTROL_PLANE` | unset | `true` admits an `http://` control plane on a host that is not loopback. For a test stack only |

## The egress gateway

`cellad egress` holds the boundary of every sandbox in one environment
and substitutes secret values on the way out. It connects outbound to the
control plane and serves two doors to sandboxes.

| Variable | Default | What it is |
|---|---|---|
| `CELLA_URL` | required | the control plane's public URL, with its path, as for the worker |
| `CELLA_ENVIRONMENT_KEY` | required | the environment key of the environment this gateway serves |
| `CELLA_EGRESS_PROXY_ADDR` | `:3128` | the proxy door a sandbox's `HTTP_PROXY` and `HTTPS_PROXY` point at |
| `CELLA_EGRESS_REVERSE_ADDR` | `:8080` | the reverse door, for a client that takes a base URL rather than a proxy. It must differ from the proxy door |
| `CELLA_EGRESS_CA_KEY` | generated at start | one PEM value carrying the certificate and the private key of the authority the gateway terminates TLS with. Generated, it lives as long as the process, so an environment with several gateways sets the same value on each |
| `CELLA_EGRESS_CA_BUNDLE` | unset | PEM certificate authorities to trust beside the system roots when the gateway dials an upstream |
| `CELLA_INSECURE_CONTROL_PLANE` | unset | as for the worker |

## The cella command

| Variable | Default | What it is |
|---|---|---|
| `CELLA_URL` | required | the control plane's address, with its path when it is served under one. `--url` overrides it |
| `CELLA_TOKEN` | unset | the bearer to send. `--token` overrides it |
| `CELLA_TOKEN_FILE` | `/run/cella/token` | a file holding the bearer, read on every request, used when `CELLA_TOKEN` is unset. `--token-file` overrides it |

## Set inside a sandbox

A runtime sets these in every sandbox's environment. A manifest may not
set any variable that starts with `CELLA_`, nor the proxy and trust
variables below, because the runtime would overwrite them.

| Variable | Set to |
|---|---|
| `CELLA_TOKEN_FILE` | the path of the sandbox's own workload token, `/run/cella/token` on Podman and Kubernetes. `cella` reads it with no flag |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and their lowercase forms | the gateway's proxy door with the sandbox's credential, when the sandbox runs behind a gateway |
| `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE` | the gateway's certificate authority, `/run/cella/egress-ca.pem` on Podman and Kubernetes, where the runtime projected it |
| `CELLA_GATEWAY_URL`, `CELLA_GATEWAY_CREDENTIAL` | the reverse door and the credential it takes, when the environment has one |

A sandbox taken from a warm pool on Kubernetes is the exception: its
container started before it had a gateway, so the three rows above are
not in its environment until its next start, whose Pod carries them, and
until then its commands reach the gateway only where they are pointed at
it.
| `DISPLAY` | `:0`, the desktop, on a sandbox with a display |
| `<env>_HEADER` or `<env>_QUERY` | for a mounted secret whose value goes somewhere a client would not look by itself: the header or the query parameter to put the placeholder in |

The control plane's address is not set inside a sandbox in this release.
A process that calls the API from inside passes `--url` to `cella`, or
exports `CELLA_URL` in its own shell.

## For tests

These exist so the project's own tests can run. None belongs in an
installation.

| Variable | What it is |
|---|---|
| `CELLA_TEST_DRIFT_DEFAULT` | makes the server resolve one spawn default wrongly, `spec.mesh.spawn.budget` or `spec.mesh.spawn.depth`, so a test can show the conformance suite notices. Accepted only with the native runtime |
