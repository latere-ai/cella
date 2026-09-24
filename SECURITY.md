# Security

Cella runs code nobody has read, written by a person or an agent, with
network access and credentials in its reach. This file is what the project
promises about that, what it does not, and how to tell us when it is wrong.

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for it. You
will hear back within three business days, and a fix for a high severity
issue ships within thirty days. Credit in the release notes on request.

Fixes go to the two most recent minor release series, which the
[releases page](https://github.com/latere-ai/cella/releases) lists.

## Commitments

- Every `/v1` request carries a token from an issuer the operator listed or
  one the control plane minted, and nothing acts before the authorizer has
  decided, with a refused object answering exactly as a missing one.
- A decision the authorizer cannot give is a refusal, never an allow.
- A sandbox reaches the network only through its egress gateway, which
  admits what its manifest's mode and host lists allow, on every environment
  that enforces egress.
- A workload token identifies one sandbox and is refused by the control
  plane once that sandbox is gone.

## What is protected, and what proves it

Each row is an asset, the control that protects it, and the tests in this
repository that hold the control to its word. A control without a test is a
claim, so every control names the test you can read and run yourself.

| Asset | Control | Proved by |
|---|---|---|
| the host a sandbox runs on | every container runs non-root with all capabilities dropped, a read-only root filesystem, no privilege escalation, no host namespaces and no service account token | `TestPodCarriesTheBaseline`, `TestHelperPodCarriesTheBaselineAndNoIdentity` |
| the hosts a sandbox may reach | the gateway refuses a connection off the allow list, on the deny list under `open`, and every connection under `none`, before a byte flows | `TestGateMatrix`, `TestHostRule` |
| other sandboxes and their workspaces | the boundary narrows and never widens after creation, and a sandbox a sandbox creates is a subset of its parent on every boundary field | `TestNarrowingIsForWorkloads`, `TestBoundaryCheck`, `TestParentCannotBeNarrowedBelowAChild` |
| the network around the control plane | the port proxy resolves a port only by a name the sandbox itself declared and opens the connection through the driver serving that sandbox, so no host, port, path or header a request carries reaches another sandbox or any other address | `TestPortProxyIsConfined` |
| a secret's value | the sandbox holds a random placeholder; the value is opened in one place and reaches the gateway alone, and is substituted only toward the hosts its owner scoped | `TestValuesAreConfined`, `TestSubstitutionIsScopedAndPlaced` |
| secret values at rest | a data key per secret under AES-256-GCM, wrapped by the installation's key, which rotates without touching a ciphertext | `TestRewrapRotatesTheKey` |
| the gateway credential | each door checks the sandbox's own credential before any dial, and the credential lives as long as the sandbox | `TestProxyDoorNeedsTheSandboxsOwnCredential`, `TestReverseDoor` |
| the workload token | it identifies one sandbox, expires with it, is refused once revoked, and never transits the gateway | `TestWorkloadTokensMintIsCappedByTheSandbox`, `TestRevokedWorkloadTokenIsRefusedByTheAPI`, `TestTheClientIgnoresProxyVariables` |
| another caller's objects | every route reads, authorizes, then acts; an unavailable decision refuses, and an allow is cached only for the window the answer names | `TestAuthorizerFailsClosed`, `TestFailsClosedWithoutRetry`, `TestDecisionCache` |
| the store | a write with a stale version is refused, a mutation and its record commit together, and a binary refuses to start against a schema it does not know | `TestSuiteHoldsTheMemoryAdapter`, `TestPostgresStore`, `TestSchemaGuards` |
| the events | each attempt is signed afresh, the sink verifies it, and no event carries a value, a placeholder, a credential or a token | `TestSignatureFormula`, `TestTheSinkVerifiesWhatTheDelivererSigns`, `TestNoContentInEvents` |
| logs and connection records | environment values and bearers are redacted, and a connection record carries no header, body, value or placeholder | `TestLogsRedact`, `TestRecordNormalize` |
| a data plane you host yourself | a key authorizes one environment's registration and stream and carries only that environment's sandboxes | `TestEnvironmentKeyReachesItsOwnEnvironmentOnly`, `TestWorkerRegistrationRoute` |
| the images you run | a tag publishes under the repository owner's namespace with signatures, a bill of materials and a build provenance attestation | `TestReleasePublishesUnderTheOwnersNamespace`, `TestBaseIsConfined` |

## What is out of scope

- A kernel escape the isolation class does not prevent. Where a container's
  floor is not enough, run the environment on a class that gives a stronger
  one.
- The `native` runtime, which runs workloads as ordinary host processes and
  enforces no boundary and no egress. It is for code you trust.
- The security of your identity provider and of the authorizer, admission
  and event endpoints you write.
- A compromised control plane host. It holds the signing key, the secret
  key and every webhook bearer, and therefore every sandbox identity and
  every secret value. Protect it as the root of the installation.
- Denial of service beyond the rate limits, and side channels between
  sandboxes sharing a node.
- Credentials a workload receives by any means other than a `Secret`.
- Your own misconfiguration. The commitments describe what a correctly
  configured control plane refuses.

## Supply chain

Dependencies are checked for known vulnerabilities on every push. A release
carries an SPDX bill of materials for the module graph and one per image,
signatures over the images and the checksums, and a build provenance
attestation per image, so `gh attestation verify` answers for the image you
are about to run.
