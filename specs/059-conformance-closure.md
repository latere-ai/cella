---
title: "Conformance closure: the drift seam, the external run on dispatch, the agent scenario against this server"
status: in-progress
track: core
depends_on:
  - specs/015-conformance-suite.md
  - specs/003-manifest-contract.md
  - specs/011-agent-client.md
  - specs/012-test-stubs-and-tiers.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/052-conformance-suite.md
affects: [internal/config/, cmd/cellad/, test/conformance/, .github/workflows/, docs/, specs/]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Conformance closure

## Overview

[[052-conformance-suite]] built the suite of [[015-conformance-suite]] and
left two of its rows open, both on purpose: the drift seam, because it is a
change to a server package and that slice changed none, and the job that runs
the documented command against somebody else's address, because nothing in
the pipelines took one. This slice builds both, and closes the one row of
[[011-agent-client]] that waited on the suite: the agent scenario run through
the built `cella` against a real server.

The drift seam exists to prove the suite notices a server that resolves a
default wrongly. A suite whose defaults case only compares what an apply
answered with what a read answers cannot notice: a server that drifts
drifts in both. So the defaults case also has to read the defaults the
manifest contract states as literals, which every conforming server
resolves identically whatever its operator configured.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `case003DefaultsAreReturned` | `test/conformance/cases_manifest.go` | it asserts the status fields and that a read equals the apply's answer; it reads no default, so a consistently drifted default passes |
| `CELLA_TEST_DRIFT_DEFAULT` | named in [[002-repository-scaffold]]'s variable table and in [[015-conformance-suite]]'s drift seam | nothing reads it |
| The in-process run | `TestTheConformanceSuiteHoldsAgainstThisServer` in `cmd/cellad` | `Cella` is empty, so `case011AgentScenario` skips on every tier |
| The documented command | `docs/conformance.md` | carries no `-count=1`, so a build cache that holds an earlier pass replays it without asking the server, and no `-timeout`, so `go test`'s ten minute default cuts a run the same page says may take fifteen |
| The external run | `verify.yml`'s `install` job and `release.yml`'s `conformance` job run the suite against stacks they bring up themselves | no job takes an address on dispatch |

## Design

### The literal defaults

[[003-manifest-contract]]'s field table gives some defaults as literals and
some as operator configuration (`Defaults.CPU`, `Defaults.TTL`, the image,
the default environment). The literals are the same on every conforming
server, so they are what a black-box suite can hold a server to:

| Field | Resolved value for a manifest that names none of these fields |
|---|---|
| `spec.workspace.path` | `/workspace` |
| `spec.workspace.source` | `empty` |
| `spec.workdir` | the resolved `spec.workspace.path` |
| `spec.network.egress.mode` | `open`, for a manifest with no host and no mounted secret |
| `spec.mesh.enabled` | `false` or absent |
| `spec.mesh.spawn.budget`, `spec.mesh.spawn.depth` | `0` or absent |

`case003DefaultsAreReturned` asserts each row on the answer to the apply,
beside what it asserts today, and reports the first field that differs as a
disagreement naming the field, the literal and the value that arrived. The
operator defaults stay unread: the suite does not know an installation's
configuration, and a case that guessed it would fail every installation that
chose differently.

### The drift seam

`CELLA_TEST_DRIFT_DEFAULT=<field>` names one field whose default the server
resolves one unit off. It is a test seam, so it is built so that it cannot be
set by accident in a deployment:

| Rule | Why |
|---|---|
| The value is one of a closed set, `spec.mesh.spawn.budget` or `spec.mesh.spawn.depth`; any other non-empty value is a start-up problem naming the variable and the set | a misspelled value fails loudly rather than drifting nothing |
| It is accepted only with `CELLA_RUNTIME=native`, which itself requires `CELLA_ALLOW_UNSAFE_NATIVE=true`; set with any other runtime it is a start-up problem | no installation that isolates anything starts with it, so a deployment that carries it by accident does not come up |
| A server started with it logs a warning at start-up naming the field and saying the server does not conform | an operator reading the log of a development node sees why its answers are off |

The two fields are the integers whose literal default is `0` and which change
nothing a native sandbox does: a budget of one with a depth of zero, or a depth
of one with a budget of zero, admits no child, so the drifted server answers
every other case exactly as the honest one. `internal/config` reads and checks
the variable into `Config.DriftDefault`.

