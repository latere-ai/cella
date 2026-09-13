---
title: "Admission: AdmitFunc, defaults and ceilings, the admission webhook, the count ceiling"
status: validated
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/006-identity.md
affects: [manifest/, internal/admission/, internal/api/, internal/config/, test/stubs/]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Admission

## Overview

Stage 3 of `Resolve` ([[003-manifest-contract]]) hands the defaulted
manifest to an admission function that may change it or refuse it.
Built in, the function is the identity. Configured, it
is one `POST` per apply to an endpoint the operator writes, which is
where a platform puts its image catalog, its policy profiles, its plan
shapes, and the secrets its runtime brokers. The control plane learns
none of that; it learns that a manifest came back or a refusal did.
This spec also owns where the operator's defaults and ceilings come
from, and the one check that needs a count the pipeline does not have,
the per-subject sandbox ceiling.

## Current state

Not built. The hosted platform applied defaults, catalog resolution,
and policy gates in three per-surface code paths; this spec is where
they become one function behind one contract.

## Design

### AdmitFunc

Declared in `manifest`, so an importer supplies its own and
`internal/admission` supplies the one built from configuration:

```go
// AdmitRequest is what an admission step knows beyond the manifest.
type AdmitRequest struct {
	Actor       Actor                 // 006's rendered subject and whether it is a workload
	Claims      map[string]any        // the caller's OIDC claims, verbatim
	Workload    *v1.SandboxStatus     // the calling sandbox's status when Actor.Workload
	Action      string                // "create" or "update"
	Existing    *v1.Sandbox           // the current object on update; nil on create
	Parent      *v1.Sandbox           // the spawning sandbox, when spawned
	Environment *v1.Environment       // the environment the manifest names, with its capabilities
	Set         *SetRef               // {Name, Index} for a SandboxSet replica
	RequestID   string                // 008's X-Request-Id
}

// AdmitFunc returns the object to continue with, warnings for
// status.warnings, or an error. A nil AdmitFunc is the identity.
type AdmitFunc func(ctx context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error)
```

Admission runs for `Sandbox` applies only, including every replica of
a `SandboxSet`, one call per replica as the scheduler admits it, with
`Set` naming the index. `Secret`, `Volume`, `SandboxSet`, and
`Environment` applies are governed by the authorizer alone
([[006-identity]]). Admission output is a function of the manifest
body and is never cached.

### Defaults and ceilings

`Options.Defaults` and `Options.Ceilings` apply at stages 2 and 5
whether or not a webhook is configured. Their fields map one to one
onto [[002-repository-scaffold]]'s variables: `Defaults.CPU`,
`.Memory`, `.Disk`, `.AutoStop`, `.TTL`, `.AutoDelete` from the six
`CELLA_DEFAULT_*`; `Ceilings.CPU`, `.Memory`, `.Disk`, `.TTL` from the
four `CELLA_MAX_*`. Precedence for an absent field at stage 2: the
environment's `spec.scheduling.defaultQueue` before `CELLA_DEFAULT_*`
([[020-scheduling-and-sets]]). Stage 5 runs after stage 3, so `Ceilings`
apply to the admission output; there is no second check, and a webhook
cannot raise a value above them.

### The built-in step

With `CELLA_ADMISSION_URL` unset, `Admit` is the identity. The core
carries no named policies: a platform expresses profiles through its
webhook, keyed off the labels a manifest carries, so one mechanism
serves every policy and the schema carries no field for it.

### The webhook

```
POST {CELLA_ADMISSION_URL}
Authorization: Bearer {CELLA_ADMISSION_TOKEN}
Content-Type: application/json

{
  "subject":     "https://login.example.com|alice",
  "issuer":      "https://login.example.com",
  "sub":         "alice",
  "claims":      {...},
  "workload":    null,
  "action":      "create",
  "existing":    null,
  "parent":      null,
  "environment": {"id": "env_01J9...", "name": "default", "isolation": "container", "capabilities": {...}},
  "set":         null,
  "manifest":    <the defaulted object>,
  "request":     {"id": "req_01J9..."}
}
```

Response, 200:

```json
{
  "allow": true,
  "manifest": <the object to continue with; absent means unchanged>,
  "reason": "",
  "warnings": ["..."]
}
```

Rules:

- The returned `manifest` replaces the input and goes through stage 1
  again, so a webhook that returns an unknown field or a bad quantity
  yields `invalid_field` naming the path, not a crash and not a pass.
- The webhook may not change `apiVersion`, `kind`, `status`, or, on
  update, `metadata.name` or any field [[003-manifest-contract]] marks
  immutable: stage 5 compares the admission output against `existing`,
  so a catalog pins `image` to a digest at create and on update echoes
  `existing.spec.image`. A change to a forbidden field is
  `admission_refused` with the path in the developer detail.
