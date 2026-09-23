---
title: "The desktop on k8s: CELLA_K8S_DISPLAY_IMAGE, the published cella-display image, and the kind tier's computer-use run"
status: in-progress
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/014-release-and-installation.md
  - specs/015-conformance-suite.md
  - specs/023-computer-use-operations.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/036-k8s-driver.md
  - specs/.archive/041-display-and-input.md
affects: [internal/config/, deploy/examples/kind-stubs/, .github/workflows/, docs/, CHANGELOG.md]
effort: small
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# The desktop on k8s

## Overview

[[041-display-and-input]] built the desktop on the k8s driver: with
`Options.DisplayImage` set, a sandbox whose manifest names `display` runs
a second container from that image beside the workload, the driver
declares `Display` and `Input`, and `DisplayReady` follows the
container's readiness probe. Three things keep that from reaching an
installation:

| Piece | Where it is | What is missing |
|---|---|---|
| `Options.DisplayImage`, `Options.DisplayResources` | `runtime/k8s` | no variable sets them, so `cellad serve` on k8s never declares a desktop |
| The `cella-display` image | `images/display/Dockerfile`, named in the artifact table of [[014-release-and-installation]] | no pipeline builds or publishes it, so an operator has no image to name |
| The computer-use case on a cluster | `case023BrowserReady` of the conformance suite | the kind stack declares `files,pool,mesh` and passes no display image, so the case skips there and the k8s desktop has never run against a real API server |

A hosted plane on the k8s driver therefore has no screen, while the
podman driver has one.

## Design

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CELLA_K8S_DISPLAY_IMAGE` | empty | the image the desktop container runs; empty declares no `Display` and no `Input` |
| `CELLA_K8S_DISPLAY_CPU` | the driver's default CPU | the desktop container's CPU limit |
| `CELLA_K8S_DISPLAY_MEMORY` | the driver's default memory | the desktop container's memory limit |

The image has no default, as the driver's own comment requires: a
reference names a registry, and the core fixes no coordinate of any
installation. The documentation names the published image by its
template, `ghcr.io/<owner>/cella-display:<tag>`.

Every resource quantity of the driver is parsed at start:
`CELLA_K8S_DEFAULT_CPU`, `CELLA_K8S_DEFAULT_MEMORY`,
`CELLA_K8S_DEFAULT_DISK` and the two display limits. A value the API
server would refuse is a start-up problem naming the variable, rather
than a `driver_unavailable` on every create.

### The published image

The release pipeline builds `images/display/Dockerfile` for
`linux/amd64` and `linux/arm64`, pushes it by digest under no tag,
signs it, attests its bill of materials and its provenance, and tags
the digest with the release's tag only in the publish job, the same
path `cellad` takes. The image's bill of materials is a release asset,
`sbom-cella-display.spdx.json`. The clean-runner job verifies the
signature and the attestation by tag.

### The kind tier

The kind stack of `deploy/examples/kind-stubs` runs the desktop:
`up.sh` builds and loads `cella-display:dev` beside the other two
images, and the overlay's ConfigMap sets `CELLA_K8S_DISPLAY_IMAGE` to
it. The conformance run against the stack, in `verify` and in
`release`, declares `display` and `input` and passes the same image as
the suite's display image, so `case023BrowserReady` creates a desktop
sandbox, reads `DisplayReady`, takes a screenshot and sends an input
batch through the API of a real cluster. In `release`, the stack runs
the published display image under that name, as it does `cellad`.

## Not in this slice

- A display image in the base of `deploy/`. The base declares no
  desktop, so an installation opts in by setting the variable, and the
  archive's pinned digest stays the one image the base runs.
- Attach, dial and resize on k8s, which are their own slices.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `CELLA_K8S_DISPLAY_IMAGE`, `CELLA_K8S_DISPLAY_CPU` and `CELLA_K8S_DISPLAY_MEMORY` reach the driver's options, empty by default | `TestLoadK8sDisplay` | planned |
| A resource quantity of the driver that does not parse is a start-up problem naming the variable | `TestLoadK8sQuantities` | planned |
| The driver built from that configuration declares `Display` and `Input` with an image and neither without one | `TestLoadK8sDisplay`, `TestCapabilitiesFollowTheDisplayImage` | planned |
| The kind stack loads the display image, sets the variable, and both kind conformance runs declare `display,input` and pass the image | `TestKindStackRunsTheDesktop` | planned |
| The release builds the display image for both architectures by digest, signs and attests it, tags it only in publish, attaches its bill of materials, and the clean runner verifies it | `TestReleasePublishesTheDisplayImage` | planned |
