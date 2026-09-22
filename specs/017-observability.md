---
title: "Observability: one metric table across three roles, traces across the seam, redacted logs, alert rules"
status: in-progress
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/018-egress-and-secrets.md
  - specs/020-scheduling-and-sets.md
  - specs/021-data-plane-workers.md
affects: [internal/metrics/, internal/api/, controller/, internal/events/, internal/egressd/, internal/worker/, internal/store/, internal/auth/, cmd/cellad/, tools/rules/, deploy/base/prometheusrule.yaml, .github/workflows/verify.yml]
effort: small
created: 2026-09-12
updated: 2026-09-21
author: changkun
---

# Observability

## Overview

An operator answers three questions from the outside: is the service
serving, are sandboxes starting, and is anything leaving that should
not. Metrics on the control plane's internal listener answer the first
two in aggregate, traces answer them per request across the worker
seam, logs carry the developer detail with every secret redacted, and
a rules file turns the metrics into the alerts an installation starts
with. Everything goes through `latere.ai/x/pkg/otel` and
`latere.ai/x/pkg/metrics`, the exporter is the standard `OTEL_*`
variables, and nothing is on by default.

## Current state

Not built. The scaffold serves `/version` and the probes and no
metrics.

## Design

### Three roles, one scrape surface

`cellad serve` is the only role with a probe listener
([[002-repository-scaffold]]), so it is the only role that serves
`/metrics`, from one registry in `internal/metrics`, the one package
that registers. The `worker` and `egress` roles open no listener beyond
their doors and streams ([[012-test-stubs-and-tiers]] asserts the
worker opens none); their process telemetry, traces, and logs leave
over OTLP through `pkg/otel.Bootstrap` and nothing else. The egress
counters below are the control plane's, recorded as `record` frames
arrive on the gateway's sync stream ([[018-egress-and-secrets]]);
worker and gateway health is the control plane's view of them.

### Metrics

Labels are bounded: no label ever carries a sandbox id, a subject, a
name, or a path. `route` is the `http.ServeMux` pattern, taken from a
context value a middleware behind the mux fills, since the OTel route
template runs before the mux matches. Gauges are scrape-time callbacks
over the store's cached indexes, never a driver `List`. Every
histogram names its bounds; labelled histograms are initialised over
their closed vocabularies at start so a series exists before its first
observation.

| Metric | Type | Labels | Buckets | Owner |
|---|---|---|---|---|
| `cella_requests_total` | counter | `route`, `status`, `code` | | 008 |
| `cella_request_duration_seconds` | histogram | `route` | 5ms to 10s, 12 bounds | 008 |
| `cella_rate_limited_total` | counter | `limit` (`subject`, `address`) | | 008 |
| `cella_sandboxes` | gauge | `phase`, `environment`, `driver` | | 005 |
| `cella_sandbox_create_duration_seconds` | histogram | `driver`, `pool` (`hit`, `miss`) | 0.5s to 300s, 12 bounds | 005, 020 |
| `cella_reaper_actions_total` | counter | `rule`, `action` | | 005 |
| `cella_recovery_attempts_total` | counter | `outcome` (`recovered`, `exhausted`, `volume_missing`) | | 005 |
| `cella_tokens_reminted_total` | counter | none | | 005, 006 |
| `cella_decisions_total` | counter | `endpoint` (`authorizer`, `admission`), `outcome` (`allow`, `deny`, `unavailable`, `cached`) | | 006, 007 |
| `cella_webhook_duration_seconds` | histogram | `endpoint` (`authorizer`, `admission`) | 5ms to 10s, 12 bounds | 006, 007 |
| `cella_events_pending` | gauge | none | | 009 |
| `cella_events_delivered_total` | counter | `outcome` (`acknowledged`, `deferred`, `dropped`) | | 009 |
| `cella_event_delivery_duration_seconds` | histogram | none | 5ms to 10s, 12 bounds | 009 |
| `cella_store_query_duration_seconds` | histogram | `op` | 1ms to 5s, 12 bounds | 010 |
| `cella_lease_held` | gauge | `name` (`reaper`, `journal`, `scheduler`, `environments`, `pool`) | | 010 |
| `cella_exec_total` | counter | `exit` (`0`, `nonzero`, `failed`) | | 008 |
| `cella_egress_connections_total` | counter | `decision`, `door` | | 018 |
| `cella_egress_bytes_total` | counter | `direction`, `door` | | 018 |
| `cella_gateways_connected` | gauge | `environment` | | 018 |
| `cella_gateway_snapshots_total` | counter | none | | 018 |
| `cella_environments` | gauge | `phase`, `reason` | | 021 |
| `cella_workers_connected` | gauge | `environment` | | 021 |
| `cella_operations_redelivered_total` | counter | none | | 021 |
| `cella_queue_depth` | gauge | `environment`, `queue` | | 020 |
| `cella_capacity` | gauge | `environment`, `resource` (`cpu`, `memory`, `disk`, `sandboxes`), `kind` (`declared`, `used`) | | 020 |
| `cella_preemptions_total` | counter | none | | 020 |
| `cella_pool_size` | gauge | `environment`, `state` (`ready`, `filling`) | | 020 |
| `cella_pool_adoptions_total` | counter | `outcome` (`adopted`, `miss`) | | 020 |
| `cella_set_replicas` | gauge | `phase` | | 020 |

