---
title: "Ended sandboxes stay readable: a deleted sandbox's spec, status and events answered by its id for a retention window, and the tail of its main process output kept with it"
status: drafted
track: core
depends_on:
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
affects: [internal/store/, internal/api/, controller/, runtime/, api/openapi.yaml, client/, test/conformance/, docs/, CHANGELOG.md]
effort: medium
created: 2026-10-22
updated: 2026-10-22
author: changkun
---

# Ended sandboxes stay readable

## Overview

A sandbox that is deleted cannot be read again. The store keeps its row
with `deleted_at` set ([[010-state]]), and the journal keeps its events
for `CELLA_JOURNAL_RETENTION`, but every read filters `deleted_at is
null`, the controller forgets the object, and `GET /v1/sandboxes/{id}`,
`GET /v1/sandboxes/{id}/events` and `GET /v1/sandboxes/{id}/logs`
answer `404 not_found`. Logs are served live by the driver and are gone
with the sandbox.

Many sandboxes end by design: an agent's session deletes its sandbox
when it ends, and a grader's check sandbox is deleted once its checks
have run. The person who started that work then has nothing to open.
What the sandbox was made from, what happened to it and what its main
process last printed are exactly what they need to read. Today a
platform keeps its own copy of the events to make up for this, and it
has no copy of the spec or the output.

This spec keeps a deleted sandbox readable, read-only, for a retention
window: its spec, its last status, its events and the tail of its main
process output.

## Design

### What a read of a deleted sandbox answers

`GET /v1/sandboxes/{id}?deleted=1` answers a deleted sandbox by its id:
the object as it was last written, its spec without the values of
`spec.env` (a delete already drops them, [[010-state]]), its last
status, and `status.deletedAt`, the time of the delete. It is authorized
as `sandbox.read` with the object's own labels and owner, as a read of a
live one is, so whoever read the sandbox while it ran reads it after.
Without `deleted=1` the read answers `404 not_found` as today, so no
client that reads a live sandbox meets a deleted one by surprise.

A name is never a key of a deleted sandbox: names are reused after a
delete, ids never ([[010-state]]). A list never includes a deleted
sandbox.

### Its events

`GET /v1/sandboxes/{id}/events` answers a deleted sandbox's events, as
the journal holds them, while it holds them: newest first, paged by
`seq`, authorized as above. The journal's retention is unchanged; an
object's newest row is kept past it, so the feed of a deleted sandbox
always ends with its `sandbox.deleted`.

### The tail of its output

At delete, before the driver removes the workload, the controller reads
the last `CELLA_KEEP_LOG_BYTES` (default 64 KiB, at most 1 MiB) of the
main process's output, as `GET .../logs?tail=` would, and stores it with
the row. `GET /v1/sandboxes/{id}/logs?deleted=1` answers it as
`text/plain`, never followed. A driver that cannot read the output at
delete stores none, and the read answers `404 not_found` naming that no
output was kept. The kept output is a credential's possible place: it
is sealed under `CELLA_SECRET_KEY` as the row's other sealed values
are, and dropped with the row.

### Retention

`CELLA_KEEP_DELETED` (default `720h`, the journal's default) is how long
a deleted sandbox stays readable. The reaper's tick removes the rows of
sandboxes deleted longer ago, with their kept output, the last step of
a delete. `0` keeps nothing: a delete then removes the row as before
this spec, and every `deleted=1` read answers `404 not_found`.

### Events

Nothing new is emitted. `sandbox.deleted` already names the object; its
payload gains `retainedUntil` when the sandbox stays readable, so a
consumer such as a platform's console can say until when.

### What it costs

One row per deleted sandbox for the window, which the store already
keeps today without a read, and at most `CELLA_KEEP_LOG_BYTES` beside
it. The count ceiling ([[007-admission]]) counts live sandboxes only,
unchanged.

## Acceptance criteria

1. A deleted sandbox reads by its id with `deleted=1` for
   `CELLA_KEEP_DELETED`, with its spec, last status and `deletedAt`, and
   no `spec.env` value; without the flag it is `404 not_found`.
2. Its events read by its id while the journal holds them, ending with
   `sandbox.deleted`.
3. The tail of its main process output reads by its id with `deleted=1`,
   sealed at rest under `CELLA_SECRET_KEY`, at most
   `CELLA_KEEP_LOG_BYTES`.
4. Each read is `sandbox.read` with the object's labels and owner.
5. The reaper removes a deleted sandbox's row and output after the
   window; `CELLA_KEEP_DELETED=0` keeps none.
6. `sandbox.deleted` names `retainedUntil` when the sandbox stays
   readable.
7. The conformance suite holds 1 to 4 against every store.

## Open questions

1. Should a deleted sandbox's workspace files be kept too? This spec
   keeps none: a session already keeps its working directory in its own
   files store, and a workspace can be large.
2. Should the window be a platform's choice per tenant? This spec makes
   it one value for the installation.
