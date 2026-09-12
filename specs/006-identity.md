---
title: "Identity: OIDC issuers, workload and environment tokens, the authorizer webhook, the owner policy"
status: validated
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
affects: [internal/auth/, internal/config/, internal/api/, test/stubs/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Identity

## Overview

`cellad` knows who is calling and asks someone else what they may do.
Who: a bearer token from any OpenID Connect issuer the operator lists,
verified against the issuer's discovery document and key set, with no
issuer-specific claim read beyond the standard ones. What: one `POST`
per request to an authorizer endpoint the operator writes, with a
built-in owner policy when none is configured. The only tokens
`cellad` mints identify sandboxes and environments: a process inside a
sandbox calls back with an identity that dies with it, and a worker on
a self-hosted data plane registers with one an operator can revoke.

## Current state

Not built. The design replaces a shared identity type aliased into an
authorization vocabulary with a subject string and an HTTP decision,
so that a hosted platform, a company's own issuer, and a laptop's stub
issuer are one code path.

## Design

### Subjects

A subject is the pair of the issuer and the `sub` claim, rendered as
one string everywhere it is stored or sent: `<iss>|<sub>` for a token
from a listed issuer, and the bare `sub` for a token `cellad` minted,
whose `sub` carries a reserved prefix, `sandbox:sbx_...` or
`environment:env_...`. A listed issuer's token whose `sub` starts with
a reserved prefix is refused with `unauthenticated`, so no issuer can
mint a sandbox's or an environment's identity. `owner` fields, event
`subject` fields, `CELLA_ADMIN_SUBJECTS` entries, and the authorizer
request all carry the rendered string; the authorizer request also
carries `issuer` and `sub` apart.

### Caller identity

`CELLA_OIDC_ISSUERS` lists issuer URLs. At start `cellad` fetches each
`/.well-known/openid-configuration` and its `jwks_uri` and refuses to
start when any is unreachable or lists no `RS256` key; afterwards it
verifies through `latere.ai/x/pkg/authkit/jwt`, which caches a key set
for its TTL, refreshes it on an unknown `kid` under that package's
rate limit, and serves the stale set while a refresh fails, so an
issuer that goes away later degrades to refusing new keys rather than
every request. A request's bearer is accepted when it is an `RS256`
JWS signed by a listed issuer's key, `iss` matches, `aud` contains
`CELLA_OIDC_AUDIENCE` (default `cella`), `exp` is in the future, and
`nbf` if present is past. `email`, `name`, and `groups` are read when
present and handed to the authorizer verbatim, never interpreted by
the control plane. An `http://` issuer is refused unless it is on a
loopback address or in `CELLA_OIDC_INSECURE_ISSUERS`.

A request without a bearer is `unauthenticated`, 401. There is no
anonymous access and no API key; a caller that wants a long-lived
credential gets one from its issuer.

### Tokens cellad mints

Both are `RS256` JWS signed with the first key of `CELLA_TOKEN_KEY`, a
PEM value holding one or two RSA private keys of at least 2048 bits.
The first block signs; every block's public key is in the key set at
`/.well-known/jwks.json`; `kid` is the RFC 7638 JWK thumbprint,
SHA-256, base64url. Rotation is the operator prepending a new key and,
after every token signed by the old one has expired, removing the old
block; the overlap is therefore in the operator's hands and survives a
restart because it is in the configuration. A `CELLA_TOKEN_KEY` with
no valid RSA block is a start-up failure.

| Claim | Workload token | Environment key |
|---|---|---|
| `iss` | `CELLA_PUBLIC_URL` | `CELLA_PUBLIC_URL` |
| `sub` | `sandbox:<sbx_ id>` | `environment:<env_ id>` |
| `aud` | `[CELLA_OIDC_AUDIENCE]` | `[CELLA_OIDC_AUDIENCE]` |
| `exp` | the sandbox's `expiresAt` or 24 hours from mint, whichever is sooner | `CELLA_ENVIRONMENT_KEY_TTL` (default `8760h`) from mint |
| `jti` | a ULID; the key of a revocation | a ULID; the key of a revocation and of `DELETE /v1/environments/{id}/keys/{jti}` |
| `environment` | the `env_` id the sandbox runs in | absent |
| `spawn` | `{budget, depth, mesh}` from desired state at mint, a copy the control plane never trusts over the store ([[022-mesh-and-spawn]]) | absent |

Both tokens verify with any conforming JWT library against the key
set, which is what lets a platform or a third service trust a
sandbox's or a worker's identity without asking `cellad`. The egress
gateway does not verify tokens: it authenticates a sandbox by a
credential the control plane mints into its map, which never rotates
under a running process ([[018-egress-and-secrets]]).

Workload token lifecycle: minted by the controller at create and again
at recovery ([[005-lifecycle-controller]]), projected by the driver at
`/run/cella/token` ([[004-runtime-contract]]), re-minted and
re-projected through `Change.Token` when two thirds of its lifetime
has passed, the previous `jti` revoked at the same moment. `POST
/v1/sandboxes/{id}/token` mints one more for a process outside the
projection, with the same `exp` rule, and revokes nothing. `cellad`
verifies its own tokens as it verifies an issuer's, with two more
checks: the `jti` is not in the revocation list, and the sandbox's
desired state exists with a phase other than `Deleting`
([[010-state]]); a deleted sandbox's token is therefore refused by
`cellad` at once. The gateway verifies offline and cannot see a
delete: its bound is the token's `exp`. The gateway's own bound is the
purge on its sync stream ([[018-egress-and-secrets]]), which removes
the sandbox's map and with it the credential the gateway accepts.
Without a durable store, desired state dies with the process and every
workload token is refused after a restart, which is the same restart
that reaps the sandboxes ([[010-state]]).

Environment keys: `POST /v1/environments/{id}/keys` mints one, shown
once, and an environment may hold several so that each worker on it
carries its own; `DELETE .../keys/{jti}` revokes one. A key authorizes
registration, claiming, and reporting for its environment and is
refused on every other route.

### The authorizer

```
POST {CELLA_AUTHORIZER_URL}
Authorization: Bearer {CELLA_AUTHORIZER_TOKEN}
Content-Type: application/json

{
  "subject":  "https://login.example.com|alice",
  "issuer":   "https://login.example.com",
  "sub":      "alice",
  "claims":   {"email": "alice@example.com", "name": "Alice", "groups": ["research"]},
  "workload": null,
  "action":   "sandbox.exec",
  "resource": {"kind": "Sandbox", "id": "sbx_01J9...", "name": "dev", "owner": "https://login.example.com|alice",
               "environment": "env_01J9...", "parent": "", "root": "sbx_01J9...", "labels": {}},
  "request":  {"id": "req_01J9...", "ip": "203.0.113.4", "user_agent": "cella/0.1"}
}
```

`workload`, when the caller is a sandbox, is `{"id", "parent", "root",
"environment", "mesh", "spawn": {"budget", "used", "depth"}}` and
`subject` is `sandbox:sbx_...`. `resource` per action:

| Action | `resource` |
|---|---|
| `sandbox.create` | `{"kind": "Sandbox", "name", "environment", "parent", "labels"}` from the manifest; no `id` yet |
| `sandbox.read`, `.update`, `.delete`, `.exec`, `.token` | the sandbox as above |
| `sandbox.list` | `{"kind": "Sandbox"}`; the response may carry `filter` |
| `secret.create`, `.read`, `.update`, `.delete`, `.list`, `.mount` | `{"kind": "Secret", "id", "name", "owner", "labels"}`; `mount` is asked at resolve for every secret a manifest names ([[018-egress-and-secrets]]) |
| `volume.create`, `.read`, `.update`, `.delete`, `.list`, `.attach`, `.snapshot` | `{"kind": "Volume", "id", "name", "owner", "environment", "labels"}`; `attach` asked at resolve ([[019-volumes]]) |
| `set.create`, `.read`, `.update`, `.delete`, `.list` | `{"kind": "SandboxSet", "id", "name", "owner", "environment", "labels"}` ([[020-scheduling-and-sets]]) |
| `environment.create`, `.read`, `.update`, `.delete`, `.list`, `.key`, `.use` | `{"kind": "Environment", "id", "name", "owner", "isolation", "labels"}`; `use` asked at resolve for the environment a manifest names ([[021-data-plane-workers]]) |

Response, 200:

```json
{
  "allow": true,
  "reason": "",
  "limits": {"requests_per_minute": 1200, "max_sandboxes": 10, "max_priority": 5},
  "filter": {"owners": ["https://login.example.com|alice"], "labels": {"team": "research"}}
}
```

`limits` is optional and every field in it is optional: an absent
field means the configured value. `requests_per_minute` overrides
`CELLA_REQUESTS_PER_MINUTE` for this subject ([[008-api]]);
`max_sandboxes` overrides `CELLA_MAX_SANDBOXES_PER_SUBJECT`
([[007-admission]]); `max_priority` caps `scheduling.priority` and
reaches `Resolve` as `Limits.MaxPriority` ([[003-manifest-contract]]).
`filter`, on `sandbox.list` only, narrows the list to the owners and
labels named.

Rules:

- A decision is any of the three actions' outcomes. Everything else is
  `authorizer_unavailable`, 503, and never an allow: connection
  refused, a TLS failure, a non-200 status, a body that does not
  parse, a body without `allow`, and a timeout of
  `CELLA_AUTHORIZER_TIMEOUT` (default `3s`). There is no retry. An
  `http://` authorizer URL is refused at start unless it is on a
  loopback address. `CELLA_AUTHORIZER_URL` without
  `CELLA_AUTHORIZER_TOKEN` is a start-up failure. Availability is not
  a readiness check: a flapping endpoint fails requests, not replicas.
- A `deny` on a request's own action is `forbidden`, 403, with the
  authorizer's `reason` as the developer detail and never in the user
  sentence. A `deny` on `secret.mount`, `volume.attach`, or
  `environment.use`, asked through `Lookup` at resolve, is `not_found`,
  so a refused object and a missing one are the same answer
  ([[003-manifest-contract]], [[013-security-and-threat-model]]).
- The API constructs `Lookup` per request from the caller's subject and
  the cache below; an importer constructs its own.
- Decisions are cached per replica for `CELLA_AUTHORIZER_CACHE`
  (default `10s`) under the key of subject, action, and resource id
  (empty for `create` and `list`), with the `limits` and `filter` that
  came with them; allows and denies are cached, unavailability never.
  A revocation at the authorizer therefore takes effect within the
  cache window, which is the accepted cost of an attach's per-message
  checks not each dialing.
- The spawn budget is enforced by the controller's atomic debit
  ([[022-mesh-and-spawn]]) and by nothing else; an authorizer or the
  owner policy may refuse a `sandbox.create` from a workload for its
  own reasons, but neither reads the ledger and neither is the budget.

### The owner policy

With `CELLA_AUTHORIZER_URL` unset, `CELLA_ADMIN_SUBJECTS` is read and
the log says `owner policy` at start:

- a subject may `create` any kind but `Environment`, and may `read`,
  `update`, `delete`, `exec`, `token`, `mount`, `attach`, and
  `snapshot` an object whose `owner` is that subject;
- `list` returns the subject's own objects;
- every subject may `use` the default environment, the one
  `CELLA_DEFAULT_ENVIRONMENT` names ([[021-data-plane-workers]]);
  every other environment is admins' only;
- a subject in `CELLA_ADMIN_SUBJECTS`, matched on the rendered subject
  string, may do all of the above on every object and may create, key,
  update, and delete environments;
- a sandbox may `read` and `exec` itself, may `read` any sandbox whose
  `parent` chain reaches it (decided from `resource.parent` and
  `resource.root`, both in the request), may `create` a child, and
  nothing else; its `list` returns its descendants.

With an authorizer set, `CELLA_ADMIN_SUBJECTS` is read and unused.
Every kind carries an `owner`, the rendered subject that applied it,
including `Environment` ([[021-data-plane-workers]]). This is a policy
with tests, not the absence of one.

### What the control plane never does

It never issues a token to a person, never stores a password, never
reads a group claim to decide anything, never calls an issuer for
anything but discovery and keys, and never holds a session. A dashboard
that needs sessions is a platform's.

## Not in this spec

The stub issuer and authorizer ([[012-test-stubs-and-tiers]]); the
threat model that ranks these controls ([[013-security-and-threat-model]]);
the HTTP envelope of 401 and 403 ([[008-api]]); the revocation store
([[010-state]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A token from a listed issuer with the right audience is accepted; wrong issuer, wrong audience, expired, unsigned, non-RS256, and a `sub` with a reserved prefix are each `unauthenticated` | `TestVerifierRefusals`, table-driven | not built |
| The same `sub` from two listed issuers is two subjects; an admin entry matches one and not the other | `TestSubjectsAreIssuerQualified` | not built |
| An unreachable issuer at start is a start-up failure; one that fails later is served from the cached key set | `TestIssuerAtStartAndLater` | not built |
| A non-loopback `http://` issuer or authorizer is refused at start unless listed insecure; an authorizer URL without a token is a start-up failure | `TestInsecureAndIncompleteEndpoints` | not built |
| `cellad` refuses to start with no issuer or with a `CELLA_TOKEN_KEY` holding no RSA key | `TestServeRefusesToStartWithoutIdentity` | not built |
| A workload token verifies with a generic JWT library against `/.well-known/jwks.json`; its `kid` is the RFC 7638 thumbprint | `TestWorkloadTokenIsVerifiable` | not built |
| With two PEM blocks, tokens signed by the second still verify; after the block is removed they do not; the first block signs | `TestKeyRotationByConfiguration` | not built |
| A workload token is refused by `cellad` after its sandbox is deleted and after its `jti` is revoked; a recovered sandbox's new token verifies and the old `jti` is revoked | `TestWorkloadTokenLifecycle` | not built |
| A token is re-minted and re-projected at two thirds of its lifetime | `TestTokenReprojection` under a fake clock | not built |
| An environment key registers and claims for its environment, is refused on every other route, and is refused at once after `DELETE .../keys/{jti}`; two keys on one environment work independently | `TestEnvironmentKeys` | not built |
| Every failure mode in the rules list is `authorizer_unavailable` and never an allow; nothing is retried | `TestAuthorizerFailsClosed`, table-driven over seven modes | not built |
| Every action in the table reaches the authorizer with the resource shape in its row, `workload` set for a sandbox caller, and `issuer` and `sub` apart | `TestAuthorizerRequestShapes` against the stub | not built |
| A deny on an own action is `forbidden`; a deny through `Lookup` is `not_found` and identical to a missing object | `TestDenyMapping` | not built |
| The cache serves a second identical decision without a call, expires at the window, never caches unavailability, and keys `create` and `list` without a resource id | `TestDecisionCache` | not built |
| `limits` override the rate limit, the count ceiling, and the priority cap; `filter` narrows a list | `TestLimitsAndFilter` | not built |
| The owner policy's rules hold for every kind and action, including that only an admin creates an environment and only the default environment is usable by a non-admin | `TestOwnerPolicy`, table-driven | not built |
| A sandbox's token reads and execs itself, reads its descendants, cannot read a sibling or delete itself, and cannot mount a secret its parent did not | `TestWorkloadIsLeastPrivileged` | not built |
