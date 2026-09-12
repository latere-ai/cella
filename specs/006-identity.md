---
title: "Identity: OIDC issuers, workload and environment tokens, the authorizer webhook, the owner policy"
status: drafted
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

The shape is Origo's, chosen so that a hosted platform, a company's
Keycloak, and a laptop's stub issuer are the same code path.

## Current state

Not built. The hosted plane verified tokens through a shared identity
library with the platform's identity type aliased into its own
authorization vocabulary; that coupling is what this spec removes by
making the subject a string and the decision an HTTP call.

## Design

### Caller identity

`CELLA_OIDC_ISSUERS` lists issuer URLs. At start `cellad` fetches each
`/.well-known/openid-configuration` and its `jwks_uri`, caches keys,
and refreshes on an unknown `kid` at most once a minute per issuer. A
request's bearer is accepted when it is a JWS signed by a listed
issuer's key, `iss` matches, `aud` contains `CELLA_OIDC_AUDIENCE`,
`exp` is in the future, and `nbf` if present is past. The subject is
`sub`; `email`, `name`, and `groups` are read when present and handed
to the authorizer verbatim, never interpreted by the core. An
`http://` issuer is refused unless it is on a loopback address or in
`CELLA_OIDC_INSECURE_ISSUERS`.

A request without a bearer is `unauthenticated`, 401. There is no
anonymous access and no API key; a caller that wants a long-lived
credential gets one from its issuer.

### Workload identity

At create the controller asks for a token for the sandbox:
`iss` = `CELLA_PUBLIC_URL`, `sub` = `sandbox:<id>`, `aud` = the
configured audience, `exp` = the sandbox's `expiresAt` or 24 hours,
whichever is sooner, refreshed by the backend's projection before
expiry; signed with `CELLA_TOKEN_KEY` (ECDSA P-256), `kid` the key's
thumbprint. The token carries `spawn: {budget, depth, mesh}` from the
sandbox's desired state ([[022-mesh-and-spawn]]) and `environment`, as
claims a gateway or a platform reads without a call; the control plane
itself checks the store, since claims are a copy.

An environment token is the same signature with `sub:
environment:<name>`, no `exp`, and a `jti` in the revocation list of
[[010-state]], minted by `POST /v1/environments/{name}/keys` and shown
once ([[021-data-plane-workers]]). A worker presents it on its one
outbound connection; it authorizes registration, claiming, and
reporting for that environment and nothing else. The public key set is served at
`/.well-known/jwks.json`, so a plane or a third service can verify a
sandbox's identity without asking `cellad`. `cellad` verifies its own
tokens the same way it verifies an issuer's, with one more check: the
sandbox `id` is in the index and not `Deleting`. A deleted sandbox's
token is therefore refused everywhere at once. Key rotation: the key
set carries the current key and the previous one for
`CELLA_TOKEN_KEY_OVERLAP` (default `24h`) after a change.

### The authorizer

```
POST {CELLA_AUTHORIZER_URL}
Authorization: Bearer {CELLA_AUTHORIZER_TOKEN}
Content-Type: application/json

{
  "subject":  "alice@example.com",
  "claims":   {"email": "...", "name": "...", "groups": ["..."]},
  "workload": null,
  "action":   "sandbox.create",
  "resource": {"id": "01J9...", "name": "dev", "owner": "alice@example.com", "labels": {}},
  "request":  {"id": "req_...", "ip": "203.0.113.4", "user_agent": "cella/0.1"}
}
```

Response, 200:

```json
{
  "allow": true,
  "reason": "",
  "limits": {"requests_per_minute": 1200, "max_sandboxes": 10}
}
```

`workload` is the sandbox record when the caller is a sandbox, so the
authorizer can grant a sandbox less than its owner. `resource` is
absent on `sandbox.list`. `limits` is optional and overrides the
configured per-subject rate and the built-in count ceiling for this
subject; `max_priority` caps `scheduling.priority`
([[020-scheduling-and-sets]]). Actions:

