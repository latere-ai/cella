---
title: "Test stubs and tiers: the stubs, the bootstrap of make run, the driver tiers, the kind overlay, CI jobs"
status: validated
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/007-admission.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/018-egress-and-secrets.md
  - specs/021-data-plane-workers.md
affects: [test/stubs/, test/e2e/, test/conformance/, Makefile, Dockerfile.stubs, .github/workflows/, deploy/examples/kind/, internal/config/]
effort: medium
created: 2026-09-12
updated: 2026-09-20
author: changkun
---

# Test stubs and tiers

## Overview

Every operator endpoint the control plane dials has a stub in the
tree, so `make run` gives a clean clone a working system and so the
tiers exercise the real clients against a real HTTP peer. The tiers
are selected by build tag and test-name prefix, run where their
substrate exists, and never by wall-clock guess. One binary,
`cella-stubs`, serves every stub, so the kind overlay runs it as
sidecars of one Pod and `make run` runs it as one process.

## Current state

Partly built. Slice 049 ([[049-stubs-and-tiers]]) built `cella-stubs` with
the issuer, the authorizer, the admission endpoint and the sink, the
bootstrap behind `make run`, the unit, podman and kind targets, the kind
overlay `deploy/examples/kind-stubs`, and the two continuous integration
jobs that walk the install document against that stack. The upstream stub,
the native, postgres, local and worker tiers, `make run-worker` and the
Cilium half of the kind tier are not built; each waits on the spec whose
behaviour it exercises.

## Design

### The stubs

| Stub | Serves | Behaviour |
|---|---|---|
| issuer | `/.well-known/openid-configuration`, `/jwks`, `POST /mint {sub, aud?, exp?}` | a real OIDC issuer over an RS256 key set; mints any subject asked, `sandbox:...` included, since refusing a reserved prefix is `cellad`'s job and the suite needs the token to prove it; `-alg es256` adds a second key and signs with it, for the ES256 acceptance case and the start-up key-set check of [[006-identity]]; a token signed with an algorithm outside `RS256` and `ES256` is built by the verifier test itself, not offered by this stub |
| authorizer | the contract of [[006-identity]], the stub `latere.ai/x/pkg/authz` ships | allow everything except the probe id `sbx_00000000000000000000000000`, always denied; `X-Stub-Deny: <action>` on the request or `-deny <action>` refuses; `-limits <json>`, `-filter <json>`, and `-ttl <seconds>` are returned on every allow; `-fail-mode timeout|malformed|status:<code>|no-allow` produces each failure 006 names, and `-fail-mode conn-drop` closes the connection before a response line so the one retry is exercised; records every request for the suite to read at `GET /requests` |
| admission | the contract of [[007-admission]] | returns the manifest unchanged, or with `-rewrite image=<ref>` applied, or `-warn <text>` added, or refuses with `-refuse`; `-fail-mode timeout|malformed|status:<code>|no-allow|unknown-field|change-kind|oversize`; records requests |
| sink | the contract of [[009-events]] | verifies `Cella-Signature` against either of two configured secrets and refuses a `t` older than five minutes; `-fail-first N` answers 503 to the first N deliveries; `-status 400` answers 400 to the next one; stores events and serves them at `GET /events` in `seq` order per object |
| upstream | an HTTPS server sandboxes reach | records every request's host, method, path, headers, query, and body at `GET /requests`, so a tier asserts what left the sandbox and what the gateway substituted; its certificate is signed by a CA the stub writes to a file at start, which the gateway trusts through `CELLA_EGRESS_CA_BUNDLE` |

The gateway is not a stub: every tier runs the real `cellad egress`
with `CELLA_EGRESS_CA_KEY` pointing at a key generated under `out/`,
its two doors on loopback ports, and `CELLA_EGRESS_CA_BUNDLE` naming
the upstream's CA. The worker is not a stub: the worker tier runs the
real `cellad worker` with the `native` driver. Each stub is a package
under `test/stubs/` with a handler and a test that drives every flag,
and `test/stubs/cmd/cella-stubs` serves the five on five ports.
`Dockerfile.stubs` builds the image [[014-release-and-installation]]
publishes for the conformance job.

### make run

Ports derive from the checkout's directory name, so two clones run
side by side; state lives under `out/run/`; `make run-down` stops
everything and `make clean` removes the state. The bootstrap, in this
order, because the gateway needs a key only a running control plane
can mint and the first create needs the gateway's acknowledgement:

1. Build `cellad`, `cella-stubs`, and `cella` when its package exists.
2. Generate once under `out/run/`: `CELLA_TOKEN_KEY` with `openssl
   genrsa 2048`, `CELLA_SECRET_KEY` from 32 random bytes,
   `CELLA_EGRESS_CA_KEY`.
