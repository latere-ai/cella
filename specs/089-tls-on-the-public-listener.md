---
title: "TLS on the public listener: cellad serves https from a certificate and key file it reloads without a restart, and a caller trusts a private CA beside the system roots"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/.archive/002-repository-scaffold.md
  - specs/011-agent-client.md
  - specs/013-security-and-threat-model.md
  - specs/018-egress-and-secrets.md
  - specs/021-data-plane-workers.md
  - specs/.archive/069-client-package.md
affects: [cmd/cellad/, internal/config/, client/, worker/, internal/egressd/, docs/configuration.md, docs/install.md, CHANGELOG.md]
effort: medium
created: 2026-11-03
updated: 2026-11-03
author: changkun
---

# TLS on the public listener

## Overview

`cellad`'s public listener, `CELLA_PUBLIC_ADDR` (`cmd/cellad/main.go`,
around the `servers` built at line 725 and the `listen` seam at line 857),
answers every `/v1` request in plain HTTP. There is no setting that puts a
certificate on it; `docs/configuration.md`'s "Listeners and local state"
table names none. A caller in the same private network as the control
plane therefore either dials it over plain HTTP, which carries its bearer
in the clear, or sits behind an operator-run terminator that is not part
of this project and is not configured by anything `cellad` reads.

Since v0.22.6 the client refuses to make that choice silently.
`client.New` (`client/client.go`) and `client.Environment`
(`client/token.go`, reading `CELLA_INSECURE_CONTROL_PLANE`) refuse a
`CELLA_URL` that is `http://` on a host other than loopback, as the worker
and the gateway already did (`internal/config/worker.go`,
`internal/config/egress.go`'s `checkControlPlaneURL`). A caller that wants
to keep talking to a control plane that serves plain HTTP must now set
`Config.Insecure` or `CELLA_INSECURE_CONTROL_PLANE`, which is consent to
exactly the plaintext bearer this spec lets an operator stop asking for.

This spec gives `cellad` a certificate of its own, served on the same
public listener and reloaded from disk when the files change, so a
certificate manager rotates it without a restart. It gives the three
callers that dial `CELLA_URL` directly, the client package, the worker and
the egress gateway, a way to trust a private certificate authority beside
the system roots, so "insecure" stops being the only way to reach a
control plane whose certificate a public root does not chain to.

## Current state

Nothing of this spec is built. The public and internal listeners are both
plain `net.Listener`s wrapped in `http.Server{Handler: ..., ReadHeaderTimeout:
10 * time.Second}` with no `TLSConfig`
(`cmd/cellad/main.go:725`). `docs/configuration.md` names no `CELLA_*TLS*`
variable. `client.Config` already carries `RootCAs *x509.CertPool` and
`HTTPClient *http.Client` (`client/client.go`), so a caller that builds its
own `tls.Config` or supplies its own `http.Client` already reaches a
control plane behind a private CA today; `client.Environment` does not
read anything into `RootCAs`, so that path is closed to a caller that only
sets environment variables, which is every `cella` command invocation
(`internal/cellacli/cli.go`) and every sandbox reading its projected
configuration. The worker's own transport
(`worker/worker.go:261`, `w.client = &http.Client{Transport:
otel.Transport(nil)}`) and the gateway's own stream dialer
(`internal/egressd/egressd.go:146`, `&websocket.Dialer{Subprotocols:
[]string{egress.Protocol}, HandshakeTimeout: 15 * time.Second}`, no
`TLSClientConfig`) each reach `CELLA_URL` with the system roots alone and
no field to add to them.

## Design

### The public listener serves TLS when given a certificate

Two new settings, read where `CELLA_PUBLIC_ADDR` is
(`internal/config/config.go`):

- `CELLA_PUBLIC_TLS_CERT_FILE`: the path of a PEM certificate, the leaf
  first and any intermediates after it.
- `CELLA_PUBLIC_TLS_KEY_FILE`: the path of its PEM private key.

Both set: the public listener, and only the public listener, is wrapped
in TLS at the `listen` seam (`cmd/cellad/main.go:857`,
`tls.NewListener(ln, tlsConfig)` in place of the bare listener passed to
`servers[0]`), and the start-up line already printed
(`cmd/cellad/main.go:721`) gains `tls=true`. Neither set: the listener
stays exactly what it is today, which keeps a local `make run`, the test
tiers, and an operator running their own terminator in front of `cellad`
working unchanged. One set and not the other is a start-up failure naming
both files, in the one-message style `checkAddr` and
`CELLA_INTERNAL_ADDR`'s same-endpoint check already use.

`CELLA_INTERNAL_ADDR` is never wrapped: it answers the probes and
`/metrics` to the orchestrator alone, on the installation's own network
(`cmd/cellad/main.go`'s own comment, "the numbers are the installation's
and not a caller's"), and adding a second certificate setting for a
listener nothing reaches from outside buys nothing.

### Reload without a restart

The files are read again, not at a fixed interval, but through
`tls.Config.GetCertificate`: each handshake stats both paths, and a
changed modification time re-reads and re-parses the pair. A certificate
manager that rotates a mounted secret does so by an atomic symlink swap
(Kubernetes' projected volumes among them), so the mtime a stat reads is
the new file's from the first handshake after the swap, not torn mid
write; polling a timer would either lag the swap or add a goroutine with
nothing to synchronize against the handshake it serves. A pair that fails
to parse, or whose certificate and key do not match, is logged once at
the failure and the last good pair keeps serving: a bad rotation is an
operator's problem to fix, not a reason to stop answering every caller
that was already trusting the old certificate.

`cellad check` (`cmd/cellad/check.go`) reads the configured pair, where
one is configured, and reports its certificate's expiry beside its other
dependency checks, so an installation catches an expiring certificate the
same way it catches an unreachable issuer.

### `http/1.1` only

The `tls.Config` sets no `NextProtos`, so ALPN never negotiates `h2`. The
client's own transport already does the same on its side
(`client/client.go`'s `transport`, `ForceAttemptHTTP2: false`) because the
exec, attach and dial calls are an HTTP/1.1 upgrade to the package's own
WebSocket framing, and a connection that negotiated `h2` has nothing to
upgrade. `MinVersion: tls.VersionTLS12` matches the client's floor.

### `CELLA_PUBLIC_URL` agrees with the listener

When both TLS files are set, `CELLA_PUBLIC_URL` must be `https://` unless
its host is loopback, the same rule `checkControlPlaneURL` already holds
`CELLA_URL` to for the worker, the gateway and the client. `cellad` mints
every token with this URL as `iss`
(`docs/configuration.md`'s "Identity" table), so a public URL that
disagrees with what the listener actually serves is a start-up mistake,
not a runtime one.

### The client, the worker and the gateway trust a private CA

`CELLA_CA_BUNDLE`: PEM certificate authorities trusted beside the system
roots when any of the three dial `CELLA_URL`, read wherever
`CELLA_INSECURE_CONTROL_PLANE` already is
(`client/token.go`'s `Environment`, `internal/config/worker.go`,
`internal/config/egress.go`'s `LoadEgress`). It is a different setting
from the egress role's existing `CELLA_EGRESS_CA_BUNDLE`
(`internal/config/egress.go`), which the gateway trusts an upstream with
on a sandbox's behalf; `CELLA_CA_BUNDLE` is what each role trusts the
control plane itself with, over its own connection to it, and the two
never need the same value.

`client.Config` already has `RootCAs *x509.CertPool`; a caller that builds
one directly keeps doing exactly that. `client.Environment` grows no new
field on `Config`; instead it parses `CELLA_CA_BUNDLE` and, non-empty,
sets `Config.RootCAs` to a clone of `x509.SystemCertPool()` with the
bundle's certificates appended. `client.New` reports a `CELLA_CA_BUNDLE`
that holds no certificate the same way it reports a malformed URL today:
before the first call, not by it. A caller that sets `Config.RootCAs`
itself after calling `Environment` keeps whatever it set; `Environment`
never looks at a `Config` the caller already built, only at the process's
own variables.

The worker's `HTTPClient` (`worker/worker.go`) and the gateway's stream
dialer (`internal/egressd/egressd.go`'s `client.dialer`) each gain a
`TLSClientConfig` built the same way from the same variable, wired
through `internal/config/worker.go` and `internal/config/egress.go`
respectively. Neither touches `upstreamTLS` or `CELLA_EGRESS_CA_BUNDLE`,
which stay the gateway's trust for a sandbox's destinations and not for
its own control plane connection.

### Alternatives considered

- **A second listen address for TLS, beside the plain one.** Rejected: one
  issuer URL, `CELLA_PUBLIC_URL`, already names one address; a second
  address would need its own setting and would still need the agreement
  rule above, with two addresses to keep straight instead of one.
- **`fsnotify` or a timer goroutine for reload.** Rejected above: a stat
  at handshake time is cheaper than a watch goroutine, needs no new
  dependency, and never races a swap that a timer could sample mid way.
- **`SIGHUP` to reload.** Rejected: it adds a second reload path to
  document and test beside the one a stat already gives for free, for no
  deployment that cannot already rotate a mounted file.
- **A restart on rotation.** Rejected: spec 076 already keeps a rolling
  control plane serving through a restart; a certificate rotation on its
  own schedule should not need to walk through that machinery.

### What this changes in the threat model

[[013-security-and-threat-model]]'s control table gains a row: the threat
"a caller in the same private network reading a bearer off the wire",
with the control "the public listener serves TLS from a certificate and
key file, reloaded without a restart; an operator with neither still
chooses plaintext, and the worker, the gateway and the client each refuse
a non-loopback `http://` `CELLA_URL` unless that choice is explicit", this
spec's number in the `Spec` cell, and this spec's own tests in the `Test`
cell. The asset table's row for the control plane's configuration gains
`CELLA_PUBLIC_TLS_CERT_FILE`, `CELLA_PUBLIC_TLS_KEY_FILE` and
`CELLA_CA_BUNDLE` beside `CELLA_TOKEN_KEY`. Neither edit lands with this
spec at `drafted`; both are made when this spec is implemented, together
with its own acceptance criteria, as [[013-security-and-threat-model]]'s
own convention already asks of the specs that own its rows.

## Not in this spec

- **mTLS.** The bearer already identifies every caller; a plane that wants
  a second, transport-level identity in front of it has a mesh in front
  of its sandboxes already ([[022-mesh-and-spawn]]) and gains nothing by
  asking `cellad`'s own listener to check a client certificate too.
- **TLS on the internal listener.** It answers the installation's own
  orchestrator, not a caller.
- **The deploy manifests that mount the certificate as a Secret.** That is
  [[014-release-and-installation]]'s to add once this spec ships.
- **Moving the reloading loader into `latere.ai/x/pkg`.** `pkg` stays lean
  until a second consumer needs the same loader; today only `cellad` does.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The public listener answers `/version` over TLS 1.2 or later when `CELLA_PUBLIC_TLS_CERT_FILE` and `CELLA_PUBLIC_TLS_KEY_FILE` are both set, and over plain HTTP when neither is | `TestPublicListenerServesTLS` | not built |
| One of the two files set and not the other is a start-up failure naming both | `TestTLSRequiresBothFiles` | not built |
| The internal listener answers `/metrics` and the probes over plain HTTP whether or not the public listener's TLS files are set | `TestInternalListenerStaysPlain` | not built |
| A handshake after the certificate and key files are replaced by an atomic rename serves the new certificate, with no restart and no dropped connection in flight | `TestTLSReloadsOnFileChange` | not built |
| A pair that fails to parse, or whose certificate and key do not match, is logged once and the last good pair keeps serving every handshake after it | `TestTLSKeepsLastGoodPairOnReloadFailure` | not built |
| The server never negotiates `h2` over ALPN | `TestTLSALPNIsHTTP1Only` | not built |
| `CELLA_PUBLIC_URL` that is not `https://` and not loopback is a start-up failure when both TLS files are set | `TestPublicURLMatchesListenerScheme` | not built |
| `cellad check` reports the configured pair's expiry, and names a pair it cannot parse | `TestCheckReportsCertificateExpiry` | not built |
| `client.Environment` reads `CELLA_CA_BUNDLE` into `Config.RootCAs` beside the system roots, and `client.New` refuses a bundle that holds no certificate before the first call | `TestClientTrustsPrivateCA` | not built |
| The worker's registration and stream, and the gateway's own stream to the control plane, each trust `CELLA_CA_BUNDLE`, distinct from `CELLA_EGRESS_CA_BUNDLE` | `TestWorkerTrustsPrivateCA`, `TestGatewayStreamTrustsPrivateCA` | not built |
| A client configured with `CELLA_CA_BUNDLE` completes a full create, exec and delete against a `cellad` serving TLS from a certificate the bundle, and no public root, chains to | `TestClusterTLSEndToEnd` | not built |
