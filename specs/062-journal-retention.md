---
title: "Journal retention: the reaper prunes finished records and answered operations, and the memory journal keeps a ring per object"
status: in-progress
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
| A prune forgets the finished records and the answered operations older than the window, keeps an undelivered record and a queued operation of any age, and reports the count | `TestRetentionPrunesWhatIsDone` | not built |
| The reaper prunes once a tick under its lease, and a prune that fails is reported with the tick and stops nothing else | `TestReapPrunesTheJournal`, `TestReapReportsAFailedPrune` | not built |
| The memory journal keeps the newest records of an object up to the cap, keeps counting the sequence past a dropped one, and bounds nothing at zero | `TestTheMemoryJournalKeepsARing` | not built |
| `CELLA_JOURNAL_RETENTION` and `CELLA_JOURNAL_CAP` are read with their defaults and refused outside their bounds | `TestLoadJournalBounds` | not built |
| `cellad serve` wires both: an acknowledged record older than the window is gone after a reaper tick | `TestJournalRetentionEndToEnd` | not built |
