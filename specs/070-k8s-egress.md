---
title: "Egress on the k8s driver: a NetworkPolicy per sandbox, the declared modes, the gateway projected into the Pod, mesh peers under confinement, and the kind tier's gateway"
status: in-progress
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/012-test-stubs-and-tiers.md
  - specs/013-security-and-threat-model.md
  - specs/015-conformance-suite.md
  - specs/018-egress-and-secrets.md
  - specs/022-mesh-and-spawn.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/036-k8s-driver.md
  - specs/.archive/039-egress-gateway.md
  - specs/.archive/065-k8s-dial.md
affects: [runtime/k8s/, internal/config/, deploy/, test/conformance/, test/kind/, .github/workflows/, docs/, CHANGELOG.md, specs/, egress_test.go, specs_test.go]
effort: large
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# Egress on the k8s driver

## Overview

The hosted deployment runs the Kubernetes driver, and the driver declares
no egress mode. Every manifest with a boundary other than `open` with
nothing denied therefore carries the warning "This environment enforces no
egress rule", `EgressEnforced` reads `False` with the reason
`NotEnforcedByDriver`, and the boundary is recorded and not enforced. The
gateway itself runs ([[039-egress-gateway]]): the controller pushes each
sandbox's map to it before the Pod exists, and the gateway refuses what the
map does not admit. What is missing is the driver's half of the
intersection [[018-egress-and-secrets]] describes: a network rule that
leaves the gateway as the only way out, and a sandbox that knows where the
gateway is.

This slice writes a NetworkPolicy per sandbox, created before its Pod, that
admits egress to cluster DNS and to the gateway's Pods and nothing else and
admits ingress from nothing; declares `none`, `allowlist` and `open` once
the operator names the gateway's Pods; projects the gateway's two doors,
the sandbox's credential and the gateway's authority into the Pod; admits a
mesh member's traffic to its own peers under that confinement; and runs the
gateway, an upstream and the enforcement cases on the kind tier.

## Current state

| Piece | Where it is | What is missing |
|---|---|---|
| `Capabilities().Egress` on k8s | `runtime/k8s/k8s.go` | empty; the comment defers it to "the slice that builds" it |
| The projection of the gateway | `runtime/podman/egress.go`, `runtime/native/egress.go` call `egress.Projection` | `runtime/k8s` never reads `CreateSpec.Egress`: a sandbox Pod carries no `HTTPS_PROXY`, no credential and no authority |
| The sandbox's Secret | `runtime/k8s/token.go`, one per sandbox, mounted on `/run/cella` | holds the token alone; its comment already reserves the mount for the gateway's authority |
| The mesh rule | `runtime/k8s/mesh.go`, one NetworkPolicy per mesh | ingress from members only; under an egress default-deny a member cannot send to a peer |
| The helper Pod of a transfer on a stopped sandbox | `runtime/k8s/files.go` | runs the sandbox's own image and carries no label a per-sandbox rule could select |
| The gateway's Pods as a peer | nowhere | `CELLA_GATEWAY` and `CELLA_GATEWAY_REVERSE` are addresses a sandbox dials, not labels a policy selects |
| The kind stack | `deploy/examples/kind-stubs` | runs no `cellad egress`, no upstream, and declares no egress to the conformance runs |
| The enforcement proof | spec 018's `TestClusterEgressBoundary`, spec 013's `TestClusterNoLateralMovement` and `TestMeshReachability` | none written; the suite's only egress case reads the records route |

## Design

### The rule: one NetworkPolicy per sandbox

The driver writes `cella-sandbox-<object name>` in the sandbox namespace:

| Field | Value |
|---|---|
| `podSelector` | `cella.latere.ai/managed-by: cella` and `cella.latere.ai/sandbox: <id>` |
| `policyTypes` | `Ingress` always; `Egress` as well once the gateway's Pods are named |
| `ingress` | none, so nothing reaches the sandbox but what another policy admits: a mesh member's peers, through the mesh's own policy |
| `egress`, rule 1 | cluster DNS: one peer carrying both a namespace selector (`kubernetes.io/metadata.name: <DNS namespace>`) and the DNS Pods' labels, on each DNS port over UDP and TCP |
| `egress`, rule 2 | the gateway: one peer carrying both the gateway's namespace and its Pods' labels, on each of the gateway's ports over TCP |
| `egress`, rule 3 | for a mesh member, the members of its own mesh (`managed-by: cella`, `mesh: <id>`), on any port |

