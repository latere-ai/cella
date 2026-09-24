---
title: "Egress and secrets: the Secret kind, placeholders, the gateway as a data plane component, sync and telemetry, the boundary a workload cannot widen"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/010-state.md
affects: [manifest/v1/, egress/, internal/egressd/, internal/api/, internal/store/, internal/config/, runtime/]
effort: large
created: 2026-09-12
updated: 2026-09-24
author: changkun
---

# Egress and secrets

## Overview

A sandbox reaches the network through one door, an egress gateway
beside it, and holds no credential of its own: every secret it needs
is a placeholder in its environment that the gateway swaps for the
value on the way out, and only toward a host the secret's owner named.
The scope of a secret travels with the secret. A manifest chooses which
secrets to mount and how far the sandbox may reach; it may narrow
either later, and a workload may never widen them. This is the network
boundary of the control plane, and the mechanism that makes "a sandbox
reaches only the hosts its manifest allows" a property rather than a
policy.

The gateway is a data plane component, not a control plane one. It
runs beside the sandboxes, in a directly driven environment or a
self-hosted one alike, and connects outbound to the control plane with
an environment key the way a worker does: it receives the credential
maps of its environment on connect and every change after, and it
reports every connection it handled on the same stream, so the control
plane never dials into a data plane, a gateway restart loses nothing,
and platform-level monitoring of what left every sandbox falls out of
the protocol. The substitution engine is `latere.ai/x/pkg/egress`;
this spec says exactly which of its mechanisms the package has today
and which the gateway role adds, fixes the `Secret` kind, how a
manifest compiles into a map, the two doors, the sync and telemetry
protocol, what is rewritten and what never is, and how the boundary
stays fixed.

## Current state

The boundary, the map, the gateway role, the sync stream and the
records are built by [[039-egress-gateway]]; the `Secret` kind, its
store and the values it substitutes are [[046-secret-kind]], and the
`Records` store surface, the metrics and the journal path are 042's and
010's. Both slices' Outcomes record where they read this spec
differently. The driver's half on k8s, the NetworkPolicy per sandbox and
the gateway projected into the Pod, is [[070-k8s-egress]], which admits
DNS and the gateway and not `cellad`, and writes no sidecar.
`pkg/egress` today holds the substitution engine (`Map`,
`Entry` with placeholder, secret, allowed hosts, an optional resolver
and a body flag; `SubstituteHTTPRequestContext` over header values,
the raw query string, and small textual bodies; framing headers
excluded), `MintPlaceholder` and `IsPlaceholder` (`cph_` plus 32
base32 characters from 20 random bytes), `Registry` keyed by
principal with `Set`, `Get`, `Delete`, an inbound ingest API with
`IngestEntry` and `DecodeIngestBody`, the `OAuthClientCredentials`
resolver, `TokenAuth` for JWT proxy credentials, and `Gateway` with
`CA`: a CONNECT proxy that authenticates the caller, refuses loopback
targets, terminates TLS for a host the map has a secret for, and
tunnels every other host untouched. It has no policy gate (no mode,
allow list, deny list, or ports), no notion of where an entry is
injected, no reverse door, no outbound sync client, and no replace-all
on the registry. Those five are what this spec adds, in `pkg/egress`
where a second consumer exists and in the `egress` role otherwise; the
index's Open table carries them. The hosted platform runs a CONNECT
gateway on the package, and a sibling application platform runs a
reverse-door gateway on it and rejected transparent interception for
its runtimes, a finding this spec keeps by offering both doors. Two
defects the reference designs had are fixed here: two secrets on one
host are refused rather than silently collapsed, and placeholders are
minted per sandbox rather than derived from names a neighbor could
guess.

## Design

### The Secret kind

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: Secret
metadata:
  name: github-token
  labels: {team: research}
spec:
  kind: static                        # static | oauth_client_credentials
  scope:
    hosts: ["api.github.com", "github.com", "*.githubusercontent.com"]
    ports: [443]                      # default [443]; 80 is allowed and warned
  inject:
    header: Authorization             # default; any header name; exclusive with query
    scheme: bearer                    # bearer | basic | raw; default derived: bearer on Authorization, raw elsewhere
    query: ""                         # a query parameter name instead of a header
    body: false                       # opt into body substitution for small JSON, form, and text bodies
  oauth:                              # oauth_client_credentials only
    tokenUrl: https://login.example.com/oauth2/token
    scope: "read:repo"
    audience: ""
  value: ghp_...                      # write-only: accepted on apply, never returned
  # for oauth_client_credentials, `value` is `clientId:clientSecret`, split on the first colon
