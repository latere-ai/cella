---
title: "Direct control plane: durable native workspace lifecycle and authorized HTTP API"
status: in-progress
track: core
depends_on: []
affects: [manifest/, controller/, internal/api/]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Direct control plane

## Overview

Make the existing identity layer and extracted runtime usable together: authenticated callers create durable native workspaces, execute commands, stop, restart, list and delete them. This is a deliberately narrow implementation slice of designs 003, 005, 008 and 010, not completion of those designs.

## Contract

`manifest/v1` defines the Sandbox envelope; `manifest` strictly decodes JSON and resolves the supported native shape. The native environment uses the host toolchain and an empty image; it advertises isolation `none`. Unsupported image, entrypoint, resource, networking, secret, volume, lifecycle, scheduling and spawn settings fail closed. Scheduling mode belongs to the configured environment and this slice only supports direct execution. User metadata is immutable after create. Owner and status come from verified identity and stored state.

`controller` stores desired objects in an atomic JSON snapshot in its private data directory. One controller process owns the directory. A process mutex serializes name reservation and per-owner quota checks with mutation; desired state is persisted before driver creation. Interrupted creation remains visible as Pending or Failed and can be deleted. Reads refresh runtime phase while retaining trusted ownership. State survives reopening; corrupt snapshots refuse startup. This slice has no distributed store or reconciliation loop.

`internal/api` serves POST/GET `/v1/sandboxes`, GET/DELETE `/v1/sandboxes/{id}`, and POST `/v1/sandboxes/{id}/start`, `/stop`, `/exec?wait=1`. Create returns 201, synchronous delete 204, other results 200. Exec drains both streams, bounds retained output, and enforces timeout. Request bodies are capped and unknown fields refused. Every route verifies a bearer, authorizes its action, and uses stored resource ownership. Create also authorizes environment use. Lists intersect policy owner/label filters and selectors, and authorize each returned object. Authorizer outages fail closed. Unsupported rate limits fail closed rather than silently ignoring granted restrictions. Max-sandbox limits are enforced atomically.

## Acceptance criteria

- Strict decoding rejects unknown/unsupported fields, malformed names, reserved env and metadata, path escapes, trailing documents, and client-written ownership.
- Duplicate names and owner quota races cannot create excess runtimes; failed creation stays deletable; state survives restart and corrupt data refuses opening.
- End-to-end HTTP tests create, list, read, exec, stop, start and delete a real native workspace with owner authorization.
- Another owner cannot read, execute or mutate it; anonymous callers, outages, forbidden environment use and policy filter widening fail closed.
- Concurrent stdout/stderr are drained and output retention is bounded; stopped exec fails.

## Deferred

YAML, apply/update, streamed exec, workload spawning, quotas beyond sandbox count, durable operation recovery, events, TTL/reaping, queueing, warm pools, volumes, secrets, egress, image/container/VM drivers and multi-node scheduling remain in their owning specs. Native execution is an explicitly opted-in trusted local development mode and cannot isolate hostile workloads.
