---
title: "Manifest contract: the Sandbox kind, decoding, validation, defaulting, resolve, the boundary check"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
affects: [manifest/, manifest/v1/, docs/]
effort: large
created: 2026-09-12
updated: 2026-09-21
author: changkun
---

# Manifest contract

## Overview

The manifest is the contract. A caller describes the environment it
wants in one document, `apiVersion: cella.latere.ai/v1beta1`, `kind:
Sandbox`, and every surface of the system, the API, the `cella`
command, and a platform importing the packages, hands that document to
one function, `Resolve`, and gets back the fully defaulted, validated
form the data plane is asked for. This spec fixes the `Sandbox` schema,
the decoding rules shared by every kind, the validation rules, the
defaulting order, the mutability of each field after creation and for
whom, the boundary check a spawned child is held to, the error codes,
and the rules under which the schema may change. The other kinds,
`Secret`, `Volume`, `SandboxSet`, and `Environment`, are fixed in their
own specs and follow the same rules.

The philosophy is Kubernetes's: a typed object with `metadata`, `spec`,
and `status`; the caller writes `spec`, the server writes `status`;
unknown fields are refused rather than dropped; defaults are applied
server-side and returned, so what a caller reads back is what runs. The
one departure is deliberate: a `Sandbox` declares a boundary, and
nothing a workload does inside it later can widen that boundary.

## Current state

A strict JSON Sandbox subset is implemented by [[026-direct-control-plane]]: metadata, the configured environment, and native execution fields. [[044-manifest-fields]] added `user`, `resources`, `workspace.path`, `lifecycle`, the full `metadata` and `env` rules, `status.expiresAt` and `status.warnings`, the quantity and duration parsers, and the staged `Resolve` with `Defaults`, `Ceilings`, `Limits`, `Admit`, `Existing` and `NewName`, over a `Lookup` that answers `Environment`. [[039-egress-gateway]] added `network.egress` with the host rule, the mode inference, the `narrow` rule a workload cannot widen, `status.conditions`, and the egress row of the capability check. [[046-secret-kind]] added `secrets[]` with its environment-key rules, `status.secrets`, `Lookup.Secret` and reference resolution, and the `Secret` kind's own decode, defaults and validation. [[047-admission-client]] added the image rule that closes stage 3, `Defaults.Image`, `Actor.Issuer` and `.Sub`, and the `Claims`, `Workload` and `RequestID` an admission step reads. [[040-mesh-and-spawn]] added `mesh.enabled` and `mesh.spawn`, `status.parent`, `.root`, `.mesh` and `.spawn`, `Options.Parent`, stage 6 over every rule with a field, the child's defaults for `ttl` and for a boundary it does not declare, and the `Mesh` row of stage 7. [[055-api-contract-gaps]] added the shared decoder: the three YAML types beside JSON, one document per request, the version before the kind before any field, the unknown-field path, and the alias and nesting limits. [[056-contract-evidence]] added the golden corpus under `manifest/testdata/v1/` and the quantity parser's differential fuzz. [[057-scheduling-queue]] added `spec.scheduling` with its four fields, the queue defaulted from the environment, the refusal of every field on a direct environment, and the priority ceiling. The volume fields, `workspace.git` and rule 4 remain to build.

Design provenance: The schema descends from a manifest that has served a
hosted platform for months, with these changes: the platform's own
services are no longer fields; volumes, secrets, an egress section with
rules, ports, mesh and spawn rights, scheduling, and an environment
selector are; and `status` is part of the contract.

## Design

### The object

