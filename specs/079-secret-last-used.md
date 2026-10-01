---
title: "Secret last use: status.lastUsedAt, written by the writer from a use the egress gateway reports when it substitutes the secret, at most once per secret per five minutes"
status: in-progress
track: core
depends_on:
  - specs/010-state.md
  - specs/018-egress-and-secrets.md
  - specs/.archive/039-egress-gateway.md
  - specs/.archive/046-secret-kind.md
  - specs/.archive/076-rolling-replicas.md
affects: [manifest/v1/, egress/, internal/egressd/, internal/api/, controller/, internal/store/, api/openapi.yaml, docs/manifest.md, docs/api.md, CHANGELOG.md]
effort: small
created: 2026-10-01
updated: 2026-10-01
author: changkun
---

# Secret last use

## Overview

A Secret's status says which object it is, who owns it, which version
its value is at, when it was created and updated, and how many sandboxes
mount it. Nothing in it says whether the secret is used. A client that
creates a secret per agent session accumulates many, and the person
reading the list cannot tell which ones still reach an upstream and
which can be deleted.

This spec adds `status.lastUsedAt`: when an egress gateway last
substituted the secret's value into an outgoing request, to within five
minutes. It fixes who observes a use, who writes it, how often, what a
standby and a handoff do, and what a failed write does.

## Current state

| Piece | Where | Today |
|---|---|---|
| The status | `v1.SecretStatus` in `manifest/v1/secret.go` | `id`, `owner`, `version`, `createdAt`, `updatedAt`, `mountedBy` |
| Substitution | `internal/egressd`: `substitutePlaced` on the reverse door, `pkg/egress.Gateway.forward` on the proxy door | the proxy door substitutes inside `pkg/egress`, which reports nothing per entry; the engine calls an entry's `Resolve` only when the entry's placeholder occurs in the request toward a host of its scope, once per request |
| Records | `egress.Record.Substituted` | the field names the secrets substituted on a connection, and no door fills it. The proxy door sends its record when the tunnel closes, which on a kept-alive connection is long after the request |
| The stream | `cellad egress` to `GET /v1/environments/{id}/egress` | the gateway's one path to the control plane. A standby forwards it to the writer like every WebSocket ([[076-rolling-replicas]]); the hub logs and skips a frame type it does not know |
| Secret writes | `Controller.persistSecret`, `Secrets.WriteSecret` | one conditional row write and one journal record per write, inside the writer fence |
| The row | `encodeSecret` in `internal/store/secret.go` | the status is a JSON column of the objects table; the snapshot store keeps the object in its file |

## Design

### The field

`status.lastUsedAt` is an RFC 3339 time in UTC, to the second. It is
absent, not zero, on a secret no gateway has substituted since it was
created. An update keeps it; a delete ends it with the secret.

`v1.SecretLastUsedResolution` (five minutes) is the one constant. The
stamp moves only to a substitution at least that long after the stamp it
replaces, so a secret in continuous use was last substituted at or after
`lastUsedAt` and, unless a report was lost, less than the resolution
after it. A lost report (below) leaves the stamp at the previous one
until the gateway's next report.

### The gateway reports

Substitution runs in `cellad egress`, a data plane process that holds no
store. Its one path to the control plane is the sync stream, so it
reports and the writer writes.

The gateway turns every entry's value into the engine's resolver: the
static value returned by a function, or the oauth token source it
already holds. The engine calls the resolver when it substitutes, so a
resolve that returned a value is a use. The gateway then reports the
pair of the sandbox's principal and the secret's id, at most once per
pair per `v1.SecretLastUsedResolution`. A snapshot forgets every pair,
so the first use after a reconnect, which after a handoff is a use the
new writer has not seen, is reported at once; a purge forgets the
principal's pairs.

The report is the up frame `use`:

```json
{"type": "use", "use": {"principal": "sandbox:sbx_01J9...", "secret": "sec_01J9...", "at": "2026-10-01T12:00:03Z"}}
```

`secret` is the Secret's id, which the map's entry now carries as `id`,
so a secret renamed after a sandbox mounted it is still the one stamped.
`at` is the instant of the substitution. The frame carries no value, no
placeholder, no credential, no host and no path. The protocol stays
`cella.egress.v1`: a hub of an earlier release logs the frame as unknown
and skips it, and a gateway of an earlier release sends none.

A report waits on a bounded buffer that the stream's writer drains, as
records do; a full buffer drops its oldest report. The request that
caused it is never held for it.

