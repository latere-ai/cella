---
title: "Egress and secrets: the Secret kind, placeholders, the gateway as a data plane component, sync and telemetry, the boundary a workload cannot widen"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/010-state.md
affects: [manifest/v1/, egress/, internal/egressd/, internal/api/, internal/store/, internal/config/, runtime/]
effort: large
created: 2026-09-12
updated: 2026-09-13
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
the protocol. The engine is `latere.ai/x/pkg/egress`, a pure
substitution core with the CONNECT proxy and the certificate authority
layered on top; this spec fixes the `Secret` kind, how a manifest
compiles into a map, the two doors, the sync and telemetry protocol,
what is rewritten and what never is, and how the boundary stays fixed.

## Current state

Not built. `pkg/egress` exists with its map, placeholder, static and
OAuth client-credentials kinds, the ingest API, the gateway, and the
CA; the hosted platform runs a CONNECT gateway on it, and a sibling
application platform runs a reverse-door gateway on the same engine
and rejected transparent interception for its runtimes, a finding this
spec keeps by offering both doors. Two defects the reference designs
had are fixed here: two secrets on one host are refused rather than
silently collapsed, and placeholders are minted per sandbox rather
than derived from names a neighbour could guess. The sync protocol and
the reverse door are candidates for extraction into `pkg/egress`, so
the sibling platform's gateway can adopt them; that is recorded as
open in the index.

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
    header: Authorization             # default; any header name
    scheme: bearer                    # bearer | basic | raw; default derived: bearer on Authorization, raw elsewhere
    query: ""                         # a query parameter name instead of a header; exclusive with header
    body: false                       # opt into body substitution for small JSON, form, and text bodies
  oauth:                              # oauth_client_credentials only
    tokenUrl: https://login.example.com/oauth2/token
    scope: "read:repo"
    audience: ""
  value: ghp_...                      # write-only: accepted on apply, never returned
  # for oauth_client_credentials, `value` is `clientId:clientSecret`
status:
  id: sec_01J9...
  owner: https://login.example.com|alice
  version: 3                          # bumps on every value change
  createdAt: ...
  updatedAt: ...
  mountedBy: 2                        # sandboxes holding a placeholder for it now
```

Rules:

- `value` is write-only. `GET` returns the object with `spec.value`
  absent; list never returns values; no event carries one. Stored under
  envelope encryption by [[010-state]] under `CELLA_SECRETS_KEK`.
- `scope.hosts` is required and non-empty, under the host rule of
  [[003-manifest-contract]]: exact names or one leading `*.` wildcard
  matched by `hostmatch`, normalized the same way, with IP literals,
  single labels, and loopback, link-local, or private ranges refused,
  so a secret cannot be aimed at the cluster. There is no "any host"
  scope, because a value sent anywhere is a value the sandbox
  effectively holds.
- `inject` names exactly one place. `basic` expects `value` as
  `user:pass` and the gateway base64-encodes it; `raw` writes the value
  verbatim; `bearer` prefixes `Bearer `. `body: true` opts into the
  body rule below. A `query` injection is scoped to the named parameter.
- Ownership is the applying subject. Whether another subject may mount
  a secret is the authorizer's `secret.mount` decision, so a platform
  models personal and shared secrets in its own terms; the owner policy
  allows the owner and admins ([[006-identity]]).
- Updating `value` bumps `status.version` and reaches every gateway
  holding a map that carries it, without a sandbox restart; the
  placeholder is unchanged. Deleting a secret removes it from every map
  and marks it `notInjectable` on every sandbox that mounts it.

### What the sandbox sees

For each `spec.secrets[]` entry `{name, env}`, the sandbox's environment
carries `env=<placeholder>`, minted by `pkg/egress.MintPlaceholder` per
sandbox per secret at create, unguessable, recorded only in the
sandbox's desired state and the gateway's map. When the secret injects
into a header other than `Authorization` or into a query parameter, a
companion `<env>_HEADER=<name>` or `<env>_QUERY=<name>` says where to
put the placeholder. `status.secrets.mounted` lists the names;
`status.secrets.notInjectable` lists any whose secret was deleted or
whose scope no longer has a host the sandbox may reach, so the caller
knows those requests will leave unauthenticated rather than fail
silently.

Two doors, one map:

| Door | The sandbox uses | For |
|---|---|---|
| proxy | `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase forms point at the gateway; the CA is projected read-only at `/run/cella/egress-ca.pem` and named by `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE` | every client that honours proxy variables and the trust variables: curl, git, pip, most SDKs |
| reverse | `CELLA_EGRESS_URL=http://<gateway>` and the convention `$CELLA_EGRESS_URL/<host>/<path>`, plain HTTP inside the environment | runtimes that ignore proxy variables or private CAs: Node's built-in fetch, the JVM, Bun; the gateway names the destination from the first path segment |

The keys above are reserved ([[003-manifest-contract]]); `egress`
exports the list as `ReservedEnv` and `TestReservedKeysMatchTheGateway`
holds the two copies equal. A program that uses neither door reaches
nothing, because the environment's network rule admits only the
gateway, DNS, and the control plane.