```yaml
apiVersion: cella.latere.ai/v1beta1
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
      allowedHosts: ["pypi.org", "*.pythonhosted.org"]   # plus every mounted secret's hosts; deniedHosts with mode open
    ports:                             # what may be reached inside (023)
      - name: web
        port: 8080
        expose: mesh                   # none | mesh | public
  mesh:                                # peers and spawn rights (022)
    enabled: true
    spawn:
      budget: 4                        # children this sandbox may create in total
      depth: 2                         # generations below this one
  scheduling:                          # on a queued environment only (020)
    priority: 0
    queue: default
    startDeadline: 10m                 # queued only
    preemptible: false
  lifecycle:
    autoStop: 15m                      # idle this long: stopped
    ttl: 24h                           # this old: deleted; or `never`
    autoDelete: 72h                    # stopped this long: deleted
  display:
    width: 1280                        # a GUI desktop; capability-gated (023)
    height: 800
status:                                # written by the server, ignored on apply
  id: sbx_01J9ZK2P7Q8R9S0T1U2V3W4X5Y
  phase: Running
  owner: https://login.example.com|alice
  environment: default
  driver: k8s
  isolation: container
  parent: ""                           # the spawning sandbox's id, when spawned
  mesh: msh_01J9...                    # inherited or assigned
  spawn: {budget: 4, used: 1, depth: 2}
  conditions:
    - {type: Ready, status: "True", reason: PodRunning, since: 2026-09-12T10:00:00Z}
    - {type: WorkspaceReady, status: "True"}
    - {type: EgressEnforced, status: "True"}
    - {type: DisplayReady, status: "True"}
  secrets:
    mounted: [github-token, openai]
    notInjectable: []                  # placeholders that will leave the sandbox unauthenticated
  volumes:
    - {name: state, volume: app-state, attached: true}
  ports:
    - {name: web, port: 8080, state: listening, url: ""}
  createdAt: 2026-09-12T10:00:00Z
  startedAt: 2026-09-12T10:00:02Z
  lastActivityAt: 2026-09-12T10:41:00Z
  expiresAt: 2026-09-13T10:00:00Z
  warnings: []
```

The example is a manifest `Resolve` accepts on a queued environment
whose capabilities include `allowlist` egress, `Mesh`, `Volumes`, and
`Display`; the acceptance criteria hold it to that.

### Fields

`metadata`

| Field | Type | Mutable | Rule |
|---|---|---|---|
| `name` | string | no | a DNS-1123 label, at most 63 characters; unique per owner; generated by `Options.NewName` when absent, `<adjective>-<noun>-<4 hex>` from the API's generator |
| `labels` | map | yes | Kubernetes label syntax for keys and values; keys under `cella.latere.ai/` are `reserved_prefix` |
| `annotations` | map | yes | keys as labels, values any string up to 4 KiB; total at most 64 KiB; `cella.latere.ai/` refused |

