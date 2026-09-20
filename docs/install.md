# Install

From nothing to a sandbox: a cluster, one secret, the manifests, and
`cellad check`. Every command is in a fenced block and the blocks run in
order in one shell, so the document is a program and not a transcript
somebody once ran.

The walk uses a [kind](https://kind.sigs.k8s.io/) cluster on your machine
with everything in memory: one replica, nothing kept across a restart,
which is the right first installation and the wrong second one. The
[deploy manifests](../deploy/README.md) carry the shape of a real one
beside it, `deploy/examples/generic`, with desired state in Postgres, an
authorization endpoint, an event sink and an egress gateway.

## What you need

`kubectl`, `kind`, `curl`, and a container runtime kind can use.

From the release page of the version you are installing:

- the deploy archive, `deploy-<tag>.tar.gz`, unpacked into the directory
  you work in, which gives you `deploy/` with the release's image already
  pinned by digest;
- the image reference, `ghcr.io/<owner>/cellad:<tag>`, where `<owner>` is
  the account the release was published under;
- optionally `cellad_<tag>_<os>_<arch>.tar.gz`, the binary, if you want to
  run the control plane outside a cluster.

Every release is signed and carries a bill of materials and a provenance
attestation. [SECURITY.md](../SECURITY.md) says how to verify one before
you run it; the short form is

```
cosign verify --certificate-identity https://github.com/<owner>/cella/.github/workflows/release.yml@refs/tags/<tag> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com ghcr.io/<owner>/cellad:<tag>
gh attestation verify oci://ghcr.io/<owner>/cellad:<tag> --repo <owner>/cella
```

You also need an **OpenID Connect issuer** whose tokens carry the audience
`cella`, and a token from it. There is no anonymous access and cellad
issues no login of its own: it verifies who is calling and asks somebody
else what they may do. The issuer is yours, a company login or a small one
you run.

## Inputs

The blocks read these and nothing else about your environment. Set the
ones without a default before you start.

```sh
# The release you are installing, and where you unpacked its deploy
# archive, a path relative to this directory.
export CELLA_INSTALL_IMAGE="${CELLA_INSTALL_IMAGE:?set CELLA_INSTALL_IMAGE to the release's image, ghcr.io/<owner>/cellad:<tag>}"
export CELLA_INSTALL_MANIFESTS="${CELLA_INSTALL_MANIFESTS:-deploy}"
# Your issuer, and a token from it with the audience cella. The subject
# that token renders to, <issuer>|<sub>, is the administrator of this
# installation under the built-in owner policy.
export CELLA_INSTALL_ISSUER="${CELLA_INSTALL_ISSUER:?set CELLA_INSTALL_ISSUER to your OpenID Connect issuer's URL}"
export CELLA_INSTALL_TOKEN="${CELLA_INSTALL_TOKEN:?set CELLA_INSTALL_TOKEN to a token from that issuer with the audience cella}"
export CELLA_INSTALL_ADMIN="${CELLA_INSTALL_ADMIN:?set CELLA_INSTALL_ADMIN to that token's subject, <issuer>|<sub>}"
# The namespace everything lands in, and the address the control plane is
# reachable at from this machine.
export CELLA_INSTALL_NAMESPACE="${CELLA_INSTALL_NAMESPACE:-cella}"
export CELLA_INSTALL_URL="${CELLA_INSTALL_URL:-http://localhost:30080}"
```

## A cluster

The kind cluster publishes node port 30080 to `localhost:30080`, which is
where the public listener lands. If you already have a cluster named
`cella`, this leaves it alone.

```sh
kind get clusters | grep -qx cella || \
  kind create cluster --name cella --config "$CELLA_INSTALL_MANIFESTS/examples/kind/kind.yaml"
kubectl cluster-info --context kind-cella
```

Pull the image once and load it into the cluster, so the Pod does not wait
on a registry:

```sh
docker pull "$CELLA_INSTALL_IMAGE"
kind load docker-image --name cella "$CELLA_INSTALL_IMAGE"
```

## The namespace

```sh
kubectl apply -f "$CELLA_INSTALL_MANIFESTS/bootstrap/namespace.yaml"
kubectl get namespace "$CELLA_INSTALL_NAMESPACE"
```

## The signing key

One secret is required in every installation: the RSA key cellad signs the
identity of every sandbox with, and serves the public half of at
`/.well-known/jwks.json`. Any service can verify a sandbox's token against
that key set without asking cellad.

```sh
openssl genrsa -out token.pem 2048
kubectl -n "$CELLA_INSTALL_NAMESPACE" create secret generic cellad-token \
  --from-file=CELLA_TOKEN_KEY=token.pem --dry-run=client -o yaml | kubectl apply -f -
```

Keep `token.pem` where your secrets live and not in a checkout. A rotation
is two PEM blocks in this one value: prepend the new key, apply, and remove
the old block on a later apply, so nothing a sandbox already holds is
refused in between.

The other four Secrets are optional, one per dependency, and
[`deploy/bootstrap/secrets.example.yaml`](../deploy/bootstrap/secrets.example.yaml)
is the template for each with the command that mints its value. This walk
applies none of them, so the installation runs on the built-in owner
policy with every state in memory.

## The manifests

Point the overlay at your issuer and your administrator, and apply it.

```sh
cd "$CELLA_INSTALL_MANIFESTS/examples/kind"
kubectl kustomize . | \
  sed -e "s#https://issuer.example.com|you#${CELLA_INSTALL_ADMIN}#" \
      -e "s#https://issuer.example.com#${CELLA_INSTALL_ISSUER}#" \
      -e "s#http://localhost:30080#${CELLA_INSTALL_URL}#" | \
  kubectl apply -n "$CELLA_INSTALL_NAMESPACE" -f -
cd - > /dev/null
```

`CELLA_PUBLIC_URL` is the address callers actually reach. It is the `iss`
of every token cellad mints and the base of every URL in a response, so a
value that is not what callers dial produces tokens nothing else accepts.

Wait for the control plane:

```sh
kubectl -n "$CELLA_INSTALL_NAMESPACE" rollout status deploy/cellad --timeout=180s
curl -fsS "$CELLA_INSTALL_URL/livez"
curl -fsS "$CELLA_INSTALL_URL/version"
```

## The first sandbox

```sh
curl -fsS -X POST "$CELLA_INSTALL_URL/v1/sandboxes" \
  -H "Authorization: Bearer $CELLA_INSTALL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "apiVersion": "cella.latere.ai/v1beta1",
    "kind": "Sandbox",
    "metadata": {"name": "first"},
    "spec": {
      "image": "docker.io/library/alpine:3.22",
      "command": ["sleep", "3600"]
    }
  }' | tee sandbox.json
```

The body is JSON: `application/json` is the one media type the route
takes today, and `apiVersion` other than `cella.latere.ai/v1beta1` is
refused with `unsupported_version`. The response is the resolved
manifest, with the defaults the installation applied and a `status` that
carries the id and the phase.

A sandbox is addressed by the name its owner gave it, or by the id in
`status.id`. Run something inside it, and then delete it:

```sh
curl -fsS -X POST "$CELLA_INSTALL_URL/v1/sandboxes/first/exec?wait=1" \
  -H "Authorization: Bearer $CELLA_INSTALL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","hello from the sandbox"]}'
curl -fsS -X DELETE "$CELLA_INSTALL_URL/v1/sandboxes/first" \
  -H "Authorization: Bearer $CELLA_INSTALL_TOKEN"
```

## Check the installation

`cellad check` prints one line per requirement and exits 1 on any failure.
It runs with the control plane's own configuration, account and namespace,
so what it answers is what the control plane meets.

```sh
kubectl -n "$CELLA_INSTALL_NAMESPACE" wait --for=condition=complete job/cellad-check --timeout=120s
kubectl -n "$CELLA_INSTALL_NAMESPACE" logs job/cellad-check
```

The same answer from the running Pod, at any time and without a Job:

```sh
kubectl -n "$CELLA_INSTALL_NAMESPACE" exec deploy/cellad -- cellad check
```

Each line is a requirement:

| Line | What it means when it fails |
|---|---|
| `configuration` | a variable is missing or malformed; the line lists every problem at once |
| `identity` | an issuer did not answer, published no usable key, or the signing key cannot sign |
| `authorizer` | the authorization endpoint did not answer, or it allowed the reserved probe, which is an endpoint that is not reading the request |
| `backend` | the cluster did not answer, or the ServiceAccount lacks one of the accesses the driver uses, which the line names |
| `data directory` | `CELLA_DATA_DIR` is not writable, so readiness fails from the first request |
| `admission`, `sink`, `store`, `gateway` | each is reported as not configured when its variable is unset, and answered for when it is set |

## What next

- [deploy/README.md](../deploy/README.md) is the reference for the
  manifests: what each Secret turns on, why the Role grants what it does,
  and how to run the egress gateway beside the control plane.
- `deploy/examples/generic` is the same installation with desired state in
  Postgres, an authorization endpoint, an event sink and a gateway. Every
  one is a Secret you apply and nothing else.
- Upgrading is applying the next release's archive. The image tag equals
  the git tag, so the version you pinned is the version that runs, and
  `cellad version` says which one is in the Pod.

## Removing it

```sh
kind delete cluster --name cella
rm -f token.pem sandbox.json
```
