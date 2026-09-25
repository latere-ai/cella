---
title: "Sandbox trust bundle: the file the trust variables name holds the public roots and the gateway's authority, so a tunneled host verifies as well as a terminated one"
status: in-progress
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/012-test-stubs-and-tiers.md
  - specs/018-egress-and-secrets.md
  - specs/021-data-plane-workers.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/039-egress-gateway.md
  - specs/.archive/070-k8s-egress.md
affects: [egress/, runtime/native/, runtime/podman/, runtime/k8s/, runtime/, cmd/cellad/, docs/, CHANGELOG.md]
effort: small
created: 2026-09-25
updated: 2026-09-25
author: changkun
---

# Sandbox trust bundle

## Overview

A sandbox behind a gateway has `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`,
`CURL_CA_BUNDLE`, `GIT_SSL_CAINFO` and `NODE_EXTRA_CA_CERTS` set to
`egress.CAPath`, `/run/cella/egress-ca.pem` in a container. The file
holds the gateway's authority and nothing else.

The gateway terminates TLS only toward a destination a mounted secret is
bound to (`decision.terminate` in `internal/egressd/gate.go`). Every other
admitted host is tunneled byte for byte (`tunnel`), so the workload sees
the upstream's own certificate. OpenSSL, Python's requests, curl, git and
Go take `SSL_CERT_FILE` and the matching variables as the whole trust
store, not as an addition to it. Every HTTPS request to a host without a
bound secret therefore fails verification inside such a sandbox: `pip
install`, `curl https://github.com`, `git clone`, `go get`. Node is
unaffected, because `NODE_EXTRA_CA_CERTS` adds to its own roots.

The k8s driver has projected the gateway since v0.6.0 ([[070-k8s-egress]]),
so every sandbox of a cluster installation with a gateway on v0.6.0 or
v0.6.1 is affected. The podman and native drivers share the projection and
have carried the defect since [[039-egress-gateway]].

No test caught it. The conformance upstream is plain HTTP on port 80, and
the in-process end-to-end test builds its client with an explicit pool
rather than reading the file the sandbox holds.

## Current state

| Piece | Where | What it does |
|---|---|---|
| The trust variables | `egress.Projection.Env` | names `CAPath` in five variables when a driver projected a file |
| The authority | `api.EgressHub.CA`, `controller.egressSpec` | the first connected gateway's authority, handed to the driver as `runtime.Egress.CAPEM` |
| native | `runtime/native/egress.go` `projectEgress` | writes `CAPEM` to `egress-ca.pem` in the sandbox's directory at create and adoption |
| podman | `runtime/podman/egress.go` `putEgressCA` | copies `CAPEM` into the container at `CAPath` at create and adoption |
| k8s | `runtime/k8s/lifecycle.go` `Create`, `pool.go` `projectAdopted` | writes `CAPEM` into the sandbox's Secret under `egress-ca.pem`, projected at `CAPath` |
| Rotation | `controller.rotateLocked`, each driver's `Update` with `Change.Token` | rewrites the token alone; on k8s only the `token` key of the Secret |
| Start | each driver's `Start` | never rewrites the file, so a stop and a start keep what the create wrote |
| The spec record | k8s claim annotation `cella.latere.ai/spec`, the native and podman records | `runtime.CreateSpec` as JSON, `Egress.CAPEM` included |

## Design

### Where the roots come from

The public roots are the system's, read once at start by each role that
builds a driver: `cellad serve` and `cellad worker`. The file is the one
`SSL_CERT_FILE` names in that process's own environment, and without it
the first file of the list Go's `crypto/x509` reads on Linux that holds a
certificate:

1. `/etc/ssl/certs/ca-certificates.crt`
2. `/etc/pki/tls/certs/ca-bundle.crt`
3. `/etc/ssl/ca-bundle.pem`
4. `/etc/pki/tls/cacert.pem`
5. `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem`
6. `/etc/ssl/cert.pem`

The released image is `gcr.io/distroless/static-debian12`, which ships the
first. `egress.LoadRoots(getenv)` reads it, keeps every `CERTIFICATE`
block that parses as an X.509 certificate, re-encodes each as PEM, and
answers the bytes, the count and the file they came from.

Why the system bundle and not an embedded one:

| Criterion | The system bundle | An embedded Mozilla bundle |
|---|---|---|
| available | `golang.org/x/crypto/x509roots/fallback` is its own module, not the `x/crypto` this module already requires; its exported behavior is an `init` that calls `x509.SetFallbackRoots`, and it exports no certificate bytes | a new dependency, and the certificates would have to be copied out of an unexported table |
| what it trusts | what `cellad` itself trusts, which is also what the gateway verifies an upstream against when it terminates; a tunneled host and a terminated one verify against one public set | NSS roots, several with a distrust-after constraint that a PEM file cannot carry, so writing them out widens trust |
| an operator's own roots | carried: a root installed into the image, or a bundle named by `SSL_CERT_FILE` on `cellad`, reaches every sandbox | dropped |
| freshness | follows the base image's `ca-certificates` | follows this module's releases |
| hermetic and testable | the image pins one file; a test names its own through the role's `getenv`, which is how the end-to-end test installs a test root as the system's | hermetic, with nothing to inject |

`SSL_CERT_DIR` and the certificate directories Go also scans are not read:
a directory holds a hashed link per certificate and may hold other files,
and every distribution in the list ships the bundle file. An operator whose
host keeps only a directory names a bundle with `SSL_CERT_FILE`.

### A role that finds no roots refuses to start