`spec`. The mutability column names who may change a field after
create: `no`; `yes` (any caller the authorizer allows); `narrow` (any
caller may narrow, only a non-workload actor may widen); `stopped`
(while `Stopped`, on an environment with the named capability).

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `environment` | string | the environment `CELLA_DEFAULT_ENVIRONMENT` names ([[021-data-plane-workers]]) | no | the name of a registered `Environment` the caller may use; `Lookup.Environment` answers `not_found` otherwise |
| `image` | string | `Defaults.Image`, from `CELLA_DEFAULT_IMAGE` | no | an OCI reference: registry optional, repository, tag or digest optional; the driver resolves `latest`. Required after stage 3 where the environment runs images and refused where it runs none, which closes stage 3 |
| `command`, `args` | []string | the image's | no | `args` without `command` appends to the image's entrypoint |
| `workdir` | string | `workspace.path` | no | absolute path |
| `user` | string | the image's | no | a uid, `uid:gid`, or a name |
| `resources.cpu`, `.memory`, `.disk` | quantity | `Defaults.CPU`, `.Memory`, `.Disk` | yes, with `Resize` | Kubernetes quantity syntax, decimal (`500m`, `2`) and binary SI (`2Gi`), parsed by the package's own parser; `disk` sizes the workspace volume |
| `workspace.path` | string | `/workspace` | no | absolute; not under `/run/cella`; may not equal or nest with a `volumes[].path` (`path_conflict`) |
| `workspace.source` | enum | `empty` | no | `empty`, `git`, or `volume` |
| `workspace.git.url`, `.ref` | string | none; url required for `git` | no | `https://` or `ssh://`; a branch, tag, or commit |
| `workspace.git.secret` | string | none | no | a `Secret` name whose scope covers the clone URL's host; the URL must be `https://`, because the gateway substitutes into HTTP and an ssh clone carries none (`secret_out_of_scope` otherwise) |
| `workspace.volume` | string | none; required for `volume` | no | a `Volume` name attached read-write at `workspace.path` in place of the managed workspace, so the files outlive the sandbox ([[019-volumes]]) |
| `volumes[]` | list | empty | stopped, `Volumes` | each `{name, path, volume, readOnly}`; `name` a DNS-1123 label, unique; `path` absolute, unique, not equal to or nested with another mount or `/run/cella`; `volume` a `Volume` the caller may attach, by name among the caller's own or by `vol_` id, in the same environment, and `Available`; an attach its `access` excludes is `volume_busy` ([[019-volumes]]) |
| `env` | map | empty | no | keys POSIX names; values up to 32 KiB total; a key in the reserved set below is `reserved_prefix` |
| `secrets[]` | list | empty | narrow | each `{name, env}`; `name` a `Secret` the caller may mount, by name among the caller's own or by `sec_` id; `env` a POSIX name not in `env`, not reserved, not another entry's `env`, and not equal to any entry's `<env>_HEADER` or `<env>_QUERY`; two entries whose secrets scope one host are `secret_host_conflict` |
| `network.egress.mode` | enum | `allowlist` when any host is set or any secret is mounted, else `open` | narrow | `none` blocks egress; `allowlist` admits `allowedHosts` and every mounted secret's hosts through the gateway; `open` admits everything but `deniedHosts`; DNS and the control plane are admitted by the driver's rule in every mode. Order for narrowing: `open` > `allowlist` > `none`. The environment's `capabilities.egress` list must contain the mode |
| `network.egress.allowedHosts` | []string | empty | narrow | host patterns under the host rule below; with any mode but `allowlist` is `exclusive_fields`; narrowing removes entries |
| `network.egress.deniedHosts` | []string | empty | narrow | host patterns; with any mode but `open` is `exclusive_fields`; a denied host wins over a mounted secret's scope; narrowing adds entries |
| `network.ports[]` | list | empty | no | each `{name, port, expose}`; `name` a DNS-1123 label, unique; `port` 1 to 65535, unique; `expose` is `none` (default), `mesh` (needs `Mesh` and membership: `mesh.enabled` on a root, an inherited `status.mesh` on a child, else `invalid_field`), or `public` (needs `Ingress`) |
| `mesh.enabled` | bool | `false` | no | joins a mesh; needs `Mesh`; a spawned child inherits the parent's mesh and may not set this field |
| `mesh.spawn.budget`, `.depth` | int | `0`, `0` | narrow | children this sandbox may create in total and generations below it; does not require `mesh.enabled`; a child's values are at most the parent's remaining budget and depth minus one |
| `scheduling.priority` | int | `0` | no | higher runs first in a queue; above `Limits.MaxPriority` is `ceiling_exceeded` |
| `scheduling.queue` | string | the environment's `spec.scheduling.defaultQueue` | no | one of the environment's `spec.scheduling.queues`, else `invalid_field` |
| `scheduling.startDeadline` | duration | none | no | queued this long without starting: `Failed` with reason `StartDeadline` |
| `scheduling.preemptible` | bool | `false` | no | may be stopped to make room for a higher priority |
| `lifecycle.autoStop` | duration | `Defaults.AutoStop` | yes | Go syntax, positive, or `never`; when `ttl` is a duration, `autoStop` must not exceed it (`invalid_field`) |
| `lifecycle.ttl` | duration or `never` | `Defaults.TTL`, or for a spawned child the lesser of that and the time to `Parent.status.expiresAt` | yes | from `createdAt` |
| `lifecycle.autoDelete` | duration | `Defaults.AutoDelete` | yes | from `stoppedAt`; `never` disables |
| `display.width`, `.height` | int | none | no | both or neither; 320 to 7680 and 240 to 4320; needs `Display` |

The host rule, shared with `Secret.spec.scope.hosts`
([[018-egress-and-secrets]]) and with the gateway: a pattern is an
exact fully qualified name or one leading `*.` wildcard, matched by
`latere.ai/x/pkg/hostmatch`, where `*.example.com` matches any
sub-label of `example.com` at any depth and not `example.com` itself.
`manifest` normalizes a pattern by lowercasing, trimming a trailing
dot, and refusing a port, and passes the same normalize function to
`hostmatch.New` that the gateway does, so the resolver and the gateway
agree. Refused with `invalid_field`: an IP literal, a single-label
name such as `localhost`, and any name that resolves by syntax to a
loopback, link-local, or private range.

The reserved environment keys, refused with `reserved_prefix` in `env`
and as a `secrets[].env`: any key under `CELLA_`; `HTTP_PROXY`,
`HTTPS_PROXY`, `NO_PROXY` and their lowercase forms; the trust-store
keys `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`,
`GIT_SSL_CAINFO`, `CURL_CA_BUNDLE`. [[018-egress-and-secrets]] is the
owner of what the gateway sets; this list is its copy and a test in
both packages asserts they are equal.

`status` (server-written, every field read-only). `Resolve` returns
`spec` and `metadata` fully resolved and an empty `status` apart from
`warnings`; the controller writes the rest, and the API merges the
stored status into every response.

