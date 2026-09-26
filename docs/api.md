# The API

For whoever calls a Cella control plane from a program. The `cella`
command is built on the same routes; [The cella command](cli.md) is the
shorter way from a shell.

Every route below is under the control plane's public URL,
`CELLA_PUBLIC_URL`. When that URL has a path, the control plane is served
under it and the path takes the place of `/v1`: with
`https://api.example.com/v1/environments`, `/v1/sandboxes` is
`https://api.example.com/v1/environments/sandboxes`, and the key set and
the OpenAPI document are under the path as well. The Go client, `cella`
and the conformance suite compose every route that way from the URL they
are given. The authoritative description of the surface is the
OpenAPI 3.1 document every installation serves at `GET /openapi.yaml`
with no credential, and which this repository carries as
[`api/openapi.yaml`](../api/openapi.yaml). This page is the orientation
around it: how to authenticate, how objects are addressed, what a refusal
looks like, and what each route is for. [Manifests](manifest.md) is the
reference for the bodies.

## Authentication

Every `/v1` request carries `Authorization: Bearer <token>`. Three kinds
of token are accepted:

| Token | Minted by | Reaches |
|---|---|---|
| a caller's token | an issuer listed in `CELLA_OIDC_ISSUERS`, with an audience in `CELLA_OIDC_AUDIENCE` and minted less than 24 hours ago | whatever your authorizer allows the subject |
| a workload token | `cellad`, for each sandbox, at `/run/cella/token` inside it | reading and running commands in its own sandbox, reading the sandboxes below it, and creating a child within its spawn budget |
| an environment key | `cellad`, through `POST /v1/environments/{id}/keys` | the worker and gateway routes of that one environment |

There is no anonymous access and no API key. A token `cellad` minted is
verifiable offline against `GET /.well-known/jwks.json`.

After the token is verified, every request is authorized before anything
acts: the control plane asks your authorization endpoint, or the built-in
owner policy, whether the subject may take the route's action on the
object. A deny is `403 forbidden`. A name resolves only among your own
objects, and an object a manifest names that you may not use, a secret to
mount or an environment to run on, answers `404 not_found` exactly as one
that does not exist, so a manifest cannot be written to find out what
somebody else owns. The action each route asks for is in the tables below and in the route's description
in the OpenAPI document; [`latere.ai/x/cella/authorizer`](../authorizer)
publishes them as constants.

## Requests and answers

**Media types.** A manifest may be sent as `application/json`, or as YAML
under `application/yaml`, `application/x-yaml`, or `text/yaml`, one
document per request. `Accept` chooses the syntax of an answer: JSON by
default, YAML when you ask for it, and `406 not_acceptable` when you ask
for neither. A field the schema does not know is refused with the path it
sits at.

**Addressing.** `{id}` in a path is an object's prefixed identifier, or a
name among your own objects: `/v1/sandboxes/dev` and
`/v1/sandboxes/<status.id>` reach the same sandbox. Secret ids start
`sec_` and environment ids `env_`; an environment's name is its id in a
path.

**Lists.** `GET /v1/sandboxes` answers `{"items": [...], "next": "..."}`.
Pass `next` back as `cursor` for the next page; an empty `next` is the
end. `limit` is 1 to 200, 50 by default. It filters by `label=key=value`
(repeatable), `phase`, `owner`, `environment`, and `root`, which returns
one spawn tree. `GET /v1/secrets` pages the same way and filters by
`label` and `owner`. A filter narrows what you may already see and never
widens it: an `owner` whose objects you may not read answers an empty
page, not an error.

A sandbox list reads each sandbox from its environment. When one
environment does not answer, the page still answers every sandbox: a row
that could not be read carries the status last recorded and the condition
`Observed` with `status` `False` and the reason `DriverUnavailable`, or
`EnvironmentNotHeld` when the server holds no environment of that name.
`phase` filters by the phase the row carries. Reading that one sandbox by
id is refused with `driver_unavailable` until its environment answers.

**Concurrency.** An environment read carries an `ETag`, and an apply that
sends `If-Match` with an older version is refused with `409
version_conflict`, so two administrators cannot silently overwrite each
other.

**Request ids.** Every answer carries `X-Request-Id`. Send your own and it
is carried through the answer, the events, and the logs; otherwise the
server makes one starting `req_`.

## Refusals

Every refusal is one JSON body:

```json
{
  "error": {
    "code": "invalid_field",
    "message": "A field has a value it cannot take.",
    "details": {"request_id": "req_...", "detail": "...", "paths": ["spec.command"]}
  }
}
```

Branch on `code`. `message` is a sentence to show a person and never
changes for a code. `details.paths` names every field at fault, and
`details.detail` is the developer's explanation. A stream that fails after
its first byte cannot change its status any more: it ends, and names the
code in the `X-Cella-Error` trailer.

| Code | Status | Means |
|---|---|---|
| `unauthenticated` | 401 | no bearer, or one that did not verify |
| `forbidden` | 403 | the authorizer refused the route's action |
| `not_found` | 404 | no such object, or one a manifest names that you may not use |
| `bad_request` | 400 | the request could not be read |
| `invalid_field` | 400 | a field has a value it cannot take |
| `missing_field` | 400 | a required field is absent |
| `unknown_field` | 400 | the manifest has a field the schema does not know |
| `exclusive_fields` | 400 | two fields that cannot be set together are set |
| `multi_document` | 400 | more than one manifest in one body |
| `unsupported_version` | 400 | an `apiVersion` other than `cella.latere.ai/v1beta1` |
| `unsupported_kind` | 400 | a kind this server does not serve |
| `reserved_prefix` | 400 | a name reserved for the control plane |
| `not_acceptable` | 406 | `Accept` names neither JSON nor YAML |
| `name_taken` | 409 | you already hold an object with this name |
| `phase_conflict` | 409 | the sandbox is not in a state that allows this |
| `version_conflict` | 409 | the object changed since you read it |
| `immutable_field` | 409 | the field cannot change after creation |
| `boundary_widened` | 409 | a sandbox tried to widen its own boundary |
| `secret_host_conflict` | 409 | two mounted secrets apply to the same host |
| `cursor_expired` | 410 | the feed no longer holds the records after that cursor |
| `body_too_large` | 413 | the body is over `CELLA_MAX_BODY_BYTES` or `CELLA_MAX_UPLOAD_BYTES` |
| `unsupported_media_type` | 415 | the body is in a type this route does not take |
| `capability_unsupported` | 422 | the environment cannot provide what the request or the manifest needs |
| `ceiling_exceeded` | 422 | a value is above what this server allows |
| `quota_exceeded` | 422 | you are at your sandbox limit |
| `admission_refused` | 422 | the admission step refused the manifest, with its reason in `detail` |
| `boundary_exceeded` | 422 | a child asked for more than its parent has |
| `spawn_budget_exhausted` | 422 | the sandbox has no spawn budget left |
| `secret_out_of_scope` | 422 | the secret does not cover the host it is used for |
| `environment_mismatch` | 422 | a worker does not match the environment it registered on |
| `rate_limited` | 429 | too many requests; for the event feed, too many open feeds |
| `upstream_unavailable` | 502 | nothing listens on the port, or the sandbox is not running |
| `authorizer_unavailable` | 503 | the authorization endpoint gave no decision; nothing was allowed |
| `admission_unavailable` | 503 | the admission endpoint gave no answer; nothing was created |
| `egress_gateway_unavailable` | 503 | the manifest's egress boundary needs a gateway and none is connected; connect one, or set `spec.network.egress.mode` to `open` with no denied host and no mounted secret |
| `driver_unavailable` | 503 | the environment's runtime did not answer |

## Routes

### Sandboxes

| Route | Action | What it does |
|---|---|---|
| `POST /v1/sandboxes` | `sandbox.create` | create a sandbox from a manifest; `201` with the sandbox `Pending` as soon as it is recorded, or held with `?wait=1` |
| `GET /v1/sandboxes` | `sandbox.list` | list the sandboxes you may read |
| `PUT /v1/sandboxes/{name}` | `sandbox.create` or `sandbox.update` | apply by name: create when the name is free, update when you hold it |
| `GET /v1/sandboxes/{id}` | `sandbox.read` | read one sandbox with its status |
| `DELETE /v1/sandboxes/{id}` | `sandbox.delete` | delete it, and every sandbox it created, in any phase |
| `POST /v1/sandboxes/{id}/start` | `sandbox.update` | start a stopped sandbox |
| `POST /v1/sandboxes/{id}/stop` | `sandbox.update` | stop a running sandbox and keep its workspace |

A create also asks `environment.use` on the environment it runs on, and
`secret.mount` on each secret it mounts.

