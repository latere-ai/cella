---
title: "Release and check: the tag pipeline, the deploy tree, and cellad check"
status: in-progress
track: core
depends_on:
  - specs/014-release-and-installation.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/002-repository-scaffold.md
  - specs/.archive/036-k8s-driver.md
affects: [.github/workflows/, Dockerfile, Dockerfile.ci, tools/release/, deploy/, docs/, internal/check/, cmd/cellad/, .lateregate.yaml, specs/]
effort: medium
created: 2026-09-20
updated: 2026-09-20
author: changkun
---

# Release and check

## Overview

Slice 048 of [[031-hosted-sandbox-consolidation]], the last row of its
cutover critical path. Every other slice made `cellad` able to run a
tenant's sandboxes; this one makes a tag produce something a plane can
pin. It builds [[014-release-and-installation]]'s pipeline, deploy tree,
install document and `check` subcommand against what the tree carries on
2026-09-20, and states in the pipeline itself which jobs are real and
which wait on [[012-test-stubs-and-tiers]] and [[015-conformance-suite]].

The consumer is fixed. A hosted plane pins `ghcr.io/<owner>/cellad:<tag>`
and runs the two subcommands `serve` and `egress` from that one image; it
also runs `check` as a Job or an init container before the first manifest.
The image reference is derived from the account that pushed the tag and
is written nowhere in the tree, so a fork's tag publishes under the fork.

## Current state

`verify.yml` runs the gate, the tidy check and the developer image.
There is no `release.yml`, no `Dockerfile.ci`, no `deploy/`, no
`docs/install.md`, and `check` is an unknown subcommand. One tag exists,
`v0.1.0`, cut before the drivers, the store and the gateway role landed.

## Design

### Artifacts

| Artifact | Name | State |
|---|---|---|
| binaries | `cellad_<tag>_<os>_<arch>.tar.gz` for `linux` and `darwin`, `amd64` and `arm64` | built |
| checksums | `checksums.txt`, and `checksums.txt.sigstore.json`, the bundle `cosign sign-blob` writes | built |
| image | `ghcr.io/<owner>/cellad:<tag>`, `linux/amd64` and `linux/arm64`, pushed by digest and tagged after the candidate passes | built |
| attestations | a cosign signature over the image, an SPDX bill of materials for the image and one for the module graph, each attested with `actions/attest-sbom`, and a build-provenance attestation | built |
| deploy archive | `deploy-<tag>.tar.gz`, the kustomize base, the bootstrap templates and both examples, with the base's image reference rewritten to the release by digest | built |
| `cella_<tag>_<os>_<arch>.tar.gz` | the agent client of [[011-agent-client]] | no `cmd/cella` in the tree |
| `ghcr.io/<owner>/cella-stubs:<tag>` | the stub binary of [[012-test-stubs-and-tiers]] | not built |
| `ghcr.io/<owner>/cella-display:<tag>` | the desktop of [[023-computer-use-operations]] | not built |

`RELEASE_IMAGE_NAMESPACE`, a repository variable, overrides the derived
namespace; unset, the namespace is `github.repository_owner` lowered.
Neither the workflow nor any file of the deploy archive holds a literal
account, which `TestReleasePublishesUnderTheOwnersNamespace` reads.

### Pipeline

`release.yml` on a `v*` tag. The jobs are [[014-release-and-installation]]'s,
in its order, with one job ahead of them and with the two that depend on
unbuilt specs reduced to what is provable today.

```mermaid
flowchart LR
  gate[gate-green: verify concluded success on this commit]
  build[build: archives, image by digest, signature, SBOMs, provenance, deploy archive]
  conf[conformance: race suite at the tag, runtimetest over native and podman, the image answers]
  pub[publish: tag the digest, checksums, signature, the GitHub release]
  inst[install-release: the document against the published artifacts]
  verify[release-verify: a clean runner verifies sums, signature, attestations]
  gate --> build --> conf --> pub --> inst --> verify
```

What each job does, and what it does not.

| Job | Real today | Waiting on |
|---|---|---|
| `gate-green` | the `verify` run for the push of this commit concluded success; a tag on a red or ungated commit publishes nothing | - |
| `build` | the eight archives from one compilation, the multi-arch image pushed by digest and under no tag, `cosign sign`, two SPDX documents attested, the deploy archive with the image pinned by digest | - |
| `conformance` | `go test -race ./...` at the tag, which is the unit and race tiers and carries `runtimetest` over the native driver and over podman with the engine the job starts; then the published image answers `version` and `check` fails closed on an unreachable issuer with every optional dependency reported not configured | the kind stack of [[012-test-stubs-and-tiers]] and the suite of [[015-conformance-suite]]; the job runs neither and says so |
| `publish` | the digest takes the `:<tag>` reference, `checksums.txt` is signed, and the release body is the CHANGELOG section the gate's reader returns | - |
| `install-release` | the published deploy archive and binary archive are downloaded on a runner with nothing of the checkout, every overlay renders, and the rendered image reference is the published digest | the kind walk of `docs/install.md`, which needs an OpenID Connect issuer the job can reach: the stub issuer is [[012-test-stubs-and-tiers]]'s. The walk runs when the repository variable `RELEASE_INSTALL_KIND` is `1` |
| `release-verify` | on a clean runner with no checkout: every asset present, `cosign verify-blob` over the checksums against this workflow's identity, `sha256sum -c`, `cosign verify` over the image, `gh attestation verify` over the image, and the image's binary compared byte for byte with the archive's | - |

