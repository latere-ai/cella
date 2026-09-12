---
title: "Repository scaffold: module, binary, configuration, quality gate, images, workflows"
status: complete
track: core
depends_on: []
affects: [cmd/cellad/, internal/config/, internal/version/, Makefile, .lateregate.yaml, Dockerfile, .github/workflows/, .githooks/, docs/]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Repository scaffold

## Overview

A compiling, testable repository that passes its whole quality gate
before any sandbox code exists: the Go module, the `cellad` binary with
its two listeners and probes, typed configuration, the gate, the
developer image, the verify workflow, and the community files an open
source repository is judged by. The shape is chosen so the repository
reads as an ordinary open source Go service to a newcomer and so every
later spec lands into a tree that already enforces the bar.

This spec is also the configuration reference. It owns every `CELLA_*`
variable the server reads, including the ones later specs give a
meaning to, so an operator has one table. It fixes the layout
[[001-architecture]] names without waiting for that design to settle,
because a repository that passes its gate is needed to write and test
anything else.

## Current state

Built and in the tree. `cmd/cellad` serves the probes, `internal/config`
reads four variables, `internal/version` carries the build identity,
`.lateregate.yaml` configures the shared gate pinned as a Go tool, and
`.github/workflows/verify.yml` calls the shared pipeline. The gate
passes locally and on the first push to `main`. This spec is complete
as built; the Outcome records the one divergence.

## Design

### Layout

Every entry either is in the tree or names the spec that builds it.

```
cmd/cellad/             main: the subcommand dispatcher, configuration, listeners, run group
cmd/cella/              main of the agent client, and nothing else (011)
manifest/               the cella/v1 types, decoding, validation, defaulting, resolve (003)
manifest/v1/            the Sandbox type and its status (003)
runtime/                the Runtime interface, capabilities, the shared types (004)
runtime/k8s/            Pod + PVC per sandbox (004)
runtime/podman/         container + volume per sandbox (004)
runtime/native/         host directory + confined host process per sandbox (004)
runtime/runtimetest/    the conformance suite a backend passes (004)
controller/             reconcile, reaper, warm pool (005)
internal/config/        typed configuration from the environment; every problem in one message
internal/version/       build identity set by -ldflags
internal/auth/          the verifier over the issuers, the workload token signer, the authorizer client, the owner policy (006)
internal/admission/     the admission client, the built-in defaults and ceilings (007)
internal/api/           the /v1 handlers, streams, the OpenAPI document (008)
internal/events/        the signed sink client and the journal (009)
internal/store/         the index, revocations, journal; memory and Postgres (010)
internal/cellacli/      the cella command: flags, defaults, exit codes (011)
internal/cellaclient/   the client of the /v1 API the command speaks (011)
test/e2e/               cellad as a process against a backend (e2e build tag) (012)
test/conformance/       the contract as an importable test package (015)
test/stubs/             the stub issuer, authorizer, admission, and sink (012)
tools/                  generators and release scripts (002, 014)
deploy/                 kustomize base, examples, bootstrap (014)
skills/cella/           the skill that teaches an agent the cella command (011)
docs/                   for people who run cellad or build against it
specs/                  this deck
```

### Local run

`make run` builds the binary and runs it on loopback with the native
backend selected and its state under `out/run/`. Until
[[004-runtime-backend-contract]] lands the process serves the probes
and nothing else; once [[012-test-stubs-and-tiers]] lands, `make run`
also starts the stub issuer, authorizer, and sink beside it and prints
a token, so a clean clone applies its first manifest in one command.

### Binary and listeners

| Listener | Default address | Serves |
|---|---|---|
| public | `:8080` (`CELLA_PUBLIC_ADDR`) | `GET /` with the build identity, `/v1/*` (008), `/.well-known/jwks.json` (006), and `GET /livez`, `GET /readyz`, `GET /version` so a release smoke reaches them through the ingress |
| internal | `:8081` (`CELLA_INTERNAL_ADDR`) | the four probes below, for the cluster |

