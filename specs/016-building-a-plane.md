---
title: "Building a plane: how a platform composes the packages and the webhooks without a fork"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/004-runtime-backend-contract.md
  - specs/006-identity.md
  - specs/007-admission.md
  - specs/015-conformance-suite.md
affects: [docs/plane.md, manifest/, runtime/, controller/]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Building a plane

## Overview

A platform that sells sandboxes with accounts, plans, a console, and
its own runtime additions builds on Cella in one of two ways, or both
in sequence: run `cellad` and implement the webhooks, or import the
packages and wrap them in its own server. This spec is the guide for
that platform, written so the first one to do it, the hosted plane
this core was extracted from, follows the same document any other
would. It also fixes what the core promises such a platform and what
it does not.

## Current state

Not built. The hosted plane runs today as one binary with its own copy
of the manifest types and runtime backends; its migration onto these
packages is its own work, tracked in its own repository, and this spec
is what it migrates against.

## Design

### Two doors

| Door | Platform runs | Platform writes | Gets |
|---|---|---|---|
| webhooks | `cellad` as a service | an authorizer, an admission endpoint, a sink; an OIDC issuer it already has | the whole core, upgraded by image tag; its logic in its own service in any language |
| packages | its own binary importing `manifest`, `runtime`, `controller` | a server around them, its own identity and store | the contract and the backends in-process; no HTTP hop; its own API shape if it wants one |

A platform that starts with the packages and later splits into a
service is not rewriting: the packages are what `cellad` is made of.

### Migrating an existing manifest

A platform whose callers already write an older group name maps it at
its own edge: accept the old `apiVersion`, rewrite it to `cella/v1`,
drop or translate the fields the core does not have into annotations
under the platform's prefix, and hand the result to `Resolve`. The
resolved object it returns carries `cella/v1`, so callers learn the
new name from every response. The core accepts one group and never
carries an alias.

### Where each concern goes

| Concern | Door: webhooks | Door: packages |
|---|---|---|
| accounts and orgs | the issuer's claims, read by the authorizer | the platform's own middleware before `Resolve` |
| plans and quotas | authorizer `limits`, admission ceilings | the platform's `Options.Ceilings` and its own count check |
| image catalog | admission rewrites `spec.image` | an `AdmitFunc` in `Options` |
| secret brokering | admission places a reference in `env`; a sidecar the k8s decorator adds resolves it | the same decorator, constructed in-process |
| audit and usage | the sink | the platform's own `events.Sink` implementation |
| a console | reads `/v1` | reads the platform's own API |
| multi-region | one `cellad` per region behind the platform's router | one controller per region |

### Promises

To a platform, the core promises the manifest contract's rules, the
packages' compatibility rules of [[001-architecture]], that the
conformance suite passes against `cellad` on every release, and that a
platform passing the suite against its own front serves the same
contract. It does not promise the shape of `internal/`, the stub
binaries, or the deploy manifests beyond a release.

### The document

`docs/plane.md` is this spec in the user register: the two doors, a
minimal authorizer in twenty lines, a minimal admission endpoint, the
sink, the migration recipe, and the conformance command.

## Not in this spec

Any platform's own migration plan.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A twenty-line authorizer and admission endpoint from `docs/plane.md`, run beside `cellad`, pass the conformance suite's identity and resolve groups | `TestPlaneDocEndpointsConform` running the doc's code blocks | not built |
| A server built from the packages in `examples/plane/` passes the conformance suite | `TestExamplePlaneConforms` | not built |
| The example plane maps an older group name at its edge and the response carries `cella/v1` | `TestExamplePlaneMapsTheOldGroup` | not built |
| Every row of the concerns table names a mechanism that exists in the tree | `TestConcernsTableIsGrounded` reading this file | not built |