| Field | Meaning |
|---|---|
| `id` | the stable identifier, a `sbx_` prefixed ULID ([[001-architecture]]), the key of every `/v1/sandboxes/{id}` path; `name` may be reused after delete, `id` never |
| `phase` | `Pending`, `Queued`, `Starting`, `Running`, `Stopping`, `Stopped`, `Recovering`, `Deleting`, `Failed`, `Lost` ([[005-lifecycle-controller]]) |
| `owner` | the subject that applied the manifest; for a spawned child, the root's owner |
| `environment`, `driver`, `isolation` | where it runs, what runs it, and the isolation class ([[004-runtime-contract]]) |
| `parent`, `root`, `mesh`, `spawn` | the spawn tree position: the parent's id and the root's, or empty and the sandbox's own id for a root; the inherited or minted mesh; the budget and what is used ([[022-mesh-and-spawn]]) |
| `set` | `{name, index}` for a `SandboxSet` replica ([[020-scheduling-and-sets]]); absent otherwise |
| `conditions` | `Ready`, `WorkspaceReady`, `EgressEnforced`, `VolumesAttached`, `Scheduled`, `DisplayReady`, each with `status`, `reason`, `message`, `since` |
| `secrets.mounted`, `.notInjectable` | which placeholders are in `env`; and which will leave the sandbox as inert strings, so the request goes out unauthenticated, because the secret was deleted or its scope no longer has a host the sandbox may reach |
| `volumes[]` | `{name, volume, attached}` per mount |
| `ports[]` | `{name, port, state, url}`; `state` is `listening` or `closed` ([[023-computer-use-operations]]); `url` set for `expose: public` |
| `createdAt`, `startedAt`, `stoppedAt`, `lastActivityAt`, `expiresAt` | RFC 3339; `expiresAt` is `createdAt` plus `ttl`, written by the controller; absent for `never` |
| `warnings` | sentences in the user register about what the environment could not honour |

A manifest that carries `status` on apply is accepted and the field
ignored, so a caller may `GET`, edit, and `PUT` without stripping it.

### Decoding

`Decode(body []byte, contentType string) (v1.Object, error)`, shared by
every kind.

- Content types: `application/json`; `application/yaml`,
  `application/x-yaml`, `text/yaml`. Anything else is
  `unsupported_media_type`. A body that begins with `{` under a YAML
  type is decoded as JSON.
- YAML is one document. A second document is `multi_document`.
- Unknown fields anywhere are `unknown_field` with the path.
- `apiVersion` other than `cella.latere.ai/v1beta1` is `unsupported_version`;
  an unknown `kind` is `unsupported_kind`. Both are checked before
  anything else, so a caller learns the version problem first.
- Durations are Go syntax or `never`; quantities are the Kubernetes
  syntax, parsed by the package's own parser for the decimal and binary
  SI subset, so an importer does not pull the Kubernetes API machinery
  to validate a manifest.
- The body limit is the API's (`CELLA_MAX_BODY_BYTES`); the package
  itself sets none. The YAML decoder refuses alias expansion beyond
  1 MiB and nesting beyond 64 levels, each with `invalid_field`.

### Resolve

```go
// Actor is who is applying: the subject, and whether it is a sandbox
// acting through its workload token.
type Actor struct {
	Subject  string
	Workload bool
}

// Lookup answers the references a manifest names, scoped to the actor:
// it returns not_found for an object that does not exist and for one the
// authorizer refuses (secret.mount, volume.attach, environment.use), so
// existence does not leak, and authorizer_unavailable when it cannot
// decide. A Secret comes back without spec.value. The API constructs it
// per request; an importer constructs its own.
type Lookup interface {
	Environment(ctx context.Context, name string) (*v1.Environment, error)
	Secret(ctx context.Context, nameOrID string) (*v1.Secret, error)
	Volume(ctx context.Context, nameOrID string) (*v1.Volume, error)
}

// Defaults are the values an absent field takes; Ceilings the values a
// resolved field may not exceed, zero meaning none. Both come from the
// operator's configuration or the admission webhook (007).
type Defaults struct {
	CPU, Memory, Disk           Quantity
	AutoStop, TTL, AutoDelete   Duration
}
type Ceilings struct {
	CPU, Memory, Disk Quantity
	TTL               Duration
}

// Limits are what the authorizer granted this actor (006).
type Limits struct {
	MaxPriority int
}

type Options struct {
	Actor     Actor
	Lookup    Lookup
	Defaults  Defaults
	Ceilings  Ceilings
	Limits    Limits
	Admit     AdmitFunc        // the admission step (007); nil is identity
	Existing  *v1.Sandbox      // the current object on update; nil on create
	Parent    *v1.Sandbox      // the spawning sandbox, desired spec and current status; nil unless spawned
	Now       func() time.Time
	NewName   func() string    // the generator for an absent metadata.name
}

func Resolve(ctx context.Context, in *v1.Sandbox, o Options) (*Resolved, error)

type Resolved struct {
	Sandbox  v1.Sandbox // spec and metadata fully resolved; status empty but warnings
	Warnings []string
}
```

