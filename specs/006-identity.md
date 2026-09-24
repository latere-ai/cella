---
title: "Identity: OIDC issuers, workload and environment tokens, the authorizer webhook, the owner policy"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
affects: [authorizer/, internal/auth/, internal/config/, internal/api/, test/stubs/]
effort: medium
created: 2026-09-12
updated: 2026-09-24
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

Not built, apart from the vocabulary package the 2026-09-16 amendment
below names. The design replaces a shared identity type aliased into an
authorization vocabulary with a subject string and an HTTP decision,
so that a hosted platform, a company's own issuer, and a laptop's stub
issuer are one code path.

Amended on 2026-09-13 by the decision "one platform over open cores":
the claims forwarded, the cache and retry rules, the verifier, the
subject string and the probe id are one contract shared by the three
open cores, Cella, Lux and Origo, so one authorizer serves all three.

Amended on 2026-09-16 by the identity leaf "one authorizer library",
which names the library and the vocabulary's home. The library is
`latere.ai/x/pkg/authz` at v0.70.0: the envelope, the client with its
cache and retry, the owner policy's frame, `authz.Vocabulary`, the stub
of `authz/stub`, the conformance suite of `authz/conformance`, and the
endpoint scaffold of `authz/server`. The vocabulary's home is
`cella/authorizer`, an importable package at this module's root beside
`lux/authorizer`: the thirty-two actions of the resource table below,
each a constant with the resource kind it acts on, `Vocabulary()` as
`authz.Vocabulary`, and `WireLimits`/`DecodeLimits` over the three
figures of the `limits` object. It is built, and its proofs are that the
table matches this spec's, that the shared stub told the vocabulary
passes `conformance.Run` driven from it, and that an endpoint written on
`authz/server` with the owner policy behind it passes the same run.

Amended on 2026-09-17 by the maintainer's decision on the family's age
bound: every core verifies an issuer-minted token against the same
bound, the `MaxTokenAge` default of `latere.ai/x/pkg/authkit/jwt`, 24
hours. A caller's token whose `iat` is older than that is refused
whatever `exp` it carries, so the list below of what refuses a caller's
token gains the age; Origo names the same figure and Lux inherits it.
A token that stamps no `iat` is refused for the same reason, because an
age no one can read is not an age within the bound.
Environment keys are not bounded by age, because one lives
`CELLA_ENVIRONMENT_KEY_TTL`, a year by default, and its `exp` is its
bound. Nothing else in this spec changes.

Amended on 2026-09-17 by the identity leaf "PAT scopes"
(infrastructure/identity id-13), which narrows a credential below the
person who holds it. A personal access token carries the grants its
holder chose as RFC 9396's `authorization_details`, one entry per pair
of an action and a resource selector, and an action is named from a
published vocabulary: `cella:sandbox.read` is one of Cella's. Both
halves below are within this spec's published design rather than beside
it. The decision point is still the seam this spec draws, the envelope
is still every claim of the verified token verbatim, and the vocabulary
is still the table below.

**The door reads the claim.** `jwt.Config.ReadsGrants` is a service's
promise that the restriction is applied somewhere, and a validator with
it false refuses a token carrying grants with `grants_unread`: a claim
that says what a credential may *not* do widens that credential when it
is ignored, and a refusal is visible where an ignored restriction is
not. `cellad` sets it on the validator for the listed issuers, which is
the one that sees a person's token. The validator for the tokens
`cellad` mints is left alone: no token `cellad` signs carries a
`token_use`, so none of them is a personal access token and there is no
grant on that path to read.

**The decision point applies it.** The answer is the decision point's
own decision and the grants, intersected:

```
allow(req) = decide(req) AND ( token_use(req) != pat
                               OR EXISTS g in G(req) : covers(g, req) )

covers(g, req) = "cella:" + req.action in g.actions
                 AND ( g.identifier = "" OR g.identifier = req.resource.id )
```

The conjunction turns an allow into a deny and never a deny into an
allow. The role is the ceiling, and a grant is a restriction and never
authority: a grant naming a sandbox the person does not own still
reaches nothing, because `decide` answers first. `authz.Restrict` is
that function, written once in `latere.ai/x/pkg/authz`, and `cellad`
takes it rather than writing a second. An operator's endpoint on
`authz/server` applies it to whatever its `Decider` returned, with no
option to switch it off. The owner policy runs in this process with no
endpoint in front of it, so the policy calls `Restrict` on its own
decision, over the claims the envelope already carried verbatim: a
request built with empty claims would restrict nothing, which is why
`Envelope` forwards the token as it came and a test pins that it does.

