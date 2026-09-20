---
title: "State: desired and observed, the store contract, transactions, secret values, the journal, queues and operations, optional Postgres"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
affects: [internal/store/, internal/config/, migrations/]
effort: large
created: 2026-09-12
updated: 2026-09-20
author: changkun
---

# State

## Overview

Two states. Desired state is what a caller applied, resolved: every
`Sandbox`, `Secret`, `Volume`, `SandboxSet`, and `Environment` object,
with the status the controller writes beside it. It is the control
plane's and lives in the store. Observed state is what the data plane
reports about a sandbox: phase, timestamps, the shape granted, the
labels the driver stamped. It is the driver's, and the store keeps a
copy as an index, rebuilt from the driver at start and after a lost
watch. Beside them the store keeps secret values encrypted, the
revocation list for tokens, the spawn ledger, the event journal, the
scheduler's queue, and the worker operation queue. Everything lives in
memory by default and in Postgres when `CELLA_DB_URL` is set. A
durable store is what lets a sandbox the data plane lost be recreated
rather than forgotten, and what lets several control plane replicas
share one queue and one set of workers; an in-memory store is what
lets one binary run on a laptop with nothing beside it.

This spec is `internal/store`'s implementation contract. The narrow
interfaces `controller` declares for itself ([[005-lifecycle-controller]])
are satisfied by an adapter in `internal/serve` over this store.

## Current state

[[043-postgres-store]] builds `internal/store`, both adapters, and one suite over them: desired state with a version, the observed index, the journal's append and read, the leases, and the secret value envelope. The seams it leaves are the revocations, the queue and the operations: their tables are in the schema and their interfaces are declared, with no accessor on `Tx` until a caller exists. [[026-direct-control-plane]]'s locked file snapshot stays for a single process with `CELLA_DATA_DIR` and no database.

Design provenance: The hosted platform kept all product state in Postgres and
read runtime truth from labels; the split into desired and observed is
what this spec adds, so that a durable store recovers a sandbox instead
of merely remembering it.

## Design

### The contract

