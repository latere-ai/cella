---
title: "Computer use operations: display, screenshot, input, ports, browser-ready sandboxes"
status: drafted
track: core
depends_on:
  - specs/004-runtime-backend-contract.md
  - specs/008-api.md
affects: [internal/api/, runtime/, manifest/v1/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Computer use operations

## Overview

An agent that uses a computer needs more than a shell: a screen to
look at, a pointer and a keyboard to act with, and ports to reach the
applications it runs. These are operations on a sandbox like exec and
files are, gated by the same authorizer action, streamed through the
same API, and executed by the same driver, with a `Display` capability
that says whether an environment has a screen to offer. This spec
fixes the display, the screenshot and input operations, port access,
and what a browser-ready sandbox is.

## Current state

Not built. The hosted platform has a GUI desktop driver with
screenshot and input validation; this spec restates it as part of the
public contract.

## Design

### Display

`spec.display {width, height}` asks for a virtual desktop: an X server
on the display, a window manager, and a VNC-less screen the operations
below read and drive. On k8s and podman it is a sidecar or an
in-container process the driver adds; on `vm` it is the guest's. An
environment without `Display` refuses the field at resolve. The
desktop starts with the sandbox and its readiness is
`conditions[DisplayReady]`.

### Operations

| Method | Path | Does | Action |
|---|---|---|---|
| `GET` | `/v1/sandboxes/{id}/screenshot?format=png&scale=0.5` | one frame, PNG or JPEG, optionally scaled | `sandbox.exec` |
| `GET` | `/v1/sandboxes/{id}/screen` | a WebSocket of frames at `?fps=` up to 10, binary PNG, for a live view | `sandbox.exec` |
| `POST` | `/v1/sandboxes/{id}/input` | a batch of events: `{"events": [{"type": "move", "x": 100, "y": 200}, {"type": "click", "button": "left"}, {"type": "key", "key": "Return"}, {"type": "type", "text": "hello"}, {"type": "scroll", "dx": 0, "dy": -3}, {"type": "wait", "ms": 200}]}`, executed in order; 200 with the count | `sandbox.exec` |
| `GET` | `/v1/sandboxes/{id}/display` | the geometry and `DisplayReady` | `sandbox.read` |

Input validation is the driver's shared package: coordinates inside the
geometry, key names from one table, `text` at most 4 KiB per event, a
batch at most 256 events, so a driver receives only what a desktop can
do. Every batch emits one `sandbox.input` event with the count and
never the text.

### Ports

`spec.network.ports[]` declares what runs inside. Three ways to reach
a port:

| `expose` | Reached by |
|---|---|
| `none` | `GET /v1/sandboxes/{id}/ports/{name}` as an HTTP proxy for an HTTP port, and `Dial` through `cella port-forward` for anything else, both under `sandbox.exec` |
| `mesh` | peers, by name ([[022-mesh-and-spawn]]) |
| `public` | an endpoint in `status.ports[].url`, provided by the environment's `Ingress` decorator; the URL is stable for the sandbox's life and gone with it |

A port declared and not listening is a `Ready: True` sandbox with a
502 on its proxy, said in `status.ports[].state`.

### Browser-ready

A browser-ready sandbox is a manifest, not a feature: an image with a
browser, a `display`, a `mesh` or `none` port for the browser's
automation protocol, and an egress allow list of the sites the task
needs. The conformance suite carries one such manifest as its
computer-use scenario, and `docs/` carries it as an example, so the
combination is proven rather than described.

## Not in this spec

The desktop's implementation per driver ([[004-runtime-backend-contract]]);
the `Ingress` decorator's shape ([[016-building-a-plane]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A sandbox with `display` reaches `DisplayReady` and a screenshot is a valid PNG of the declared geometry | `TestScreenshot` on podman and k8s | not built |
| An input batch types text into a focused terminal and the next screenshot shows it | `TestInputRoundTrip` | not built |
| Out-of-geometry coordinates, an unknown key, and a 257-event batch are each `invalid_field` | `TestInputValidation` | not built |
| The screen stream delivers frames at the requested rate and closes with the sandbox | `TestScreenStream` | not built |
| An HTTP port with `expose: none` is reachable through the proxy route and by nothing else; `public` gets a URL where `Ingress` holds | `TestPorts` | not built |
| The browser-ready manifest in `docs/` runs the computer-use scenario end to end | conformance `case023BrowserReady` | not built |
| `display` on an environment without `Display` is refused at resolve | `TestDisplayCapability` | not built |
