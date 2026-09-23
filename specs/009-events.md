---
title: "Events: one signed record per mutation and operation, typed, ordered per object, to the operator's sink"
status: in-progress
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/010-state.md
affects: [internal/events/, internal/api/, internal/config/, test/stubs/]
effort: small
created: 2026-09-12
updated: 2026-09-23
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
	Seq       int64           `json:"seq"`       // per object, monotonic, from the journal, on every record including an operation
	Type      Type            `json:"type"`      // the closed enum below
	Time      time.Time       `json:"time"`      // when the change happened
	Object    Object          `json:"object"`    // what the event is about
	Sandbox   *Object         `json:"sandbox,omitempty"` // the sandbox in context: an event about another kind (volume.attached), and every operation
	Subject   string          `json:"subject"`   // 006's rendered subject; sandbox:sbx_... for a workload; "controller" for the reaper and the scheduler
	Workload  *Workload       `json:"workload,omitempty"` // the sandbox a workload token names
	RequestID string          `json:"requestId"` // 008's req_ id; empty for the controller's own acts
	Reason    Reason          `json:"reason,omitempty"` // the closed enum below, on transitions
	Data      json.RawMessage `json:"data,omitempty"`   // the per-type struct below, redacted
}

type Object struct {
	Kind   string            `json:"kind"`  // Sandbox, Secret, Volume, SandboxSet, Environment
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	Labels map[string]string `json:"labels,omitempty"` // the labels a plane stamped on the object
}

// Workload is the sandbox whose own token made the call. 006's full
// {id, parent, root, environment, mesh, spawn} reference arrives with the
// tokens of slice 045; the id is the part that exists, and the object leaves
// room for the rest without a wire break.
type Workload struct {
	ID string `json:"id"`
}

// Sink is what internal/events delivers to; the stub of 012 and the
// operator's endpoint both implement the wire below.
type Sink interface {
	Deliver(ctx context.Context, e Event) error
}
```

`Object.Labels` and the labels of the sandbox in context travel on every
record. A plane files a record under the tenant its labels name and under
nothing else, so a record that arrived without them would belong to no
tenant and be read by none. `Seq` is on every record, operations included:
the per-object feed and every fold over the stream order by it.

`subject`, `workload` and `requestId` are flat fields and not an actor
object. A sink decodes the JSON above and nothing else, so the field names
here are the contract.

### Types and their data

The type set is closed; a test reads every spec and asserts that each
type here is named as an emission point by the spec that owns the act,
and that every emission point a spec names is here.

| Type | When | `data` | Owner |
|---|---|---|---|
| `sandbox.created` | `Create` accepted | the resolved manifest with `spec.env` reduced to its keys and `spec.secrets[]` to names | 005 |
| `sandbox.updated` | an update applied | `{paths: []string}`, the changed paths | 005 |
| `sandbox.started`, `.stopped`, `.deleted`, `.failed`, `.lost` | the transition completed, `started` included where a create's first driver read finds the sandbox running | `{phase}`; `Reason` set | 005 |
| `sandbox.recovered` | a lost sandbox recreated | `{workspace: "kept" or "recreated", volumes: []string}` | 005 |
| `sandbox.spawned` | a workload created a child | `{child: sbx_..., budgetLeft}` | 022 |
| `sandbox.exec` | a command ended | `{exitCode, durationMs}`; never the command, its input or its output | 008 |
| `sandbox.attach`, `sandbox.dial`, `sandbox.screen` | a session ended | `{durationMs, bytesIn, bytesOut}` | 008, 023 |
| `sandbox.files` | a transfer or a file operation ended | `{operation, direction, paths: []string, bytes}`; `operation` is `import`, `export`, `read`, `write`, `stat`, `list`, `mkdir`, `remove` or `move`, and `direction` is on a transfer only ([[033-file-operations]]) | 008 |
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
gateway credential, a token, the command line of an exec, exec output or
input, attach bytes, a screenshot or any frame bytes, typed input text, a
query string, or a request or response header. The command line is here
because it is where a secret reaches a process: a record that cannot hold
the string needs no scan to prove it does not. The manifest's own
`spec.command` stays in the `sandbox.created` record, because that is the
object's declared state and a read of the sandbox already serves it. The structural rule is the `data` table;
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
backoff from 1 second to 5 minutes; after `CELLA_EVENTS_RETRY_WINDOW`
(default `24h`) of attempts the event is `Drop`ped, `cella_events_dropped_total` moves, and a log line
names it. A 401 is `Defer`red with the same backoff and logged at error:
it is the one 4xx that says nothing about the bytes, because it says the
two ends hold different secrets, and dropping would discard every record
emitted during a botched rotation. Any other 4xx is an immediate `Drop`
with the same metric, because a sink that refuses the body will refuse it
tomorrow. Events
for one object are delivered in `seq` order: a deferred event holds
the ones behind it for that object and no other. `cella_events_pending`
is the journal's unacknowledged count. With Postgres a restart resumes
where it was; without, the memory ring of `CELLA_JOURNAL_CAP` per
object loses whatever was not yet acknowledged at process end, which
the start-up log says. `CELLA_EVENTS_URL` without `CELLA_EVENTS_SECRET`
is a start-up failure, and so is a secret without the URL, which is a
deployment that believes it delivers; with no URL a record is journaled
acknowledged, so the per-object feed still reads it and retention forgets
it on schedule rather than holding a row nothing will ever post; a non-loopback `http://` sink is refused unless
`CELLA_EVENTS_INSECURE_SINK=1`, which the stubs set and no deployment
does. With `CELLA_EVENTS_EGRESS=1` or `CELLA_EVENTS_PORTS=1`, those
records share each object's memory ring and can evict lifecycle events
there; with Postgres nothing is evicted, and the flags are an
operator's choice made knowing that.

