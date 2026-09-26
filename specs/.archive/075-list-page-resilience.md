---
title: "List page resilience: a row whose driver read fails is answered with the status last written and Observed False, and the page is not failed"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
  - specs/008-api.md
  - specs/.archive/074-client-closed-requests.md
affects: [internal/api/, manifest/v1/, docs/api.md, docs/manifest.md, CHANGELOG.md]
effort: small
created: 2026-09-26
updated: 2026-09-26
author: changkun
---

# List page resilience

## Overview

`GET /v1/sandboxes` reads every row it answers from the row's driver:
after the selectors and the authorizer, the handler calls
`Controller.Refresh`, which asks the driver of the sandbox's environment
for `Inspect`. Before this spec a read that failed failed the page: the
handler answered the error, `driver_unavailable` (503) for a driver
error and `not_found` (404) for a sandbox placed on an environment the
control plane holds no driver for.

So one environment that did not answer hid every sandbox on every other
environment, and a single sandbox whose read failed hid the rest of its
own. A caller that lists to act, such as a plane's pass that finds and
deletes the objects of deleted accounts, could not list at all until
the environment recovered, although everything it needed to act on, the
id and the owner, is desired state the control plane holds without
asking a driver.

## Design

### The row is kept and marked

A row whose read fails is answered with the status this control plane
last wrote for it, and the condition:

| Field | Value |
|---|---|
| `type` | `Observed` (`v1.ConditionObserved`) |
| `status` | `False` |
| `reason` | `EnvironmentNotHeld` where the error is `controller.ErrNoEnvironment`; `DriverUnavailable` for every other error |
| `message` | one fixed sentence: the environment did not answer this read and the status is the one last recorded |
| `since` | the instant of the failed read |

The condition is written on that one answer and never stored. A row
whose read succeeds carries no `Observed` condition; its absence is the
ordinary case, so no row read fresh grows a condition it did not have.

The driver's error goes to the log at warn, with the sandbox id and the
environment, and not into the row: a driver's error can name the
substrate behind it (an address, a namespace), and a condition carries a
user sentence and a reason, not a developer detail.

The `?phase=` selector runs after the read, as before, so it compares
the phase the row answers: the runtime's where the read succeeded, the
last written one where it did not. The cursor and the limit count the
rows answered, which now include the marked ones, so paging is
unchanged.

### What "last written" is

The status a marked row carries is the controller's own record: the
phase, conditions and instants of the last `persist`, which the
reaper's enforcement, a verb, the create's settle and the recovery
write. A list read is never persisted, so a runtime transition that
only a list observed, such as a workload that exited between two
reaper ticks, is not in it. The row can therefore say `Running` for a
sandbox the runtime has since stopped. That is what `Observed` `False`
says, and why the row is marked rather than silently answered.

### A closed request still ends the page

A read that failed because the request's own context ended is the
caller leaving, not the environment failing ([[074-client-closed-requests]]).
The handler checks the request's context before marking a row; when it
is done, the handler answers the error as before, which `observe`
counts `client_closed`, and does not go on to answer a page nobody
reads.

### Why not omit the row

The alternative was to leave a failed row out and flag the page as
partial. It was not taken:

- A caller lists to find objects. Omission hides exactly the ones whose
  environment is in trouble, which are often the ones a caller must act
  on, and the id and owner it needs are known without the driver.
- `next` is the id of the last row answered and `limit` counts answered
  rows. Omitting rows while computing `next` from the survivors either
  skips objects across pages or makes a page shorter than its limit
  without meaning the end, and a page-level flag does not say which
  rows.
- A marked row leaves the envelope unchanged. A client that decodes
  `items` and `next`, or only `metadata` and `status.id` and
  `status.owner`, reads the page as it did.

### What is unchanged

- `GET /v1/sandboxes/{id}` still refuses with the read's error. One
  object has one answer, and a caller asking about one sandbox is better
  served by `driver_unavailable`, which it retries, than by a status
  that may be stale.
- `awaitStart`, the held create, already answers the object as desired
  state holds it when a read fails, and the display routes read one
  sandbox and refuse as the item read does.
- The secret and environment lists read no driver and have no such
  failure.
- An authorizer read that produced no decision still refuses the whole
  list with `authorizer_unavailable`, as design 008's list rule says: a
  page is never answered around a decision. A driver read is not a
  decision, so the two are treated differently on purpose.

## Not in this spec

| Item | Why |
|---|---|
| A bound per row on the driver's read | the reads are sequential with the request's context and no deadline of their own, so a driver that hangs rather than fails still holds the page until the caller gives up; a per-row bound needs a figure per driver and is its own change |
| Skipping the remaining rows of an environment whose first read failed | a failed read is fast in the failure this spec answers (refused connection, unregistered environment), and a skip would mark rows whose own read would have succeeded |
| A metric of marked rows | design 017's one metric table and its test would grow a row; the warn line carries the sandbox and the error, which is what an operator reads first |
| A marker in the `cella get` table | the JSON and YAML outputs carry the condition; the column form is for a person and changing its columns is a separate decision |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| One sandbox whose driver read fails does not fail the page: every row is answered, the failed one with its last written phase and `Observed` `False` `DriverUnavailable` with a fixed sentence and no driver error, the others read fresh and unmarked; `?phase=` reads the phase the row answers; the failure is logged at warn with the sandbox and the error; the item read of that sandbox is still `driver_unavailable` | `TestAListAnswersARowWhoseRuntimeDidNotAnswer` | passing; failed on the tree before the change with `503 driver_unavailable` for the page |
| A read that failed because the caller closed the request ends the page, counted `client_closed`, and no page is answered | `TestAListWhoseCallerLeftIsNotAnsweredStale` | passing; a guard on the branch that tells the caller's leaving from the environment's failure |
| A row on an environment the control plane does not hold is marked `EnvironmentNotHeld`, and a row carries at most one `Observed` condition | `TestUnobservedNamesTheCause` | passing |

## Outcome

Built as designed.

| Piece | Where |
|---|---|
| `ConditionObserved`, `ReasonDriverUnavailable`, `ReasonEnvironmentNotHeld` | `manifest/v1/network.go` |
| The list's per-row branch and `unobserved` | `internal/api/api.go` |
| The tests | `internal/api/unobserved_test.go` |
| The list rule and the status table | `specs/008-api.md`, `specs/003-manifest-contract.md` |
| The caller's page and the release note | `docs/api.md`, `docs/manifest.md`, `CHANGELOG.md` |
