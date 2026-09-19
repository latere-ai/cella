---
title: "Native runtime migration: durable lifecycle, execution and file transfer"
status: complete
track: core
depends_on:
  - specs/004-runtime-contract.md
affects: [driver/]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Native runtime migration

## Overview

The first reusable runtime slice moves the hosted sandbox's native lifecycle,
metadata, process execution and archive transfer concepts into a public driver.
It runs host processes with **no isolation** and is for local development and
tests. Hosted policy, accounts, billing and scheduling do not enter this package.

## Design

- Export a driver contract using the lifecycle, inspect, execution and archive
  operations in spec 004. `driver` avoids collision with Go's runtime package.
- Persist owner, name, labels and lifecycle timestamps atomically on disk.
- Create starts a directory-backed environment. Start and stop are idempotent;
  stop/delete terminate managed executions. No image is pulled.
- Execute host commands in the mapped workspace with explicit environment,
  bounded duration, separate output streams and real exit status.
- Import/export workspace tar archives while running or stopped. Confine file
  API paths using `os.Root`; refuse traversal, links and special archive entries.
- Report only implemented capabilities. PTY, background main commands, resource
  enforcement, volumes, pools, scheduling, ingress and recovery of active
  processes are deferred and are not advertised.

## Acceptance criteria

- A native end-to-end test creates, executes, transfers files, stops, reopens,
  starts and deletes an environment while preserving identity and timestamps.
- Execution tests cover nonzero exit, cancellation, timeout and stopped state.
- Archive tests reject traversal and link escapes and preserve ordinary modes.
- Race tests pass; package statement coverage exceeds 90%.

## Outcome

Implemented the public `driver` contract and `driver/native`. The end-to-end
suite covers create, execute, archive round-trip while stopped, reload metadata,
restart and delete. `go test -race -coverprofile=/tmp/cella-native.cover
./driver/...` passes with 95.5% native statement coverage.

### Migration provenance

The native runtime ports the hosted implementation's atomic JSON metadata model
(`native/meta.go`), idempotent start/stop and workspace mapping (`native/native.go`),
command environment merging and nonzero exit-code handling (`native/exec.go`).
Host-specific policy, proxy, account and billing fields are removed. The former
native runtime's OS confinement belongs to the separate `local` driver in spec
004; this package accurately reports `none` and does not claim confinement.

Archive transfer replaces the hosted `native/files.go` shell tar calls with Go
archive streams and `os.Root`. This prevents path/link escapes at the driver
boundary and removes the tar executable dependency. Per-file staged writes
preserve the prior file if an upload is truncated. Tests cover these regressions.
Execution uses process-group cancellation and a single background reap; stop,
delete and close cancel child process groups, and host environment secrets are
not inherited. Concurrent stdout/stderr draining follows the streaming contract.

### Remaining contract

The implemented slice accepts directory-backed environments and explicit exec.
Image pulls, main-process supervision, PTY, logs, watch, resource enforcement,
token projection, volumes, snapshots and active-process recovery remain in spec
004. Nonempty image/main-command fields and unsupported exec flags fail explicitly.
Lifecycle timestamps are durable; enforcement belongs to the controller in spec
005. This completes the scoped migration, not the full runtime contract.
