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
| `cellad-authorizer` | the authorization endpoint of spec 006 | the built-in owner policy decides, and `CELLA_ADMIN_SUBJECTS` names who acts on everything |
| `cellad-events` | signed delivery of every record to a sink | the journal holds every record and nothing is delivered |
| `cellad-db` | desired state in Postgres, and sealed secret values | every state is in memory: nothing survives a restart, and a sandbox the backend lost is not recovered |
| `cellad-egress` | read by the egress gateway, not by the control plane | a sandbox whose manifest declares a boundary is refused at create |

A URL and the bearer that authorizes it are in one Secret, because a URL
with no bearer is a start-up failure and a bearer with no URL is an
installation that believes it calls an endpoint and does not.

## The Role

`base/rbac.yaml` grants the accesses the Kubernetes driver uses and
nothing else, in the one namespace it writes in:

| Resource | Verbs | What for |
|---|---|---|
| Pods, PersistentVolumeClaims | `get`, `list`, `create`, `delete`, `patch` | one claim and one Pod per sandbox |
| `pods/exec` | `create`, `get` | commands, terminals and file transfers |
| `pods/log` | `get` | a sandbox's logs |
| Secrets | `create`, `get`, `update`, `delete` | a sandbox's identity token, never a secret value |
| Services, NetworkPolicies | `create`, `delete` | a mesh's headless Service and the policy that admits its members |

`pods/exec` needs both verbs. The driver opens each exec over a WebSocket
and falls back to the older SPDY upgrade when that is refused; the API
server authorizes the WebSocket's `GET` as `get`, and from Kubernetes 1.35
as `create` as well, and the SPDY `POST` as `create`. A Role with `create`
alone still runs commands on a cluster before 1.35, one refused round trip
later each time.

The driver proves each access at start with a `SelfSubjectAccessReview`,
so a rule you narrow by hand is named in the start-up log, and by
`cellad check`, rather than found at the first create. It grants no
`watch` (the driver lists and gets) and nothing cluster-wide.

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
control plane, and the `cellad-egress` Secret, and sets `CELLA_GATEWAY` in
the ConfigMap to the doors that Deployment serves.
