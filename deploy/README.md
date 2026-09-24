# Deploy

Kustomize manifests for running `cellad` on a Kubernetes cluster. The
install walk that uses them is [docs/install.md](../docs/install.md);
this page is the reference for what each file is and why.

```
base/          the control plane: Deployment, Service, account, Role, policy, budget, check Job
base/prometheusrule.yaml   alert templates, beside the kustomization and outside it
bootstrap/     the Namespace and the Secrets, applied once by hand
examples/kind/     a laptop cluster: memory store, node port, no dependencies
examples/generic/  a cluster an installation runs on: Postgres, an authorizer, a sink, a gateway
```

## Apply order

```sh
kubectl apply -f bootstrap/namespace.yaml
cp bootstrap/secrets.example.yaml secrets.yaml   # fill it, keep it out of a checkout
kubectl -n cella apply -f secrets.yaml
kubectl apply -k examples/generic
kubectl -n cella logs job/cellad-check
```

The Secrets come before the overlay: the Deployment reads
`CELLA_TOKEN_KEY` as a required key, so a Pod applied without it waits
rather than starting without a signing key.

The namespace is applied on its own and is not in any kustomization. A
namespace outlives every apply of what is in it, and an overlay that
owned it would take the sandboxes with the control plane on a
`kubectl delete -k`.

## What the base pins, and what it does not

The base names **no namespace** and **no image registry**. An overlay
sets the namespace, and the image is the placeholder `cellad`, which an
overlay points at a release:

```sh
cd examples/generic && kustomize edit set image cellad=ghcr.io/<owner>/cellad:<tag>
```

The deploy archive attached to a release, `deploy-<tag>.tar.gz`, is this
directory with that reference already pinned to the release by digest, so
an installation from the archive edits nothing to run the version it
downloaded.

## Configuration

Everything an installation chooses is the ConfigMap `cellad`, which the
overlay carries: the public URL, the issuers, the environment's name, the
gateway's doors, and the driver's defaults. Everything that is a
credential is a Secret from `bootstrap/`, and each is read as an optional
key, so a dependency is turned on by applying its Secret and nothing else.

| Secret | Turns on | Without it |
|---|---|---|
| `cellad-token` | required in every mode: the key cellad signs a sandbox's identity with | the Pod does not start |
| `cellad-authorizer` | your authorization endpoint | the built-in owner policy decides, and `CELLA_ADMIN_SUBJECTS` names who acts on everything |
| `cellad-events` | signed delivery of every record to a sink | the journal holds every record and nothing is delivered |
| `cellad-db` | desired state in Postgres, and sealed secret values | every state is in memory: nothing survives a restart, and a sandbox the backend lost is not recovered |
| `cellad-egress` | read by the egress gateway, not by the control plane | a sandbox whose manifest declares a boundary is refused at create |

A URL and the bearer that authorizes it are in one Secret, because a URL
with no bearer is a start-up failure and a bearer with no URL is an
installation that believes it calls an endpoint and does not.

A desktop is one ConfigMap key. Each release publishes the desktop image
beside the control plane's, under the same tag,
`ghcr.io/<owner>/cella-display:<tag>`. With `CELLA_K8S_DISPLAY_IMAGE` set
to it, a sandbox whose manifest names `display` runs that image as a
second container of its Pod, and the screen, screenshot and input routes
answer for it. `CELLA_K8S_DISPLAY_CPU` and `CELLA_K8S_DISPLAY_MEMORY` are
the desktop container's limits, the driver's defaults when unset. Without
the image the environment declares no desktop, and a manifest that asks
for one is refused with the field named.

## The Role

`base/rbac.yaml` grants the accesses the Kubernetes driver uses and nothing
else, all in the one namespace:

| Resource | Verbs | What for |
|---|---|---|
| `pods`, `persistentvolumeclaims` | `get`, `list`, `create`, `delete`, `patch` | a sandbox is one claim and one Pod |
| `pods/exec` | `create`, `get` | commands, terminals, file transfers, the port probe, the desktop's tools |
| `pods/log` | `get` | a sandbox's output |
| `pods/portforward` | `get`, `create` | the dial socket, the port proxy and `cella port-forward` |
| `secrets` | `create`, `get`, `update`, `delete` | the workload token of each sandbox, never a secret value, which stays sealed in the store under `CELLA_SECRET_KEY` |
| `services`, `networkpolicies` | `create`, `delete` | one headless Service and one policy per mesh, and one policy per sandbox, replaced by a delete and a create |

