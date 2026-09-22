---
title: "Computer use operations: the desktop, screenshot, screen, input, ports and the proxy, browser-ready sandboxes"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/008-api.md
  - specs/009-events.md
affects: [internal/api/, runtime/, runtime/display/, manifest/v1/, docs/, test/conformance/]
effort: medium
created: 2026-09-12
updated: 2026-09-23
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
fixes the desktop, the types and validation of every operation, the
screenshot and screen streams, the input batch and its failure
semantics, port access through the proxy, dial, and public exposure,
and what a browser-ready sandbox is.

## Current state

The desktop, the screenshot, the screen stream, the input batch and the
port probe are built by [[041-display-and-input]] on podman and k8s, with
the types and the rules in `runtime/display` and the four routes in
`internal/api`. The HTTP port proxy and the dial socket are built by
[[060-dial-and-port-proxy]] over the `Dial` of the native and podman
drivers. `expose: mesh`, `expose: public` and the browser-ready example
are not: the first two need `Mesh` and an `Exposer`.

## Design

### The desktop

`spec.display {width, height}` asks for a virtual desktop; absent, the
sandbox has none, `DisplayReady` is absent from its conditions, and
`GET .../display` is `not_found`. With it, the driver runs the desktop
from the `cella-display` image [[014-release-and-installation]]
publishes: an X server on `:0` at the declared geometry and depth 24,
the image's window manager, and the capture tool. The workload sees
`DISPLAY=:0` and the socket at `/tmp/.X11-unix/X0`; on k8s the
desktop is a sidecar container and `/tmp` is one `emptyDir` shared by
both containers, which is the one writable mount the baseline of
[[004-runtime-contract]] already grants, with no host IPC; on podman
the desktop is a second process in the container; on `vm` the guest
agent runs it. `DisplayReady` is written by the driver when the X
socket accepts connections and the window manager has registered. The
desktop is rebuilt at every `Start`, since it lives in the process
tree that `Stop` ends; the geometry is immutable
([[003-manifest-contract]]). A pool entry with `spec.pool.display` has
its desktop up, and adoption requires the geometry to match
([[020-scheduling-and-sets]]). Frames are captured from the root window
and encoded in the driver.

### Types

Declared in `runtime/display`, the shared package every driver's
desktop uses for validation, and aliased by `runtime`:

| Type | Fields |
|---|---|
| `Geometry` | `Width`, `Height` int |
| `ScreenshotRequest` | `Format` (`png`, `jpeg`; default `png`), `Scale` (0.1 to 1.0; default 1.0) |
| `Frame` | `At` time, `Format`, `Data []byte` |
| `InputEvent` | `Type`, `X`, `Y`, `ToX`, `ToY` int, `Button` (`left`, `middle`, `right`), `Modifiers []string` (`ctrl`, `alt`, `shift`, `super`), `Key` string, `Text` string, `Direction` (`up`, `down`, `left`, `right`), `Amount` int, `Ms` int |

| Event type | Required | Optional | Does |
|---|---|---|---|
| `move` | `x`, `y` | | moves the pointer |
| `mouse_down`, `mouse_up` | `button` | `x`, `y`, `modifiers` | presses or releases at the pointer, or at `x`, `y` after a move |
| `click`, `double_click`, `triple_click` | `button` | `x`, `y`, `modifiers` | one, two, or three presses and releases |
| `drag` | `button`, `x`, `y`, `toX`, `toY` | `modifiers` | press at `x`, `y`, move, release at `toX`, `toY` |
| `scroll` | `direction`, `amount` | `x`, `y`, `modifiers` | `amount` wheel clicks, 1 to 50, as X buttons 4 to 7 |
| `key` | `key` | `modifiers` | one keysym pressed and released with the modifiers held |
| `type` | `text` | | the text typed as Unicode, at most 1 KiB |
| `wait` | `ms` | | pauses 1 to 10000 ms |