The probes are `latere.ai/x/pkg/health`, mounted whole on the internal
listener and path by path on the public one.

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise, and `not ready: draining: shutting down` during shutdown; text, the developer register |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version`, set by `-ldflags` |
| GET | `/metrics` | the `latere.ai/x/pkg/metrics` registry in the Prometheus text format (017); internal listener only |

Readiness runs its checks with a 2 second budget: `draining` and
`disk` (create and remove a file under `CELLA_DATA_DIR`) today, the
backend's own check once [[004-runtime-backend-contract]] lands, and
the store's once [[010-state]] does. Shutdown on `SIGTERM` or `SIGINT`:
readiness answers 503 at once, the process waits a 3 second drain
delay, then closes the HTTP servers with a 60 second grace period, then
the controllers. A Deployment sets `terminationGracePeriodSeconds` 90.

`cellad -version` prints `cellad <version> (<commit>, <date>)` and
exits 0; a bad flag exits 2; a configuration or start-up failure exits
1 with one line on stderr prefixed `cellad:`. The first argument that
does not start with `-` selects a subcommand; without one the binary
serves.

| Subcommand | Reads | Does | Spec |
|---|---|---|---|
| `serve` (default) | the whole table | the listeners and the controllers | this spec |
| `check` | the whole table | one line per requirement of the installation, exit 1 on any failure | 014 |

An unknown subcommand is a usage error, exit 2. `check` is an unknown
subcommand until [[014-release-and-installation]] lands.

### Configuration

Every variable is read once at start-up by `internal/config.Load`,
which collects every problem and fails with one message
`configuration: <problem>; <problem>; ...` sorted by variable name. A
blank value is unset. An unknown variable is never an error, so a
deployment that sets one before its spec lands is not refused.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `CELLA_PUBLIC_ADDR`, `CELLA_INTERNAL_ADDR` | no | `:8080`, `:8081` | listen addresses; a test binds `127.0.0.1:0`; the two must differ unless both ask for port 0 |
| `CELLA_DATA_DIR` | no | `/var/lib/cella` | local disk cellad keeps state on: the readiness write test, the native and podman workspaces (004); created at start |
| `CELLA_RUNTIME` | no | `k8s` | the backend: `k8s`, `podman`, or `native` (004) |
| `CELLA_PUBLIC_URL` | yes, from 006 | none | the absolute URL callers reach the public listener at; the issuer of workload tokens and the base of every URL in a response |
| `CELLA_KUBECONFIG`, `CELLA_NAMESPACE` | 004 | in-cluster, `cella` | the cluster and namespace the k8s backend creates in |
| `CELLA_PODMAN_SOCKET` | 004 | the user's default socket | the libpod API the podman backend drives |
| `CELLA_POOL_SIZE` | 005 | `0` | warm sandboxes kept per default image; `0` disables the pool |
| `CELLA_OIDC_ISSUERS` | yes, from 006 | none | comma separated issuer URLs whose tokens are accepted |
| `CELLA_OIDC_AUDIENCE` | 006 | `cella` | the audience a token must carry |
| `CELLA_OIDC_INSECURE_ISSUERS` | 006 | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production |
| `CELLA_TOKEN_KEY` | yes, from 006 | none | PEM-encoded ECDSA P-256 private key that signs workload tokens; required in every mode |
| `CELLA_AUTHORIZER_URL`, `CELLA_AUTHORIZER_TOKEN` | 006 | unset | the operator's authorization endpoint and the bearer cellad sends it; unset selects the built-in owner policy; the URL without the token is a start-up failure |
| `CELLA_ADMIN_SUBJECTS` | 006 | unset | comma separated subjects the built-in owner policy lets act on every sandbox; read and unused when an authorizer is set |
| `CELLA_ADMISSION_URL`, `CELLA_ADMISSION_TOKEN` | 007 | unset | the operator's admission endpoint and its bearer; unset selects the built-in defaults and ceilings |
| `CELLA_DEFAULT_CPU`, `CELLA_DEFAULT_MEMORY`, `CELLA_DEFAULT_DISK` | 007 | `1`, `2Gi`, `10Gi` | the resources a manifest gets when it names none |
| `CELLA_DEFAULT_AUTOSTOP`, `CELLA_DEFAULT_TTL`, `CELLA_DEFAULT_AUTODELETE` | 007 | `15m`, `24h`, `72h` | the lifecycle a manifest gets when it names none |
| `CELLA_MAX_CPU`, `CELLA_MAX_MEMORY`, `CELLA_MAX_DISK`, `CELLA_MAX_TTL` | 007 | unset | ceilings a resolved manifest may not exceed; unset is no ceiling |
| `CELLA_EVENTS_URL`, `CELLA_EVENTS_SECRET` | 009 | unset | the event sink and the HMAC key; events are off when the URL is unset; the URL without the secret is a start-up failure |
| `CELLA_DB_URL` | 010 | unset | a Postgres URL; unset keeps the index in memory |
| `CELLA_REQUESTS_PER_MINUTE` | 008 | `600` | requests one subject may send in a minute; `0` turns the limit off |
| `CELLA_MAX_BODY_BYTES` | 008 | `65536` | the largest manifest body accepted |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_*` | 017 | unset | the standard OpenTelemetry exporter variables, read by `latere.ai/x/pkg/otel`; telemetry is off without the endpoint |

