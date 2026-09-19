---
title: "Native main process and logs"
status: in-progress
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