```go
type Store interface {
	// Tx runs fn in one transaction: every write inside commits together
	// or not at all. The memory store holds one mutex for the duration.
	Tx(ctx context.Context, fn func(Tx) error) error
	Tx // the same method sets, outside a transaction, each call its own
	Ready(ctx context.Context) error
	Close() error
}

type Tx interface {
	Desired() Desired
	Observed() Observed
	Values() Values
	Revocations() Revocations
	Ledger() Ledger
	Journal() Journal
	Queue() Queue
	Operations() Operations
	Records() Records
	Leases() Leases
}

// v1.Object is the interface every kind implements (003): Kind, ID,
// Owner, Name, and the object itself for JSON.
type Desired interface {
	Put(ctx context.Context, obj v1.Object, ifVersion int64) (version int64, err error) // ErrVersionConflict when the row moved; 0 creates
	Get(ctx context.Context, kind, id string) (v1.Object, int64, error)
	ByName(ctx context.Context, kind, owner, name string) (v1.Object, int64, error)
	List(ctx context.Context, kind string, f Filter, page Page) (objs []v1.Object, next string, err error)
	Delete(ctx context.Context, kind, id string) error // marks deleted; the row stays until the journal is drained
	Count(ctx context.Context, kind, owner string) (int, error) // excludes phase Deleting and deleted rows (007)
	PutStatus(ctx context.Context, kind, id string, status any) error // the controller's; Rebuild never touches it
	LastApplied(ctx context.Context, id string) (*v1.Sandbox, error)
	SetLastApplied(ctx context.Context, id string, s *v1.Sandbox) error
}

type Observed interface {
	Put(ctx context.Context, environment string, s runtime.State) error
	Get(ctx context.Context, id string) (runtime.State, string, error) // state and its environment
	List(ctx context.Context, f Filter, page Page) ([]runtime.State, string, error)
	Rebuild(ctx context.Context, environment string, states []runtime.State) error // replaces that environment's rows only
}

type Values interface { // secret values; the one decrypting method has one caller, egress.Compile (018)
	Put(ctx context.Context, secretID string, plaintext []byte) (version int, err error)
	Open(ctx context.Context, secretID string) (plaintext []byte, version int, err error)
	Delete(ctx context.Context, secretID string) error
	Rewrap(ctx context.Context, oldKEK, newKEK []byte) (n int, err error)
}

type Revocations interface {
	Revoke(ctx context.Context, jti string, exp time.Time) error
	Revoked(ctx context.Context, jti string) (bool, error)
	Forget(ctx context.Context, before time.Time) (n int, err error) // rows whose exp passed
}

type Ledger interface {
	Debit(ctx context.Context, parentID string) error  // ErrBudgetExhausted at zero; atomic
	Credit(ctx context.Context, parentID string) error // the undo of 005 step 1
	Balance(ctx context.Context, parentID string) (budget, used int, err error)
}

type Journal interface {
	Append(ctx context.Context, e Event) (seq int64, err error)            // seq is per object, monotonic
	Pending(ctx context.Context, limit int) ([]Event, error)               // unacknowledged, next_attempt_at passed, one per object at most, oldest seq first
	Acknowledge(ctx context.Context, id string) error
	Defer(ctx context.Context, id string, attempts int, next time.Time) error
	Drop(ctx context.Context, id string) error
	ByObject(ctx context.Context, objectID string, page Page) ([]Event, string, error) // newest first
	Prune(ctx context.Context, before time.Time) (n int, err error)
}

type Queue interface {
	Enqueue(ctx context.Context, item QueueItem) error
	Dequeue(ctx context.Context, environment, queue string) (*QueueItem, error) // highest priority, then fair share, then arrival (020)
	Remove(ctx context.Context, sandboxID string) error
	Position(ctx context.Context, sandboxID string) (int, error)
}

type Operations interface { // the worker operation queue (021)
	Enqueue(ctx context.Context, op Operation) error
	Claim(ctx context.Context, environment, worker string, n int) ([]Operation, error) // unclaimed or claimed by a worker whose lease lapsed
	Acknowledge(ctx context.Context, opID string, result []byte) error
	Heartbeat(ctx context.Context, environment, worker string, at time.Time) error
	Workers(ctx context.Context, environment string) ([]Worker, error) // with their last heartbeat
}

type Records interface { // egress connection records (018); never the journal
	Append(ctx context.Context, sandboxID string, r Record) error
	BySandbox(ctx context.Context, sandboxID string, page Page) ([]Record, string, error) // newest first
	Prune(ctx context.Context, before time.Time) (n int, err error)
}

type Leases interface {
	Acquire(ctx context.Context, name, holder string, ttl time.Duration) (held bool, err error)
	Release(ctx context.Context, name, holder string) error
}
```

`Filter` here is the store's, distinct from `runtime.Filter`: `Owner`,
`Phase`, `Environment`, `Root`, `Parent`, `Labels map[string]string`,
`Queue`, `IDs`. `Page` is `Limit` and `Cursor`; a list returns the next
cursor or empty. `Event`, `QueueItem`, `Operation`, and `Worker` carry
the fields their tables below name.

### Desired state and status

One row per object holds the resolved JSON, the status JSON the
controller writes through `PutStatus`, the last applied desired state
for [[005-lifecycle-controller]]'s update diff, a version for
optimistic concurrency, and `deleted_at`. The API reads an object and
its version, resolves, and `Put`s with that version; a row that moved
in between is `ErrVersionConflict`, which [[008-api]] returns as 409
`version_conflict`. `(kind, owner, name)` is unique among rows with
`deleted_at` null, which is what backs `name_taken` and lets a name be
reused after delete. Status carries the controller's own phases,
`Queued` and `Recovering`, which no driver reports and `Rebuild`
never removes. `Count` is the count ceiling's query
([[007-admission]]).

### Observed state

One row per sandbox per environment, the driver's `State` as JSON
beside its stamped identity as columns, and the environment id.
`Rebuild` replaces the rows of one environment inside one transaction
and leaves every other environment's and every `objects` row alone;
after it, a desired sandbox with no observed counterpart is `Lost` and
the controller acts. `status.driver` and `status.isolation` come from
the environment's own status, not from this table.

### Secret values

Envelope encryption per [[018-egress-and-secrets]]: a random 32-byte
data key per secret, the value encrypted under it with AES-256-GCM,
the data key wrapped by `CELLA_SECRET_KEY` with AES-256-GCM. The row
holds the secret id, the version, the wrapped data key with its nonce,
and the ciphertext with its nonce; the two are separate columns so
`Rewrap` touches one. `Open` is the only method that returns a
plaintext, and its only caller is `egress.Compile`, asserted by a test
over `go list -deps` and call sites. The memory store holds the same
ciphertext under the same KEK, so no store keeps a plaintext at rest.