The drift is applied in `cmd/cellad`, as a wrapper around stage 3 of
[[003-manifest-contract]]'s resolve: before the operator's admission step reads
the defaulted object, a field still at its literal `0` is set to `1`. Every
later stage, and the admission step itself, reads the drifted object as the
defaulting stage's output. The wrapper is in the command and not in the
`manifest` package, so the package every platform composes carries no test
seam, and a server built from the packages without `cellad` has none to set.

`TestSuiteCatchesADriftedDefault` starts `cellad serve` in the process with
the stubs, as the in-process run does, with `CELLA_TEST_DRIFT_DEFAULT=spec.mesh.spawn.budget`,
runs the whole suite, and asserts the failed list is exactly
`case003DefaultsAreReturned`: nothing else fails, no declared gap is touched,
and the disagreement names the field.

### The external run

A new workflow, `.github/workflows/conformance.yml`, runs on
`workflow_dispatch` only, with one job, `conformance-external`. It is its own
workflow rather than a job of `verify.yml` because a dispatch of `verify.yml`
runs the gate, the image build and the install walk beside it, all of which
are about this tree and none about the address given; and the release
pipeline reads `verify.yml`'s runs to decide whether a tag is green, which a
run against somebody else's server has no part in.

| Input | Source | Reaches the command as |
|---|---|---|
| the server's URL | the `url` input, required | `CELLA_TEST_URL` |
| the capabilities the environment declares | the `capabilities` input, optional | `CELLA_TEST_CAPABILITIES` |
| the image every case creates from | the `image` input, optional | `CELLA_TEST_IMAGE` |
| the caller's bearer | the repository secret `CONFORMANCE_TOKEN` | `CELLA_TEST_TOKEN` |
| an administrator's bearer | the repository secret `CONFORMANCE_ADMIN_TOKEN`, optional | `CELLA_TEST_ADMIN` |

A dispatch input is shown in the run's summary to anyone who can read the
run, so a token is never one. Every value reaches the step through `env`,
never through an expression inside the script, so neither an input nor a
secret is part of the script text the runner prints, and an input cannot
inject a command. A step with no token fails before the suite runs, with the
secret named, rather than reporting every authenticated case as failed.

The step runs the documented command, with its flags taking the values from
those variables, and passes when `TestContract` passed.
`TestTheExternalRunIsTheDocumentedCommand` reads `docs/conformance.md`'s
command and the job's, and holds them equal: the same `go test` flags, the
same package, and the same suite flags in the same order, with the values
free. The documented command gains `-count=1` and `-timeout 30m`, for the two
reasons in Current state, and the job carries both because the test holds it
to the page.

### The agent scenario against this server

`TestTheConformanceSuiteHoldsAgainstThisServer` builds `./cmd/cella` into a
temporary directory and passes it as `Cella`, so `case011AgentScenario`
applies, execs, reads and deletes through the binary against the node in
the process on every push, where it skipped before.

## Not in this slice

The kind half of [[015-conformance-suite]]'s timing row asserts the
capabilities the pipelines pass, `files` and `pool`; the Kubernetes driver
also declares `mesh`, and asserting it on kind is the spawn group's to prove
([[022-mesh-and-spawn]]). The operator defaults as server configuration
(`CELLA_DEFAULT_CPU` and the rest of [[007-admission]]'s rows) and a suite
input that states them. A control endpoint in `cella-stubs`
([[012-test-stubs-and-tiers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `CELLA_TEST_DRIFT_DEFAULT` accepts only the closed set of fields and only with the native runtime; any other value, or the variable with any other runtime, is a start-up problem naming the variable | `TestTheDriftDefaultIsATestSeam` | open |
| The drift sets a field still at its literal default one unit off before the admission step reads it, leaves a field the resolve already moved alone, and is the identity when unset | `TestTheDriftMovesOneDefault` | open |
| The defaults case reads the literal defaults, passes against a server that resolves them, and reports the field against one that drifts | `TestADriftedDefaultIsReportedFailed`, `TestTheConformanceSuiteHoldsAgainstThisServer` | open |
| A server started with `CELLA_TEST_DRIFT_DEFAULT` fails exactly the resolve group's defaults case | `TestSuiteCatchesADriftedDefault` | open |
| The dispatch job runs the documented command against the address given, with the token from a repository secret through the environment and never in the script | `TestTheExternalRunIsTheDocumentedCommand` | open |
| The agent scenario runs through the built `cella` against this server on every push | `TestTheConformanceSuiteHoldsAgainstThisServer` | open |
