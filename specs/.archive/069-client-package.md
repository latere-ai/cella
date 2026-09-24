---
title: "Client package: the typed /v1 client exported as latere.ai/x/cella/client, with the calls a consumer outside this module needs"
status: complete
track: core
depends_on:
  - specs/008-api.md
  - specs/009-events.md
  - specs/011-agent-client.md
  - specs/021-data-plane-workers.md
  - specs/031-hosted-sandbox-consolidation.md
  - specs/.archive/050-cella-command.md
  - specs/.archive/055-api-contract-gaps.md
  - specs/.archive/066-events-follow.md
affects: [client/, internal/cellaclient/, internal/cellacli/, cmd/cella/, test/conformance/, cmd/cellad/, arch_test.go, docs/, CHANGELOG.md]
effort: medium
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# Client package

## Overview

Two programs outside this module speak the core's `/v1`: a sandbox
provider that creates, drives and deletes sandboxes for its agents, and
a command line that manages them for a person. The typed client they
need exists, one method per route, the error envelope decoded, the exec,
attach and dial sockets over the package's own WebSocket framing. It
lives at `internal/cellaclient`, which Go refuses to let another module
import, so each consumer would write its own.

This slice moves it to `latere.ai/x/cella/client`, gives it the options
a library caller supplies rather than the ones a command reads from its
environment, and adds the calls the command never needed: apply by
name in JSON or YAML, the environments and their keys, and the events
feed, one page or followed.

## Current state

| Piece | Where it is | What is missing for a consumer |
|---|---|---|
| The client | `internal/cellaclient` | importable only inside this module |
| Its configuration | `Config`: `URL`, `Token`, `TokenFile`, `CAFile`, `UserAgent`, `Getenv` | reads `CELLA_URL` and `CELLA_TOKEN` whenever a field is empty; no token source a caller computes, no HTTP client of the caller's |
| Sockets | `dialConn`: its own TCP and TLS dial | a caller's transport, proxy or instrumentation does not reach them |
| Create | `CreateSandbox(body)`, always `application/json` | no apply by name (`PUT /v1/sandboxes/{name}`), no YAML body, which the server takes |
| Kinds | sandbox and secret | no environment calls, no key mint or revocation |
| Events | none | no page of `GET /v1/events`, no following feed |
| The error | `Error`: status, code, message, request id, paths, detail, `Retry-After` | the envelope's `details` object is not kept whole |
| Users | `internal/cellacli`, `cmd/cella`, `test/conformance` | none outside |

## Design

### One implementation, moved

`internal/cellaclient` moves to `client/` with its tests, package name
`client`. `internal/cellacli`, `cmd/cella` and `test/conformance` import
it; no package stays behind under `internal/`. What is the command's and
not the client's moves the other way: the singular or plural kind names
a person types (`ParseKind`, the list of kinds the command serves) and
the reading of a certificate authority file into a pool are
`internal/cellacli`'s.

### Configuration

```go
client.New(client.Config{
    URL:        "https://cella.example.com", // required, http or https
    Token:      client.StaticToken(token),   // or TokenFile, or a TokenFunc
    HTTPClient: nil,                         // nil: the package's transport
    RootCAs:    nil,                         // the package's transport only
    UserAgent:  "provider/1.2",
})
```

| Field | Meaning |
|---|---|
| `URL` | the control plane's base address; nothing is read in its place |
| `Token` | a `TokenSource`, asked once per request with the request's context; nil sends no bearer |
| `HTTPClient` | carries every call, the sockets included; nil is a transport with no proxy, a 10 second dial and TLS handshake, and 10 seconds to the first response byte |
| `RootCAs` | the authorities the package's transport trusts; nil is the system roots |
| `UserAgent` | the identity every request carries; empty is `cella-client` |

`StaticToken(s)` is a fixed bearer. `TokenFile(path)` reads the file on
every request and trims it, because a projected token is rewritten
before it expires and a followed stream outlives one token; a file that
cannot be read or holds nothing is a `*NoBearer`. `TokenFunc` adapts a
function, which is what a caller with its own issuer passes.

`Environment(getenv)` is design 011's reading of the environment as a
`Config`: `CELLA_URL`, then `CELLA_TOKEN`, then the file
`CELLA_TOKEN_FILE` names, then the projected token at
`DefaultTokenPath`. It is called, never implied, so a library caller's
configuration is what it wrote. Code running inside a sandbox writes
`client.New(client.Environment(os.Getenv))`; the command passes a getenv
its flags override, which keeps the precedence design 011 states.

### Sockets over the caller's client

The exec, attach and dial sockets send their upgrade through the same
`http.Client` as every other call. The transport keeps an upgrade on
HTTP/1.1, and a `101` answer's body is the connection, which the
package's RFC 6455 framing reads and writes as before. A caller's
proxy, dialer, trust and instrumentation therefore reach every call.
An `http.Client` with a `Timeout` bounds the whole exchange, streams
included, and hands back a body that cannot be written; a socket over
such a client is refused with a sentence that says so. A caller bounds
calls with the context instead.

