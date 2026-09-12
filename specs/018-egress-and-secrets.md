---
title: "Egress and secrets: the Secret kind, placeholders, the gateway, the boundary a workload cannot widen"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
affects: [manifest/v1/, egress/, internal/egressd/, internal/api/, internal/store/, runtime/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Egress and secrets

## Overview

A sandbox reaches the network through one door, an egress gateway
beside it, and holds no credential of its own: every secret it needs
is a placeholder in its environment that the gateway swaps for the
value on the way out, and only toward a host the secret's owner named.
The scope of a secret travels with the secret. A manifest chooses which
secrets to mount and may narrow where the sandbox may go; it cannot
widen either. This is the network boundary of the control plane, and
the mechanism that makes "a sandbox reaches only the hosts its manifest
names" a property rather than a policy.

The gateway is the `egress` role of `cellad`, built on `latere.ai/x/pkg/egress`, whose
substitution engine is a pure, exhaustively tested core with the
CONNECT proxy and the certificate authority layered on top. This spec
fixes the `Secret` kind, how a manifest's secrets and egress rules
compile into the gateway's map, how the gateway is placed in each
environment, what the sandbox sees, and what is rewritten and what is
never rewritten.

## Current state

Not built. `pkg/egress` exists with its map, placeholder, static and
OAuth client-credentials kinds, the ingest API, the gateway, and the
CA. The hosted platform runs an earlier gateway on it with a vault of
its own. Two designs informed this one: one that scoped a credential to
hosts on the credential itself, and one that let a manifest name hosts
freely; the first is what this spec keeps, with two defects the
reference designs had fixed here: two secrets on one host are refused
rather than silently collapsed, and placeholders are minted per
sandbox rather than derived from names a neighbour could guess.

## Design

### The Secret kind

```yaml
apiVersion: cella.latere.ai/v1
kind: Secret
metadata:
  name: github-token
  labels: {team: research}
spec:
  kind: static                        # static | oauth_client_credentials
  scope:
    hosts: ["api.github.com", "github.com", "*.githubusercontent.com"]
    ports: [443]                      # default [443]; 80 is allowed but warned
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
  owner: alice@example.com
  version: 3                          # bumps on every value change
  createdAt: ...
  updatedAt: ...
  mountedBy: 2                        # sandboxes holding a placeholder for it now
```

Rules:

- `value` is write-only. `GET` returns the object with `spec.value`
  absent; list never returns values; no event carries one. Stored
  envelope-encrypted: a random data key per secret under AES-256-GCM,
  the data key wrapped by `CELLA_SECRETS_KEK` (32 bytes, base64; a
  start-up failure when absent and any secret exists). Rotation of the
  KEK rewraps every data key in one migration and never rewrites a
  value.
- `scope.hosts` is required and non-empty, under the host rule of
  [[003-manifest-contract]]: exact names or one leading `*.` wildcard
  matched by `hostmatch`, normalized the same way, with IP literals,
  single labels, and loopback, link-local, or private ranges refused
  with `invalid_field`, so a secret cannot be aimed at the cluster. There is no "any host" scope, because a value sent
  anywhere is a value the sandbox effectively holds.
- `inject` names exactly one place. `basic` expects `value` as
  `user:pass` and the gateway base64-encodes it; `raw` writes the value
  verbatim; `bearer` prefixes `Bearer `. `body: true` opts into the
  body rule below. A `query` injection is scoped to the named parameter
  only.
- Ownership is the applying subject. Whether another subject may mount
  a secret is the authorizer's decision, asked as `secret.mount` with
  the secret as the resource, so a platform models personal and shared
  secrets in its own terms; the built-in owner policy allows the owner
  and admins.
- Updating `value` bumps `status.version` and pushes the new value to
  every gateway holding a map for a sandbox that mounts it, without a
  sandbox restart; the placeholder is unchanged. Deleting a secret
  purges it from every map and marks it `notInjectable` on every
  sandbox that mounts it.

### What the sandbox sees

For each `spec.secrets[]` entry `{name, env}`, the sandbox's environment
carries `env=<placeholder>`, where the placeholder is minted by
`pkg/egress.MintPlaceholder` per sandbox per secret at create,
unguessable, and recorded only in the sandbox's desired state and the
gateway's map. When the secret injects into a header other than
`Authorization` or into a query parameter, a companion
`<env>_HEADER=<name>` or `<env>_QUERY=<name>` is set so a program knows
where to put the placeholder. `status.secrets.mounted` lists the names;
`status.secrets.notInjectable` lists any whose secret was deleted or
whose scope no longer has a host the sandbox may reach, so the caller
knows those requests will leave unauthenticated rather than fail
silently.

The environment also carries the gateway's address in `HTTP_PROXY`,
`HTTPS_PROXY`, `NO_PROXY` and their lowercase forms, and the gateway's
CA in the trust-store variables `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
`REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`, with the CA
file projected read-only at `/run/cella/egress-ca.pem`. `egress`
exports this list as `ReservedEnv`; `manifest` holds a copy and
`TestReservedKeysMatchTheGateway` asserts they are equal
([[003-manifest-contract]]). A program that ignores them
reaches nothing, because the environment's network rule admits only
the gateway, DNS, and the control plane.

### The map

`egress.Compile(resolved v1.Sandbox, secrets []v1.Secret) (Map, []Warning)`
produces the gateway's view of one sandbox:

```go
type Map struct {
	Principal string   // "sandbox:<id>"
	Mode      Mode     // open | allowlist | none
	Allow     []string // allowedHosts plus every mounted secret's hosts, plus DNS and the control plane
	Entries   []Entry  // one per mounted secret: placeholder, kind, hosts, ports, inject, body flag, value or oauth
}
```

Compile is pure and runs in the control plane; the value is decrypted
only here, only to be pushed. Two entries sharing a host is refused at
resolve as `secret_host_conflict` ([[003-manifest-contract]]), because
one route carries one injection and a silent collapse is how a
credential goes to the wrong header.

The map reaches the gateway through `pkg/egress`'s ingest contract,
`PUT /internal/maps/{principal}`, authenticated with a bearer the
gateway holds (`CELLA_EGRESS_INGEST_TOKEN`); a directly driven
environment's gateway is dialed by the controller, a worker's gateway
is fed by the worker from the operation it claimed
([[021-data-plane-workers]]), so the control plane never dials into a
self-hosted plane. `DELETE` purges on sandbox delete.

### The gateway

`cellad egress` runs `pkg/egress.Gateway` with `pkg/egress.CA`: a
CONNECT proxy that terminates TLS with a per-environment authority,
authenticates the caller by the sandbox's workload token presented as
proxy credentials (`pkg/egress.TokenAuth` against `cellad`'s key set,
requiring the audience `cella-egress`, which every workload token
carries beside the control plane's own ([[006-identity]])), looks up
the principal's map, and forwards.
Placement per environment:

| Environment | Gateway | Network rule |
|---|---|---|
| k8s | one Deployment per namespace, or a per-pod sidecar when the operator sets `CELLA_EGRESS_SIDECAR=1`; the token reaches it as `Proxy-Authorization` | a NetworkPolicy per sandbox admits DNS, the gateway, and `cellad`; nothing else; ingress only from mesh peers |
| podman | one container per host on the sandboxes' network | per-sandbox network with the gateway as the only route |
| native | one process per host on loopback | the driver's OS sandbox denies every host but the proxy (`local` driver) or warns that it cannot (`native` driver) |
| a worker's environment | whatever the worker's host runs, registered as the environment's gateway | the worker's driver's rule |

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
  streaming, chunked, larger, or binary body is never read, so uploads
  and SSE are never buffered.
- `mode: allowlist` refuses a CONNECT to a host not on `Allow` with 403
  before any bytes flow; `mode: none` refuses every CONNECT; `mode:
  open` admits every host and still substitutes only in scope.
- A host on `Allow` that no entry scopes is a plain tunnel: TLS is
  passed through, not terminated, so the sandbox's own client
  certificates and pinned trust keep working for hosts that carry no
  secret.

### The boundary never widens

At create the controller provisions in an order where every partial
failure leaves no running workload rather than partial policy: the map
is pushed to the gateway, volumes are attached, then the driver creates
the sandbox, and inside `Create` a driver applies the network rule
before it starts the workload (a NetworkPolicy before the Pod, a
per-sandbox network before the container, the sandbox runtime's
allowlist before the process), so the token the sandbox holds
authenticates to a gateway whose map already exists behind a rule
already in force. After create, a
manifest update may narrow `allowedHosts` or `mode` and may remove a
secret; it may not add a host, loosen the mode, or add a secret whose
hosts are not already on `Allow` unless the caller is the owner acting
through the API, never a workload acting through its own token. A
spawned child's `Allow` and secrets are subsets of its parent's
([[022-mesh-and-spawn]]). The gateway re-reads its map on every push and
the environment's rule on every reconcile, so a change lands without a
restart and a stop and start replays the same boundary from desired
state.

### Package layout

`egress` at the module root holds `Compile`, `Map`, `Entry`, and the
wire types shared with the gateway, and imports `manifest/v1` and
`pkg/egress`. `internal/egressd` is the `egress` role of `cellad`:
`pkg/egress.Gateway` plus configuration and the ingest listener, with
its own dependency allow list in the gate. `internal/store` holds the
encrypted values. Nothing else touches a value.

## Not in this spec

The `Volume` kind ([[019-volumes]]); how a worker feeds its gateway
([[021-data-plane-workers]]); the subset rule for children
([[022-mesh-and-spawn]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `GET` and list of a Secret never return `spec.value`; no event carries it; a canary value appears in no log | `TestSecretValueIsWriteOnly`, `TestNoSecretLeaks` | not built |
| Values are stored under a per-secret data key wrapped by the KEK; a KEK rotation rewraps without rewriting values | `TestEnvelopeEncryption`, `TestKEKRotation` | not built |
| A scope with an IP, a loopback, or a private range is `invalid_field`; an empty scope is `missing_field` | `TestScopeRefusals` | not built |
| `Compile` produces one entry per mounted secret with a per-sandbox placeholder; two sandboxes mounting one secret get different placeholders | `TestCompilePlaceholdersArePerSandbox` | not built |
| Every mounted secret's hosts are on `Allow`; a host on `Allow` with no entry is a passthrough tunnel | `TestAllowListAndPassthrough` against the gateway | not built |
| A placeholder sent to an in-scope host is substituted in the header, or the query, or the body as declared; sent to any other host it passes through verbatim | `TestSubstitutionIsDestinationScoped` against the gateway | not built |
| Framing headers are never rewritten; a chunked, a large, and a binary body are never read | `TestBodyRule` | not built |
| `basic` base64-encodes `user:pass`; `raw` writes verbatim; `bearer` prefixes | `TestSchemes` | not built |
| An `oauth_client_credentials` secret mints a token at the token URL and caches it; the client secret never leaves the gateway | `TestOAuthKind` against a stub token endpoint | not built |
| Updating a value reaches a running sandbox's next request without a restart; deleting it marks the sandbox `notInjectable` and its next request leaves unauthenticated | `TestLiveUpdateAndRevoke` | not built |
| Provisioning fails at each of the three steps in turn and the sandbox has no egress in every case | `TestFailClosedOrdering` | not built |
| A workload token cannot add a host or a secret to its own sandbox; the owner can narrow both | `TestBoundaryNeverWidens` | not built |
| On k8s, a sandbox reaches an allowed host through the gateway and cannot reach a disallowed host, another sandbox, or the Pod network | e2e `TestClusterEgressBoundary` | not built |
| `mode: none` refuses every CONNECT; `mode: open` admits every host and substitutes only in scope | `TestModes` | not built |
