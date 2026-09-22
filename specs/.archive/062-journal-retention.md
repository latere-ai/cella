---
title: "Journal retention: the reaper prunes finished records and answered operations, and the memory journal keeps a ring per object"
status: complete
track: core
depends_on:
  - specs/010-state.md
  - specs/009-events.md
  - specs/005-lifecycle-controller.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/042-events.md
  - specs/.archive/043-postgres-store.md
affects: [internal/store/, internal/config/, controller/, cmd/cellad/, CHANGELOG.md]
effort: small
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Journal retention

## Overview

[[010-state]] bounds the event journal two ways. With Postgres,
acknowledged and dropped records older than `CELLA_JOURNAL_RETENTION`
(default `720h`) are pruned on the reaper's tick. With the memory
store, each object keeps a ring of its last `CELLA_JOURNAL_CAP` records
(default `1000`). Neither is wired: `Journal.Prune` exists on both
adapters and nothing calls it, which [[042-events]] recorded as left
open, and the memory journal appends without a bound. The operation
records every exec and file act writes are the high-volume ones, so
the journal grows for as long as the process or the database lives,
and the memory store copies all of it on every transaction.

This slice wires both bounds, and prunes the worker operations that
were answered, which [[021-data-plane-workers]] keeps in the same
store, under the same window.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `Journal.Prune(before)`, finished rows only | both adapters, proved by the storetest `Journal` case | no caller |
| `Operations.Prune(before)`, done operations only | both adapters, proved by the storetest `Operations` case | no caller |
| `CELLA_JOURNAL_RETENTION`, `CELLA_JOURNAL_CAP` | named in the configuration table of [[002-repository-scaffold]] | not read |
| The memory journal | `internal/store/memory` | appends without a bound |
| The reaper's tick | `controller.Reap`, which sweeps the revocation list once a tick | prunes nothing else |

## Design

### Retention

`store.Retention` holds a store and a window. `Prune(ctx, now)` runs
one transaction that forgets the journal's finished records and the
answered operations created before `now` minus the window, and reports
how many went. A record still waiting for the sink is never pruned:
[[009-events]] decides when delivery gives up, not the retention.

The controller declares the seam it calls, `controller.Retention`, and
`Options.Retention` carries it. `Reap` calls it once a tick, after the
revocation sweep and under the same `reaper` lease, so one replica
prunes. A prune that fails is reported with the tick's other failures
and holds nothing else. `cellad serve` passes a `store.Retention` over
the store the journal lives in, whichever adapter that is.

### The memory ring

`memory.Options.JournalCap` bounds each object's records. An append
past the cap drops that object's oldest record, acknowledged or not,
which is the loss [[009-events]] names for a store that does not outlive
the process. Zero is no cap. `cellad serve` passes `CELLA_JOURNAL_CAP`.
The sequence an object's records carry keeps counting past a dropped
one, so the feed pages by the same `seq` and a reader never sees a
number reused.

### Configuration

| Variable | Default | Bounds |
|---|---|---|
| `CELLA_JOURNAL_RETENTION` | `720h` | at least `1h` |
| `CELLA_JOURNAL_CAP` | `1000` | 1 to 1000000 |

## Not in this slice

A phase change a driver makes on its own, such as a main process that
exits, producing a record: [[042-events]] recorded it beside the
retention and it is the observation loop's. A retention per record
kind. A start-up line that names the memory journal's loss, which the
configuration table and this page already carry.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A prune forgets the finished records and the answered operations older than the window, keeps an undelivered record and a queued operation of any age, and reports the count | `TestRetentionPrunesWhatIsDone`, `TestRetentionReportsAFailedPrune` | built |
| The reaper prunes once a tick under its lease, and a prune that fails is reported with the tick and stops nothing else | `TestReapPrunesTheJournal`, `TestReapReportsAFailedPrune` | built |
| The memory journal keeps the newest records of an object up to the cap, keeps counting the sequence past a dropped one, and bounds nothing at zero | `TestTheMemoryJournalKeepsARing` | built |
| `CELLA_JOURNAL_RETENTION` and `CELLA_JOURNAL_CAP` are read with their defaults and refused outside their bounds | `TestLoadJournalBounds` | built |
| `cellad serve` bounds the memory journal by `CELLA_JOURNAL_CAP`, read back through the per-object feed | `TestJournalRetentionEndToEnd` | built |

## Outcome

Both bounds of the journal are wired.

| Piece | Where |
|---|---|
| `store.Retention`, one transaction over the journal and the operations | `internal/store/retention.go` |
| The memory journal's ring per object | `internal/store/memory/memory.go` (`Options.JournalCap`) |
| `controller.Retention` and its call on the reaper's tick | `controller/reaper.go` |
| `CELLA_JOURNAL_RETENTION`, `CELLA_JOURNAL_CAP` | `internal/config/events.go` |
| The retention over the journal's store, and the cap on the memory journal | `cmd/cellad/main.go` |

Coverage on `go test -cover`: `internal/store` 90.1%, memory 93.3%,
`internal/config` 91.5%, `controller` 91.0%, `cmd/cellad` 90.4%. The
end-to-end that ran is `TestJournalRetentionEndToEnd`: a node with a
cap of three, a sandbox with a create and five execs behind it, and a
feed that reads its newest three in sequence.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[010-state]] | retention applies to the Postgres journal | it applies to the store the journal lives in, memory included | the memory adapter's `Prune` is the same contract, and a long-lived process without a database otherwise holds every finished record until the ring evicts it |
| [[010-state]] | a retention for the journal | the same window prunes answered worker operations | [[021-data-plane-workers]] names no window of its own, and an answered operation is read by nothing once its result was taken |

### What this leaves open

| Open | Why |
|---|---|
| The end-to-end proof of the retention's wiring | a record cannot be older than the one-hour floor inside a test; the prune and its tick are proved at the store and at the reaper, and `cellad serve` hands the reaper the journal's own store |
| A record for a phase change the driver made on its own | the observation loop's, not the journal's |
