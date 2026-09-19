---
title: "Workspace operations: authorized tar transfer and process logs"
status: in-progress
track: core
depends_on: []
affects: [controller/, internal/api/]
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Workspace operations

## Overview

Expose the extracted runtime's existing file primitives through the routes already designed in 008. Callers can move application inputs and results into their environments and read the main process output once the runtime supplies it.

## Design

GET `/v1/sandboxes/{id}/files?path=/workspace/...` streams a tar archive; PUT on the same route with `dest=/workspace/...` imports an `application/x-tar` body. Both require `sandbox.exec`, using stored ownership and labels. Files capability is checked before runtime access. The upload is spooled into a private temporary file under the configurable upload byte cap before extraction, so a too-large or disconnected request cannot partially overwrite workspace files. Archive entry safety and per-file replacement remain the runtime's responsibility. A later invalid archive entry may leave earlier complete files in place, as documented by the runtime contract.

Exports stream directly with `X-Cella-Error` trailers on errors after the first byte. Paths outside `/workspace` are rejected before streaming. GET `/v1/sandboxes/{id}/logs` requires `sandbox.read` and forwards `follow`, `since`, and `tail` to the runtime. Unsupported runtime modes return capability_unsupported. These routes do not introduce hosted drives or platform policy.

## Acceptance criteria

- Real HTTP tar import then export preserves file contents across stop/start.
- Another owner's upload, download and logs are denied; malformed/traversing archive paths are refused.
- Exceeding the configured upload cap leaves existing files unchanged; wrong content types and malformed selectors fail before the runtime.
- An export error before bytes produces a JSON error; a later export error is carried in the trailer.
- Main-process log reads use the runtime contract and pass a native end-to-end check when spec 030 lands.