The stages, in order, each one total before the next begins:

1. Structural validation: the field rules above that need no defaults
   or lookups (syntax, enums, ranges, reserved names, exclusive pairs,
   path collisions, port uniqueness, the host rule).
2. Defaulting: every absent field with a default is set from
   `Defaults`, from the environment's `spec.scheduling.defaultQueue`,
   and for a child from `Parent`. `network.egress.mode` is
   inferred. `workdir` from `workspace.path`. `metadata.name` from
   `NewName` when absent.
3. Admission: `Admit` receives the defaulted object and returns the
   object to continue with or an error. It may change any field,
   including ones the caller set; it may not change `apiVersion`,
   `kind`, `status`, or `metadata.name` on update. Its output goes
   through stage 1 again. An error the step names a code on keeps that
   code: `admission_refused` is a policy refusal and
   `admission_unavailable` is no decision at all, and the two are never
   folded together ([[007-admission]]).
   Stage 3 closes with the image rule, which is why it is here and not
   at stage 1: `spec.image` is required where the environment runs images
   and refused where it runs none, and an image catalogue is exactly the
   admission step that supplies one ([[007-admission]]). A manifest that
   still names none after `Defaults.Image` and the admission step is
   `missing_field`; one that names one on an environment of the `none`
   isolation class is `capability_unsupported`.
4. Reference resolution, through `Lookup`: the environment, every
   `Secret`, every `Volume`. Each mounted secret's hosts join the allow
   list. Two secrets scoping one host is `secret_host_conflict`. A
   `workspace.git.secret` whose scope has no host of the clone URL, or
   a clone URL that is not `https://`, is `secret_out_of_scope`. A
   volume in another environment is `invalid_field`; an attach the
   volume's `access` excludes is `volume_busy` as of the lookup, and
   the controller's attach is authoritative if that changes between
   resolve and create, ending the sandbox `Failed VolumeBusy`
   ([[019-volumes]]). The secrets' inject modes fix
   which `<env>_HEADER` and `<env>_QUERY` companions exist, and those
   names are checked against `env` and every other `secrets[].env`.
5. Semantic validation: ceilings (`ceiling_exceeded` with the field and
   the ceiling; `scheduling.priority` against `Limits.MaxPriority`);
   immutability against `Existing` (`immutable_field` naming every path
   that changed); narrowing on the `narrow` fields when
   `Actor.Workload` is true (`boundary_widened` naming every path,
   where a wider allow list is one not contained in the old under the
   host rule, a looser mode is one earlier in the order, an added
   secret is one not in the old list, and a larger budget or depth is
   larger); `autoStop` against `ttl`.
6. Boundary check, when `Parent` is set, nine rules, any violation
   `boundary_exceeded` with every offending path: (1) `egress.mode` is
   at least as strict as the parent's; (2) `allowedHosts` is contained
   in the parent's effective allow list under the host rule; (3)
   `secrets[]` names only secrets the parent mounts; (4) `volumes[]`
   and `workspace.volume` name only volumes the parent mounts, with no
   read-only attachment of the parent's made read-write; (5)
   `resources` do not exceed the parent's; (6) `ttl` does not end
   after `Parent.status.expiresAt`; (7) the parent has `depth > 0`,
   `spawn.budget` is at most the parent's `budget - used - 1` and
   `spawn.depth` at most the parent's `depth - 1`, both zero when
   absent; (8) `environment` is the parent's; (9) `mesh.enabled` is
   unset, since the mesh is inherited. A set's replicas are checked
   against the set's template as `Parent`, reading the rules as
   [[020-scheduling-and-sets]] says. On an update of a sandbox with
   live descendants, the same rules run with the updated sandbox as
   their parent, and a descendant left outside is `boundary_exceeded`
   naming it ([[022-mesh-and-spawn]]).
