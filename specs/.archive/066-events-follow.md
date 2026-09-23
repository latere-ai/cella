---
title: "Following the events feed: one object's records from a cursor and then live, and every readable record from now, as newline-delimited JSON"
status: complete
track: core
depends_on:
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/055-api-contract-gaps.md
  - specs/.archive/062-journal-retention.md
affects: [internal/store/, internal/events/, internal/api/, cmd/cellad/, test/conformance/, api/openapi.yaml, docs/, CHANGELOG.md]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# Following the events feed

## Overview

[[009-events]] serves the journal on `GET /v1/events?object=` newest
first, paged by `seq`, "or from now on as newline-delimited JSON with
`follow=1`". [[055-api-contract-gaps]] built the pages and refused
`follow`, because a caller that asked to follow and was handed one page
would read the absence of later records as their absence. A client that
waits for a sandbox to reach a phase, and a console that keeps a live
list, poll the pages today.

This slice serves the following feed in two forms. With `object=`, the
stream replays that object's records after a cursor and then stays open
for new ones, with no gap and no duplicate between what the caller read
and what the stream sends. Without `object=`, the stream sends every
record the caller may read, from the moment it opens, filtered one
record at a time the way the list route filters one row at a time.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| The pages | `internal/api/events.go`, `Emitter.Feed`, `Journal.ByObject` on both adapters | nothing |
| `follow=1` | the same handler | refused with `capability_unsupported` |
| A read of one object's records after a sequence, oldest first | nowhere | `ByObject` reads newest first and only below a cursor |
| Knowing that a record was appended | nowhere | a reader has nothing to wait on but a timer |
| The route's negotiation | `h.handle`, which refuses an `Accept` naming neither JSON nor YAML | a follow client asking for `application/x-ndjson` is refused with 406 before the handler runs |
| Ending a long stream at shutdown | `http.Server.Shutdown` waits for active requests up to the 60 second grace | nothing tells a stream the server is stopping |
| `rate_limited` | [[008-api]]'s error table | `errorEnvelope` has no case for it, so the code would answer 503 with another code's sentence |
| The Postgres sequence after a prune | `Append` takes `max(seq) + 1` over the rows the table holds | a retention that pruned every row of an object restarts that object at `seq` 1, so a number is reused |

## Design

### The two forms

`GET /v1/events?follow=1&object=<id>&cursor=<seq>` follows one object.
The object is read and its kind's read is authorized exactly as the
pages do ([[009-events]]); the stream then sends the object's records
with `seq` above the cursor, oldest first, and stays open. Without a
cursor it starts from now: the object's newest record at the moment the
stream opens is the position, and only records after it are sent.

The cursor is the newest `seq` the caller already holds. That is the
inverse of a page's `next`, which names where the next older page
starts: a caller that read a page resumes from `items[0].seq`, not from
`next`. `cursor=0` replays every record the journal still holds.

`GET /v1/events?follow=1` without `object=` follows every record the
caller may read, from now. There is no cursor: `seq` counts within one
object, the journal has no position across objects, and a stream that
claimed to resume one would skip what it could not order. `cursor`
without `object` is `invalid_field` on `cursor`. A caller that needs the
history of one object reads its pages or follows it.

`follow=0` is a page and any value but `0` and `1` is `invalid_field`
on `follow`. `limit` bounds a page and is not read by a follow.

### No gap, no duplicate

The follower subscribes to the journal before it reads anything, then
reads the records after its position from the store, then takes what
the subscription delivers. A record committed while the history was
being read arrives twice, from the read and from the subscription, and
the second copy is dropped because its `seq` is not above the position.
Nothing committed after the subscription can be missed, because the
subscription existed before the read began.

A record the subscription hands over is sent only when its `seq` is
exactly one above the position. Anything else, a later `seq` that
overtook an earlier one or a subscription that fell behind, sends the
follower back to the store for the records after its position, which
come back in `seq` order. `seq` is dense and assigned in commit order
within an object on both adapters ([[010-state]]), so reading after the
position is complete by construction.