status:
  id: sec_01J9...
  owner: https://login.example.com|alice
  version: 3                          # bumps on every value change
  createdAt: ...
  updatedAt: ...
  mountedBy: 2                        # derived at read: sandboxes whose desired state names it, not Deleting
```

Rules:

- `value` is write-only. `GET` returns the object with `spec.value`
  absent; list never returns values; no event or record carries one.
  Stored under envelope encryption by [[010-state]] under
  `CELLA_SECRET_KEY`.
- `scope.hosts` is required and non-empty, under the host rule of
  [[003-manifest-contract]]: exact names or one leading `*.` wildcard
  matched by `hostmatch`, normalized the same way, with IP literals,
  single labels, and loopback, link-local, or private ranges refused,
  so a secret cannot be aimed at the cluster. There is no "any host"
  scope, because a value sent anywhere is a value the sandbox
  effectively holds.
- `inject` names exactly one place: a header or a query parameter.
  `scheme` is the encoding of the substituted value and nothing more:
  `basic` substitutes the base64 of `user:pass` (split on the first
  colon); `bearer` and `raw` substitute the value verbatim, and the
  client writes the surrounding `Bearer ` itself, since substitution
  replaces the placeholder where the client put it. `body: true` opts
  into the body rule below.
- Ownership is the applying subject. Whether another subject may mount
  a secret is the authorizer's `secret.mount` decision
  ([[006-identity]]).
- Updating `value` bumps `status.version` and reaches every gateway
  holding a map that carries it, without a sandbox restart; the
  placeholder is unchanged. Deleting a secret removes it from every map
  and marks it `notInjectable` on every sandbox that mounts it.

### What the sandbox sees

For each `spec.secrets[]` entry `{name, env}`, the sandbox's environment
carries `env=<placeholder>`, minted by `pkg/egress.MintPlaceholder` per
sandbox per secret at create: `cph_` and 32 characters of `[a-z2-7]`, a
value and never a key, so no reserved-name rule touches it. It is
recorded only in the sandbox's desired state and the gateway's map;
`RebuildMap` of the package, which exists for a consumer that did not
retain placeholders, is unused here. When the secret injects into a
header other than `Authorization` or into a query parameter, a
companion `<env>_HEADER=<name>` or `<env>_QUERY=<name>` says where to
put the placeholder. `status.secrets.mounted` lists the names;
`status.secrets.notInjectable` lists any whose secret was deleted, or
whose every host is off the allow list or on the deny list, so the
caller knows those requests will leave unauthenticated rather than
fail silently.

The gateway credential: `Compile` mints, once per sandbox at create,
a random 32-byte credential that lives as long as the sandbox and
travels in the map. It is what both doors authenticate, so nothing the
gateway checks ever rotates under a running process.

| Door | The sandbox uses | For |
|---|---|---|
| proxy | `HTTPS_PROXY=http://sandbox:<credential>@<gateway>:<port>`, the same in `HTTP_PROXY`, `NO_PROXY` for loopback and the mesh, and their lowercase forms; the CA projected read-only at `/run/cella/egress-ca.pem` and named by `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE` | every client that honors proxy variables and the trust variables; stock clients send the URL's userinfo as `Proxy-Authorization: Basic`, which the gateway reads |
| reverse | `CELLA_GATEWAY_URL=http://<gateway>:<port>` and the convention `$CELLA_GATEWAY_URL/<host>/<path>` over plain HTTP inside the environment, with the header `Cella-Egress-Credential: <credential>`; `CELLA_GATEWAY_CREDENTIAL` carries the value | runtimes that ignore proxy variables or private CAs: Node's built-in fetch, the JVM, Bun; the gateway names the destination from the first path segment, so `Authorization` stays free for the upstream's placeholder |

The keys above are reserved ([[003-manifest-contract]]); `egress`
exports the list as `ReservedEnv` and `TestReservedKeysMatchTheGateway`
holds the two copies equal. A program that uses neither door reaches
nothing, because the driver's network rule admits only the gateway,
DNS, and the control plane.

### The map

