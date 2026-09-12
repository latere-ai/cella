---
title: "Manifest contract: the cella.latere.ai/v1 Sandbox, decoding, validation, defaulting, resolve, the boundary check"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
affects: [manifest/, manifest/v1/, docs/]
effort: large
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Manifest contract

## Overview

The manifest is the contract. A caller describes the environment it
wants in one document, `apiVersion: cella.latere.ai/v1`, `kind:
Sandbox`, and every surface of the system, the API, the `cella`
command, and a platform importing the packages, hands that document to
one function, `Resolve`, and gets back the fully defaulted, validated
form the data plane is asked for. This spec fixes the `Sandbox` schema,
the decoding rules shared by every kind, the validation rules, the
defaulting order, the mutability of each field after creation, the
boundary check a spawned child is held to, the error codes, and the
rules under which the schema may change. The other kinds, `Secret`,
`Volume`, `SandboxSet`, and `Environment`, are fixed in their own specs
and follow the same rules.

The philosophy is Kubernetes's: a typed object with `metadata`, `spec`,
and `status`; the caller writes `spec`, the server writes `status`;
unknown fields are refused rather than dropped; defaults are applied
server-side and returned, so what a caller reads back is what runs. The
one departure is deliberate: a `Sandbox` declares a boundary, and
nothing that happens inside it later can widen that boundary.

## Current state

Not built. The schema descends from a manifest that has served a
hosted platform for months, with these changes: the platform's own
services are no longer fields; volumes, secrets, an egress section with
rules, ports, mesh and spawn rights, scheduling, and an environment
selector are; and `status` is part of the contract.

## Design

### The object