7. Capability check against the environment's `status.capabilities`,
   one row per capability a manifest field depends on: `Display` and
   `Input` refuse `display`;
   `Ingress` refuses `expose: public`; `Mesh` refuses `mesh.enabled`
   and `expose: mesh`; `Volumes` refuses `volumes[]` and
   `workspace.source: volume`; `Resize` refuses a `resources` change on
   update; `capabilities.egress` refuses an `egress.mode` it does not
   list, and an empty list is a warning that `EgressEnforced` will be
   false; an environment whose `spec.scheduling.mode` is `direct`
   refuses every `scheduling` field. Refusals
   are `capability_unsupported`. `Attach`, `Dial`, `Snapshots`, `Files`,
   and `Detach` gate operations and routes ([[008-api]]), not manifest
   fields, and are not resolve concerns.

`Resolve` is deterministic: the same input, options, `Now`, `NewName`,
and `Lookup` answers produce byte-identical output, which is what
[[001-architecture]]'s `TestAPIAndImporterResolveAgree` compares across
surfaces.

Pool adoption ([[020-scheduling-and-sets]]) is a create in manifest
terms: the driver's `Update` that turns a pool entry into the caller's
sandbox carries fields this table marks immutable, because the object
never existed for the caller before.

### Errors

One type, `*manifest.Error{Code, Path, Message}`, where `Path` is the
JSON path of the field and `Message` is one sentence in the user
register. [[008-api]] owns the fixed sentence per code and the HTTP
status, and its `TestErrorTable` asserts every code here has one.

| Code | When |
|---|---|
| `unsupported_media_type` | the content type is not JSON or YAML |
| `multi_document` | more than one YAML document |
| `unsupported_version` | `apiVersion` is not `cella.latere.ai/v1beta1` |
| `unsupported_kind` | `kind` is not one the server serves |
| `unknown_field` | a field the schema does not have |
| `missing_field` | a required field is absent |
| `invalid_field` | a value fails its syntax, enum, range, or host rule; the YAML limits |
| `reserved_prefix` | a label, annotation, or env key under a reserved prefix or equal to a reserved name |
| `exclusive_fields` | `allowedHosts` without `mode: allowlist`; `deniedHosts` without `mode: open` |
| `path_conflict` | two mounts at one path, or a mount under another |
| `not_found` | a named `Environment`, `Secret`, or `Volume` the actor cannot see |
| `secret_host_conflict` | two mounted secrets scope one host |
| `secret_out_of_scope` | a secret named for a purpose whose host or scheme it does not serve |
| `volume_busy` | an attach a volume's `access` excludes, or a delete of an attached volume |
| `immutable_field` | an update changes a field the table marks `no` |
| `boundary_widened` | a workload actor widens a `narrow` field |
| `boundary_exceeded` | a spawned child or a set replica exceeds its parent's boundary |
| `ceiling_exceeded` | a resolved value exceeds an operator ceiling or an authorizer limit |
| `capability_unsupported` | a field the environment cannot serve |
| `admission_refused` | the admission step refused, with its reason |
| `admission_unavailable` | the admission step could not be reached |
| `authorizer_unavailable` | `Lookup` could not decide a mount, attach, or use |

`spawn_budget_exhausted` is the controller's, raised by the atomic
debit of [[022-mesh-and-spawn]] when a child that passed stage 6 finds
no budget left at create; `Resolve` never emits it.

### Schema evolution

- Within `cella.latere.ai/v1beta1`, a change adds an optional field with a
  default that preserves the previous behaviour, or adds an enum value.
  A field never changes type or meaning, and is never removed.
- A manifest accepted by stages 1 and 2 of one `v1` build is accepted
  by every later `v1` build and resolves to the same object, defaults
  aside. Stages 4 to 7 depend on the actor, the referenced objects, and
  the environment, and are outside the promise.
- A change that cannot meet those rules is `cella.latere.ai/v2`, with a
  conversion in both directions and a period where both are served.
- Until the first tagged release, the schema may still change; the
  CHANGELOG names every field change.
- `manifest/testdata/v1/` holds a corpus of accepted manifests with
  golden resolved outputs under fixed options; a change that alters a
  golden file is a schema change and needs its CHANGELOG line.
- The OpenAPI description of every kind is generated from the Go types
  ([[008-api]]) and a drift between the two fails the gate.

### Package layout