`egress.Compile(resolved v1.Sandbox, secrets []v1.Secret, credential []byte) (Map, []string)`
produces the gateway's view of one sandbox and the `notInjectable`
names:

```go
type Map struct {
	Principal  string        // "sandbox:<sbx_ id>"
	Version    int64         // per principal, assigned by the control plane, bumped on every Send
	Credential []byte        // what both doors authenticate
	Mode       v1.EgressMode
	Allow      []string      // mode allowlist: allowedHosts plus every mounted secret's hosts
	Deny       []string      // mode open: deniedHosts; a denied host wins over a secret's scope
	Entries    []Entry       // one per injectable secret: placeholder, kind, hosts, ports, inject, body flag, value or oauth
}
```

`Compile` is pure and runs in the control plane; the value is decrypted
only here, through `Values.Open` of [[010-state]], only to be sent. A
secret whose every host is denied or off the allow list gets no entry
and is named in the second return, which the controller writes into
`status.secrets.notInjectable`. Two entries sharing a host is refused
at resolve as `secret_host_conflict` ([[003-manifest-contract]]).
`Version` is a counter in the sandbox's status, incremented by the
controller on every `Send`, so a value update that changes no sandbox
field still produces a higher version, and a gateway applies a map if
and only if its version is above the one it holds for that principal.
DNS and the control plane are not in `Allow`: they are admitted by the
driver's network rule, which never routes them through the gateway.
Every map of an environment is a function of desired state, secret
values, and the credentials in desired state, so the control plane can
always produce the full set for a gateway that connects with nothing.

`egress` at the module root imports `manifest/v1` and
`pkg/egress/placeholder`, a dependency-free subpackage holding
`MintPlaceholder` and `IsPlaceholder` that this spec asks `pkg/egress`
to split out, so the root package keeps [[001-architecture]]'s rule
that it dials nothing.

### Sync

The gateway is `cellad egress`. It reads `CELLA_URL`,
`CELLA_ENVIRONMENT_KEY`, `CELLA_EGRESS_PROXY_ADDR`,
`CELLA_EGRESS_REVERSE_ADDR`, and `CELLA_EGRESS_CA_KEY`, and opens one
WebSocket to `GET /v1/environments/{id}/egress` ([[008-api]]) with the
environment key as its bearer, subprotocol `cella.egress.v1`, framed
as one JSON text message per line:

| Message | Direction | Meaning |
|---|---|---|
| `hello {gatewayId, principal?, versions: {principal: version}}` | up, first | who connected, an optional single-principal filter for a sidecar, and what it holds |
| `snapshot {maps: [...]}` | down, in reply | every map of the environment, or the one filtered; authoritative: the gateway replaces its whole registry and drops every principal absent from it |
| `put {map}` | down | a new or changed map; applied when its version is higher |
| `purge {principal}` | down | the map is gone |
| `ack {principal, version}` | up | applied, or already held at that version |
| `heartbeat` | both, every 15 seconds | a connection with none for 45 seconds is closed by either side |
| `record {...}` | up | one connection handled (below) |

The control plane keeps, per environment, the set of connected
gateways and, per connection and principal, the highest version
acknowledged. A `put` or `purge` fans out to every connection of the
environment and is retried on each until acknowledged; a connection
that drops is forgotten, and its replacement's `hello` and `snapshot`
make it whole. A reconnect after any interruption therefore loses
nothing, a purge included, and the control plane never needs an
inbound route. Several gateways per environment each hold the full
set; a per-pod sidecar (`CELLA_EGRESS_SIDECAR=1` on k8s) holds one.
The same protocol serves the in-process environment over loopback and
a self-hosted one over the internet; there is no second mechanism.

Provisioning at create is [[005-lifecycle-controller]]'s step 3,
`Egress.Send`: the map is sent and acknowledged by at least one
connected gateway of the environment before the driver is called, so
a sandbox never starts before a gateway knows it; the remaining
connections catch up by retry. When no gateway is connected, or none
acknowledges within `CELLA_EGRESS_ACK_TIMEOUT` (default `5s`), the
create fails with `driver_unavailable`, whose sentence, "The
environment is unavailable", is exact, since the gateway is part of
the environment's data plane, and the environment's phase becomes
`Degraded` with reason `NoGateway` ([[021-data-plane-workers]]). In
sidecar mode no gateway exists before the Pod, so the gate moves
inside the driver's `Create`: the sidecar starts first, connects, and
acknowledges its one map before the workload container is allowed to
start, which the native sidecar's start ordering already provides.

