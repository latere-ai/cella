---
title: "Conformance suite: the API contract as executable cases against any server, with a report"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/011-agent-client.md
  - specs/012-test-stubs-and-tiers.md
  - specs/018-egress-and-secrets.md
  - specs/019-volumes.md
  - specs/020-scheduling-and-sets.md
  - specs/021-data-plane-workers.md
  - specs/023-computer-use-operations.md
affects: [test/conformance/, test/e2e/, internal/config/, .github/workflows/]
effort: large
created: 2026-09-12
updated: 2026-09-20
author: changkun
---

# Conformance suite

## Overview

The contract is a suite, not a document: `test/conformance` is an
importable Go test package that, given a server URL and tokens, runs
every acceptance criterion the specs mark as a conformance case over
the API and reports which held, which failed, and which were skipped
and why. `cellad` passes it on every tier; a platform built on the
packages runs it against its own front to prove that its edge did not
change what a manifest means; a release does not publish until it is
green from the published images. This is the API-level suite. The
driver-level suite is `runtimetest` of [[004-runtime-contract]], and a
criterion that cites `runtimetest` is not a conformance case here.

## Current state

Not built.

## Design

### Shape

```go
// Run executes every group the inputs allow and returns what happened.
func Run(t *testing.T, cfg Config) Report

type Config struct {
	URL   string
	Token func(ctx context.Context, subject string) (string, error) // mints for alice and bob
	Admin string // a token the server treats as an administrator: environments, ?owner=

	Image        string // an image with a shell and curl; every case creates from it
	DisplayImage string // an image with a browser and the desktop; empty skips computer use
	Upstream     string // the host of 012's upstream stub as sandboxes reach it; empty skips egress

	QueuedEnvironment string // a queued environment; empty skips sets
	WorkerEnvironment string // an environment served by a worker; empty skips indistinguishability
	AuthorizerControl string // the stub authorizer's control URL; empty skips the deny, retry, probe, and unavailable cases

	Skip []string // group or case names to skip, reported as skipped by request
}

type Report struct {
	Passed, Failed, Skipped []string // case names, <NNN>/<Name>
	Reasons map[string]string        // why each skipped case skipped
	Created []string                 // every object id the run made and deleted
}
```

The suite learns the server, never the other way: a sandbox's
`status.driver` and `status.isolation` and its environment's
`status.capabilities`, read through `GET /v1/environments/{id}` with
`Admin`, gate the capability cases, so a server that declares a
capability it does not honour fails rather than being excused. Cases
are `case<NNN><Name>` functions in `test/conformance`, run as subtests
`<NNN>/<Name>`, named after the spec whose criterion they prove; each
runs under a 2 minute timeout, creates every object with a per-run
name prefix `conformance-<run>-`, records every id it created, and
deletes those ids and only those, never by prefix or by list, so two
runs against one server and a run against a live server touch nothing
they did not make. The client is `internal/cellaclient`
([[011-agent-client]]); the agent group shells out to the built `cella`
binary.

### Groups

Every group's scope names its routes and the codes it produces; a
group whose input is empty skips with a reason in the report and never
silently.

| Group | Proves | Routes and codes | From |
|---|---|---|---|
| decode | every refusal code of decoding, version before kind, unknown fields at depth, the two content types, 415 and 406 | `POST /v1/sandboxes`, `PUT /v1/sandboxes/{name}`; `unsupported_media_type`, `not_acceptable`, `multi_document`, `unsupported_version`, `unsupported_kind`, `unknown_field`, `body_too_large` | 003, 008 |
| resolve | defaults returned, immutability, narrowing, ceilings, `If-Match` and `version_conflict`, and determinism: one manifest applied by `POST`, its generated name read, the identical manifest applied by `PUT` to that name, the two bodies equal with `status` stripped | the same routes; `immutable_field`, `boundary_widened`, `ceiling_exceeded`, `version_conflict`, `name_taken` | 003, 008 |
| lifecycle | the transitions reachable through the API and their refusals; `Delete` in every phase | `start`, `stop`, `DELETE`; `phase_conflict` | 005, 008 |
| identity | `unauthenticated`; `forbidden` on an own action; `not_found` through a refused reference; a workload token's scope; under `AuthorizerControl`, a deny and every unavailability mode refusing every request, a `conn-drop` retried once and no other mode retried, and the probe id denied | every route; `unauthenticated`, `forbidden`, `not_found`, `authorizer_unavailable` | 001, 006 |
| list | the envelope, paging, `?limit=300` as `invalid_field`, every selector, `?owner=` as admin intersected with the filter, `?root=` | `GET /v1/<kinds>` | 008 |
| streams | exec framing and exit codes with and without stdin, attach round trip and resize, dial, tar both ways, logs with `follow`, the capability gates | the stream routes; `capability_unsupported` | 004, 008 |
| errors | every code of [[008-api]]'s table the suite can provoke arrives with its status and its fixed sentence; the two public documents need no bearer | every route | 008 |
| events | every mutation and operation the suite performs yields its event on `GET /v1/events?object=` with a planted canary secret value appearing in none | the events routes | 009 |
| secrets | the kind's field rules, `spec.value` write-only on `GET` and list, `secret_host_conflict`, `secret_out_of_scope`, `notInjectable` after a delete | `/v1/secrets`; the codes named | 018 |
| volumes | the kind's field rules, access modes and `volume_busy` both ways, a workspace volume surviving its sandbox, `retain`, snapshots where declared | `/v1/volumes`, snapshots; `volume_busy`, `invalid_field` | 019 |
| sets | a set of 8 with parallelism 2 to completion on `QueuedEnvironment`, results collected, `PUT` of `parallelism`, `stop`, a widening variant refused | `/v1/sandboxsets`; `boundary_exceeded` | 020 |
| environments | list and read as admin; `environment.use` refused for a non-admin on a non-default environment; `capability_unsupported` for a scheduling field on a direct one | `/v1/environments` | 021, 003 |
| egress | through both doors toward `Upstream`: substitution in the declared place, passthrough elsewhere, `allowlist`, `open` with a deny list, `none`, the records route | exec inside the sandbox; `GET .../egress` | 018 |
| spawn | a workload's apply within the boundary and each of the nine rules refused; `?root=`; cascade | `POST /v1/sandboxes` as a workload; `boundary_exceeded`, `spawn_budget_exhausted` | 022 |
| agent | the scenario an agent runs from the skill alone, through `cella`: apply, exec, cp, get, delete, with the exit codes | the binary | 011 |
| computer use | `case023BrowserReady` over `docs/examples/browser.yaml` with `DisplayImage`: `DisplayReady`, the port proxy, screenshot, input, screenshot again | the operations routes | 023 |
| indistinguishability | the same manifest on the default and on `WorkerEnvironment` yields responses equal but for `status.environment`, `status.driver`, and `status.isolation` | `POST /v1/sandboxes` | 001, 021 |
| capability | each capability the environment declares holds through the API, and each it does not declare refuses its field or route with `capability_unsupported` | as declared | 003, 004 |