**A create answers at once.** `POST /v1/sandboxes`, and `PUT
/v1/sandboxes/{name}` when it creates, answer `201` with `Location` and
the sandbox as soon as it is recorded, before its workload runs. The
sandbox is `Pending`; the control plane then pushes its boundary to a
gateway, mints its identity and asks the environment's runtime for it, and
the sandbox moves to `Running`, or to `Failed` with its `reason`. Read it,
or follow its events, to see that happen. On a runtime that provisions and
attaches storage before a workload starts, that can take tens of seconds,
and none of it is spent on your request. A sandbox the environment adopts
from its prewarmed pool answers already running, and one a queued
environment cannot place yet answers `Queued`, as before.

Everything that refuses a create still refuses it on the request, with
nothing recorded: a manifest the server rejects, a denied action, your
sandbox limit, a name you already hold, an environment that is not ready,
and a boundary that needs an egress gateway while none is connected.

**Holding the answer.** `?wait=1` holds the answer until the sandbox has
left `Queued`, `Pending` and `Starting`: it is `Running`, `Failed` with
its reason, or whatever phase the runtime reports after the create.
`timeout` bounds the hold, a duration such as `90s`, positive and at most
`1h`, `10m` when absent; when it passes first, the answer is the sandbox
as it stands. The status is `201` either way, because the sandbox exists
whatever became of its start. A sandbox deleted while the answer is held
answers `404 not_found`. Set your HTTP client's timeout above the hold.

A program that acts on the sandbox as soon as the create answers, running
a command or copying files into it, reads it until `Running` first, or
asks for `?wait=1`. A runtime that cannot make the sandbox leaves it
`Failed` with its `reason`, which is what a read, the events and a held
answer carry.

### Commands and terminals

| Route | Action | What it does |
|---|---|---|
| `POST /v1/sandboxes/{id}/exec` | `sandbox.exec` | run a command. With `?wait=1`, one JSON answer with `exitCode`, `stdout`, `stderr`, `truncated`, and `durationMs`. Without it, a stream as the command produces output |
| `GET /v1/sandboxes/{id}/exec` | `sandbox.exec` | a WebSocket for a command with input or a terminal |
| `GET /v1/sandboxes/{id}/attach` | `sandbox.exec` | a WebSocket carrying a pseudo-terminal: binary frames are bytes, `{"resize":{"cols","rows"}}` resizes, and the session ends with `{"exit": n}` |
| `GET /v1/sandboxes/{id}/logs` | `sandbox.read` | the main process's output, with `tail`, `since`, and `follow=1` |

The exec body is `{"command": [...], "env": {...}, "workdir": "...",
"timeout": "30s"}`. The stream without `?wait=1` is
`application/vnd.cella.exec-stream`: frames of one byte naming the
channel (1 standard output, 2 standard error, 3 the exit code, 4 an error
envelope), four bytes of big-endian length, and the payload, at most 1 MiB
each.

### Files

Every path is absolute and at or below the sandbox's workspace. A path
that leaves it, by traversal or through a link the workload planted, is
refused.

| Route | Action | What it does |
|---|---|---|
| `GET /v1/sandboxes/{id}/files` | `sandbox.exec` | read the named paths as one tar stream |
| `PUT /v1/sandboxes/{id}/files` | `sandbox.exec` | with `path`, write one file of any type; with `dest`, extract an `application/x-tar` body below that directory |
| `DELETE /v1/sandboxes/{id}/files` | `sandbox.exec` | remove a file or a tree |
| `GET /v1/sandboxes/{id}/files/content` | `sandbox.exec` | read one file's bytes |
| `GET /v1/sandboxes/{id}/files/stat` | `sandbox.exec` | describe one entry |
| `GET /v1/sandboxes/{id}/files/list` | `sandbox.exec` | list a directory, sorted by name |
| `POST /v1/sandboxes/{id}/files/mkdir` | `sandbox.exec` | make a directory and its missing parents |
| `POST /v1/sandboxes/{id}/files/move` | `sandbox.exec` | move a path to an exact destination |

A write is staged and renamed, so a body that ends early leaves the
previous file as it was.

### Ports

| Route | Action | What it does |
|---|---|---|
| `GET /v1/sandboxes/{id}/ports` | `sandbox.read` | each declared port, `listening` or `closed` |
| `/v1/sandboxes/{id}/ports/{name}/{path}` | `sandbox.exec` | forward any HTTP request, WebSocket upgrades included, to the port declared under `name`; your bearer is not passed on |; the path without its trailing slash answers `307` with a relative `Location`, so a proxy serving the core under a prefix keeps it |
| `GET /v1/sandboxes/{id}/dial/{port}` | `sandbox.exec` | a WebSocket with subprotocol `cella.dial.v1` whose binary frames are a TCP connection's bytes |

