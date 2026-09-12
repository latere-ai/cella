---
title: "Conformance suite: the manifest contract and the API as executable tests, against any server"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/008-api.md
  - specs/011-agent-client.md
affects: [test/conformance/, test/e2e/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Conformance suite

## Overview

The contract is a suite, not a document: `test/conformance` is an
importable Go test package that, given a server URL and a token, runs
every acceptance criterion of the manifest contract and the API and
reports which hold. `cellad` passes it on every backend in the tiers;
a platform built on the packages runs it against its own front to prove
that its edge did not change what a manifest means. A release does not
publish until the suite is green from the published images.

## Current state

Not built.

## Design

### Shape

```go
func Run(t *testing.T, cfg Config)

type Config struct {
	URL      string
	Token    func(subject string) string // mints for the suite's subjects
	Backend  string                     // what the server declares; gates capability cases
	Features Features                   // what the server under test claims to serve
}
```

Cases are functions `case<NNN><Name>(t, *client)` named after the spec
whose criterion they prove, so a failing case names its spec. The suite
mints tokens for three subjects (`alice`, `bob`, `admin`) through
`Token`, and a server whose issuer is not a stub supplies a `Token` that
knows how.

### Groups

| Group | Proves | From |
|---|---|---|
| decode | every refusal code of decoding, version before kind, unknown fields at depth | 003 |
| resolve | defaults returned, immutability, ceilings, determinism across `POST` and `PUT` | 003 |
| lifecycle | the phase transitions reachable through the API and their refusals | 005, 008 |
| identity | `unauthenticated`, `forbidden`, the owner policy or the authorizer, the workload token's scope | 006 |
| streams | exec framing, exit codes, attach round trip, tar both ways, logs | 008 |
| events | every mutation and exec yields its event on `GET .../events` with no secret in it | 009 |
| agent | the scenario an agent runs from the skill alone: apply, exec, cp, delete, by way of `cella` | 011 |
| capability | `display`, `resize`, `network`, `persist`, each gated on `Features` | 004 |

Each case cleans up what it created and runs under a timeout; the suite
runs in under five minutes against the native backend.

### Where it runs

The native tier on every push; podman where a socket exists; kind on
tags; the release pipeline from the published images. `go test
./test/conformance -args -url ... -token ...` runs it against any
server, which is the command a platform puts in its own CI.

## Not in this spec

The stubs a server needs beside it ([[012-test-stubs-and-tiers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every acceptance criterion of 003 and 008 that names a conformance case has one, and the case names the spec | `TestEveryCriterionHasACase` reading the specs | not built |
| The suite passes against `cellad` on the native backend in under five minutes | the native tier | not built |
| The suite passes against the k8s backend on kind with every capability case enabled | the kind tier | not built |
| A server that changes one default fails exactly the resolve group's defaults case | `TestSuiteCatchesADriftedDefault` against a mutated server | not built |
| The suite runs from a clean checkout against an external URL with the documented command | `TestExternalInvocation` | not built |
