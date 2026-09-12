---
title: "Release and installation: images, binaries, attestations, the deploy manifests, cellad check, upgrades"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/012-test-stubs-and-tiers.md
  - specs/015-conformance-suite.md
affects: [.github/workflows/release.yml, Dockerfile.ci, deploy/, tools/release/, docs/install.md, docs/upgrades/, cmd/cellad/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Release and installation

## Overview

A `v*` tag is a release: two binaries for four platforms, two images,
checksums, signatures, bills of materials, provenance, a deploy
archive, and a GitHub release whose notes are the CHANGELOG section for
that version. Installation is one document that is executed by CI on
every push, so it is never stale. `cellad check` tells an operator
whether an installation meets the requirements before the first
manifest.

## Current state

Not built. `verify.yml` runs the gate; there is no `release.yml`, no
`deploy/`, and no install document. The CHANGELOG rule is already in
force through the gate.

## Design

### Artifacts

| Artifact | Name |
|---|---|
| binaries | `cellad_<tag>_<os>_<arch>.tar.gz`, `cella_<tag>_<os>_<arch>.tar.gz` for linux and darwin, amd64 and arm64, plus `checksums.txt` |
| images | `ghcr.io/<owner>/cellad:<tag>`, `ghcr.io/<owner>/cella-stubs:<tag>`, multi-arch, digest-pinned in the release notes |
| attestations | cosign signatures over the images and the checksums; an SPDX SBOM per image and one for the module graph; SLSA provenance per image |
| deploy archive | `deploy-<tag>.tar.gz`, the kustomize base and examples with the image references rewritten to the release |

The image namespace derives from the repository owner running the
workflow, overridable by `CELLA_IMAGE_NAMESPACE`, so a fork's tag
publishes under the fork's namespace; a test refuses a fixed `latere-ai`
anywhere in the workflow or the archive.

### Pipeline

`release.yml` on `v*`: build (archives, images, signatures,
attestations, the deploy archive) → conformance (the kind stack of
[[012-test-stubs-and-tiers]] from the published images, running
[[015-conformance-suite]]) → publish (the GitHub release) →
install-release (`docs/install.md` walked against a bare kind cluster
using the published artifacts) → release-verify (a clean runner
downloads the archives and checks the sums and signatures). A deploy
to an operator's own cluster is not part of the public pipeline; the
archive is the hand-off.

`Dockerfile.ci` copies the binary the pipeline built; its runtime stage
is byte-identical to `Dockerfile`'s between the markers, checked by a
test.

### Deploy manifests

`deploy/base`: Namespace-less Deployment (one replica; the Postgres
lease of [[010-state]] permits more), Service, ServiceAccount with a
Role limited to Pods, PVCs, NetworkPolicies, and Secrets in one
namespace, NetworkPolicy for `cellad` itself, PodDisruptionBudget, and
the Pod security fields of [[013-security-and-threat-model]].
`deploy/bootstrap`: the Namespace and Secret templates for
`CELLA_TOKEN_KEY` and the webhook secrets, applied by hand once.
`deploy/examples/kind`, `deploy/examples/generic`: overlays an operator
starts from. Every overlay renders in CI.

### cellad check

Reads the whole configuration and prints one line per requirement:
issuer discovery reachable, token key parses, authorizer and admission
and sink answer a probe, backend reachable with the permissions the
Role grants (a dry-run Pod create), Postgres reachable and at the
binary's schema, data directory writable. Exit 1 on any failure. The
install document ends with it.

### Versioning

Semantic versions. Before `v1.0.0` a minor may change the schema or
the exported packages with a CHANGELOG entry; from `v1.0.0` the
manifest contract's rules ([[003-manifest-contract]]) and the
packages' promises ([[001-architecture]]) bind. `docs/upgrades/` says
what to verify, how to roll back, and that a downgrade across a schema
migration is refused.

## Not in this spec

The tiers themselves ([[012-test-stubs-and-tiers]]); the suite the
conformance job runs ([[015-conformance-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every artifact in the table is attached to the release of a tag | the `release-verify` job | not built |
| The workflow and the archive fix no image namespace | `TestReleasePublishesUnderTheOwnersNamespace` | not built |
| `Dockerfile` and `Dockerfile.ci` share the runtime stage byte for byte | `TestRuntimeStagesMatch` | not built |
| Every overlay renders and the base carries every Pod security field | `TestOverlaysRender`, `TestBaseIsConfined` | not built |
| `docs/install.md` walks green against a bare kind cluster on every push | the `install` job | not built |
| `cellad check` fails on each requirement removed one at a time | `TestCheckNamesEachFailure` | not built |
| A tag without a CHANGELOG section is refused at pre-push and in the pipeline | the gate's `release` command | passing for the rule; the pipeline not built |
