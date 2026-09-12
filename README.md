# Cella

**Declarative sandboxes for agents and workloads.** A manifest in the
shape of a Kubernetes object describes an environment. `cellad` makes it
exist on Kubernetes, on Podman, or on the host, keeps it alive for as
long as the manifest says, and lets you run commands, attach a
terminal, and move files in and out. Identity comes from any OpenID
Connect issuer. Permission comes from an endpoint you write.

Latere runs Cella inside its hosted platform; this repository is the
open core that platform is built on, and anyone can run it.

[![CI](https://github.com/latere-ai/cella/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/cella/actions/workflows/verify.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/cella)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## Status

Design. The specs are written and the repository passes its quality
gate. `cellad` serves its probes and creates nothing yet; the
[build order](specs/README.md#build-order) says what lands when. The
schema below may still change before the first tagged release, and the
CHANGELOG names every change to it.

## The problem

An agent needs somewhere to run code that is not your machine and not
production: a Linux environment with the right image, a working tree,
credentials scoped to the task, network access to the hosts the task
needs and no others, and a lifetime that ends on its own. Sandbox
services exist, and each has its own API, its own idea of identity, and
its own account system, so a platform that composes one is written
against that vendor.

Cella makes the environment a document and the service a component.

## How it works

- **One manifest, one meaning.** `apiVersion: cella/v1`, `kind:
  Sandbox`. Every surface, the API, the `cella` command, and a
  platform importing the packages, resolves a manifest through one
  function, and what you read back is what runs, defaults included.
- **Three backends, one contract.** A Pod and a volume on Kubernetes,
  a container and a volume on Podman, a directory and a process on the
  host. Each declares what it can enforce and passes one conformance
  suite.
- **Identity in, decisions out.** `cellad` verifies bearer tokens from
  the OIDC issuers you list and asks an authorizer endpoint you write
  whether the caller may act. No decision is a refusal. The only tokens
  it mints identify sandboxes, so a process inside one can call back
  with an identity that dies with it.
- **The backend is the truth.** A sandbox's state lives in the labels
  of the object that runs it. `cellad` rebuilds its index from the
  backend at start; Postgres is optional and holds history, not truth.

```yaml
apiVersion: cella/v1
kind: Sandbox
metadata:
  name: dev
spec:
  image: ghcr.io/example/sandbox:1.4
  workspace:
    source: git
    git: { url: https://github.com/example/repo.git, ref: main }
  network:
    allowedHosts: ["*.github.com", "pypi.org"]
  lifecycle: { autoStop: 15m, ttl: 24h }
```

```sh
cella apply -f sandbox.yaml -w
cella exec dev -- make test
cella cp dev:/workspace/out ./out
```

## Try it

```sh
make run   # cellad on loopback with the native backend
make       # the quality gate
```

Today `make run` serves the probes at `http://127.0.0.1:8081/readyz`.
Once the backends and the API land it starts the stub issuer,
authorizer, and sink beside the server and prints a token to apply the
manifest above with.

## What you get

- A schema with strict decoding, server-side defaults, and a `status`
  the server writes, evolving under written rules.
- The three backends and the conformance suite a fourth must pass.
- A lifecycle: create, start, stop, delete, idle auto-stop, TTL,
  deadline, and a warm pool for the common shape.
- Exec, attach, file transfer, and logs as streams.
- OIDC from any issuer, an authorizer webhook with a built-in owner
  policy, an admission webhook with built-in defaults and ceilings, and
  a signed event sink.
- Go packages a platform imports: `manifest`, `runtime`, `controller`.
- The `cella` command and a skill file that teaches an agent to use it.
- Signed images, SBOMs, and provenance on every release.

## Documentation

| Page | |
|---|---|
| [Specs](specs/README.md) | the design, one spec per component, with the build order |
| [Architecture](specs/001-architecture.md) | components, packages, what the core owns and what a platform supplies |
| [Manifest contract](specs/003-manifest-contract.md) | every field, every rule, every error code |
| [Building a plane](specs/016-building-a-plane.md) | how a platform composes the packages and the webhooks |
| [docs/](docs/README.md) | for people who run `cellad` or build against it |

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build, the bar, and
where a package belongs. [`SECURITY.md`](SECURITY.md) is where to
report a vulnerability.

## License

Apache-2.0. See [LICENSE](LICENSE).