`pods/exec` and `pods/portforward` each need both verbs. The driver opens
each session over a WebSocket and falls back to the older SPDY upgrade
when that is refused; the API server authorizes the WebSocket's `GET` as
`get`, and for an exec from Kubernetes 1.35 as `create` as well, and the
SPDY `POST` as `create`. With `create` alone a session still opens on a
cluster before 1.35, one refused round trip later each time.

The driver proves each access at start with a `SelfSubjectAccessReview`,
and `cellad serve` does not start without all of them, so a rule you
narrow by hand is named in the start-up log, and by `cellad check`, rather
than found at the first create or the first dial.

It grants no `watch` (the driver lists and gets) and nothing cluster-wide.
Reaching a port inside a sandbox needs no NetworkPolicy rule: the kubelet
opens the connection inside the sandbox Pod, and `cellad` only talks to the
API server, on the port its own egress rule already admits. The policy of
each sandbox, which admits no inbound connection but a mesh peer's, does
not stand in the way for the same reason.

The Deployment reads `CELLA_K8S_NAMESPACE` from the downward API, so
sandbox Pods and claims land in the namespace the overlay set and the Role
covers. An installation that wants its workloads in a namespace of their
own sets the variable to that namespace in the ConfigMap and applies the
Role and the RoleBinding there instead.

## One replica

`replicas: 1`, `strategy: Recreate`. The store elects one writer through
its leases table, so a second replica takes no lease, runs no reaper, and
spends database connections; an installation with no database keeps its
desired state on the Pod's own disk, where two processes must never meet.
The disruption budget is written as `maxUnavailable: 1` for the same
reason: `minAvailable: 1` over one replica refuses every node drain.

## Checking an installation

`base/check-job.yaml` runs `cellad check` with the Deployment's own
configuration, account and namespace: one line per requirement, exit 1 on
any failure.

```sh
kubectl -n cella logs job/cellad-check
```

It is a Job and not an init container on purpose. An init container would
make every optional dependency mandatory for a restart, so a sink that was
briefly down would keep the Pod from starting, where `cellad serve` starts
and journals.

A Job's pod template is immutable, so an apply that would change it, an
upgrade to a new image most of all, fails while the finished Job is still
there. It removes itself ten minutes after it ends; inside that window:

```sh
kubectl -n cella delete job cellad-check
```

The same answer from the running Pod, which needs no Job at all:

```sh
kubectl -n cella exec deploy/cellad -- cellad check
```

## Alerts

`base/prometheusrule.yaml` is beside the kustomization and not in it:
`PrometheusRule` is the Prometheus Operator's custom resource, and a
cluster without that operator refuses the apply. Apply it yourself where
you have the operator, after setting the label your Prometheus selects
rules by.

The rules are templates. The metric names are the ones the observability
design fixes, and that design is not built, so every expression evaluates
over an empty series today and no alert can raise. Edit the thresholds
when the metrics land.

## The gateway

`cellad egress` is the second role of the same image: the gateway that
enforces a sandbox's network boundary. This base deploys the control plane
alone. An installation that wants a boundary runs a second Deployment of
the same image with `args: ["egress"]`, `CELLA_URL` pointing at the
control plane, and the `cellad-egress` Secret, and sets three values in the
ConfigMap:

| Key | Value |
|---|---|
| `CELLA_GATEWAY`, `CELLA_GATEWAY_REVERSE` | the two doors as a sandbox dials them, usually the gateway's Service |
| `CELLA_K8S_GATEWAY_SELECTOR` | the labels of the gateway's Pods, for example `app.kubernetes.io/name=cellad-egress` |
| `CELLA_K8S_GATEWAY_NAMESPACE`, `CELLA_K8S_GATEWAY_PORTS` | only where the gateway runs in another namespace, or its Pods listen on other ports than `3128` and `8080` |

With the labels named, the driver writes a NetworkPolicy for every sandbox
that admits cluster DNS, the gateway's Pods and the sandbox's own mesh
peers, and nothing else, and the environment enforces the `none`,
`allowlist` and `open` boundaries. Without them each sandbox's policy
admits no inbound connection and limits nothing outbound, and a boundary is
recorded and not enforced. The cluster's network plugin has to enforce
NetworkPolicy, egress included.

The environment key is minted at the control plane once it runs,
`POST /v1/environments/<name>/keys` with an administrator's token, so the
gateway's Deployment starts after the control plane's. The gateway's own
Pods want two policies of the installation's: ingress on the two doors
from the sandboxes alone, and egress to the upstreams sandboxes may reach
and to the control plane's URL, with the cluster's private ranges left out
so the gateway is no route into the cluster.
`examples/kind-stubs/gateway.yaml` is the test stack's gateway, and
`examples/kind-stubs/up.sh` mints its key.
