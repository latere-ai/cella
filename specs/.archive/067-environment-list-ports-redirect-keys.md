---
title: "The environment list applies the authorizer's filter, a port path without its slash redirects relatively, and an environment's keys are listed"
status: complete
track: core
depends_on:
  - specs/006-identity.md
  - specs/008-api.md
  - specs/010-state.md
  - specs/021-data-plane-workers.md
  - specs/023-computer-use-operations.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/051-environments-and-workers.md
  - specs/.archive/054-environments-desired-state.md
  - specs/.archive/060-dial-and-port-proxy.md
  - specs/.archive/066-events-follow.md
affects: [internal/api/, internal/auth/, internal/store/, cmd/cellad/, api/openapi.yaml, docs/, CHANGELOG.md]
effort: medium
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# The environment list, the port redirect, and the key list

## Overview

Three defects on the environment and port routes.

`GET /v1/environments` asks `environment.list` and then discards the
decision: every environment is returned to anyone who may list, while
the sandbox list narrows by the decision's `filter` and decides each
row by `sandbox.read`. An authorizer that narrows the environment list
to a tenant is ignored, so a caller sees environments the authorizer
meant to hide.

`GET /v1/sandboxes/{id}/ports/{name}`, the port path without its
trailing slash, is answered by the mux's own redirect, whose `Location`
is the absolute path `/v1/sandboxes/{id}/ports/{name}/`. A proxy that
serves the control plane under another prefix forwards that `Location`
unchanged, and the client follows it out of the prefix.

`POST /v1/environments/{id}/keys` mints and `DELETE
/v1/environments/{id}/keys/{jti}` revokes, and nothing lists. The
control plane keeps no record of a key it minted, so a console can
revoke only a key whose `jti` it saw in the same page view.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| The environment list | `environmentList` in `internal/api/environments.go` | the decision's filter and a per-row read |
| The sandbox list's filter | `list` in `internal/api/api.go`: the filter's owners and labels, then `sandbox.read` per row, a refused row left out, a read with no decision refusing the page | a helper the other lists share |
| The secret list's filter | `listSecrets`: owners only | the filter's labels |
| The feed gate of [[066-events-follow]] | `passes` in `internal/api/follow.go`, a copy of the sandbox list's rule | the rule the environment list now needs for the default environment |
| The port redirect | `http.ServeMux`, for a request naming `{name}` without the trailing slash of `{path...}` | a relative `Location` |
| A record of each minted key | nowhere: the mint keeps no copy, the revocation list holds a revoked `jti` and its expiry, and the journal's `environment.keyed` record is pruned by retention | a key registry and a route that reads it |

## Design

### The list rule

Every list route answers the same way, and the environment list now
follows it:

1. The kind's list action is decided once. A deny is `forbidden`; a
   call with no decision is `authorizer_unavailable`; an allow carrying
   a ceiling this server does not hold, `limits.requests_per_minute`, is
   `capability_unsupported`.
2. Each row is held to the decision's `filter`: its `owner` is one the
   filter names when it names any, and it carries every label the
   filter names with the value named.
3. Each row the filter admits is decided by the kind's read action, and
   a row the read refuses is left out. A read that produced no decision
   refuses the whole list with `authorizer_unavailable`, rather than
   answering a page that silently drops or keeps the row.

One function, `admits`, is step 2 for the sandbox, secret and
environment lists and for the feed gate. The secret list applied the
filter's owners and not its labels; it applies both now.

The contract's filter is `owners` and `labels` and nothing else, and
the core applies both to every kind that lists. A filter member outside
those two never reaches the core: the shared client decodes the answer
into the two members and drops any other before a list reads it. A
refusal of such a member belongs to that decoder, and is left open
below.

### The default environment

The default environment, the one `CELLA_DEFAULT_ENVIRONMENT` names, is
the exception to step 2. It is decided by step 3 alone: it is listed
when `environment.read` on it is allowed, whatever the filter names.

The reason is the shape of the filter. Every subject may use the
default environment ([[006-identity]], [[021-data-plane-workers]]), and
its owner is the control plane's own subject, `controller`, or the
administrator who applied it. It carries no caller's labels. A filter
is a conjunction of owners and labels, so a filter that narrows a
member to the member's tenant excludes the default, and no filter the
contract can express admits "this tenant's environments and the shared
default" without admitting what it exists to hide. `environment.read`
names the one object, and it is the question `GET
/v1/environments/{id}` already asks, so a caller who may read the
default by id finds it in the list, and one who may not finds it in
neither.

This is the smallest change to the contract that expresses the union:
no member is added to the filter and no endpoint changes its answer.
What an authorizer must return, for a member to see the environments of
the member's tenant and the default:

| Action | Answer |
|---|---|
| `environment.list` | an allow; the filter that narrows to the tenant, as it is |
| `environment.read` on the default environment's id | an allow for every subject that may use it |
| `environment.read` on any other environment | the tenant's rule, the same one the filter states |

An authorizer that must hide the default from a subject denies its
`environment.read`, which hides it from the read by id as well.

The feed gate of [[066-events-follow]] filters records the way the list
filters rows, so a record about the default environment passes the gate
by `environment.read` alone too.

### The port redirect

`/v1/sandboxes/{id}/ports/{name}`, any method, answers `307 Temporary
Redirect` with `Location: {name}/`, followed by `?` and the query when
the request carried one. The segment is the escaped one the request
named, and a segment holding a colon is written `./{name}/`, so a
client cannot read it as a scheme. A relative reference resolves
against the path the client requested, so a client that reached the
control plane as `/prefix/v1/sandboxes/{id}/ports/{name}` is sent to
`/prefix/v1/sandboxes/{id}/ports/{name}/`. `307` keeps the method and
the body, which is what the mux's own redirect answers and what a proxy
that forwards every method needs.

The redirect reads no object and asks no question, so it tells a caller
nothing about a sandbox it may not read: the route that answers after
it reads, authorizes and gates as before. The bearer is required, as on
every route under `/v1`.

### The key registry

The store gains a facet, `Keys`, beside `Revocations`: one row per key
the control plane minted, `jti`, `environment`, `subject` (who minted
it), `mintedAt`, `expiresAt`, and `revokedAt`, zero while the key is
live. The token is never stored. `store.KeyRegistry` is the facet
outside a transaction:

| Method | Does |
|---|---|
| `Record(key)` | writes the row at mint |
| `Revoke(jti, at, exp)` | in one transaction, marks the row revoked at `at` (the earliest mark is kept) and writes the revocation list's row; a `jti` with no row is revoked all the same, so a key minted before the registry is ended the way it always was |
| `List(environment, cursor, limit)` | the environment's rows in `jti` order, which is mint order, one page |

A row is kept until the key's expiry passes. The reaper's tick already
sweeps the revocation list through `RevocationList.Forget`; that sweep
forgets the expired key rows in the same transaction, so no new tick
and no new lease is needed. Postgres holds the rows in the table
`environment_keys` of migration `000005`; the memory store holds them
in the process, as it holds the revocation list.

`auth.EnvironmentKeys` records at mint with the caller's subject, marks
at revocation, and lists. `NewEnvironmentKeys` takes the registry, and
without one it mints and revokes as before and lists nothing.

### The key list route

```
GET /v1/environments/{id}/keys?limit=&cursor=      environment.key
```

```json
{
  "items": [
    {"jti": "01JBQ7...", "mintedAt": "2026-09-24T10:00:00Z", "exp": "2027-09-24T10:00:00Z",
     "revoked": false, "mintedBy": "https://login.example.com|ops"}
  ],
  "next": ""
}
```

[[006-identity]] names no read action for keys, and a key is an
administrator's credential, so the list is authorized as the mint and
the revocation are: `environment.key`, on the environment the path
names. `limit` is 1 to 200, 50 when absent, and above is
`invalid_field`; `cursor` is the `next` of the previous page. `exp` is
the name the mint's own answer uses. `revokedAt` is present on a
revoked key; `mintedBy` is absent on a row that recorded no subject.
Neither the token nor any part of it is ever in the answer. A control
plane with no signer answers `capability_unsupported`, as the mint and
the revocation do; an environment it does not hold is `not_found`.

## Not in this slice

