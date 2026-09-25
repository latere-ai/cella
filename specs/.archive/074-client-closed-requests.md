---
title: "Client-closed requests: a request its caller closed is counted as client_closed and not as the failure of the dependency its cancellation reached"
status: complete
track: core
depends_on:
  - specs/006-identity.md
  - specs/008-api.md
  - specs/017-observability.md
affects: [internal/api/, internal/auth/, docs/observability.md, CHANGELOG.md]
effort: small
created: 2026-09-25
updated: 2026-09-25
author: changkun
---

# Client-closed requests

## Overview

`net/http` cancels a request's context while its handler runs when the
client's connection closes. Every call `cellad` makes on a request's path
takes that context: the driver's log stream, the authorizer, the store.
A caller that closes its request mid-call therefore ends the call with
`context.Canceled`, and `errorEnvelope` in `internal/api/api.go` maps an
error it does not recognize to `driver_unavailable` (503), and the
authorizer maps any call that produced no decision to
`authorizer_unavailable` (503).

So a browser that leaves a log view while the log is still opening is
recorded as a driver failure. On a hosted plane on 2026-09-25 a
`GET /v1/sandboxes/{id}/logs` was logged `5xx driver_unavailable` after
547 ms, and the proxy in front of `cellad` logged `context canceled` for
the same trace 15 ms earlier; a `GET /v1/sandboxes` was logged
`5xx authorizer_unavailable` the same way. Each raised the 5xx rate of
`cella_requests_total`, and the authorizer's also counted
`cella_decisions_total{outcome="unavailable"}`, which is the outcome the
"a decision endpoint unavailable" alert of [[017-observability]] reads.

## Design

The one count and the one log line per request are taken in `observe`,
deferred by the handler, before the handler returns and before
`net/http` cancels the context for its own reasons. A done context there,
on a connection that was not hijacked, is the caller's leaving.

- A request whose context is done at `observe`, whose connection was not
  hijacked, and whose handler wrote a status of 500 or above is counted
  and logged with the status class of 499, the status proxies log for a
  client that closed its request, and the code `client_closed`
  (`api.ClientClosed`). The class is `4xx`, so the `status` label keeps
  its five values.
- The code is recorded and never written: the envelope the handler wrote
  goes nowhere, and the error table of [[008-api]] is unchanged.
- A decision whose context is done and which produced no decision is not
  counted in `cella_decisions_total` and not timed. It is still refused
  as `authorizer_unavailable`, since no decision is never an allow.
- A 5xx answered to a caller that is still waiting is unchanged, and so
  is every answer below 500 and every hijacked stream.

## Not in this spec

| Item | Why |
|---|---|
| The admission endpoint's count on a caller's cancellation | a create's admission call runs under its own timeout and the create path answers before a browser typically leaves; nothing observed it |
| A code for a path no route serves (the router's plain 404 is logged with an empty route and code) | a separate question about the request log's vocabulary |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A log read whose caller closes it while the driver is still opening it is counted and logged `4xx` `client_closed` | `TestACallerThatLeftIsNotADependencyFailure` | passing; failed on the tree before the fix with `5xx` `driver_unavailable` in both the count and the line |
| The same driver failure with the caller still waiting stays `5xx` `driver_unavailable` | `TestACallerThatLeftIsNotADependencyFailure` | passing |
| An authorization question its caller abandoned is refused as `authorizer_unavailable` and not counted | `TestADecisionItsCallerAbandonedIsNotCounted` | passing; failed on the tree before the fix with one `unavailable` count |
| An endpoint that fails with the caller waiting is still counted `unavailable` | `TestDecisionsAreCounted` | passing |

## Outcome

Built as designed.

| Piece | Where |
|---|---|
| `ClientClosed`, `statusClientClosed`, `callerGone`, the reclassification in `observe` | `internal/api/metrics.go` |
| The decision left uncounted on the caller's cancellation | `internal/auth/authorizer.go` |
| The tests | `internal/api/closed_test.go`, `internal/auth/metrics_test.go` |
| The operator's page and the release note | `docs/observability.md`, `CHANGELOG.md` |