### Revocations and the ledger

Revocations are `jti` and `exp`; `Forget` runs on the reaper's tick
and drops rows whose `exp` passed, environment keys included, since
they carry an `exp` ([[006-identity]]). The ledger is one row per
sandbox with `budget` and `used`; `Debit` is `UPDATE ... WHERE used <
budget` and reports exhaustion from the row count, so two concurrent
debits at one remaining unit yield one success, and `Credit` is its
undo.

### The journal

One row per event: `id` (`evt_`), `object_id`, `seq` per object,
`type`, `time`, `payload`, `attempts`, `next_attempt_at`, `acked_at`.
Delivery ([[009-events]]) runs on the replica holding the `journal`
lease: `Pending` returns at most one event per object, the lowest
unacknowledged `seq`, so a failing event holds the ones behind it for
that object and no other; `Defer` records the backoff; `Drop` after 24
hours of attempts. `ByObject` serves [[008-api]]'s `GET .../events`
newest first, paged by `seq`. Postgres retention is
`CELLA_JOURNAL_RETENTION` (default `720h`), applied by `Prune` on the
reaper's tick to acknowledged or dropped rows; the memory store keeps
a ring of `CELLA_JOURNAL_CAP` per object and loses unacknowledged
events at process end, which the start-up log says.

### The scheduler's queue and capacity

`queue` rows are `sandbox_id`, `environment`, `queue`, `priority`,
`subject`, `enqueued_at`. `Dequeue` orders as
[[020-scheduling-and-sets]] says and runs under the `scheduler`
lease. Capacity in use is never stored: `cpu`, `memory`, and `sandboxes` are
the sum over desired sandboxes on the environment whose status phase
is `Pending`, `Starting`, `Running`, `Stopping`, or `Recovering`, and
`disk` the sum over those and `Stopped` and `Failed` as well, since a
stopped sandbox's storage stays on the substrate
([[020-scheduling-and-sets]]); a restart cannot double count.

### Operations and workers

`operations` rows are `id` (`op_`), `environment`, `sandbox_id`,
`type`, `payload`, `state` (`queued`, `claimed`, `acked`),
`claimed_by`, `claimed_at`, `attempts`, `result`. `Claim` hands a
worker unclaimed operations and ones whose claimer's heartbeat is
older than the worker lease, so a dropped connection redelivers to
whichever worker and replica is live ([[021-data-plane-workers]]).
`workers` rows are `environment`, `worker`, `replica`,
`last_heartbeat`; the environment's phase is computed from them and
written to its status by the replica holding the `environments`
lease. A worker's stream is one replica's; its claims and results are
every replica's.

### Memory

The default. Maps under one mutex, which is `Tx`. `Ready` is always
nil. Desired state is not durable, so the start-up log says recovery
is off and states the assumption that this process is the only
replica; two in-memory replicas cannot detect each other.

### Postgres

Selected by `CELLA_DB_URL`, a `postgres://` URL. Tables: `objects`,
`observed`, `secret_values`, `revocations`, `ledger`, `events`,
`egress_records`, `queue`, `operations`, `workers`, `leases`. Migrations are embedded
under `migrations/` and applied at start through
`latere.ai/x/pkg/pgxmigrate.Up`, which imports no driver, so
`internal/store` blank-imports golang-migrate's `pgx/v5` driver and
hands the migrator the URL with its scheme rewritten to `pgx5://`.
Before `Up`, the store reads `schema_migrations`: a version above the
highest embedded migration is a start-up failure naming both, since
`Up` would report no change and run against a schema it does not know;
a dirty flag is a start-up failure naming the version, for the
operator to repair. `Ready` is `SELECT 1` with a 1 second budget,
inside [[002-repository-scaffold]]'s readiness budget. The pool is
sized by `CELLA_DB_MAX_CONNS` (default 4), because a replica set
shares the database's connection ceiling.

Indexes, one per query shape: `objects (kind, owner, name) unique where
deleted_at is null`; `objects (kind, owner, phase)` for lists and the
count; `objects (kind, environment)`; `objects (kind, root)`; a GIN
index on `objects.labels` for `?label=`; `observed (environment)`;
`events (object_id, seq)` and `events (next_attempt_at) where acked_at
is null`; `revocations (exp)`; `egress_records (sandbox_id, at desc)`; `queue
(environment, queue, priority desc, enqueued_at)`; `operations (environment, state, created_at)`;
`workers (environment, last_heartbeat)`.