The every-push half is the `install` job of `verify.yml`: it renders
every overlay with `kubectl kustomize` and asserts the base pins no
namespace and no registry. The same reason holds it back from the walk.

`Dockerfile.ci` has no build stage. Its runtime stage is `Dockerfile`'s
between the two `shared runtime base` markers, byte for byte, which
`TestRuntimeStagesMatch` reads; the file's own instructions are the
`COPY` of `dist/cellad_linux_${TARGETARCH}`, the file buildx substitutes
per platform, and the entry point. The image and the archive therefore
carry one compilation, which `release-verify` proves with `cmp`.

### Deploy manifests

`deploy/base` pins no namespace and no registry, so an overlay sets both
and the release pipeline appends the `images` entry that points the
placeholder `cellad` at the published digest.

| Object | What it carries |
|---|---|
| `Deployment` | one replica, `Recreate`, the hardening below, the probes of [[002-repository-scaffold]] on the internal port, `terminationGracePeriodSeconds: 90` |
| `Service` | the public port; the internal port is the cluster's and is not routed |
| `ServiceAccount` | mounted, unlike a sandbox's: the k8s driver builds its client from the in-cluster configuration |
| `Role`, `RoleBinding` | the twelve accesses `runtime/k8s`'s verb table names, in one namespace: `get`, `list`, `create`, `delete`, `patch` on Pods and PersistentVolumeClaims, `create` on `pods/exec`, `get` on `pods/log` |
| `NetworkPolicy` | `cellad` itself: ingress on the public port, egress to DNS and to the endpoints it dials |
| `PodDisruptionBudget` | `maxUnavailable: 1`; a budget of `minAvailable: 1` over one replica blocks every node drain |

The Role is narrower than [[014-release-and-installation]] first wrote.
That spec named a dry-run create of a Pod, a PVC, a NetworkPolicy and a
Secret. The driver of [[036-k8s-driver]] writes no NetworkPolicy, because
the boundary is the gateway's ([[018-egress-and-secrets]]), and no Secret,
because a secret value is sealed in Postgres under `CELLA_SECRET_KEY`.
Granting either would be access nothing uses. The same two objects drop
out of the check.

The Pod's security context is the one [[014-release-and-installation]]
lists and is distinct from the sandbox baseline of
[[013-security-and-threat-model]]: `runAsNonRoot`, a non-zero uid, every
capability dropped, `seccompProfile: RuntimeDefault`, no privilege
escalation, and a read-only root filesystem with `CELLA_DATA_DIR` on an
`emptyDir`. `CELLA_K8S_NAMESPACE` comes from the downward API, so the
driver writes in the namespace the overlay chose rather than in the
driver's default, which would be a namespace the Role does not cover.

`deploy/bootstrap` holds the Namespace and the Secret templates an
operator applies once by hand: `CELLA_TOKEN_KEY`, the authorizer's URL
and bearer, the sink's URL and secret, `CELLA_DB_URL` with
`CELLA_SECRET_KEY`, and the gateway's `CELLA_ENVIRONMENT_KEY` with
`CELLA_EGRESS_CA_KEY`. Each template carries the command that mints its
value. A bearer and the URL it authorises are one Secret, because a URL
without its bearer is a start-up failure and the pair is rotated
together.

`deploy/examples/kind` and `deploy/examples/generic` are the two
overlays an operator starts from: the first with the memory store on a
laptop cluster, the second with Postgres, an authorizer and a sink.
`prometheusrule.yaml` sits beside the base's kustomization and outside
it, because the kind it declares is the Prometheus Operator's CRD and a
cluster without that operator refuses the apply. Its groups are
[[017-observability]]'s alerts as placeholders: the metric names are the
ones that spec fixes, and the spec is not built, so the file is a
template an operator edits rather than a rule set that fires.

### cellad check

`internal/check` reads the whole configuration and prints one line per
requirement, `ok`, `FAIL` with the reason, or `skip` with why the
requirement does not apply. Exit 1 on any failure. The mandatory rows
come first, then the optional ones in configuration order, so the output
of two installations can be read side by side.

The checks are not written twice. The line is the start-up path of
`cellad serve` asked one question at a time: `config.Load`,
`auth.Start`, `Authorizer.Check`, `Driver.Preflight`. An installation
that passes `check` is one that would have started.

