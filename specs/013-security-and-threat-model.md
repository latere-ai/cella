---
title: "Security and threat model: assets, adversaries, every control with its test, what is out of scope"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/006-identity.md
  - specs/007-admission.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/011-agent-client.md
  - specs/018-egress-and-secrets.md
  - specs/019-volumes.md
  - specs/021-data-plane-workers.md
  - specs/022-mesh-and-spawn.md
  - specs/023-computer-use-operations.md
affects: [internal/auth/, internal/api/, internal/egressd/, internal/worker/, internal/store/, runtime/, egress/, controller/, manifest/, deploy/, test/e2e/, SECURITY.md]
effort: medium
created: 2026-09-12
updated: 2026-09-20
author: changkun
---

# Security and threat model

## Overview

A sandbox runs code nobody has read, written by a person or an agent,
with network access and credentials in its reach. This spec names the
assets, the adversaries, and the controls, so a reviewer checks the
design and `SECURITY.md` points at one document. Every control is a
row with the spec that owns it and the test that proves it; a control
without a test is a claim, and a test reads this file to hold the
table to that rule.

## Current state

Not built. The controls descend from the hosted platform's, minus the
ones that belonged to its identity and billing surfaces, plus the ones
the control plane's own design added: the gateway credential, the
boundary check, the bounded fetch.

## Design

### Assets

| Asset | Where it lives |
|---|---|
| the hosts and clusters sandboxes run on | the data plane |
| other sandboxes and their workspaces | the data plane |
| secret values | the store, encrypted; a gateway's memory, in plaintext, for its environment |
| the gateway credential per sandbox | the map and the sandbox's environment |
| the workload token, the environment keys | the sandbox's projection; the worker's and gateway's hosts |
| `CELLA_TOKEN_KEY`, `CELLA_SECRET_KEY`, `CELLA_AUTHORIZER_TOKEN`, `CELLA_ADMISSION_TOKEN`, `CELLA_EVENTS_SECRET` | the control plane's configuration |
| `CELLA_EGRESS_CA_KEY`, the authority every sandbox trusts | the gateway's host |
| volumes and snapshots | the data plane |
| the store: desired state, the journal, the ledger, revocations, the egress records that name which hosts each sandbox reached | Postgres or the control plane's memory |
| the events, which name who did what | the journal and the sink |
| the service's availability | the control plane |

### Adversaries

| Adversary | Holds | Wants |
|---|---|---|
| a sandbox process | its own token, its gateway credential, placeholders, the gateway per its manifest | the host, another sandbox, a host its manifest did not allow, a secret's value, a longer life, a child with a wider boundary |
| an authenticated caller | a valid bearer | another subject's sandbox, secret, or volume; a ceiling bypass; output it may not read |
| a network position | the wire | a token, a secret, a forged event, a forged map |
| a compromised webhook | the authorizer or admission endpoint | to allow everything or to run an image it chose |
| a compromised worker host | an environment key, the sandboxes it runs | another environment's sandboxes, the store, a secret bound for another environment |
| a compromised gateway host | every map of its environment and the CA key | to substitute values toward hosts of its choosing, to intercept every sandbox's TLS |
| a `Secret` owner | a secret whose scope names a victim's host | to have a victim's sandbox send a request to that host with the owner's credential |
| a platform's decorator author | a hook over the driver's object | to weaken the Pod baseline |

An operator's own misconfiguration is out of scope; the controls say
what a correctly configured control plane refuses.

### Controls