### The map

`egress.Compile(resolved v1.Sandbox, secrets []v1.Secret) (Map, []Warning)`
produces the gateway's view of one sandbox:

```go
type Map struct {
	Principal string      // "sandbox:<sbx_ id>"
	Version   int64       // the desired state's version; a gateway keeps the highest it saw
	Mode      v1.EgressMode
	Allow     []string    // mode allowlist: allowedHosts plus every mounted secret's hosts
	Deny      []string    // mode open: deniedHosts; a denied host wins over a secret's scope
	Entries   []Entry     // one per mounted secret: placeholder, kind, hosts, ports, inject, body flag, value or oauth
}
```

`Compile` is pure and runs in the control plane; the value is decrypted
only here, through the one method of [[010-state]], only to be sent.
Two entries sharing a host is refused at resolve as
`secret_host_conflict` ([[003-manifest-contract]]), because one route
carries one injection and a silent collapse is how a credential goes
to the wrong header. Every map of an environment is a function of
desired state and secret values, so the control plane can always
produce the full set for a gateway that connects with nothing.

### Sync

The gateway is `cellad egress`. It reads `CELLA_URL`,
`CELLA_ENVIRONMENT_KEY`, and its own listener addresses, and opens one
WebSocket to `GET /v1/environments/{id}/egress` ([[008-api]]) with the
environment key as its bearer, subprotocol `cella.egress.v1`. On
connect the control plane sends `{"type": "snapshot", "maps": [...]}`
with every map of the environment, then `{"type": "put", "map": ...}`
and `{"type": "purge", "principal": ...}` as desired state changes; the
gateway answers each with `{"type": "ack", "principal", "version"}`,
and the control plane keeps, per environment and gateway connection,
the highest version acknowledged. A reconnect after any interruption
replays the snapshot, so a gateway restart or a network cut loses
nothing and the control plane never needs an inbound route. A gateway
running as a per-pod sidecar (`CELLA_EGRESS_SIDECAR=1` on k8s) connects
with `?principal=sandbox:<id>` and receives that one map. Several
gateways per environment each hold the full set. The same protocol
serves the in-process environment over loopback and a self-hosted one
over the internet; there is no second mechanism.

Provisioning order at create is [[005-lifecycle-controller]]'s: the map
is sent and acknowledged by at least one gateway of the environment
before the driver is called, so a sandbox never starts before its
gateway knows it; a `put` that no gateway acknowledges within
`CELLA_EGRESS_ACK_TIMEOUT` (default `5s`) fails the create with
`driver_unavailable`, and the environment's `EgressReady` condition
goes false.

### Telemetry

On the same stream the gateway sends one record per connection it
handled, and the control plane writes it to the journal under the
sandbox with type `sandbox.egress`:

```json
{"type": "record", "principal": "sandbox:sbx_01J9...", "at": "...", "door": "proxy",
 "host": "api.github.com", "port": 443, "decision": "allowed",
 "substituted": ["github-token"], "method": "GET", "path": "/repos/example/repo",
 "status": 200, "bytesOut": 812, "bytesIn": 40213, "durationMs": 231}
```

`decision` is `allowed`, `denied` (off the allow list or on the deny
list), `unknown` (no map for the principal), or `passthrough` (a host
in scope with no secret, tunnelled without termination). `method`,
`path`, and `status` are present only where the gateway saw them, on
the reverse door and on terminated connections; `path` carries no
query string. No record carries a header, a body, a value, or a
placeholder. Records serve `GET /v1/sandboxes/{id}/egress`
([[008-api]]), feed the `cella_egress_connections_total` and
`cella_egress_bytes_total` metrics ([[017-observability]]) with
`decision` and `door` labels, and reach the sink as `sandbox.egress`
events when `CELLA_EVENTS_EGRESS=1` ([[009-events]]); the default is
off, because a busy sandbox produces thousands and a platform that
wants them has the journal and the metrics regardless.

### The gateway