[Sandboxes on Kubernetes](kubernetes.md#reaching-a-port) is how each one
behaves and what closes a dial.

### The desktop

For a sandbox whose manifest asked for `display`.

| Route | Action | What it does |
|---|---|---|
| `GET /v1/sandboxes/{id}/display` | `sandbox.read` | the desktop's size and whether it is up |
| `GET /v1/sandboxes/{id}/screenshot` | `sandbox.exec` | one frame as PNG or JPEG, at a scale you choose |
| `GET /v1/sandboxes/{id}/screen` | `sandbox.exec` | a WebSocket of frames, up to ten a second; a viewer that falls behind skips frames |
| `POST /v1/sandboxes/{id}/input` | `sandbox.exec` | one batch of pointer and keyboard events, validated whole before the first runs |

No frame, keystroke, or typed character reaches an event or a log line.

### The boundary and events

| Route | Action | What it does |
|---|---|---|
| `GET /v1/sandboxes/{id}/egress` | `sandbox.read` | the connections the gateway recorded for the sandbox, newest first |
| `GET /v1/events` | the object kind's read | one object's records with `object=<id>`, newest first; with `follow=1`, a feed of new records as newline-delimited JSON |

[Following events](events.md) is the feed in full: cursors, heartbeats,
and what a proxy in front of the server must allow.

### Secrets

A secret's value is written and never read back: no answer, event, or log
line carries it.

| Route | Action | What it does |
|---|---|---|
| `POST /v1/secrets` | `secret.create` | create a secret from a manifest |
| `GET /v1/secrets` | `secret.list` | list your secrets; `owner` and `label` narrow the list as they do for sandboxes |
| `PUT /v1/secrets/{name}` | `secret.create` or `secret.update` | apply by name; a second apply rotates the value in place, and running sandboxes use the new value on their next request |
| `GET /v1/secrets/{name}` | `secret.read` | read one secret without its value |
| `DELETE /v1/secrets/{name}` | `secret.delete` | delete it; a sandbox that mounted it reports the placeholder as not injectable |

### Environments

An environment is where sandboxes run: the control plane's own runtime,
or a runtime a worker serves. Under the owner policy only an
administrator creates one.

| Route | Action | What it does |
|---|---|---|
| `GET /v1/environments` | `environment.list` | list the environments this control plane holds |
| `POST /v1/environments` | `environment.create` | create one from a manifest; its name is its id |
| `GET /v1/environments/{id}` | `environment.read` | read one with its phase and capabilities |
| `PUT /v1/environments/{id}` | `environment.create` or `environment.update` | apply by name, with `If-Match` against a concurrent edit |
| `DELETE /v1/environments/{id}` | `environment.delete` | delete one no sandbox is placed on; the control plane's own environment is not deletable |
| `POST /v1/environments/{id}/keys` | `environment.key` | mint an environment key, shown once |
| `GET /v1/environments/{id}/keys` | `environment.key` | the keys minted for the environment, oldest first: each jti, when it was minted and by whom, when it expires, and whether it was revoked; never the token |
| `DELETE /v1/environments/{id}/keys/{jti}` | `environment.key` | revoke one key |

### Workers and gateways

These three routes take an environment key and nothing else, and are
called by `cellad worker` and `cellad egress`, not by a person.

| Route | What it does |
|---|---|
| `POST /v1/environments/{id}/workers` | register a worker |
| `GET /v1/environments/{id}/operations` | the WebSocket a worker receives its operations on |
| `GET /v1/environments/{id}/egress` | the WebSocket a gateway receives its environment's boundaries on |

[Self-hosting a data plane](workers.md) is how they fit together.

## Outside `/v1`

| Route | Listener | What it is |
|---|---|---|
| `GET /openapi.yaml` | public | the OpenAPI document, with no credential |
| `GET /.well-known/jwks.json` | public | the keys `cellad` signs with, with no credential |
| `GET /` | public | the build's identity |
| `GET /livez`, `GET /readyz`, `GET /version` | both | the probes and the build's version |
| `GET /metrics` | internal | the Prometheus exposition; [Observability](observability.md) lists the series |