| Threat | Control | Spec | Test |
|---|---|---|---|
| escape from a sandbox to the host | the k8s driver runs every Pod with the baseline table: non-root, all capabilities dropped, read-only root except the workspace, `/tmp`, and the volumes, `seccomp: RuntimeDefault`, no privilege escalation, not privileged, no host namespaces, `shareProcessNamespace: false`, no service account token; podman runs rootless when the socket is a user's | 004 | `TestPodSecurityFields`, `TestClusterPodIsConfined` |
| a decorator weakening the baseline by removal or addition | the driver compares the object field by field against the baseline and its own stamps after every decorator and refuses with `decorator_violation` | 004 | `TestDecoratorCannotWeakenTheBaseline` |
| lateral movement between sandboxes | a NetworkPolicy per Pod admits ingress from mesh peers to `mesh` ports and from the exposer to `public` ports only, and egress to the gateway, DNS, and the control plane only; podman uses one network per sandbox and one per mesh | 004, 018, 022 | `TestClusterNoLateralMovement`, `TestMeshReachability` |
| egress to a host the manifest does not allow | the driver's rule routes every connection to the gateway; the gateway's policy gate refuses a CONNECT or a reverse-door request off the allow list, on the deny list under `open`, or any at all under `none`, before a byte flows; an entry substitutes only toward its hosts and ports | 003, 018 | `TestModes`, `TestClusterEgressBoundary`, `TestSubstitutionIsScopedAndPlaced` |
| a secret's value in a sandbox | the sandbox holds a per-sandbox random placeholder; the value is decrypted in the control plane only inside `Compile` and reaches only the gateway; a placeholder sent off scope leaves as an inert string | 018, 010 | `TestSecretValueIsWriteOnly`, `TestNoSecretLeaks`, `TestValuesAreConfined` |
| secret values at rest | envelope encryption: a data key per secret under AES-256-GCM, wrapped by `CELLA_SECRET_KEY`; `Values.Open` has one caller; `Rewrap` rotates the KEK without touching a ciphertext | 010, 018 | `TestValuesAreConfined`, `TestRewrap` |
| a secret aimed at the cluster | the host rule refuses IP literals, single labels, and private ranges in a scope and in an allow list | 003, 018 | `TestHostRule`, `TestScopeRefusals` |
| a `Secret` owner aiming at a victim's host | a secret is mounted only when the authorizer's `secret.mount` allows it for the mounting subject; a mounted secret's hosts join the allow list at resolve; two secrets on one host are refused | 003, 006, 018 | `TestSecretReferences`, `TestLookupErrors` |
| a sandbox authenticating to the gateway as another | each door checks the sandbox's own credential from the map before any dial; the credential lives as long as the sandbox and never rotates under it | 018 | `TestCredentialOnBothDoors` |
| a gateway that missed a purge | the snapshot on reconnect is authoritative and drops every principal absent from it | 018 | `TestSyncSnapshotIsAuthoritative` |
| the reverse door turned toward a host of the caller's choosing | the destination is the first path segment, matched against the map; anything else is 403 | 018 | `TestModes` |
| a compromised gateway host | the blast radius is its environment: its maps, its CA; a map carries only the secrets of sandboxes placed there, an environment key authorizes only its environment's streams, and a purge or a revocation removes what it held | 018, 021, 006 | `TestEnvironmentKeys`, `TestEgressMapCrossesOneHop` |
| a sandbox acting as its owner | the workload token's subject is the sandbox; the owner policy grants it read and exec on itself, read on descendants, and create within its budget; the authorizer sees `workload` set | 006 | `TestWorkloadIsLeastPrivileged` |
| a listed issuer minting a sandbox's or an environment's identity | subjects are issuer-qualified and the `sandbox:` and `environment:` prefixes are reserved to the control plane's own issuer | 006 | `TestSubjectsAreIssuerQualified`, `TestVerifierRefusals` |
| a token outliving its sandbox | `exp` is the sandbox's expiry or 24 hours; `cellad` refuses a revoked `jti` and a sandbox whose desired state is absent or `Deleting`; the gateway uses no tokens, and its bound is the purge of the sandbox's map | 006, 018 | `TestWorkloadTokenLifecycle`, `TestLiveUpdateAndRevoke` |
| a revoked permission still honoured | an allow is cached per replica for the answer's `ttl` bounded by `CELLA_AUTHORIZER_CACHE`, sixty seconds by default and capped at ten minutes, a deny for five seconds, which is the accepted window; unavailability is never cached | 006 | `TestDecisionCache` |
| a caller reaching another's object | every item route reads, authorizes, then acts; a deny on the caller's own action is 403, and a refused reference at resolve is the same `not_found` as a missing one, so a manifest cannot probe for another's secrets, volumes, or environments | 006, 008 | `TestReadAuthorizeAct`, `TestRefusedReferencesLookMissing` |
| an allow without a decision | the authorizer and admission clients fail closed on every non-200, malformed, incomplete, or late answer; the authorizer retries once, only when the connection failed before a response line arrived, never on a non-200, a post-request timeout, or an unparsable body, and admission never retries; an `http://` endpoint off loopback is refused at start | 006, 007 | `TestAuthorizerFailsClosed`, `TestAdmissionFailsClosed` |
| a webhook that rewrites the manifest past a ceiling or into a boundary | `Ceilings` apply after admission; the boundary check runs after admission | 007, 003 | `TestCeilingsAreAFloorOnStrictness`, `TestAdmissionCannotOpenABoundary` |
| a forged or replayed event | HMAC-SHA256 over the attempt time and the body, recomputed per attempt, one signature per configured secret; the sink refuses a `t` older than five minutes, which is the operator's half | 009 | `TestSignature`, `TestSinkVerifiesSignature` |
| secrets in events, logs, records, or status | events reduce the manifest to env keys and secret names and carry no value, placeholder, credential, token, output, frame, text, query, or header, with `RedactJSON` behind the structural strip; egress records carry no header, body, value, placeholder, or credential; logs redact env values and `Authorization` | 009, 018, 017 | `TestEventsCarryNoSecrets`, `TestTelemetry`, `TestLogsRedact` |
| a workload widening its own boundary | the boundary fields narrow only after create for a workload actor; a child is a subset of its parent by nine rules; a parent cannot be narrowed below a live descendant; only a workload spawns and only within budget and depth | 003, 022 | `TestNarrowingIsForWorkloads`, `TestSpawnBoundary`, `TestParentCannotBeNarrowedBelowAChild`, `TestOnlyWorkloadsSpawn` |
| a runaway spawn tree | the budget is debited in the desired-write transaction and never refunded on delete; depth decrements per generation; a child's `ttl` ends no later than its parent's; delete cascades deepest first | 005, 022 | `TestSpawnDebitIsAtomic`, `TestSpawnFieldsAndDepth`, `TestCascade` |
| a volume as an escape | no host path on k8s; a volume attaches only where `volume.attach` allows; a read-only attachment stays read-only in a child; `Write` is the control plane's path and no sandbox has it | 019, 022 | `TestAttachByIdUnderTheAuthorizer`, `TestControlPlaneWriter` |
| a hostile archive source | the host is on `CELLA_SOURCE_ALLOW` and re-checked on every redirect; the download stops at `CELLA_MAX_SOURCE_BYTES`; the digest is compared before extraction; entries are confined to the volume root | 019 | `TestArchiveFetchIsBounded` |
| a manifest body that exhausts memory | `CELLA_MAX_BODY_BYTES` on manifests, `CELLA_MAX_UPLOAD_BYTES` on tar, 1 MiB on input, the YAML alias and depth limits, the annotation cap | 003, 008 | `TestYAMLLimits`, `TestBodiesAndTypes` |
| a flood | a token bucket per subject after authentication, the authorizer's override, and a bucket per client address before authentication | 008 | `TestRateLimits` |
| a client-supplied request id used for log injection | an `X-Request-Id` is accepted only when it is at most 128 printable ASCII characters, else replaced | 008 | `TestRequestId` |
| exec output to a caller without the right | `sandbox.exec` guards exec, attach, dial, files, screenshot, screen, input, and the port proxy; logs and events are `sandbox.read` and never carry exec output | 008 | `TestRouteTableActions` |
| the port proxy turned toward another sandbox or the Pod network | a port name resolves only within the sandbox's own declared ports and the dial goes through the driver; no caller-supplied host or port reaches the dialer | 023 | `TestPortProxyIsConfined` |
| input used to inject into the driver's argument list | every batch is validated whole; a key matches `^[A-Za-z0-9_]+$`, so a leading `-` never reaches the desktop tool | 023 | `TestInputValidation` |
| a lost write | `Put` with a stale version is `ErrVersionConflict`; the count ceiling and the ledger debit share the desired write's transaction | 010, 007 | `TestOptimisticConcurrency`, `TestTxIsAtomic` |
| a binary running against a schema it does not know | a schema version above the highest embedded migration, or a dirty flag, is a start-up failure | 010 | `TestSchemaGuards` |
| a compromised worker host | an environment key authorizes only its environment's registration, stream, and gateway; an operation carries only that environment's sandboxes; a value crosses to that environment's gateway only for sandboxes placed there; a mismatched isolation is refused; nothing dials into the worker, which connects outbound and claims | 021 | `TestWorkerRegistration`, `TestNoInboundToTheDataPlane`, `TestEgressMapCrossesOneHop` |
| a worker's token minted by the worker | the token is minted by the controller and projected by the worker's driver from the `Create` payload | 021, 006 | `TestWorkerConformance` |
| the control plane's URL reached over plaintext | `CELLA_URL` for the worker, the gateway, and the client must be `https://` unless loopback, or `CELLA_INSECURE_CONTROL_PLANE=1`, which the stubs set and no deployment does | 021 | `TestControlPlaneURLRule` |
| a workload token sent to the gateway by the client | the `cella` client's transport reads no proxy variable, so a workload token never transits the gateway | 011 | `TestClientIgnoresProxyVariables` |
| a key or a secret in a log | every key and bearer is read once at start and never logged; the deploy manifests mount them from Secrets | 002, 014 | `TestLogsRedact`, `TestBaseIsConfined` |
| a dependency with a known vulnerability | the `vuln` gate on every push | 002 | the gate |
| an image that is not what was released | signed images, SBOMs, and provenance attestations; `gh attestation verify` documented | 014 | `TestReleasePublishesUnderTheOwnersNamespace`, the `release-verify` job |
| a test that touches a developer's real state | every tier binds `:0`, keeps state under a temp dir, and reads no real kubeconfig, socket, or sandbox runtime configuration | 012 | `TestTiersAreIsolated` |

