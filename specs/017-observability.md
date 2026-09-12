---
title: "Observability: metrics, traces, logs, and the alerts an operator starts from"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-lifecycle-controller.md
  - specs/008-api.md
affects: [internal/api/, controller/, internal/events/, deploy/base/prometheusrule.yaml]
effort: small
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Observability

## Overview

An operator answers three questions from the outside: is the service
serving, are sandboxes starting, and is anything leaking. Metrics on
the internal listener answer the first two in aggregate, traces answer
them per request, logs carry the developer detail, and a rules file
turns the metrics into the alerts an installation starts with. All of
it goes through `latere.ai/x/pkg/otel` and `latere.ai/x/pkg/metrics`,
so the exporter is the standard `OTEL_*` variables and nothing is on by
default.

## Current state

Not built. The scaffold serves `/version` and the probes and no
metrics.

## Design

### Metrics

| Metric | Type | Labels |
|---|---|---|
| `cella_requests_total` | counter | `route`, `status`, `code` |
| `cella_request_duration_seconds` | histogram | `route` |
| `cella_sandboxes` | gauge | `phase`, `environment`, `driver` |
| `cella_sandbox_create_duration_seconds` | histogram | `backend`, `pool` (`hit`, `miss`) |
| `cella_reaper_actions_total` | counter | `rule`, `action` |
| `cella_pool_size` | gauge | `state` (`ready`, `filling`) |
| `cella_webhook_duration_seconds` | histogram | `webhook` (`authorizer`, `admission`, `sink`), `outcome` |
| `cella_events_pending` | gauge | none |
| `cella_events_dropped_total` | counter | none |
| `cella_exec_total` | counter | `exit` (`0`, `nonzero`, `failed`) |
| `cella_store_query_duration_seconds` | histogram | `op` |

One registry, one package that registers, and a test that every name
above exists and nothing else does, so the table is the reference.

### Traces

One span per request, named by route, carrying the subject hash, the
sandbox id, and the request id; child spans for the authorizer call,
the admission call, the backend call, and the store. A stream's span
covers its open, not its life. The `X-Request-Id` is the trace's
correlation and appears in every log line of the request.

### Logs

`slog` through the OTel bridge, JSON on stderr, one line per request
at `INFO` with route, status, code, duration, subject hash, and sandbox
id; `WARN` for a webhook failure and a dropped event; `ERROR` for a
backend call that failed. `env` values, tokens, and `Authorization`
headers are redacted by a handler that runs before any exporter.

### Alerts

`deploy/base/prometheusrule.yaml`: readiness failing for 5 minutes;
create p95 above 30 seconds for 10 minutes; any authorizer or admission
unavailability in 5 minutes; events pending above 1000 or any drop;
sandboxes `Lost` above zero for 15 minutes. The rules are checked with
`promtool` in CI and every metric an alert names is in the table.

## Not in this spec

Dashboards; a platform builds those from the same metrics.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The registry holds exactly the metrics in the table | `TestMetricsTable` reading this file | not built |
| A request produces one parent span and the four child spans named, with the request id on each | `TestRequestSpans` with an in-memory exporter | not built |
| A canary env value and a canary token appear in no log line | `TestLogsRedact` | not built |
| The rules file passes `promtool check rules` and names only metrics in the table | the CI step and `TestAlertsNameKnownMetrics` | not built |
