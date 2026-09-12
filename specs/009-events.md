---
title: "Events: one signed record per mutation and exec, to the operator's sink"
status: drafted
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
affects: [internal/events/, internal/config/, test/stubs/]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Events

## Overview

Every change to a sandbox and every command run in one produces one
event: who, what, which sandbox, when, why. The core delivers events to
one sink the operator names, signed so the sink can trust them, at
least once and in order per sandbox, and serves them back per sandbox
on the API. What the sink does with them, an audit trail, a usage
meter, an activity feed, is the operator's. The core keeps no opinion
and no output.

## Current state

Not built. The hosted plane wrote an audit stream to object storage
and derived activity and usage from it; the event shape here is the
part of that stream that describes the sandbox rather than the
platform.

## Design

### The record

```json
{
  "id": "evt_01J9...",
  "type": "sandbox.exec",
  "time": "2026-09-12T10:41:00.123Z",
  "sandbox": {"id": "01J9...", "name": "dev", "owner": "alice@example.com", "labels": {}},
  "subject": "alice@example.com",
  "workload": null,
  "request_id": "req_...",
  "reason": "",
  "data": {"command": ["make", "test"], "exit_code": 0, "duration_ms": 8123}
}
```

| Type | When | `data` |
|---|---|---|
| `sandbox.created` | `Create` accepted | the resolved manifest |
| `sandbox.updated` | an update applied | the changed paths and new values |
| `sandbox.started`, `.stopped`, `.deleted` | the transition completed | `reason`: `request`, `autoStop`, `ttl`, `deadline`, `autoDelete`, `lost`, `failed` |
| `sandbox.failed`, `sandbox.lost` | the controller observed it | the backend's condition |
| `sandbox.exec` | a command ended | command, exit code, duration; never stdin or output |
| `sandbox.attach` | a session ended | duration, bytes each way |
| `sandbox.files` | a transfer ended | direction, paths, bytes |
| `sandbox.token` | a token minted outside the projection | the token's `exp`; never the token |

`workload` is set when the subject is a sandbox. No event carries an
environment value, a token, or a byte of a sandbox's output.

### Delivery

`CELLA_EVENTS_URL` receives `POST` with `Content-Type: application/json`,
a body of one event, and `Cella-Signature: t=<unix>,v1=<hex HMAC-SHA256
of "<t>.<body>" under CELLA_EVENTS_SECRET>`. A 2xx acknowledges.
Anything else is retried with exponential backoff from 1 second to 5
minutes for 24 hours, then the event is dropped with a metric and a log
line. Events for one sandbox are delivered in order: a failing event
holds the ones behind it for that sandbox and no other. Delivery runs
from the journal of [[010-state]], so a restart resumes where it was;
without a store the journal is in memory and a restart loses what was
not yet acknowledged, which the log says at start.

### The API

`GET /v1/sandboxes/{id}/events` serves the sandbox's events from the
journal, newest first, so a caller reads the history without a sink.
With no store the history is the process's lifetime.

## Not in this spec

The stub sink ([[012-test-stubs-and-tiers]]); the journal table
([[010-state]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every type in the table is emitted by the action in its row with the `data` named | `TestEventTable` | not built |
| No event body contains an env value, a token, or exec output | `TestEventsCarryNoSecrets` with canary strings | not built |
| The signature verifies with the documented formula and a body changed by one byte does not | `TestSignature` | not built |
| A sink that fails three times receives the event on the fourth try and the sandbox's later events after it, in order | `TestDeliveryIsOrderedPerSandbox` | not built |
| A restart with a store resumes delivery of an unacknowledged event | `TestDeliveryResumesFromTheJournal` | not built |