| Action | On |
|---|---|
| `sandbox.create` | a new sandbox; `resource` carries the resolved name and labels |
| `sandbox.read` | `GET` one, `GET` its events, its logs |
| `sandbox.list` | the collection; the authorizer may return a `filter` of owners or labels the list is narrowed to |
| `sandbox.update` | `PUT`, `start`, `stop` |
| `sandbox.delete` | `DELETE` |
| `sandbox.exec` | `exec`, `attach`, file transfer |
| `sandbox.token` | minting a workload token for a sandbox the caller owns |
| `secret.create`, `secret.read`, `secret.update`, `secret.delete`, `secret.mount` | the `Secret` kind; `mount` is asked at resolve for every secret a manifest names, with the secret as the resource, so sharing is the authorizer's model ([[018-egress-and-secrets]]) |
| `volume.create`, `volume.read`, `volume.update`, `volume.delete`, `volume.attach`, `volume.snapshot` | the `Volume` kind; `attach` asked at resolve ([[019-volumes]]) |
| `set.create`, `set.read`, `set.delete` | the `SandboxSet` kind ([[020-scheduling-and-sets]]) |
| `environment.create`, `environment.read`, `environment.update`, `environment.delete`, `environment.key`, `environment.use` | the `Environment` kind; `use` asked at resolve for the environment a manifest names, so a platform decides who may run where ([[021-data-plane-workers]]) |

Rules: a non-200 response, a body that does not parse, and a timeout
(`CELLA_AUTHORIZER_TIMEOUT`, default `3s`) are `authorizer_unavailable`,
503, and never an allow. A decision is cached for
`CELLA_AUTHORIZER_CACHE` (default `10s`) per subject, action, and
resource id, so an attach's per-message checks do not each dial. A
`deny` is `forbidden`, 403, with the authorizer's `reason` as the
developer detail and never in the user sentence. The authorizer's
availability is a readiness check.

### The owner policy

With `CELLA_AUTHORIZER_URL` unset:

- a subject may `create` any kind, and may `read`, `update`, `delete`,
  `exec`, `token`, `mount`, `attach`, and `snapshot` an object whose
  `owner` is that subject;
- `list` returns the subject's own objects;
- every subject may `use` the default environment; other environments
  are admins' until an authorizer says otherwise;
- a subject in `CELLA_ADMIN_SUBJECTS` may do all of the above on every
  object and may create, key, and delete environments;
- a sandbox may `read` and `exec` itself, may `create` a child while
  its spawn budget allows, and may `read` its descendants; nothing
  else.

This is a policy with tests, not the absence of one, and the log says
`owner policy` at start so an operator knows which is in force.

### What the core never does

It never issues a token to a person, never stores a password, never
reads a group claim to decide anything, never calls an issuer for
anything but discovery and keys, and never holds a session. A dashboard
that needs sessions is a plane's.

## Not in this spec

The stub issuer and authorizer ([[012-test-stubs-and-tiers]]); the
threat model that ranks these controls ([[013-security-and-threat-model]]);
the HTTP envelope of 401 and 403 ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A token from a listed issuer with the right audience is accepted; wrong issuer, wrong audience, expired, and unsigned are each refused with `unauthenticated` | `TestVerifierRefusals`, table-driven | not built |
| An unknown `kid` triggers one key refresh and no more than one per minute | `TestKeyRefreshIsRateLimited` | not built |
| A non-loopback `http://` issuer is refused at start unless listed insecure | `TestInsecureIssuersMustBeListed` | not built |
| `cellad` refuses to start with no issuer or no token key | `TestServeRefusesToStartWithoutAnIssuer`, `TestServeRefusesToStartWithoutATokenKey` | not built |
| A workload token verifies against `/.well-known/jwks.json` with a generic JWT library and is refused after the sandbox is deleted | `TestWorkloadTokenLifecycle` | not built |
| After rotation, tokens signed by the previous key verify for the overlap and not after | `TestKeyRotationOverlap` | not built |
| Authorizer down, timeout, 500, and a malformed body are each `authorizer_unavailable` and never an allow | `TestAuthorizerFailsClosed` | not built |
| The authorizer receives every field of the request shape above for every action | `TestAuthorizerRequestShape` against the stub | not built |
| The owner policy's rules hold for every kind and a non-owner is `forbidden` | `TestOwnerPolicy`, table-driven over kinds and actions | not built |
| A sandbox's token cannot delete the sandbox, read a sibling, or mount a secret its parent did not | `TestWorkloadIsLeastPrivileged` | not built |
| An environment token registers and claims for its environment and is refused for any other route; a revoked one is refused at once | `TestEnvironmentToken` | not built |
| Every action in the table reaches the authorizer with the object as resource, including `secret.mount`, `volume.attach`, and `environment.use` at resolve | `TestAuthorizerRequestShape` against the stub | not built |
