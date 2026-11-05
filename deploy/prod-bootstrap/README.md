# Production bootstrap

The release pipeline never reads this directory. Everything it applies runs
again on every release, and `deploy/prod` holds only that: the two
Deployments (`cellad` and `cellad-egress`), their Services, accounts,
disruption budgets, the Ingress and the policies that select the control
plane's own Pods, all in the `latere` namespace. This directory holds what
the pipeline's rollout identity may not write, or must not apply again.

It is Latere's own installation. The install path for everyone else is
`deploy/bootstrap` and the overlays under `deploy/examples`.

## What an operator applies

| File | Objects | Why it is not in `deploy/prod` |
|---|---|---|
| `namespace.yaml` | Namespace `cella-workloads` | cluster scoped; a namespace outlives every apply of what is in it |
| `storageclass.yaml` | StorageClass `cella-block-storage` | cluster scoped |
| `admissionpolicy.yaml` | ValidatingAdmissionPolicy and binding `cella-workloads-sandbox-pool` | cluster scoped |
| `rbac.yaml` | ServiceAccount `default`, Role and RoleBinding `cellad-workloads` in `cella-workloads`; ClusterRole and ClusterRoleBinding `cellad-storageclass` | the grant of the control plane over the sandboxes' namespace, and two cluster scoped objects; an identity that applied them could grant itself anything |
| `networkpolicy.yaml` | `cella-workloads-default-deny`, `cella-workloads-dns` and `cella-workloads-to-gateway` in `cella-workloads`; `cellad-platformd-tls` in `latere` | the first three are the sandboxes' boundary in another namespace; the last selects platformd's Pods, which this service's identity may not select |
| `prepull.yaml` | DaemonSet `cella-prepull` in `cella-workloads` | another namespace, and its list of images follows the image catalog, which moves on platformd's release, not on this one |
| `rollout-identity.yaml` | ServiceAccount, Role, RoleBinding and token Secret `cellad-rollout` in `latere` | the credential of the pipeline itself |

The Secrets the Deployments read (`cellad-oidc`, `cellad-token`,
`cellad-authz`, `cellad-admission`, `cellad-events`, `cellad-db`, `cella-db`,
`cellad-egress`, `ghcr-secret`, and `platformd-internal-ca` in `latere`, and
`ghcr-pull` in `cella-workloads`) are created outside this repository and
keep their names. `cellad` runs its database migrations at start, so there is
no migration Job to run before a release.

Apply a file with `kubectl apply -f deploy/prod-bootstrap/<file>`, once, and
again only when the file changes. `kubectl diff -f` shows what an apply
would change.

## The rollout identity

`rollout-identity.yaml` is the account the release pipeline deploys with: a
ServiceAccount, a Role and RoleBinding in the `latere` namespace over the
kinds `deploy/prod` contains, and the token Secret bound to the account. A
kind added to `deploy/prod` is added to the Role and applied again by an
operator.

```sh
kubectl apply -f deploy/prod-bootstrap/rollout-identity.yaml

NS=latere SA=cellad-rollout
kubeconfig=$(mktemp) && chmod 600 "$kubeconfig"
KUBECONFIG="$kubeconfig" kubectl config set-cluster latere-k8s \
  --server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
KUBECONFIG="$kubeconfig" kubectl config set clusters.latere-k8s.certificate-authority-data \
  "$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.ca\.crt}')"
KUBECONFIG="$kubeconfig" kubectl config set-credentials "$SA" \
  --token="$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.token}' | base64 -d)"
KUBECONFIG="$kubeconfig" kubectl config set-context "$SA" \
  --cluster=latere-k8s --user="$SA" --namespace="$NS"
KUBECONFIG="$kubeconfig" kubectl config use-context "$SA"

# The server-side dry run authorizes every object a release applies without
# changing any, so a Role that misses a kind fails here and not in a release.
KUBECONFIG="$kubeconfig" kubectl apply -k deploy/prod --dry-run=server

gh secret set CELLA_KUBECONFIG --repo latere-ai/cella < "$kubeconfig"
rm "$kubeconfig"
gh variable set CELLA_RELEASE_DEPLOY --repo latere-ai/cella --body 1
```

The `deploy` job of `.github/workflows/release.yml` runs only while the
repository variable `CELLA_RELEASE_DEPLOY` is set, so a fork, which sets
neither it nor the secret, publishes every artifact and deploys nothing.
Unsetting the variable turns the deploy off again.

Rotating the credential is deleting the token Secret, applying the identity
file again, and running the same steps.

## Where the pipeline's pins come from

`deploy/prod/kustomization.yaml` names the release the target runs under
`images`, and `lateregate release` moves that tag, and the display image's tag
in `deploy/prod/deployment.yaml`, in the commit that writes the changelog
section. The pipeline pins `cellad` by digest before it applies, and the image
a volume is filled from follows it.