Paging the environment list by `limit` and `cursor`: the list is small
and answers whole, as it did. A filter member outside `owners` and
`labels` being refused: that is the shared decoder's, not this
server's. A revocation that checks the `jti` belongs to the environment
the path names: a key minted before the registry has no row to check
against.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The environment list narrows by the decision's filter on owners and labels and decides each admitted row by `environment.read`: a member sees the environments the filter admits and the read allows and no other, a filter on labels alone admits every environment carrying them, and no filter admits every readable row | `TestEnvironmentListAppliesTheFilter` | built |
| The default environment is listed whenever `environment.read` on it is allowed, whatever the filter names, and is left out when that read is denied | `TestTheDefaultEnvironmentIsListedByItsRead` | built |
| A list that cannot apply its decision refuses rather than answering around it, on the environment list as on the sandbox list: a per-row read with no decision is 503 `authorizer_unavailable`, and an allow carrying a rate limit is 422 `capability_unsupported` | `TestListsRefuseWhatTheyCannotApply` | built |
| The secret list applies the filter's labels as well as its owners | `TestSecretListAppliesTheWholeFilter` | built |
| The feed gate passes a record about the default environment by `environment.read` alone, and holds every other environment's record to the filter | `TestRecordGateDecidesEachRecord` | built |
| A port path without its trailing slash answers 307 with a relative `Location` that keeps the query, for any method, before any read | `TestPortPathWithoutItsSlashRedirectsRelatively` | built |
| Behind a proxy that serves the control plane under another prefix, a client that follows the redirect reaches the server inside the sandbox with its path and query | `TestPortRedirectThroughAPrefixProxy` | built |
| The store keeps one row per minted key, lists an environment's rows in `jti` order by page, marks a revocation with the earliest instant, and forgets a row once its expiry passes, on both adapters | `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore`, case `Keys` | built |
| The registry revokes in one transaction with the revocation list, revokes a `jti` it holds no row for, and its rows are swept with the expired revocations | `TestKeyRegistry` | built |
| The mint records the key with the caller's subject, the revocation marks it, and without a registry the mint and the revocation work as before and the list is empty | `TestEnvironmentKeysRecordAndList` | built |
| `GET /v1/environments/{id}/keys` lists each key's `jti`, mint time, `exp`, whether it is revoked and who minted it, never the token, by page; it is refused to a caller without `environment.key`, is `not_found` for an environment this server does not hold, and is `capability_unsupported` without a signer | `TestEnvironmentKeyList`, `TestEnvironmentKeyRoutesWithoutASigner` | built |
| `cellad serve` lists a key minted through its route, and lists it revoked after the revocation | `TestEnvironmentKeysAreListedEndToEnd` | built |
| The API document describes the key list and the redirect, and the document and the mux agree | `TestTheDocumentAndTheMuxAgree` | built |

## Outcome

The three defects are fixed, each with a test that fails without its fix.

| Piece | Where |
|---|---|
| `admits`, the filter step every list and the feed gate share, `admitsEnvironment`, and `pageLimit` | `internal/api/lists.go` |
| The environment list under the list rule | `environmentList` in `internal/api/environments.go` |
| The sandbox list, the secret list and the feed gate on `admits` | `internal/api/api.go`, `internal/api/secrets.go`, `internal/api/follow.go` |
| The relative port redirect | `portRedirect` in `internal/api/portproxy.go` |
| The `Keys` facet on both adapters, and migration `000005` | `internal/store/store.go`, `internal/store/memory/memory.go`, `internal/store/postgres/statements.go`, `internal/store/postgres/migrations/000005_environment_keys.*.sql` |
| `store.KeyRegistry`, and the expired key rows forgotten with the revocations | `internal/store/keys.go`, `internal/store/revocations.go` |
| `auth.KeyRecord`, `auth.KeyLog`, the mint's record with its subject and the revocation's mark | `internal/auth/environmentkeys.go`, `Token.IssuedAt` in `internal/auth/mint.go` |
| `GET /v1/environments/{id}/keys` | `environmentKeyList` in `internal/api/environments.go` |
| The registry over the store the revocation list is in | `cmd/cellad/main.go` |
| The redirect and the key list in the API document | `api/openapi.yaml` |

Coverage on `go test -cover`: `internal/api` 92.4%, `internal/auth`
95.7%, `internal/store` 91.1%, memory 93.1%, postgres 91.0%. The
end-to-end that ran is `TestEnvironmentKeysAreListedEndToEnd`: a key
minted through a running `cellad serve`, listed live with its minter and
without its token, then listed revoked.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[006-identity]] | `filter`, on `sandbox.list` only, narrows the list | the filter narrows every list, and the default environment is decided by its `environment.read` alone | the environment list discarded its decision, and the default carries no caller's owner or labels, so a filter that narrows to a tenant excluded it |
| [[006-identity]], [[021-data-plane-workers]] | the control plane keeps no copy of a key; two key routes | it keeps a record of each key and never the key; a third route lists the records | a console lists and revokes a key without having kept the `jti` its mint returned |
| This slice's overview | three defects | the secret list's filter now applies its labels as well | the defect is the environment list's in part, and one function is now the filter step of every list |

### What this leaves open

| Open | Why |
|---|---|
| A filter member outside `owners` and `labels` is dropped by the shared client's decoder before a list reads it, so no list can refuse one | the decoder is `latere.ai/x/pkg/authz`'s `ParseDecision`; refusing such a member there, as that package's conformance suite already flags it at the authorizer, closes it for every core that shares the client |
| Paging the environment list by `limit` and `cursor` | the list is small and answers whole |
| A revocation that checks the `jti` belongs to the environment the path names | a key minted before the registry has no record to check against |
| The owner policy refuses `environment.list` to a subject that is not an administrator, so without an authorizer such a subject cannot list the default environment it may use | the owner policy's rows are [[006-identity]]'s and unchanged here |
| The `X-Forwarded-Prefix` the port proxy sends to the server inside names this server's path, not a prefix a proxy in front serves it under | the proxy in front owns its prefix; the relative redirect needs no knowledge of it |
