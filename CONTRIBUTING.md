# Contributing

Thanks for looking. This file is for people and agents changing Cella.
Users read the [README](README.md) and [`docs/`](docs/README.md); the
design and the reasoning behind it live in [`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer, `git`, `openssl`, and `curl`. Then:

```sh
make run   # the stubs and cellad on loopback with the native runtime, state under out/run
make       # the quality gate
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

Install the hooks once with `make hooks`. They run formatting and license
checks before a commit and the linter before a push, so you see a finding
before CI does.

## Sending a change

Fork the repository, work on a branch, and open a pull request. Keep one
logical change per commit, stage the files explicitly, and write the
subject in the imperative, saying what changed for whoever reads the log.
Maintainers push to `main` directly; the pipeline runs the gate on every
push and pull request, and a `v*` tag cuts a release. A tag needs a
section in [`CHANGELOG.md`](CHANGELOG.md), written for whoever runs
`cellad` or builds on the packages.

If you are planning something large, open an issue first. A design that
lands without a spec is harder to review than one that arrives with the
reasoning attached.

## The bar

`make` runs the whole gate (`go tool lateregate`): formatting, the
linter, modernization, known vulnerabilities, the suite with and without
the race detector, per-package coverage at 90% or more, the suite with
only the toolchain and the system tool directories on `PATH`, the suite
against an empty temporary directory, the license notice, the dependency
allow list, the Postgres connection rules, and the spec tree.
`go tool lateregate list` names the gates and `go tool lateregate <name>`
runs one.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so
the exception is reviewable rather than invisible.

## The test tiers

| Tier | Command | Needs |
|---|---|---|
| unit | `make test` | nothing beyond Go. The Postgres store's cases start a container through Testcontainers and skip, saying so, where no container runtime answers |
| Podman | `make tier-podman` | a rootless Podman engine; runs the Podman runtime's suite and skips where no socket answers |
| kind | `make tier-kind` | kind and a container engine; brings up a cluster with the stubs beside `cellad` and drives the lifecycle through the API |
| conformance | `go test -tags=e2e -run '^TestContract$' ./test/conformance -args -url <address> -token <bearer>` | a running server; the same suite an installation runs, described in [`docs/conformance.md`](docs/conformance.md) |
| install | `tools/docs/run-blocks.sh docs/install.md` | a kind cluster and the `CELLA_INSTALL_*` inputs the page names; runs every shell block of the page in order, which CI does on every push and every tag |

A runtime of your own passes `runtime/runtimetest`, the suite every
driver in this repository passes.

Several pages under `docs/` are held by tests, so a change to the code
that makes them wrong fails the build: `docs/configuration.md` must name
every variable the configuration reads, `docs/api.md` every path the
OpenAPI document serves and every refusal code with its status, and
`docs/cli.md` the command table `cella help` prints. `SECURITY.md` is
held to the threat model and its tests.

## How the code is organized

| Where | What |
|---|---|
| `cmd/cellad` | the server binary and its roles: `serve`, `worker`, `egress`, `check`, `version` |
| `cmd/cella` | the client binary; `internal/cellacli` is the command, and the HTTP client is the exported `client` package |
| `cmd/cella-stubs` | the test stubs `make run` and the kind tier use; `internal/stubs` holds each role |
| `manifest/`, `manifest/v1` | the kinds, strict decoding, and `Resolve`: defaults, admission, ceilings, capability checks, and the spawn boundary |
| `runtime/` | the driver contract; `native`, `podman`, `k8s`, and `remote`, the driver that speaks to a worker; `runtimetest`, the suite a driver passes |
| `controller/` | desired and observed state: create, the lifecycle rules, recovery, scheduling, pools, preemption, spawn, tokens, and the egress map |
| `egress/` | the boundary as the gateway holds it, and the variables a sandbox is pointed at it with |
| `authorizer/` | the action vocabulary an authorization endpoint imports |
| `api/` | the OpenAPI document and its handler |
| `internal/api` | the `/v1` routes, the refusal envelope, and the streams |
| `internal/auth` | token verification, the signer, the authorizer client, and the owner policy |
| `internal/admission`, `internal/events` | the admission client, and the journal with signed delivery to a sink |
| `internal/store` | the store contract, the memory and Postgres adapters, and the suite both pass |
| `internal/worker`, `internal/egressd` | the `worker` and `egress` roles |
| `internal/config`, `internal/check` | configuration loading, and `cellad check` |
| `examples/plane` | a server built from the packages, compiled on every push |
| `deploy/` | the Kubernetes base, the bootstrap Secrets, and the overlays |

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. The implementation follows the spec, and a divergence is
recorded in the spec's Outcome section rather than left in the code.
Names in a spec (manifest fields, error codes, environment variables,
metrics) are the names the code uses.

Small fixes do not need a spec. Anything that changes the manifest
schema, the `/v1` API, a webhook or event payload, or a configuration
variable does.

## Where a package belongs

Three places, by who imports it:

- The module root (`manifest/`, `runtime/`, `controller/`, `egress/`,
  `authorizer/`) holds the packages a platform built on Cella imports. A
  change there keeps existing call sites compiling or names the break in
  the CHANGELOG.
- `internal/` holds what only `cellad` and `cella` need: the HTTP API,
  identity, configuration, the stores.
- A generic package with a plausible second consumer outside Cella
  belongs in [`latere.ai/x/pkg`](https://github.com/latere-ai/pkg), the
  shared library Cella already depends on. If you are unsure, put it in
  `internal/` and say so in the pull request.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields and the `cella` command's
output, the contributor in specs, this file, package documentation, and
commit messages, the developer in logs, `/readyz`, and error details. The
rule and the review checklist are in
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md).

## Reporting a vulnerability

Do not open an issue. [`SECURITY.md`](SECURITY.md) says where to send it.
