---
title: "Mesh and spawn: the spawn tree, the propagated budget, the boundary as a subset check"
status: in-progress
track: core
depends_on:
  - specs/022-mesh-and-spawn.md
  - specs/003-manifest-contract.md
  - specs/006-identity.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/045-workload-tokens.md
  - specs/.archive/047-admission-client.md
affects: [manifest/, manifest/v1/, controller/, internal/auth/, internal/api/, internal/store/, internal/events/, runtime/, runtime/podman/, runtime/k8s/, cmd/cellad/, specs/]
effort: large
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# Mesh and spawn

## Overview

Slice 040 of [[031-hosted-sandbox-consolidation]]. It ports the mesh
identity, the membership and the runtime spawning of
`sandbox/internal/sandbox/mesh` and `sandbox/internal/sandbox/spawn`
into the shape [[022-mesh-and-spawn]] states, and it adds the rule the
manifest contract made possible and the hosted platform never had: the
boundary a root declared bounds every descendant, checked at resolve as
a subset and never negotiated as a policy.

A process inside a sandbox creates further sandboxes with its own
workload token. The control plane treats that apply as a spawn: the
child's resolved manifest is held to the parent's on every boundary
field, the parent's spawn budget is debited in the same store
transaction that writes the child, the child inherits the parent's mesh
and root, and deleting an ancestor deletes the tree below it.

What the hosted product carried and this slice drops: the two-axis
ledger (descendants and compute credits) keyed by root, the mTLS peer
identity and its certificate issue path, the reserve-commit-rollback
handle, and the descendant garbage-collection sweeper that recounted
live pods. [[010-state]] fixes one row per sandbox with a conditional
decrement; [[004-runtime-contract]] fixes the mesh as a driver-owned
network with a `.mesh` zone rather than a mutual-TLS fabric; the
cascade of [[022-mesh-and-spawn]] replaces the sweeper, because desired
state names the tree and no recount is needed.

## Current state

`manifest` has no `mesh` section and no `status.parent`, `.root`,
`.mesh` or `.spawn`. `Options` has no `Parent` and `Resolve` runs six
of its seven stages, stage 6 absent. `internal/auth` mints the `spawn`
claim shape (`auth.Spawn`) and `WorkloadTokens.Mint` leaves it nil with
a comment naming this slice. `internal/store` declares `Ledger` in
[[010-state]]'s prose and neither the accessor nor the table exists;
[[043-postgres-store]] left it as a seam. `controller.Create`
takes no parent, mints no mesh id and debits nothing; its delete acts on
one sandbox. `internal/api` refuses `?root=` with
`capability_unsupported`. `runtime.CreateSpec` has no `Mesh` and
`runtime.State` no `MeshID` or `Parent`; no driver declares the `Mesh`
capability.

## Design

### The spawn tree

```mermaid
graph TD
  R["root sbx_A<br/>budget 4, depth 2<br/>egress allowlist: a.example, b.example"]
  C1["child sbx_B<br/>parent A, root A<br/>budget 2, depth 1<br/>hosts: a.example"]
  C2["child sbx_C<br/>parent A, root A<br/>budget 0, depth 0"]
  G1["grandchild sbx_D<br/>parent B, root A<br/>budget 0, depth 0<br/>hosts: a.example"]
  X["refused: hosts a.example, c.example<br/>boundary_exceeded at<br/>spec.network.egress.allowedHosts"]
  R -->|debit A| C1
  R -->|debit A| C2
  C1 -->|debit B| G1
  C1 -.->|resolve| X
  R -.-|"status.mesh msh_01J9 inherited by B, C, D"| C2
```

Every sandbox carries `status.root` and `status.parent`. A sandbox a
subject applied is its own root and has no parent. A sandbox a workload
applied has that workload's sandbox as `parent` and the parent's `root`
as `root`. Both are written by the controller at create and never
change. `owner` is the root's owner throughout the tree, so a child
differs from a sandbox the owner applied directly only by `parent`.

The budget flows down by subtraction. Write `b(x)` for the resolved
`spec.mesh.spawn.budget` of `x`, `u(x)` for the ledger's count of
children `x` created, and `d(x)` for its `spec.mesh.spawn.depth`. A
spawn from parent `p` to child `c` is admitted only when

```
b(p) - u(p) > 0   and   d(p) > 0
b(c) <= b(p) - u(p) - 1
d(c) <= d(p) - 1
```

The first line is the gate, the second and third the subset rule on the
two spawn fields. `u(p)` counts children created in total, so a deleted
child never returns a unit and the tree's total size is bounded by
`b(root)` at every instant.

### The boundary as a subset

Stage 6 of [[003-manifest-contract]] runs when `Options.Parent` is set,
after semantic validation and before the capability check. Each rule is
a containment, and every violation is `boundary_exceeded` naming its
path. One error names every offending path.

