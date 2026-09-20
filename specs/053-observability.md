---
title: "Observability: the metric registry of 017, the spans across the seam, the redacting log handler, and the alert rules over what is emitted"
status: in-progress
track: core
depends_on:
  - specs/017-observability.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/042-events.md
  - specs/.archive/048-release-and-check.md
affects: [internal/metrics/, internal/api/, internal/auth/, internal/admission/, internal/events/, internal/store/, controller/, cmd/cellad/, deploy/base/prometheusrule.yaml, docs/observability.md]
effort: medium
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# Observability

## Overview

[[017-observability]] fixes one metric table, the spans a request draws,
the redaction every log line passes through, and the alerts an
installation starts with. Nothing of it is built: the scrape surface
does not exist, `pkg/otel` is reached only by the outbound clients of
[[006-identity]] and [[007-admission]], and
`deploy/base/prometheusrule.yaml` carries [[.archive/048-release-and-check]]'s
placeholders over metric names no binary emits.

This slice builds the registry, the seams that feed it, the handler that
redacts, and the rules that read it. It emits the rows of 017's table
whose owning spec has landed and declares the rest as awaiting their
slice, so the table in code and the table in 017 are one document and
drift in either direction is a test failure.

The hosted source ([[031-hosted-sandbox-consolidation]], the row for
`internal/platform/metrics` and `internal/platform/traffic`) pushed
dotted OTel instruments and served no scrape surface. What ports is the
behaviour, not the shape: the counts an operator read (sandboxes by
state, warm pool depth, create time cold against warm, idle stops, cap
hits) become 017's rows, and the route taxonomy that bounded the
hosted `path` label becomes 017's `route`, which is the `http.ServeMux`
pattern and needs no taxonomy of its own.

## Current state

| Piece | State |
|---|---|
| `internal/metrics` | does not exist |
| `/metrics` on the internal listener | not served; the listener answers the probes of [[.archive/002-repository-scaffold]] alone |
| `pkg/otel.Bootstrap` | never called; `cellad` logs through the default `slog` handler and exports nothing |
| spans | the outbound transports of `internal/auth` and `internal/admission` draw client spans; no server span exists to parent them |
| redaction | none |
| `deploy/base/prometheusrule.yaml` | five alerts over `cella_ready`, `cella_authorizer_calls_total` and `cella_sandbox_phase_total`, none of which is in 017's table |

## Design

### Where the registry lives and who may reach it

`internal/metrics` holds one `*Registry`, built over
`latere.ai/x/pkg/metrics`: labelled counters, histograms with declared
bounds and a `+Inf` bucket, and scrape-time gauge callbacks, written in
the Prometheus text exposition format. It is the one package that
registers, and `cmd/cellad` builds it once.

`arch_test.go` holds `./controller` and `./egress` to the contract types
and forbids every import under `internal/`. The seam is therefore the
consumer's own interface: each package declares the narrow method set it
calls, over standard-library types alone, and defaults to an unexported
no-op when its option is nil. `*metrics.Registry` satisfies all of them
structurally and is named in none of them.

```mermaid
flowchart LR
  CTL[controller.Metrics] --> REG
  API[api.Metrics] --> REG
  AUTH[auth.Metrics] --> REG
  ADM[admission.Metrics] --> REG
  EV[events.Metrics] --> REG
  ST[store.Metrics] --> REG
  REG[internal/metrics.Registry] --> SCRAPE["GET /metrics, internal listener"]
  REG --> GAUGE["scrape callbacks: controller, hub, journal"]
```

A counter or a histogram is pushed: the code path that owns the number
calls one method with one line. A gauge is pulled: `cmd/cellad` hands the
registry a closure over the index that already holds the answer, so no
loop exists to keep a gauge current and no driver `List` runs at scrape
time. `cella_lease_held` is the one exception: a lease is held or not at
the moment a loop takes its tick, and nothing outside the loop knows, so
the loop pushes it.

### The table as emitted

Twenty-one of 017's twenty-nine rows have an owning spec that has
landed. Each is registered at start-up, so a series exists before the
first observation, and every labelled histogram is initialised over its
closed vocabulary.

