---
title: "State: backend truth, the index, revocations, the journal, optional Postgres"
status: drafted
track: core
depends_on:
  - specs/004-runtime-backend-contract.md
  - specs/005-lifecycle-controller.md
affects: [internal/store/, internal/config/, migrations/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# State

## Overview

The backend holds the truth about every sandbox. `cellad` keeps beside
it an index for fast reads, a revocation list for workload tokens, and
a journal for events. All three live in memory by default and in
Postgres when `CELLA_DB_URL` is set. No lifecycle decision reads the
store; a store that is lost is rebuilt from the backend, minus the
journal. This is what lets a single binary run on a laptop with nothing
beside it and the same binary run replicated behind a database.

## Current state

Not built. The hosted plane kept all product state in Postgres; the
index-not-truth rule is inherited from its runtime, where labels were
already the truth for the reaper.

## Design

### The interface

```go
type Store interface {
	Index
	Revocations
	Journal
	Ready(ctx context.Context) error
	Close() error
}

type Index interface {
	Put(ctx context.Context, s runtime.State) error
	Get(ctx context.Context, id string) (runtime.State, error)
	ByName(ctx context.Context, owner, name string) (runtime.State, error)
	List(ctx context.Context, f Filter, page Page) ([]runtime.State, string, error)
	Delete(ctx context.Context, id string) error
	Rebuild(ctx context.Context, states []runtime.State) error
}
```

`Rebuild` replaces the whole index from a backend `List`, called at
start and after a lost watch. `Revocations` holds `jti` and `exp` of
tokens revoked before expiry and forgets them at `exp`. `Journal`
appends events, marks them acknowledged, and serves them per sandbox.

### Memory

The default. Maps under a mutex, the journal a ring per sandbox capped
at `CELLA_JOURNAL_CAP` (default 1000 events) and dropped at process
end. `Ready` is always nil.

### Postgres

Selected by `CELLA_DB_URL`. Three tables, `sandboxes`, `revocations`,
`events`, with migrations embedded and applied at start by
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

### Why not truth

A store that were truth would need the backend to agree with it, and
the failure mode of that design is a sandbox the store forgot that a
cluster still bills for. With the backend as truth, the worst loss on
a store failure is history, and the reaper keeps running from labels.

## Not in this spec

Backups and restores of the Postgres tables
([[014-release-and-installation]]); the leader election's interaction
with the k8s backend's own lease, which is this spec's lease and not a
Kubernetes Lease object.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Both stores pass one suite: put, get, by name, filtered and paged list, delete, rebuild, revoke and forget at `exp`, append and acknowledge and read per sandbox | `TestStoreSuite` over memory and Postgres in a test container | not built |
| `Rebuild` after the backend holds three sandboxes and the store holds a fourth leaves three | `TestRebuildDropsWhatTheBackendLost` | not built |
| A binary older than the schema refuses to start naming both versions | `TestSchemaAheadOfBinary` | not built |
| Two replicas against one Postgres run one reaper; killing the holder moves the lease within 15 seconds | `TestLeaseFailover` | not built |
| The Postgres store serves a 100,000 row list page in under 50 ms | `TestListIsIndexed` with `EXPLAIN` asserting an index scan | not built |
| No package outside `internal/store` imports the Postgres driver | `TestDriverIsConfined` | not built |
