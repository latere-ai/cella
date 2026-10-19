---
title: "The local driver runs on macOS's own Seatbelt: nothing to install on a Mac, an allowlist held by a proxy beside each stage, files that run code later write-protected inside a grant, and cellad calling hostsandbox.Main"
status: complete
track: core
depends_on:
  - specs/.archive/081-local-driver.md
  - specs/.archive/082-the-worker-as-a-library.md
affects: [go.mod, go.sum, cmd/cellad/main.go, runtime/local/local_test.go, runtime/local/stages_test.go, docs/local.md, docs/configuration.md, CHANGELOG.md]
effort: small
created: 2026-10-19
updated: 2026-10-19
author: changkun
---

# The local driver runs on macOS's own Seatbelt

## Overview

The local driver confines each command with `latere.ai/x/pkg/hostsandbox`,
which on macOS ran every stage through srt, a Node.js program that also
needs ripgrep, installed by hand before a computer could serve as an
environment. hostsandbox v0.103.0 adds a native macOS driver: Seatbelt
through `/usr/bin/sandbox-exec` with a profile it renders, reviewed four
times before release. A Mac then needs nothing installed. Linux keeps srt.

## Design

- **The driver's code does not change.** `newSandbox = hostsandbox.New`
  stays. hostsandbox's `IsolationAuto` picks Seatbelt on macOS in a
  program whose `main` called `hostsandbox.Main`, and srt otherwise, so a
  program embedding the worker keeps srt until it opts in.
- **cellad opts in.** `hostsandbox.Main()` is the first statement of
  `cmd/cellad`'s `main`: a stage with a network allowlist runs under a
  launcher that is cellad started again, which serves the stage's proxy
  and never returns from `Main`.
- **The network.** The allowlist is enforced by that proxy, which admits
  a connection by its host and port and never decrypts it: a host named
  alone is reached on 443 and 80, and `host:port` names another port. A
  destination on the machine itself or a link-local one is refused.
- **Files that run code later.** Inside a writable grant, a stage cannot
  write shell startup files, `.envrc`, `.gitconfig`, `.gitmodules`,
  `.mcp.json`, `*.code-workspace`, or anything in `.claude`, `.cursor`,
  `.vscode` and `.idea`. Inside a gitdir it writes only what everyday git
  writes, never config, hooks or a redirect. `git init`, `clone`, `config`
  and `remote add` therefore fail in a granted folder on macOS.
- **Tests.** The stand-ins that fake srt name `IsolationSRT`, the real srt
  conformance run names it too, and a second conformance run names
  `IsolationSeatbelt` on macOS.

## Acceptance criteria

1. On macOS with no srt, Node.js or ripgrep installed, `cellad worker` with the local runtime passes its preflight and serves sandboxes.
2. The runtime conformance suite passes under real Seatbelt on macOS (`TestLocalConformanceUnderSeatbelt`).
3. The stand-in tests and the srt conformance run behave as before on every platform.
4. On Linux nothing changes: srt with bubblewrap confines every stage.

## Outcome

Built on 2026-10-19 with pkg v0.103.0. The Seatbelt conformance run passes
25 cases on macOS 26.6, skipping the 15 the local runtime does not offer
(a terminal, attach, prewarm), the same as under srt. A network allowlist
is not exercised by the conformance suite; it is checked end to end in
latere-cli's device tests, which host the same driver.
