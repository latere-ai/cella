---
title: "Repository scaffold: module, binary, configuration, quality gate, images, workflows"
status: complete
track: core
depends_on: []
affects: [cmd/cellad/, internal/config/, internal/version/, Makefile, .lateregate.yaml, Dockerfile, .github/workflows/, .githooks/, docs/]
effort: small
created: 2026-09-12
updated: 2026-09-19
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
api/                    openapi.yaml, generated from the kinds, the routes, and the error table; embedded (008)
cmd/cellad/             main: the subcommand dispatcher, configuration, listeners, run group
cmd/cella/              main of the agent client, and nothing else (011)
manifest/               decoding, validation, defaulting, resolve, the boundary check, for every kind (003)
manifest/v1/            the Sandbox, Secret, Volume, SandboxSet, and Environment types (003, 018, 019, 020, 021)
runtime/                the Driver interface, isolation classes, capabilities, the shared types (004)
runtime/k8s/            Pod + PVC per sandbox; a runtime class for the vm class (004)
runtime/podman/         container + volume per sandbox (004)
runtime/vm/             a microVM per sandbox; a stub until its own spec (004)
runtime/local/          an OS sandbox around a host process on the operator's machine (004)
runtime/native/         host directory + process, no boundary; the suite's driver (004)
runtime/remote/         every method as an operation a worker claims (004, 021)
runtime/runtimetest/    the conformance suite a driver passes (004)
controller/             desired to observed, phases, reaper, recovery, cascade, scheduler, sets (005, 020)
egress/                 compiling a sandbox's secrets and rules into the gateway's map (018)
internal/metrics/       the one registry and the table of 017
internal/config/        typed configuration from the environment; every problem in one message
internal/version/       build identity set by -ldflags
internal/auth/          the verifier over the issuers, the workload token signer, the authorizer client, the owner policy (006)
internal/admission/     the admission client, the built-in defaults and ceilings (007)
internal/api/           the /v1 handlers, streams, the OpenAPI document (008)
internal/events/        the signed sink client and the journal (009)
internal/store/         desired and observed state, secret values, revocations, ledger, journal; memory and Postgres (010)
internal/serve/         the control plane role of cellad: wiring of the API, controller, store, and webhook clients (008)
internal/worker/        the worker role of cellad: registration, the claim loop, the stream relay (021)
internal/egressd/       the egress role of cellad: pkg/egress's gateway, CA, and ingest listener (018)
internal/check/         the check role of cellad (014)
internal/cellacli/      the cella command: flags, defaults, exit codes (011)
internal/cellaclient/   the client of the /v1 API the command speaks (011)
test/e2e/               cellad as a process against a driver (e2e build tag) (012)
test/conformance/       the contract as an importable test package (015)
test/stubs/             the stub issuer, authorizer, admission endpoint, sink, and upstream, and the cella-stubs binary (012)
tools/                  generators and release scripts (002, 014); tools/rules prints the alert rules for promtool (017)
deploy/                 kustomize base, examples, bootstrap (014)
skills/cella/           the skill that teaches an agent the cella command (011)
docs/                   for people who run cellad or build against it
specs/                  this deck
```

### Local run

`make run` builds the binary and runs it on loopback with the native
driver selected and its state under `out/run/`. Until
[[004-runtime-contract]] lands the process serves the probes
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
driver's own check once [[004-runtime-contract]] lands, and
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
| `serve` (default) | the whole table | the control plane: the listeners, the controller, the scheduler | this spec |
| `worker` | `CELLA_URL`, `CELLA_ENVIRONMENT_KEY`, and the selected driver's variables; none of the control plane's | one registered environment's data plane: claims operations and runs a driver | 021 |
| `egress` | `CELLA_URL`, `CELLA_ENVIRONMENT_KEY`, `CELLA_EGRESS_PROXY_ADDR`, `CELLA_EGRESS_REVERSE_ADDR`, `CELLA_EGRESS_CA_KEY`; none of the control plane's | the credential-substituting gateway, connecting outbound to the control plane | 018 |
| `check` | the whole table | one line per requirement of the installation, exit 1 on any failure | 014 |

One binary, one image, one role per process: a Deployment selects the
role by its args. Each role is a package under `internal/` with its own
dependency allow list in the gate, so the binary carrying every role
does not loosen what any one role may reach. An unknown subcommand is a
usage error, exit 2. `worker`, `egress`, and `check` are unknown
subcommands until their specs land.

### Configuration

Every variable is read once at start-up by `internal/config.Load`,
which collects every problem and fails with one message
`configuration: <problem>; <problem>; ...` sorted by variable name. A
blank value is unset. An unknown variable is never an error, so a
deployment that sets one before its spec lands is not refused.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `CELLA_PUBLIC_ADDR`, `CELLA_INTERNAL_ADDR` | no | `:8080`, `:8081` | listen addresses; a test binds `127.0.0.1:0`; the two must differ unless both ask for port 0 |
| `CELLA_DATA_DIR` | no | `/var/lib/cella` | local disk cellad keeps state on: the readiness write test, the native and local drivers' sandboxes and volumes (004, 019); created at start |
| `CELLA_ALLOW_UNSAFE_NATIVE` | no | `false` | explicit consent to native execution without isolation; required with `CELLA_RUNTIME=native` (028) |
| `CELLA_RUNTIME` | no | `k8s` | the in-process driver of the default environment: `k8s`, `podman`, `native`, and from 004 `local`, `vm`, or `none` for a control plane that serves only workers |
| `CELLA_PUBLIC_URL` | yes, from 006 | none | the absolute URL callers reach the public listener at; the issuer of workload and environment tokens and the base of every URL in a response |
| `CELLA_K8S_KUBECONFIG`, `CELLA_K8S_NAMESPACE` | 004, 036 | in-cluster, `cella` | the cluster the k8s driver drives and the namespace its claims and Pods live in; an empty kubeconfig means the in-cluster configuration and never the caller's own loading rules |
| `CELLA_K8S_STORAGE_CLASS`, `CELLA_K8S_NODE_SELECTOR`, `CELLA_K8S_TOLERATIONS`, `CELLA_K8S_IMAGE_PULL_SECRETS` | 036 | unset, unset, unset, unset | what one installation supplies: the class the workspace claim is provisioned from, `key=value` pairs every Pod is scheduled by, `key[=value][:effect]` entries it tolerates, and the secrets it pulls with |
| `CELLA_K8S_RUN_AS_USER`, `CELLA_K8S_RUN_AS_GROUP` | 036 | `1000`, `1000` | the uid and the gid a sandbox runs as when its manifest names none; the gid is also the `fsGroup`, so the claim is writable; zero is refused, since the baseline of 013 runs nothing as root |
| `CELLA_K8S_CPU_REQUEST_RATIO`, `CELLA_K8S_MEMORY_REQUEST_RATIO` | 036 | `0.1`, `1` | the request as a fraction of the limit, which is how densely idle sandboxes pack; `1` gives requests equal to limits |
| `CELLA_K8S_DEFAULT_CPU`, `CELLA_K8S_DEFAULT_MEMORY`, `CELLA_K8S_DEFAULT_DISK` | 036 | `1`, `1Gi`, `5Gi` | the limits and the claim size for a manifest that names no resources |
| `CELLA_K8S_READY_TIMEOUT`, `CELLA_K8S_GRACE_PERIOD` | 036 | `90s`, `10s` | how long a create or a start waits for a Pod, and the Pod's termination grace period |
| `CELLA_K8S_RUNTIME_CLASS`, `CELLA_K8S_RUNTIME_CLASS_ISOLATION` | 004 | unset, `container` | the runtime class every Pod gets and the isolation class the operator declares that class provides (`container` or `vm`); never inferred |
| `CELLA_PODMAN_SOCKET` | 004 | the rootless socket, then the system one | the libpod API the podman driver drives; unset, it tries `$XDG_RUNTIME_DIR/podman/podman.sock` and then `/run/podman/podman.sock`, and `Preflight` names every candidate when none answers (035) |
| `CELLA_LOCAL_SRT` | 004 | `srt` on `PATH` | the sandbox runtime binary the local driver confines a process with |
| `CELLA_REAP_INTERVAL`, `CELLA_TOUCH_INTERVAL`, `CELLA_LOST_GRACE`, `CELLA_RECOVERY_ATTEMPTS` | 005 | `30s`, `1m`, `10m`, `5` | the reaper's tick, each bounded to between `1s` and `1h`; how often one sandbox's activity reaches the driver; how long a sandbox is `Lost` without a durable store before it is reaped; how many recreations a durable one gets before `Failed` |
| `CELLA_POOL_SIZE`, `CELLA_POOL_IMAGE`, `CELLA_SCHEDULING_MODE` | 020, 021 | `0`, unset, `direct` | the default environment's `spec.pool.size`, `spec.pool.image`, and `spec.scheduling.mode`; every other environment declares its own |
| `CELLA_SCHEDULE_INTERVAL`, `CELLA_CAPACITY_HEADROOM`, `CELLA_MAX_PREEMPTIONS`, `CELLA_MAX_SET_REPLICAS` | 020 | `5s`, `0.1`, `3`, `4096` | the scheduler loop's tick; the fraction an `auto` capacity keeps free; how often one sandbox may be preempted; the largest set |
| `CELLA_SECRET_KEY` | yes when any Secret exists, from 018 | none | 32 bytes, base64, wrapping every secret's data key |
| `CELLA_EGRESS_ACK_TIMEOUT` | 018 | `5s` | how long a create waits for one gateway of the environment to acknowledge the sandbox's map |
| `CELLA_EGRESS_RECORDS_RETENTION`, `CELLA_EGRESS_RECORDS_CAP` | 018 | `168h`, `1000` | how long egress records stay in Postgres; how many the memory store keeps per sandbox |
| `CELLA_URL`, `CELLA_ENVIRONMENT_KEY` | 018, 021 | none | read by the `worker` and `egress` roles: the control plane's public URL and the environment key that authenticates the role's one outbound stream; `CELLA_URL` is also what `cella` reads and what every sandbox is given (004, 011) |
| `CELLA_INSECURE_CONTROL_PLANE` | 021 | unset | `1` admits a non-loopback `http://` `CELLA_URL` for the worker, the gateway, and the client; set by the stubs only |
| `CELLA_TOKEN`, `CELLA_TOKEN_FILE` | 011 | unset, `/run/cella/token` | the bearer `cella` sends, or the file it reads per request when the variable is unset |
| `CELLA_EGRESS_PROXY_ADDR`, `CELLA_EGRESS_REVERSE_ADDR`, `CELLA_EGRESS_CA_KEY` | 018 | `:3128`, `:8080`, none | the `egress` role's two doors and the certificate authority it terminates TLS with, as one PEM value carrying the certificate and the private key, since a gateway given only a key could not tell a sandbox what to trust; absent, an authority is generated at start and lives for that process, so an environment with several gateways sets one value on each (039) |
| `CELLA_EGRESS_CA_BUNDLE` | 012 | unset | PEM authorities the gateway trusts beside the system roots when it dials an upstream; unset in production, the tiers set it to the upstream stub's |
| `CELLA_TEST_URL`, `CELLA_TEST_TOKEN` | 012 | unset | what the conformance and kind tiers read; printed by the kind overlay's `up.sh` |
| `CELLA_TEST_DRIFT_DEFAULT` | 015 | unset | a field whose default resolves one unit off, so a test proves the conformance suite notices; empty in every deployment |
| `CELLA_EVENTS_EGRESS` | 018 | unset | `1` delivers per-connection egress records to the sink as events; the journal and the metrics carry them regardless |
| `CELLA_EGRESS_SIDECAR` | 018 | unset | `1` runs the gateway as a per-Pod sidecar on k8s instead of one Deployment |
| `CELLA_SOURCE_ALLOW`, `CELLA_MAX_SOURCE_BYTES`, `CELLA_MAX_VOLUME_SIZE` | 019 | unset, `10Gi`, unset | hosts a `Volume` archive source may be fetched from, re-applied to redirects (unset refuses every archive); the most an archive fetch downloads; the largest `Volume.spec.size` (unset is no ceiling) |
| `CELLA_ENVIRONMENT_OFFLINE` | 021 | `2m` | how long without a worker heartbeat, or with the in-process driver not ready, before an environment is `Offline` |
| `CELLA_DEFAULT_ENVIRONMENT`, `CELLA_CAPACITY`, `CELLA_GATEWAY` | 021 | `default`, `auto` on k8s, unset | the name of the default environment; the seed of its `spec.capacity` and `spec.gateway` at first start, with `CELLA_SCHEDULING_MODE` and `CELLA_POOL_*` seeding the rest; `CELLA_GATEWAY` is the proxy door as a sandbox of that environment dials it, `host` or `host:port` |
| `CELLA_GATEWAY_REVERSE` | 018 | unset | the reverse door as a sandbox dials it, `host` or `host:port`; set only where the runtimes in the environment ignore proxy variables, and set with `CELLA_GATEWAY` or refused. The two variables are what `CreateSpec.Egress` carries into a driver (039) |
| `CELLA_OIDC_ISSUERS` | yes, from 006 | none | comma separated issuer URLs whose tokens are accepted |
| `CELLA_OIDC_AUDIENCE` | 006 | `cella` | comma-separated accepted external audiences; locally minted tokens carry only the first (027) |
| `CELLA_OIDC_INSECURE_ISSUERS` | 006 | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production |
| `CELLA_TOKEN_KEY` | yes, from 006 | none | one or two PEM-encoded RSA private keys of at least 2048 bits; the first signs workload tokens and environment keys, every block is in the key set, so rotation is prepending a key and later removing the old block; required in every mode |
| `CELLA_AUTHORIZER_URL`, `CELLA_AUTHORIZER_TOKEN` | 006 | unset | the operator's authorization endpoint and the bearer cellad sends it; unset selects the built-in owner policy; the URL without the token is a start-up failure |
| `CELLA_AUTHORIZER_TIMEOUT`, `CELLA_AUTHORIZER_CACHE` | 006 | `5s`, `60s` | one decision's deadline and how long an allow is cached per subject, action, and resource; the cache follows the answer's `ttl` bounded by this value, capped at `600s`, and a deny is held five seconds |
| `CELLA_ENVIRONMENT_KEY_TTL` | 006 | `8760h` | the lifetime of an environment key from mint |
| `CELLA_ADMIN_SUBJECTS` | 006 | unset | comma separated subjects the built-in owner policy lets act on every sandbox; read and unused when an authorizer is set |
| `CELLA_ADMISSION_URL`, `CELLA_ADMISSION_TOKEN`, `CELLA_ADMISSION_TIMEOUT` | 007 | unset, unset, `3s` | the operator's admission endpoint, its bearer, and one call's deadline, between `100ms` and `30s`; the URL unset selects the built-in identity step; the URL without the token, or a non-loopback `http://` URL, is a start-up failure |
| `CELLA_MAX_SANDBOXES_PER_SUBJECT` | 007 | `0` | the count ceiling per subject: every desired sandbox of the subject whose phase is not `Deleting`, a queued and a stopped one included; `0` is none |
| `CELLA_DEFAULT_CPU`, `CELLA_DEFAULT_MEMORY`, `CELLA_DEFAULT_DISK` | 007 | unset | the resources a manifest gets when it names none |
| `CELLA_DEFAULT_AUTOSTOP`, `CELLA_DEFAULT_TTL`, `CELLA_DEFAULT_AUTODELETE` | 007 | unset | the lifecycle a manifest gets when it names none |
| `CELLA_DEFAULT_IMAGE` | 007 | unset | the image a manifest gets when it names none, where the environment runs images; an installation with an admission endpoint lets that endpoint supply it, and this project ships no value |
| `CELLA_MAX_CPU`, `CELLA_MAX_MEMORY`, `CELLA_MAX_DISK`, `CELLA_MAX_TTL` | 007 | unset | ceilings a resolved manifest may not exceed; unset is no ceiling |