### Waking up

The store publishes every journal row a transaction appended once the
transaction commits, to the subscriptions of this process, with a
non-blocking send into a bounded buffer per subscription. The memory
adapter publishes while it still holds the lock the transaction ran
under, so rows arrive in commit order. The Postgres adapter publishes
after `Commit` returns, where two transactions of one object can
publish in the other order; the `seq` rule above restores the order. A
subscription whose buffer is full is closed as behind rather than
blocking a commit.

A subscription is for one object or for every object. The object form
of a follower on a store that outlives the process, which is Postgres,
also reads the records after its position once a second, so a record
another replica appended arrives within that interval.

| Deployment | Object form | Every-object form |
|---|---|---|
| Memory store, the one process that holds the state | complete, at the commit | complete, at the commit |
| Postgres, one replica, which is what `deploy/` ships | complete, at the commit | complete, at the commit |
| Postgres, more than one replica | complete: this replica's records at the commit, another's within a second | this replica's records only |

### A position the journal no longer holds

The retention of [[062-journal-retention]] prunes finished records older
than `CELLA_JOURNAL_RETENTION`, and the memory ring keeps the newest
`CELLA_JOURNAL_CAP` records per object. When the record after the
position is gone and a later one is held, the records between are lost
to this caller, and the answer is an error the caller acts on rather
than a stream that silently skips them.

At open the follow is refused before any header is sent: `cursor_expired`,
status 410, "The feed no longer holds the records after that position;
read it again from the newest." A cursor above the object's newest
`seq` names a record the object never had and is `invalid_field` on
`cursor`. During the stream, the same gap, which a follower that fell a
whole ring behind can meet, ends the stream with an error line carrying
`cursor_expired`. An every-object stream whose subscription fell behind
ends the same way, because the records it dropped cannot be read back.

The retention keeps each object's newest record whatever its age, on
both adapters. Postgres takes an object's next `seq` from the rows it
holds, so a prune that took every row of an object restarted it at 1,
and a follower at position 57 would wait forever for a record numbered
58 while the object wrote 1, 2 and 3. Keeping the newest record keeps
the sequence counting and keeps "the object's newest `seq`" readable
for the rule above.

### The stream

`Content-Type: application/x-ndjson`, with `Cache-Control: no-cache`
and `X-Accel-Buffering: no`. The status and headers are flushed at
open, so a stream that starts from now tells its caller it is
connected, and every line is flushed as it is written.

| Line | Meaning |
|---|---|
| a JSON object with `id`, `seq`, `type` | one record, byte for byte what a page carries |
| an empty line | a heartbeat, every 15 seconds without a record, so a proxy does not close an idle stream |
| a JSON object with an `error` member | the envelope of [[008-api]]'s error table; it is the last line and says why the stream ended |

The stream ends:

| When | How |
|---|---|
| the caller disconnects | the handler returns and releases its subscription |
| the server stops taking requests | every stream ends when the shutdown begins, so a shutdown does not wait out the grace period for them; the caller reconnects with its position |
| the bearer's `exp` passes | an error line with `unauthenticated`: a stream never outlives the token it was authorized under |
| the object's `*.deleted` record was sent, in the object form | the stream ends after it, which is [[008-api]]'s "the connection closes with the sandbox" |
| the position is gone, or the subscription fell behind | an error line with `cursor_expired` |
| the journal fails | an error line with the code the failure maps to |

A proxy in front of `cellad` must pass the response through unbuffered
and allow an idle read of more than 15 seconds.

### Who reads what

The object form authorizes once, at open, under the object's own kind,
as the pages do. The every-object form decides `sandbox.list` at open
and refuses with that decision's error. Then, per record, it applies
the list route's rule to the record's own object: the kind's list
decision and its filter on owner and labels, then the kind's read on a
resource built from the record, each decision asked once per kind or
per object and held for the stream. The resource is the record's and
never a read of the object, because the object a `sandbox.deleted`
record is about no longer exists. A record the caller may not read is
skipped, as the list route skips a row.