| Metric | Type | Labels | Buckets | Recorded at |
|---|---|---|---|---|
| `cella_requests_total` | counter | `route`, `status`, `code` | | the route wrapper in `internal/api`, on return |
| `cella_request_duration_seconds` | histogram | `route` | latency | the same wrapper, excluding a hijacked stream |
| `cella_sandboxes` | gauge | `phase`, `environment`, `driver` | | scrape callback over `Controller.List` |
| `cella_sandbox_create_duration_seconds` | histogram | `driver`, `pool` | create | `Controller.createLocked`, on the reply |
| `cella_reaper_actions_total` | counter | `rule`, `action` | | `Controller.Reap`, per enforced rule |
| `cella_recovery_attempts_total` | counter | `outcome` | | `Controller.recoverLocked` and `graceLocked` |
| `cella_tokens_reminted_total` | counter | none | | `Controller.rotateLocked` |
| `cella_decisions_total` | counter | `endpoint`, `outcome` | | `auth.Authorizer.decide`, `admission.Client.admit` |
| `cella_webhook_duration_seconds` | histogram | `endpoint` | latency | the same two calls |
| `cella_events_pending` | gauge | none | | scrape callback over the journal |
| `cella_events_delivered_total` | counter | `outcome` | | `events.Deliverer`, beside each count it already keeps |
| `cella_event_delivery_duration_seconds` | histogram | none | latency | around one delivery attempt |
| `cella_store_query_duration_seconds` | histogram | `op` | store | the methods of `store.Controlled` |
| `cella_lease_held` | gauge | `name` | | pushed by the reaper tick, the pool tick and one delivery pass |
| `cella_exec_total` | counter | `exit` | | the exec socket, at the exit frame |
| `cella_egress_connections_total` | counter | `decision`, `door` | | `EgressHub.onRecord` |
| `cella_egress_bytes_total` | counter | `direction`, `door` | | the same record |
| `cella_gateways_connected` | gauge | `environment` | | scrape callback over `EgressHub.Connected` |
| `cella_gateway_snapshots_total` | counter | none | | `EgressHub.join`, per gateway seeded |
| `cella_pool_size` | gauge | `environment`, `state` | | scrape callback over the pool index |
| `cella_pool_adoptions_total` | counter | `outcome` | | the create path, on hit and on miss |

Bucket sets, each twelve bounds, named once and shared:

| Name | Bounds |
|---|---|
| latency, 5ms to 10s | 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10 |
| create, 0.5s to 300s | 0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120, 180, 300 |
| store, 1ms to 5s | 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5 |

Closed vocabularies, initialised at start so a scrape before the first
event carries the series:

| Label | Values |
|---|---|
| `endpoint` | `authorizer`, `admission` |
| `outcome`, on a decision | `allow`, `deny`, `unavailable`, `cached` |
| `outcome`, on delivery | `acknowledged`, `deferred`, `dropped` |
| `outcome`, on recovery | `recovered`, `exhausted`, `volume_missing` |
| `outcome`, on adoption | `adopted`, `miss` |
| `pool` | `hit`, `miss` |
| `op` | `load`, `save`, `write`, `remove`, `rebuild`, `events`, `acquire` |
| `name` | `reaper`, `journal`, `pool` |
| `door` | `proxy`, `reverse` |
| `decision` | `allowed`, `denied`, `unknown`, `passthrough` |
| `direction` | `in`, `out` |
| `exit` | `0`, `nonzero`, `failed` |
| `state`, on the pool | `ready`, `filling` |

`name` carries the lease's kind and not its key: the refill loop's lease
is `pool:<environment>`, and an environment per series would make the
label unbounded. 017's `scheduler` and `environments` arrive with their
loops.

The eight rows awaiting their owner, declared in the same table in code
and registered by none:

| Metric | Waits on |
|---|---|
| `cella_rate_limited_total` | the limiter of [[008-api]] |
| `cella_environments` | the Environment kind of [[021-data-plane-workers]] |
| `cella_workers_connected` | the worker role of [[021-data-plane-workers]] |
| `cella_operations_redelivered_total` | the operation queue of [[021-data-plane-workers]] |
| `cella_queue_depth` | the scheduler of [[020-scheduling-and-sets]] |
| `cella_capacity` | the same |
| `cella_preemptions_total` | the same |
| `cella_set_replicas` | the sets of the same |

### One scrape surface, three roles

`cellad serve` is the only role with an internal listener, so it is the
only role that serves `/metrics`. The listener becomes a mux: `GET
/metrics` writes the registry, and every other path is the probe handler
it was. `cellad egress` opens its two doors and its one outbound stream
and nothing else; its process telemetry, spans and logs leave over OTLP
and its connection counts are the control plane's, recorded where the
`record` frames of [[018-egress-and-secrets]] arrive. The worker role of
[[021-data-plane-workers]] takes the same seam: it declares the methods
it calls, and a control plane that drives no worker registers no worker
series.

### Traces

`pkg/otel.Bootstrap` runs first in both roles, before the store, the
identity and the driver are opened, because a collaborator built earlier
captures the `slog` default as it stands. `otel.Handler` wraps `/v1/`
alone: the probes, the key set and the version line draw no span.

The server span is named by the route once the mux has matched, which is
also when `route` is known: the wrapper behind the mux reads
`http.Request.Pattern`, writes it into the slot the outer handler put in
the context, and renames the span. Attributes are the subject, the
sandbox id, the request id and the trace id. The authorizer call and the
admission call are child spans around the same calls the counters wrap,
so a decision's latency has one span and one histogram from one place.
The gateway's sync stream is one span per accepted stream and not per
frame; a `record` frame carries no trace and correlates by principal.

A hijacked route (exec, attach, the screen stream, the sync stream)
counts one request and observes no duration: its lifetime is the
stream's and not the request's, and mixing the two makes the latency
histogram unreadable.

### Logs

