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
| the network the gateway runs on | the gateway connects only to public addresses and the ranges its operator names, judged by the address each connection dials, so a name that resolves to a private, loopback or link-local address is refused like the address itself, on both doors and inside a tunnel it terminates; no range the operator names reaches loopback or link-local | `TestGateMatrix`, `TestANameThatResolvesInwardIsRefusedAtTheDial`, `TestATerminatedRequestThatResolvesInwardIsRefused`, `TestLoadEgressRefusesARangeThatIsNotOne` |
| other sandboxes and their workspaces | the boundary narrows and never widens after creation, and a sandbox a sandbox creates is a subset of its parent on every boundary field | `TestNarrowingIsForWorkloads`, `TestBoundaryCheck`, `TestParentCannotBeNarrowedBelowAChild` |
| the network around the control plane | the port proxy resolves a port only by a name the sandbox itself declared and opens the connection through the driver serving that sandbox, so no host, port, path or header a request carries reaches another sandbox or any other address | `TestPortProxyIsConfined` |
| a secret's value | the sandbox holds a random placeholder; the value is opened in one place and reaches the gateway alone, and is substituted only toward the hosts its owner scoped | `TestValuesAreConfined`, `TestSubstitutionIsScopedAndPlaced` |
| secret values at rest | a data key per secret under AES-256-GCM, wrapped by the installation's key, which rotates without touching a ciphertext | `TestRewrapRotatesTheKey` |
| a sandbox's environment values and gateway credential at rest | the database holds both sealed under the installation's key and drops them when the sandbox is deleted, and no label or annotation of any object a driver writes carries either | `TestNoRowHoldsACredential`, `TestNoObjectButTheSecretCarriesACredential`, `TestNoLabelCarriesAValue` |
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
is built from a tagged commit whose verify run passed, through the shared
release stages in [latere-ai/ci](https://github.com/latere-ai/ci). Each
image is signed by the build stage's identity, run for this repository, and
carries an SPDX bill of materials and a build provenance attestation, so
`gh attestation verify` answers for the image you are about to run. The
release carries the archives, the deploy archive, the bill of materials of
each image, and `checksums.txt` over all of them, signed by the publish
stage's identity. To check a release, with `OWNER` the account it was
published under and `TAG` its tag:

```sh
gh release download "$TAG" -R "$OWNER/cella" -D cella-release
cd cella-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository "$OWNER/cella" checksums.txt
sha256sum -c checksums.txt

for image in cellad cella-display; do
  cosign verify "ghcr.io/$OWNER/$image:$TAG" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
    --certificate-github-workflow-repository "$OWNER/cella"
  gh attestation verify "oci://ghcr.io/$OWNER/$image:$TAG" -R "$OWNER/cella" \
    --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
done
```

The release workflow runs the same checks against every release it
publishes.

Releases up to v0.22.5 were signed by this repository's own release
workflow, whose identity names the tag. Check one of those with the same
steps, the identity naming that workflow in both `cosign` commands, and no
`--signer-workflow`; `cella-display` is in the releases from v0.4.0:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/$OWNER/cella/.github/workflows/release.yml@refs/tags/$TAG" \
  checksums.txt
sha256sum -c checksums.txt

for image in cellad cella-display; do
  cosign verify "ghcr.io/$OWNER/$image:$TAG" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/$OWNER/cella/.github/workflows/release.yml@refs/tags/$TAG"
  gh attestation verify "oci://ghcr.io/$OWNER/$image:$TAG" -R "$OWNER/cella"
done
```