A record carries a sandbox's owner, name and labels and not its parent
or root, so a workload token's every-object stream carries its own
sandbox's records and not its children's; a workload follows a child
by the object form, which reads the child and decides on it whole.

### Limits

One process holds at most 256 following streams; the next is refused
with `rate_limited` (429, "Too many requests; wait and retry.") and the
code joins `errorEnvelope`'s table. A subscription buffers 256 rows. A
replay reads 200 records a statement. The heartbeat is 15 seconds and
the Postgres read is once a second. They are constants: `api.Options`
overrides the cap and the heartbeat for tests, and no `CELLA_*`
variable exists for any of them.

### The route

`GET /v1/events` moves from `h.handle` to `h.stream`, and the page
branch negotiates `Accept` itself as before, so a page answers JSON or
YAML and a follow answers `application/x-ndjson` whatever `Accept`
says. An API built with no emitter refuses `follow=1` with
`capability_unsupported`: a server that journals nothing has nothing to
follow, and an empty stream would read as a quiet one.

`cellad serve` hands the API a channel that closes when the public
server's shutdown begins.

## Not in this slice

The every-object form across replicas. It needs a position that orders
commits across processes, or Postgres `LISTEN`/`NOTIFY` on a session
connection (`CELLA_DB_URL`, never the pooled endpoint) with a reconnect
loop that ends every stream it could not keep whole. The history of the
every-object form, for the same reason. `GET /v1/sandboxes/{id}/events`,
which [[008-api]] names and no handler serves. A `CELLA_*` variable for
the cap, the heartbeat or the read interval. A follow flag on the
`cella` command, which has no events subcommand.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Both adapters publish the rows a transaction appended once it commits, with the sequence each took, nothing from a transaction that failed, only an object's rows to that object's subscription, and end every subscription at close; a read after a sequence is ascending and bounded | the `Follow` case of `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore`, `TestTheJournalFollowsWhatCommits` | built |
| A subscription whose buffer is full is closed as behind and never blocks the publisher | `TestBroadcastDropsASlowSubscriber` | built |
| The retention keeps each object's newest record, and the next append continues the sequence | the `Sequence` case of `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore`, `TestRetentionPrunesWhatIsDone` | built |
| A follower replays the records after its cursor and then the live ones, loses none appended during the replay, sends none twice, and starts from now without a cursor | `TestFollowReplaysThenStaysLive`, `TestFollowFromNow` | built |
| A follower sends records in sequence when they arrive out of order, and reads the store again after its subscription fell behind | `TestFollowRestoresTheSequence`, `TestFollowResubscribesWhenBehind` | built |
| A cursor whose next record was pruned is refused as expired, a cursor above the newest is refused as ahead, and a follower that falls behind the ring mid-stream ends as expired | `TestFollowRefusesAPositionItCannotServe` | built |
| The every-object follower sends each object's records from now and ends when it falls behind | `TestFollowAllFromNow` | built |
| A follower on a store another replica shares reads a record that replica appended | `TestAFollowerReadsAnotherReplicasAppend`, `TestFollowReadsASharedJournal` | built |
| `follow=1&object=` answers newline-delimited JSON with the headers above, flushed, replays from the cursor, and carries a live record before the stream ends | `TestFollowedFeed` | built |
| The refusals: a bad `follow`, a malformed cursor, a cursor above the newest, an expired cursor with 410 `cursor_expired`, a cursor without an object, no emitter, and the cap with 429 `rate_limited` | `TestFollowedFeedRefusals` | built |
| An idle stream carries a heartbeat line | `TestFollowedFeedHeartbeat` | built |
| The stream ends on the drain, on the caller's disconnect, at the bearer's expiry with an `unauthenticated` line, and after the object's deleted record | `TestFollowedFeedEnds`, `TestExpiryReadsEveryNumberForm` | built |
| The every-object stream carries the caller's records and not another owner's, and is refused without `sandbox.list`; each record passes the kind's list filter and the read on its own object, asked once per object, and an authorizer that fails ends the stream | `TestFollowedFeedOfEveryObject`, `TestRecordGateDecidesEachRecord` | built |
| On a running node, a followed feed carries an exec's record, and a stop ends the stream and the node before the grace period | `TestFollowedFeedEndToEnd` | built |
| The contract holds over HTTP against this server | conformance case `case009FollowFeed`, run by `TestTheConformanceSuiteHoldsAgainstThisServer` | built |

