---
title: "Agent client: the cella command and the skill"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/008-api.md
affects: [cmd/cella/, internal/cellacli/, internal/cellaclient/, skills/cella/, docs/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Agent client

## Overview

`cella` is one binary that speaks the `/v1` API from a shell or from an
agent: apply a manifest, list, inspect, exec, attach, copy files, watch
events. It has stable exit codes, JSON output on request, and a skill
file that teaches an agent the command in a few hundred bytes. It is
the client the conformance suite and the documentation use, so the API
is never exercised only through `curl`.

## Current state

Not built. A platform's own CLI may wrap or replace it; this one is
the core's and knows only the core's API.

## Design

### Commands

| Command | Does | Exit |
|---|---|---|
| `cella apply -f sandbox.yaml [-w]` | `PUT` by `metadata.name`, or `POST` when absent; `-w` waits for `Running` | 0 applied; 3 refused with the code |
| `cella get [name|id] [-o json|yaml|wide]` | one or the list | 0; 4 not found |
| `cella delete <name|id>` | `DELETE` | 0; 4 |
| `cella start`, `cella stop` | the verbs | 0; 5 phase conflict |
| `cella exec <name|id> [-i] [-t] -- cmd args...` | the exec stream, stdin when `-i`, a PTY when `-t` | the command's own exit code; 6 when it could not start |
| `cella attach <name|id>` | the PTY over the WebSocket, raw terminal | 0 |
| `cella cp <src> <name>:<dest>`, `cella cp <name>:<src> <dest>` | tar in or out | 0; 6 |
| `cella logs <name|id> [-f]` | the main process output | 0 |
| `cella events <name|id> [-f]` | the journal | 0 |
| `cella token <name|id>` | a workload token on stdout | 0 |
| `cella version` | client and, when reachable, server identity | 0 |

Every command reads `CELLA_URL` and `CELLA_TOKEN`, or `--url` and
`--token`, and nothing else from the environment; there is no config
file and no login, because the token comes from the caller's issuer.
Exit 1 is a server error, 2 a usage error, 7 the server unreachable.
Errors print the API's `message` on stderr and the `code` and `detail`
after it when `-v` is set.

### Output

Human output is one line per object in `get` (name, phase, image, age,
owner) and the object in YAML for one; `-o json` is the API's JSON
unchanged, so `jq` works on it. Streams are never buffered.

### The skill

`skills/cella/SKILL.md` with `name` and `description` frontmatter: the
two variables, the four commands an agent needs (`apply`, `exec`, `cp`,
`delete`), a minimal manifest, the exit codes, and how to read a
refusal. The claim to check is that the file stays under 2 KiB and
that an agent given only it completes the conformance suite's agent
scenario.

### The client package

`internal/cellaclient` is the typed client the command uses, with one
method per route and the exec and attach streams as `io.ReadWriteCloser`
pairs. It is internal because an importer building on the packages has
no HTTP hop; a platform with its own client generates one from the
OpenAPI document.

## Not in this spec

Distribution of the binary ([[014-release-and-installation]]); the
scenario that drives it ([[015-conformance-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every command in the table calls the route in its row and exits with the code in its row on the case named | `TestCommandTable` against a stub server | not built |
| `cella exec` returns the command's own exit code and streams a 64 MiB output without buffering | `TestExecExitCodeAndStreaming` | not built |
| `-o json` output is byte-identical to the API response | `TestJSONIsTheAPIs` | not built |
| The built binary carries the exit codes through the process | `TestBinaryExitCodes` running `out/cella` | not built |
| The skill file is under 2 KiB and names every command the agent scenario uses | `TestSkillIsSmallAndComplete` | not built |