### Leases

`leases` rows are `name`, `holder`, `expires_at`; `Acquire` is a
conditional upsert. Names: `reaper`, `journal`, `scheduler`,
`environments`, and `pool:<environment>`; every TTL is 15 seconds and
the holder renews at a third of it.

### Why two states

If the store were the only truth, a sandbox the store forgot would run
on unbilled while the control plane denied it existed. If the driver
were the only truth, a Pod evicted by a node drain would take a
tenant's environment with it and nothing would notice but the tenant.
Desired in the store and observed in the driver gives each failure a
recovery: a driver that lost an object is told to recreate it; a store
that lost its observed index rebuilds it from labels; the reaper keeps
running from labels either way.

## Not in this spec

Backups and restores of the Postgres tables
([[014-release-and-installation]]); the queue's ordering rule
([[020-scheduling-and-sets]]); the worker protocol over the operations
table ([[021-data-plane-workers]]); the `v1.Object` interface
([[003-manifest-contract]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Both stores pass one suite over every method of every interface; the memory store is exempt only from durability across a restart and the schema check | `TestStoreSuite` over memory and Postgres in a test container | built for `Desired`, `Observed`, `Journal`, `Values` and `Leases` ([[043-postgres-store]], [[046-secret-kind]]); the seams have no accessor yet |
| Writes inside `Tx` commit together or not at all: a desired put plus a debit, and a desired put plus a count check, under a failure injected between them | `TestTxIsAtomic` | built ([[040-mesh-and-spawn]] closed the ledger half: a spawn's debit, the child's row and its record commit together) |
| `Put` with a stale version is `ErrVersionConflict`; with the current version it advances it | `TestOptimisticConcurrency` | built ([[043-postgres-store]]) |
| `(kind, owner, name)` is unique among live rows and reusable after delete | `TestNamesAreUniqueAmongLiveRows` | built ([[043-postgres-store]]) |
| `Rebuild` for one environment replaces only that environment's observed rows and touches no `objects` row; a desired sandbox with no observed counterpart is reported `Lost` and keeps its `Queued` or `Recovering` status | `TestRebuildIsScopedAndKeepsStatus` | built ([[043-postgres-store]]) |
| `Count` excludes `Deleting` and deleted rows | `TestCountExcludesDeleting` | built ([[043-postgres-store]]) |
| A plaintext value is returned by `Open` alone; its only caller in the tree is the control plane's compile path; both stores hold ciphertext; `Rewrap` under a new key leaves every ciphertext byte unchanged and `Open` still works | `TestValuesAreConfined`, `TestRewrapRotatesTheKey` | built ([[043-postgres-store]], [[046-secret-kind]]); the confinement test parses every non-test file and holds `Values.Open` and `Controlled.OpenValue` to one caller each |
| `Debit` at one remaining unit under contention yields one success; `Credit` restores it | `TestLedgerIsAtomic` | built ([[040-mesh-and-spawn]]), with the budget carried by the debit rather than held in the row: `Debit(parentID, budget)`, `Credit`, `Used` and `Forget`, and the count under eight racing debits |
| `Pending` returns one event per object, oldest first, and holds later events behind a deferred one; `Drop` after the retry window; `ByObject` pages newest first; `Prune` respects retention | `TestJournal` | `Append`, `ByObject` and `Prune` built ([[043-postgres-store]]); delivery waits for [[009-events]] |
| `Dequeue` orders by priority, fair share, arrival; capacity in use equals the sum over the named phases after a restart | `TestQueueOrder`, `TestCapacityIsDerived` | not built |
| `Claim` redelivers an operation whose claimer's heartbeat lapsed, exactly once to a live worker | `TestOperationsRedeliver` | not built |
| A schema ahead of the binary and a dirty migration each refuse to start naming the version | `TestSchemaGuards` | built ([[043-postgres-store]]) |
| Every list and count query in the index list uses its index | `TestQueriesUseIndexes` with `EXPLAIN` | not built |
| Two holders contend for one lease; one holds; the other acquires after the TTL lapses | `TestLeases` | built, with renewal at a third of the term and a release on Close ([[043-postgres-store]]) |
| No package outside `internal/store` imports the Postgres driver or the migrator | `TestDriverIsConfined` | built ([[043-postgres-store]]) |
