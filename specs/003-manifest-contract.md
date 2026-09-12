---
title: "Manifest contract: the cella/v1 Sandbox schema, decoding, validation, defaulting, resolve"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
affects: [manifest/, manifest/v1/, docs/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Manifest contract

## Overview

The manifest is the contract. A caller describes the environment it
wants in one document, `apiVersion: cella/v1`, `kind: Sandbox`, and
every surface of the system, the API, the `cella` command, and a
platform importing the packages, hands that document to one function,
`Resolve`, and gets back the fully defaulted, validated form the
backend is asked for. This spec fixes the schema, the decoding rules,
the validation rules, the defaulting order, the mutability of each
field after creation, the error codes, and the rules under which the
schema may change.

The philosophy is Kubernetes's: a typed object with `metadata`, `spec`,
and `status`; the caller writes `spec`, the server writes `status`;
unknown fields are refused rather than dropped; defaults are applied
server-side and returned, so what a caller reads back is what runs.

## Current state

Not built. The schema below descends from a manifest that has served
a hosted platform for months, with three changes: the group is
`cella/v1`, the fields that named that platform's own services are
gone, and `status` is part of the contract. The sandbox that platform
creates today is one this schema still describes.

## Design

### The object

```yaml
apiVersion: cella/v1
kind: Sandbox
metadata:
  name: dev                       # optional; generated when absent
  labels:
    team: research
  annotations:
    example.com/ticket: "1234"
spec:
  image: ghcr.io/example/sandbox:1.4   # required
  command: ["/bin/bash", "-l"]         # optional; the image's entrypoint when absent
  args: []
  workdir: /workspace                  # default: workspace.path
  user: "1000"                         # optional; the image's user when absent
  tier: ephemeral                      # ephemeral | persistent
  resources:
    cpu: "1"                           # Kubernetes quantity syntax
    memory: 2Gi
    disk: 10Gi
  workspace:
    path: /workspace
    source: git                        # empty | git
    git:
      url: https://github.com/example/repo.git
      ref: main
      tokenEnv: GIT_TOKEN              # the env key whose value authenticates the clone
  env:
    GIT_TOKEN: ghp_...                 # values; a plane brokers secrets into them
  network:
    mode: allowlist                    # open | allowlist | none
    allowedHosts: ["*.github.com", "pypi.org"]
  lifecycle:
    autoStop: 15m                      # idle this long: stopped
    ttl: 24h                           # this old: deleted; or `never`
    deadline: 2026-10-01T00:00:00Z     # absolute alternative to ttl
    autoDelete: 72h                    # stopped this long: deleted
  display:
    width: 1280                        # optional GUI desktop; capability-gated
    height: 800
  policy: restricted                   # optional named policy the operator defines
status:                                # written by the server, ignored on apply
  id: 01J9ZK2P7Q8R9S0T1U2V3W4X5Y
  phase: Running
  owner: alice@example.com
  backend: k8s
  conditions:
    - type: Ready
      status: "True"
      reason: PodRunning
      since: 2026-09-12T10:00:00Z
  createdAt: 2026-09-12T10:00:00Z
  startedAt: 2026-09-12T10:00:02Z
  lastActivityAt: 2026-09-12T10:41:00Z
  expiresAt: 2026-09-13T10:00:00Z
  warnings: []
```

### Fields

`metadata`

| Field | Type | Rule |
|---|---|---|
| `name` | string | a DNS-1123 label, at most 63 characters; unique per owner; generated as `<adjective>-<noun>-<4 hex>` when absent |
| `labels` | map | Kubernetes label syntax for keys and values; keys under `cella/` are refused with `reserved_prefix` |
| `annotations` | map | keys as labels, values any string up to 4 KiB; total at most 64 KiB; `cella/` refused |

`spec`

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `image` | string | none, required | no | an OCI reference: registry optional, repository, tag or digest optional; the backend resolves `latest` |
| `command`, `args` | []string | the image's | no | `args` without `command` appends to the image's entrypoint |
| `workdir` | string | `workspace.path` | no | absolute path |
| `user` | string | the image's | no | a uid, `uid:gid`, or a name |
| `tier` | enum | `ephemeral` | ephemeral to persistent only | `persistent` keeps the workspace across stop and start; `ephemeral` loses it at stop |
| `resources.cpu`, `.memory`, `.disk` | quantity | the configured defaults | with the `Resize` capability | Kubernetes quantity syntax: decimal (`500m`, `2`) and binary SI (`2Gi`); disk applies to the workspace volume |
| `workspace.path` | string | `/workspace` | no | absolute path the workspace volume mounts at |
| `workspace.source` | enum | `empty` | no | `empty` or `git` |
| `workspace.git.url` | string | none; required when source is `git` | no | an `https://` or `ssh://` URL |
| `workspace.git.ref` | string | the remote's default | no | a branch, tag, or commit |
| `workspace.git.tokenEnv` | string | none | no | the key in `env` whose value the clone authenticates with; the value never appears in `status` or in an event |
| `env` | map | empty | no | keys are POSIX names; values up to 32 KiB total; keys under `CELLA_` are refused |
| `network.mode` | enum | `open`, or `allowlist` when `allowedHosts` is set | yes | `none` blocks egress; `allowlist` admits the listed hosts and DNS; `open` admits everything |
| `network.allowedHosts` | []string | empty | yes | exact hostnames or `*.` wildcards, matched with `latere.ai/x/pkg/hostmatch`; setting it with `mode: open` is `invalid_field` |
| `lifecycle.autoStop` | duration | the configured default | yes | Go syntax, positive; `never` disables; must not exceed `ttl` |
| `lifecycle.ttl` | duration or `never` | the configured default | yes | counted from `createdAt`; exclusive with `deadline` |
| `lifecycle.deadline` | RFC 3339 | none | yes | absolute; in the future at apply; exclusive with `ttl` |
| `lifecycle.autoDelete` | duration | the configured default | yes | counted from `stoppedAt`; `never` disables |
| `display.width`, `.height` | int | none | no | both or neither; 320 to 7680 and 240 to 4320; refused with `capability_unsupported` on a backend without `Display` |
| `policy` | string | none | no | a name the admission step resolves; unknown names are `admission_refused` |

`status` (server-written, every field read-only)

| Field | Meaning |
|---|---|
| `id` | the stable identifier, a ULID, the key of every `/v1/sandboxes/{id}` path; `name` may be reused after delete, `id` never |
| `phase` | `Pending`, `Starting`, `Running`, `Stopping`, `Stopped`, `Deleting`, `Failed`, `Lost` ([[005-lifecycle-controller]]) |
| `owner` | the subject that applied the manifest |
| `backend` | `k8s`, `podman`, or `native` |
| `conditions` | `Ready`, `WorkspaceReady`, `NetworkEnforced`, each with `status`, `reason`, `message`, `since` |
| `createdAt`, `startedAt`, `stoppedAt`, `lastActivityAt`, `expiresAt` | RFC 3339; `expiresAt` is the earlier of the TTL and the deadline |
| `warnings` | sentences in the user register about what the backend could not honour, such as `network.mode is not enforced by the native backend` |

A manifest that carries `status` on apply is accepted and the field
ignored, so a caller may `GET`, edit, and `PUT` without stripping it.

### Decoding

`Decode(body []byte, contentType string) (*v1.Sandbox, error)`.

- Content types: `application/json`; `application/yaml`,
  `application/x-yaml`, `text/yaml`. Anything else is
  `unsupported_media_type`. A body that begins with `{` under a YAML
  type is decoded as JSON, since JSON is YAML.
- YAML is one document. A second document is `multi_document`.
- Unknown fields anywhere are `unknown_field` with the path.
- `apiVersion` other than `cella/v1` is `unsupported_version`; `kind`
  other than `Sandbox` is `unsupported_kind`. Both are checked before
  anything else, so a caller learns the version problem first.
- Durations are Go syntax or `never`; quantities are the Kubernetes
  syntax, parsed by the package's own parser for the decimal and binary
  SI subset, so an importer does not pull the Kubernetes API machinery
  to validate a manifest.
- The body limit is the API's (`CELLA_MAX_BODY_BYTES`); the package
  itself sets none.

### Resolve

```go
type Options struct {
	Defaults     Defaults          // the operator's defaults (007)
	Ceilings     Ceilings          // the operator's ceilings (007); zero is none
	Admit        AdmitFunc         // the admission step (007); nil is identity
	Existing     *v1.Sandbox       // the current object on update; nil on create
	Capabilities runtime.Capabilities // what the backend can enforce (004)
	Now          func() time.Time
}

func Resolve(ctx context.Context, in *v1.Sandbox, o Options) (*Resolved, error)

type Resolved struct {
	Sandbox  v1.Sandbox // every default applied; what the backend is asked for
	Warnings []string   // what the backend cannot honour; copied to status.warnings
}
```

The stages, in order, each one total before the next begins:

1. Structural validation: the field rules above that need no defaults
   (syntax, enums, ranges, reserved prefixes, exclusive pairs).
2. Defaulting: every absent field with a default is set from
   `Defaults`. `network.mode` is inferred from `allowedHosts`.
   `workdir` from `workspace.path`. `metadata.name` is generated.
3. Admission: `Admit` receives the defaulted object and returns the
   object to continue with or an error. It may change any field,
   including ones the caller set; it may not change `apiVersion`,
   `kind`, or `status`. Its output goes through stage 1 again, so a
   webhook cannot produce an object the schema refuses.
4. Semantic validation: ceilings (`ceiling_exceeded` with the field and
   the ceiling), immutability against `Existing` (`immutable_field`
   with every path that changed), the tier rule, `deadline` in the
   future, `autoStop` not above `ttl`.
5. Capability check: a field the backend cannot enforce is either a
   refusal (`display` without `Display`) or a warning
   (`network.mode` on `native`), as the table above says per field.

`Resolve` is deterministic: the same input, options, and `Now` produce
byte-identical output, which is what the conformance case of
[[001-architecture]] compares across surfaces.

### Errors

One type, `*manifest.Error{Code, Path, Message}`, where `Path` is the
JSON path of the field (`spec.lifecycle.ttl`) and `Message` is one
sentence in the user register. The API maps it to 400, 409, or 422
([[008-api]]).

| Code | When |
|---|---|
| `unsupported_media_type` | the content type is not JSON or YAML |
| `multi_document` | more than one YAML document |
| `unsupported_version` | `apiVersion` is not `cella/v1` |
| `unsupported_kind` | `kind` is not `Sandbox` |
| `unknown_field` | a field the schema does not have |
| `missing_field` | a required field is absent |
| `invalid_field` | a value fails its syntax, enum, or range rule |
| `reserved_prefix` | a label, annotation, or env key under a reserved prefix |
| `exclusive_fields` | `ttl` with `deadline`, or `allowedHosts` with `mode: open` |
| `immutable_field` | an update changes a field the table marks immutable |
| `ceiling_exceeded` | a resolved value exceeds an operator ceiling |
| `capability_unsupported` | a field the backend cannot serve and cannot ignore |
| `admission_refused` | the admission step refused, with its reason |
| `admission_unavailable` | the admission step could not be reached |

### Schema evolution

- Within `cella/v1`, a change adds an optional field with a default
  that preserves the previous behaviour, or adds an enum value. A
  field never changes type or meaning, and is never removed.
- A manifest accepted by one `v1` build is accepted by every later
  `v1` build and resolves to the same object, defaults aside.
- A change that cannot meet those rules is `cella/v2`, with a
  conversion in both directions and a period where both are served.
- Until the first tagged release, the schema may still change; the
  CHANGELOG names every field change.
- The OpenAPI description of the schema is generated from the Go types
  ([[008-api]]) and a drift between the two fails the gate.

### Package layout

`manifest/v1` holds the types and nothing that imports anything but the
standard library. `manifest` holds `Decode`, `Resolve`, the quantity
and duration parsers, the name generator, and the error type, and
imports `manifest/v1`, `runtime` (for `Capabilities`), and
`latere.ai/x/pkg/hostmatch`. Neither imports `internal/`.

## Not in this spec

Where defaults and ceilings come from and what the admission webhook
receives ([[007-admission]]); how a resolved manifest becomes a
`runtime.CreateSpec` ([[005-lifecycle-controller]]); the HTTP
mapping of the errors ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The example above decodes from YAML and from its JSON form to equal objects | `TestDecodeYAMLAndJSONAgree` | not built |
| Every unknown field, at any depth, is refused with its path | `TestUnknownFieldNamesThePath`, table-driven over ten paths | not built |
| A second YAML document, a wrong version, and a wrong kind are refused with their codes, version before kind | `TestDecodeRefusals` | not built |
| Every default in the table is applied and returned; a field the caller set is never overwritten by a default | `TestDefaultsFillOnlyAbsentFields` | not built |
| An admission function's output is validated again; one that returns an unknown field is `invalid_field`, not a crash | `TestAdmissionOutputIsValidated` | not built |
| Every immutable field changed on update is named in one `immutable_field` error | `TestImmutableFieldsAreNamedTogether` | not built |
| Ceilings refuse with the field and the ceiling; a ceiling of zero is no ceiling | `TestCeilings` | not built |
| `display` on a backend without `Display` is refused; `network.mode` on one without `Network` is a warning | `TestCapabilityRefusalsAndWarnings` | not built |
| `Resolve` on the same input twice yields byte-identical JSON | `TestResolveIsDeterministic` | not built |
| The quantity parser agrees with the Kubernetes parser on a fuzz corpus of the decimal and binary SI subset | `FuzzQuantity` against `resource.ParseQuantity` in a test-only dependency | not built |
| `manifest/v1` imports only the standard library; `manifest` imports no `internal/` package | `TestManifestImports` | not built |
