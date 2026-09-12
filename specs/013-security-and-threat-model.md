---
title: "Security and threat model: what Cella protects, against whom, and how"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/004-runtime-backend-contract.md
  - specs/006-identity.md
  - specs/008-api.md
affects: [internal/auth/, internal/api/, runtime/, deploy/, SECURITY.md]
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

The host and the cluster the backend runs on. Other sandboxes. The
credentials in a sandbox's `env`. The workload token. The operator's
webhook secrets and token key. The events, which name who did what.

### Adversaries

| Adversary | Holds | Wants |
|---|---|---|
| a sandbox process | its own token, its env, network per its manifest | the host, another sandbox, a host its manifest did not name, a longer life |
| an authenticated caller | a valid bearer | another subject's sandbox, a ceiling bypass, output it may not read |
| a network position | the wire | a token, a webhook secret, a forged event |
| a compromised webhook | the authorizer or admission endpoint | to allow everything or to run an image it chose |

### Controls

| Threat | Control | Spec |
|---|---|---|
| escape from a sandbox to the host | the k8s backend runs Pods as non-root with every capability dropped, a read-only root file system except the workspace and `/tmp`, `seccomp: RuntimeDefault`, no privilege escalation, no host namespaces, no service account token; podman runs rootless; the native backend confines only by directory and says so | 004 |
| lateral movement between sandboxes | a NetworkPolicy per Pod denies ingress from other Pods and egress to the Pod network; the podman network is per sandbox | 004 |
| egress to a host not in the manifest | the allow list is enforced where the backend can and reported as a warning where it cannot; `none` blocks all | 003, 004 |
| a sandbox acting as its owner | the workload token's subject is the sandbox, the owner policy grants it read and exec on itself, and the authorizer sees `workload` set | 006 |
| a token outliving its sandbox | the token's `exp` is the sandbox's expiry, and verification checks the index for `Deleting` | 006 |
| a caller reaching another's sandbox | every route authorizes before it looks up; `not_found` and `forbidden` are the same 404 to a caller that is not the owner, so existence does not leak | 006, 008 |
| an allow without a decision | the authorizer and admission clients fail closed on every non-200 and every timeout | 006, 007 |
| a forged or replayed event | HMAC over timestamp and body; the sink refuses a timestamp older than 5 minutes | 009 |
| a webhook that rewrites the manifest past a ceiling | the built-in ceilings apply after the webhook | 007 |
| secrets in events, logs, or status | events carry no env value, no token, no output; logs redact `env` and `Authorization`; `status` never echoes `env` | 003, 009 |
| a manifest body that exhausts memory | `CELLA_MAX_BODY_BYTES`, the YAML decoder's alias and depth limits, and a 64 KiB annotation cap | 003, 008 |
| a flood from one subject | the per-subject rate limit and the authorizer's `limits` | 008 |
| exec output to a caller without the right | `sandbox.exec` guards exec, attach, files, and logs are `sandbox.read` and never include exec output | 008 |
| a key in the environment | `CELLA_TOKEN_KEY` and the webhook secrets are read once at start and never logged; the deploy manifests mount them from Secrets | 002, 014 |
| a dependency with a known vulnerability | the `vuln` gate on every push | 002 |
| an image that is not what was released | signed images, SBOMs, and provenance attestations; `gh attestation verify` documented | 014 |

### What is out of scope

Kernel escapes the backend's runtime class does not prevent; an
operator picks gVisor or Kata through a decorator. The security of the
issuer and of the webhooks themselves. Secrets management beyond
`env`; a plane brokers.

## Not in this spec

The deploy manifests that carry the Pod security fields
([[014-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A Pod the k8s backend creates has every field of the escape control set | `TestPodSecurityFields` over the rendered Pod, and the kind tier's `TestClusterPodIsConfined` running `id`, `cat /proc/1/status`, and a mount attempt | not built |
| A sandbox cannot reach another sandbox's IP or the Pod network | `TestClusterNoLateralMovement` | not built |
| `GET` of another subject's sandbox is 404 and identical to a missing id | `TestForbiddenLooksLikeNotFound` | not built |
| A canary env value and a canary token appear in no event, log line, or status body across the e2e tier | `TestNoSecretLeaks` grepping every capture | not built |
| A YAML body with a billion-laughs alias chain is refused in under 100 ms | `TestYAMLBombIsRefused` | not built |
| Every control in the table names a test that exists in the tree | `TestThreatModelControlsHaveTests` reading this file | not built |