## Outcome

Both forms of the following feed are served, and the retention no longer
lets a Postgres object's sequence start over.

| Piece | Where |
|---|---|
| `Broadcast` and `Subscription`, published at commit | `internal/store/broadcast.go`; `memory.Store.Tx` under its lock, `postgres.Store.Tx` after `Commit` |
| `Store.Watch`, `Journal.After`, and `Prune` keeping each object's newest row | `internal/store/store.go`, both adapters |
| The journal's follow half: `After`, `Watch`, `Shared` | `internal/store/events.go`, declared on `events.Journal` |
| `Follower`, `Emitter.Follow`, `Emitter.FollowAll`, `Ends` | `internal/events/follow.go` |
| The route, the stream, the gate of every object's feed, `cursor_expired` and `rate_limited` in the error table | `internal/api/follow.go`, `internal/api/events.go`, `internal/api/api.go` |
| The public server's shutdown ending every feed | `cmd/cellad/main.go` (`Options.Draining`, `RegisterOnShutdown`) |
| `case009FollowFeed`, and `cursor_expired` in the suite's table | `test/conformance/` |
| The route's parameters and its two answers | `api/openapi.yaml` |
| The page for a caller | `docs/events.md` |

Coverage on `go test -cover`: `internal/store` 91.1%, memory 93.3%,
postgres 91.6%, `internal/events` 96.4%, `internal/api` 92.1%,
`cmd/cellad` 90.7%. The end-to-end that ran is
`TestFollowedFeedEndToEnd`: a node, a feed on a sandbox, an exec whose
record arrives on the open feed, and a stop that ends the feed and the
node in the drain delay rather than the 60 second grace period, which
the same test measured at 63 seconds without the shutdown hook. The
sequence case fails on Postgres without the retention change: the
append after a prune took the sequence 1.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[009-events]] | the API serves any object's events, or follows with `follow=1` | `follow=1` without `object` also follows every record the caller may read, from now | a console that keeps a live list follows one feed rather than one per object; it has no cursor because the journal orders nothing across objects |
| [[010-state]], [[062-journal-retention]] | the retention prunes finished records older than the window | it keeps each object's newest record whatever its age | Postgres takes an object's next `seq` from the rows it holds, so pruning every row restarted the object at 1 and reused numbers a reader held |
| [[008-api]] | the error table | `cursor_expired` (410) joins it | a position whose records are gone is neither the caller's malformed field nor a missing object, and the caller acts on it differently: it reads a page again and accepts the gap |

### What this leaves open

| Open | Why |
|---|---|
| The every-object feed across replicas | the journal has no position ordered across processes; Postgres `LISTEN`/`NOTIFY` on a session connection, with a reconnect that ends what it could not keep whole, would carry it. `deploy/` ships one replica, where it is complete |
| The history of the every-object feed | the same missing position |
| The memory journal after a restart | it starts every object's `seq` again at 1, as it loses every record; a cursor above the new newest is refused, and one below it is not told apart from a record of the same number |
| `GET /v1/sandboxes/{id}/events` | [[008-api]] names it and no handler serves it; `GET /v1/events?object=` serves the same records |
| A follow flag on the `cella` command | the command has no events subcommand |
| A workload token's every-object feed carrying its children | a record carries no parent or root; a workload follows a child by `object` |