### The gate

`make` is `go tool lateregate`. The gates live in `latere.ai/x/ci-gate`,
pinned in `go.mod` with a `tool` directive, and `.lateregate.yaml`
holds only what this repository chose: the spec vocabulary and required
frontmatter, the empty hermetic allowance (cellad forks no binary of
its own), the `depcheck` allow list of `./cmd/cellad`, and the licence.
The git hooks are two-line shims that call the gate. A coverage
exemption or a waiver is a line in that file with a reason, never a
tag in the code.

### Images

`Dockerfile` compiles inside the image and runs `cellad` on a
distroless static base as a non-root user, with `/var/lib/cella` a
volume and both ports exposed. The release image of
[[014-release-and-installation]] copies a binary the pipeline built and
attested and shares this runtime stage byte for byte between the two
markers, checked by a test in that spec.

### Workflows

`verify.yml` runs on every push to `main`, every pull request, every
tag, and on demand: the `gate` job calls
`latere-ai/ci/.github/workflows/lateregate.yml@v1` on hosted runners,
`tidy` checks `go mod tidy -diff`, and `image` builds the developer
image and asks it for its version. The tiers that need a backend beside
them join in [[012-test-stubs-and-tiers]]; the release pipeline is
[[014-release-and-installation]]'s. Every third-party action is pinned
by commit with its version in a comment.

### Community files

`README.md` positions the project and says what works today. `LICENSE`
is Apache-2.0. `CONTRIBUTING.md` says how to build, the bar, specs
first, and where a package belongs. `CODE_OF_CONDUCT.md` is the
Contributor Covenant 2.1. `SECURITY.md` names the address, the
response times, and the properties the design commits to. `CHANGELOG.md`
has one section per release and a tag without one is refused.
`.github/` carries the bug and feature templates, the pull request
template, and the actionlint configuration that declares no self-hosted
runner. `AGENTS.md`, with `CLAUDE.md` a symlink to it, holds the
conventions an agent working in the tree follows.

## Not in this spec

The release pipeline, the deploy manifests, and the install document
([[014-release-and-installation]]); the stubs and the tiers that need a
backend ([[012-test-stubs-and-tiers]]); the generated configuration
page, which lands with the generator once the table has two owners.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `cellad -version` prints the identity and exits 0; an unknown subcommand and a bad flag exit 2 | `TestVersionFlagPrintsTheIdentityAndExitsZero`, `TestUnknownSubcommandIsAUsageError`, `TestBadFlagIsAUsageError` | passing |
| A configuration with three problems fails with one sorted line, exit 1 | `TestLoadReportsEveryProblemInOneSortedMessage`, `TestBadConfigurationExitsOneWithOneLine` | passing |
| Both listeners answer `/livez`, `/readyz`, `/version`; the public one answers `/` and not `/metrics`; a stop returns 0 | `TestServeAnswersTheProbesOnBothListenersAndStopsCleanly` | passing |
| Readiness fails once draining begins and when the data directory is unwritable | `TestReadinessFailsOnceDrainingBegins`, `TestDiskCheckReportsAnUnwritableDirectory` | passing |
| An occupied address or an unwritable data directory is a start-up failure with the variable named | `TestOccupiedAddressExitsOne`, `TestUnwritableDataDirExitsOne` | passing |
| Every package clears 90% coverage and every gate passes locally | `go tool lateregate` | passing |
| The gate, the tidy check, and the image build pass on the first push to `main` | the `verify` workflow run | passing, run 34710405564 |
| The developer image runs `cellad -version` as a non-root user | the `image` job | passing, the same run |

## Outcome

Built on 2026-09-12 in four commits and proven by the first `verify`
run on `main` (run id 34710405564, nineteen jobs green). One divergence
from the first draft: the two listeners bind through
`net.ListenConfig` with the run context rather than `net.Listen`, and
the shutdown runs on `context.WithoutCancel` of the stop context, both
because the shared linter's `noctx` and `contextcheck` rules refuse the
bare forms. Two ":0" addresses are allowed for both listeners so the
suite binds loopback without choosing ports; the equality rule applies
to every other pair. Deferred as the spec says: the release pipeline
and the deploy manifests to [[014-release-and-installation]], the stubs
and the tiers to [[012-test-stubs-and-tiers]], the generated
configuration page until the table has two owners.
