---
title: "Native main process and logs"
status: complete
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/025-native-runtime-migration.md
affects: [runtime/native/, runtime/driver.go]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Native main process and logs

## Overview

A native environment can run a supplied command as its main process. Its actual
exit becomes observable state, its output is available through Logs, and stop,
start and delete manage the process rather than only a workspace flag. This
implements another slice of the existing runtime and lifecycle specs.

## Design

Persist the command and arguments beside the existing identity and environment.
Reuse the native execution process-group cancellation and one-reaper machinery.
The main process outlives the create request and ends on runtime Close, stop,
delete or natural exit. Natural exit records Stopped for zero and Failed otherwise, with reason Exited
and the exit code;
failed command launch is an error and never claims Running. Start repeats the
stored command. Empty commands retain directory-backed exec workspaces.

Capture stdout and stderr into timestamped chunks in an append-only log file.
Logs returns their combined bytes, supports since, tail lines and follow, and
ends follow when the main process ends. Closing the reader cancels the follower.
Requested initial tails are capped at 1 MiB of retained bytes and refuse larger
selections; untailed streaming has no total-size limit. Log record reads are
bounded to 256 KiB, above the maximum record produced by the process copier.

The native driver remains isolation none and Detach false. This slice does not
recover an active process after an unexpected daemon crash. A persisted Running
main-process record becomes Lost with reason ProcessUnrecoverable on reopening;
start and stop refuse that record so no duplicate is launched. Operators must
terminate any surviving process themselves; delete only removes the stale record.
Orderly Close cancels and reaps every process and persists Stopped first.
Image resolution, resource enforcement, PTY and detached recovery remain in spec 004.

## Acceptance criteria

- E2E: command starts, output is observable, stop reaps, restart reruns, delete
  removes the workspace; natural success and nonzero exits preserve exit codes.
- Closing waits for persisted completion; reopen after orderly close succeeds;
  reopening a stale main process reports Lost and refuses restart.
- Logs tests cover since, tail lines, follow, cancellation and read errors.
- A child left by a completed exec is terminated, demonstrated by a regression
  test which fails without group cleanup.
- Race tests pass with native statement coverage above 90%.

## Outcome

Implemented explicit-command supervision in `runtime/native`, using the same
host command construction, environment merging, process-group cancellation and
single execution reaper introduced in spec 025. Main processes persist their
restart argv, timestamps, exit reason and exit code. They outlive HTTP request
cancellation; orderly shutdown waits for their stored terminal state and reports
persistence failures. Unexpected restart exposes Lost without trusting a reused
PID or launching a duplicate. Image-based launches and detached recovery remain
unimplemented parts of spec 004.

Logs capture both output streams in serialized timestamped records. Snapshot,
since, tail and follow behavior are covered; reader closure and context
cancellation release followers. A truncated final record returns an error,
including after the process ends. This was verified with a regression that
previously spun until its context deadline.

Execution also terminates descendants that detach their IO and outlive their
parent command. Removing that cleanup makes the marker-writing regression fail;
restoring it passes. Lifecycle cleanup waits for the existing reaper, so no new
process-wait implementation was introduced.

`go test -race -timeout=45s -coverprofile=/tmp/cella-native-main.cover
./runtime/...` passes at 93.6% native statement coverage. `go vet ./runtime/...`
passes. End-to-end tests cover commands, logs, success/failure exit states,
stop/restart/delete, orderly reopen and unrecoverable process records.