`manifest/v1` holds the types of every kind, including `Capabilities`,
which `runtime` aliases so the two never disagree, and the `Object`
interface every kind implements (`Kind() string`, `ID() string`,
`Owner() string`, `Name() string`), which `Decode` returns and the
store keys by; it imports nothing but the standard library. `manifest` holds `Decode`, `Resolve`, the
quantity and duration parsers, the host rule, the boundary check, and
the error type, and imports `manifest/v1` and
`latere.ai/x/pkg/hostmatch`. Neither imports `internal/` or `runtime`.

## Not in this spec

The `Secret`, `Volume`, `SandboxSet`, and `Environment` kinds
([[018-egress-and-secrets]], [[019-volumes]],
[[020-scheduling-and-sets]], [[021-data-plane-workers]]); where
`Defaults` and `Ceilings` come from ([[007-admission]]); how a resolved
manifest becomes a driver call ([[005-lifecycle-controller]]); the HTTP
mapping and user sentences of the errors ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The example above decodes from YAML and from its JSON form to equal objects, and resolves without error under options that grant every capability it uses | `TestDecodeYAMLAndJSONAgree`, `TestTheExampleResolves` | built over the fields this schema carries, with `TestDecodeTakesYAMLAndJSON` over the three YAML types, and over HTTP as conformance case `case003ContentTypes` ([[055-api-contract-gaps]]); the example's volume, port, scheduling and `workspace.git` fields wait on the specs that add them |
| Every unknown field, at any depth, is refused with its path | `TestUnknownFieldNamesThePath`, table-driven over twenty paths | built: fourteen paths through `metadata`, `spec`, `status` and two list entries, with the open maps that admit any member and the two other kinds ([[055-api-contract-gaps]]) |
| A second YAML document, a wrong version, a wrong kind, and an unsupported content type are refused with their codes, version before kind | `TestDecodeRefusals` | built: the second document in both syntaxes, the version and the kind each before any field, and every content type outside the four ([[055-api-contract-gaps]]) |
| An alias chain past 1 MiB and nesting past 64 levels are each refused in under 100 ms | `TestYAMLLimits` | built ([[055-api-contract-gaps]]) |
| Every syntax rule in the field table has a refusing case: quantity, duration and `never`, RFC 3339, DNS-1123 names, port range and uniqueness, display ranges, OCI reference, absolute paths | `TestFieldSyntax`, table-driven | partial: `TestFieldSyntax`, `TestParseQuantity` and `TestParseDuration` over the fields that exist |
| Every default in the table is applied and returned; a field the caller set is never overwritten; `mode` is inferred from hosts and secrets; an absent name comes from `NewName` | `TestDefaultsFillOnlyAbsentFields`, `TestModeInference`, `TestNameGeneration` | partial: `TestDefaultsFillOnlyAbsentFields`, `TestNameGeneration`, `TestModeInference` ([[039-egress-gateway]]) and `TestModeInferenceFromASecret` ([[046-secret-kind]]); the fields the later kinds add wait on them |
| Each `exclusive_fields`, `path_conflict`, and `missing_field` case in the table is refused with the code | `TestEgressExclusiveFields`, one case per rule | partial: `TestEgressExclusiveFields` with [[039-egress-gateway]]; the path and missing-field cases wait on volumes |
| An admission function's output is validated again; one that changes `kind` or `metadata.name` on update is `admission_refused` | `TestAdmissionOutputIsValidated`, `TestReturnedManifestIsDecodedStrictly` | built |
| An admission step's error keeps the code it named: a refusal is `admission_refused` and no decision is `admission_unavailable` | `TestAdmissionErrorsKeepTheirCode`, `TestAdmissionRefusalAndOutageAreTheirOwnAnswers` | built |
| The image rule of stage 3: required where the environment runs images, refused where it runs none, `Defaults.Image` applied at stage 2 only where it is required | `TestImageIsRequiredAfterAdmission` | built |
| Every mounted secret's hosts join the allow list; two secrets on one host are `secret_host_conflict`; a clone secret without the clone host, or with an `ssh://` URL, is `secret_out_of_scope`; companion names collide with `env` and other entries | `TestSecretReferences` | partial: `TestSecretReferences` and `TestTheMapCarriesTheValue` over the mounts, the conflict and the companion names ([[046-secret-kind]]); the join is the compiler's, and the clone secret waits on `workspace.git` |
| `Lookup` returning not-found and refused both surface as `not_found`; unavailable surfaces as `authorizer_unavailable` | `TestLookupErrors` | partial: `TestLookupErrors` over `Environment`, the one reference the interface carries |
| A `single` volume attached read-write elsewhere is `volume_busy`; a volume in another environment is `invalid_field` | `TestVolumeReferences` | not built |
| Every reserved env key and prefix, in `env` and as a `secrets[].env`, is `reserved_prefix`; the list equals the gateway's | `TestResolve`, `TestFieldSyntax`, `TestSecretReferences`, `TestReservedKeysMatchTheGateway` | built: `TestResolve` and `TestFieldSyntax` over `env`, `TestSecretReferences` over `secrets[].env` ([[046-secret-kind]]), and `TestReservedKeysMatchTheGateway` ([[039-egress-gateway]]) |
| The host rule: each refused form (IP, single label, port, private range) is `invalid_field`; wildcard containment matches `hostmatch` on a table of pattern pairs | `TestHostRule`, `TestHostPatternCovers` | built: `TestHostRule` and `TestHostPatternCovers` ([[039-egress-gateway]]), and `TestSecretRefusals` over a Secret's scope ([[046-secret-kind]]) |
| Every immutable field changed on update is named in one `immutable_field` error; a workload widening each `narrow` field is `boundary_widened`; the owner widening the same is accepted | `TestImmutableFields`, `TestNarrowingIsForWorkloads` | partial: `TestImmutableFields`, `TestNarrowingIsForWorkloads` over the egress fields ([[039-egress-gateway]]), `TestAWorkloadCannotMountASecret` over the mounts ([[046-secret-kind]]), and `TestMeshEnabledIsImmutable` with `TestWorkloadCannotRaiseItsOwnBudget` over the mesh and the two spawn axes ([[040-mesh-and-spawn]]); the four `scheduling` fields as `TestSchedulingIsImmutable` ([[057-scheduling-queue]]) |
| Each of the nine boundary rules, violated one at a time against a parent, is `boundary_exceeded` naming the path; a conforming child passes; a child's default `ttl` is cut to the parent's remaining life | `TestBoundaryCheck`, table-driven over the nine rules | built by [[040-mesh-and-spawn]] for the eight rules with a field, and the child's default `ttl` with it; rule 4 waits on [[019-volumes]] |
| Ceilings refuse with the field and the ceiling; a ceiling of zero is no ceiling; `priority` above `Limits.MaxPriority` is `ceiling_exceeded` | `TestCeilings`, `TestSchedulingFields` | built: `TestCeilings` over the resource and ttl ceilings, and `TestSchedulingFields` over `scheduling.priority` against a positive limit and against a limit no manifest meets ([[057-scheduling-queue]]) |
| Each capability row of stage 7 refuses or warns as stated when the capability is absent and passes when present | `TestImmutableFields`, `TestFieldSyntax` and `TestMeshCapability`, one case per row | partial: the `Resize` row in `TestImmutableFields`, the workspace source rows in `TestFieldSyntax`, and the `Mesh` row in `TestMeshCapability` ([[040-mesh-and-spawn]]); the scheduling row, every field refused on a direct environment and the queue held to the ones a queued environment declares, in `TestSchedulingFields` ([[057-scheduling-queue]]) |
| `status` on apply is ignored; `Resolve` returns an empty status but warnings | `TestStatusIsIgnoredOnApply` | built |
| `Resolve` on the same input, options, and lookup answers twice yields byte-identical JSON | `TestResolveIsDeterministic` | built |
| Every manifest in `testdata/v1/` resolves to its golden output | `TestGoldenCorpus`, `TestCorpusCoversTheSchema` | built ([[056-contract-evidence]]): seven accepted manifests with their resolved goldens, four of them with a YAML form beside the JSON that decodes to the same object ([[055-api-contract-gaps]]), and fifteen refusals with the code and the paths each earns, under one fixed set of options on a queued environment, so every scheduling field is set by an accepted entry and an unknown queue is refused ([[057-scheduling-queue]]); every field of the schema is set by some entry and every code a golden records is one this package emits |
| The quantity parser agrees with the Kubernetes parser on a fuzz corpus | `FuzzQuantity` against `resource.ParseQuantity` in a test-only dependency | built ([[056-contract-evidence]]): three million executions with no divergence; the two narrowings, a value finer than a milli-unit and a milli form past an int64, are named in the test and are the whole of what the subset refuses |
| `manifest/v1` imports only the standard library; `manifest` imports no `internal/` or `runtime` package | `TestManifestImports` | built |
