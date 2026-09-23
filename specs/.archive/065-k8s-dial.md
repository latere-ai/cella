---
title: "Dial on the k8s driver: the port forwarding subresource, the declared-port rule, the Role, and the kind tier"
status: complete
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/008-api.md
  - specs/012-test-stubs-and-tiers.md
  - specs/013-security-and-threat-model.md
  - specs/015-conformance-suite.md
  - specs/023-computer-use-operations.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/036-k8s-driver.md
  - specs/.archive/060-dial-and-port-proxy.md
affects: [runtime/k8s/, deploy/, deploy_test.go, test/kind/, .github/workflows/, docs/, CHANGELOG.md, specs/]
effort: medium
created: 2026-09-23
updated: 2026-09-24
author: changkun
---

# Dial on the k8s driver

## Overview

[[060-dial-and-port-proxy]] built the dial socket
`GET /v1/sandboxes/{id}/dial/{port}` (`cella.dial.v1`), the port listing
`GET /v1/sandboxes/{id}/ports` and the proxy
`/v1/sandboxes/{id}/ports/{name}/{path...}` over the optional `Dialer`
of [[004-runtime-contract]], and implemented it on `native` (the host's
loopback) and `podman` (each declared port published on `127.0.0.1`).
The `k8s` driver, the one a hosted deployment runs, declares no `Dial`,
so on a cluster both routes answer 422 `capability_unsupported` and
`cella port-forward` exits with the refusal.

This slice implements `Dialer` on `runtime/k8s` through the API
server's `pods/portforward` subresource, declares `Dial`, adds the
subresource to the verbs the driver reviews at start and to the Role of
`deploy/base`, and runs the dial on the kind tier.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `Capabilities.Dial` on k8s | `runtime/k8s/k8s.go` | false; the comment defers it to "the slice that builds" it |
| Declared ports on k8s | `CreateSpec.Ports`, kept in the claim's `spec` annotation; `Inspect` probes them with a command in the workload's container | no route from the control plane to a port inside the Pod |
| The verbs the driver reviews | `verbs` in `runtime/k8s/k8s.go`, held to the Role by `TestRoleMatchesTheDriversVerbs` | no `pods/portforward` |
| The kind stack's declaration | `-capabilities files,pool,mesh` in `verify.yml` and `release.yml` | no `dial`, so `case008Dial` skips on the cluster |
| `TestClusterConformance` | `runtime/k8s/conformance_test.go` | passes no `Echo`, so `DialReachesAPort` skips; no workflow runs the test |

## Design

### The path: port forwarding, not the Pod's address

Two routes lead from `cellad` to a port inside a sandbox Pod:

| | `pods/portforward` through the API server | a TCP connection to the Pod's IP |
|---|---|---|
| What the deployment grants | an RBAC rule on one subresource | an ingress rule into the sandbox namespace from the control plane on every declared port, and an egress rule out of the control plane to the pod network on any port |
| A mesh member | reached: the kubelet connects from inside the Pod's own network namespace, which no NetworkPolicy filters | dropped: the mesh policy this driver writes admits the mesh's own members and nothing else, and the driver cannot name the control plane's Pod labels without fixing a coordinate of one installation |
| A control plane outside the cluster (`Options.Kubeconfig`) | reached, over the same connection every other call takes | unreachable: a Pod address is routed inside the cluster only |
| A server bound to `127.0.0.1` inside | reached: the kubelet connects to `localhost:<port>` in the Pod's namespace, as `native` reaches the host's loopback | unreachable |
| A port nothing listens on | the session opens and ends at once, with the kubelet's reason on the error stream | a refused connection; behind a policy that drops, a dial that hangs to its deadline |
| The data path | API server and kubelet in the path; one upgrade per connection | direct |

The driver takes port forwarding. The cost is the data path through the
API server and the kubelet, and one upgraded connection per dial; the
proxy opens one per request, so a page with many assets opens as many
sessions. Against it: the direct path needs a rule in two policies the
repository does not own, cannot reach a mesh member at all without the
driver naming the control plane's labels, cannot be proved by a
conformance run from outside the cluster, and misses a development
server that binds loopback, which is the default of several. Spec
[[004-runtime-contract]] already names port forwarding as the k8s route.
Both behind an option is not built: the option's cost is the policy
story above, not the code.

### Dial

`Dial(ctx, id, port)`:

1. a port outside 1 to 65535 is `ErrInvalid`;
2. the claim is read: absent, `ErrNotFound`;
3. the Pod is read and the phase derived by the package's phase table:
   anything but `Running` is `ErrNotRunning`, which covers a stopped
   sandbox, a Pod still pulling its image, and a workload that has not
   reported ready;
4. the port is looked up in the declared ports the claim's spec carries:
   an undeclared port is `ErrNotFound`, with podman's wording, because
   the other container driver can reach only a declared port and the
   port forwarding subresource would otherwise reach any port of the
   Pod, the desktop container's included;
