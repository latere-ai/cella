---
title: "Admission: defaults, ceilings, named policies, the admission webhook"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/006-identity.md
affects: [internal/admission/, internal/config/, test/stubs/]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Admission

## Overview

Between defaulting and validation, the resolve pipeline of
[[003-manifest-contract]] hands the manifest to an admission step that
may change it or refuse it. Built in, the step applies the operator's
defaults and ceilings from configuration. Configured, it is one `POST`
to an endpoint the operator writes, which is where a platform puts its
image catalog, its policy profiles, its quota per plan, and the
sidecars its runtime needs. The core learns none of that; it learns
that a manifest came back or a refusal did.

## Current state

Not built. The hosted plane applied defaults, catalog resolution, and
policy gates in three per-surface code paths; this spec is where they
become one step behind one contract.

## Design

### Built-in step

With `CELLA_ADMISSION_URL` unset, the step is:

1. `Defaults` from `CELLA_DEFAULT_*`, applied by stage 2 of `Resolve`:
   the `manifest.Defaults` fields `CPU`, `Memory`, `Disk`, `AutoStop`,
   `TTL`, `AutoDelete` map one to one onto the six variables of
   [[002-repository-scaffold]]; `manifest.Ceilings` fields `CPU`,
   `Memory`, `Disk`, `TTL` onto the four `CELLA_MAX_*`.
2. `spec.policy`, when set, must name one of the built-in policies:
   `default` (no change) or `restricted` (`network.egress.mode: allowlist`
   with the manifest's hosts, `user` forced to non-root when unset,
   `display` refused). An unknown name is `admission_refused`.
3. `Ceilings` from `CELLA_MAX_*`, checked by stage 5.
4. A count ceiling: `CELLA_MAX_SANDBOXES_PER_SUBJECT` (default `0`, no
   ceiling), or the authorizer's `limits.max_sandboxes` when present,
   refused with `quota_exceeded`.

### The webhook

```
POST {CELLA_ADMISSION_URL}
Authorization: Bearer {CELLA_ADMISSION_TOKEN}
Content-Type: application/json

{
  "subject":  "alice@example.com",
  "claims":   {...},
  "action":   "create" | "update",
  "existing": <the current object on update, null on create>,
  "manifest": <the defaulted object>,
  "limits":   <the authorizer's limits, if any>
}
```

Response, 200:

```json
{
  "allow": true,
  "manifest": <the object to continue with>,
  "reason": "",
  "warnings": ["..."]
}
```

Rules: the returned `manifest` replaces the input and goes through
structural validation again, so a webhook that returns an unknown field
or a bad quantity yields `invalid_field` naming the path, not a crash
and not a pass. The webhook may not change `apiVersion`, `kind`,
`status`, or `metadata.name` on update; a change there is
`admission_refused` with the path in the developer detail. `warnings`
are appended to the resolved manifest's. `allow: false` is
`admission_refused` with the `reason` as the developer detail. A
non-200, a timeout (`CELLA_ADMISSION_TIMEOUT`, default `3s`), or a body
that does not parse is `admission_unavailable`, 503, never a pass. The
built-in ceilings still apply after the webhook, so an operator's
`CELLA_MAX_*` is a floor on strictness a webhook cannot lower.

### What a plane does here

An image catalog: rewrite `spec.image: python` to a pinned digest. A
plan: set `resources` and `lifecycle` from the subject's plan and refuse
what exceeds it. A sidecar: add an annotation its k8s decorator reads.
A secret broker: replace a placeholder in `env` with a reference its
egress proxy resolves. None of these is the core's, and each is one
`if` in the operator's endpoint.

## Not in this spec

The decorators the annotations drive ([[004-runtime-contract]]);
the stub admission endpoint ([[012-test-stubs-and-tiers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The built-in defaults and ceilings apply with no URL set | `TestBuiltInAdmission` | not built |
| `restricted` forces the allow list and a non-root user and refuses `display` | `TestRestrictedPolicy` | not built |
| The webhook receives the defaulted manifest and its returned manifest is what the backend gets | `TestWebhookOutputIsWhatRuns` against the stub | not built |
| A webhook that returns an unknown field is `invalid_field`; one that changes `kind` is `admission_refused` | `TestWebhookOutputIsValidated` | not built |
| Down, timeout, 500, and a malformed body are `admission_unavailable` and never a pass | `TestAdmissionFailsClosed` | not built |
| A webhook cannot raise a value above `CELLA_MAX_*` | `TestCeilingsAreAFloorOnStrictness` | not built |
| The count ceiling refuses the (n+1)th sandbox with `quota_exceeded` and admits it after a delete | `TestCountCeiling` | not built |