Each peer is one element of `to` holding both selectors, which the API reads
as a Pod that matches both. Two elements would be read as either, and every
Pod of the gateway's namespace would become reachable on the gateway's ports.

The rule does not depend on the boundary's mode. `none`, `allowlist` and
`open` differ in which hosts the gateway admits, and the gateway decides that
per connection from the map the controller pushed; the driver's part is that
nothing leaves except through the gateway. That is why one rule serves all
three modes, why a workload's narrowing never touches the cluster, and why a
prewarmed entry, which has no boundary until it is adopted, runs under the
same rule from its first instant.

The `sandbox` label is new. The sandbox's Pod carries it, and so does the
helper Pod a file transfer on a stopped sandbox runs in: the helper runs the
sandbox's own image, so its `tar` is the workload's own program, and it runs
inside the same rule rather than outside every rule. The helper keeps no
`id` label, so `List` still never reads it as the sandbox.

### When it is written, and when it goes

| Moment | What the driver does |
|---|---|
| `Create` | after the claim, before the mesh objects, the Secret and the Pod, so the Pod is confined from its first packet; a failure after the claim removes the rule with everything else |
| `Start` | before the new Pod, replacing whatever rule is there, so a sandbox created before this release or before the gateway was named runs under the rule the driver's options now describe |
| a transfer on a stopped sandbox | before the helper Pod, replaced the same way |
| `Delete` | after the Pod is gone and the claim with it, so no instant has a live Pod and no rule |

A replacement is a delete and a create. The Role already grants both verbs
on `networkpolicies` for the mesh, and at each of the three moments no Pod of
the sandbox is running, so the gap between the two calls confines nothing
that runs. No verb is added to the Role or to the preflight table.

### With no gateway named

Without the gateway's Pods the driver writes the ingress half alone and
declares no egress mode. Confining egress to DNS with no gateway to route
through would leave every sandbox, the inferred default `open` one included,
reaching nothing while `EgressEnforced` says `NotEnforcedByDriver`; the
declaration and the rule stay together. The ingress half costs such an
installation nothing it uses: the control plane reaches a sandbox through
the API server and the kubelet for commands, files, logs and ports, and no
policy applies to that path ([[065-k8s-dial]]).

### Why a rule per sandbox where the namespace already denies

The hosted deployment's namespace carries its own default-deny with only the
gateway and DNS allowed out. The per-sandbox rule is written there as well:

- a deployment of the open core that carries no namespace policy of its own
  still confines every sandbox, which is what makes the declared modes true
  of the driver rather than of one installation's namespace;
- the rule names the sandbox by label and goes with the sandbox, so a
  confinement never outlives the object it confines, and a mesh member's
  peer rule is scoped to its own mesh;
- policies add, so under a namespace default-deny the per-sandbox rule
  admits exactly what the namespace's own rules admit plus the member's
  peers, and weakens nothing.

### The peers are the operator's

The gateway's Pods and cluster DNS are named by the operator:

| Variable | Default | What it is |
|---|---|---|
| `CELLA_K8S_GATEWAY_SELECTOR` | unset | `key=value` labels of the gateway's Pods. Set, the driver confines egress and declares the three modes; unset, it confines ingress alone and declares none |
| `CELLA_K8S_GATEWAY_NAMESPACE` | the sandbox namespace | the namespace the gateway's Pods run in |
| `CELLA_K8S_GATEWAY_PORTS` | `3128,8080` | the ports the gateway's Pods listen on, which are `CELLA_EGRESS_PROXY_ADDR` and `CELLA_EGRESS_REVERSE_ADDR` of the gateway and not the ports of its Service |
| `CELLA_K8S_DNS_SELECTOR` | `k8s-app=kube-dns` | `key=value` labels of the cluster DNS Pods |
| `CELLA_K8S_DNS_NAMESPACE` | `kube-system` | the namespace they run in |

The ports are a variable of their own rather than read off `CELLA_GATEWAY`:
a NetworkPolicy matches the destination Pod's own port after the Service has
translated the address, and `CELLA_GATEWAY` names what a sandbox dials, which
is usually the Service. A worker that drives a cluster reads the same five
variables, since it holds no `CELLA_GATEWAY` of its own. The gateway's
namespace, the ports, and the DNS pair are refused at start without the
selector they belong to; the selector is refused on a control plane with no
`CELLA_GATEWAY`, because a sandbox confined to a gateway it is not pointed
at reaches nothing; and a label, a namespace or a port the API server would
refuse is a start-up problem naming the variable.

