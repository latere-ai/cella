---
title: "Mesh and spawn: peers that reach each other, sandboxes that create sandboxes, a boundary that never moves"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/010-state.md
  - specs/018-egress-and-secrets.md
affects: [manifest/, controller/, internal/auth/, internal/api/, internal/store/, runtime/]
effort: medium
created: 2026-09-12
updated: 2026-09-20
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
egress, its secrets, its volumes, its resources, its lifetime, its
environment, its mesh, and its spawn rights, bounds every descendant,
and no workload widens it by what it does.

## Current state

Built by [[040-mesh-and-spawn]]: the spawn tree and its status fields,
the boundary check of stage 6 over every rule with a field, the ledger
and its debit inside the child's own write, the mesh minted at a root's
create and inherited down the tree, the cascade, the `sandbox.spawned`
record, the `?root=` selector, and the mesh's own objects on podman and
on k8s. What waits: rule 4 on [[019-volumes]], a set's template as a
parent on [[020-scheduling-and-sets]], a workload's `PUT` on the route
[[008-api]] has yet to serve, and the peer-reachability case on the
conformance tier of [[012-test-stubs-and-tiers]].

## Design

### The spawn tree

Every sandbox has `status.root` and `status.parent`
([[003-manifest-contract]]): a sandbox a subject applied is its own
root and has no parent; a sandbox a workload applied has that
workload's sandbox as `parent` and the parent's `root` as `root`. Both
are set by the controller at create and never change. `owner` is the
root's owner throughout the tree, so every member shares one owner
and, since names are unique per owner, one namespace of names; a
child differs from a sandbox the owner applied directly only by
`parent`. `GET /v1/sandboxes?root=<id>` returns a tree ([[008-api]]).
Spawn is only a workload's act: no manifest field names a parent,
`status` is ignored on apply, so an owner cannot spawn on a sandbox's
behalf.

### Mesh

A mesh is an `msh_` id and nothing more: it has no row and no kind
([[010-state]]); it is the set of live sandboxes whose `status.mesh`
carries the id. The controller mints the id at the create of a root
with `mesh.enabled: true`, or at the apply of a `SandboxSet` whose
template enables it ([[020-scheduling-and-sets]]), and writes it into
`status.mesh`; a child inherits its parent's `status.mesh` at create
and may not set `mesh.enabled`, which is rule 9. Membership is
`status.mesh`, so a child declares `expose: mesh` ports on the strength
of its inherited membership, and `expose: mesh` on a sandbox with no
`status.mesh` is `invalid_field` ([[003-manifest-contract]]). A root
with `enabled: false` has children that are not networked.

Members reach each other at `<sandbox-name>.mesh` on a port the peer
declared with `expose: mesh`; nothing else reaches such a port, and a
`public` port is a separate declaration ([[023-computer-use-operations]]).
The driver owns the `.mesh` zone inside its members
([[004-runtime-contract]]): on k8s a headless Service per mesh selects
members by the stamped `mesh` label, each Pod's `hostname` is the
sandbox name and its `subdomain` the Service, and the installation's
DNS rewrites `*.mesh` to that Service's zone; on podman one network per
mesh with `<name>.mesh` as each container's alias. A `Stopped` member
keeps `status.mesh` and its name resolves to no address until it
starts. A mesh never spans environments, since rule 8 keeps every
child on its parent's environment. The mesh survives its root being
`Stopped`; it ends when its last member is deleted, at which point the
driver removes the per-mesh Service or network. A mesh is not a bypass
of egress: traffic to a peer stays inside the environment and never
touches the gateway, and traffic to the network from any member still
goes through that member's own gateway map.

### Spawn

A workload's apply of a name that does not yet exist among the owner's
objects is a spawn; its apply of an existing name is an update of a
sandbox it may update, with `Parent` nil and no boundary check
([[008-api]]). A spawn is gated on the parent's `status.spawn`: it
needs `budget - used > 0` and `depth > 0`, else `spawn_budget_exhausted`
at the debit, or `boundary_exceeded` at `spec.mesh.spawn.depth` when
the parent has no depth left. The request is handled like any other,
with three additions:

1. Identity sees `workload` set and the authorizer decides
   `sandbox.create` for the sandbox as subject; the owner policy allows
   it for any sandbox subject. The budget is not an authorization
   input; neither policy reads the ledger ([[006-identity]]).
2. Resolve runs with `Options.Parent` set to the immediate parent's
   desired spec and current status, and the nine numbered rules of
   [[003-manifest-contract]]'s stage 6 hold the child to it. A child's
   `spawn.budget` and `.depth` default to zero when absent; an ask
   above `budget - used - 1` or `depth - 1` is `boundary_exceeded`. A
   child that names no `ttl` gets the lesser of the default and the
   parent's remaining life; one that names a longer `ttl` is
   `boundary_exceeded`. A child may change its image, command, args,
   env, ports, labels, and resources within the parent's, and mount
   a subset of the parent's secrets and volumes; it may not change its
   environment or its mesh.
