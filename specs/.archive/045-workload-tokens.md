---
title: "Workload tokens: the identity every sandbox carries, projected by the driver, rotated by the reaper, revoked by the jti"
status: complete
track: core
depends_on:
  - specs/006-identity.md
  - specs/005-lifecycle-controller.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/037-lifecycle-enforcement.md
  - specs/.archive/043-postgres-store.md
affects: [runtime/, controller/, internal/auth/, internal/store/, internal/api/, manifest/v1/, cmd/cellad/, specs/]
effort: medium
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# Workload tokens

## Overview

Slice 045 of [[031-hosted-sandbox-consolidation]]. It ports the mint,
the rotation and the revocation list of `sandbox/internal/tokens` into
the shape [[006-identity]] states, and it closes the four acceptance
rows that three specs left waiting on each other: the mint is
[[006-identity]]'s, the re-mint at two thirds of a lifetime is
[[005-lifecycle-controller]]'s, the projection at `/run/cella/token` is
[[004-runtime-contract]]'s, and the revocation list is [[010-state]]'s.

A process inside a sandbox calls the control plane back with an identity
that dies with the sandbox. The control plane mints it at create, the
driver projects it into the sandbox as a file, the reaper re-mints and
re-projects it before it expires, and every token the control plane
replaces or ends is refused from that moment by its `jti`.

What the hosted product carried and this slice drops: operator tokens
for people, the token catalog and its scope filter, the hosted session
handling of `sandbox/internal/auth`, and `NeedsIdentityToken`, the
hosted rule that gave a token only to a sandbox whose policy or
credential class asked for one. [[004-runtime-contract]] states that
every driver projects the workload token, so every sandbox gets one.

## Current state

`internal/auth/mint.go` signs both tokens [[006-identity]] names, with
the claims of its table, the `jti`, the reserved subjects and the key
set at `/.well-known/jwks.json`. Nothing calls `MintWorkload` outside
its own test: the controller has no `Tokens`, `runtime.CreateSpec` has
no `Token`, no driver projects one, and the verifier checks no
revocation, because the list is the seam [[043-postgres-store]] left at
`store.Revocations`, with its table in the schema and no accessor on
`Tx`.

The reaper of [[037-lifecycle-enforcement]] holds three of the five
rules of [[005-lifecycle-controller]]'s table and [[043-postgres-store]]
holds the fourth; the `token` row is the empty slot. Recovery recreates
a lost sandbox with the same id, name, labels and lifecycle and mints no
token, which is the one thing its comment says slice 045 owes it.

## Design

### The claims

What `Signer.MintWorkload` signs, unchanged by this slice except that a
caller now exists:

| Claim | Value |
|---|---|
| `iss` | `CELLA_PUBLIC_URL` |
| `sub` | `sandbox:<sbx_ id>` |
| `aud` | `[CELLA_OIDC_AUDIENCE]` |
| `exp` | the sandbox's `expiresAt` or `WorkloadTokenLifetime` (24 hours) from the mint, whichever is sooner |
| `iat` | the mint |
| `jti` | a ULID, the key of a revocation |
| `environment` | the `env_` id the sandbox runs in |
| `spawn` | absent until [[022-mesh-and-spawn]] fills the budget from desired state |

`kid` is the RFC 7638 thumbprint of the signing key, so the token
verifies against the key set with any conforming library and without
asking `cellad`.

### The act

```mermaid
sequenceDiagram
  participant C as controller
  participant T as Tokens (signer + revocations)
  participant D as Driver
  participant S as sandbox

  C->>T: Mint(sandbox)
  T-->>C: token, jti, exp
  C->>D: Create(spec with Token)
  D->>S: project /run/cella/token, 0400
  Note over C: status.token = {jti, iat, exp}

  loop every reaper tick
    C->>C: now >= iat + 2/3 * (exp - iat) ?
  end
  C->>T: Mint(sandbox)
  T-->>C: token', jti', exp'
  C->>D: Update(Change{Token: token'})
  D->>S: re-project the file, no restart
  C->>T: Revoke(jti, exp)

  C->>D: Delete
  C->>T: Revoke(jti', exp')
```

The rotation instant, where `iat` is the mint of the token the sandbox
holds and `exp` its expiry:

$$t_{\text{rotate}} = iat + \tfrac{2}{3}\,(exp - iat)$$