Every `CELLA_DEFAULT_*` is optional. Stage 2 of a resolve runs before the
admission step of 007 and that step cannot tell a field the caller wrote
from one stage 2 defaulted, so a deployment whose policy lives in an
admission endpoint leaves all seven unset and sets only the `CELLA_MAX_*`
ceilings: the endpoint supplies the defaults, and the ceilings stay the
operator's own second check over whatever the endpoint returned.
| `CELLA_EVENTS_URL`, `CELLA_EVENTS_SECRET` | 009 | unset | the event sink and one or two comma-separated HMAC secrets; events are off when the URL is unset and the journal still holds them; the URL without a secret, a secret without the URL, or a non-loopback `http://` URL without `CELLA_EVENTS_INSECURE_SINK=1`, is a start-up failure |
| `CELLA_EVENTS_TIMEOUT`, `CELLA_EVENTS_PORTS`, `CELLA_EVENTS_INSECURE_SINK` | 009 | `10s`, unset, unset | one delivery's deadline; `1` emits a `sandbox.port` event per proxied request; `1` admits an `http://` sink, set by the stubs only |
| `CELLA_EVENTS_RETRY_WINDOW` | 009 | `24h` | how long a record the sink has not taken is retried before it is dropped and counted; at least `1m` |
| `CELLA_DB_URL`, `CELLA_DB_MAX_CONNS` | 010 | unset, `4` | a Postgres URL and the pool size; the URL unset keeps desired state in the single-process snapshot of 026 and turns recovery off |
| `CELLA_JOURNAL_CAP` | 010 | `1000` | events kept per object in the in-memory journal |
| `CELLA_JOURNAL_RETENTION` | 010 | `720h` | how long acknowledged or dropped events stay in the Postgres journal |
| `CELLA_REQUESTS_PER_MINUTE` | 008 | `600` | requests one subject may send in a minute; `0` turns the limit off |
| `CELLA_MAX_BODY_BYTES` | 008 | `65536` | the largest manifest or JSON body accepted |
| `CELLA_MAX_UPLOAD_BYTES` | 008 | `1Gi` | the largest tar upload accepted |
| `CELLA_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | 008 | `60` | requests one client address may send before authentication in a minute |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_*` | 017 | unset | the standard OpenTelemetry exporter variables, read by `latere.ai/x/pkg/otel`; telemetry is off without the endpoint |

### The gate

`make` is `go tool lateregate`. The gates live in `latere.ai/x/ci-gate`,
pinned in `go.mod` with a `tool` directive, and `.lateregate.yaml`
holds only what this repository chose: the spec vocabulary and required
frontmatter, the empty hermetic allowance (cellad forks no binary of
its own), the `depcheck` allow lists of `./cmd/cellad` and `./cmd/cella`
(the standard library and `pkg/httpjson`, [[011-agent-client]]), and the
licence.
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
image and asks it for its version. The tiers that need a driver beside
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
driver ([[012-test-stubs-and-tiers]]); the generated configuration
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
