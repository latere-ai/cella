# Specs

Design specs for Cella, an open source control plane for sandboxes: a
declarative contract for an agent or workload environment, the API
that creates, drives, and observes it, the scheduling that decides
when and where it runs, and the boundary it lives inside. One spec
covers one component. Each states the problem, the design with enough
precision to build from, and acceptance criteria that are testable
statements. Spec 001 fixes the architecture every other spec assumes;
read it first. Spec 003 is the contract a caller codes against, and
spec 015 is the suite that proves a server serves it. Spec 002 is the
configuration reference: every `CELLA_*` variable is in its table,
owned by it or listed with its owner.

## Layout

Flat files `specs/NNN-name.md` in one number space with `track: core`
in the frontmatter. Numbers are stable identifiers and are never
reused. Open specs sit here and are the work queue. A terminal spec
moves to `specs/.archive/` keeping its number so `depends_on` paths
keep resolving.

## Lifecycle

```mermaid
stateDiagram-v2
  [*] --> vague
  [*] --> drafted
  vague --> drafted: scoped
  drafted --> validated: review passes
  validated --> dispatched: every dependency at testing or later
  dispatched --> in_progress: first commit
  in_progress --> testing: implementation lands
  testing --> complete: verified, Outcome written
  drafted --> stale
  validated --> stale
```

`in_progress` is written `in-progress` in the frontmatter. A spec at
`testing` moves to `complete` when every acceptance criterion has a
passing test in the tree and the Outcome records every divergence. The
dispatch gate is on the dependencies' state: a validated spec is
dispatched when every spec in its `depends_on` is at `testing` or
later.

## Index

| # | Spec | Effort | Status | Builds on |
|---|---|---|---|---|
| [001](001-architecture.md) | Architecture: control plane and data plane, packages, extension points, invariants | medium | validated | - |
| [002](.archive/002-repository-scaffold.md) | Repository scaffold: module, binary, configuration, gate, images, workflows | small | complete | - |
| [003](003-manifest-contract.md) | Manifest contract: the Sandbox kind, decoding, validation, defaulting, resolve, the boundary check | large | in-progress | 001 |
| [004](004-runtime-contract.md) | Runtime contract: the Driver interface, optional interfaces, isolation classes, capabilities, the six drivers, conformance | large | in-progress | 001, 003 |
| [005](005-lifecycle-controller.md) | Lifecycle controller: desired to observed, the phase machine, create and update, the reaper, recovery, cascade | large | in-progress | 003, 004 |
| [006](006-identity.md) | Identity: OIDC issuers, workload and environment tokens, the authorizer webhook, the owner policy | medium | in-progress | 001, 002 |
| [007](007-admission.md) | Admission: AdmitFunc, defaults and ceilings, the admission webhook, the count ceiling | small | validated | 003, 006 |
| [008](008-api.md) | API: the /v1 kinds, addressing and concurrency, streams, the error table, OpenAPI | large | in-progress | 003, 005, 006, 007, 010 |
| [009](009-events.md) | Events: one signed record per mutation and operation, typed, ordered per object, to the operator's sink | small | validated | 005, 006, 010 |
| [010](010-state.md) | State: desired and observed, the store contract, transactions, secret values, the journal, queues and operations, optional Postgres | large | in-progress | 003, 004, 005 |
| [011](011-agent-client.md) | Agent client: the cella command, its client package, exit codes, output, the skill | medium | validated | 002, 003, 006, 008, 018, 019, 020, 021, 023 |
| [012](012-test-stubs-and-tiers.md) | Test stubs and tiers: the stubs, the bootstrap of make run, the driver tiers, the kind overlay, CI jobs | medium | validated | 002, 004, 006, 007, 009, 010, 018, 021 |
| [013](013-security-and-threat-model.md) | Security and threat model: assets, adversaries, every control with its test, what is out of scope | medium | validated | 001, 003, 004, 006, 007, 008, 009, 010, 011, 018, 019, 021, 022, 023 |
| [014](014-release-and-installation.md) | Release and installation: images, binaries, attestations, deploy manifests, cellad check, upgrades | medium | validated | 002, 012, 015 |
| [015](015-conformance-suite.md) | Conformance suite: the API contract as executable cases against any server, with a report | large | validated | 003, 004, 005, 006, 008, 009, 011, 012, 018, 019, 020, 021, 023 |
| [016](016-building-a-plane.md) | Building a plane: how a platform composes the packages and the webhooks without a fork | small | validated | 001, 004, 006, 007, 015 |
| [017](017-observability.md) | Observability: one metric table across three roles, traces across the seam, redacted logs, alert rules | small | validated | 002, 005, 006, 008, 009, 010, 018, 020, 021 |
| [018](018-egress-and-secrets.md) | Egress and secrets: the Secret kind, placeholders, the gateway as a data plane component, sync and telemetry, the boundary a workload cannot widen | large | validated | 003, 004, 006, 010 |
| [019](019-volumes.md) | Volumes: the Volume kind, access and attachment, sources and fill, snapshots, the managed workspace | medium | validated | 003, 004, 005 |
| [020](020-scheduling-and-sets.md) | Scheduling and sets: environment modes, capacity, the queue, preemption, pools, the SandboxSet kind for rollouts | large | validated | 003, 004, 005, 007, 010, 019 |
| [021](021-data-plane-workers.md) | Data plane workers: the Environment kind, the default environment, registration, the worker stream and its operations | large | validated | 004, 006, 008, 018 |
| [022](022-mesh-and-spawn.md) | Mesh and spawn: peers that reach each other, sandboxes that create sandboxes, a boundary that never moves | medium | validated | 003, 005, 006, 010, 018 |
| [023](023-computer-use-operations.md) | Computer use operations: the desktop, screenshot, screen, input, ports and the proxy, browser-ready sandboxes | medium | validated | 003, 004, 005, 008, 009 |
| [024](024-vm-driver.md) | VM driver: a hardware-isolated sandbox per environment; the design held open | large | vague | 004, 019 |
| [031](031-hosted-sandbox-consolidation.md) | Hosted sandbox consolidation: every package of latere-ai/sandbox lands in cella, in the platform, or is dropped | large | in-progress | 001 |