A token whose `exp` is already the sandbox's own `expiresAt` is not
rotated: a re-mint cannot extend it, so the rule would fire on every
tick until the sandbox is reaped. The sandbox and the identity it
carries end at the same instant, which is what the cap in the `exp` row
above is for.

### The seam the controller declares

```go
// In controller, implemented in internal/auth over the signer and the
// store's revocation list.
type Tokens interface {
	Mint(ctx context.Context, obj v1.Sandbox) (token, jti string, exp time.Time, err error)
	Revoke(ctx context.Context, jti string, exp time.Time) error
}

// TokenSweeper is the optional half: a list that forgets a row whose
// exp has passed, swept on the reaper's tick.
type TokenSweeper interface {
	Forget(ctx context.Context, before time.Time) (int, error)
}
```

`Options.Tokens` unset is a control plane that mints nothing: every
sandbox is created with no `Token`, every driver projects no file, and
the rule never fires. That is the hosted regression "no token when the
sandbox needs none" restated in the contract that replaced the hosted
rule.

What the control plane keeps per sandbox is `status.token`, three fields
beside `status.egressState` and read the same way: the `jti` a
revocation is keyed by, the `iat` the two thirds is measured from, and
the `exp`. The value is never one of them. It is desired state and not
something a caller reads, so the API strips it from every response, and
it survives a restart with the object it belongs to, which is what lets
a control plane that restarted revoke the token a sandbox still holds.

### Where the mint sits in the create order

Step 5 of [[005-lifecycle-controller]]'s create order, after the
boundary is in a gateway and before the driver is called, undone by a
`Revoke` of the `jti` when a later step fails. Recovery mints at the
same point in the same order, and revokes the token the lost sandbox
held once the recreated object exists: a `Create` that reports
`ErrAlreadyExists` adopted a running sandbox that still holds the old
token, so the new one is projected with `Update` before the old `jti` is
revoked, and a `Create` that failed for any other reason revokes the
`jti` it just minted and leaves the sandbox's own alone.

A delete revokes at `Deleting`, before the driver's `Delete` returns,
which is what makes a deleted sandbox's token refused at once
([[006-identity]]) without the verifier reading a phase.

### The projection

`runtime.CreateSpec.Token` and `runtime.Change.Token`, both `[]byte` and
both additive. `runtime.TokenPath` is `/run/cella/token` and
`runtime.TokenFileEnv` is `CELLA_TOKEN_FILE`, which every driver sets to
where it actually put the file, as the trust variables of
[[018-egress-and-secrets]] name the authority's path. A sandbox reads
one variable and finds its identity whichever driver runs it, and the
conformance case is capability-neutral because of it.

`CreateSpec.Token` is not serialized: the k8s driver keeps the create
spec in a claim annotation so a `Start` re-renders the Pod it first
rendered, and a credential does not belong in an annotation.

| Driver | Where | How a re-projection lands |
|---|---|---|
| native | `<sandbox dir>/run/cella/token`, mode 0400, in the directory the driver owns rather than the workspace, which is the user's | the file is rewritten in place |
| podman | `/run/cella/token` in the container, mode 0400, owned by `spec.user` where one is set | a second archive PUT over the same path |
| k8s | a Secret `cella-token-<object name>` projected read-only at `/run/cella`, `defaultMode` 0400 | the Secret's data is patched and kubelet re-syncs the file, so the workload keeps running |

The k8s mount is a directory and a `projected` volume with one `secret`
source, for two reasons. A `subPath` mount of a Secret key does not
re-sync when the Secret changes, so a rotation would need a restart; and
[[018-egress-and-secrets]] puts the gateway's authority in the same
`/run/cella` directory, which is a second source of the same projection
rather than a second mount. The mode is 0400 and the Pod's `fsGroup`
makes the file readable by the sandbox's own group and by nobody else.
A claim annotation records that a token was projected, so a `Start`
renders the mount the create rendered without holding the value.

### The revocation list

`store.Tx.Revocations()` on both adapters, over the table and the `exp`
index [[043-postgres-store]] left: `Revoke` is idempotent, because a
retried rotation or recovery revokes a `jti` it already revoked;
`Revoked` is the verifier's question; `Forget` drops the rows whose
`exp` has passed and runs on the reaper's tick, so the list is bounded
by the longest lifetime any live token has and not by the number of
sandboxes that ever ran.