5. a driver built with no cluster connection (`Options.Client` and no
   `Options.REST`) is `ErrUnsupported`, as `Exec` is;
6. one port forwarding session is opened to the Pod, and one pair of
   streams in it: the error stream, then the data stream, both carrying
   the port and request id `0`.

The session is negotiated as current `kubectl` does: a WebSocket that
tunnels the stream protocol (`SPDY/3.1+portforward.k8s.io`) first, and
where the API server refuses the upgrade, the SPDY upgrade. Both
requests carry the dial's context, which bounds the TCP and TLS
handshakes; the whole negotiation runs apart from the caller and is
abandoned at the context's end, with a connection that arrives after
it closed at once. The connection handed back does not depend on the
context: the dial route cancels its bound as soon as `Dial` returns.

The connection is one end of an in-memory pipe, pumped to the data
stream both ways, so deadlines work as on any `net.Conn`. The caller's
`Close` ends the session. When the data stream ends, the pump waits for
the error stream to end as well, because the kubelet writes its reason
there before it closes both: a non-empty reason is the error the next
`Read` returns in place of `io.EOF`. A port nothing listens on
therefore reads as a connection that fails with the kubelet's sentence,
which the dial socket closes with 1011 `upstream_unavailable` and the
proxy answers with 502, rather than as a clean close.

### Access

The driver's verbs gain `pods/portforward` with two verbs, one per
transport: `get`, which authorizes the WebSocket request (a `GET`), and
`create`, which authorizes the SPDY upgrade (a `POST`). With `create`
alone the dial still works, over the fallback, but every dial first
pays for a refused WebSocket handshake and leaves a denial in the API
server's audit log, and the proxy dials per request. Preflight reviews
both, so a Role without them is named at start, and `cellad serve`
refuses to start on it, which is the rule for every verb in the table.

`deploy/base/rbac.yaml` grants both, and no NetworkPolicy changes: the
session is a request to the API server, on the port the control
plane's egress rule already admits for every other call.

### The kind tier

`TestClusterConformance` passes `Echo` as busybox's netcat serving every
connection with its own `cat`, which is what the podman suite runs, so
`DialReachesAPort` runs wherever a cluster is configured.

The install job runs `TestClusterConformance`'s `DialReachesAPort` and
`PortsReportListening` against the kind cluster, from the runner, in a
namespace of its own, because `sweep` deletes every sandbox in the
namespace it drove. The whole suite does not run there: it has never
run against a cluster, and its other cases belong to the slices that
build them.

`TestClusterDial` in `test/kind` drives the dial socket of the `cellad`
the stack deployed, under the Role and the NetworkPolicy of
`deploy/base`: a sandbox serving an echo on a declared port, two
sockets at once each carrying a line both ways, and a socket to an
undeclared port closing with 1011 `not_found`. It runs in both kind
jobs, whose `-run '^TestCluster'` already selects it.

The kind stack's conformance run declares `dial` in both workflows, so
`case008Dial` runs against the cluster.

## Not in this slice