- Stage 3 runs before stage 6, so a webhook that widens a spawned
  child's boundary is refused by `boundary_exceeded`; admission cannot
  open a boundary.
- `warnings` are sentences in the user register, at most 8 of at most
  256 characters each, appended to the resolved manifest's. A response
  carries no `limits`; those are the authorizer's ([[006-identity]]).
  The response body is capped at 1 MiB.
- `allow: false` is `admission_refused` with the `reason` as the
  developer detail.
- Everything that is not a parsed 200 with `allow` present is
  `admission_unavailable`, 503, and never a pass: connection refused, a
  TLS failure, a non-200 status, a body that does not parse, a body
  without `allow`, a body over the cap, and a timeout of
  `CELLA_ADMISSION_TIMEOUT` (default `3s`). There is no retry: admission
  may rewrite the manifest, so a resend could apply a mutation twice,
  where the authorizer of [[006-identity]] is a pure read and retries
  once on a pre-response connection failure. An
  `http://` URL is refused at start unless it is on a loopback
  address; `CELLA_ADMISSION_URL` without `CELLA_ADMISSION_TOKEN` is a
  start-up failure.

### The count ceiling

`CELLA_MAX_SANDBOXES_PER_SUBJECT` (default `0`, no ceiling), or the
authorizer's `limits.max_sandboxes` when present, bounds how many
sandboxes one subject holds. `Resolve` has no count, so the check is
the API handler's, taken in the same store transaction as the
desired-state write ([[010-state]]) so two concurrent creates cannot
both pass at the ceiling: the count is every desired `Sandbox` of the
subject whose phase is not `Deleting`, `Queued` and `Stopped` included.
For a workload creating a child, the subject counted is the root's
owner, so a spawn tree never escapes its owner's ceiling. The refusal
is `quota_exceeded`, 422; [[008-api]] carries its sentence.

### What a platform does here

An image catalog: rewrite `spec.image: python` to a pinned digest at
create. A plan: set `resources` and `lifecycle` from the subject's
plan and refuse what exceeds it. A sidecar: add an annotation its
driver decorator reads. A brokered credential: add a `secrets[]` entry
naming a `Secret` the platform wrote through the API, whose scope then
joins the allow list ([[018-egress-and-secrets]]). None of these is the
control plane's.

## Not in this spec

The decorators the annotations drive ([[004-runtime-contract]]); the
stub admission endpoint's flags ([[012-test-stubs-and-tiers]]), which
are `-rewrite image=<ref>`, `-refuse`, `-warn <text>`, and `-fail-mode
timeout|malformed|status:<code>|no-allow|unknown-field|change-kind|oversize`
so every rule above is drivable.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A nil `AdmitFunc` is the identity; `Defaults` and `Ceilings` apply with and without a webhook | `TestDefaultsAndCeilingsApplyRegardless` | not built |
| The environment's `defaultQueue` wins over `CELLA_DEFAULT_*`; a caller's value wins over both | `TestDefaultsPrecedence` | not built |
| With no webhook, `Admit` returns its input unchanged | `TestBuiltInAdmitIsIdentity` | not built |
| The webhook receives every field of the request shape, `workload` and `parent` set for a spawn, `set` for a replica | `TestAdmissionRequestShape` against the stub | not built |
| The webhook's returned manifest is what the driver gets; an absent `manifest` leaves the input unchanged; warnings land in `status.warnings` | `TestWebhookOutputIsWhatRuns` | not built |
| A webhook that returns an unknown field is `invalid_field`; one that changes `kind`, or `image` on update, is `admission_refused` naming the path; one that pins `image` at create passes | `TestWebhookOutputIsValidated` with `-fail-mode unknown-field`, `change-kind`, `-rewrite` | not built |
| A webhook that widens a child's boundary is `boundary_exceeded` | `TestAdmissionCannotOpenABoundary` | not built |
| Each of the seven failure modes is `admission_unavailable` and never a pass; nothing is retried | `TestAdmissionFailsClosed`, table-driven over the stub's `-fail-mode` values | not built |
| A non-loopback `http://` URL and a URL without a token are start-up failures | `TestAdmissionStartupRules` | not built |
| A webhook cannot raise a value above `CELLA_MAX_*` | `TestCeilingsAreAFloorOnStrictness` | not built |
| The count ceiling refuses the (n+1)th sandbox with `quota_exceeded`, counts `Queued` and `Stopped`, admits after a delete, and holds under two concurrent creates at the ceiling; a spawned child counts against the root's owner | `TestCountCeiling`, `TestCountCeilingIsAtomic`, `TestSpawnCountsAgainstTheRoot` | not built |
| A set of eight replicas produces eight admission calls, each with its index | `TestSetReplicasAreAdmittedEach` | not built |
