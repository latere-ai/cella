# Observability

Three questions an operator answers from outside a running `cellad`: is
the service serving, are sandboxes starting, and is anything leaving that
should not. Metrics answer the first two in aggregate, traces answer them
per request, logs carry the developer detail with every secret redacted,
and the alert rules turn the metrics into the pages an installation
starts with.

Nothing is on by default. The scrape surface is served on the internal
listener, which is not published outside the cluster; export to a
collector starts when the deployment sets `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Turning it on

| What you want | What to do |
|---|---|
| A Prometheus scrape | Point a `ServiceMonitor` at the internal port and path `/metrics`. Nothing to configure in `cellad`. |
| Traces and logs in a backend | Set `OTEL_EXPORTER_OTLP_ENDPOINT` on the Deployment. Add `OTEL_EXPORTER_OTLP_HEADERS` where the collector needs a credential. |
| Alerts | `kubectl -n <namespace> apply -f deploy/base/prometheusrule.yaml`, after setting the namespace and whatever label your Prometheus selects rules by. It needs the Prometheus Operator's CRD. |
| To confirm what is on | The start-up line says `telemetry=otlp` or `telemetry=off`. |

The variables are OpenTelemetry's own and this project defines none of
its own.

| Variable | What it does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector. Unset, nothing is exported and logs stay on stderr. |
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers for the collector, `k=v,k=v`. |
| `OTEL_SDK_DISABLED` | `true` turns every signal off with the endpoint still set. |
| `OTEL_TRACES_SAMPLER_ARG` | The head-sampling ratio for root spans, `0` to `1`. The default is `0.2`; `cellad` does not override it. |
| `LATERE_ENV`, `POD_NAME` | The deployment environment and the replica label on every record. |

## What a scrape carries

`GET /metrics` on the internal listener, in the Prometheus text format.
The public listener answers `404`: the numbers are the installation's and
not a caller's. The `egress` role opens its two doors and one outbound
stream and serves no scrape surface at all; its connection counts are the
control plane's, taken as its records arrive.

Labels are bounded. No series carries a sandbox id, a subject, a name or
a path: `route` is the router's pattern, not the URL, and `environment`
and `driver` are what the installation configured.

### Requests

| Metric | Labels |
|---|---|
| `cella_requests_total` | `route`, `status` (the class), `code` (the error envelope's, empty on a success) |
| `cella_request_duration_seconds` | `route` |
| `cella_exec_total` | `exit` (`0`, `nonzero`, `failed`) |

A stream is counted and not timed. Its life is not a request's latency,
and mixing the two makes the histogram unreadable.

### Sandboxes

| Metric | Labels |
|---|---|
| `cella_sandboxes` | `phase`, `environment`, `driver` |
| `cella_sandbox_create_duration_seconds` | `driver`, `pool` (`hit` where a prewarmed entry was adopted) |
| `cella_reaper_actions_total` | `rule`, `action` |
| `cella_recovery_attempts_total` | `outcome` (`recovered`, `exhausted`) |
| `cella_tokens_reminted_total` | none |
| `cella_pool_size` | `environment`, `state` (`ready`, `filling`) |
| `cella_pool_adoptions_total` | `outcome` (`adopted`, `miss`) |
| `cella_queue_depth` | `environment`, `queue` |
| `cella_capacity` | `environment`, `resource` (`cpu`, `memory`, `disk`, `sandboxes`), `kind` (`declared`, `used`) |

### Decisions, records and state

| Metric | Labels |
|---|---|
| `cella_decisions_total` | `endpoint` (`authorizer`, `admission`), `outcome` (`allow`, `deny`, `unavailable`) |
| `cella_webhook_duration_seconds` | `endpoint` |
| `cella_events_pending` | none: the records the sink has not taken |
| `cella_events_delivered_total` | `outcome` (`acknowledged`, `deferred`, `dropped`) |
| `cella_event_delivery_duration_seconds` | none |
| `cella_store_query_duration_seconds` | `op` |
| `cella_lease_held` | `name` (`reaper`, `journal`, `pool`, `environments`, `scheduler`) |

`cella_decisions_total{endpoint="authorizer"}` counts every authorization
question, whether your endpoint or the built-in owner policy answered it.

### The boundary

| Metric | Labels |
|---|---|
| `cella_egress_connections_total` | `decision` (`allowed`, `denied`, `unknown`, `passthrough`), `door` (`proxy`, `reverse`) |
| `cella_egress_bytes_total` | `direction` (`in`, `out`), `door` |
| `cella_gateways_connected` | `environment` |
| `cella_gateway_snapshots_total` | none |

A histogram carries a series before its first observation, so a rate over
a quiet installation reads zero rather than nothing.

## Traces

With the endpoint set, each request draws one span named after its route,
carrying the subject, the request id and the sandbox the route names. The
calls `cellad` makes on the request's own path, to your authorizer and
your admission endpoint, are child spans on the same trace, so one trace
shows what a refusal or a slow create was waiting on. Delivery to your
event sink runs on a loop of its own and draws its own trace: it is not
on any request's path.

The response header `X-Trace-Id` carries the trace, and every log line of
the request carries `trace_id` and the request id. Copy either out of
`kubectl logs` and it resolves in the backend.

## Logs

JSON on stderr, and the same records over OTLP where the endpoint is set.
One line per request and one at a stream's open and close; none per frame
and none per egress connection, which is a record and not a log line.

Every line passes one redacting handler before either destination sees
it. An attribute whose key names a value, a token, a credential, a secret
or an authorization header is replaced by `[redacted]`, and an egress
credential placeholder anywhere in a message or a value is replaced the
same way. A canary written through every log path proves it on both
destinations.

If you are reading this because a log line looks over-redacted: that is
the design. The rule is deliberately wide, and nothing that identifies a
sandbox, a subject or a request is touched by it. It does reach a few
names that are not secret: a line naming which Secret could not be read
carries `[redacted]` in place of that Secret's own id, and the gateway's
start-up line carries it in place of the environment's name. The sandbox
id on the same line says which workload it was about.

## Alerts

`deploy/base/prometheusrule.yaml` is applied on its own, not by the base
kustomization: a cluster without the Prometheus Operator refuses the
kind. Set the namespace and your Prometheus's rule-selector label, and
move the thresholds against your own figures.

| Alert | It means |
|---|---|
| `CelladDown` | The scrape is failing. No sandbox can be created, read or stopped. |
| `CelladNotReady` | A Pod is up and failing a readiness check. `/readyz` on the internal listener names which. |
| `CelladDecisionEndpointUnavailable` | Your authorizer or admission endpoint is giving no decision, so every request needing it is refused. `cellad check` names the endpoint. |
| `CelladEventsUndelivered` | Records are queued for the sink. The journal holds them until the retry window passes. |
| `CelladEventsDropped` | Records are being dropped undelivered. This is the one failure the pipeline cannot make good later. |
| `CelladLeaseNotHeld` | A control-plane loop is running on no replica. The lease name is on the series. |
| `CelladSlowCreates` | The ninety-fifth percentile create is above thirty seconds: capacity, image pulls, or a degraded backend. |
| `CelladSandboxesLost` | The driver no longer has sandboxes the control plane still wants, and recovery is not bringing them back. |

Two further rules, `CelladEnvironmentOffline` and
`CelladOperationsRedelivered`, read metrics the environment and worker
loops emit. They ship here and stay quiet until those loops run.