| Rule | Field | Containment |
|---|---|---|
| 1 | `spec.network.egress.mode` | `EgressModeRank(child) >= EgressModeRank(parent)`, the order `open > allowlist > none` |
| 2 | `spec.network.egress.allowedHosts` | every child pattern is covered by some parent pattern under the host rule |
| 2 | `spec.network.egress.deniedHosts` | every parent pattern is present in the child's, since narrowing a deny list adds entries |
| 3 | `spec.secrets[]` | every child mount names a secret the parent mounts |
| 4 | `spec.volumes[]`, `spec.workspace.volume` | no field yet ([[019-volumes]]); the rule lands with the kind |
| 5 | `spec.resources.{cpu,memory,disk}` | the child's parsed quantity does not exceed the parent's |
| 6 | `spec.lifecycle.ttl` | `createdAt + ttl <= Parent.status.expiresAt`; a parent that never expires bounds nothing |
| 7 | `spec.mesh.spawn.{budget,depth}` | the two inequalities above; the gate `d(p) > 0` is refused at `spec.mesh.spawn.depth` |
| 8 | `spec.environment` | equal to the parent's |
| 9 | `spec.mesh.enabled` | unset, since the mesh is inherited |

Rule 2 reads `allowedHosts` as the declared list on both sides. A
secret's hosts join the allow list at stage 4 for parent and child
alike, so a child that passes rule 3 carries no host rule 2 did not
already admit.

Rule 6 needs the wall clock, which is why `Options.Now` exists. A child
that names no `ttl` is defaulted at stage 2 to the lesser of
`Defaults.TTL` and the time from `Now()` to `Parent.status.expiresAt`,
so the common case conforms without the caller computing anything.

### Mesh

A mesh is an `msh_` id and nothing more: no row, no kind. It is the set
of live sandboxes whose `status.mesh` carries the id. The controller
mints the id at the create of a root whose `spec.mesh.enabled` is true;
a child inherits its parent's `status.mesh` whatever the parent's
`enabled` says, and may not set the field. Membership is therefore
`status.mesh`, and `MeshMember` in `manifest` is the one function that
answers it: `spec.mesh.enabled` on a root, a non-empty `status.mesh` on
a child. [[023-computer-use-operations]]'s `expose: mesh` port reads the
same function, so the port rule and the mesh rule cannot disagree.

What each driver enforces, from [[004-runtime-contract]]'s table:

| Driver | Mesh | Mechanism |
|---|---|---|
| podman | yes | one network per mesh, named from the mesh id, with `<sandbox-name>` and `<sandbox-name>.mesh` as the container's aliases on it; the container keeps its default network, so the gateway route is unchanged |
| k8s | yes | one `NetworkPolicy` per mesh admitting ingress from pods carrying the same `mesh` label and no other, and one headless `Service` per mesh; each Pod's `hostname` is the sandbox name and its `subdomain` the Service, so `<name>.<mesh-service>` resolves |
| native | no | the capability is false; `mesh.enabled` is `capability_unsupported` and `status.mesh` stays empty |

The mesh id is not a DNS-1123 label: `msh_01J9ZK...` carries an
underscore and uppercase Crockford digits. One function per driver
package derives the object name, lowercasing and replacing the prefix
separator, and a test holds the result to the label syntax.

A mesh is not a bypass of egress. Traffic to a peer stays inside the
environment and never reaches the gateway; traffic out of any member
still goes through that member's own map. The east-west rule is the
driver's, so `egress` and `internal/egressd` are unchanged by this
slice.

### One spawn

```mermaid
sequenceDiagram
  participant W as workload in sbx_A
  participant API as internal/api
  participant M as manifest.Resolve
  participant C as controller
  participant S as store
  participant D as driver
  W->>API: POST /v1/sandboxes, bearer the projected token
  API->>API: caller.Sandbox() is sbx_A; load A
  API->>M: Resolve with Actor.Workload, Options.Parent = A
  M->>M: stages 1 to 5, then stage 6 against A, then 7
  M-->>API: resolved child, or boundary_exceeded
  API->>API: owner is A's owner; resource carries parent and root
  API->>C: Spawn(child, A, max)
  C->>C: id, name, quota; parent, root and mesh from A
  C->>S: one Tx: debit A against b(A), put the child, append sandbox.created
  S-->>C: ErrBudgetExhausted, or committed
  C->>D: Create with Mesh{ID}
  D-->>C: running
  C->>S: sandbox.spawned on A with the child and the budget left
  C-->>API: the child with status.parent, .root, .mesh, .spawn
```

The debit is inside the transaction that writes the child, which is
what makes two concurrent spawns against one remaining unit yield one
child. Every undo path of the create order credits the parent back: a
gateway that refused the map, a mint that failed, a driver that refused.
A child deleted later credits nothing, because `used` counts children
created in total.

### The ledger

[[010-state]] fixes one row per sandbox with `budget` and `used`, and
`Debit` as `UPDATE ... WHERE used < budget`. This slice puts the budget
where the manifest already keeps it, in desired state, and the ledger
keeps `used` alone:

```go
type Ledger interface {
	// Debit records one child against the parent, atomically, refusing
	// at budget with ErrBudgetExhausted.
	Debit(ctx context.Context, parentID string, budget int) error
	// Credit is the undo of a create that did not complete.
	Credit(ctx context.Context, parentID string) error
	// Used is how many children the parent has created in total.
	Used(ctx context.Context, parentID string) (int, error)
	// Forget drops one sandbox's row.
	Forget(ctx context.Context, parentID string) error
}
```

The budget travels as an argument rather than a column because a root
narrowed after its children exist must take effect at the next debit
with no second write, and because two sources of truth for one number
is a drift the contract does not need. `status.spawn.used` is projected
from `Used` after every debit and credit, so a reader of the status and
the row that gates the next spawn are the same count.

Both stores carry it. The durable store of [[010-state]] runs the
decrement inside `Tx`, in one statement with a `WHERE` on the count. The
single-process snapshot store holds a ledger map in the same document as
the objects, so one `rename` commits the child and the debit together.

### The workload token's claim

`spawn: {budget, depth, mesh}` is the grant at mint, from desired state,
and carries no `used`. The control plane never reads it: every gate is
the ledger and the store's copy of the parent. `Caller.Spawn` exposes
the claim to a caller that wants to know what it was granted without a
round trip, and a token carrying a forged higher budget spawns exactly
as far as the store says.

### Cascade

Deleting a sandbox deletes its descendants deepest generation first,
then the sandbox, each descendant with reason `Parent` and the sandbox
with the request's own reason. The tree is read from desired state by
`status.parent`, so no driver is asked what belongs to what. A
descendant the driver has already lost is forgotten rather than
retried, because the delete of its ancestor is the last word on it.

### A parent narrowed after its children exist

An update of a sandbox with live descendants runs stage 6 again with
the updated sandbox as the parent of each immediate child, and a child
left outside is `boundary_exceeded` naming that child's id in the
message and the offending path in `Path`. The tree is therefore a subset
at every instant, and no child's manifest is rewritten behind its
author.

## Not in this spec

The `Volume` kind and rule 4 ([[019-volumes]]); `network.ports[]` and
`expose: mesh` at the port level ([[023-computer-use-operations]], slice
041), which reads `MeshMember` from here; the `SandboxSet` template as a
parent ([[020-scheduling-and-sets]]); the installation's `*.mesh` DNS
rewrite, which is a cluster concern and not a driver call.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each boundary rule with a field, violated one at a time, is `boundary_exceeded` naming the path; a conforming child passes | `TestBoundaryCheck` in `manifest`, table-driven | |
| A child that names no `ttl` is cut to the parent's remaining life; one that names a longer `ttl` is refused | `TestChildTTLIsBoundedByTheParent` | |
| A child's absent `spawn` is zero; one asking above the remainder is refused; a parent at `depth: 0` refuses at `spec.mesh.spawn.depth` | `TestSpawnFieldsAndDepth` | |
| `MeshMember` answers membership for a root and for a child, and stage 7 refuses `mesh.enabled` where the environment declares no `Mesh` | `TestMeshMembership`, `TestMeshCapability` | |
| `Debit` at one remaining unit under contention yields one success and one `ErrBudgetExhausted`; `Credit` restores it; both adapters agree | `storetest.Run`'s ledger case, over memory and Postgres | |
| The debit and the child's row commit together: a failure injected between them leaves neither | `TestSpawnDebitIsAtomic` | |
| A child is created with `parent`, `root`, the inherited mesh and the root's owner; a grandchild's parent is its immediate parent | `TestSpawnInheritance` | |
| Two concurrent spawns against one remaining unit yield one child and one `spawn_budget_exhausted` | `TestConcurrentSpawnsRaceForTheLastUnit` | |
| A create that fails after the debit credits the parent back; a deleted child does not | `TestFailedSpawnCreditsBack`, `TestDeletedChildDoesNotCredit` | |
| Deleting a root deletes every descendant deepest first with reason `Parent`, and the root with the request's reason | `TestCascade` | |
| Narrowing a root below a live descendant is `boundary_exceeded` naming the descendant | `TestParentCannotBeNarrowedBelowAChild` | |
| The token's `spawn` claim equals the grant at mint; a forged higher budget does not raise the ledger | `TestClaimsAreTheGrant` | |
| `sandbox.spawned` is emitted on the parent with the child and the budget left | `TestSpawnedEvent` | |
| `GET /v1/sandboxes?root=` returns the tree | `TestRootQuery` | |
| podman creates one network per mesh and attaches members with a `.mesh` alias; k8s renders the policy and the headless Service and sets `hostname` and `subdomain`; native declares no `Mesh` | `TestMeshNetwork` (podman, fake and real engine), `TestRenderMesh` (k8s), `TestNativeDeclaresNoMesh` | |
| A node on native serves a spawn tree end to end: two children, a third refused with `spawn_budget_exhausted`, a grandchild refused at `spec.mesh.spawn.depth`, a wider child refused with `boundary_exceeded`, and the root's delete taking both children | `TestSpawnTreeEndToEnd` in `cmd/cellad` | |
