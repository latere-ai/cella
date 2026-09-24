# Cella

**An open source control plane for sandboxes.** A manifest in the shape
of a Kubernetes object describes the environment an agent or a workload
runs in: its image, its resources, the credentials it may use, the hosts
it may reach, what it may spawn, and how long it lives. `cellad` makes
that environment exist on a data plane and lets you run commands, attach
a terminal, drive a screen, reach its ports, and move files in and out.
The data plane is a cluster or a machine `cellad` drives directly, or
your own infrastructure running a worker that connects outbound.
Identity comes from any OpenID Connect issuer. Permission comes from an
endpoint you write.

Latere runs Cella inside its hosted platform; this repository is the
control plane that platform is built on, and anyone can run it.

[![CI](https://github.com/latere-ai/cella/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/cella/actions/workflows/verify.yml)
[![Release](https://img.shields.io/github/v/release/latere-ai/cella)](https://github.com/latere-ai/cella/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/cella)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## The problem

An agent needs somewhere to run code that is not your machine and not
production: a Linux environment with the right image, a working tree
that outlives the process, credentials scoped to the task that the code
itself never sees, network access to the hosts the task needs, and a
lifetime that ends on its own. A training loop needs a thousand such
environments, each different, none kept warm. Sandbox services exist,
and each has its own API, its own idea of identity, its own account
system, and its own opinion about where the sandbox runs, so a platform
that composes one is written against that vendor.

Cella makes the environment a document and the control plane a
component you run.

## A sandbox

A manifest names what the sandbox runs and the boundary it runs inside:

```json
{
  "apiVersion": "cella.latere.ai/v1beta1",
  "kind": "Sandbox",
  "metadata": {"name": "dev"},
  "spec": {
    "image": "docker.io/library/python:3.13-alpine",
    "command": ["sleep", "3600"],
    "resources": {"cpu": "2", "memory": "4Gi"},
    "secrets": [{"name": "github-token", "env": "GITHUB_TOKEN"}],
    "network": {"ports": [{"name": "web", "port": 8080}]},
    "lifecycle": {"autoStop": "15m", "ttl": "24h"}
  }
}
```

The `cella` command applies it and works inside it:

```sh
cella apply -f github-token.json --value-from-env GITHUB_TOKEN
cella apply -f dev.json -w
cella exec dev -- python3 -c 'print("hello")'
cella cp ./src dev:/workspace/src
cella port-forward dev 8080:8080
cella delete sandbox dev
```

`GITHUB_TOKEN` inside the sandbox is a placeholder, not the token. The
egress gateway swaps it for the value on requests to the hosts the
`Secret` names, and nowhere else. What you read back from the API is the
manifest as it runs, with every default filled in and a `status` the
server writes.

## What you get

- **Three kinds.** `Sandbox`, `Secret`, and `Environment`, decoded
  strictly from JSON or YAML, with server-side defaults and one error
  code per refusal naming the field at fault.
- **Runtimes.** Kubernetes (a claim and a Pod per sandbox), Podman (a
  container per sandbox), and a native runtime that runs host processes
  for trusted development. A `cellad worker` runs any of the three on
  your own infrastructure and connects outbound to a control plane
  somebody else operates.
- **A lifecycle.** Create, apply by name, start, stop, delete, idle
  auto-stop, a time to live, auto-delete after a stop, and recovery of a
  lost sandbox from desired state when state lives in Postgres.
- **Operations as streams.** Exec with or without a terminal, attach,
  logs, whole-tree and single-file transfers, ports reached through an
  HTTP proxy or a raw byte stream, and a virtual desktop with
  screenshots, a live screen, and pointer and keyboard input.
- **Secrets that stay out of the sandbox.** A `Secret` holds a value or
  an OAuth 2.0 client-credentials grant, stored under envelope
  encryption. The sandbox holds a placeholder, and the egress gateway
  substitutes the value toward the hosts the secret's owner scoped.
- **Identity inside every sandbox.** A workload token at
  `/run/cella/token`, verifiable offline against the published key set,
  re-minted before it expires and refused once its sandbox is gone.
- **Spawn and mesh.** A sandbox may create sandboxes within a budget and
  a depth, each child a subset of its parent, and a tree may share a
  private network.
- **Scheduling.** Start now or fail, or queue against an environment's
  capacity by priority and fair share, with preemption and warm pools of
  prewarmed sandboxes.
- **Three webhooks for your policy.** An authorizer decides every
  request, an admission endpoint rewrites or refuses every manifest, and
  a signed event sink receives one record per change. Each has a
  built-in default, so none is required.
- **A command and a skill.** `cella` speaks the API from a shell and from
  inside a sandbox, where it needs no flag and no login, and
  [`skills/cella`](skills/cella/SKILL.md) teaches an agent to use it.
- **A conformance suite** that holds any server claiming the API to it,
  including a platform's own front.
- **Packages to build on.** `manifest`, `runtime`, `controller`, and
  `egress` are what `cellad` is made of, for a platform that wants the
  contract in its own binary.
- **Signed releases.** Images, binaries, and a deploy archive, with SPDX
  bills of materials and build provenance.

## What is not in this release

The design covers more than this release serves. A manifest that asks
for one of these is refused with `capability_unsupported` or
`unsupported_kind`, never accepted and ignored.

- The `Volume` and `SandboxSet` kinds. The authorizer vocabulary names
  their actions; the API does not serve them.
- A workspace filled from git or from a volume. `spec.workspace.source`
  takes `empty`.
- A public endpoint for a port (`expose: public`). No runtime provides
  one.
- Egress confined to the gateway outside Kubernetes. The Podman and native
  runtimes declare no enforced egress mode, and neither does Kubernetes
  until its operator names the gateway's Pods; there a boundary other than
  `open` is recorded, the sandbox reports `EgressEnforced` false, and the
  create answers with a warning. The gateway still decides and records
  every connection that reaches it, and substitutes secret values.
- Ports and the desktop on an environment a self-hosted worker serves.
- A virtual machine runtime.

## Try it

You need Go 1.27 or newer, `openssl`, and `curl`.

```sh
make run   # the stubs and cellad on loopback, with a token to call it with
make       # the quality gate
```

`make run` needs nothing of your own. It starts `cella-stubs`, which
serves an OpenID Connect issuer, an authorization endpoint, an admission
endpoint, and an event sink on loopback, then starts `cellad serve` wired
to the issuer, the authorization endpoint, and the sink, and prints the command that mints a caller token and the `curl`
that creates the first sandbox. Interrupt it to stop both. The signing
key and the sink's secret are generated once under `out/run/` and kept,
so a restart does not invalidate the token you are holding; `make clean`
removes them. A second clone runs beside the first with
`CELLA_RUN_PORT=8090 make run`.

`cella-stubs` is a test binary. It answers what a flag tells it to
answer, holds nothing across a restart, and belongs in no installation:
in a real one, each of those four endpoints is yours.

`make run` uses the native runtime, which runs commands on this host with
no isolation and no image. [The native quickstart](docs/native.md) is
what it can and cannot do. To develop against an isolated runtime on a
laptop, start `cellad serve` yourself with `CELLA_RUNTIME=podman`.

## Install it

[`docs/install.md`](docs/install.md) goes from a cluster to a first
sandbox and `cellad check`. Every release publishes the image
`ghcr.io/<owner>/cellad:<tag>` under the account that pushed the tag, the
`cellad` and `cella` binaries for Linux and macOS, and
`deploy-<tag>.tar.gz`, the Kubernetes manifests with the image pinned by
digest.

You supply an OpenID Connect issuer, a signing key, and a hostname. An
authorization endpoint, an event sink, Postgres, and an egress gateway
are each optional, and the manifests turn each one on with one Secret.
An admission endpoint is optional too.

## Identity

`cellad` knows who is calling and asks somebody else what they may do.
There is no anonymous access and no API key.

- **Who.** `CELLA_OIDC_ISSUERS` lists the issuers you trust. A bearer is
  accepted when a listed issuer signed it, its `aud` names an entry of
  `CELLA_OIDC_AUDIENCE` (`cella` by default), it has not expired, and it
  was minted less than 24 hours ago. A subject is the issuer and the
  `sub` joined, `https://login.example.com|alice`, and every claim of the
  token reaches your authorizer as it arrived.
- **What.** `CELLA_AUTHORIZER_URL` points at an endpoint you write, which
  answers allow or deny per subject, action, and object. Anything that is
  not a decision refuses the request. Unset, the built-in owner policy
  applies: you create anything but an environment, you act on what you
  own, and the subjects in `CELLA_ADMIN_SUBJECTS` act on everything.
  [`latere.ai/x/cella/authorizer`](authorizer) publishes the actions, so
  an endpoint written in Go keeps no copy of the strings.
- **What cellad signs.** `CELLA_TOKEN_KEY` is one or two RSA keys. The
  first signs every sandbox's identity and every environment key, and
  every key is published at `/.well-known/jwks.json`. A rotation is
  prepending a key and later removing the old one.

```sh
openssl genrsa -out token.pem 2048

CELLA_RUNTIME=podman CELLA_DATA_DIR="$PWD/state" \
CELLA_OIDC_ISSUERS=https://login.example.com \
CELLA_PUBLIC_URL=https://cella.example.com \
CELLA_TOKEN_KEY="$(cat token.pem)" \
CELLA_ADMIN_SUBJECTS='https://login.example.com|alice' \
  cellad serve
```

[`docs/configuration.md`](docs/configuration.md) is every variable, and
[`docs/plane.md`](docs/plane.md) is how the authorizer, the admission
endpoint, and the event sink fit a platform of your own.

## Documentation

| | |
|---|---|
| [Native quickstart](docs/native.md) | create and drive a sandbox on your own machine |
| [Install](docs/install.md) | a cluster, the release's manifests, the first sandbox, and `cellad check` |
| [Configuration](docs/configuration.md) | every environment variable, its default, and which role reads it |
| [The cella command](docs/cli.md) | the client: commands, output, and exit codes |
| [Manifests](docs/manifest.md) | every field of `Sandbox`, `Secret`, and `Environment`, with defaults |
| [API](docs/api.md) | the `/v1` routes, authentication, errors, and the OpenAPI document |
| [Sandboxes on Kubernetes](docs/kubernetes.md) | what a sandbox on a cluster can do and what the Role allows |
| [Self-hosting a data plane](docs/workers.md) | running sandboxes on your own machines with `cellad worker` |
| [Capacity and queues](docs/scheduling.md) | capacity, queues, priority, preemption, and warm pools |
| [Following events](docs/events.md) | an object's history and the live feed |
| [Observability](docs/observability.md) | metrics, traces, logs, and alert rules |
| [Building a plane](docs/plane.md) | a platform on top of Cella, through the webhooks or the packages |
| [Conformance](docs/conformance.md) | running the suite against an installation |

[`docs/README.md`](docs/README.md) is the index, and
[`deploy/README.md`](deploy/README.md) is the reference for the manifests.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build and test a change,
the quality bar, and where a package belongs. The design, and the
reasoning behind each decision, is in [`specs/`](specs/README.md).
[`SECURITY.md`](SECURITY.md) is what the project protects and where to
report a vulnerability; please do not open an issue for one.

## License

Apache-2.0. See [LICENSE](LICENSE).