`Bootstrap` returns the logger over its tee. This slice wraps that
logger's handler once and sets the default again, so both paths of the
tee, the local JSON handler and the OTLP bridge, pass through one
redacting handler. It applies 017's two rules to every attribute, to
attributes carried on the handler by `WithAttrs`, to attributes inside a
group, and to the message:

1. a key in the denylist, or a key containing a denylisted word, case
   insensitive, has its value replaced by `[redacted]`. The list is
   `env`, `value`, `token`, `credential`, `secret`, `Authorization`,
   `Proxy-Authorization`, `Cella-Egress-Credential`.
2. a value holding `cph_` followed by 32 characters of `[a-z2-7]`, the
   placeholder of [[018-egress-and-secrets]], has the placeholder
   replaced.

The rule at the call site stands: no handler passes a secret value, a
credential or a token as an attribute at all. This handler is the second
line, and a canary written through every log path this slice touches
proves it holds on both paths of the tee.

### Alerts

`deploy/base/prometheusrule.yaml` keeps 048's shape and its alert names
where a name still describes what is measured, and its expressions
become expressions over the table. `CelladNotReady` reads
`kube_pod_status_ready` from kube-state-metrics as 017 states, since no
`cella_` metric reports readiness. Each alert carries the runbook
sentence 017 gives it.

| Alert | Expression over | Fires when |
|---|---|---|
| `CelladDown` | `up{job="cellad"}` | the scrape fails for 5 minutes |
| `CelladNotReady` | `kube_pod_status_ready` | a `cellad` Pod is not ready for 5 minutes |
| `CelladSlowCreates` | `cella_sandbox_create_duration_seconds_bucket` | p95 above 30 seconds for 10 minutes |
| `CelladDecisionEndpointUnavailable` | `cella_decisions_total{outcome="unavailable"}` | increasing over 5 minutes |
| `CelladEventsUndelivered` | `cella_events_pending` | above 1000 for 10 minutes |
| `CelladEventsDropped` | `cella_events_delivered_total{outcome="dropped"}` | increasing over 10 minutes |
| `CelladSandboxesLost` | `cella_sandboxes{phase="Lost"}` | above zero for 15 minutes |
| `CelladLeaseNotHeld` | `cella_lease_held` | zero for a name for 2 minutes |
| `CelladEnvironmentOffline` | `cella_environments{phase="Offline"}` | above zero for 5 minutes |
| `CelladOperationsRedelivered` | `cella_operations_redelivered_total` | increasing over 10 minutes |

The last two read rows [[021-data-plane-workers]] owns. They ship now
and evaluate over an empty series until that slice emits them, which is
the state 017's own alert table describes; the rule test holds every
`cella_` name in the file to the declared table rather than to the
registered subset, so a name that is neither is still a failure.

### Configuration

This slice reads no variable of its own. The exporter is the `OTEL_*`
set `pkg/otel` reads: `OTEL_EXPORTER_OTLP_ENDPOINT` turns export on,
`OTEL_EXPORTER_OTLP_HEADERS` authenticates it, `OTEL_SDK_DISABLED` turns
every signal off, `OTEL_TRACES_SAMPLER_ARG` is the head-sampling ratio
which `cellad` does not override, and `LATERE_ENV` and `POD_NAME` label
the resource. The deployment sets them and this project ships no value
for any. The start-up line gains one token, `telemetry=otlp` or
`telemetry=off`, so an operator reads from the first line whether
anything is leaving.

## Not in this spec

Dashboards. `tools/rules` and the `promtool check rules` job of 017:
the rules document is held to the table by a Go test in this repository,
and the external validator is a workflow change this slice does not
carry. The worker and scheduler rows of the table, which land with their
loops.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The registered metrics match 017's table row for row in name, type, labels and buckets, and the unregistered rows are exactly the declared awaiting set | `TestMetricsTable`, reading `017-observability.md` through `runtime.Caller` | not built |
| Every labelled histogram has a series before its first observation, and no label value is a sandbox id, a subject, a name or a path | `TestSeriesExistAtStart`, `TestLabelsAreBounded` | not built |
| Each counter and histogram is moved by the code path that owns it | one test per owner package against a fake recorder, and `TestRegistryRecords` over the registry | not built |
| `/metrics` on the internal listener serves the exposition and the public listener does not | `TestScrapeSurface`, `TestMetricsIsInternalOnly` | not built |
| The egress role opens no listener beyond its doors | `TestEgressRoleOpensNoScrapeSurface` | not built |
| A request draws one server span named by the route with the request id and trace id, and the authorizer and admission calls are child spans | `TestRequestSpans` over an in-memory span exporter | not built |
| One canary per kind (env value, secret value, placeholder, credential, token, `Authorization`, `Proxy-Authorization`, `Cella-Egress-Credential`) appears on neither path of the tee | `TestLogsRedact`, `TestRedactionReachesTheBridge` against an OTLP collector | not built |
| Every `cella_` metric and label the rules file names is in the declared table, and the document parses | `TestAlertsNameKnownMetrics` | not built |
| A serve, create, exec, stop and delete moves the counters an operator reads, with export on | `TestObservabilityEndToEnd` in `cmd/cellad` | not built |