A sandbox pointed at a gateway with the authority alone is the defect this
spec removes, and a log line at start is read after the sandboxes fail. So
a role that needs roots and finds none refuses to start, with one sentence
naming `SSL_CERT_FILE` and the files it tried.

| Role | Reads the roots | Finding none |
|---|---|---|
| `cellad serve` with `CELLA_GATEWAY` set | yes | refuses to start |
| `cellad serve` without `CELLA_GATEWAY` | no: no sandbox is pointed at a gateway, so no trust variable is ever set | nothing to refuse |
| `cellad worker` | yes: the control plane decides whether the worker's sandboxes are pointed at a gateway, and the worker cannot know | refuses to start; its own stream to an `https` control plane verifies against the same files, so such a host could not run a worker anyway |
| `cellad egress` | no: the gateway projects nothing into a sandbox | unchanged |

`SSL_CERT_FILE` set to a file that is missing, unreadable or holds no
certificate is refused whatever the role, since the operator named it. The
roots are bounded at 512 KiB (`egress.MaxRootsBytes`): a Kubernetes Secret
holds at most 1 MiB, and the sandbox's Secret carries the token beside
them. A system bundle is about 220 KB.

### The bundle, and where it is composed

`egress.TrustBundle(roots []byte, authority string) []byte` is the file:
the roots, then the authority, each block ending in a newline. Empty roots
give the authority alone, which is what a driver built without roots
writes. An empty authority gives nothing, and no trust variable is set, as
today.

The driver composes it, from roots it was given at construction and the
authority in `runtime.Egress.CAPEM`:

| Driver | Takes the roots | Writes the bundle |
|---|---|---|
| native | `(*native.Driver).SetTrustRoots` | at create and adoption, to `egress-ca.pem` in the sandbox's directory |
| podman | `podman.Options.TrustRoots` | at create and adoption, into the container at `CAPath` |
| k8s | `k8s.Options.TrustRoots` | at create and adoption, and at every token rotation and start, into the Secret's `egress-ca.pem` |
| remote | none | the worker's own driver writes the worker host's roots |

`runtime.Egress.CAPEM` stays the gateway's authority. The k8s driver keeps
the create spec as a claim annotation, and Kubernetes caps a resource's
annotations at 256 KiB, so the roots are never in the spec, the claim, the
native or podman record, or on the worker stream. A worker composes with
its own host's roots, which are the roots of the network its sandboxes
send from.

### One file, `NODE_EXTRA_CA_CERTS` included

`NODE_EXTRA_CA_CERTS` keeps naming the same file. Node adds the file's
certificates to its own roots, so the public roots in it are duplicates it
already holds and the gateway's authority is the one addition. One file is
one path in the reserved-variable table and one key in the Secret.

### Sandboxes that already run

A sandbox created before this change holds the authority alone until its
file is written again.

| Driver | What rewrites it |
|---|---|
| k8s | the next token rotation, which runs at two thirds of the token's life, rewrites `egress-ca.pem` beside the token and the kubelet syncs the mounted file into the running container; a start writes it before the new Pod. A sandbox whose token expires with the sandbox itself is never rotated, and a stop and a start rewrite it |
| podman, native | nothing: neither record keeps the authority, so the sandbox is deleted and created again |

A Kubernetes Secret's projection follows the Secret, because the driver
mounts the directory and not a `subPath` of the key.

## Not in this spec

| Item | Why |
|---|---|
| An HTTPS upstream in the conformance case `case018EgressEnforced` or the kind tier | feasible without a public CA: the stack's `cellad` would read a test root through `SSL_CERT_FILE` mounted from a ConfigMap, and the upstream would serve a certificate from it. It needs a TLS upstream in place of the netcat echo, a verifying client that honors `SSL_CERT_FILE` in the sandbox's image (the probe is written for busybox, whose `wget` does not read the variable), and that file becomes `cellad`'s own trust for every call it makes to the stubs. The `cmd/cellad` egress tier proves the same path over the native driver with a stock client, and the k8s driver's composition is proved over the fake clientset |
| Rewriting the file on podman and native at rotation or start | neither keeps the authority beside the sandbox; the fix for them is a create |
| Reading `SSL_CERT_DIR` | see the roots above |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The bundle is the roots, then the authority, each block on its own lines; empty roots give the authority alone; no authority gives nothing | `TestTrustBundle` | not built |
| `SSL_CERT_FILE` wins over the list; the list is read in Go's order and a file with no certificate is passed over; a named file that is missing or empty is refused; nothing found is refused; non-certificate blocks are dropped; roots over the bound are refused | `TestLoadRoots` | not built |
| The native driver writes the bundle at create | `TestNativeProjectsTheTrustBundle` | not built |
| The podman driver writes the bundle at create | `TestPodmanProjectsTheTrustBundle` | not built |
| The k8s driver writes the bundle into the Secret at create and adoption, and rewrites it at a rotation and a start | `TestK8sTrustBundleInTheSecret` | not built |
| `cellad serve` with `CELLA_GATEWAY` and `cellad worker` refuse to start without roots | `TestServeRefusesAGatewayWithoutPublicRoots`, `TestTheWorkerRefusesWithoutPublicRoots` | not built |
| A workload reaches an HTTPS host tunneled through the proxy door, verifying against the projected file alone, where the host's certificate chains to a root the control plane read as the system's | `TestTheSandboxTrustsThePublicRootsAndTheGateway` | not built |
| The same workload reaches a host a mounted secret is bound to, terminated by the gateway, verifying the gateway's leaf against the same file | `TestTheSandboxTrustsThePublicRootsAndTheGateway` | not built |