The `environment` label is the environment's name, bounded by what an
operator applies. `TestMetricsTable` holds the registry to exactly
this table, reading this file through `runtime.Caller` since the gate
runs the suite from an empty directory.

### Traces

One span per request, named by the route pattern, carrying the
subject, the sandbox id, the `req_` request id, and the trace id as
attributes; child spans for the authorizer call, the admission call,
the driver call, and each store call. Under the owner policy and the
built-in admission there is no authorizer or admission child, and the
span test configures a stub of each. A stream's span covers its open,
not its life. Across the worker seam the `operation` control frame
carries a `traceparent` ([[021-data-plane-workers]]) that the worker
continues, so the driver child span covers the work and not the
enqueue; an egress record carries no trace and correlates by
principal. `pkg/otel` samples at `OTEL_TRACES_SAMPLER_ARG`, default
`0.2`, which `cellad` does not override; the suite sets `always_on`.
The request id is not the trace id: both appear on every log line of
the request and on its spans.

### Logs

`slog` through the OTel bridge, JSON on stderr, one line per request
at `INFO` with route, status, code, duration, subject, sandbox id,
request id, and trace id; one line at a stream's open and one at its
close, none per frame; none per egress connection, which is a record
([[018-egress-and-secrets]]); `WARN` for a webhook failure, a deferred
event, and a lost sandbox; `ERROR` for a driver call that failed and
a dropped event. Redaction is one handler that wraps both paths of
`pkg/otel.Bootstrap`'s tee, the local JSON handler and the OTLP bridge,
and applies two rules to every attribute before either sees it: a key
whose name is in the denylist (`env`, `value`, `token`, `credential`,
`secret`, `Authorization`, `Proxy-Authorization`,
`Cella-Egress-Credential`, and any key containing those words) has its
value replaced by `[redacted]`; a value that contains `cph_` followed by
32 characters of `[a-z2-7]` has the placeholder replaced. The
structural rule at the call site is that no handler passes a secret
value, a credential, or a token as an attribute at all; the redaction
handler is the second line.

### Alerts

`deploy/base/prometheusrule.yaml` is a PrometheusRule object, kept out
of the base kustomization and applied by an operator whose cluster
has the operator's CRD ([[014-release-and-installation]]). `tools/rules`
prints the rules document inside it, and a `rules` job in `verify.yml`
runs `promtool check rules` over that output and
`TestAlertsNameKnownMetrics`, which fails on an alert naming a `cella_`
metric or label absent from the table; a metric another exporter
publishes does not carry the prefix and is not checked.

