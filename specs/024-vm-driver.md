---
title: "VM driver: a hardware-isolated sandbox per environment; the design held open"
status: vague
track: core
depends_on:
  - specs/004-runtime-backend-contract.md
  - specs/019-volumes.md
affects: [runtime/vm/, runtime/k8s/, internal/config/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# VM driver

## Overview

Some workloads need a floor a container cannot give: code from an
untrusted source, a tenant whose compliance regime names hardware
isolation, a kernel the workload may modify. [[004-runtime-backend-contract]]
reserves the `vm` isolation class for them and fixes what any `vm`
driver must do: one virtual machine per sandbox, an OCI image or a root
file system as its disk, one network interface routed to the egress
gateway, exec and attach through a guest agent, volumes as block
devices or a shared file system, and the same conformance suite as
every other driver. What it does not fix is how, and this spec holds
that decision open on purpose: it is not on the critical path of the
first releases, and the two ways to get there differ in cost by an
order of magnitude. This spec records the options, the criteria, and
the placeholder the tree carries until one is chosen.

## Current state

Not built, and not scheduled. `runtime/vm` exists as a package whose
`Preflight` reports not ready and names this spec, so a manifest that
asks for a `vm` environment is refused with a reason rather than a
missing case. The `k8s` driver's runtime class path, which is the
zero-code half of option A below, is part of
[[004-runtime-backend-contract]] and is not waiting on this spec.

## Design

### Option A: the class through Kubernetes

A cluster that has a `RuntimeClass` backed by Kata Containers, or by
Firecracker through a Kata or similar shim, gives every Pod the `k8s`
driver creates a hardware boundary with no driver code of Cella's. The
driver reports `Isolation() == vm` when `CELLA_K8S_RUNTIME_CLASS` names
such a class, and everything else, exec, attach, volumes, network
policy, is what the driver already does through the Pod.

| Property | Value |
|---|---|
| Cost to Cella | configuration and one conformance run against a cluster with the class |
| Where it runs | any cluster with the class installed; not a laptop, not a bare host |
| Start | seconds; a pool hides it |
| Snapshots and restore | not through the Pod API |
| What is lost | a `vm` environment without Kubernetes; control over the guest kernel and agent |

### Option B: a microVM driver of Cella's own

`runtime/vm` drives a hypervisor directly, Firecracker or Cloud
Hypervisor on Linux with KVM, and, if ever wanted on a developer's
machine, the platform's own virtualization framework. Cella owns the
guest: a minimal kernel, an init that starts a guest agent over vsock,
and the agent that serves exec, attach, files, and port dial to the
driver. An OCI image becomes a root file system at create through an
image-to-disk step the driver caches by digest. Volumes attach as block
devices, or over a shared file system for a read-only tools volume.
Networking is a tap per VM on a bridge whose only route is the egress
gateway. Snapshot and restore of a running VM give a start in tens of
milliseconds from a warmed template and a fork of a running
environment, which is what a rollout that branches from a common state
wants.

| Property | Value |
|---|---|
| Cost to Cella | a guest agent, an image conversion pipeline, a network setup, a snapshot store; the largest driver by far |
| Where it runs | any Linux host with KVM, including a worker's; a laptop with a framework of its own |
| Start | tens of milliseconds from a snapshot, seconds cold |
| Snapshots and restore | first class |
| What is lost | months; a second image format to keep current; a kernel to patch |

### Option C: A now, B when a need names it

The runtime class covers the compliance floor on the clusters where
Latere and most operators run, at no driver cost. Option B is
justified by one of three needs: a `vm` environment on hosts without
Kubernetes, snapshot-fork for rollouts, or a guest the operator
controls. Until one of those is asked for by a consumer with a date,
`runtime/vm` stays a stub.

### Criteria for the decision

| Criterion | Weighs toward |
|---|---|
| a consumer needs `vm` on a worker host without Kubernetes | B |
| a consumer needs sub-second starts of diverse environments, so a pool cannot help | B, through snapshots |
| the operators in view run Kubernetes with a Kata class available | A |
| the team can carry a guest kernel and agent as a product | B |
| the first releases must ship without it | A, or C |

### What is fixed whichever way

The `Driver` interface and the conformance suite; the `vm` class as
what `status.isolation` reports; the gateway as the only route; volumes
as `Volume` objects; the boundary rules. A workload cannot tell, and
must not need to tell, which option produced its VM.

## Not in this spec

The `k8s` driver's runtime class support, which is
[[004-runtime-backend-contract]]'s; any guest agent design, which is
option B's own spec if chosen.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `runtime/vm` exists, reports `Isolation() == vm`, and `Preflight` returns a not-ready error whose message names this spec | `TestVMIsAStub` | not built |
| A manifest naming an environment of the `vm` class whose driver is the stub is refused at resolve with `capability_unsupported` and the not-ready reason, not with a missing case | `TestVMStubRefusesAtResolve` | not built |
| The decision, once taken, is recorded in this spec's Outcome with the criterion that decided it, and the spec moves to `drafted` with the chosen option's design | review | open |
| Whichever option is built passes the conformance suite with `Egress`, `Volumes`, and `Persist` declared | `runtimetest` | not built |