Validation runs over the whole batch before anything executes: a
field outside its range or enum, a missing required field, a
coordinate outside the geometry, a `key` that does not match
`^[A-Za-z0-9_]+$` (the keysym rule, which also keeps a leading `-`
from reaching the driver's argument list), a batch above 256 events,
or a batch whose `wait` total exceeds 60 seconds is `invalid_field`
with `paths` naming `events[i].<field>`. The body cap of [[008-api]],
1 MiB, is checked before validation and is 413; 256 events of 1 KiB
text and their envelope fit inside it.

### Operations

| Method | Path | Does | Action |
|---|---|---|---|
| `GET` | `/v1/sandboxes/{id}/display` | the geometry and `DisplayReady`; `not_found` without a display | `sandbox.read` |
| `GET` | `/v1/sandboxes/{id}/screenshot?format=&scale=` | one frame as `image/png` or `image/jpeg` | `sandbox.exec` |
| `GET` | `/v1/sandboxes/{id}/screen?fps=&format=` | a WebSocket, `cella.screen.v1`, binary frames server-paced at `fps` up to 10; a client behind by one frame has the newest frame dropped, never queued; closed with 1000 when the sandbox stops | `sandbox.exec` |
| `POST` | `/v1/sandboxes/{id}/input` | a batch `{"events": [...]}`; validated whole, then executed in order; stops at the first driver failure and answers 200 `{"executed": n, "failed": {"index", "reason"}}`, or `{"executed": n}` when all ran | `sandbox.exec` |

Every operation needs the `Display` capability, `input` also `Input`
([[008-api]]). Each screenshot, each input batch, each proxied request,
and each screen frame counts as activity: the handlers call
`Controller.Touch`, coalesced by [[005-lifecycle-controller]].

### Ports

`spec.network.ports[]` declares what runs inside. Three ways to reach
a port:

| `expose` | Reached by |
|---|---|
| `none` | `any /v1/sandboxes/{id}/ports/{name}/{path...}`, an HTTP proxy under `sandbox.exec`, and the dial WebSocket through `cella port-forward` ([[008-api]], [[011-agent-client]]) |
| `mesh` | peers, at `<sandbox-name>.mesh` ([[022-mesh-and-spawn]]) |
| `public` | an endpoint in `status.ports[].url`, provided by the environment's `Exposer` decorator ([[004-runtime-contract]]); TLS termination and the hostname scheme are the exposer's; the URL is stable for the sandbox's life and gone with it |

The proxy resolves `{name}` against the sandbox's own declared ports
and nothing else: an undeclared name is `not_found`, and no
caller-supplied host or port ever reaches the dialer, so the route
cannot be turned toward another sandbox or the Pod network
([[013-security-and-threat-model]]). It dials through the driver's
`Dial`, forwards every method and the path suffix, forwards headers
but the hop-by-hop set, passes a WebSocket upgrade, streams bodies
both ways, waits 60 seconds for response headers, and answers 502
`upstream_unavailable` when nothing listens or the sandbox is not
`Running`. `status.ports[].state` is the driver's probe at inspect
([[004-runtime-contract]]).

### Browser-ready

A browser-ready sandbox is a manifest, not a feature: an image with a
browser, a `display`, a `none` port for the browser's automation
protocol, and an egress allow list of the sites the task needs.
`docs/examples/browser.yaml` carries it under `cella.latere.ai/v1beta1`,
and the computer-use group of [[015-conformance-suite]] runs it:
apply, wait for `DisplayReady`, drive the browser through the port,
screenshot, type, screenshot again.

### Events

Each screenshot emits `sandbox.screenshot {width, height, format}`,
each input batch `sandbox.input {events: n}`, each screen session
`sandbox.screen {durationMs, bytesIn, bytesOut}` on close, and each
proxied request `sandbox.port` when `CELLA_EVENTS_PORTS=1`; never the
text, the frames, or a body ([[009-events]]).

## Not in this spec

Per-driver process placement of the desktop ([[004-runtime-contract]]);
the `Exposer`'s implementation, which is a platform's; the display
image's contents beyond the X server, the window manager, and the
capture tool ([[014-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every rule in the validation table refuses with `invalid_field` and the `events[i].<field>` path; a batch of 256 one-KiB `type` events passes the body cap; a 257th is `invalid_field`; a total wait above 60 s is refused | `TestValidate` and `TestValidateBatchBounds` over the rules, `TestInputValidation` and `TestInputBodyCap` over HTTP | passing, [[041-display-and-input]] |
| A sandbox with `display` reaches `DisplayReady` with the X socket and the window manager up; without it the condition is absent and `GET .../display` is `not_found`; the desktop is rebuilt at `Start` | `TestCreateStartsTheDesktop` and `TestDisplayReadyFollowsTheProbe` on podman, `TestDisplayReadyIsTheContainersOwn` on k8s, `TestDisplayRoutes` over HTTP | passing, [[041-display-and-input]] |
| A screenshot is a valid PNG or JPEG of the declared geometry at the requested scale | the `DisplayScreenshot` conformance case, `TestScreenshot` on podman, `TestDisplayRoutes` over HTTP | passing, [[041-display-and-input]] |
| Every gesture type lands: a drag moves a window, a chord `ctrl+a` selects, typed text is visible in the next frame, a scroll moves a document | `TestArgv`, table-driven | partial, [[041-display-and-input]]: every type expands to the argument lists the input tool takes, and a chorded click no longer clears the modifiers it is held under; what the desktop then shows is not asserted |
| A driver failure at event 5 of 10 answers 200 with `executed: 4` and the index; `Touch` is called for each operation | `TestInputPartialFailure`, `TestScreenStampsActivity` | passing, [[041-display-and-input]] |
| The screen stream paces at the requested rate, drops the newest frame for a slow client, and closes 1000 with the sandbox | `TestFramesDropsForASlowReader`, `TestScreen` on podman, `TestScreenStream` and `TestScreenSessionEndsWithTheClient` over HTTP | passing, [[041-display-and-input]] |
| The proxy forwards every method and the path suffix, passes a WebSocket upgrade, answers `not_found` for an undeclared name and 502 for a closed port or a stopped sandbox, and never dials an address the caller supplied | `TestPortProxy`, `TestPortProxyIsConfined` | built ([[060-dial-and-port-proxy]]); the `sandbox.port` record under `CELLA_EVENTS_PORTS=1` is not emitted |
| `public` gets a URL where an `Exposer` is installed and `Ingress` is declared; `state` follows the probe | `TestPublicPorts` | not built |
| `docs/examples/browser.yaml` resolves under the current schema and runs the computer-use scenario end to end | conformance `case023BrowserReady` | not built |
| `display` on an environment without `Display` or `Input` is refused at resolve | `TestDisplayCapability`, `TestDisplayCapabilityGate` | passing, [[041-display-and-input]] |