## Dependency graph

Arrows point from a spec to the specs it builds on. The picture is the
transitive reduction of the `depends_on` edges: an arrow is drawn only
where no other path already carries it. The Builds on column above
carries each spec's literal `depends_on`.

```mermaid
flowchart BT
  S001[001 architecture]
  S002[002 scaffold]
  S003[003 manifest contract]
  S004[004 runtime contract]
  S005[005 lifecycle controller]
  S006[006 identity]
  S007[007 admission]
  S008[008 API]
  S009[009 events]
  S010[010 state]
  S011[011 agent client]
  S012[012 stubs + tiers]
  S013[013 security]
  S014[014 release + install]
  S015[015 conformance suite]
  S016[016 building a plane]
  S017[017 observability]
  S018[018 egress + secrets]
  S019[019 volumes]
  S020[020 scheduling + sets]
  S021[021 workers]
  S022[022 mesh + spawn]
  S023[023 computer use]
  S024[024 vm driver]
  S003 --> S001
  S004 --> S003
  S005 --> S004
  S006 --> S001
  S006 --> S002
  S007 --> S006
  S007 --> S003
  S008 --> S005
  S008 --> S007
  S008 --> S010
  S009 --> S005
  S009 --> S006
  S009 --> S010
  S010 --> S005
  S011 --> S008
  S011 --> S018
  S011 --> S019
  S011 --> S020
  S011 --> S021
  S011 --> S023
  S012 --> S007
  S012 --> S009
  S012 --> S010
  S012 --> S018
  S012 --> S021
  S013 --> S008
  S013 --> S004
  S013 --> S018
  S013 --> S019
  S013 --> S021
  S013 --> S022
  S013 --> S023
  S013 --> S011
  S014 --> S012
  S014 --> S015
  S015 --> S011
  S015 --> S012
  S015 --> S018
  S015 --> S019
  S015 --> S020
  S015 --> S021
  S015 --> S023
  S016 --> S015
  S016 --> S004
  S017 --> S008
  S017 --> S018
  S017 --> S020
  S017 --> S021
  S018 --> S004
  S018 --> S006
  S018 --> S010
  S019 --> S005
  S020 --> S007
  S020 --> S010
  S020 --> S019
  S021 --> S018
  S021 --> S008
  S022 --> S018
  S022 --> S010
  S023 --> S008
  S023 --> S009
  S024 --> S019
```

## Build order

| Phase | Specs | Delivers |
|---|---|---|
| 0 | 001, 002 | the design and a repository that passes its gate |
| 1 | 003, 004 | every kind as code, the driver contract, the `native` and `local` drivers passing the conformance suite of 004 |
| 2 | 005, 006, 007, 010 | a sandbox's lifecycle with desired and observed state, identity in and out, admission, recovery |
| 3 | 008, 009, 018, 019 | the API, the events, the gateway with secrets, volumes: `cellad` creates a bounded sandbox from a manifest |
| 4 | 021, 012, 017 | workers and self-hosted environments, the stubs, `make run`, the tiers, the metrics |
| 5 | 020, 022, 023, 013, 011 | the scheduler and sets, mesh and spawn, computer use, the threat model's controls tested, the command |
| 6 | 015, 014, 016 | the contract as a suite, a release, an install document CI executes, the plane guide; the point at which a platform builds on it |