### Manifests in either syntax

`Manifest` is a document and its media type. `JSON(body)` and
`YAML(body)` name the syntax; `Encode(obj)` marshals a typed object to
JSON. The server decodes both syntaxes to one object ([[055-api-contract-gaps]]),
so the client sends the caller's bytes unchanged.

| Call | Route |
|---|---|
| `CreateSandbox(ctx, m)` | `POST /v1/sandboxes` |
| `ApplySandbox(ctx, name, m)` | `PUT /v1/sandboxes/{name}` |
| `CreateSecret(ctx, m)`, `ApplySecret(ctx, name, m)` | `POST /v1/secrets`, `PUT /v1/secrets/{name}` |
| `CreateEnvironment(ctx, m)`, `ApplyEnvironment(ctx, name, m)` | `POST /v1/environments`, `PUT /v1/environments/{name}` |

### The rest of the surface

| Call | Route |
|---|---|
| `GetSandbox`, `GetSecret`, `GetEnvironment` | `GET /v1/<kinds>/{ref}` |
| `ListSandboxes`, `ListSecrets`, `ListEnvironments` with `ListOptions` | `GET /v1/<kinds>`, following `next` to the end or to `Limit` |
| `GetAs`, `ListAs` | the same routes under an `Accept` the caller names, bytes unchanged |
| `StartSandbox`, `StopSandbox` | `POST /v1/sandboxes/{ref}/start`, `.../stop` |
| `Delete(ctx, kind, ref)` | `DELETE /v1/<kinds>/{ref}` |
| `Exec` | `POST .../exec?wait=1` |
| `ExecSession`, `AttachSession` | the exec and attach sockets |
| `Logs` | `GET .../logs` |
| `ExportTar`, `ImportTar`, `FileList`, `FileStat`, `FileGet`, `FilePut`, `FileMkdir`, `FileRemove`, `FileMove` | the file routes |
| `Dial` | the dial socket; a port forward is one `Dial` per accepted connection |
| `EgressRecords` | `GET .../egress` |
| `MintEnvironmentKey`, `RevokeEnvironmentKey` | `POST /v1/environments/{ref}/keys`, `DELETE .../keys/{jti}` |
| `Events(ctx, object, EventOptions)` | `GET /v1/events?object=`, one page |
| `FollowEvents(ctx, FollowOptions)` | `GET /v1/events?follow=1`, one object's from a cursor or every readable object's from now |
| `ServerVersion` | `GET /version` |