`cellad egress` runs `pkg/egress.Gateway` with `pkg/egress.CA` on the
proxy door and a reverse-door handler on a second listener, both over
one `Registry` the sync stream fills. The proxy door authenticates the
caller by the sandbox's workload token presented as `Proxy-Authorization`
(`pkg/egress.TokenAuth` against `cellad`'s key set, audience
`cella-egress`, which every workload token carries beside the control
plane's own, [[006-identity]]); the reverse door authenticates the same
token as `Authorization`, or, on k8s and podman, resolves the caller
from the source address to its sandbox through the driver's stamped
identity when no token is present. Placement per environment:

| Environment | Gateway | Network rule |
|---|---|---|
| k8s | one Deployment per namespace, or a per-pod sidecar with `CELLA_EGRESS_SIDECAR=1` | a NetworkPolicy per sandbox admits DNS, the gateway, and `cellad`; nothing else; ingress only from mesh peers |
| podman | one container per host on the sandboxes' network | per-sandbox network with the gateway as the only route |
| local | one process per host on loopback | the sandbox runtime's allow-only proxy names the gateway as its one domain |
| native | one process per host on loopback | unenforced; `EgressEnforced` false |
| a worker's environment | `cellad egress` on the worker's side, connecting to the same control plane with the same environment key | the worker's driver's rule |

### What is rewritten

The rule is `pkg/egress`'s and is restated here because it is the
security property:

- A placeholder is replaced with its value if and only if the request's
  destination host matches one of that entry's hosts and its port one
  of its ports. Sent anywhere else, the placeholder passes through
  verbatim: an exfiltration attempt leaks an opaque token.
- Header values and the raw query string are in scope. The framing and
  hop-by-hop headers (`Content-Length`, `Transfer-Encoding`, `TE`,
  `Trailer`, `Connection`, `Keep-Alive`, `Upgrade`,
  `Proxy-Connection`, `Proxy-Authorization`, every `X-Forwarded-*`) are
  never rewritten.
- The body is in scope only for entries with `body: true`, and only
  when the body is small and textual: a known `Content-Length` at most
  `DefaultMaxBodyBytes` and a JSON, form, or text content type. A
  streaming, chunked, larger, or binary body is never read.
- `mode: allowlist` refuses a connection to a host not on `Allow`;
  `mode: open` refuses one to a host on `Deny`; `mode: none` refuses
  every connection; each before any bytes flow, with 403 on the reverse
  door and a refused CONNECT on the proxy door.
- A host in scope with no entry is a plain tunnel on the proxy door:
  TLS is passed through, not terminated, so the sandbox's own client
  certificates and pinned trust keep working for hosts that carry no
  secret. The reverse door always terminates, since the sandbox spoke
  plain HTTP to it.

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
imports `manifest/v1` and `pkg/egress`. `internal/egressd` is the
`egress` role of `cellad`: `pkg/egress.Gateway`, the CA, the reverse
door, the sync client, and the record emitter, with its own dependency
allow list. `internal/api` serves the sync stream and the egress
records route. Nothing else touches a value.

## Not in this spec

The `Volume` kind ([[019-volumes]]); the worker's own stream
([[021-data-plane-workers]]); the subset rule for children
([[022-mesh-and-spawn]]); the metrics' full table
([[017-observability]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `GET` and list of a Secret never return `spec.value`; no event or record carries a value or a placeholder; a canary value appears in no log | `TestSecretValueIsWriteOnly`, `TestNoSecretLeaks` | not built |
| A scope with an IP, a single label, or a private range is `invalid_field`; an empty scope is `missing_field` | `TestScopeRefusals` | not built |
| `Compile` produces one entry per mounted secret with a per-sandbox placeholder; two sandboxes mounting one secret get different placeholders; every mounted secret's hosts are on `Allow`; a denied host wins over a secret's scope | `TestCompile` | not built |
| A gateway that connects with nothing receives every map of its environment; after a cut it reconnects and receives them again; a sidecar receives one; two gateways each hold the full set | `TestSyncSnapshotAndReplay` | not built |
| A `put` acknowledged by no gateway within the timeout fails the create with `driver_unavailable` and no sandbox is left | `TestCreateWaitsForTheGateway` | not built |
| A placeholder sent to an in-scope host is substituted in the header, the query, or the body as declared, on both doors; sent to any other host it passes through verbatim | `TestSubstitutionIsDestinationScoped` against the gateway | not built |
| Framing headers are never rewritten; a chunked, a large, and a binary body are never read | `TestBodyRule` | not built |
| `basic` base64-encodes `user:pass`; `raw` writes verbatim; `bearer` prefixes | `TestSchemes` | not built |
| An `oauth_client_credentials` secret mints a token at the token URL and caches it; the client secret never leaves the gateway | `TestOAuthKind` against a stub token endpoint | not built |
| `none` refuses every connection; `allowlist` refuses an unlisted host; `open` refuses a denied host and admits the rest, with 403 on the reverse door and a refused CONNECT on the proxy door | `TestModes` | not built |
| The reverse door authenticates a token or, on k8s and podman, the source address, and refuses anything else | `TestReverseDoorCallers` | not built |
| Every connection yields one record with the fields named and never a header, body, value, or placeholder; the sandbox's records are served newest first; the counters move; events reach the sink only with `CELLA_EVENTS_EGRESS=1` | `TestTelemetry` | not built |
| Updating a value reaches a running sandbox's next request without a restart; deleting it marks the sandbox `notInjectable` and its next request leaves unauthenticated | `TestLiveUpdateAndRevoke` | not built |
| A workload token cannot add a host, remove a denied host, or add a secret to its own sandbox; the owner can | `TestBoundaryNeverWidens` | not built |
| On k8s, a sandbox reaches an allowed host through each door and cannot reach a disallowed host, another sandbox, or the Pod network | e2e `TestClusterEgressBoundary` | not built |
| The reserved key list in `egress` equals `manifest`'s | `TestReservedKeysMatchTheGateway` | not built |
