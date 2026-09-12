# Security

Report a vulnerability to security@latere.ai. Do not open a public issue
for one. You will hear back within three business days, and a fix for a
high severity issue ships within thirty days. Credit in the release notes
on request.

Fixes go to the two most recent minor release series. There is no release
yet; the first one is `v0.1.0`.

What Cella protects, against whom, and how each threat is answered is
written down in the [threat model](specs/013-security-and-threat-model.md),
where every control names the test that proves it, so a reviewer can
check the design rather than take it on faith. The commitments the
design makes: every `/v1` request carries a token from an issuer the
operator listed or one the control plane minted, and nothing acts
before the authorizer has decided, with a refused object answering
exactly as a missing one; a decision the authorizer cannot give is a
refusal, never an allow; a sandbox reaches the network only through
its egress gateway, which admits what its manifest's mode and host
lists allow, on every environment that enforces egress; a workload
token identifies one sandbox and is refused by the control plane once
that sandbox is gone.

Dependencies are checked for known vulnerabilities on every push. A
release will carry an SPDX bill of materials for the module graph and one
per image, cosign signatures over the images and the checksums, and an
SBOM and a build provenance attestation per image, so
`gh attestation verify` answers for the image you are about to run.