### The API

`GET /v1/events?object=<id>&cursor=&limit=&follow=1` ([[008-api]])
serves any object's events from the journal, newest first and paged by
`seq`, or with `follow=1` as newline-delimited JSON: the records after
`cursor`, the newest `seq` the caller holds, and then each one as it
commits, or from now without a cursor; the handler reads the object by
id, derives its kind, and authorizes `<kind>.read`. `follow=1` without
`object` carries every record the caller may read from now, each decided
as the list route decides a row ([[066-events-follow]]). `GET /v1/sandboxes/{id}/events` is the same for a
sandbox. Without a store the history is the last `CELLA_JOURNAL_CAP`
events per object.

## Not in this spec

The stub sink and its flags ([[012-test-stubs-and-tiers]]); the
journal's columns ([[010-state]]); the routes' envelope ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every type in the table is named as an emission point by its owning spec and every emission point in the specs is in the table | `TestEventTableMatchesTheSpecs`, reading `specs/` | not built: the types of 018 to 023 wait on the slices that emit them |
| Every type is emitted by the act in its row with the `data` named, `Object.Kind` right per kind, `Sandbox` set for attach and detach, and one `Reason` from the enum on every terminal transition | `TestRecordShapes`, `TestRecordCarriesLabelsAndSeqEverywhere`, table-driven | built for the `Sandbox` types, as `TestRecordShapes` and `TestRecordCarriesLabelsAndSeqEverywhere` ([[042-events]]); the scheduler's `Preempted`, `NoCapacity` and `StartDeadline` joined the enum with [[058-preemption]], as `TestReasonOfHoldsTheEnum`, so a terminal record of the scheduler's no longer reads `DriverFailed` |
| No event body contains an env value, a secret value, a placeholder, a credential, a token, exec output, frame bytes, input text, a query string, or a header | `TestNoContentInEvents` with canary strings through every emission point | built as `TestNoContentInEvents` over a whole `cellad` session ([[042-events]]) |
| The signature verifies with the documented formula for each of two secrets, is recomputed with a fresh `t` on a retry, and a body changed by one byte does not verify | `TestSignatureFormula`, `TestSignatureVerifies`, `TestSignatureRejects`, `TestSignatureIsFreshOnEveryAttempt`, and [[012-test-stubs-and-tiers]]'s `TestTheSinkVerifiesWhatTheDelivererSigns` | built as `TestSignatureVerifies`, `TestSignatureRejects`, `TestSignatureIsFreshOnEveryAttempt` and `TestSignatureFormula` ([[042-events]]) |
| A sink failing three times receives the event on the fourth try and the object's later events after it, in `seq` order; another object's events flow meanwhile; a 400 drops at once; a 401 is held; a record past the retry window drops and is counted | `TestDeliveryIsOrderedPerObject`, `TestDeliveryRetries`, `TestDropRules` under a fake clock | built ([[042-events]]) |
| Delivery runs only on the lease holder, in batches with bounded concurrency | `TestDeliveryNeedsTheLease` | built ([[042-events]]) |
| The record commits with the mutation it explains, and one object's records keep their order under concurrent mutations | `TestRecordCommitsWithTheMutation`, `TestOrderUnderConcurrentMutations` | built ([[042-events]]) |
| A restart with Postgres resumes delivery of an unacknowledged event; without, the ring holds the cap and unacknowledged events are gone | the `Delivery` case of `TestSuiteHoldsTheMemoryAdapter` and `TestPostgresStore`; the memory ring's cap | the Postgres row survives a restart and the delivery columns are proved over both adapters by the store suite; the memory ring's cap is not built ([[042-events]]) |
| A URL without a secret, a secret without a URL, and a non-loopback `http://` sink are start-up failures; the escape hatch admits the stub | `TestSinkStartupRules` | built ([[042-events]]) |
| `GET /v1/events?object=` serves each kind's events newest first, authorizes the kind's read, pages by `seq`, and follows | `TestObjectFeed`, `TestObjectFeedRefusals`, `TestObjectFeedAuthorizesTheObjectsKind`, `TestTheFeedReadsAnyEnvironment`, `TestFollowedFeed`, `TestFollowedFeedRefusals` | built: the route serves one object's records newest first, pages by the sequence the journal assigned, and authorizes the kind the id names, `TestObjectFeed`, `TestObjectFeedRefusals` and `TestObjectFeedAuthorizesTheObjectsKind`, with every environment the control plane holds read by its name, `TestTheFeedReadsAnyEnvironment` with the read half of the journal as `TestFeedReadsOneObjectNewestFirst` and `TestByObjectRebuildsTheRecord`, and over HTTP as conformance case `case009ObjectFeed` ([[055-api-contract-gaps]]). `follow=1` replays from the cursor and stays open, `TestFollowedFeed`, `TestFollowedFeedRefusals`, `TestFollowedFeedHeartbeat`, `TestFollowedFeedEnds` and `TestFollowReplaysThenStaysLive`, follows every readable object from now, `TestFollowedFeedOfEveryObject`, ends at a stop, `TestFollowedFeedEndToEnd`, and holds over HTTP as conformance case `case009FollowFeed` ([[066-events-follow]]) |