### Telemetry

On the same stream the gateway sends one record per connection it
handled:

```json
{"type": "record", "principal": "sandbox:sbx_01J9...", "at": "...", "door": "proxy",
 "host": "api.github.com", "port": 443, "decision": "allowed",
 "substituted": ["github-token"], "method": "GET", "path": "/repos/example/repo",
 "status": 200, "bytesOut": 812, "bytesIn": 40213, "durationMs": 231}
```

`decision` is `allowed`, `denied` (off the allow list or on the deny
list), `unknown` (no map for the principal), or `passthrough` (a host
in scope with no secret, tunneled without termination). `method`,
`path`, and `status` are present only where the gateway saw them, on
the reverse door and on terminated connections; `path` carries no
query string. No record carries a header, a body, a value, a
placeholder, or the credential. Records have a store surface of their
own, `Records` in [[010-state]]: a Postgres table with retention
`CELLA_EGRESS_RECORDS_RETENTION` (default `168h`) and a memory ring of
`CELLA_EGRESS_RECORDS_CAP` (default `1000`) per sandbox, never the
journal, so a busy sandbox evicts none of its lifecycle events.
Records serve `GET /v1/sandboxes/{id}/egress` ([[008-api]]) newest
first, feed `cella_egress_connections_total` and
`cella_egress_bytes_total` with `decision` and `door` labels
([[017-observability]]), and, with `CELLA_EVENTS_EGRESS=1`, are also
appended to the journal as `sandbox.egress` events for the sink
([[009-events]]); the default is off, because a platform that wants
them has the records and the metrics regardless.

### The gateway

`cellad egress` runs the proxy door on `CELLA_EGRESS_PROXY_ADDR`
(`pkg/egress.Gateway` with `pkg/egress.CA` under a policy gate) and
the reverse door on `CELLA_EGRESS_REVERSE_ADDR`, both over one
`Registry` the sync stream fills. On both doors the caller is the
principal whose credential matches: `Proxy-Authorization: Basic` with
the URL's userinfo on the proxy door, `Cella-Egress-Credential` on the
reverse door; a request with no matching credential is refused before
any dial. `TokenAuth` of the package is not used, since a credential
in the map needs no key set and never rotates.

| Environment | Gateway | Network rule |
|---|---|---|
| k8s | one Deployment per namespace, or a per-pod sidecar with `CELLA_EGRESS_SIDECAR=1` | a NetworkPolicy per sandbox admits DNS, the gateway, and `cellad`; nothing else; ingress only from mesh peers |
| podman | one container per host on the sandboxes' network | per-sandbox network with the gateway as the only route |
| local | one process per host on loopback | the sandbox runtime's own allow-only proxy admits exactly one domain, the gateway's; the sandbox's `HTTPS_PROXY` dials the gateway, so the runtime's policy never sees a real host and `open` cannot be expressed through it, which is why [[004-runtime-contract]]'s `local` row omits `open` |
| native | one process per host on loopback | unenforced; `EgressEnforced` false |
| a worker's environment | `cellad egress` on the worker's side, connecting to the same control plane with the same environment key | the worker's driver's rule |

`Environment.spec.gateway` ([[021-data-plane-workers]]) is the address
sandboxes reach the gateway at, and is what `CreateSpec.Egress.GatewayURL`
carries.

### What is enforced, and by what

What `pkg/egress` enforces today, restated because it is the security
property:

- A placeholder is replaced with its value if and only if the request's
  destination host matches one of that entry's hosts. Sent anywhere
  else, the placeholder passes through verbatim: an exfiltration
  attempt leaks an opaque token.
- Header values and the raw query string are in scope. The framing and
  hop-by-hop headers (`Content-Length`, `Transfer-Encoding`, `TE`,
  `Trailer`, `Connection`, `Keep-Alive`, `Upgrade`,
  `Proxy-Connection`, `Proxy-Authorization`, every `X-Forwarded-*`) are
  never rewritten.
- The body is in scope only for entries with `body: true`, and only
  when the body is small and textual: a known `Content-Length` at most
  `DefaultMaxBodyBytes` and a JSON, form, or text content type. A
  streaming, chunked, larger, or binary body is never read.