### The control plane is not admitted

Design 018's row for k8s admits `cellad` beside DNS and the gateway, so a
process inside reaches the API with its token. This slice does not: a
sandbox reaches the control plane at its public URL, which on a cluster
resolves to an ingress controller or a load balancer the driver cannot name
without fixing a coordinate of one installation, and the hosted deployment's
namespace policies admit no such path either. Under confinement, `cella`
inside a k8s sandbox does not reach its control plane. A child is still
spawned with the sandbox's token from outside it, which is how the spawn
cases run.

### The gateway inside the Pod

The Pod's workload container carries what `egress.Projection` renders for
the create spec's boundary: `HTTP_PROXY`, `HTTPS_PROXY` and their lowercase
forms pointing at the proxy door with the sandbox's credential, `NO_PROXY`
for loopback, `CELLA_GATEWAY_URL` and `CELLA_GATEWAY_CREDENTIAL` where a
reverse door is configured, and the five trust variables naming
`/run/cella/egress-ca.pem` where the gateway's authority was handed over.
The authority is a second key of the sandbox's own Secret, projected
read-only beside the token at `0444`; a token rotation replaces the token's
key alone. A command run through `exec` inherits the container's
environment, so every command sees the same variables the main process
does. The desktop container carries none of them: it draws the screen and
opens no connection.

A prewarmed Pod is rendered with both keys of the projection listed and
optional, so an adoption that writes the authority into the Secret reaches
the running entry through the kubelet's sync. The environment variables of a
running container cannot change, so an adopted entry's workload does not
carry the proxy variables until its next start, whose Pod is rendered from
the adopted record and carries them; until then it runs confined, and
reaches the gateway only where it is pointed at it by hand.

### Mesh peers under confinement

The mesh's own policy admits ingress from the mesh's members, and nothing
admitted a member's egress to them. The peer rule is rule 3 of the member's
own policy, not a second half of the mesh's:

- the mesh's policy is written once by the first member and left as it is,
  so a rule there would stay in the shape of whichever control plane made
  the mesh; the member's own rule is rewritten at every start;
- an `Egress` type on the mesh's policy would confine a member's egress to
  its peers on an installation that named no gateway, where the member
  reaches the network directly today.

A member reaches a peer at `<name>.<mesh object name>`, the hostname and the
subdomain of the headless Service, over DNS the rule admits.

### Declaring the modes

`Capabilities().Egress` is `[none, allowlist, open]` when the gateway's
selector is set and empty otherwise. The environment's status carries the
list, the manifest's stage 7 stops warning for a boundary the environment
now enforces, and `EgressEnforced` is `True` with `Enforced` once a gateway
also acknowledged the map, as [[039-egress-gateway]] builds it.

### The kind tier

`deploy/examples/kind-stubs` gains:

| Object | What it is |
|---|---|
| `cellad-egress` Deployment | `cellad egress` from the stack's own image, `CELLA_URL=http://cellad:8080` with `CELLA_INSECURE_CONTROL_PLANE=true`, the environment key from the `cellad-egress` Secret |
| `cellad-egress` Service | the two doors, 3128 and 8080 |
| `cellad-egress` NetworkPolicy | the doors admit the namespace's sandboxes and nothing else |
| `cella-upstream` Deployment and Service | a plain HTTP server on port 80, the host a sandbox is allowed to reach |
| the ConfigMap | `CELLA_GATEWAY`, `CELLA_GATEWAY_REVERSE` and `CELLA_K8S_GATEWAY_SELECTOR=app.kubernetes.io/name=cellad-egress` |

`up.sh` mints the environment key the way an operator does, with the stub
issuer's token at `POST /v1/environments/default/keys`, writes it into the
`cellad-egress` Secret, restarts the gateway, and waits until the default
environment reports a connected gateway. The key does not exist before the
control plane is up, which is why the gateway is a Deployment of its own and
not a container of the control plane's Pod.

The conformance suite gains `case018EgressEnforced`, run where the
environment declares `egress` and an upstream is given: a sandbox whose
allow list names the upstream reports `EgressEnforced` true; from inside it,
a `CONNECT` through its own proxy door to the upstream is answered `200`,
one to a host off the list is answered `403`, and a request sent straight to
the upstream gets no byte back; the refused connection is in the sandbox's
records as `denied`. Both kind conformance runs declare `egress` and pass
`-upstream cella-upstream.cella.svc:80`.

