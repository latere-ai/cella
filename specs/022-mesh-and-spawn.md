---
title: "Mesh and spawn: peers that reach each other, sandboxes that create sandboxes, a boundary that never moves"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/006-identity.md
  - specs/018-egress-and-secrets.md
affects: [manifest/, controller/, internal/auth/, internal/api/, runtime/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Mesh and spawn

## Overview

Two capabilities let sandboxes compose: a mesh, in which peers reach
each other's declared ports and nothing else reaches them, and spawn,
in which a process inside a sandbox creates further sandboxes through
the control plane with its own identity. Together they let an agent
run a planner that starts workers, a service that starts a browser, or
a coordinator that fans out a rollout, without a human in the loop for
each create. The rule that keeps this safe is fixed here and enforced
at resolve: the boundary declared when the root was created, its
egress, its secrets, its volumes, its resources, its lifetime, and its
spawn rights, bounds every descendant, and no workload widens it by
what it does.

## Current state

Not built. The hosted platform has a mesh with a spawn budget carried
in the workload token and a ledger, shipped and proven; this spec
restates that design against the manifest contract and adds the subset
check that the manifest now makes possible.

## Design

### Mesh

`spec.mesh.enabled: true` on a root sandbox creates a mesh, identified
by `status.mesh`, with the root as its first member. A spawned child
inherits its parent's mesh and may not set `mesh.enabled`; a root
created directly with `enabled: false` is alone. Members reach each
other's `network.ports[]` entries with `expose: mesh` by name:
`<name>.<sandbox-name>.mesh` resolves inside every member to the peer's
address, through a DNS the driver provides (a headless Service per mesh
on k8s, a per-mesh network with the engine's DNS on podman). Nothing
outside the mesh reaches a `mesh` port; a `public` port is a separate
declaration and needs `Ingress`. Mesh membership ends with the
sandbox; a mesh with no members is gone.

A mesh is not a bypass of egress: traffic to a peer is inside the
environment and never touches the gateway, and traffic to the network
from any member still goes through that member's own gateway map.

### Spawn

A sandbox with `mesh.spawn.budget > 0` may apply a `Sandbox` manifest to
`/v1/sandboxes` with its workload token. The request is handled like
any other, with three additions:

1. Identity sees `workload` set and the authorizer decides
   `sandbox.create` for the sandbox as subject; the built-in owner
   policy allows it when the budget allows.
2. Resolve runs with `Options.Parent` set to the parent's desired spec
   and current status, and the nine numbered rules of
   [[003-manifest-contract]]'s stage 6 hold the child to it. A
   violation is `boundary_exceeded` naming every path. A child that
   names no `ttl` gets the lesser of the default and the parent's
   remaining life; one that names a longer `ttl` or a later `deadline`
   is refused.
3. The controller debits the parent's budget and creates the child in
   one act against the store, so two concurrent spawns cannot both
   take the last unit; the child's `status.parent` is set, its `owner`
   is the root's owner, and its mesh is the parent's.

The workload token carries `spawn: {budget, depth, mesh}` as claims so
the gateway and a platform can read a sandbox's rights without a call;
the control plane still checks the store, since claims are a copy.

### Cascade

Deleting a sandbox deletes its descendants, breadth first, and emits
one event per sandbox with `reason: parent`. A child never
expires after its parent, by the boundary rule, so nothing survives its
root by accident. A
parent that is `Stopped` leaves its children running; a parent that is
`Lost` and recovers keeps its children, because their desired state
names it by id and the id is stable.

### Depth

`mesh.spawn.depth` is generations below this sandbox. A child gets at
most `depth - 1`, so a tree of depth 2 is a root, its children, and
their children, and a grandchild's `spawn.budget` resolves to zero
whatever it asks. The root's owner sees the whole tree through
`GET /v1/sandboxes?root=<id>`.

### Why the boundary is a subset and not a policy

A policy that lets a child ask for one more host is a policy the
workload negotiates with. A subset rule has nothing to negotiate: the
root's owner declared where the tree may go, and the tree may go there
and nowhere else, however many generations it grows. The same rule
makes a spawned rollout safe: the set's template is the parent of every
replica ([[020-scheduling-and-sets]]).

## Not in this spec

The driver's mesh mechanics ([[004-runtime-backend-contract]]); the
token's format ([[006-identity]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Two members of one mesh reach each other's `mesh` ports by name; a non-member cannot reach them; a `none`-exposed port is reachable by neither | e2e `TestMeshReachability` on k8s and podman | not built |
| A child spawned with the parent's exact boundary is created with `status.parent`, the parent's mesh, and the root's owner | `TestSpawnInheritance` | not built |
| Each of the nine boundary rules of [[003-manifest-contract]], violated one at a time through the spawn path, is `boundary_exceeded` naming the path | `TestSpawnBoundary`, table-driven over the same nine | not built |
| Two concurrent spawns against a budget of one yield one child and one `spawn_budget_exhausted` | `TestBudgetIsAtomic` | not built |
| A grandchild under depth 2 resolves to `spawn.budget: 0` and its own spawn is refused | `TestDepth` | not built |
| Deleting the root deletes every descendant with `reason: parent`; a child with no `ttl` expires no later than its parent | `TestCascade` | not built |
| The workload token's `spawn` claims equal the store's values at mint and are not trusted over the store at check | `TestClaimsAreACopy` | not built |
