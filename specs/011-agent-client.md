---
title: "Agent client: the cella command, its client package, exit codes, output, the skill"
status: validated
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-manifest-contract.md
  - specs/006-identity.md
  - specs/008-api.md
  - specs/018-egress-and-secrets.md
  - specs/019-volumes.md
  - specs/020-scheduling-and-sets.md
  - specs/021-data-plane-workers.md
  - specs/023-computer-use-operations.md
affects: [cmd/cella/, internal/cellacli/, internal/cellaclient/, skills/cella/, docs/cli.md, internal/config/]
effort: medium
created: 2026-09-12
updated: 2026-09-20
author: changkun
---

# Agent client

## Overview

`cella` is one binary that speaks the `/v1` API from a shell or from an
agent, including an agent running inside a sandbox: apply a manifest of
any kind, list, inspect, exec, attach, forward a port, copy files,
follow logs and events, take a screenshot, drive input, manage secrets,
volumes, snapshots, sets, and environments. It has one exit-code
scheme, JSON output that is the API's bytes, streams that are never
buffered, and a skill file that teaches an agent the command. It is
the client the conformance suite and the documentation use, so the API
is never exercised only through `curl`.

## Current state

Not built. A platform's own CLI may wrap or replace it; this one is the
control plane's and knows only the control plane's API.

## Design

### Reaching the control plane

Every command reads `CELLA_URL` and one of `CELLA_TOKEN` or
`CELLA_TOKEN_FILE`, overridable by `--url`, `--token`, `--token-file`,
and nothing else from the environment: no config file and no login,
because a token comes from the caller's issuer, or, inside a sandbox,
from the projection. Inside a sandbox `CELLA_URL` is injected by the
control plane ([[004-runtime-contract]]) and the token is at
`/run/cella/token`, the default of `--token-file` when `CELLA_TOKEN` is
unset; the file is read per request, never once at start, because the
controller re-projects it before expiry and a `logs -f` must outlive
one token. `internal/cellaclient` builds its transport with `Proxy: nil`
and reads no proxy or trust-store variable, so a `cella` run inside a
sandbox reaches `CELLA_URL` directly through the driver's rule rather
than through the egress gateway, whose allow list has no entry for the
control plane ([[018-egress-and-secrets]]).

### Commands

One verb set for every kind. `<ref>` is a name or a prefixed id, with
the alias rule of [[008-api]]. `<kind>` is `sandbox`, `secret`,
`volume`, `set`, `environment`, singular or plural.

| Command | Route | Notes |
|---|---|---|
| `cella apply -f <file> [-w] [--if-match <etag>]` | `PUT /v1/<kinds>/{name}`; `POST /v1/sandboxes` for a `Sandbox` with no name | reads `apiVersion` and `kind` from the file; `-w` waits for `Running`, `Available`, `Ready`, or the set's `Succeeded`; a `version_conflict` is exit 5 |
| `cella get <kind> [<ref>] [-o json|yaml|wide|name] [-l k=v]... [--phase] [--owner] [--environment] [--root <id>] [--limit n]` | `GET /v1/<kinds>[/{id}]` | one object or a list; a list follows `next` to the end unless `--limit` stops it |
| `cella delete <kind> <ref>` | `DELETE` | 202 or 200 is exit 0 |
| `cella start <ref>`, `cella stop <ref>` | `POST .../start`, `.../stop` | a sandbox; `cella stop set <ref>` is `POST /v1/sandboxsets/{id}/stop` |
| `cella exec <ref> [-i] [-t] [--timeout d] -- <cmd> [args...]` | `POST .../exec` without `-i` or `-t`; the exec WebSocket with either | stdout and stderr to the caller's, the child's exit code as the exit |
| `cella attach <ref> [-- <cmd>]` | the attach WebSocket | raw terminal, `SIGWINCH` to resize, restored on exit |
| `cella port-forward <ref> <local>:<port>` | the dial WebSocket per accepted connection | binds `<local>` on loopback |
| `cella cp <ref>:<src>... <dest>`, `cella cp <src>... <ref>:<dest>` | `GET .../files?path=` repeated; `PUT .../files?dest=` | tar streamed, no temporary file |
| `cella files ls\|stat\|get\|put\|mkdir\|rm\|mv <ref>:<path> [...]` | the granular file routes | one operation on one path, which is what a caller wants when the tree is large and the change is small; the routes are [[033-file-operations]]'s and postdate this table |
| `cella logs <ref> [-f] [--since t] [--tail n]` | `GET .../logs` | |
| `cella events <ref> [-f]`, `cella events --object <id> [-f]` | `GET /v1/sandboxes/{id}/events`; `GET /v1/events?object=` | any kind by id |
| `cella egress <ref>` | `GET .../egress` | the gateway's records |
| `cella token <ref>` | `POST .../token` | the token on stdout, nothing else |
| `cella screenshot <ref> [-o file] [--format png|jpeg] [--scale s]` | `GET .../screenshot` | PNG bytes to the file or stdout |
| `cella screen <ref> [--fps n]` | the screen WebSocket | frames as a PNG stream to stdout, for a viewer to read |
| `cella input <ref> -f events.json` | `POST .../input` | |
| `cella display <ref>` | `GET .../display` | |
| `cella snapshot create <volume-ref>`, `list <volume-ref>`, `delete <volume-ref> <snp_ id>` | the snapshot routes | |
| `cella env key create <ref>`, `revoke <ref> <jti>` | the key routes | the key on stdout once |
| `cella version` | `GET /version` | client and, when reachable, server identity |

