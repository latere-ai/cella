---
title: "Events: one signed record per mutation and operation, typed, ordered per object, to the operator's sink"
status: validated
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/010-state.md
affects: [internal/events/, internal/api/, internal/config/, test/stubs/]
effort: small
created: 2026-09-12
updated: 2026-09-13
author: changkun
---

# Events

## Overview

Every change to an object and every operation on a sandbox produces
one event: who, what, which object, when, why. The control plane
delivers events to one sink the operator names, signed so the sink can
trust them, at least once and in order per object, and serves them
back per object on the API. What the sink does with them, an audit
trail, a usage meter, an activity feed, is the operator's. This spec
fixes the record, the event and reason vocabularies the other specs
emit, the delivery contract over the journal of [[010-state]], the
signature, and what an event never carries.

## Current state

Not built. The hosted platform wrote an audit stream to object storage
and derived activity and usage from it; the event shape here is the
part of that stream that describes the objects rather than the
platform. `latere.ai/x/pkg/audit` exists and is not adopted for the
envelope: its `Subject` names the resource acted on where this spec's
`subject` names the actor, its `Category` is an open string where the
type here is a closed enum a test checks, and its `OrgID` and `Policy`
are platform concepts the control plane pushes out. Its `RedactJSON` is
used over `data` as a second line behind the structural strip below.

## Design

### The record

```go
// Event is one record. Every field but Data and Sandbox is always set.
type Event struct {
	ID        string          `json:"id"`        // evt_ ULID
	Seq       int64           `json:"seq"`       // per object, monotonic, from the journal
	Type      Type            `json:"type"`      // the closed enum below
	Time      time.Time       `json:"time"`      // when the change happened
	Object    Object          `json:"object"`    // what the event is about
	Sandbox   *Object         `json:"sandbox,omitempty"` // the sandbox in context, for an event about another kind (volume.attached)
	Subject   string          `json:"subject"`   // 006's rendered subject; sandbox:sbx_... for a workload; "controller" for the reaper and the scheduler
	Workload  *v1.WorkloadRef `json:"workload,omitempty"` // 006's {id, parent, root, environment, mesh, spawn} when the subject is a sandbox
	RequestID string          `json:"requestId"` // 008's req_ id; empty for the controller's own acts
	Reason    Reason          `json:"reason,omitempty"` // the closed enum below, on transitions
	Data      json.RawMessage `json:"data,omitempty"`   // the per-type struct below, redacted
}

type Object struct {
	Kind  string `json:"kind"`  // Sandbox, Secret, Volume, SandboxSet, Environment
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// Sink is what internal/events delivers to; the stub of 012 and the
// operator's endpoint both implement the wire below.
type Sink interface {
	Deliver(ctx context.Context, e Event) error
}
```

### Types and their data

The type set is closed; a test reads every spec and asserts that each
type here is named as an emission point by the spec that owns the act,
and that every emission point a spec names is here.

| Type | When | `data` | Owner |
|---|---|---|---|
| `sandbox.created` | `Create` accepted | the resolved manifest with `spec.env` reduced to its keys and `spec.secrets[]` to names | 005 |
| `sandbox.updated` | an update applied | `{paths: []string}`, the changed paths | 005 |
| `sandbox.started`, `.stopped`, `.deleted`, `.failed`, `.lost` | the transition completed | `{phase}`; `Reason` set | 005 |
| `sandbox.recovered` | a lost sandbox recreated | `{workspace: "kept" or "recreated", volumes: []string}` | 005 |
| `sandbox.spawned` | a workload created a child | `{child: sbx_..., budgetLeft}` | 022 |
| `sandbox.exec` | a command ended | `{command: []string, exitCode, durationMs}`; never stdin or output | 008 |
| `sandbox.attach`, `sandbox.dial`, `sandbox.screen` | a session ended | `{durationMs, bytesIn, bytesOut}` | 008, 023 |
| `sandbox.files` | a transfer ended | `{direction, paths: []string, bytes}` | 008 |
| `sandbox.input`, `sandbox.screenshot` | an operation ended | `{events: n}`; `{width, height, format}`; never text or frame bytes | 023 |
| `sandbox.port` | a proxied request ended, only with `CELLA_EVENTS_PORTS=1` | `{port, method, path, status, durationMs}`; no query string | 008 |
| `sandbox.token` | a token minted outside the projection | `{exp}`; never the token | 006 |
| `sandbox.egress` | a connection the gateway handled, only with `CELLA_EVENTS_EGRESS=1` | the record of [[018-egress-and-secrets]] | 018 |
| `secret.created`, `.updated`, `.deleted` | the `Secret` kind | `{version, hosts: []string}`; never the value | 018 |
| `volume.created`, `.updated`, `.attached`, `.detached`, `.snapshotted`, `.failed`, `.deleted` | the `Volume` kind | `{mode}` with `Sandbox` set for attach and detach; `{snapshot: snp_...}`; `Reason` for `failed` | 019 |
| `set.created`, `.replica`, `.collect_failed`, `.completed`, `.stopped`, `.deleted` | the `SandboxSet` kind | `{counts}`; `{index, sandbox, phase, exitCode}` for `replica` and `collect_failed` | 020 |
| `environment.created`, `.updated`, `.registered`, `.offline`, `.keyed`, `.key_revoked`, `.deleted` | the `Environment` kind | `{workers}`; `{jti}` for the key events | 021 |

Every terminal transition of [[005-lifecycle-controller]] carries one
`Reason` from one enum, written in one case:

`Request`, `AutoStop`, `AutoDelete`, `Expired`, `Exited`, `Lost`,
`Parent`, `Preempted`, `CreateFailed`, `NoCapacity`, `StartDeadline`,
`DriverFailed`, `OOMKilled`, `RecoveryExhausted`, `VolumeMissing`,
`VolumeBusy`, `SourceUnreachable`, `SourceTooLarge`,
`SourceDigestMismatch`, `SourceUnsupported`, `ClassUnavailable`.

A refused apply and a denied action are not mutations and emit no
event; the request log and the metrics of [[017-observability]] carry
them, and an operator building an audit trail of refusals reads those.

### What an event never carries

An environment variable's value, a secret's value, a placeholder, the
gateway credential, a token, exec output or input, attach bytes, a
screenshot or any frame bytes, typed input text, a query string, or a
request or response header. The structural rule is the `data` table;
`audit.RedactJSON` runs over every `data` before it is stored, and the
canary test asserts both.

### Delivery

`CELLA_EVENTS_URL` receives `POST` with `Content-Type:
application/json`, a body of one event as the journal stored it, byte
for byte, and

```
Cella-Signature: t=<unix seconds of this attempt>,v1=<hex HMAC-SHA256 of "<t>.<body>" under the first secret>[,v1=<the same under the second>]
```

`t` is the time of the delivery attempt, not the event's `time`, and
the signature is recomputed on every attempt, so a retry days later
still passes the sink's freshness window of five minutes
([[013-security-and-threat-model]]). `CELLA_EVENTS_SECRET` holds one
secret or two separated by a comma; with two, both signatures are
sent, which is how a secret is rotated without an outage.

Delivery runs on the replica holding the `journal` lease
([[010-state]]), in a loop that takes `Pending` in batches of 64, at
most one event per object, and delivers up to 16 objects
concurrently, so a slow object never holds another. A 2xx is
`Acknowledge`. A 408, a 429, a 5xx, a connection failure, or a timeout
of `CELLA_EVENTS_TIMEOUT` (default `10s`) is `Defer` with exponential
backoff from 1 second to 5 minutes; after 24 hours of attempts the
event is `Drop`ped, `cella_events_dropped_total` moves, and a log line
names it. Any other 4xx is an immediate `Drop` with the same metric,
because a sink that refuses the body will refuse it tomorrow. Events
for one object are delivered in `seq` order: a deferred event holds
the ones behind it for that object and no other. `cella_events_pending`
is the journal's unacknowledged count. With Postgres a restart resumes
where it was; without, the memory ring of `CELLA_JOURNAL_CAP` per
object loses whatever was not yet acknowledged at process end, which
the start-up log says. `CELLA_EVENTS_URL` without `CELLA_EVENTS_SECRET`
is a start-up failure; a non-loopback `http://` sink is refused unless
`CELLA_EVENTS_INSECURE_SINK=1`, which the stubs set and no deployment
does. With `CELLA_EVENTS_EGRESS=1` or `CELLA_EVENTS_PORTS=1`, those
records share each object's memory ring and can evict lifecycle events
there; with Postgres nothing is evicted, and the flags are an
operator's choice made knowing that.

### The API

`GET /v1/events?object=<id>&cursor=&limit=&follow=1` ([[008-api]])
serves any object's events from the journal, newest first and paged by
`seq`, or from now on as newline-delimited JSON with `follow=1`; the
handler reads the object by id, derives its kind, and authorizes
`<kind>.read`. `GET /v1/sandboxes/{id}/events` is the same for a
sandbox. Without a store the history is the last `CELLA_JOURNAL_CAP`
events per object.

## Not in this spec

The stub sink and its flags ([[012-test-stubs-and-tiers]]); the
journal's columns ([[010-state]]); the routes' envelope ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every type in the table is named as an emission point by its owning spec and every emission point in the specs is in the table | `TestEventTableMatchesTheSpecs`, reading `specs/` | not built |
| Every type is emitted by the act in its row with the `data` named, `Object.Kind` right per kind, `Sandbox` set for attach and detach, and one `Reason` from the enum on every terminal transition | `TestEventTable`, table-driven | not built |
| No event body contains an env value, a secret value, a placeholder, a credential, a token, exec output, frame bytes, input text, a query string, or a header | `TestEventsCarryNoSecrets` with canary strings through every emission point | not built |
| The signature verifies with the documented formula for each of two secrets, is recomputed with a fresh `t` on a retry, and a body changed by one byte does not verify | `TestSignature`, and [[012-test-stubs-and-tiers]]'s `TestSinkVerifiesSignature` | not built |
| A sink failing three times receives the event on the fourth try and the object's later events after it, in `seq` order; another object's events flow meanwhile; a 400 drops at once; a 24-hour-old event drops with the metric | `TestDeliveryIsOrderedPerObject`, `TestDropRules` under a fake clock | not built |
| Delivery runs only on the lease holder, in batches with bounded concurrency | `TestDeliveryNeedsTheLease` | not built |
| A restart with Postgres resumes delivery of an unacknowledged event; without, the ring holds the cap and unacknowledged events are gone | `TestDeliveryResumesFromTheJournal`, `TestMemoryRing` | not built |
| A URL without a secret and a non-loopback `http://` sink are start-up failures; the escape hatch admits the stub | `TestSinkStartupRules` | not built |
| `GET /v1/events?object=` serves each kind's events newest first, authorizes the kind's read, pages by `seq`, and follows | `TestEventsRoute` | not built |