The `k8s` and `podman` drivers land during phase 3 once the tiers
exist. The `vm` driver is [[024-vm-driver]], held at `vague` until a
consumer names a need; it is not in any phase.

## Decisions across specs

| Decision | Where | Why |
|---|---|---|
| a control plane, not a sandbox: the data plane is a driver in-process or a worker that connects outbound | 001, 021 | the open component is the contract and the decisions; where sandboxes run is the operator's, including their own infrastructure |
| `cella.latere.ai/v1beta1` as the API group and version | 001, 003 | Kubernetes asks only that a group be a DNS subdomain and every non-core project uses its own; this is Latere's open source project, and the group is the one place the name appears |
| the whole control plane is public and one installation is Latere's | 001 | Origo showed the shape; policy leaves through webhooks, not through a private directory |
| exported packages and a binary | 001 | a platform migrates by import first and by process split later, without a rewrite between |
| desired state in the store, observed state in the driver | 001, 005, 010 | a durable store recovers a sandbox the data plane lost; a rebuildable index survives a store the operator lost; neither failure takes a tenant's environment |
| scope lives on the secret; a manifest chooses secrets and narrows hosts, never widens | 018 | a boundary the workload negotiates with is not a boundary |
| two secrets on one host are refused, placeholders are random per sandbox | 018 | the two defects the reference designs collapsed into silence or made guessable |
| the boundary declared at the root bounds every descendant, as a subset check at resolve | 003, 022 | spawn and sets are safe only if a child cannot ask for one more host |
| volumes are a kind, not a mount of a file plane | 019 | an application's state and a run's tools need storage with a life of its own; a remote mount under every run is a sync layer forever |
| scheduling is the environment's: `direct` or `queued`, an optional pool; a manifest never chooses | 020, 021 | when and where to run is an operator's capacity decision; a pool is a transparent acceleration, and diverse environments cannot be kept warm economically |
| `v1beta1` until the schema settles | 003 | the kinds may still change field by field before a `v1` promise binds importers |
| one way per thing: no `tier` beside `Volume`, no `deadline` beside `ttl`, no `policy` beside labels and admission, no `OpenEgress` beside the egress mode list | 003, 004, 007, 019 | an overlapping pair is two rules to keep consistent and two ways for a reader to be wrong |
| isolation is a class the environment declares: container, vm, process, none | 004 | a caller that needs a floor names it; the control plane never downgrades silently |
| the `local` driver builds on `latere.ai/x/pkg/hostsandbox` (v0.60.1), extracted from its first consumer on 2026-09-12 | 004 | one srt renderer, deny table, preflight, and detached handle for both consumers; a fix lands once |
| the egress gateway is a data plane component that connects outbound with an environment key, like a worker | 018, 021 | maps down and records up on one stream: restart-safe, no inbound route, one mechanism for both plane kinds, monitoring included |
| two shipped binaries: `cellad` with `serve`, `worker`, `egress`, `check` as roles, and a small `cella` client; stubs test-only | 001, 002 | one image for the server side, a dependency-light client where agents run, one allow list per role package so the merge loosens nothing |
| fail closed on every webhook and in the provisioning order | 006, 007, 009, 018 | an unavailable decision that allowed, or a half-provisioned sandbox with egress, would make an outage a privilege escalation |
| the owner policy exists | 006 | a laptop and a small team need no authorizer, and "no authorizer" must still be a policy with tests |
| identity is the shared libraries', not Cella's: the verifier is `latere.ai/x/pkg/authkit/jwt`, the envelope, client, cache, retry, owner-policy frame, stub and conformance suite are `latere.ai/x/pkg/authz`, and `cellad` adds its action vocabulary and its `resource` shapes and nothing else | 006 | one implementation of the wire across the sibling open cores, so an endpoint written for one answers another; the gate's `verifier` waiver is gone and `identity` passes with none |
| Postgres optional | 010 | one binary from a laptop to a replicated service; the difference is one variable |
| Apache-2.0 | LICENSE | a contract others implement wants the patent grant; the sibling open cores carry the same licence |

### Open

| Question | Where | Owner |
|---|---|---|
| The `vm` driver's isolation: a microVM driver of Cella's own, or the k8s runtime class alone; options and criteria in [[024-vm-driver]] | 024 | when a consumer names a need; not before the first release |

### Settled