| Alert | Expression, in words |
|---|---|
| readiness failing | `kube_pod_status_ready` false for the `cellad` Pods for 5 minutes, from kube-state-metrics |
| slow creates | `cella_sandbox_create_duration_seconds` p95 above 30 seconds for 10 minutes |
| a decision endpoint unavailable | `cella_decisions_total{outcome="unavailable"}` increasing in 5 minutes |
| events backing up | `cella_events_pending` above 1000 for 10 minutes, or `cella_events_delivered_total{outcome="dropped"}` increasing |
| an environment offline | `cella_environments{phase="Offline"}` above zero for 5 minutes |
| sandboxes lost on a ready environment | `cella_sandboxes{phase="Lost"}` above zero for 15 minutes where the environment's `cella_environments{phase="Ready"}` is one |
| a lease not held | `cella_lease_held` zero for a name for 2 minutes |
| operations redelivered | `cella_operations_redelivered_total` increasing for 10 minutes |

## Not in this spec

Dashboards; a platform builds those from the same metrics. The
`traceparent` field's place in the operation frame
([[021-data-plane-workers]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The registry holds exactly the metrics, labels, and buckets in the table, and every labelled histogram has a series at start | `TestMetricsTable` reading this file through `runtime.Caller` | built for the twenty-three rows whose owning spec has landed, as `TestMetricsTable`, `TestAwaitingRowsAreRegisteredByNobody` and `TestSeriesExistAtStart` ([[053-observability]]), `cella_queue_depth` and `cella_capacity` among them ([[057-scheduling-queue]]); the six rows of 008's limiter, 020 and 021 are declared in the same table and registered by nothing |
| No label value is a sandbox id, a subject, a name, or a path over a full e2e run | `TestLabelValuesAreBounded`, `TestObservabilityEndToEnd` | built as `TestLabelValuesAreBounded` and the scrape `TestObservabilityEndToEnd` reads after a create, two execs, a stop and a delete ([[053-observability]]) |
| The worker and gateway roles open no metrics listener and their telemetry arrives over OTLP | `TestEgressRoleServesNoScrapeSurface` and `TestTelemetryExportsOverOTLP` with an in-memory collector | built for the gateway as `TestEgressRoleServesNoScrapeSurface` and `TestTelemetryExportsOverOTLP` ([[053-observability]]); the worker role lands with [[021-data-plane-workers]] and takes the same seam |
| A request produces one parent span with the route pattern, the request id and trace id as attributes, and the four child spans under stub webhooks; a remote driver call's child span is continued on the worker | `TestRequestSpans`, `TestTheAPIDrawsAServerSpan` | the parent span, its name and its attributes are built as `TestRequestSpans` and `TestTheAPIDrawsAServerSpan`, which configures a stub authorizer and reads its client span back off the collector; the parent link is in the span context and is not decoded, and the driver and store children are not drawn ([[053-observability]]). The seam has nothing to cross until [[021-data-plane-workers]] |
| One canary per kind (env value, secret value, placeholder, credential, token, `Authorization`, `Proxy-Authorization`, `Cella-Egress-Credential`) appears in no log line on either path of the tee | `TestLogsRedact` | built as `TestLogsRedact`, `TestCanaryNeverReachesALogLine` and `TestTelemetryExportsOverOTLP`, which reads the canary back off the bridge ([[053-observability]]) |
| One log line per request, one per stream open and close, none per frame or egress connection | `TestOneLogLinePerRequest`, `TestOneLogLinePerStreamAndNonePerFrame` | built as `TestOneLogLinePerRequest` and `TestOneLogLinePerStreamAndNonePerFrame` ([[053-observability]]). A stream is one request and so one line, not two |
| The rules document extracted by `tools/rules` passes `promtool check rules` and names only metrics in the table | the `rules` job, `TestAlertsNameKnownMetrics` | the table half is built as `TestAlertsNameKnownMetrics`, `TestAlertsAggregateOnLabelsThatExist` and `TestEveryAlertCarriesItsRunbookSentence` over `deploy/base/prometheusrule.yaml` ([[053-observability]]); `tools/rules` and the `promtool` job are open |