```yaml
apiVersion: cella.latere.ai/v1
kind: Sandbox
metadata:
  name: dev                       # optional; generated when absent
  labels:
    team: research
  annotations:
    example.com/ticket: "1234"
spec:
  environment: default            # a registered Environment; the default when absent
  image: ghcr.io/example/sandbox:1.4   # required
  command: ["/bin/bash", "-l"]         # optional; the image's entrypoint when absent
  args: []
  workdir: /workspace                  # default: workspace.path
  user: "1000"                         # optional; the image's user when absent
  tier: ephemeral                      # ephemeral | persistent
  resources:
    cpu: "1"                           # Kubernetes quantity syntax
    memory: 2Gi
    disk: 10Gi                         # the workspace volume
  workspace:
    path: /workspace
    source: git                        # empty | git | volume
    git:
      url: https://github.com/example/repo.git
      ref: main
      secret: github-token             # a Secret whose scope covers the clone host
  volumes:                             # additional mounts (019)
    - name: state
      path: /data
      volume: app-state                # a Volume object; attached read-write
    - name: tools
      path: /opt/tools
      volume: analysis-tools
      readOnly: true
  env:
    LOG_LEVEL: debug                   # values; never a secret
  secrets:                             # placeholders in env, values at the gateway (018)
    - name: github-token
      env: GITHUB_TOKEN
    - name: openai
      env: OPENAI_API_KEY
  network:
    egress:
      mode: allowlist                  # open | allowlist | none
      allowedHosts: ["pypi.org", "*.pythonhosted.org"]   # plus every mounted secret's hosts
    ports:                             # what may be reached inside (023)
      - name: web
        port: 8080
        expose: mesh                   # none | mesh | public
  mesh:                                # peers and spawn rights (022)
    enabled: true
    spawn:
      budget: 4                        # children this sandbox may create in total
      depth: 2                         # generations below this one
  scheduling:                          # when and where (020)
    strategy: immediate                # immediate | pooled | queued
    priority: 0
    queue: ""
    startDeadline: 10m
    preemptible: false
  lifecycle:
    autoStop: 15m                      # idle this long: stopped
    ttl: 24h                           # this old: deleted; or `never`
    deadline: 2026-10-01T00:00:00Z     # absolute alternative to ttl
    autoDelete: 72h                    # stopped this long: deleted
  display:
    width: 1280                        # a GUI desktop; capability-gated (023)
    height: 800
  policy: restricted                   # a named policy the admission step resolves
status:                                # written by the server, ignored on apply
  id: 01J9ZK2P7Q8R9S0T1U2V3W4X5Y
  phase: Running
  owner: alice@example.com
  environment: default
  driver: k8s
  parent: ""                           # the spawning sandbox's id, when spawned
  mesh: msh_01J9...                    # inherited or assigned
  spawn: {budget: 4, used: 1, depth: 2}
  conditions:
    - {type: Ready, status: "True", reason: PodRunning, since: 2026-09-12T10:00:00Z}
    - {type: WorkspaceReady, status: "True"}
    - {type: EgressEnforced, status: "True"}
  secrets:
    mounted: [github-token, openai]
    notInjectable: []                  # placeholders whose secret has no host in scope
  volumes:
    - {name: state, volume: app-state, attached: true}
  ports:
    - {name: web, port: 8080, url: "https://web-01j9.sandboxes.example.com"}
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
| `labels` | map | Kubernetes label syntax for keys and values; keys under `cella.latere.ai/` are refused with `reserved_prefix` |
| `annotations` | map | keys as labels, values any string up to 4 KiB; total at most 64 KiB; `cella.latere.ai/` refused |

`spec`

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `environment` | string | the default environment | no | the name of a registered `Environment`; unknown is `not_found` at resolve; the authorizer may narrow which environments a subject may name |
| `image` | string | none, required | no | an OCI reference: registry optional, repository, tag or digest optional; the driver resolves `latest`; on a `native` environment, a local root file system or a command |
| `command`, `args` | []string | the image's | no | `args` without `command` appends to the image's entrypoint |
| `workdir` | string | `workspace.path` | no | absolute path |
| `user` | string | the image's | no | a uid, `uid:gid`, or a name |
| `tier` | enum | `ephemeral` | ephemeral to persistent only | `persistent` keeps the workspace across stop and start; `ephemeral` loses it at stop |
| `resources.cpu`, `.memory`, `.disk` | quantity | the configured defaults | with the `Resize` capability | Kubernetes quantity syntax, parsed by the package's own parser; `disk` sizes the workspace volume |
| `workspace.path` | string | `/workspace` | no | absolute; may not collide with a `volumes[].path` |
| `workspace.source` | enum | `empty` | no | `empty`, `git`, or `volume` |
| `workspace.git.url`, `.ref` | string | none; url required for `git` | no | `https://` or `ssh://`; a branch, tag, or commit |
| `workspace.git.secret` | string | none | no | a `Secret` name whose scope covers the clone host, so the clone authenticates at the gateway; a secret without that host is `secret_out_of_scope` |
| `workspace.volume` | string | none; required for `volume` | no | a `Volume` name mounted at `workspace.path` read-write; `tier` must be `persistent` |
| `volumes[]` | list | empty | add and remove with the `Volumes` capability while `Stopped` | each `{name, path, volume, readOnly}`; `path` absolute, unique, not under `/run/cella`; `volume` a `Volume` the caller may attach; a read-write attach of a volume already attached read-write elsewhere is `volume_busy` ([[019-volumes]]) |
| `env` | map | empty | no | keys POSIX names; values up to 32 KiB total; keys under `CELLA_`, the proxy keys `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase forms, and the trust-store keys the gateway sets are refused with `reserved_prefix` |
| `secrets[]` | list | empty | add and remove | each `{name, env}`; `name` a `Secret` the caller may mount; `env` a key not in `env` and not reserved; two entries naming secrets that scope the same host are `secret_host_conflict` ([[018-egress-and-secrets]]) |
| `network.egress.mode` | enum | `allowlist` when any host is set or any secret is mounted, else `open` | yes, narrowing only after create | `none` blocks egress; `allowlist` admits the listed hosts, every mounted secret's hosts, and DNS; `open` admits everything through the gateway with substitution still scoped |
| `network.egress.allowedHosts` | []string | empty | narrowing only | exact hostnames or `*.` wildcards; with `mode: open` is `exclusive_fields` |
| `network.ports[]` | list | empty | no | each `{name, port, expose}`; `expose` is `none` (default; reachable only by the operations of [[023-computer-use-operations]]), `mesh` (reachable by mesh peers), or `public` (an endpoint in `status.ports[].url`; needs the `Ingress` capability) |
| `mesh.enabled` | bool | `false` | no | joins a mesh: peers reach each other's `mesh` ports; a spawned child inherits the parent's mesh and may not set this field |
| `mesh.spawn.budget`, `.depth` | int | `0`, `0` | narrowing only | children this sandbox may create in total and generations below it; a child's values are at most the parent's remaining budget and depth minus one ([[022-mesh-and-spawn]]) |
| `scheduling.strategy` | enum | the environment's default | no | `immediate`, `pooled`, or `queued` ([[020-scheduling-and-sets]]) |
| `scheduling.priority` | int | `0` | no | higher runs first in a queue; the authorizer's `limits.max_priority` caps it |
| `scheduling.queue` | string | the environment's default queue | no | a queue the environment declares |
| `scheduling.startDeadline` | duration | none | no | queued this long without starting: `Failed` with reason `StartDeadline` |
| `scheduling.preemptible` | bool | `false` | no | may be stopped to make room for a higher priority |
| `lifecycle.autoStop` | duration | the configured default | yes | Go syntax, positive; `never` disables; must not exceed `ttl` |
| `lifecycle.ttl` | duration or `never` | the configured default | yes | from `createdAt`; exclusive with `deadline` |
| `lifecycle.deadline` | RFC 3339 | none | yes | absolute; in the future at apply; exclusive with `ttl` |
| `lifecycle.autoDelete` | duration | the configured default | yes | from `stoppedAt`; `never` disables |
| `display.width`, `.height` | int | none | no | both or neither; 320 to 7680 and 240 to 4320; refused with `capability_unsupported` on an environment without `Display` |
| `policy` | string | none | no | a name the admission step resolves; unknown names are `admission_refused` |

`status` (server-written, every field read-only)

| Field | Meaning |
|---|---|
| `id` | the stable identifier, a ULID, the key of every `/v1/sandboxes/{id}` path; `name` may be reused after delete, `id` never |
| `phase` | `Pending`, `Queued`, `Starting`, `Running`, `Stopping`, `Stopped`, `Recovering`, `Deleting`, `Failed`, `Lost` ([[005-lifecycle-controller]]) |
| `owner` | the subject that applied the manifest; for a spawned child, the root's owner |
| `environment`, `driver` | where it runs and what runs it |
| `parent`, `mesh`, `spawn` | the spawn tree position, the inherited mesh, the budget and what is used |
| `conditions` | `Ready`, `WorkspaceReady`, `EgressEnforced`, `VolumesAttached`, `Scheduled`, each with `status`, `reason`, `message`, `since` |
| `secrets.mounted`, `.notInjectable` | which placeholders are in `env`, and which will leave the sandbox inert because their secret scopes no host |
| `volumes[]`, `ports[]` | attachment state; the public URLs |
| `createdAt`, `startedAt`, `stoppedAt`, `lastActivityAt`, `expiresAt` | RFC 3339; `expiresAt` is the earlier of the TTL and the deadline |
| `warnings` | sentences in the user register about what the environment could not honour |

A manifest that carries `status` on apply is accepted and the field
ignored, so a caller may `GET`, edit, and `PUT` without stripping it.

### Decoding

`Decode(body []byte, contentType string) (Object, error)`, shared by
every kind.

- Content types: `application/json`; `application/yaml`,
  `application/x-yaml`, `text/yaml`. Anything else is
  `unsupported_media_type`. A body that begins with `{` under a YAML
  type is decoded as JSON.
- YAML is one document. A second document is `multi_document`.
- Unknown fields anywhere are `unknown_field` with the path.
- `apiVersion` other than `cella.latere.ai/v1` is `unsupported_version`;
  an unknown `kind` is `unsupported_kind`. Both are checked before
  anything else, so a caller learns the version problem first.
- Durations are Go syntax or `never`; quantities are the Kubernetes
  syntax, parsed by the package's own parser for the decimal and binary
  SI subset, so an importer does not pull the Kubernetes API machinery
  to validate a manifest.
- The body limit is the API's (`CELLA_MAX_BODY_BYTES`); the package
  itself sets none. The YAML decoder refuses alias expansion beyond 1
  MiB and nesting beyond 64 levels.

### Resolve

```go
type Options struct {
	Defaults     Defaults               // the operator's defaults (007)
	Ceilings     Ceilings               // the operator's ceilings (007); zero is none
	Admit        AdmitFunc              // the admission step (007); nil is identity
	Existing     *v1.Sandbox            // the current object on update; nil on create
	Parent       *v1.Sandbox            // the spawning sandbox's resolved manifest; nil unless spawned
	Environment  v1.EnvironmentStatus   // capabilities and defaults of the chosen environment (021)
	Lookup       Lookup                 // Secret and Volume metadata by name, scoped to the caller (018, 019)
	Now          func() time.Time
}

func Resolve(ctx context.Context, in *v1.Sandbox, o Options) (*Resolved, error)

type Resolved struct {
	Sandbox  v1.Sandbox // every default applied; what the data plane is asked for
	Warnings []string   // what the environment cannot honour; copied to status.warnings
}
```

The stages, in order, each one total before the next begins:

1. Structural validation: the field rules above that need no defaults
   (syntax, enums, ranges, reserved prefixes, exclusive pairs, path
   collisions).
2. Defaulting: every absent field with a default is set from
   `Defaults` and from the environment's own defaults (strategy,
   queue). `network.egress.mode` is inferred. `workdir` from
   `workspace.path`. `metadata.name` is generated.
3. Admission: `Admit` receives the defaulted object and returns the
   object to continue with or an error. It may change any field,
   including ones the caller set; it may not change `apiVersion`,
   `kind`, `status`, or `metadata.name` on update. Its output goes
   through stage 1 again.
4. Reference resolution: every `Secret` and `Volume` named is looked up
   through `Lookup`, which answers only what the caller may mount or
   attach, so an object the caller may not see is `not_found`. Each
   mounted secret's hosts join the allow list. Two secrets scoping one
   host is `secret_host_conflict`. A `workspace.git.secret` that scopes
   no host of the clone URL is `secret_out_of_scope`.
5. Semantic validation: ceilings (`ceiling_exceeded`), immutability
   against `Existing` (`immutable_field` naming every path that
   changed), narrowing rules on the mutable boundary fields
   (`boundary_widened`), the tier rules, `deadline` in the future,
   `autoStop` not above `ttl`.
6. Boundary check, when `Parent` is set: the child's egress mode is at
   least as strict, its allowed hosts and secrets are subsets of the
   parent's, its volumes are among the parent's with no read-only
   attachment made read-write, its spawn budget and depth fit the
   parent's remainder, its resources do not exceed the parent's, its
   `ttl` does not outlive the parent's, and `mesh.enabled` is not set
   because it is inherited. Any violation is `boundary_exceeded` with
   every offending path.
7. Capability check against `Environment`: a field the environment
   cannot enforce is either a refusal (`display` without `Display`,
   `expose: public` without `Ingress`) or a warning (`egress` on an
   environment without `Egress`), as the field table says.

`Resolve` is deterministic: the same input, options, and `Now` produce
byte-identical output, which is what the conformance case of
[[001-architecture]] compares across surfaces.

### Errors

One type, `*manifest.Error{Code, Path, Message}`, where `Path` is the
JSON path of the field and `Message` is one sentence in the user
register. The API maps it to 400, 404, 409, or 422 ([[008-api]]).

| Code | When |
|---|---|
| `unsupported_media_type` | the content type is not JSON or YAML |
| `multi_document` | more than one YAML document |
| `unsupported_version` | `apiVersion` is not `cella.latere.ai/v1` |
| `unsupported_kind` | `kind` is not one the server serves |
| `unknown_field` | a field the schema does not have |
| `missing_field` | a required field is absent |
| `invalid_field` | a value fails its syntax, enum, or range rule |
| `reserved_prefix` | a label, annotation, env key, or path under a reserved prefix |
| `exclusive_fields` | `ttl` with `deadline`, or `allowedHosts` with `mode: open` |
| `path_conflict` | two mounts at one path, or a mount under another |
| `not_found` | a named `Environment`, `Secret`, or `Volume` the caller cannot see |
| `secret_host_conflict` | two mounted secrets scope one host |
| `secret_out_of_scope` | a secret named for a purpose whose host it does not scope |
| `volume_busy` | a read-write attach of a volume attached read-write elsewhere |
| `immutable_field` | an update changes a field the table marks immutable |
| `boundary_widened` | an update widens a boundary field that may only narrow |
| `boundary_exceeded` | a spawned child exceeds its parent's boundary |
| `ceiling_exceeded` | a resolved value exceeds an operator ceiling |
| `capability_unsupported` | a field the environment cannot serve and cannot ignore |
| `admission_refused` | the admission step refused, with its reason |
| `admission_unavailable` | the admission step could not be reached |

### Schema evolution

- Within `cella.latere.ai/v1`, a change adds an optional field with a
  default that preserves the previous behaviour, or adds an enum value.
  A field never changes type or meaning, and is never removed.
- A manifest accepted by one `v1` build is accepted by every later
  `v1` build and resolves to the same object, defaults aside.
- A change that cannot meet those rules is `cella.latere.ai/v2`, with a
  conversion in both directions and a period where both are served.
- Until the first tagged release, the schema may still change; the
  CHANGELOG names every field change.
- The OpenAPI description of every kind is generated from the Go types
  ([[008-api]]) and a drift between the two fails the gate.

### Package layout

`manifest/v1` holds the types of every kind and nothing that imports
anything but the standard library. `manifest` holds `Decode`,
`Resolve`, the quantity and duration parsers, the name generator, the
boundary check, and the error type, and imports `manifest/v1`,
`runtime` (for capabilities), and `latere.ai/x/pkg/hostmatch`. Neither
imports `internal/`.

## Not in this spec

The `Secret`, `Volume`, `SandboxSet`, and `Environment` kinds
([[018-egress-and-secrets]], [[019-volumes]],
[[020-scheduling-and-sets]], [[021-data-plane-workers]]); where defaults
and ceilings come from ([[007-admission]]); how a resolved manifest
becomes a driver call ([[005-lifecycle-controller]]); the HTTP mapping
of the errors ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The example above decodes from YAML and from its JSON form to equal objects | `TestDecodeYAMLAndJSONAgree` | not built |
| Every unknown field, at any depth, is refused with its path | `TestUnknownFieldNamesThePath`, table-driven over twenty paths | not built |
| A second YAML document, a wrong version, and a wrong kind are refused with their codes, version before kind | `TestDecodeRefusals` | not built |
| Every default in the table is applied and returned; a field the caller set is never overwritten by a default | `TestDefaultsFillOnlyAbsentFields` | not built |
| An admission function's output is validated again | `TestAdmissionOutputIsValidated` | not built |
| Every mounted secret's hosts join the allow list; two secrets on one host are `secret_host_conflict`; a clone secret without the clone host is `secret_out_of_scope` | `TestSecretReferences` | not built |
| A reserved env key, a proxy key, and a trust-store key are each `reserved_prefix` | `TestReservedEnvKeys` | not built |
| Every immutable field changed on update is named in one `immutable_field` error; a widened allow list is `boundary_widened` | `TestImmutableAndNarrowingFields` | not built |
| Each of the eight boundary rules, violated one at a time against a parent, is `boundary_exceeded` naming the path; a conforming child passes | `TestBoundaryCheck`, table-driven | not built |
| Ceilings refuse with the field and the ceiling; a ceiling of zero is no ceiling | `TestCeilings` | not built |
| `display` without `Display` is refused; egress without `Egress` is a warning; `expose: public` without `Ingress` is refused | `TestCapabilityRefusalsAndWarnings` | not built |
| `Resolve` on the same input twice yields byte-identical JSON | `TestResolveIsDeterministic` | not built |
| The quantity parser agrees with the Kubernetes parser on a fuzz corpus | `FuzzQuantity` against `resource.ParseQuantity` in a test-only dependency | not built |
| A YAML body with an alias chain expanding past 1 MiB is refused in under 100 ms | `TestYAMLBombIsRefused` | not built |
| `manifest/v1` imports only the standard library; `manifest` imports no `internal/` package | `TestManifestImports` | not built |
