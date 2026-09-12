---
title: "State: desired and observed, the store, secret values, revocations, the journal, optional Postgres"
status: drafted
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
affects: [internal/store/, internal/config/, migrations/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# State

## Overview

Two states. Desired state is what a caller applied, resolved: every
`Sandbox`, `Secret`, `Volume`, `SandboxSet`, and `Environment` object.
It is the control plane's and lives in the store. Observed state is
what the data plane reports about a sandbox: phase, timestamps, the
shape granted, the labels the driver stamped. It is the driver's, and
the store keeps only a copy of it as an index, rebuilt from the driver
at start and after a lost watch. Beside them the store keeps secret
values encrypted, the revocation list for tokens, the spawn ledger, and
the event journal. Everything lives in memory by default and in
Postgres when `CELLA_DB_URL` is set. A durable store is what lets a
sandbox the data plane lost be recreated rather than forgotten, and
what lets several control plane replicas share one queue; an in-memory
store is what lets one binary run on a laptop with nothing beside it.

## Current state

Not built. The hosted platform kept all product state in Postgres and
read runtime truth from labels; the split into desired and observed is
what this spec adds, so that a durable store recovers a sandbox instead
of merely remembering it.

## Design

### The interface

```go
type Store interface {
	Desired      // every kind's resolved objects, keyed by kind and id
	Observed     // the index of driver state per sandbox
	Secrets      // encrypted values, by secret id and version
	Revocations
	Ledger       // spawn budgets, debited atomically with a child's create
	Journal
	Leases
	Ready(ctx context.Context) error
	Close() error
}

type Desired interface {
	Put(ctx context.Context, obj v1.Object) error
	Get(ctx context.Context, kind, id string) (v1.Object, error)
	ByName(ctx context.Context, kind, owner, name string) (v1.Object, error)
	List(ctx context.Context, kind string, f Filter, page Page) ([]v1.Object, string, error)
	Delete(ctx context.Context, kind, id string) error
}

type Observed interface {
	Put(ctx context.Context, s runtime.State) error
	Get(ctx context.Context, id string) (runtime.State, error)
	List(ctx context.Context, f Filter, page Page) ([]runtime.State, string, error)
	Rebuild(ctx context.Context, environment string, states []runtime.State) error
}
```

`Rebuild` replaces the observed index of one environment from a
driver's `List`, called at start and after a lost watch; desired state
is untouched by it, which is what makes recovery possible: after a
rebuild, a desired sandbox with no observed counterpart is `Lost` and
the controller acts ([[005-lifecycle-controller]]). `Secrets` holds
values under envelope encryption ([[018-egress-and-secrets]]) and hands
a plaintext to one caller, `egress.Compile`. `Revocations` holds `jti`
and `exp` of tokens revoked before expiry, and environment keys, which
have no `exp`. `Ledger` debits a spawn budget in the same transaction
that writes the child. `Journal` appends events, marks them
acknowledged, and serves them per object.

### Memory

The default. Maps under a mutex, the journal a ring per object capped
at `CELLA_JOURNAL_CAP` (default 1000 events), everything dropped at
process end. `Ready` is always nil. Desired state is not durable, so
the start-up log says recovery is off and a second replica is refused.

### Postgres

Selected by `CELLA_DB_URL`. Tables `objects` (desired, one row per
kind and id with the resolved JSON and a version), `observed`,
`secret_values`, `revocations`, `ledger`, `events`, `leases`, with
migrations embedded and applied at start by
`latere.ai/x/pkg/pgxmigrate`; a schema newer than the binary is a
start-up failure naming both versions. `Ready` is one `SELECT 1` with a
1 second budget. The pool is sized by `CELLA_DB_MAX_CONNS` (default 8),
because a replica set shares the database's connection ceiling. Every
query is parameterized and every table has the sandbox `id` as a key,
so a list over a hundred thousand sandboxes is an index scan.

### Replicas

With Postgres, several `cellad` replicas may run against one backend.
Each keeps its own memory of watches; the index is shared; the reaper
and the pool run on one replica at a time under a lease row in
`leases` with a 15 second TTL, so two replicas do not both delete or
both refill. Without Postgres, a second replica is a configuration
error the start-up log names, since two memories of one backend would
each try to be the reaper.

### Why two states

If the store were the only truth, a sandbox the store forgot would run
on unbilled while the control plane denied it existed. If the driver
were the only truth, a Pod evicted by a node drain would take a
tenant's environment with it and nothing would notice but the tenant.
Desired in the store and observed in the driver gives each failure a
recovery: a driver that lost an object is told to recreate it; a store
that lost its index rebuilds it from labels; the reaper keeps running
from labels either way.

## Not in this spec

Backups and restores of the Postgres tables
([[014-release-and-installation]]); the leader election's interaction
with the k8s backend's own lease, which is this spec's lease and not a
Kubernetes Lease object.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Both stores pass one suite: desired put, get, by name, filtered and paged list per kind, delete; observed put, get, list, rebuild; secret value round trip under encryption; revoke and forget at `exp`; ledger debit that fails at zero and is atomic under contention; append, acknowledge, read per object; lease acquire and expiry | `TestStoreSuite` over memory and Postgres in a test container | not built |
| `Rebuild` after the driver holds three sandboxes and the observed index holds a fourth leaves three observed and four desired, and the fourth is reported `Lost` | `TestRebuildKeepsDesiredState` | not built |
| A plaintext secret value is returned by exactly one method and appears in no other query result or log | `TestSecretValuesAreConfined` | not built |
| A binary older than the schema refuses to start naming both versions | `TestSchemaAheadOfBinary` | not built |
| Two replicas against one Postgres run one reaper; killing the holder moves the lease within 15 seconds | `TestLeaseFailover` | not built |
| The Postgres store serves a 100,000 row list page in under 50 ms | `TestListIsIndexed` with `EXPLAIN` asserting an index scan | not built |
| No package outside `internal/store` imports the Postgres driver | `TestDriverIsConfined` | not built |