`cella apply` of a `Secret` reads `spec.value` from the file, from
`--value-from-env <NAME>`, or from `--value-file <path>`, and never
writes it to stdout or stderr.

### Exit codes

One scheme, by the response's status class with named exceptions, so a
code the client does not know still has an exit:

| Exit | Outside `exec` | Under `exec` |
|---|---|---|
| 0 | success | the child exited 0 |
| 1 to 124 | 1: a 5xx, including every `*_unavailable` | the child's own exit code; 124 is the server's timeout |
| 2 | a usage error: flags, a file that does not parse | the same |
| 3 | refused: 400, 401, 403, 413, 415, 422, 429 | as 125 |
| 4 | 404 | as 125 |
| 5 | 409, `version_conflict` and `phase_conflict` included | as 125 |
| 7 | the server unreachable: dial, TLS, or timeout before a status | as 127 |
| 125 | | any client-side failure that outside `exec` would be 1, 3, 4, or 5 |
| 126 | | the command could not start: `capability_unsupported` for `-i` or `-t`, or an error frame before the first byte |
| 127 | | the server unreachable |

A refusal prints the API's `message` on stderr as one line; with `-v`
the `code`, the `details`, and the request id follow on their own
lines. A 429 prints `Retry-After` in the line. `CELLA_TOKEN` and the
token file's contents reach no stdout or stderr byte, `-v` included.

```
$ cella get sandboxes
NAME   PHASE    READY  IMAGE                          AGE  OWNER
dev    Running  True   ghcr.io/example/sandbox:1.4    41m  https://login.example.com|alice
$ cella apply -f sandbox.yaml
This field cannot be changed after the object is created.
$ cella apply -f sandbox.yaml -v
This field cannot be changed after the object is created.
code: immutable_field
paths: spec.image
request: req_01J9...
```

### Output

| Kind | Default columns | `wide` adds |
|---|---|---|
| Sandbox | `NAME`, `PHASE`, `READY`, `IMAGE`, `AGE`, `OWNER` | `ID`, `ENVIRONMENT`, `DRIVER`, `MESH`, `EXPIRES` |
| Secret | `NAME`, `KIND`, `HOSTS`, `VERSION`, `MOUNTED`, `OWNER` | `ID`, `UPDATED` |
| Volume | `NAME`, `PHASE`, `SIZE`, `ACCESS`, `ATTACHED`, `OWNER` | `ID`, `ENVIRONMENT`, `CLASS`, `SNAPSHOTS` |
| SandboxSet | `NAME`, `PHASE`, `PENDING`, `RUNNING`, `SUCCEEDED`, `FAILED`, `OWNER` | `ID`, `ENVIRONMENT`, `PARALLELISM` |
| Environment | `NAME`, `PHASE`, `MODE`, `DRIVER`, `WORKERS`, `USED` | `ID`, `ISOLATION`, `CAPACITY` |