The deny's reason is `grant`, which a caller reads as the developer
detail of the 403 a refused action answers, or of the 404 a refused
lookup answers. A personal access token carrying no grant at all is
denied too: the claim is a restriction, so an absent one is not full
authority, and auth writes a grant list onto every key rather than
leaving one empty.

`authz/conformance` drives the rule as case A13 under `WithVocabulary`,
and this repository runs it twice, for a reason the scaffold makes
necessary. `authz/server` narrows whatever its decider returned, so a
suite driven through the scaffold passes whether or not the policy
narrows anything; the second run serves the owner policy over the
contract's wire and the bearer alone, and reads the policy's own
answer. That is the run that holds the promise `ReadsGrants` makes.

`Vocabulary()` also carries a heading per resource kind now, read
through `Vocabulary.Label`: Sandboxes, Secrets, Volumes, Sandbox sets,
Environments. A key's grant picker groups by kind, because an action
acts on exactly one kind and `set.*` is why the grouping is the kind
and not the action's prefix, and it names each group from this table
rather than from a word of its own. `SandboxSet` is a type name and not
a heading.

One thing here is pkg v0.75.0's and not id-13's. `jwt.Validator.Warm`
reads every configured issuer's key set once. `cellad` already reads
each one at start and refuses to start when one does not answer, but
the validator holds its own cache and that check filled none of it, so
the first request a node ever served paid for a discovery document and
a key set on the request path. The cache is warmed at start now,
through the same client, and a set that does not answer there is
refused the way the start-up check's is: one rule, and at start every
issuer answers or `cellad` does not start.

Nothing else in this spec changes. The resource table, the subjects,
the tokens `cellad` mints and the owner policy's own rows are
unchanged: the grants narrow the answer the policy gives and rewrite
none of it.

Built on 2026-09-17, on pkg v0.72.0: the `CELLA_*` configuration, the
verifier over the issuers, the signer and the key set at
`/.well-known/jwks.json`, the client and the guard that ask the
endpoint, the owner policy's own rows, and the node wiring that brings
the three up before the listeners. `CELLA_OIDC_ISSUERS` is
`jwt.Config.Issuers` and `CELLA_TOKEN_KEY` is `jwt.Config.LocalKeys`,
both of which pkg added for this spec; the cache, the retry and the
failure rules are `authz.Client`'s and nothing here restates them. The
gate's `verifier` waiver is gone: `go tool lateregate identity` prints
`PASS verifier` and `PASS authorizer` with no waiver, and `cellad`
reaches the OpenTelemetry SDK, which spec 001 admits and the gate's
`depcheck` rows now record.

The conformance tier runs `authkit/conformance` against the
authenticator `cellad` runs and `authz/conformance` against both the
stub of `authz/stub` and the owner policy served through
`authz/server`; `CELLA_TEST_AUTHORIZER_URL` and
`CELLA_TEST_AUTHORIZER_TOKEN` point the second half at a deployed
endpoint, which is what a release run sets. Those two names are the
tiers' and belong in [[012-test-stubs-and-tiers]]'s table.

Completed on 2026-09-20 by [[045-workload-tokens]]: the controller mints
a workload token at create and at recovery, every driver projects it,
the reaper re-mints and re-projects it at two thirds of its lifetime,
and the revocation list of [[010-state]] is written at a rotation, a
recovery and a delete and read by the verifier on every token `cellad`
signed. The sandbox phase is not read on that path and needs no read: a
delete revokes, so a deleted sandbox's token is refused at once by the
list. A token `cellad` minted that carries no `jti` is refused, because
a credential that cannot be revoked is not one this control plane
issued.

Amended on 2026-09-24 by [[067-environment-list-ports-redirect-keys]]:
the `filter` of a list decision narrows every list, not the sandbox list
alone. Each list holds every row to the filter's owners and labels and
then asks the kind's read on each row the filter admits, leaving out a
row the read refuses; a read that produced no decision refuses the whole
list. The default environment is the one row the filter does not narrow:
every subject may use it, it carries no caller's owner or labels, so no
filter over owners and labels names it without naming what the filter
hides, and it is listed whenever `environment.read` on it is allowed. An
authorizer that narrows `environment.list` to a tenant keeps the default
visible by allowing that read, and hides it by denying it, which hides it
from the read by id as well. `environment.key` also authorizes `GET
/v1/environments/{id}/keys`, the listing of an environment's keys, which
the control plane now records at mint with the subject that minted them.