### The writer writes

The hub takes a `use` off the stream, normalizes it (a sandbox
principal, an id with the `sec_` prefix, no placeholder), and queues it
on a bounded buffer of the stream, which one goroutine per stream hands
to `Controller.SecretUsed`. The read loop never waits on the controller:
it carries the acknowledgments a create waits on, and the controller's
mutex is held across driver calls. A full buffer drops the report and
logs it.

`SecretUsed(ctx, sandboxID, secretID, at)`:

1. Writes nothing unless the secret exists and the sandbox binds it in
   `status.egressState.secrets`, so a gateway can stamp only a secret a
   sandbox of this control plane mounts.
2. Clamps `at` to the control plane's clock, so a gateway whose clock
   runs ahead cannot stamp the future, and truncates it to the second.
3. Writes when the secret has no stamp, or when `at` is at least
   `v1.SecretLastUsedResolution` after the stamp. A report older than
   the stamp never moves it back.
4. Writes through `SecretUses.WriteSecretUse`, an optional interface of
   the store: one conditional row write at the version last read, with
   no journal record, inside the writer fence. A store that does not
   implement it stamps nothing. The durable adapter over the memory and
   Postgres stores and the snapshot store both implement it.

The stamp appends no journal record. It is an observation of use, as a
sandbox's `lastActivityAt` is, not an act on the Secret; recording it
would append a record every five minutes per live secret and make
`secret.updated` mean "used". The column it lands in is the status JSON
the row already has, so no migration is needed.

### Standby and handoff

A standby never receives a `use`: the gateway's stream is a WebSocket the
standby forwards to the writer ([[076-rolling-replicas]]), and the hub
that answers it is the writer's. A writer that lost its lease without
knowing is refused by the fence with `ErrNotWriter`, which is logged and
dropped like any failed write. The next writer loads every Secret with
its stamp at promotion, so the rule of step 3 reads the stored stamp and
holds across a handoff; the gateways reconnect to it, take a snapshot,
and report their next use at once.

### A failed write

A write of `lastUsedAt` never fails or delays the proxied request. On the
gateway the request path is a map read and a non-blocking enqueue; the
write runs later, in another process. A write that fails is logged at
warn with the sandbox and the secret, is not retried, and the next report
writes the stamp.

## Not in this spec

| Item | Why |
|---|---|
| Filling `Record.Substituted` | the connection record is a separate surface; the proxy door would have to collect names per tunnel, and a use stamp does not need it |
| A `LAST USED` column in `cella get secrets` | the JSON output carries the field; a column is a client change of its own |
| Use counts, or the last use per sandbox | a count is a write per use; the console asks only whether a secret is still used |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A secret never substituted answers no `lastUsedAt`; one stamped answers it in UTC to the second | `TestASecretNeverUsedHasNoLastUse` | not built |
| Two uses inside the resolution write once, one after it writes again, and a report older than the stamp never moves it back; a future `at` is clamped to the control plane's clock | `TestSecretUsedWritesOncePerResolution` | not built |
| A use for a secret the sandbox does not bind, a deleted secret, or an unknown sandbox writes nothing; a store without `SecretUses` stamps nothing | `TestSecretUsedNeedsAMount` | not built |
| The stamp is one conditional row write with no journal record, the next value write is not a version conflict, and an update keeps the stamp; the snapshot store keeps it across a reopen | `TestASecretUseIsWrittenWithoutARecord`, `TestTheFileStoreKeepsASecretsLastUse` | not built |
| A demoted writer's stamp is refused by the fence | `TestTheFenceRefusesAWriterThatLostItsLease` | not built |
| The next writer reads the stamp from Postgres at promotion | `TestAPromotedWriterReadsTheLastUse` | not built |
| The gateway reports a substitution on both doors once per pair per resolution, forgets on a snapshot, and reports nothing for a request that carried no placeholder | `TestTheGatewayReportsASubstitution` | not built |
| A full report buffer never holds the request | `TestAFullUseBufferDoesNotHoldTheRequest` | not built |
| The hub hands a use to the controller off the read loop, a failing write leaves the stream serving acknowledgments, and a malformed use is refused | `TestTheHubHandsUsesToTheController` | not built |
| Through a running control plane, gateway and sandbox, a request on either door stamps the secret it carried | `TestASubstitutionStampsTheSecretsLastUse` | not built |
| The API document and the manifest reference state the resolution the constant holds | `TestTheLastUseResolutionIsDocumented` | not built |