- A host with an entry is terminated and substituted; a host without
  one is tunneled untouched on the proxy door, so a sandbox's own
  client certificates and pinned trust keep working there. The reverse
  door always terminates, since the sandbox spoke plain HTTP to it.

What the gateway role adds, and the tests of this spec prove:

- The policy gate: `mode: allowlist` refuses a connection to a host not
  on `Allow`; `mode: open` refuses one to a host on `Deny`; `mode:
  none` refuses every connection; each before any bytes flow, with 403
  on the reverse door and a refused CONNECT on the proxy door.
- Ports: an entry substitutes only toward its `scope.ports`.
- Placement: an entry's value replaces its placeholder only in the
  header or query parameter its `inject` names, and in the body when
  opted in; a placeholder elsewhere in the request passes verbatim.
- The reverse door, the credential check on both doors, the sync
  client, and the authoritative snapshot.

### The boundary never widens

The map and the driver's rule enforce the boundary twice, and the
effective boundary is their intersection, so an update in either
order keeps every instant within both the old and the new manifest
([[005-lifecycle-controller]]). After create, any caller may narrow
`mode`, remove an allowed host, add a denied host, or remove a secret;
only a non-workload actor may widen any of them
([[003-manifest-contract]]). A spawned child's `Allow`, `Deny`, and
secrets are held to its parent's by the boundary check
([[022-mesh-and-spawn]]). A stop and start replays the same map from
desired state.

### Package layout

`egress` at the module root holds `Compile`, `Map`, `Entry`,
`ReservedEnv`, the sync message types, and the record type, and
imports `manifest/v1` and `pkg/egress/placeholder`. `internal/egressd`
is the `egress` role of `cellad`: the policy gate over
`pkg/egress.Gateway`, the CA, the reverse door, the sync client, and
the record emitter, with its own dependency allow list. `internal/api`
serves the sync stream and the records route. Nothing else touches a
value.

## Not in this spec