`-o name` prints `<kind>/<name>` per line. For one object `-o json`
and `-o yaml` write the response body as received, never decoded and
re-encoded, so field order and bytes are the API's; for a list that
spanned pages, `items` of every page are concatenated into one
envelope with `next` empty, and each item's bytes are unchanged.
`-o yaml` for a list asks the server with `Accept`.

### Streams

`exec` without `-i` or `-t` reads the framed response, writes channel
1 to stdout and 2 to stderr as frames arrive, and exits with the exit
frame; an error frame is exit 126 before any byte and 125 after. With
`-i` or `-t` it opens the exec WebSocket, sends the request as the
first text frame, pumps stdin, and exits with the `{"exit": n}` frame.
`attach` puts the terminal in raw mode, restores it on any exit path,
and sends `{"resize"}` on `SIGWINCH`. `port-forward` accepts on
`<local>`, opens one `cella.dial.v1` WebSocket per connection, and
copies bytes both ways. `cp` streams tar from `GET` to disk and from
disk to `PUT` without a temporary file. `logs -f` and `events -f`
write each line as it arrives. Nothing is buffered beyond one frame.

### Client policy

No retry, ever: a caller that wants one has the exit code. A 10 second
deadline to the first response byte, none on a stream. Every request
carries a fresh `X-Request-Id`, printed under `-v`, and
`User-Agent: cella/<version>`. `RateLimit-Remaining` is not read;
`Retry-After` is reported. On `unsupported_version` the refusal line is
followed by the server's `/version`, so a caller sees the skew; a
response carrying fields the client does not know is rendered without
them, so a newer server is usable from an older client.

### The client package

`internal/cellaclient` is the typed client the command uses, one method
per route. Streams are typed: `Exec` returns `{Stdin io.WriteCloser,
Stdout, Stderr io.Reader, Resize(cols, rows int) error, Wait(ctx)
(int, error)}`, `Attach` the same with one output, `Dial` an
`io.ReadWriteCloser`, `Screen` a channel of frames. The WebSocket
client is the package's own RFC 6455 implementation over `net/http`'s
hijack, so `./cmd/cella` reaches the standard library and
`latere.ai/x/pkg/httpjson`'s envelope and nothing else, recorded as a
`depcheck` row in `.lateregate.yaml` ([[002-repository-scaffold]]). The
package is internal because an importer building on the packages has
no HTTP hop; a platform with its own client generates one from the
OpenAPI document.

### The skill and the document

`skills/cella/SKILL.md` has `name` and `description` frontmatter of at
most 256 bytes together, the resident cost; the body teaches the two
variables and the in-sandbox defaults, a minimal manifest, `apply`,
`exec`, `cp`, `delete`, `get`, the exit table, and how to read a
refusal with and without `-v`. The claim to test is reachability: an
agent given only the skill completes the agent scenario of
[[015-conformance-suite]]. `docs/cli.md` is the command table above in
the user register, and a test holds it equal to the binary's `--help`.

## Not in this spec