### What is out of scope

- Kernel escapes the environment's isolation class does not prevent;
  an operator picks a `vm` class for a floor a container cannot give.
- The `native` driver, which enforces no boundary and no egress, and
  says so in its first sentence.
- The `local` driver's read posture: the sandbox runtime denies the
  operator's home directory and the always-deny table, and everything
  outside the home directory stays readable; the harness's own
  configuration is readable by design so its login works.
- The security of the issuer and of the webhooks themselves.
- A compromised control plane host, which holds `CELLA_TOKEN_KEY`,
  `CELLA_SECRET_KEY`, and every webhook bearer, and therefore every
  sandbox identity and every secret value; an operator protects it as
  the root of the installation.
- Denial of service beyond the rate limits, and side channels between
  sandboxes on one node.
- Credentials a workload receives by means other than a `Secret`.

### The root file

`SECURITY.md` names the address, the response times, and four
commitments, each a row above: every `/v1` request carries a token
from a listed issuer or one the control plane minted, and nothing acts
before the authorizer has decided, with a refusal indistinguishable
from a missing object; an unavailable decision is a refusal; a sandbox
reaches the network only through its gateway, which admits what its
manifest's mode and lists allow, where the environment enforces egress;
a workload token identifies one sandbox and is refused by the control
plane once the sandbox is gone. A test reads both files and holds each
commitment to a row.