The `Volume` kind ([[019-volumes]]); the worker's own stream
([[021-data-plane-workers]]); the subset rule for children
([[022-mesh-and-spawn]]); the metrics' full table
([[017-observability]]); the `Records` store surface's columns
([[010-state]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `GET` and list of a Secret never return `spec.value`; no event or record carries a value, a placeholder, or the credential; a canary value appears in no log | `TestSecretRoutes`, `TestSecretVersionRules`, e2e `TestSecretValuesNeverEnterASandbox` | [[046-secret-kind]]: passing |
| A scope with an IP, a single label, or a private range is `invalid_field`; an empty scope is `missing_field`; `header` with `query` is `exclusive_fields` | `TestSecretRefusals` | [[046-secret-kind]]: passing |
| `Compile` produces one entry per injectable secret with a per-sandbox placeholder and one credential; two sandboxes mounting one secret get different placeholders; every mounted secret's hosts are on `Allow`; a secret whose hosts are all denied gets no entry and is named `notInjectable` | `TestCompileCarriesTheBoundary`, `TestCompileCarriesTheValueAndTheKind`, `TestTheMapCarriesTheValue` | [[039-egress-gateway]]: `TestCompile*`; [[046-secret-kind]]: `TestCompileCarriesTheValueAndTheKind`, `TestTheMapCarriesTheValue`, `TestASecretTheBoundaryDeniesIsNotInjectable` |
| A gateway that connects with nothing receives every map; one that missed a purge while disconnected drops that principal on reconnect; a sidecar receives one; two gateways each hold the full set and a put unacknowledged by the second is retried until it acks | `TestSnapshotIsAuthoritative`, `TestASnapshotIsSentOnConnect`, `TestASidecarReceivesOneMap` | [[039-egress-gateway]]: `TestASnapshotIsSentOnConnect`, `TestSendReturnsOnTheFirstAcknowledgment`, `TestASidecarReceivesOneMap`, `TestSnapshotIsAuthoritative` |
| A put with a version at or below the held one is acknowledged and not applied; a value update produces a higher version and lands | `TestApplyTakesOnlyAHigherVersion`, `TestAPutAppliesAndIsAcknowledged` | [[039-egress-gateway]]: `TestApplyTakesOnlyAHigherVersion`, `TestAPutAppliesAndIsAcknowledged` |
| With no gateway connected, or none acknowledging within the timeout, the create fails with `driver_unavailable`, no sandbox is left, and the environment is `Degraded NoGateway`; in sidecar mode the workload container does not start before the sidecar's ack | `TestCreateWaitsForTheGateway` | [[039-egress-gateway]]: `TestCreateWaitsForTheGateway`, less the sidecar and the Degraded phase |
| A placeholder sent to an in-scope host and port is substituted in the header or query its `inject` names, or the body when opted in, on both doors; sent to any other host, port, or place it passes through verbatim | `TestSubstitutionIsScopedAndPlaced`, `TestSubstitutionHoldsThePortScope` against the gateway | [[046-secret-kind]]: passing, less placement on the proxy door, which `pkg/egress` has no seam for |
| Framing headers are never rewritten; a chunked, a large, and a binary body are never read | `TestBodySubstitutionIsOptIn`, `TestSecretRefusals` | [[046-secret-kind]]: `TestBodySubstitutionIsOptIn` over the opt-in and the binary body; a framing header is refused at apply by `TestSecretRefusals`, and the rest is `pkg/egress`'s own suite |
| `basic` substitutes base64 of `user:pass`; `bearer` and `raw` substitute verbatim and the upstream sees one `Bearer ` | `TestValueOf`, `TestSubstitutionIsScopedAndPlaced` | [[046-secret-kind]]: passing |
| An `oauth_client_credentials` secret mints a token at the token URL and caches it; the client secret never leaves the gateway | `TestOAuthKind` against a stub token endpoint | [[046-secret-kind]]: passing, with the cache held across a map at a higher version |
| `none` refuses every connection; `allowlist` refuses an unlisted host; `open` refuses a denied host and admits the rest, with 403 on the reverse door and a refused CONNECT on the proxy door | `TestGateMatrix`, `TestEgressEndToEnd` | [[039-egress-gateway]]: `TestGateMatrix`, `TestEgressEndToEnd` |
| A request on either door without the sandbox's credential is refused before any dial; the credential does not change over the sandbox's life while the workload token rotates | `TestProxyDoorNeedsTheSandboxsOwnCredential`, `TestReverseDoor`, `TestCredentialAuthenticate` | [[039-egress-gateway]]: `TestProxyDoorNeedsTheSandboxsOwnCredential`, `TestReverseDoor`, `TestCredentialAuthenticate` |
| Every connection yields one record with the fields named and never a header, body, value, placeholder, or credential; a sandbox's records are served newest first; the counters move; under a flood the ring keeps the cap and the journal's lifecycle events are untouched; events reach the sink only with `CELLA_EVENTS_EGRESS=1` | `TestRecordNormalize`, `TestRecordsAreKeptPerSandboxNewestFirst`, `TestTheRecordsRouteIsTheSandboxOwnersToRead` | [[039-egress-gateway]]: the fields, the ring and the route; the counters, the journal and the sink are open |
| Updating a value reaches a running sandbox's next request without a restart; deleting it marks the sandbox `notInjectable` and its next request leaves unauthenticated | `TestLiveUpdateAndRevoke`, `TestRotationAndRevocationReachTheGateway` | [[046-secret-kind]]: passing |
| A workload token cannot add a host, remove a denied host, or add a secret to its own sandbox; the owner can | `TestNarrowingIsForWorkloads`, `TestAWorkloadCannotMountASecret` | [[039-egress-gateway]]: `TestNarrowingIsForWorkloads`; [[046-secret-kind]]: `TestAWorkloadCannotMountASecret` |
| On `local`, the sandbox runtime's policy names only the gateway and a request to an allowed host succeeds through the chain | `TestLocalEgress` | not built |
| On k8s, a sandbox reaches an allowed host through its proxy door and cannot reach a disallowed host, another sandbox, or an upstream around the gateway | `TestClusterEgressBoundary`, `TestClusterNoLateralMovement`, conformance case `case018EgressEnforced` | [[070-k8s-egress]]: passing against a local kind cluster, and both kind jobs require it; the reverse door on the cluster is open, since it reaches an upstream over TLS on 443 and the tier's upstream serves neither; the Pod network through the gateway is the gateway's own egress rule, which the operator writes |
| The reserved key list in `egress` equals `manifest`'s; `egress` imports no package that dials | `TestReservedKeysMatchTheGateway`, `TestRootPackagesDialNothing` | [[039-egress-gateway]]: `TestReservedKeysMatchTheGateway`, `TestRootPackagesDialNothing` |
