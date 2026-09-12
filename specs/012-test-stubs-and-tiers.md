---
title: "Test stubs and tiers: the stub issuer, authorizer, admission, and sink; make run; the backend tiers; CI jobs"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/006-identity.md
  - specs/007-admission.md
  - specs/009-events.md
affects: [test/stubs/, test/e2e/, Makefile, .github/workflows/, deploy/examples/kind/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Test stubs and tiers

## Overview

Every operator endpoint the core dials has a stub in the tree, small
and honest, so `make run` gives a clean clone a working system and so
the tiers exercise the real clients against a real HTTP peer. The tiers
are selected by build tag and test name prefix, run where their
substrate exists, and never by wall-clock guess. One binary,
`cella-stubs`, serves all four stubs, so the kind overlay runs one Pod.

## Current state

Not built. The shape is Origo's stubs, with an admission stub added.

## Design

### The stubs

| Stub | Serves | Behaviour |
|---|---|---|
| issuer | `/.well-known/openid-configuration`, `/jwks`, `POST /mint {"sub"}` | a real OIDC issuer over one generated key; mints any subject asked, which is what a stub is for and why it never runs in production |
| authorizer | the contract of [[006-identity]] | allow everything, with `X-Stub-Deny: <action>` on the incoming request or a `-deny` flag to refuse; records every request for the suite to read back |
| admission | the contract of [[007-admission]] | returns the manifest unchanged, or with a `-rewrite image=<ref>` applied, or refuses with `-refuse`; records requests |
| sink | the contract of [[009-events]] | verifies the signature, stores events, serves them at `GET /events`, fails the first `-fail-first N` deliveries |
| gateway | `cella-egress` itself, not a stub | the real gateway of [[018-egress-and-secrets]] on a loopback port with a generated CA, so the tiers prove substitution against the code that ships |
| upstream | an HTTPS server the sandboxes reach | records every request's host, headers, query, and body, so a tier asserts what left the sandbox and what the gateway rewrote |

Each stub is a package under `test/stubs/` with a handler and a test,
and `test/stubs/cmd/cella-stubs` serves them on four ports.

### make run

Builds `cellad`, `cella-egress`, and `cella-stubs`, generates
`CELLA_TOKEN_KEY` and `CELLA_SECRETS_KEK` under `out/` once, starts the
stubs and the gateway on loopback ports derived from the checkout's
directory name, starts `cellad` with the `local` driver where the
sandbox runtime is installed and `native` otherwise, mints a token for
subject `dev`, and prints `export CELLA_URL=... CELLA_TOKEN=...` and a
`cella apply` line. `make run-worker` starts a `cella-worker` against
the same control plane with a second environment, so the self-hosted
path is one command away. `make run-down` stops all of it.

### Tiers

| Tier | Tag | Prefix | Needs | Runs |
|---|---|---|---|---|
| unit | none | any | Go | every push, the gate |
| native e2e | `e2e` | `TestNative` | Go | every push, `make test-e2e` |
| local | `e2e` | `TestLocal` | the sandbox runtime | every push on a runner that has it; skipped with the remediation printed otherwise |
| worker | `e2e` | `TestWorker` | Go | every push: a `cella-worker` running `native` behind a firewall that refuses inbound, against the same `cellad` |
| podman | `podman` | `TestPodman` | a Podman socket | every push on a runner with Podman |
| kind | `e2e` | `TestCluster` | kind, kubectl | tags and dispatch |
| conformance | none | `TestContract` | a server URL | against every tier's server ([[015-conformance-suite]]) |

Every tier starts its own `cellad` and stubs as processes and asserts
through the API and the backend both; a native tier check that a
directory exists, a kind tier check that a Pod has the label. The kind
overlay under `deploy/examples/kind/` runs `cellad`, `cella-egress`,
the stubs, Postgres, and a second node pool with a `cella-worker`, and
`up.sh` loads the candidate images.

### CI

`verify.yml` gains `e2e-native` and `podman` on every push, `kind` on
tags and dispatch, each passing `-v` so the log names the tests that
ran. A tier's tests run under a prefix and nothing else, so a new test
is in one tier by its name.

## Not in this spec

The release pipeline that reuses the kind job
([[014-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each stub serves its contract and its test drives every behaviour flag | one test per stub package | not built |
| `make run` on a clean clone prints a token and `cella apply` of the minimal manifest succeeds against it | `TestNativeMakeRun` | not built |
| The native e2e tier creates, execs, stops, and deletes through the API with events at the sink | `TestNativeLifecycle` | not built |
| The worker tier does the same on the worker's environment with no inbound connection to the worker's host | `TestWorkerLifecycle` | not built |
| A sandbox in the local tier reaches the upstream through the gateway with its placeholder substituted and cannot reach an unlisted host | `TestLocalEgress` | not built |
| The kind tier does the same with a Pod and a PVC observed | `TestClusterLifecycle` | not built |
| The sink stub refuses a body whose signature does not verify | `TestSinkVerifiesSignature` | not built |