## Not in this spec

The deploy manifests that carry the Pod baseline
([[014-release-and-installation]]); the controls' own designs, which
their specs own.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every row's `Test` cell names a test function `go test -list` finds in the module, and every test named appears in the acceptance criteria of the spec the row's `Spec` cell names | `TestThreatModelControlsHaveTests`, reading this file | not built |
| Every commitment in `SECURITY.md` maps to a control row | `TestSecurityPolicyMatchesTheModel`, reading both files | not built |
| A Pod the k8s driver creates has every field of the baseline, `shareProcessNamespace: false` included | `TestPodSecurityFields`, `TestClusterPodIsConfined` running `id`, `cat /proc/1/status`, and a mount attempt | not built |
| A sandbox cannot reach another sandbox's IP or the Pod network except a mesh peer's `mesh` port | `TestClusterNoLateralMovement` | not built |
| A canary secret value and a canary token appear in no sandbox environment, file system, event, record, log, or API response across the e2e tier, and the value reaches the upstream only in the declared header | `TestSecretValuesNeverEnterASandbox` | built in part ([[046-secret-kind]]): the secret value is followed through the sandbox's environment and files, the control plane's data directory, the delivered events, the connection records, both processes' logs and every API answer, and is read back at the upstream in the header its owner named; no canary token is followed |
| A workload token that tries to add a host, mount a secret, spawn past its budget, or narrow its parent below itself is refused with the code named | `TestWorkloadCannotWiden` | not built |
| A non-loopback `http://` `CELLA_URL` is refused by the worker, the gateway, and the client unless the escape hatch is set | `TestControlPlaneURLRule` | not built |
| A worker host with inbound refused runs the whole tier | [[012-test-stubs-and-tiers]]'s `TestWorkerNoInbound`, `TestClusterWorkerNoInbound` | not built |