What is left of this spec waits on other specs rather than on a
decision, and the acceptance table says which per row: the
routes an environment key authorizes ([[008-api]],
[[021-data-plane-workers]]), the ceilings an allow overrides
([[007-admission]], [[008-api]], [[003-manifest-contract]]), the
`cellad check` subcommand ([[014-release-and-installation]]), and the
stub of [[012-test-stubs-and-tiers]]. The resource builders are
`internal/auth`'s for now rather than the vocabulary package's, because
the fields of an object are [[003-manifest-contract]]'s types and the
published package promises what an endpoint imports: the action and its
kind.

One note the resource table below does not carry. Cella's list answer is
not a page of its own: it is a decision, an allow whose optional
`filter` narrows the page to owners and labels, as the response below
shows. On pkg v0.70.0 the scaffold of `authz/server` routed every action
whose name ends in `.list` to a `Lister`, which is a page; v0.70.1
corrected that, and an endpoint now names the actions whose answer is a
page in `server.Options.PageActions`. Cella names none, so one `Decider`
answers all thirty-two rows and writes the filter itself. `authz.IsList`
reads the verb and routes nothing.

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
start when any is unreachable or lists no `RS256` or `ES256` key;
afterwards it
verifies through `latere.ai/x/pkg/authkit/jwt`, which caches a key set
for its TTL, refreshes it on an unknown `kid` under that package's
rate limit, and serves the stale set while a refresh fails, so an
issuer that goes away later degrades to refusing new keys rather than
every request. A request's bearer is accepted when it is a JWS signed
`RS256` or `ES256` by a listed issuer's key, `iss` matches, `aud` contains
`CELLA_OIDC_AUDIENCE` (default `cella`), `exp` is in the future,
`nbf` if present is past, and `iat` is present and less than 24 hours
old (amended 2026-09-17). Every claim of the verified token is handed
to the authorizer verbatim in `claims`, and none is interpreted by the
control plane: an issuer's organization, role, or group claims mean
something to the authorizer that reads them and nothing to `cellad`.
An `http://` issuer is refused unless it is on a
loopback address or in `CELLA_OIDC_INSECURE_ISSUERS`.

A request without a bearer is `unauthenticated`, 401. There is no
anonymous access and no API key; a caller that wants a long-lived
credential keeps one at its issuer and re-mints it, because a bearer
`cellad` accepts is at most a day old (amended 2026-09-17).

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

`CELLA_PUBLIC_URL` may carry a path, the base the control plane is
served under ([[071-serving-under-a-base-path]]). The `iss` is the URL
with its path, the key set is served at the `iss` followed by
`/.well-known/jwks.json`, and the verifier's local issuer is the same
value, so a token minted under a base verifies with no rule of its own.
A change of `CELLA_PUBLIC_URL`, a move under a base included, changes
the issuer and refuses every token minted under the old one.

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
carries its own; `GET .../keys` lists them, never the token, and
`DELETE .../keys/{jti}` revokes one. A key authorizes
registration, claiming, and reporting for its environment and is
refused on every other route.

### The authorizer

```http
POST {CELLA_AUTHORIZER_URL}
Authorization: Bearer {CELLA_AUTHORIZER_TOKEN}
Content-Type: application/json

{
  "subject":  "https://login.example.com|alice",
  "issuer":   "https://login.example.com",
  "sub":      "alice",
  "claims":   {"email": "alice@example.com", "name": "Alice", "groups": ["research"], "org_id": "…", "roles": ["owner"]},
  "workload": null,
  "action":   "sandbox.exec",
  "resource": {"kind": "Sandbox", "id": "sbx_01J9...", "name": "dev", "owner": "https://login.example.com|alice",
               "environment": "env_01J9...", "parent": "", "root": "sbx_01J9...", "labels": {}},
  "request":  {"id": "req_01J9...", "ip": "203.0.113.4", "user_agent": "cella/0.1"}
}
```