| Question | Where | How it was answered |
|---|---|---|
| The `pkg/egress` additions [[018-egress-and-secrets]] needed before the gateway role: a dependency-free `placeholder` subpackage, a per-principal policy gate before a CONNECT completes, per-entry `inject` placement, `Registry` replace-all, the reverse door and the outbound sync client | 018 | `placeholder` moved to `latere.ai/x/pkg/egress/placeholder` in v0.79.0, because a package that dials nothing must be able to mint one. The other five landed in `internal/egressd` with [[039-egress-gateway]]: cella is their only consumer today, and `pkg` holds small client primitives. A second consumer moves any of them, and the gate, the doors and the client are written to be moved |

## Open source readiness

The repository is public from its first commit. The conditions the
tree keeps to, checked by tests where a test can: the gate passes on
every push; every spec has acceptance criteria that name tests; no
Latere hostname or namespace in a released artifact, a deploy manifest,
an inherited default, or a documentation page, except as an example or
the API group (the module path, its `latere.ai/x/*` dependencies, and
the shared CI pipeline are the project's coordinates); a fork's tag
publishes under the fork's namespace; the
install document is executed by CI; the threat model is written down
and each control names its test.

## Conventions

Frontmatter fields: `title`, `status`, `track`, `depends_on`,
`affects`, `effort`, `created`, `updated`, `author`. Sections:
Overview, Current state, Design, Not in this spec, Acceptance criteria,
and Outcome once complete. Acceptance criteria are a table of
criterion, the test that proves it, and its state. Names in a spec are
the names the code uses. Cross-references are `[[NNN-name]]`
wikilinks. No em dashes; the technical register throughout.

## Consolidation slices

Slices of [[031-hosted-sandbox-consolidation]]: one package group of the
hosted sandbox each, ported into the contract its parent spec states.
The map, the order, and the rule for a slice are in 031.

| Spec | Status | Deliverable |
|---|---|---|
| [025-native-runtime-migration.md](.archive/025-native-runtime-migration.md) | complete | Native lifecycle, execution and archive transfer; partial spec 004 |
| [026-direct-control-plane.md](.archive/026-direct-control-plane.md) | complete | Strict manifest subset and durable direct sandbox API |
| [027-configured-audience-set.md](.archive/027-configured-audience-set.md) | complete | External audience sets with one local signing audience |
| [028-runnable-native-control-plane.md](.archive/028-runnable-native-control-plane.md) | complete | Server wiring and authenticated native lifecycle end-to-end |
| [029-workspace-files-and-logs.md](.archive/029-workspace-files-and-logs.md) | complete | Authorized tar import/export and process log routes |
| [030-native-main-process.md](.archive/030-native-main-process.md) | complete | Main command supervision and persisted output |
| [032-runtime-conformance-suite.md](.archive/032-runtime-conformance-suite.md) | complete | Driver conformance suite, Nop fake, dial-nothing and coordinates tests |
| [033-file-operations.md](.archive/033-file-operations.md) | complete | The `FileStore` capability, the granular file routes, and one containment rule across the three drivers |
| [034-terminal-attach.md](.archive/034-terminal-attach.md) | complete | Attacher and the native pseudo-terminal, podman attach, the exec and attach WebSockets |
| [035-podman-driver.md](.archive/035-podman-driver.md) | complete | Container isolation over the libpod socket, with the sandbox record in the engine |
| [037-lifecycle-enforcement.md](.archive/037-lifecycle-enforcement.md) | complete | Reaper deadline rules, lease and clock seams, activity stamping |
| [043-postgres-store.md](.archive/043-postgres-store.md) | complete | Store contract of spec 010, memory and Postgres adapters, the lost rule and recovery |
| [039-egress-gateway.md](.archive/039-egress-gateway.md) | complete | The network boundary, the map compiler, the gateway role and its two doors, sync and records |
| [042-events.md](.archive/042-events.md) | complete | Signed event records per mutation and operation, the journal's delivery half, the operator's sink |
| [044-manifest-fields.md](.archive/044-manifest-fields.md) | complete | Resource, user, workspace and lifecycle manifest fields with the staged resolver |
| [036-k8s-driver.md](.archive/036-k8s-driver.md) | complete | Kubernetes driver: a claim and a Pod per sandbox, the baseline, exec, logs, archives, identity on the objects |
| [045-workload-tokens.md](.archive/045-workload-tokens.md) | complete | The workload token minted at create, projected by every driver, rotated at two thirds of its life, revoked by jti |
| [046-secret-kind.md](.archive/046-secret-kind.md) | complete | The Secret kind, its stored value, and the substitution that value reaches a sandbox's request by |
| [048-release-and-check.md](.archive/048-release-and-check.md) | complete | The v* tag pipeline, the deploy tree an operator installs from, the install walk, and `cellad check` |
