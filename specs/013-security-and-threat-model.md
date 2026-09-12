---
title: "Security and threat model: what Cella protects, against whom, and how"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/008-api.md
affects: [internal/auth/, internal/api/, internal/egressd/, internal/worker/, runtime/, egress/, deploy/, SECURITY.md]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Security and threat model

## Overview

A sandbox runs code nobody has read, written by a person or an agent,
with network access and credentials in its environment. This spec names
the assets, the adversaries, and the controls, so a reviewer checks the
design and `SECURITY.md` points at one document. Each control is a
criterion with a test; a control without a test is a claim.

## Current state

Not built. The controls descend from the hosted plane's, minus the
ones that belonged to its identity and billing surfaces.

## Design

### Assets

The host and the cluster the data plane runs on. Other sandboxes. The
values of every `Secret`. The workload token, the environment key, and
the gateway's CA. The operator's webhook secrets, token key, and
secrets KEK. Volumes and what they hold. The events, which name who did
what.

### Adversaries

| Adversary | Holds | Wants |
|---|---|---|
| a sandbox process | its own token, placeholders in its env, the gateway per its manifest | the host, another sandbox, a host its manifest did not name, a secret's value, a longer life, a child with a wider boundary |
| a worker host | an environment key, the sandboxes it runs | another environment's sandboxes, the control plane's store, a secret bound for another environment |
| an authenticated caller | a valid bearer | another subject's sandbox, a ceiling bypass, output it may not read |
| a network position | the wire | a token, a webhook secret, a forged event |
| a compromised webhook | the authorizer or admission endpoint | to allow everything or to run an image it chose |

### Controls

| Threat | Control | Spec |
|---|---|---|
| escape from a sandbox to the host | the k8s driver runs Pods as non-root with every capability dropped, a read-only root file system except the workspace and `/tmp`, `seccomp: RuntimeDefault`, no privilege escalation, no host namespaces, no service account token; podman runs rootless; the native driver confines only by directory and says so | 004 |
| lateral movement between sandboxes | a NetworkPolicy per Pod denies ingress from other Pods and egress to the Pod network; the podman network is per sandbox | 004 |
| egress to a host not in the manifest | the environment's rule admits only the gateway, DNS, and the control plane; the gateway refuses a CONNECT to a host off the allow list before any bytes flow; where a driver cannot enforce it, `EgressEnforced` is false and the resolved manifest warns | 004, 018 |
| a secret's value in a sandbox | the sandbox holds a per-sandbox placeholder; the value is decrypted in the control plane only to be pushed to the gateway, substituted only toward the secret's own hosts, and never returned by any API | 018 |
| a placeholder guessed or replayed toward another host | placeholders are random per sandbox; a placeholder sent off scope leaves verbatim as an inert string | 018 |
| a workload widening its own boundary | the boundary fields narrow only after create; a workload token may not add a host or a secret; a child is a subset of its parent at resolve | 003, 018, 022 |
| a runaway spawn tree | budget and depth debited atomically; a child's `ttl` clamped to its parent's; delete cascades | 022 |
| a volume as an escape | no host path on k8s; a volume attaches only where the authorizer allows; read-only stays read-only in a child | 019, 022 |
| a compromised worker | an environment key authorizes one environment's queue; operations carry only that environment's sandboxes; secret values cross to that environment's gateway only for sandboxes placed there; the control plane accepts no inbound from a worker | 021 |
| a sandbox acting as its owner | the workload token's subject is the sandbox, the owner policy grants it read and exec on itself, and the authorizer sees `workload` set | 006 |
| a token outliving its sandbox | the token's `exp` is the sandbox's expiry, and verification checks the index for `Deleting` | 006 |
| a caller reaching another's sandbox | every item route reads, authorizes, then acts; a deny on the caller's own action is 403, and a refused reference at resolve is the same `not_found` as a missing one, so a manifest cannot probe for another's secrets, volumes, or environments | 006, 008 |
| an allow without a decision | the authorizer and admission clients fail closed on every non-200 and every timeout | 006, 007 |
| a forged or replayed event | HMAC over timestamp and body; the sink refuses a timestamp older than 5 minutes | 009 |
| a webhook that rewrites the manifest past a ceiling | the built-in ceilings apply after the webhook | 007 |
| secrets in events, logs, or status | events carry no env value, no token, no output; logs redact `env` and `Authorization`; `status` never echoes `env` | 003, 009 |
| a manifest body that exhausts memory | `CELLA_MAX_BODY_BYTES`, the YAML decoder's alias and depth limits, and a 64 KiB annotation cap | 003, 008 |
| a flood from one subject | the per-subject rate limit and the authorizer's `limits` | 008 |
| exec output to a caller without the right | `sandbox.exec` guards exec, attach, files, and logs are `sandbox.read` and never include exec output | 008 |
| the port proxy turned toward another sandbox or the Pod network | the proxy resolves a port name only within the sandbox's own declared ports and dials through the driver; no caller-supplied host or port reaches the dialer | 023 |
| a key in the environment | `CELLA_TOKEN_KEY` and the webhook secrets are read once at start and never logged; the deploy manifests mount them from Secrets | 002, 014 |
| a dependency with a known vulnerability | the `vuln` gate on every push | 002 |
| an image that is not what was released | signed images, SBOMs, and provenance attestations; `gh attestation verify` documented | 014 |

### What is out of scope

Kernel escapes the environment's isolation class does not prevent; an
operator picks a `vm` class for a floor the container class cannot
give. The security of the
issuer and of the webhooks themselves. Secrets management beyond
`env`; a plane brokers.

## Not in this spec

The deploy manifests that carry the Pod security fields
([[014-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A Pod the k8s driver creates has every field of the escape control set | `TestPodSecurityFields` over the rendered Pod, and the kind tier's `TestClusterPodIsConfined` running `id`, `cat /proc/1/status`, and a mount attempt | not built |
| A sandbox cannot reach another sandbox's IP or the Pod network | `TestClusterNoLateralMovement` | not built |
| A manifest naming another subject's secret, volume, or environment is `not_found`, identical to a missing one | `TestRefusedReferencesLookMissing` | not built |
| A canary env value and a canary token appear in no event, log line, or status body across the e2e tier | `TestNoSecretLeaks` grepping every capture | not built |
| A YAML body with a billion-laughs alias chain is refused in under 100 ms | `TestYAMLBombIsRefused` | not built |
| A canary secret value appears in no sandbox environment, file system, event, log, or API response across the e2e tier, and reaches the upstream only in the declared header | `TestSecretValuesNeverEnterASandbox` | not built |
| A workload token that tries to add a host, mount a secret, or spawn past its budget is refused with the code named | `TestWorkloadCannotWiden` | not built |
| A worker host with inbound refused runs the whole tier | `TestNoInboundToTheDataPlane` | not built |
| Every control in the table names a test that exists in the tree | `TestThreatModelControlsHaveTests` reading this file | not built |