`claims` is every claim of the token; the example shows three an
issuer commonly stamps beside two a platform's issuer adds.
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
  "ttl": 60,
  "limits": {"requests_per_minute": 1200, "max_sandboxes": 10, "max_priority": 5},
  "filter": {"owners": ["https://login.example.com|alice"], "labels": {"team": "research"}}
}
```

`ttl` is optional, the seconds this allow may be cached, capped at
`600`; an absent `ttl` means the default `CELLA_AUTHORIZER_CACHE`, and
an explicit `0` means this allow is not cached and every call
revalidates. `limits` is optional and
every field in it is optional: an absent field means the configured
value. `requests_per_minute` overrides
`CELLA_REQUESTS_PER_MINUTE` for this subject ([[008-api]]);
`max_sandboxes` overrides `CELLA_MAX_SANDBOXES_PER_SUBJECT`, and it means
the count [[007-admission]] defines: every desired `Sandbox` of the
subject whose phase is not `Deleting`, a queued and a stopped one
included, because each holds a name and a workspace;
`max_priority` caps `scheduling.priority` and
reaches `Resolve` as `Limits.MaxPriority` ([[003-manifest-contract]]).
`filter`, on a list action, narrows the list to the owners and labels
named, and each row it admits is then decided by the kind's read; the
default environment is decided by its `environment.read` alone (amended
2026-09-24).

Rules:

- A decision is an allow or a deny. Everything else is
  `authorizer_unavailable`, 503, and never an allow: connection
  refused, a TLS failure, a non-200 status, a body that does not
  parse, a body without `allow`, and a timeout of
  `CELLA_AUTHORIZER_TIMEOUT` (default `5s`), which bounds each attempt,
  so the retry below makes the worst case twice that. The call is retried once
  when the connection failed before a response line arrived, a refused
  or reset connection or a dial timeout, and never on a non-200, a
  timeout after the request was sent, or a body that does not parse. An
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
- An allow is cached per replica for the answer's `ttl`,
  `CELLA_AUTHORIZER_CACHE` (default `60s`) when the answer names none,
  capped at `600s`; a deny for `5s`; unavailability never; under the
  key of subject, action, and resource id (empty for `create` and
  `list`), with the `limits` and `filter` that came with them. A
  revocation at the authorizer therefore takes effect within the
  allow's `ttl`, which the authorizer chooses, and which is the
  accepted cost of an attach's per-message checks not each dialing.
- The reserved probe id is the shared contract's, `authz.ProbeID`, the
  one string every core in the family sends: every authorizer denies it
  for every subject and every action, and `cellad check`
  ([[014-release-and-installation]]) sends it and reads an allow as an
  endpoint that does not read the request. The owner policy denies it
  too. Corrected on 2026-09-17: this spec named a Cella-shaped id,
  `sbx_00000000000000000000000000`, which nothing in the shared contract
  knows. One probe id is what lets one check command read one answer
  from an endpoint that serves several cores, so the id is the
  package's, not a product's.
- The envelope, the client, the cache, the retry, the owner policy's
  frame, the stub authorizer, and the conformance test an authorizer
  passes are `latere.ai/x/pkg/authz`, shared with the sibling open
  cores; `cellad` adds its action vocabulary and its `resource` shapes
  and nothing else.
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
  nothing else; its `list` returns its descendants. That last narrowing
  is the API's own, from the `workload` member of the request: the
  contract's `filter` names owners and labels, and a descendant shares
  neither with its root, so the decision is an allow with no filter and
  [[008-api]] pages the tree.

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
| A token from a listed issuer with the right audience is accepted, signed `RS256` or `ES256`; wrong issuer, wrong audience, expired, unsigned, an algorithm outside the two, and a `sub` with a reserved prefix are each `unauthenticated` | `TestVerifierRefusals`, table-driven | built |
| A listed issuer's token whose `iat` is more than 24 hours old is `unauthenticated` with the reason `iat`, whatever `exp` it carries, a token that stamps no `iat` is refused for the same reason, and an environment key 30 days old with `exp` in the future is accepted | `TestTokenAgeIsBoundedForIssuersAndNotForEnvironmentKeys`; the two age rows of `TestVerifierRefusals` | built |
| The same `sub` from two listed issuers is two subjects; an admin entry matches one and not the other | `TestSubjectsAreIssuerQualified` | built |
| An unreachable issuer at start is a start-up failure; one that fails later is served from the cached key set | `TestIssuerAtStartAndLater` | built |
| A non-loopback `http://` issuer or authorizer is refused at start unless listed insecure; an authorizer URL without a token is a start-up failure | `TestInsecureAndIncompleteEndpoints` | built |
| `cellad` refuses to start with no issuer or with a `CELLA_TOKEN_KEY` holding no RSA key | `TestServeRefusesToStartWithoutIdentity` | built |
| A workload token verifies with a generic JWT library against `/.well-known/jwks.json`; its `kid` is the RFC 7638 thumbprint | `TestWorkloadTokenIsVerifiable` | built |
| With two PEM blocks, tokens signed by the second still verify; after the block is removed they do not; the first block signs | `TestKeyRotationByConfiguration` | built |
| A workload token is refused by `cellad` after its sandbox is deleted and after its `jti` is revoked; a recovered sandbox's new token verifies and the old `jti` is revoked | `TestRevokedTokenIsRefused`, `TestRecoveryMintsAndRevokes`, `TestDeleteRevokes`, and the `cellad` run of `TestWorkloadTokenEndToEnd` | built ([[045-workload-tokens]]): the delete revokes, so a deleted sandbox's token is refused by the list and no phase is read |
| A token is re-minted and re-projected at two thirds of its lifetime | `TestTokenReprojection` under a fake clock; the `TokenProjection` case of the conformance suite | built ([[045-workload-tokens]]) |
| An environment key registers and claims for its environment, is refused on every other route, and is refused at once after `DELETE .../keys/{jti}`; two keys on one environment work independently | `TestMintedTokensCarryTheirOwnSubject`, `TestTwoKeysOnOneEnvironmentAreTwoJTIs`, `TestEnvironmentKeyRoutes` | built in part: a key is minted, verified, and read back as its environment's, and two on one environment are two jtis; the routes are [[008-api]]'s and [[021-data-plane-workers]]'s and the revocation list is [[010-state]]'s |
| The keys of an environment are listed under `environment.key` with each key's `jti`, mint time, `exp`, revocation and minter, and never the token | `TestEnvironmentKeyList`, `TestEnvironmentKeysRecordAndList` | built ([[067-environment-list-ports-redirect-keys]]) |
| Every failure mode in the rules list is `authorizer_unavailable` and never an allow, and unavailability fails the request without flipping readiness; a connection failure before a response line is retried once and nothing else is | `TestAuthorizerFailsClosed`, table-driven over six modes; `TestAuthorizerRetriesOnlyBeforeAResponseLine` | built: the six modes, the one retry and the readiness rule hold; the 503 they render as is [[008-api]]'s |
| Every action in the table reaches the authorizer with the resource shape in its row, `workload` set for a sandbox caller, `issuer` and `sub` apart, and every claim of the token in `claims` verbatim | `TestAuthorizerRequestShapes` against the stub | built ([[040-mesh-and-spawn]]): every row's shape holds, and `workload` carries the caller's sandbox with its `parent`, `root`, `mesh` and `spawn` read from the store |
| A deny on an own action is `forbidden`; a deny through `Lookup` is `not_found` and identical to a missing object | `TestDenyMapping` | built: the two codes hold at the guard; the 403 and the 404 they render as are [[008-api]]'s |
| The cache serves a second identical decision without a call, expires an allow at the answer's `ttl` and at the `600s` cap, a deny at `5s`, never caches unavailability, and keys `create` and `list` without a resource id | `TestDecisionCache` | built |
| The probe id is denied by the stub authorizer and by the owner policy for every subject and action, and `cellad check` reports an authorizer that allows it | `TestProbeIdIsAlwaysDenied` | built in part: the stub and the owner policy deny it for every subject and action, and the client reads an allow as a misconfiguration; the `cellad check` subcommand is [[014-release-and-installation]]'s |
| `limits` override the rate limit, the count ceiling, and the priority cap; `filter` narrows a list | `TestLimitsAndFilterReachTheCaller`, `TestCountCeilingCountsEveryDesiredSandbox`, `TestEnvironmentListAppliesTheFilter`, `TestTheDefaultEnvironmentIsListedByItsRead`, `TestSecretListAppliesTheWholeFilter` | partial: `max_sandboxes` is honored at create as the count [[007-admission]] defines, and the filter narrows the sandbox, secret and environment lists by owners and labels, with the default environment decided by its read alone ([[067-environment-list-ports-redirect-keys]]); there is still no rate limit ([[008-api]]) and no `Resolve` option carrying `max_priority` ([[003-manifest-contract]]) to override |
| The owner policy's rules hold for every kind and action, including that only an admin creates an environment and only the default environment is usable by a non-admin | `TestOwnerPolicy`, table-driven | built |
| A sandbox's token reads and execs itself, reads its descendants, cannot read a sibling or delete itself, and cannot mount a secret its parent did not | `TestWorkloadIsLeastPrivileged`; `TestWorkloadReachesItsOwnSandbox` over the served routes | built, and held over the API as well as over the policy ([[045-workload-tokens]]); a sandbox creating a child is [[040-mesh-and-spawn]]'s `TestSpawnOverTheAPI`, and the secret a child's parent mounts is not reachable through the API yet |
| A workload token and an environment key minted under a public URL with a path name the URL with its path as `iss`, and the control plane accepts both under the base it is served at | `TestServingUnderABasePathEndToEnd` | built ([[071-serving-under-a-base-path]]) |