3. Start `cella-stubs`; wait for the issuer's key set.
4. Start `cellad serve` with `CELLA_RUNTIME=local` where the sandbox
   runtime is on `PATH` and `native` otherwise, `CELLA_OIDC_ISSUERS`
   and `CELLA_OIDC_INSECURE_ISSUERS` naming the stub issuer,
   `CELLA_AUTHORIZER_URL`, `CELLA_ADMISSION_URL`, and
   `CELLA_EVENTS_URL` on loopback, `CELLA_ADMIN_SUBJECTS` holding the
   rendered `<issuer>|dev`; wait for `/readyz`.
5. Mint an admin token at the issuer's `/mint` for `dev`; `POST
   /v1/environments/default/keys` with it.
6. Start `cellad egress` with that key; wait until the environment's
   phase is `Ready`.
7. Print `export CELLA_URL=... CELLA_TOKEN=...`, a `curl` apply of the
   minimal manifest, and the `cella apply` line.

`make run-worker` applies a second `Environment` with `mode: worker`,
mints its key, and starts `cellad worker` with the `native` driver
against it, so the self-hosted path is one command away.

### Tiers

The gate runs the untagged suite only, under its empty hermetic
allowance; every tier below is outside the gate, behind a build tag, so
`srt`, `bwrap`, `socat`, `rg`, `podman`, `kind`, and `docker` are never
on the gate's path. A tier starts its own `cellad` and stubs, binds
every listener on `:0`, keeps state under `t.TempDir()`, tears down
every container it started in `t.Cleanup`, never reads the developer's
kubeconfig, socket, or sandbox runtime configuration, and removes the
`srt-mux-*.sock` files the sandbox runtime leaves in `TMPDIR` so the
`tempdir` gate stays clean when a tier runs locally.

| Tier | Command | Needs | Runs |
|---|---|---|---|
| unit | the gate | Go | every push |
| native | `go test -tags=e2e -v -run '^TestNative' ./test/e2e/...` | Go | every push |
| postgres | `go test -tags=e2e -v -run '^TestPostgres' ./test/e2e/... ./internal/store/...` | Docker, a Postgres test container | every push; the recovery cases of [[005-lifecycle-controller]] and the store suite of [[010-state]] |
| local | `go test -tags=e2e -v -run '^TestLocal' ./test/e2e/... ./runtime/local/...` | the sandbox runtime; skipped whole with its remediation printed where absent | every push on Linux, with `bubblewrap`, `socat`, `ripgrep`, and `@anthropic-ai/sandbox-runtime` installed by the job; on macOS on dispatch, for Seatbelt |
| worker | `go test -tags=e2e -v -run '^TestWorker' ./test/e2e/...` | Go | every push; `cellad worker` with `native` against the tier's `cellad`; the tier asserts the worker opens no listening socket and `cellad` opens no connection toward the worker's address for the tier's duration |
| podman | `go test -tags=podman -v -run '^TestPodman' ./test/e2e/... ./runtime/podman/...` | a rootless Podman socket the job starts with `podman system service --time=0` | every push on `ubuntu-latest`, which ships Podman |
| kind | `go test -tags=e2e -v -run '^TestCluster' ./test/e2e/... ./runtime/k8s/...` | kind, kubectl, the overlay's images | tags and dispatch |
| conformance | `go test -v -run '^TestContract' ./test/conformance -args -url $CELLA_TEST_URL -token ...` | a server URL | against every tier's server ([[015-conformance-suite]]) |

The `e2e` and `podman` tags sit on the files under `test/e2e/` and on
each driver's conformance test file; the native driver's conformance
test is untagged and in the unit suite. The kind tier is where the
no-inbound proof of [[021-data-plane-workers]] is a network fact: the
worker's Pod carries a NetworkPolicy denying all ingress, and the tier
runs the whole lifecycle through it.

### The kind overlay

`deploy/examples/kind/` holds `kind.yaml` (two node pools, the default
CNI disabled), `up.sh`, `down.sh`, and the kustomize overlay. `up.sh
-name <cluster> <images...>` creates the cluster, installs Cilium so
NetworkPolicy is enforced, loads the `cellad` and `cella-stubs` image
tarballs, applies the overlay, waits for readiness, and prints
`CELLA_TEST_URL` and a token. The overlay runs: one `cellad serve` Pod
with the issuer, authorizer, admission, and sink as sidecar containers,
so every webhook URL is `http://127.0.0.1:<port>` and passes the
loopback rule of [[006-identity]] and [[007-admission]] with no hatch;
Postgres; `cellad egress` with the upstream stub beside it; and, on the
second node pool, an `Environment` with `mode: worker` and a `cellad
worker` Pod under a deny-all ingress policy. `down.sh -name <cluster>`
deletes the cluster.

