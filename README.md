# Cella

**An open source control plane for sandboxes.** A manifest in the shape
of a Kubernetes object describes the environment an agent or a workload
runs in: its image, its storage, the credentials it may use, the hosts
it may reach, what it may spawn, and how long it lives. `cellad` makes
that environment exist on a data plane, keeps it inside the boundary
the manifest declared, and lets you run commands, attach a terminal,
drive a screen, and move files in and out. The data plane is a cluster
or a machine `cellad` drives directly, or your own infrastructure
running a worker that connects outbound. Identity comes from any OpenID
Connect issuer. Permission comes from an endpoint you write.

Latere runs Cella inside its hosted platform; this repository is the
control plane that platform is built on, and anyone can run it.

[![CI](https://github.com/latere-ai/cella/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/cella/actions/workflows/verify.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/cella)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## Status

The first executable slice supports trusted native workspaces: authenticated
create, list, inspect, exec, files, logs, stop, start, and delete, with local state that
survives a server restart. Native execution has **no isolation** and requires
`CELLA_ALLOW_UNSAFE_NATIVE=true`; it is for development and tests.

The full contract below remains the target. Container drivers, credential
egress, independent volumes, queued scheduling, workers, and application
ports are not implemented yet. Unsupported manifest fields are refused.
The [build order](specs/README.md#build-order) tracks the remaining work.

## The problem

An agent needs somewhere to run code that is not your machine and not
production: a Linux environment with the right image, a working tree
and storage that outlives the process, credentials scoped to the task
that the code itself never sees, network access to the hosts the task
needs and no others, and a lifetime that ends on its own. A training
loop needs a thousand such environments, each different, none kept
warm. An application an agent built needs one that keeps its state and
answers on a port. Sandbox services exist, and each has its own API,
its own idea of identity, its own account system, and its own opinion
about where the sandbox runs, so a platform that composes one is
written against that vendor.

Cella makes the environment a document and the control plane a
component you run.

## Planned contract

- **One manifest, one meaning.** `apiVersion: cella.latere.ai/v1beta1`,
  `kind: Sandbox`, and beside it `Secret`, `Volume`, `SandboxSet`, and
  `Environment`. Every surface, the API, the `cella` command, and a
  platform importing the packages, resolves a manifest through one
  function, and what you read back is what runs, defaults included.
- **A boundary the workload cannot move.** A secret carries its own
  destination scope; a sandbox holds only a placeholder and an egress
  gateway swaps it for the value on the way out, toward those hosts and
  no others. A sandbox may spawn sandboxes, and every child is a subset
  of its parent. Nothing that happens inside widens what was declared.
- **Your data plane or ours.** Six drivers behind one contract:
  Kubernetes, Podman, a virtual machine class, an OS-level sandbox on
  your own laptop, a bare process for tests, and a remote driver whose
  worker runs on infrastructure the control plane never dials into.
- **Desired state survives the data plane.** What you applied lives in
  the control plane; what runs is read back from the driver's labels. A
  sandbox a cluster loses is recreated with its volumes reattached.
- **Scheduling, not just pools.** Start it now, take it from a pool, or
  queue it against an environment's capacity with a priority. A
  `SandboxSet` runs a thousand variants of one template and collects
  the results.

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: Sandbox
metadata:
  name: dev
spec:
  image: ghcr.io/example/sandbox:1.4
  workspace:
    source: git
    git: { url: https://github.com/example/repo.git, ref: main, secret: github-token }
  secrets:
    - { name: github-token, env: GITHUB_TOKEN }
  volumes:
    - { name: state, path: /data, volume: app-state }
  network:
    egress: { allowedHosts: ["pypi.org", "*.pythonhosted.org"] }
    ports: [{ name: web, port: 8080, expose: public }]
  lifecycle: { autoStop: 15m, ttl: 24h }
```

```sh
cella secret apply -f github-token.yaml --value-from-env GITHUB_TOKEN
cella volume apply -f app-state.yaml
cella apply -f sandbox.yaml -w
cella exec dev -- make test
cella cp dev:/workspace/out ./out
```

## Try it

```sh
make run   # the stubs and cellad on loopback, with a token to call it with
make       # the quality gate
```

`make run` needs nothing of your own. It starts `cella-stubs`, which serves
an OpenID Connect issuer, an authorization endpoint, an admission endpoint
and an event sink on loopback, then starts `cellad serve` wired to them, and
prints the one command that mints a caller token and the `curl` that creates
the first sandbox. Interrupt it to stop both. The signing key and the sink's
secret are generated once under `out/run/` and kept, so a restart does not
invalidate the token you are holding; `make clean` removes them. A second
clone runs beside the first with `CELLA_RUN_PORT=8090 make run`.

`cella-stubs` is a test binary. It answers what a flag tells it to answer,
it holds nothing across a restart, and no installation runs it: in a real
one, each of those four endpoints is yours.

`make run` opts into native execution, which runs commands on this host with
no isolation and binds loopback. Follow [the native quickstart](docs/native.md)
for the manifest and the API it covers. `CELLA_RUNTIME=podman` runs the same
server against a podman engine instead, one container per sandbox, which is
the isolated backend to develop against on a laptop: point
`CELLA_PODMAN_SOCKET` at the libpod socket, or leave it unset where the
rootless socket is in its usual place. The larger manifest and client
commands above describe the planned contract.

The tiers: `make test` is the unit suite, `make test-podman` runs the
container driver's suite against a rootless engine, and `make test-kind`
brings up a kind cluster with the stubs beside `cellad` and runs the
lifecycle through the API against it.

## Identity

`cellad` knows who is calling and asks somebody else what they may do.
There is no anonymous access and no API key: a caller that wants a
long-lived credential keeps one at its own issuer and re-mints it.

**Who.** `CELLA_OIDC_ISSUERS` lists the OpenID Connect issuers you
trust, any of them. At start `cellad` reads each one's discovery
document and key set and refuses to start when one does not answer,
names another issuer, or publishes no RS256 or ES256 key, so a wrong
issuer is a deployment you fix rather than a log you read later. A
bearer is accepted when a listed issuer signed it, its `aud` matches an entry in
`CELLA_OIDC_AUDIENCE`, it has not expired, and it was minted less than
24 hours ago: a token older than that, or one that does not say when it
was minted, is refused however long its `exp` runs, so a caller re-mints
at its issuer rather than holding one token for a month. Nothing else about the token is interpreted. A subject is
the issuer and the `sub` claim joined, `https://login.example.com|alice`,
so two issuers that agree on a `sub` are two different subjects, and
every claim of the token reaches your authorizer exactly as it arrived.
An organisation, role, or group claim means whatever your authorizer
decides it means, and nothing to `cellad`.

**What.** `CELLA_AUTHORIZER_URL` points at an endpoint you write.
`cellad` POSTs the subject, its claims, an action, and the object to it,
and reads back an allow or a deny, with optional ceilings and, on a
list, a filter. An allow is cached for the `ttl` your endpoint chooses,
a deny for five seconds, and anything that is not a decision fails the
request rather than allowing it. Write the endpoint in Go against
[`latere.ai/x/cella/authorizer`](authorizer), which publishes the
thirty-two actions and the ceilings so you keep no copy of the strings.

With `CELLA_AUTHORIZER_URL` unset, `cellad` runs its own owner policy:
you may create anything but an environment, you may act on what you own,
a list returns your own objects, and `CELLA_ADMIN_SUBJECTS` names the
subjects who may act on everything and who alone make environments. A
sandbox's own identity is least privileged: it reads and execs itself,
reads the tree below it, creates a child, and nothing else.

**What cellad signs.** `CELLA_TOKEN_KEY` is one or two PEM RSA keys. The
first signs the identity every sandbox gets at `/run/cella/token` and
the keys your self-hosted workers register with; every key is published
at `/.well-known/jwks.json`, so anything can verify a sandbox's identity
offline with a stock JWT library. Rotation is yours: put a new key in
front, and take the old block out once the tokens it signed have
expired.

```sh
openssl genrsa -out token.pem 2048

CELLA_OIDC_ISSUERS=https://login.example.com \
CELLA_PUBLIC_URL=https://cella.example.com \
CELLA_TOKEN_KEY="$(cat token.pem)" \
CELLA_ADMIN_SUBJECTS='https://login.example.com|alice' \
  cellad
```

| Variable | Required | Default | |
|---|---|---|---|
| `CELLA_OIDC_ISSUERS` | yes | | issuer URLs whose tokens are accepted, comma separated |
| `CELLA_PUBLIC_URL` | yes | | where callers reach the public listener; the issuer of the tokens `cellad` signs |
| `CELLA_TOKEN_KEY` | yes | | one or two PEM RSA private keys of at least 2048 bits; the first signs, all are published |
| `CELLA_OIDC_AUDIENCE` | | `cella` | comma-separated accepted audiences; the first is used for locally signed tokens |
| `CELLA_OIDC_INSECURE_ISSUERS` | | | issuers from the list that may use `http://` off a loopback address; for a local issuer, never for a deployment |
| `CELLA_AUTHORIZER_URL`, `CELLA_AUTHORIZER_TOKEN` | | | your authorization endpoint and the bearer `cellad` sends it; the URL unset selects the owner policy, and the URL without the token is a start-up failure |
| `CELLA_AUTHORIZER_TIMEOUT`, `CELLA_AUTHORIZER_CACHE` | | `5s`, `60s` | one decision's deadline, and how long an allow that names no `ttl` is held |
| `CELLA_ADMIN_SUBJECTS` | | | rendered subjects the owner policy lets act on everything; read and unused with an authorizer set |
| `CELLA_ENVIRONMENT_KEY_TTL` | | `8760h` | how long a worker's environment key lives |

The [repository scaffold spec](specs/.archive/002-repository-scaffold.md) is the
whole table, identity and everything else; the
[identity spec](specs/006-identity.md) is why each rule is what it is.

## Remaining target capabilities

- Five kinds with strict decoding, server-side defaults, and a
  `status` the server writes, evolving under written rules.
- Six drivers, an isolation class each, and the conformance suite a
  seventh must pass.
- A lifecycle: create, start, stop, delete, idle auto-stop, TTL,
  recovery from desired state, cascade over a spawn tree.
- An egress gateway that substitutes credentials by destination and
  never lets a value into a sandbox.
- Volumes with a life of their own, and a workspace that is one.
- Three scheduling strategies, queues with priority and fair share,
  and sets for rollouts and evaluations.
- Mesh networking between peers and spawn with a budget.
- Exec, attach, files, logs, ports, screenshot, and input as streams.
- OIDC from any issuer, an authorizer webhook with a built-in owner
  policy, an admission webhook with built-in defaults and ceilings, and
  a signed event sink.
- Self-hosted environments through a worker that connects outbound.
- Runtimes: Kubernetes (`CELLA_RUNTIME=k8s`, a claim and a Pod per sandbox)
  and a native runtime for trusted local development.
- Go packages a platform imports: `manifest`, `runtime`, `controller`,
  `egress`.
- The `cella` command and a skill file that teaches an agent to use it.
- Signed images, SBOMs, and provenance on every release.

## Documentation

| Page | |
|---|---|
| [Specs](specs/README.md) | the design, one spec per component, with the build order |
| [Architecture](specs/001-architecture.md) | the two planes, the packages, what the control plane owns and what a platform supplies |
| [Manifest contract](specs/003-manifest-contract.md) | every field, every rule, every error code |
| [Egress and secrets](specs/018-egress-and-secrets.md) | how a credential reaches a request without reaching the sandbox |
| [Data plane workers](specs/021-data-plane-workers.md) | running sandboxes on your own infrastructure |
| [Building a plane](specs/016-building-a-plane.md) | how a platform composes the packages and the webhooks |
| [Install](docs/install.md) | a cluster, the manifests from a release's deploy archive, the first sandbox, and `cellad check` |
| [Deploy](deploy/README.md) | what each manifest and each Secret is, and why the Role grants what it does |
| [docs/](docs/README.md) | for people who run `cellad` or build against it |

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build, the bar, and
where a package belongs. [`SECURITY.md`](SECURITY.md) is where to
report a vulnerability.

## License

Apache-2.0. See [LICENSE](LICENSE).