`Dial` on the `remote` driver, which is the worker stream's. The
`sandbox.port` record. `expose: mesh` and `expose: public`. The exec
path's own WebSocket attempt, which the Role still answers with a
refusal before the SPDY fallback because `pods/exec` is granted
`create` alone; that belongs with the attach slice. Reuse of one
session across dials.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The driver declares `Dial` beside `Files`, `Pool` and `Mesh`, and implements `runtime.Dialer` | `TestDeclarations` | passing |
| `Dial` refuses a port outside 1 to 65535 with `ErrInvalid`, an absent sandbox with `ErrNotFound`, a stopped, pending or starting one with `ErrNotRunning`, an undeclared port with `ErrNotFound`, a claim with no spec with `ErrInvalid`, and a driver with no cluster connection with `ErrUnsupported`, before any session opens | `TestDialRefusals`, `TestDialRefusesAClaimWithoutASpec` | passing |
| A dial opens one session to the sandbox's own Pod and port, and pairs one error and one data stream in it with the port and the request id | `TestDialForwardsTheDeclaredPort`, `TestPortForwardCarriesBytesBothWays` | passing |
| The session is negotiated over the WebSocket tunnel where the API server serves it and over the SPDY upgrade where it refuses the WebSocket, carries bytes both ways on two connections at once, and outlives the context it was dialed with | `TestPortForwardCarriesBytesBothWays`, `TestPortForwardFallsBackToTheUpgrade`, `TestPortForwardOutlivesTheDialContext` | passing |
| A port nothing listens on reads as a connection whose `Read` returns the kubelet's reason; a refused session and an API server that does not answer are errors from `Dial`; a negotiation that does not answer ends with the dial's context; closing the connection ends the session | `TestPortForwardReportsTheKubeletsReason`, `TestPortForwardRefused`, `TestPortForwardDials`, `TestPortForwardHonorsTheDialContext`, `TestPortForwardCloseEndsTheSession` | passing |
| A configuration the session cannot build its address from is refused at `New` | `TestNewRefusesAConfigurationThePortForwardCannotUse` | passing |
| Every request the dial makes is one the verbs table reviews, and the Role of `deploy/base` grants exactly the table | `TestEveryDialRequestIsInTheVerbTable`, `TestRoleMatchesTheDriversVerbs` | passing |
| The driver passes `DialReachesAPort` and `PortsReportListening` against a kind cluster | `TestClusterConformance` | built: `Echo` is passed, and `verify.yml`'s install job runs the two cases against its kind cluster; skipped here, where no cluster is reachable |
| The `cellad` the kind stack deploys reaches a declared port through the dial socket, two sockets at once, reports the port listening, and closes a socket to an undeclared port with 1011 `not_found` | `TestClusterDial` | built: both kind jobs run it and require its pass; skipped here, where no cluster is reachable |
| Both kind conformance runs declare `dial`, both kind jobs require `TestClusterDial`, and the install job runs the dial and port cases of the driver suite | `TestKindRunsDeclareDial` | passing |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates`, `TestNoLatereCoordinatesInReleasedArtifacts` | passing |

## Outcome

`runtime/k8s` implements `runtime.Dialer` through the `pods/portforward`
subresource and declares `Dial`, so the port listing, the proxy, the dial
socket and `cella port-forward` work on the Kubernetes environment for
every port a manifest declares. The Role of `deploy/base` grants `get` and
`create` on the subresource, and no NetworkPolicy changes.

| Piece | Where |
|---|---|
| `Dial`, the session negotiated WebSocket first with the SPDY fallback, the stream pair, the connection pumped through an in-memory pipe with the kubelet's reason as the read error | `runtime/k8s/dial.go` |
| `Dial: true`, the forwarder built at `New` from `Options.REST`, `pods/portforward` `get` and `create` in the verbs table | `runtime/k8s/k8s.go` |
| The fake API server that answers the WebSocket tunnel and the SPDY upgrade with the stream protocol and serves a pair as the kubelet does | `runtime/k8s/dial_test.go` |
| `Echo` in the cluster conformance run | `runtime/k8s/conformance_test.go` |
| The Role, its table, the operator's reference | `deploy/base/rbac.yaml`, `deploy_test.go`, `deploy/README.md` |
| The dial through the deployed control plane, and the stack brought up once per package in local mode | `test/kind/dial_test.go`, `test/kind/kind_test.go` |
| `dial` declared to both kind conformance runs, `TestClusterDial` required in both kind jobs, the driver's dial and port cases in the install job | `.github/workflows/verify.yml`, `.github/workflows/release.yml`, `dial_test.go` |
| The operator's and caller's page | `docs/kubernetes.md`, `docs/README.md`, `docs/install.md`, `CHANGELOG.md` |

`go tool lateregate` passes with `runtime/k8s` at 92.5%. The session code
is exercised against a fake API server built from the same client-go and
apimachinery stream libraries the kubelet and the API server use, over
both transports. No kind cluster was reachable on the machine this was
built on, so `TestClusterConformance` and `TestClusterDial` did not run
against a real API server and kubelet here; the first `verify` run of the
install job is that proof.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[004-runtime-contract]] | k8s `Dial` is "yes: port forwarding", with no word on which ports | a declared port only, `ErrNotFound` otherwise | the subresource reaches any port of the Pod, the desktop container's included, and podman reaches only a declared port |
| This spec's Access | the choice between one verb and two | `get` and `create` on `pods/portforward`, both reviewed at start | with `create` alone every dial pays a refused WebSocket handshake and an audit denial before the fallback, and the proxy dials per request |
| [[008-api]] | a port nothing listens on closes the dial socket 1011 `upstream_unavailable` | holds on k8s: the kubelet's reason is the connection's read error; podman behind a user-space forwarder still reads it as a clean close | the kubelet writes its reason on the error stream before it closes both streams, and the pump waits for it |

### What this leaves open

| Open | Why |
|---|---|
| `Dial` on the `remote` driver | the worker stream carries no dial; a k8s worker environment still answers 422 on both routes |
| The exec path's WebSocket attempt | `pods/exec` is granted `create` alone, so every exec on a real cluster is refused over the WebSocket before the SPDY fallback; the attach slice owns that path |
| The whole driver suite against a cluster | only `DialReachesAPort` and `PortsReportListening` run in the install job; the other cases belong to the slices that build each capability |
| Reuse of one session across dials | one session per connection is the simplest correct shape; the proxy's per-request dial makes it the first thing to measure |