The verifier refuses a revoked `jti` on the minted path only, where the
token is one `cellad` signed; a listed issuer's token is its issuer's to
revoke. A minted token that carries no `jti` is refused for the same
reason: a credential that cannot be revoked is not one this control
plane issued.

### What emits no event

[[009-events]]'s type set is closed and names one token event,
`sandbox.token`, for a mint outside the projection, which is
[[008-api]]'s `POST /v1/sandboxes/{id}/token` and not this slice. A mint
at create rides in the `sandbox.created` record that step already
writes; a rotation writes `status.token` under `MutationStatus`, which
[[042-events]] journals and never delivers, because the workload's
identity changing is not a change a reader of the feed acts on.

## Not in this spec

The `POST /v1/sandboxes/{id}/token` route and the environment key routes
([[008-api]], [[021-data-plane-workers]]); the spawn budget the claim
carries ([[022-mesh-and-spawn]]); the gateway's own credential, which is
not a token and never rotates under a running process
([[018-egress-and-secrets]]); `CELLA_TOKEN_FILE` as a reserved
environment key, which belongs with the rest of the reserved set in
`egress` and `manifest`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A minted workload token carries every claim of the table above and no others, with `exp` capped by the sandbox's own expiry | `TestWorkloadTokensMintClaims` over the decoded payload; `TestWorkloadTokensMintIsCappedByTheSandbox` | built |
| A revoked `jti` is refused by the verifier, an unrevoked one is accepted, a list that cannot answer refuses rather than accepts, and a minted token with no `jti` is refused | `TestRevokedTokenIsRefused`, `TestRevocationListThatCannotAnswerRefusesTheToken`, `TestMintedTokenWithoutAJTIIsRefused` | built |
| `Forget` drops a row whose `exp` passed and keeps one whose has not, on both store adapters, and a repeated revocation is one row | the `Revocations` case of `internal/store/storetest`, run against memory and Postgres and required to fail two deliberately wrong adapters | built |
| A sandbox is created with a token, the driver projects it, and the control plane records the `jti` and never the value, in no answer a caller reads | `TestCreateMintsAndProjects`; `TestTokenIsNotInTheClaimsRecord` for the k8s claim | built |
| A create whose driver call fails revokes the `jti` it minted, and one whose mint fails reaches no driver | `TestCreateFailureRevokes`, `TestCreateRefusesWhenTheMintFails` | built |
| A token past two thirds of its life is re-minted, re-projected and the old `jti` revoked in one act, and a token already capped by the sandbox's expiry is not rotated | `TestTokenReprojection` and `TestTokenRuleRunsUnderTheReaperLoop` under the fake clock; `TestCappedTokenIsNotRotated`; `TestDueForRotation` over the rule; `TestRotationFailureKeepsTheOldToken` | built |
| A recovered sandbox carries a new token and the previous `jti` is revoked, and an adopted one is re-projected before the previous is ended | `TestRecoveryMintsAndRevokes`, `TestRecoveryAdoptionReprojectsBeforeRevoking` | built |
| A delete revokes the sandbox's `jti`, whether a caller asked for it or a deadline rule did | `TestDeleteRevokes`, `TestReapedSandboxRevokes` | built |
| A control plane with no `Tokens` creates a sandbox with no token and projects no file | `TestNoTokensNoProjection`, and the per driver `TestNativeProjectsNoTokenWhenThereIsNone`, `TestPodmanWithoutATokenProjectsNothing`, `TestNoTokenProjectsNothing` | built |
| Every driver projects the token at the path its row names, mode 0400, and an `Update` with a new token is what the next read returns | the `TokenProjection` case of `runtime/runtimetest`, which a driver that drops a re-projection fails; the k8s half over the client double | built |
| A workload token reads its own sandbox and execs into it, and is refused on another sandbox's routes and on its own delete and stop | `TestWorkloadReachesItsOwnSandbox`, `TestRevokedWorkloadTokenIsRefusedByTheAPI` | built |
| A sandbox created by `cellad serve` reads its token out of the projection, calls the API with it, is refused on another sandbox, and is refused everywhere once it is deleted | `TestWorkloadTokenEndToEnd` in `cmd/cellad` | built |

## Outcome

