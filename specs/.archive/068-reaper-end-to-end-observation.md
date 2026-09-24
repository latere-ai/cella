---
title: "Reaper end-to-end observation: the native reaper test reads the stop from the act record instead of polling for a state the next rule ends"
status: complete
track: core
depends_on:
  - specs/005-lifecycle-controller.md
  - specs/009-events.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/037-lifecycle-enforcement.md
affects: [controller/, CHANGELOG.md]
effort: small
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# Reaper end-to-end observation

## Overview

`TestReaperEndToEndOverNative` drives the reaper loop over the native
driver with no fakes: a sandbox goes idle, the `autoStop` rule stops it
with reason `AutoStop`, and once it has been stopped for longer than its
`autoDelete` the next rule deletes it. It timed out once in the
instrumented run of the cover gate, waiting 30 seconds for the sandbox to
be `Stopped` with reason `AutoStop`, and passed on the rerun.

This slice finds the cause, states what was ruled out, and fixes the
defect where it lives.

## Current state

| Piece | Where it is | What it does |
|---|---|---|
| The loop | `controller.RunReaper`, `tick`, `Reap`, `reapEnvironment` | one pass per `ReapInterval` under the `reaper` lease; `LocalLease` grants it on every tick |
| The rules | `reapRule`, `activityOf` | `autoStop` counts from `LastActivityAt`, else `CreatedAt`; `autoDelete` from `StoppedAt` |
| The act | `enforce`, `stopLocked`, `deleteLocked` | re-reads the sandbox under the controller's lock and acts while the rule still matches |
| The test | `controller/reaper_test.go` | `ReapInterval` 5ms, `AutoStop` 40ms, `AutoDelete` 40ms; polls `Get` every 2ms for `Stopped`/`AutoStop`, then for the record to be gone |

## Design

### The cause

The state the first wait polls for is transient by design. The
`autoStop` act writes `Stopped` with reason `AutoStop`; 40ms later the
`autoDelete` act writes `Deleting` and forgets the record. The poll can
observe the stopped record only inside that window, and a probe is not
2ms apart under load: `Get` takes the controller's lock, which the
reaper holds across the driver's `Inspect` and `Stop` and the file
store's write, and the instrumented run shares the machine with every
other package of the module. A probe gap longer than the window misses
the state for good: the record is gone, `Get` answers `ErrNotFound`
from then on, and the wait runs to its 30 second bound.

Reproduced with the instrumented test binary (`-race -cover
-coverpkg=./...`) beside two processes that each keep 72 threads busy,
`-count=100 -cpu 1,2 -failfast`: the run stopped on the CI failure's
sentence. A diagnostic variant that recorded the acts and the widest gap
between two probes failed 3 times in 400 runs, each time with the probe gap
between 38ms and 127ms and the act record reading `sandbox.stopped`
with reason `AutoStop`, then `sandbox.deleting` and `sandbox.deleted`
with reason `AutoDelete`. The reaper did what the rules say; the test
did not see it.

### What was ruled out

| Candidate | Why it is not the cause |
|---|---|
| The lease never taken or lost under load | the test passes no `Lease`, so `LocalLease` grants every tick |
| The native `List` reporting a phase other than `Running` while a process starts | the manifest has no command; `Create` writes the record `Running` before it returns, and no main process exists |
| Activity stamped repeatedly | nothing calls `Touch` in the test; the native `Inspect` and `List` stamp nothing |
| A tick that errors before enforcing | the file store is not `Durable`, so no `Rebuild` runs; `Tokens` and `Retention` are nil, so the revocation sweep and the prune are no-ops; both run after the rules in any case |
| The enforce path failing silently | the act record of every failing run holds the stop with reason `AutoStop` and the delete with reason `AutoDelete` |
| The clock | the wall clock; `CreatedAt` and `StoppedAt` are the driver's wall time, and a tick reads `now` once after `List` |

### The fix

The test is wrong, not the reaper. It records the controller's acts
through the `Events` seam, which the file store emits through inside
`persist`, so every transition is kept whatever the scheduling. It waits
only for the terminal condition, the record gone, which cannot be
missed, and then asserts from the record that a `sandbox.stopped` act
with reason `AutoStop` came before a `sandbox.deleting` act with reason
`AutoDelete`, and that the driver no longer holds the workspace. The
first read happens after the delete, which is the schedule that failed
in CI, on every run.

No production code changes, and the 30 second bound of the wait is not
raised.

## Not in this slice

Other tests that poll: the rest of the controller's waits read states
that persist once reached (`Running`, a revoked token, a recovered
sandbox journaled by mutation), so a late probe still sees them.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| An idle sandbox is stopped with reason `AutoStop` and then deleted with reason `AutoDelete`, read from the act record after the record is gone, and the driver holds nothing afterwards | `TestReaperEndToEndOverNative` | built |
| The same run, under the load that reproduced the failure, passes every time | `TestReaperEndToEndOverNative` at `-count=200 -cpu 1,2` under `-race -cover` beside two CPU-saturating processes | built: 400 of 400 runs passed |

## Outcome

The defect was the test's, not the reaper's: its first wait polled for a
state that lasts one `autoDelete` and that no later probe can see once
the next rule has ended the record. The test now reads that state from
the act record and waits only for the terminal one.

| Piece | Where |
|---|---|
| The end-to-end test reading the stop and the delete from the act record | `controller/reaper_test.go` |
| `all` and `types` on the test's act recorder | `controller/spawn_test.go` |

No production code changed. `controller` is at 92.5% on the cover
gate. The run that proves the fix is the one that reproduced the
failure: the instrumented binary beside two processes that each keep 72
threads busy, `-count=200 -cpu 1,2`, passed 400 of 400, each run between
0.11s and 0.30s. The same load had stopped the old test at its 30 second
bound within one `-count=100` run.

### What diverges from the specs above

None. [[005-lifecycle-controller]]'s reaper row now names this test as
the end-to-end proof of the `autoStop` and `autoDelete` rules.

### What this leaves open

| Open | Why |
|---|---|
| A driver phase change, such as a main process that exits, producing a record | the observation loop's, as [[042-events]] recorded; this test's sandbox has no main process |