Distribution of the binary ([[014-release-and-installation]]); the
scenario that drives it ([[015-conformance-suite]]); the routes'
semantics ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every command in the table calls the route in its row with the method, addressing, and flags named; `apply` dispatches on `kind` and sends `--if-match` | `TestCommandTable` against an `httptest` server | passing for the rows [[050-cella-command]] built, as `TestEveryCommandCallsTheRouteOfItsRow`; `--if-match` waits on the `PUT` apply of [[008-api]], which this API does not serve |
| Every error code of [[008-api]] and every status class maps to the exit in the table, outside and under `exec`; an unknown code maps by class | `TestErrorsBecomeExits`, table-driven over every code | passing, as `TestEveryErrorCodeBecomesItsExit` and `TestTheExitsThatAreNotARefusal` ([[050-cella-command]]) |
| The built binary carries the exit codes through the process, and `exec` passes the child's code through unchanged for 0, 3, 7, and 124 | `TestBinaryExitCodes` running `out/cella` | passing through the binary's own entry point, as `TestMainIsTheEntryPoint` and the child's codes in `TestTheExitsThatAreNotARefusal` ([[050-cella-command]]) |
| With `HTTPS_PROXY` set to a refusing address, `cella` still reaches `CELLA_URL` | `TestClientIgnoresProxyVariables` | passing, as `TestTheClientIgnoresProxyVariables` ([[050-cella-command]]) |
| With `CELLA_TOKEN` unset and a token file that changes between two requests, each request sends the file's current bytes | `TestTokenFileIsReadPerRequest` | passing, as `TestTheTokenFileIsReadPerRequest` ([[050-cella-command]]) |
| `CELLA_TOKEN`, the token file, and a secret's value reach no stdout or stderr byte, under `-v` included | `TestSecretsAreNeverWritten` with canaries | passing, as `TestNoTokenReachesAnOutput` and `TestASecretValueIsPlacedInTheDocumentAndPrintedNowhere` ([[050-cella-command]]) |
| `-o json` and `-o yaml` for one object are byte-identical to the response; a two-page list is one envelope with every item's bytes unchanged and `next` empty | `TestOutputFidelity` | the JSON half passes, as `TestOneObjectUnderJSONIsTheAPIsOwnBytes` and `TestAListThatSpannedPagesIsOneEnvelope` ([[050-cella-command]]); `-o yaml` waits on a handler that reads `Accept` |
| Each output row in the columns table renders as stated for each kind; `-o name` and `-o wide` do | `TestColumns` | passing for the two kinds this API serves, as `TestTheOutputsAreWhatTheGoldenFilesHold` ([[050-cella-command]]) |
| `exec` writes channel 1 and 2 to the right descriptors as frames arrive, 64 MiB without buffering; `-i` pumps stdin; `attach` enters and restores raw mode and sends a resize; `port-forward` carries bytes both ways; `cp` streams both ways | `TestStreams` | the socket half passes, as `TestExecWithInputIsTheSocket`, `TestATerminalSessionSetsRawModeAndRestoresIt` and the transfers of [[050-cella-command]]; the framed `POST` stream waits on the handler, which serves `?wait=1` only, and `port-forward` on the dial route |
| A list follows `next` to the end and stops at `--limit`; every selector flag becomes its query parameter | `TestListPagingAndSelectors` | passing, as `TestAListFollowsTheCursorAndCarriesTheSelectors` and `TestALimitStopsTheListAndNeverAsksPastTheCeiling` ([[050-cella-command]]) |
| The three output examples above are what the binary prints | `TestExamplesAreExact` | the forms pass as golden files ([[050-cella-command]]); the examples themselves carry an `OWNER` and an id of one installation and are not compared byte for byte |
| `unsupported_version` prints the server identity after the refusal; unknown fields render without error | `TestVersionSkew` | the unknown-field half passes, as `TestAFieldTheServerLeftEmptyIsADash` ([[050-cella-command]]); printing the server's identity after that one refusal is not built |
| The skill's frontmatter is under 256 bytes and an agent given only the skill completes the agent scenario | `TestSkillFrontmatterIsSmall`, conformance case `case011AgentScenario` | the bound passes, as `TestTheSkillIsSmallEnoughToBeResident` ([[050-cella-command]]); the scenario waits on [[015-conformance-suite]] |
| `docs/cli.md` equals the binary's `--help` for every command | `TestCLIDocIsCurrent` | passing, as `TestTheDocumentCarriesTheCommandsHelp` ([[050-cella-command]]) |
| `./cmd/cella`'s build list is the standard library plus `pkg/httpjson` | the `depcheck` gate | passing: the list is the standard library, `latere.ai/x/pkg/httpjson` and the `github.com/google/uuid` it reaches ([[050-cella-command]]) |