Every sandbox carries an identity. The controller mints one at step 5 of the
create order, the driver projects it as a file the sandbox's own user reads,
the reaper re-mints and re-projects it once two thirds of its lifetime has
passed, and the `jti` of every token the control plane replaced or ended is
refused by the verifier from that moment.

What each package holds:

- `runtime` carries `CreateSpec.Token` and `Change.Token`, `TokenPath` and
  `TokenFileEnv`. `CreateSpec.Token` is tagged `json:"-"`, because the k8s
  driver keeps the create spec in a claim annotation and a credential does
  not belong in one. The conformance suite gained `TokenProjection`, which
  reads the token back through `Exec` at the path the variable names, checks
  the mode, and reads the re-projection an `Update` makes; a driver that
  drops `Change.Token` fails it, which the suite's own liar case proves.
- `native` writes `<sandbox dir>/run/cella/token` at mode 0400 through a
  temporary file and a rename, so a workload reading while the controller
  re-projects reads one whole token or the other. `podman` puts it in the
  container at `/run/cella/token` with an archive PUT, owned by the numeric
  user the spec names, and the gateway authority of slice 039 now goes
  through the same helper. `k8s` projects a Secret `cella-token-<name>` on
  `/run/cella` as a `projected` volume with one `secret` source, which is
  what lets slice 018 add the authority beside it and what makes a rotation
  reach a running Pod: a `subPath` mount does not follow a Secret that
  changed. A claim annotation records that a token was projected, so a
  `Start` renders the mount without holding the value, and the transfer
  helper Pod carries no identity at all.
- `controller` declares `Tokens` and the optional `TokenSweeper`. The mint
  sits between the boundary and the driver with a revocation as its undo;
  the `token` rule is asked only of a sandbox no deadline rule claimed, and
  skips a token whose expiry is already the sandbox's own, because a re-mint
  cannot extend it and the rule would otherwise fire on every tick. A
  rotation mints, re-projects, then revokes, so the sandbox never holds a
  revoked token and never holds none. Recovery mints before the driver call
  and revokes the previous token after; an adopted sandbox is re-projected
  first, because it is running with the token it was created with. Every
  delete revokes, whether a caller asked for it or a deadline rule did.
- `internal/auth` gained `WorkloadTokens` over the signer and the list, and
  `Verifier.VerifyContext`, which refuses a revoked `jti`, refuses a minted
  token that carries none, and refuses rather than accepts when the list
  cannot answer.
- `internal/store` filled the `Revocations` seam on both adapters, with an
  idempotent `Revoke`, and `NewRevocations` for the three callers that are
  not inside a transaction of their own.
- `manifest/v1` gained `SandboxStatus.TokenState`, three fields and never
  the token, stripped from every answer the way `EgressState` is.

Nothing emits an event. [[009-events]]'s set is closed and its one token
type is for a mint outside the projection, which is [[008-api]]'s route; a
rotation writes the status under `MutationStatus`, which [[042-events]]
journals and never delivers.

The `cellad` run that proves it end to end is `TestWorkloadTokenEndToEnd`:
a node serving the native driver, two sandboxes created over `/v1`, the
first reading its own token out of the projection with
`cat "$CELLA_TOKEN_FILE"`, calling `GET /v1/sandboxes/{id}` with it and
reading itself, refused on the second sandbox's read and delete, and
refused everywhere once it is deleted. The rotation is proven at the
controller under the fake clock, in `TestTokenReprojection` and in
`TestTokenRuleRunsUnderTheReaperLoop`, which drives it through the loop a
tick runs.

Coverage on the packages this slice touched: `internal/auth` 95.2%,
`controller` 94.6%, `runtime/runtimetest` 96.8%, `runtime/k8s` 93.3%,
`runtime/podman` 93.6%, `runtime/native` 90.4%, `internal/store` 91.3%,
`internal/store/memory` 95.8%, `internal/store/postgres` 92.3%,
`internal/api` 91.2%, `manifest/v1` 100%, `cmd/cellad` 90.6%. `go test
-race` passes and `go tool lateregate` reports sixteen gates passed.

One thing left open. `CELLA_TOKEN_FILE` is not in the reserved environment
set that `egress` and `manifest` share, so a manifest that sets it has its
value overwritten by the projection without being refused. The set belongs
to those two packages and to [[018-egress-and-secrets]]'s slice, and adding
one key to it there is smaller than reaching into them from here.