### CI

`verify.yml` gains one job per tier, each on hosted runners, each
running the exact command of the table with `-v` so the log names the
tests that ran: `e2e-native`, `e2e-postgres`, `e2e-local` (with the
install step), `e2e-worker`, and `podman` on every push; `kind` on tags
and dispatch, after a `build` job that produces the two image tarballs
from `Dockerfile` and `Dockerfile.stubs`; `e2e-local-macos` on
dispatch only. Every tier's log is the proof a spec's criterion
cites.

### Configuration

| Variable | Owner | Default | Purpose |
|---|---|---|---|
| `CELLA_EGRESS_CA_BUNDLE` | 012 | unset | PEM certificate authorities the gateway trusts beside the system roots when it dials an upstream; unset in production; the tiers name the upstream stub's |
| `CELLA_TEST_URL`, `CELLA_TEST_TOKEN` | 012 | unset | what the conformance tier and the kind tier read; printed by `up.sh` |

## Not in this spec

The release pipeline that reuses the kind job and publishes
`cella-stubs` ([[014-release-and-installation]]); the suite the
conformance tier runs ([[015-conformance-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each stub serves its contract and every flag in its row is driven | `TestTheIssuerIsOneTheVerifierAccepts`, `TestTheAuthorizerPassesTheSharedConformanceSuite`, `TestTheAdmissionEndpointAnswersTheEnvelope`, `TestTheSinkVerifiesWhatTheDelivererSigns` and their neighbours in `internal/stubs` | passing for the four roles of `cella-stubs` (049); the upstream stub waits on the egress tiers |
| The sink refuses a body whose signature does not verify, a stale `t`, and accepts either secret | `TestTheSinkRefusesWhatItCannotVerify`, `TestTheSinkVerifiesWhatTheDelivererSigns` | passing (049) |
| `make run` on a clean clone completes the bootstrap in order, the environment is `Ready` with the gateway connected, and the printed `curl` apply of the minimal manifest succeeds; `make run-down` stops everything; two clones run side by side | `TestRunBootstrap` in `test/run`, behind the `e2e` tag | passing for the stubs and `cellad serve` (049): the bootstrap needs no issuer, mints a token and creates a sandbox with it, and an interrupt stops both. The gateway leg, `run-down` and the side-by-side run wait on the egress tier; a second clone runs today with `CELLA_RUN_PORT` |
| `make run-worker` registers a second environment and a sandbox applied to it runs on the worker | `TestMakeRunWorker` | not built |
| The native tier creates, execs, stops, and deletes through the API with events at the sink | `TestNativeLifecycle` | not built |
| The postgres tier recovers a lost sandbox after a `cellad` restart and passes the store suite | `TestPostgresRecovery` | not built |
| The local tier reaches the upstream through the gateway with its placeholder substituted and cannot reach an unlisted host; the sandbox runtime's policy names only the gateway | `TestLocalEgress` | not built |
| The worker tier runs the lifecycle on the worker's environment while the worker opens no listening socket and `cellad` opens no connection toward it | `TestWorkerLifecycle`, `TestWorkerNoInbound` | not built |
| The podman tier runs the lifecycle and the driver's conformance suite rootless | `TestPodmanConformance` through `make test-podman` | the driver's suite runs rootless in `verify` and in the release pipeline (048, 049); the API lifecycle over podman waits on `test/e2e` |
| The kind tier runs the lifecycle with a Pod and a PVC observed, through Cilium-enforced policy, on both the in-process environment and the worker's; `up.sh` and `down.sh` leave nothing behind | `TestClusterLifecycle` over `deploy/examples/kind-stubs` | passing for the in-process environment (049): create, ready, exec, delete and the records at the sink, with `up.sh` and `down.sh` as the overlay's own. Cilium and the worker's half wait on the worker tier |
| Every tier binds `:0`, keeps state under `t.TempDir()`, tears down its containers, and leaves no `srt-mux-*.sock` | `TestTiersAreIsolated` | not built |
| The verify workflow has one job per tier with the command from the table | `TestTheInstallJobWalksTheDocument`, `TestTheReleaseRunsTheStubsAndTheStack` | the `install` job runs the bootstrap tier, the document's walk and the kind tier on one cluster per run, and the release pipeline runs the kind stack and publishes `cella-stubs` (049). One job per tier arrives with the tiers that are not built |
