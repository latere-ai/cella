---
title: Runnable native control plane
status: complete
track: core
depends_on: [specs/025-native-runtime-migration.md, specs/026-direct-control-plane.md]
affects: [cmd/cellad, internal/config, Makefile, docs]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Runnable native control plane

## Overview

Wire the extracted runtime and durable API into `cellad`, so a developer
can run the first working core without importing packages manually.

## Design

The native runtime stores its workspace and metadata below `CELLA_DATA_DIR`;
the controller stores desired objects separately below the same directory.
The public listener mounts the authenticated `/v1/` API. The internal
listener continues to expose probes only.

Native execution has no isolation. `CELLA_RUNTIME=native` therefore also
requires `CELLA_ALLOW_UNSAFE_NATIVE=true`. The development `make run` target
sets that opt-in and binds loopback. Unimplemented runtimes fail startup;
there is no fallback from a container runtime to native execution.

An end-to-end test starts the real server with a local issuer, creates a
sandbox, executes a command, checks owner isolation, stops and starts it,
and deletes it. A second server instance over the same state proves restart
recovery. This is a single-node development deployment, not the hosted
production replacement.

## Acceptance criteria

| Criterion | Test |
|---|---|
| Unsafe native execution requires explicit opt-in | `TestNativeRuntimeOptIn` |
| Missing runtime implementations fail startup | `TestServeRefusesUnavailableRuntime` |
| Authenticated API drives the real native runtime | `TestNativeServerEndToEnd` |
| Restart restores desired state | `TestNativeServerEndToEnd` |

## Outcome

`cellad` now mounts the authenticated native Sandbox API and refuses unimplemented
runtimes. Native execution requires explicit opt-in; `make run` binds loopback and
sets it. `TestNativeServerEndToEnd` covers two OIDC users, the platform audience,
create, exec with real exit/stdout/stderr, owner isolation, stop/start, server
restart over the same state, and deletion. The internal listener exposes no API.
The race-enabled suite passed; command-package coverage was 91.7%.

This is the trusted development slice. It does not replace an isolated hosted
runtime or complete spec 014's production manifests and release contract.

Follow-up regressions prove a second server acquires no runtime before the data
lock, a missing runtime directory fails readiness, and runtime shutdown errors
are reported. Request-size settings are wired to the HTTP API.