3. The desired write of [[005-lifecycle-controller]]'s create step 1
   debits the parent through `Ledger.Debit` in the same transaction;
   exhaustion there is `spawn_budget_exhausted`. A failed create
   credits the debit back in the undo; a deleted child never does,
   since `budget` counts children created in total. The controller
   projects `status.spawn.used` from `Ledger.Balance` after every debit
   and credit.

The workload token's `spawn: {budget, depth, mesh}` claims are the
grant at mint, never the balance: they carry no `used`, are not
re-minted on a spawn, and the control plane reads the ledger
([[006-identity]]).

### A parent narrowed after its children exist

The `narrow` fields of a root may be narrowed by a non-workload actor
after create ([[003-manifest-contract]]). An update that would leave
any live descendant outside its parent's boundary is refused with
`boundary_exceeded` naming the descendant, so the tree stays a subset
at every instant without the control plane rewriting a child's
manifest behind its author.

### Cascade

Deleting a sandbox deletes its descendants deepest generation first,
then the sandbox ([[005-lifecycle-controller]]), each descendant with
reason `Parent` and the root with the request's own reason, `Request`
or `Expired`. A child never expires after its parent, by rule 6. A
parent that is `Stopped` leaves its children running; a parent that is
`Lost` and recovered keeps its children, because their desired state
names it by id and the id is stable.

### Events

A successful spawn emits `sandbox.spawned` on the parent with the
child's id and the budget left ([[009-events]]); the child's own
`sandbox.created` is a separate object's event and is unordered
relative to it. The cascade emits `sandbox.deleted` per sandbox with
the reasons above.

### Why the boundary is a subset and not a policy

A policy that lets a child ask for one more host is a policy the
workload negotiates with. A subset rule has nothing to negotiate: the
root's owner declared where the tree may go, and the tree may go there
and nowhere else, however many generations it grows. The same rule
makes a spawned rollout safe: the set's template is the parent of
every replica ([[020-scheduling-and-sets]]).

## Not in this spec

The driver's mesh mechanics ([[004-runtime-contract]]); the token's
format ([[006-identity]]); the ledger's methods ([[010-state]]); the
nine rules' text ([[003-manifest-contract]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Two members of one mesh reach each other's `mesh` ports at `<name>.mesh`; a non-member cannot; a `none`-exposed port is reachable by neither; a `Stopped` member's name resolves to no address | e2e `TestMeshReachability` on k8s and podman | built in part by [[040-mesh-and-spawn]]: each driver's own objects are proven, and the reachability case waits on the conformance tier of [[012-test-stubs-and-tiers]] |
| The mesh id is minted at the root's create and at a set's apply, inherited by children and replicas, and the driver's per-mesh object is gone after the last member's delete | `TestMeshLifetime` | built by [[040-mesh-and-spawn]] for a root's create; a set's apply waits on [[020-scheduling-and-sets]] |
| A child spawned within the parent's boundary is created with `parent`, `root`, the inherited mesh, and the root's owner; a grandchild's `Parent` is its immediate parent | `TestSpawnInheritance` | built by [[040-mesh-and-spawn]] |
| Each of the nine rules, violated one at a time through the spawn path, is `boundary_exceeded` naming the path | `TestSpawnBoundary`, table-driven | built by [[040-mesh-and-spawn]] for the eight rules with a field; rule 4 waits on [[019-volumes]] |
| A child that omits `spawn` gets zero; one asking one more than the remainder is refused; a spawn from a parent at `depth: 0` is `boundary_exceeded` at `spec.mesh.spawn.depth` | `TestSpawnFieldsAndDepth` | built by [[040-mesh-and-spawn]] |
| A workload's `PUT` of its own name is an update with no boundary check | `TestWorkloadSelfUpdateIsNotASpawn` | not built: this API serves no `PUT` of a sandbox, so a workload's apply of an existing name has no route yet |
| A non-workload actor cannot create a sandbox with a parent | `TestOnlyWorkloadsSpawn` | built by [[040-mesh-and-spawn]] |
| The debit is in the desired-write transaction; two concurrent spawns against one remaining unit yield one child and one `spawn_budget_exhausted`; a failed create credits back; a deleted child does not; `status.spawn.used` tracks the ledger | [[005-lifecycle-controller]]'s `TestSpawnDebitIsAtomic`, `TestSpawnUsedTracksTheLedger` | built by [[040-mesh-and-spawn]] |
| Narrowing a root below a live descendant is `boundary_exceeded` naming the descendant | `TestParentCannotBeNarrowedBelowAChild` | built by [[040-mesh-and-spawn]] |
| Deleting the root deletes every descendant deepest first with `Parent`, and the root with the request's reason | [[005-lifecycle-controller]]'s `TestCascade` | built by [[040-mesh-and-spawn]] |
| `GET /v1/sandboxes?root=` returns the tree | `TestRootQuery` | built by [[040-mesh-and-spawn]] |
| The workload token's `spawn` claims equal the grant at mint and are not trusted over the ledger | `TestClaimsAreTheGrant` | built by [[040-mesh-and-spawn]] |
| Admission sees `Parent` on a spawn | [[007-admission]]'s `TestAdmissionRequestShape` | built by [[040-mesh-and-spawn]]: a workload's apply resolves with `Options.Parent` and the step is handed it |