A call that answers one object returns the decoded object and the
response's own bytes, so a caller that forwards the API's JSON has it.
`Event` carries the record's wire fields and its line's bytes. An
`EventStream` returns one record per `Next`, passes over the empty
heartbeat lines, returns `io.EOF` where the feed closed (after the
object's delete record, for one object), and an `*Error` where the
feed's last line is an error envelope.

### The error

`Error` keeps what it has and adds `Details`, the envelope's `details`
object as decoded, so a code whose details carry more than the paths,
the detail and the request id reaches the caller whole. `Unreachable`,
`StreamError`, `NoBearer` and `CodeOf` keep their meaning.

### The build list

`client` reaches the standard library, `manifest/v1`, and the envelope
of `latere.ai/x/pkg/httpjson` with the `github.com/google/uuid` that
package reaches. Nothing under `internal/` and nothing of the server: a
consumer that imports it builds no store, driver, or policy. A test
reads `go list -deps` and holds the list to exactly that.

### Coordinates

No Latere hostname in the package, its tests or its page. The base
address is always the caller's; the examples use `example.com`.

## Not in this slice

The display calls (`screenshot`, the screen socket, `input`,
`display`), the framed exec stream, the environment key listing, and
`If-Match` on an apply: no consumer named them, and each is one method
over a route that already exists. The command reading YAML in `cella
apply`, which is the command's decision and not the client's.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The package's build list is the standard library, `manifest/v1`, `pkg/httpjson` and `uuid`, and nothing under `internal/` | `TestTheClientPackageReachesNoServer` | built |
| The command is built on the exported package, and no copy stays under `internal/` | `TestTheCommandSpeaksThroughTheExportedClient` | built |
| `New` refuses a missing or non-http address; a nil token source sends no bearer; a token source is asked per request with the request's context; a token file is read per request | `TestABadConfigurationIsRefusedAtOnce`, `TestATokenSourceIsAskedPerRequestWithTheCallersContext`, `TestTheTokenFileIsReadPerRequest` | built |
| `Environment` reads the address and the bearer in design 011's order, with the projected token last | `TestTheAddressAndTheBearerComeFromTheEnvironment`, and on the command `TestTheFlagsOverrideTheVariables` | built |
| A caller's `http.Client` carries the calls and the sockets; one that cannot hand over an upgrade is refused with a sentence | `TestTheCallersHTTPClientCarriesEveryCall` | built |
| The package's transport reads no proxy variable and trusts the authorities the caller named | `TestTheClientIgnoresProxyVariables`, `TestTLSUsesTheSystemRootsAndTheAuthorityTheCallerNamed`, and on the command `TestTheCertificateAuthorityFlagIsReadBeforeACall` | built |
| A refusal decodes to the envelope's code, message, details, request id and paths | `TestARefusalDecodesToTheEnvelope` | built |
| A manifest travels in its own syntax, by create or by apply under a name | `TestAManifestTravelsInItsOwnSyntax` | built |
| Every object call reaches its route, start and stop and the three kinds' reads and deletes included | `TestTheRoutesOfEveryObjectCall` | built |
| Environments list, read, apply and delete, and a key is minted and revoked | `TestEnvironmentsAndTheirKeys` | built |
| A page of events is one object's history with its cursor | `TestAnEventPageIsOneObjectsHistory` | built |
| A followed feed hands over each record with its bytes, passes over heartbeats, ends at the close, and returns the error line as an `Error` | `TestAFollowedFeedReadsRecordsAsTheyArrive`, `TestAFeedThatEndsOnAFailureIsTheError` | built |
| The documented use compiles | `ExampleNew`, `ExampleEnvironment`, `ExampleClient_FollowEvents` | built |
| Against a running node, the exported client applies a YAML manifest, execs, moves a file, follows the sandbox's records and deletes it | `TestTheExportedClientDrivesARunningNode` | built |
| No Latere coordinate in the package or its page | `TestNoLatereCoordinatesInReleasedArtifacts` | built: the check walks every tracked file |

## Outcome

The client is `latere.ai/x/cella/client`, and it is the only one: the
command, the conformance suite and the running-node test import it, and
nothing stays under `internal/`.

| Piece | Where |
|---|---|
| `Config`, `New`, the package's transport, the request plumbing | `client/client.go` |
| `TokenSource`, `StaticToken`, `TokenFile`, `TokenFunc`, `NoBearer`, `Environment` | `client/token.go` |
| `Manifest`, `JSON`, `YAML`, `Encode` | `client/manifest.go` |
| Sandbox, secret and environment calls, the key mint and revocation | `client/sandboxes.go`, `client/secrets.go`, `client/environments.go` |
| `Events`, `FollowEvents`, `EventStream` | `client/events.go` |
| The socket upgrade through the caller's `http.Client` | `client/websocket.go` |
| `Error.Details` | `client/errors.go`, `client/session.go` |
| The command's reading of flags over variables, `--ca`, and the kinds it serves | `internal/cellacli/cli.go`, `internal/cellacli/objects.go` |
| The build-list tests | `arch_test.go` |
| The page for consumers | `docs/client.md` |

Coverage on the cover gate: `client` 95.0%, `internal/cellacli` 90.6%,
`cmd/cellad` 90.7%. The end-to-end that ran is
`TestTheExportedClientDrivesARunningNode`: `cellad serve` on loopback,
a YAML manifest applied under a name, a label selection, a waited exec
and a socket session, a file written and read back, the sandbox's
records followed from the newest one read to the delete record and the
end of the feed, and an environment key minted and revoked.

### What diverges from the specs above

| Spec | What it said | What was built | Why |
|---|---|---|---|
| [[011-agent-client]] | the package is internal, because an importer building on the packages has no HTTP hop and a platform generates its own client from the OpenAPI document | the package is exported as `client` | two consumers outside this module speak `/v1` over HTTP, and a generated client would be a second implementation of the sockets and the envelope; 011 is amended |
| [[011-agent-client]] | the WebSocket is dialed by the package over `net/http`'s hijack, with the one deadline on the handshake | the upgrade is a request through the caller's `http.Client`, whose transport hands back the connection; the package's own transport keeps the 10 second first-byte deadline | a caller's proxy, dialer, trust and instrumentation reach the sockets as well as the calls, and the separate dial and TLS setup are gone |
| [[011-agent-client]] | the client reads `CELLA_URL` and the token variables | `New` reads nothing; `Environment` reads them when called, and the command calls it | a library caller's configuration is what it wrote |
| this spec | `Config` as listed | `RootCAs` replaces the certificate authority file, and the command reads `--ca` into a pool | a library caller holds a pool, not a path |
| this spec | the calls as listed | `After(seq)` builds a follow cursor from a record's sequence | a page's `Next` points the other way, and a cursor from `Seq` is the one a follower resumes from |

### Unspecified work

A reference that needs escaping in a URL path was escaped twice: the
client put the escaped segment into the URL's decoded path, which
escaped it again, so the server looked up a name with `%20` in it rather
than one with a space. The path is now set in both forms. The row of
`TestTheRoutesOfEveryObjectCall` that sends such a reference fails
without the fix.

### What this leaves open

| Open | Why |
|---|---|
| The display calls, the framed exec stream, `If-Match` on an apply | no consumer named them; each is one method over a route that exists |
| The environment key listing | its route lands with another slice; the call follows it |
| `cella apply` of a YAML file, `cella get environments`, `cella events` | the command's decisions, not the client's; the client already has each call |