| Line | What it asks | Failure means |
|---|---|---|
| `configuration` | `config.Load` over the environment | a variable is missing or malformed; the line carries every problem in one sorted message, so a deployment is fixed in one round |
| `identity` | `auth.Start`: each issuer's discovery document and key set fetched, `CELLA_TOKEN_KEY` parsed into a signer | an issuer that does not answer, publishes no usable key, or a key that cannot sign |
| `authorizer` | `Authorizer.Check`: the reserved probe id `sbx_00000000000000000000000000` ([[006-identity]]) | the endpoint is unreachable, or it allowed the probe, which is an endpoint that does not read the request. The line holds under the built-in owner policy, which denies the probe too |
| `backend` | `Driver.Preflight` for the selected `CELLA_RUNTIME` | on `k8s`, the cluster does not answer or the ServiceAccount lacks one of the verb table's accesses, which the line names, or the storage class does not exist; on `podman`, no candidate socket answered; on `native`, the state directory is gone |
| `data directory` | a file created and removed under `CELLA_DATA_DIR` | the readiness probe of [[002-repository-scaffold]] would fail from the first request |
| `admission` | `CELLA_ADMISSION_URL` | not configured when unset. Set, the line reports that the client lands with slice 047: `internal/config` reads no admission variable today, so `check` cannot probe an endpoint the binary does not dial |
| `sink` | `CELLA_EVENTS_URL`: one signed probe record, `Cella-Signature` over the body under each configured secret | not configured when unset. A 2xx is the acknowledgement; a 400 also passes, because the signature verified and the sink refused the body, which is [[009-events]]'s permanent drop; a 401 is a secret the two ends do not share |
| `store` | `CELLA_DB_URL`: the pool opens and `schema_migrations` is read | not configured when unset. A database with no schema passes, since `serve` applies it at start; a version below the binary's passes and says how many migrations are pending; a dirty schema, or a version above the binary's, fails with both numbers, which is `internal/store/postgres`'s own guard |
| `gateway` | `CELLA_GATEWAY`: the host resolves | not configured when unset. The line resolves and does not dial: the boundary a plane deploys admits the sandboxes to the gateway and not the control plane, so a refused connection from `cellad` is the policy working |

The backend line asks `Preflight` rather than issuing a dry-run create.
The two are the same question: `Preflight` runs one
`SelfSubjectAccessReview` per entry of the driver's own verb table, which
is the list a Role is written from, and it writes nothing. A dry-run
create would need the driver's Pod renderer, which is `runtime/k8s`'s,
and would prove less.

The store line reads and never migrates. `postgres.Open` applies the
pending migrations, which is right for a process that is about to serve
and wrong for a command an operator points at a live database.

### Versioning

Semantic versions, and the module stays `v0`, so a minor may change the
schema or an exported package with a CHANGELOG entry until `v1.0.0`. The
image tag is the git tag, unaltered: `ghcr.io/<owner>/cellad:v0.2.0` is
built from `v0.2.0` and from no other commit. `cellad version` prints
the same identity as `cellad -version`, so a Job that is one container
and one argument can read it.

A `v*` tag needs its CHANGELOG section, which the pre-push hook refuses
without and which `publish` reads with the gate's own reader. The tag is
cut with `go tool lateregate release vX.Y.Z`.

## Not in this spec

The stubs and the tiers ([[012-test-stubs-and-tiers]]); the suite the
conformance job will run ([[015-conformance-suite]]); the alert rules
themselves ([[017-observability]]); `docs/upgrades/` and the backup and
restore rows of [[014-release-and-installation]], which belong with a
second release to roll back to.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `Dockerfile` and `Dockerfile.ci` share the runtime stage byte for byte, and the release file has no build stage | `TestRuntimeStagesMatch` | open |
| No workflow, manifest, document or script names a fixed image namespace, and the workflow derives one from the repository owner | `TestReleasePublishesUnderTheOwnersNamespace` | open |
| Every overlay renders and the base pins no namespace and no registry | `TestOverlaysRender` | open |
| The base carries every Pod security field and the Role holds exactly the driver's verb table | `TestBaseIsConfined`, `TestRoleMatchesTheDriversVerbs` | open |
| Nothing under `deploy/`, `docs/`, the workflows or `internal/config`'s defaults names a Latere host, image, pool or namespace | `TestNoLatereCoordinatesInReleasedArtifacts` | open |
| `cellad check` fails on each mandatory requirement removed one at a time, an authorizer that allows the probe included | `TestCheckNamesEachFailure`, `TestCheckFailsAnAuthorizerThatAllowsTheProbe` | open |
| Each optional dependency reports not configured when its variable is unset, and answers when it is set | `TestCheckSkipsUnconfigured`, `TestCheckReachesEachOptionalDependency` | open |
| `cellad check` and `cellad version` are subcommands, and an unknown one is still exit 2 | `TestCheckSubcommand`, `TestVersionSubcommand`, `TestUnknownSubcommandIsAUsageError` | open |
| A tag's release carries every artifact of the table and verifies from a clean runner | the `release-verify` job | open |

## Outcome