The kind tier gains three tests of what the API cannot see:

| Test | What it proves |
|---|---|
| `TestClusterEgressBoundary` | the same three answers through the stack, the upstream's own answer read back through the tunnel, and the records |
| `TestClusterNoLateralMovement` | a sandbox reaches no other sandbox's Pod address directly, and the gateway, whose own egress is open, reaches none either, which is the ingress half |
| `TestClusterMeshReachability` | a parent and the child spawned with its token reach each other by name on a port each serves, both ways |

## Not in this slice

The per-Pod gateway sidecar of `CELLA_EGRESS_SIDECAR=1`. The control plane
as an admitted peer. The proxy variables of an adopted pool entry. The
reverse door on the cluster, whose upstream is reached over TLS on port 443
and needs an upstream with a certificate the gateway trusts. Narrowing a
mesh's ingress to the ports a member declares `mesh`. The `<name>.mesh`
address podman gives a peer.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The driver declares `none`, `allowlist` and `open` when the gateway's Pods are named and no egress mode otherwise | `TestDeclarations`, `TestEgressModesFollowTheGateway` | passing |
| The rule selects the sandbox by its `sandbox` label, admits no ingress, and admits egress to DNS on UDP and TCP and to the gateway's ports, each peer one element holding the namespace and the Pod selector | `TestTheSandboxRuleAdmitsDNSAndTheGateway` | passing |
| Without a gateway the rule confines ingress alone | `TestTheSandboxRuleWithoutAGatewayConfinesIngress` | passing |
| The rule is in the cluster before the Pod, is removed when a create fails after the claim, and is removed with the sandbox after its Pod | `TestTheSandboxRuleIsWrittenBeforeThePod`, `TestAFailedCreateRemovesTheSandboxRule`, `TestDeleteRemovesTheSandboxRule` | passing |
| A start and a transfer on a stopped sandbox replace the rule before their Pod, and the helper Pod carries the label the rule selects | `TestStartRewritesTheSandboxRule`, `TestTheFileHelperRunsUnderTheSandboxRule` | passing |
| A mesh member's rule admits egress to its own mesh's members and no other mesh's | `TestAMeshMemberReachesItsPeersAlone` | passing |
| The workload container carries the proxy, trust and reverse door variables, the authority is a key of the sandbox's Secret projected at the reserved path, a rotation keeps it, an adoption writes it, and a sandbox with no gateway carries none of it | `TestThePodCarriesTheGatewayProjection`, `TestTheAuthoritySurvivesARotation`, `TestAnAdoptionWritesTheAuthority`, `TestNoGatewayProjectsNothing` | passing |
| A malformed peer is refused at `New`; the five variables are read with their defaults, and each malformed or orphaned one is a start-up problem naming it | `TestNewRefusesAMalformedPeer`, `TestK8sEgressVariables`, `TestTheConfigurationPageNamesEveryVariable` | passing |
| The Role and the preflight table are unchanged | `TestRoleMatchesTheDriversVerbs`, `TestEveryDialRequestIsInTheVerbTable` | passing |
| A sandbox allowed one upstream reaches it through its proxy door, is refused a host off its list, reaches nothing directly, and its refusal is recorded | conformance case `case018EgressEnforced`, run by `TestContract` | built: both kind jobs run it; the unit suite runs it against the fake |
| The same through the kind stack, with the upstream's answer read through the tunnel | `TestClusterEgressBoundary` | built: both kind jobs run it and require its pass; skipped here, where no cluster is reachable |
| A sandbox reaches no other sandbox's Pod, directly or through the gateway | `TestClusterNoLateralMovement` | built: both kind jobs run it and require its pass; skipped here, where no cluster is reachable |
| Two members of one mesh reach each other by name under confinement | `TestClusterMeshReachability` | built: both kind jobs run it and require its pass; skipped here, where no cluster is reachable |
| The kind stack runs the gateway with a minted key and an upstream, its ConfigMap names the gateway's Pods by the labels they carry, and both kind conformance runs declare `egress`, pass the upstream and require the three cluster tests | `TestKindRunsTheGateway` | passing |
| No file this slice adds names a Latere host, image, pool or namespace | `TestNoLatereCoordinates`, `TestNoLatereCoordinatesInReleasedArtifacts` | passing |