### Time

The native tier declares no egress, mesh, display, input, or pool, so
on it the egress, computer-use, and pool cases skip and the run
completes in under five minutes; with every group enabled on the kind
tier the budget is fifteen. The kind overlay declares `egress: [none,
allowlist, open]`, `mesh`, `volumes`, `display`, `input`, `attach`,
`dial`, `files`, `resize`, and `pool`, and not `ingress` or
`snapshots`, so the kind run asserts that set.

### Where it runs

`TestContract`, under the `e2e` build tag, wraps `Run`: it reads
`-url`, either `-issuer` (a stub issuer to mint every subject from) or
`-token`, `-token-bob`, and `-admin`, and the input flags above, and
skips whole with the reason when `-url` is empty. The command is
[[012-test-stubs-and-tiers]]'s conformance tier row, `go test -v -run
'^TestContract' ./test/conformance -args -url $CELLA_TEST_URL ...`,
run against every tier's server; the release pipeline runs it from the
published images before publishing ([[014-release-and-installation]]);
a platform puts the same command in its own CI with its own tokens
([[016-building-a-plane]]). A CI job, not a Go test, proves the
external form: a clean checkout runs the command against a URL.

### The marker

A spec criterion is a conformance case when its test column contains
the literal form conformance case `caseNNNName`. `TestEveryCriterionHasACase`
reads every file under `specs/` and `specs/.archive/`, collects every
such marker, and fails on a marker with no function of that name in
`test/conformance` or a function with no marker. A criterion citing
`runtimetest` is the driver suite's and is not a marker.

### The drift seam

`CELLA_TEST_DRIFT_DEFAULT=<field>`, read by `internal/config` and
applied in the defaulting stage, makes `cellad` resolve one default
one unit off; it is empty in every deployment and exists so a test can
prove the suite notices: `TestSuiteCatchesADriftedDefault` starts
`cellad` with it set and asserts exactly the resolve group's defaults
case fails.

### What the suite does not prove

Recovery, the reaper's timing, the journal's ordering and signature,
and the rate limit against a shared bucket are not black-box
properties and are proved by their specs' own tests and the tiers.

## Not in this spec

The stubs a server needs beside it ([[012-test-stubs-and-tiers]]); the
driver-level suite ([[004-runtime-contract]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every marker in the specs has a case and every case a marker | `TestEveryCriterionHasACase` reading `specs/` and `specs/.archive/` | passing, [[052-conformance-suite]] |
| Every group in the table exists with the scope named, and skips with its reason when its input is empty | `TestGroupsAndSkips` | passing for the eighteen groups, with 51 cases in them ([[052-conformance-suite]]); the groups whose kind this API does not serve hold one case each, gated on the capability or the input they need |
| The suite passes against `cellad` on the native tier in under five minutes and against the kind tier with the declared capability set in under fifteen | the conformance tier of [[012-test-stubs-and-tiers]] | the native half passes in ten seconds, against an in-process `cellad serve` with the stubs on every push, and against the development stack in the install job ([[052-conformance-suite]]); the kind half is wired into that job and into the release pipeline and reports on the first run. The tier's command carries `-tags=e2e`, which [[015-conformance-suite]] puts `TestContract` behind |
| A run leaves nothing behind and two concurrent runs against one server touch none of each other's objects | `TestRunCleansUp`, `TestConcurrentRuns` | passing as `TestRunCleansUp`, which asserts every created id deleted, nothing else deleted, and two runs naming their objects apart ([[052-conformance-suite]]) |
| A server started with `CELLA_TEST_DRIFT_DEFAULT` fails exactly the resolve group's defaults case | `TestSuiteCatchesADriftedDefault` | not built: the drift default is a change to `internal/config` and [[052-conformance-suite]] changed no server package. The same property holds against a server that answers one field wrong, `TestAServerThatAnswersTheWrongValueIsReportedFailed` |
| A server declaring a capability it does not honour fails the capability group | `TestSuiteCatchesAFalseCapability` against a lying server | passing, [[052-conformance-suite]] |
| A clean checkout runs the documented command against an external URL | the `conformance-external` CI job on dispatch | the documented command runs against the development stack and against the kind stack in `verify.yml`'s install job and in the release pipeline ([[052-conformance-suite]]); a job that takes an address of somebody else's on dispatch is not built |
