---
title: "Egress scope by path and confirmation refusals: a Secret scoped by host, port and path prefix where the longest prefix wins, substitution into one header only, confirmation patterns the gateway refuses with 403 and records for the sandbox's owner, one-shot allowances, and upstream refusals in the same record"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/005-lifecycle-controller.md
  - specs/006-identity.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/018-egress-and-secrets.md
  - specs/022-mesh-and-spawn.md
  - specs/.archive/039-egress-gateway.md
  - specs/.archive/046-secret-kind.md
affects: [manifest/v1/, manifest/, egress/, internal/egressd/, internal/api/, internal/store/, controller/, authorizer/, client/, api/openapi.yaml, test/conformance/, docs/, CHANGELOG.md]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Egress scope by path and confirmation refusals

## Overview

A client that creates sandboxes, an agent runtime for example, runs code
inside them that it did not write and cannot fully read, and has to assume
that code may be hostile. The sandbox holds no credential: the client
mounts Secrets, the sandbox holds placeholders, and the egress gateway
swaps a placeholder for its value on the way out, toward the scope the
Secret's owner named ([[018-egress-and-secrets]]). Two things that client
needs are missing.

The first is scope below the host. One API host commonly fronts several
services, each under its own path prefix, each with its own credential. A
Secret today is scoped by host and port, and two Secrets on one host are
refused, so the client either gives one credential the whole host or cannot
mount the second. This spec scopes a Secret by host, port and path prefix,
chooses the credential of the longest prefix that covers a request, and
fixes the path rules so that no encoding, dot segment, or reading a server
might apply moves a request from one prefix onto another. A value is
written into one header and nowhere else, since a value the workload can
place in a body or a query string is a value an upstream can store and
serve back to it.

The second is confirmation. Some requests are consequential enough that the
sandbox's owner wants to confirm them before a credential is used: a
delete, a push, a refund. The gateway sees the method and path of every
request it swaps a credential into and is the one place that can refuse
before the credential leaves. A Secret or a sandbox carries confirmation
patterns, `METHOD host path-glob`; a request carrying a swapped-in
credential that matches one is answered 403 `confirmation_required` and
recorded on the sandbox for its owner to read. The owner, with its own
credential, may place a one-shot allowance for exactly that method, host
and path, at most five minutes long and consumed by the first request it
admits. Services behind the gateway refuse with the same code on their own
terms, and the gateway records those refusals in the same list, so the
client reads one place whichever side refused.

## Current state

| Piece | Where | What it does today |
|---|---|---|
| The scope | `v1.SecretScope` in `manifest/v1/secret.go` | `hosts` and `ports`; no path |
| The injection place | `v1.SecretInject` | `header`, `scheme`, `query`, `body`; header or query, and the body when `body: true` |
| The conflict rule | `refuseSharedScopes` in `manifest/secretref.go`, `refuseSharedHosts` in `egress/egress.go` | two mounted secrets whose host patterns overlap are refused, at resolve as `secret_host_conflict` and at compile as `ErrSecretHostConflict`; ports are not considered |
| Selection | `egress.Map.EntryFor(host, port)` | the first entry whose hosts and ports match |
| Termination | `gate.decide` in `internal/egressd/gate.go` | terminates when `store.HasSecretFor(principal, host)`, by host alone, and tunnels every other admitted host |
| The proxy door's terminated requests | `pkg/egress.Gateway.forward` | `SubstituteHTTPRequestContext` over every substitutable header, the raw query string, and an opted-in body, scoped by host; round-trips through the engine's own transport. Placement is not held on this door, which [[046-secret-kind]]'s Open table records |
| The reverse door | `ServeReverse`, `upstreamRequest`, `substitutePlaced` | rebuilds the upstream URL from the decoded path (`splitReversePath(r.URL.Path)`) and applies each entry to its one header or query parameter with a single-entry table |
| Records | `egress.Record`, `GET /v1/sandboxes/{id}/egress` | one per connection on the proxy door and per request on the reverse door; decisions `allowed`, `denied`, `unknown`, `passthrough`; served newest first under `sandbox.read`, which the owner policy grants a sandbox over itself |
| The stream | `egress.Protocol` in `egress/sync.go` | `cella.egress.v1`; `Decode` refuses a frame type it does not know |

There is no path in a scope, no confirmation pattern, no refusal record, no
allowance, and no reading of an upstream's response.

## Design

### The Secret's scope

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: Secret
metadata:
  name: billing-api
spec:
  kind: static
  scope:
    hosts: ["api.example.com"]
    ports: [443]
    paths: ["/billing"]                 # path prefixes; default ["/"]
  inject:
    header: Authorization               # the one place the value is written; default Authorization
    scheme: bearer
  confirm:                              # requests carrying this credential that need the owner's confirmation
    - "DELETE api.example.com/billing/**"
    - "POST api.example.com/billing/*/refunds"
  value: ...
```

`scope.paths` rules:

- An entry is `/`, or `/` followed by segments separated by `/`. A segment
  is one or more characters of `A-Z a-z 0-9 - . _ ~ ! $ & ' ( ) * + , = : @`,
  which is RFC 3986's `pchar` less the percent sign and the semicolon. A
  segment `.` or `..`, an empty segment, and an entry that does not begin
  with `/` are `invalid_field` at `spec.scope.paths[i]`.
- A trailing `/` is removed at resolve, so `/billing/` is stored
  `/billing`. Duplicates are removed and the list is stored sorted. At most
  32 entries of at most 256 bytes each.
- An absent or empty list is `["/"]`. A Secret stored before this spec has
  no paths and reads as `["/"]`, so it covers what it covered.
- The scope is the product: every host of `hosts`, at every port of
  `ports`, under every prefix of `paths`.

A prefix `P` covers a path when `P` is `/`, when the path equals `P`, or
when the path begins with `P/`. The match is on segment boundaries:
`/billing` covers `/billing`, `/billing/` and `/billing/invoices/42`, and
not `/billing-v2` or `/billingx`.

Two mounted secrets conflict when a host pattern of one overlaps a host
pattern of the other (the symmetric coverage rule both conflict checks use
today), their ports share a port, and a prefix of one equals a prefix of the
other ignoring ASCII case. The pair is refused at resolve as
`secret_scope_conflict`, 409, naming both, and at compile as
`ErrSecretScopeConflict`; both replace their `host` namesakes (D7). Two
secrets on one host with different prefixes are accepted: `/billing` beside
`/search`, or `/` beside `/billing`, where `/billing` takes its subtree and
`/` the rest. Equality ignores case because the selection rule below cannot
tell `/Billing` from `/billing` on a server that routes without case.

A Secret's owner may change its `paths` while sandboxes mount it. A
recompile that finds a conflict on a running sandbox does not fail the
update the owner made, since that owner may not see the sandbox: both
entries leave that sandbox's map and both are named in
`status.secrets.notInjectable`, because a request whose credential cannot be
chosen is sent with neither (D12).

`workspace.git.secret` ([[003-manifest-contract]]) is held to the path as
well: the clone URL's host and port are in the secret's scope and its path
is under one of its prefixes, else `secret_out_of_scope`. A smart HTTP clone
requests `<path>/info/refs` and `<path>/git-upload-pack`, both under the
clone URL's path.

### Canonical paths

The gateway judges a request by the path it will forward. Two servers can
read one path differently: one decodes `%2F` into a separator and another
does not, one resolves `..` and another does not, one routes without case,
another strips `;` parameters from a segment. A path that the gateway and
an upstream can read as being under two different prefixes is one on which
the gateway cannot know which credential belongs. The rule is that a path
is given a credential only when every reading listed here puts it under the
same prefix, and a path some reading cannot even parse is given none.

The gateway reads the path as it arrived, the escaped form of the request
line or of `:path` (Go's `URL.EscapedPath`, never the decoded `URL.Path`),
in eight readings: each combination of

- raw, or decoded, where every escape is decoded and `%2F` becomes a
  separator;
- as it is, or with ASCII letters folded to lower case;
- as it is, or with each segment cut at its first `;`, after decoding in
  the decoded readings.

A trailing empty segment is dropped in every reading. A prefix is compared
in the same reading, so in a folded reading `/Billing` and `/billing` are
one prefix. A prefix holds no `%` and no `;`, so the decoded and the cut
readings leave a prefix as it is.

A request path is canonical when all of these hold:

1. It begins with `/`. The asterisk form (`OPTIONS *`) is not canonical.
   The path of an absolute-form target is judged the same way, and its
   authority by the host rule of the termination section.
2. Every `%` begins an escape of two hexadecimal digits, and no escape
   encodes `%`, `\`, or a control byte: `%zz`, a trailing `%4`, `%25`,
   `%5C` and `%00` are not canonical. `%25` is refused because a server
   that decodes twice would find a second escape behind it.
3. There is no raw `\` and no raw control byte (below `0x20`, and `0x7F`),
   since some servers read a backslash as a separator.
4. In no reading is a segment empty except the last: `/a//b` and
   `/a%2F%2Fb` are not canonical, `/a/b/` is.
5. In no reading is a segment `.` or `..`: `/a/../b`, `/a/%2e%2e/b` and
   `/a/..;/b` are not canonical.

A server that decodes and one that does not, one that folds case and one
that does not, one that strips parameters and one that keeps them, each
read the path as one of the eight readings. Rules 4 and 5 make every
reading a plain list of segments with no step that walks back up the tree,
and selection below asks all eight for one answer. So a path is given a
credential only when every one of those servers would place it under the
same prefix. An escaped letter, `/bill%69ng`, is canonical and reads as
`billing` only when decoded, so it selects nothing unless both spellings
fall under one prefix; an escaped separator, as in `/projects/a%2Fb`, keeps
its credential wherever the split does not change the prefix.

The upstream receives the escaped path the gateway judged, byte for byte:
the gateway serializes the request it parsed and never rewrites its path,
on either door. The reverse door keeps the escaped form of everything after
the destination segment, where today it rebuilds the upstream URL from the
decoded path.

### Selecting the credential

For a request toward host `h` and port `p` with a canonical path, the
candidates are the entries of the map whose hosts match `h` under the host
rule and whose ports contain `p`. In each reading, the selected entry is
the candidate with the longest prefix that covers the path. The conflict
rule leaves no two candidates with an equal prefix, and two different
prefixes that cover one path differ in length, so the longest is unique.

The path is ambiguous when the eight readings do not select the same entry,
including one reading selecting an entry and another none. A request whose
path is not canonical or is ambiguous selects no entry (D6).

At most one entry is selected per request, so a request carries at most one
credential. With entries A (`api.example.com`, `/`), B (`api.example.com`,
`/billing`), C (`api.example.com`, `/billing/admin`) and D
(`*.example.com`, `/search`), on `api.example.com:443`:

| Path | Selected | Why |
|---|---|---|
| `/billing/invoices/42` | B | the longest covering prefix |
| `/billing` | B | a prefix covers its own path |
| `/billing/admin/users` | C | longer than B |
| `/billing-v2/x` | A | segment boundary |
| `/search/q` | D | D's host covers the name, and `/search` is longer than `/` |
| `/Billing/x` | none | ambiguous: the raw reading selects A, the folded one B |
| `/billing;v=2/x` | none | ambiguous: the raw reading selects A, the cut one B |
| `/billing/%2e%2e/admin` | none | not canonical, rule 5 in the decoded readings |
| `//billing/x` | none | not canonical, rule 4 |
| `/bill%69ng/x` | none | ambiguous: the raw reading selects A, the decoded one B |
| `/search/a%2Fb` | D | every reading selects D |

A request that selects no entry is forwarded as sent, and every placeholder
in it goes out as the inert string it is. Its record names no secret and
carries the reason `PathNotCanonical` or `PathAmbiguous` where one of them
applies. The boundary stays a matter of hosts, because the driver's network
rule sees hosts and not paths: a mounted secret's hosts join the allow list
for every path, as today. The path scope decides which credential a request
carries, and never whether it may be sent.

### Substitution into one header

The selected entry's value replaces its placeholder in the values of the
one header its `inject.header` names, and nowhere else: not in another
header, not in the query string, not in the body. Every other placeholder
in the request, the selected entry's in any other place and every other
entry's anywhere, goes out verbatim.

A sandbox that may run hostile code chooses what it sends to a host in
scope. A value written into a body or a query string is a value the
workload can make the upstream keep and give back: posted into a comment,
an issue or a document title, echoed in an error message, carried in a
redirect's `Location`, then read through the same credential. A header the
owner named is a place an upstream reads as a credential rather than as
content.

`inject.query` and `inject.body` are removed from the Secret kind (D1):
`inject` is `{header, scheme}`, `header` defaults to `Authorization`, and
`scheme` is derived as today. A manifest carrying either removed field is
`unknown_field`. The companion `<env>_QUERY` is no longer projected;
`<env>_HEADER` stays.

`body: true` was the one way a value entered content: an opt-in that ran
the engine's body rule, a known `Content-Length` of at most
`DefaultMaxBodyBytes` and a JSON, form or text type, over the entries that
set it. Removing it removes that path whole. The egress role no longer
calls `SubstituteHTTPRequestContext`, which walks every header, the query
string and the body; it calls `SubstituteValueContext` of the selected
entry's single-entry table over that one header's values, on both doors.
The oauth kind is unaffected: the gateway sends the client id and secret to
the token endpoint itself, and what replaces the placeholder is the minted
token, in the one header.

A Secret stored with `body: true` loads without the flag and substitutes
into its header as before, which narrows it. A Secret stored with
`inject.query` loads with no header; it compiles to no entry, and every
sandbox mounting it names it in `status.secrets.notInjectable` until its
owner applies it again with a header. No stored value moves into a header
its owner did not name.

### Termination

A CONNECT names a host and a port and nothing else; the path is inside the
TLS session. The proxy door therefore terminates a connection when some
entry's hosts match the host and its ports contain the port, whatever that
entry's prefixes, and tunnels every other admitted connection untouched, as
today. A confirmation pattern never causes termination: a pattern applies
only to a request carrying a swapped-in credential, and a host no entry is
scoped to carries none. A request on a terminated connection that selects
no entry is read and forwarded without substitution. That is the cost of a
path scope: the gateway reads every request to a credential's host and
port, not only the requests it substitutes into.

On a terminated connection:

- The leaf is minted for the CONNECT host, and the gateway dials the CONNECT
  host with its own server name and `Host`, never with the client's.
- Every request is judged on its own. An HTTP/1.1 connection carries
  requests one after another and an HTTP/2 connection carries streams side
  by side, and each may be under another prefix, so selection,
  substitution, confirmation and the record are per request.
- A request's `Host`, its `:authority` on HTTP/2, and the authority of an
  absolute-form target name the CONNECT host, with no port or the CONNECT
  port. Otherwise the gateway answers 421 `misdirected_request` and
  forwards nothing.
- `CONNECT` inside a terminated connection is answered 405.
- An upgrade (`Upgrade: websocket`) is judged at its upgrade request. After
  the switch the bytes are relayed and not read.

The reverse door always terminates, since the sandbox speaks plain HTTP to
it. The destination is the first path segment and the rest is the path the
rules above judge, in its escaped form, toward port 443.

### Confirmation patterns

```
pattern = method SP host path
method  = "*" / token                   ; an HTTP method, matched ignoring ASCII case
host    = "*" / host-pattern            ; the host rule of 003; "*" is every host
path    = "/" / 1*( "/" glob )
glob    = "**" / 1*( segment-char / "*" )
```

`segment-char` is the prefix segment character set above, less `*`. `*`
within a segment matches any run of that segment's characters, the empty
run included; `**` as a whole segment matches zero or more segments. There
is no `?`, no character class and no query. A pattern is stored normalized:
the method upper-cased, the host by `NormalizeHost`, a trailing `/`
removed.

| Pattern | Matches | Does not match |
|---|---|---|
| `DELETE api.example.com/**` | every DELETE to the host | a GET |
| `POST api.example.com/billing/*/refunds` | `/billing/inv_42/refunds` | `/billing/a/b/refunds` |
| `* api.example.com/admin/**` | any method on `/admin` and under it | `/administrator` |
| `PUT */**` | a PUT toward every host a credential is swapped toward | a PUT with no credential in it |
| `POST git.example.com/**/git-receive-pack` | a push over smart HTTP | a fetch, `git-upload-pack` |

Where patterns live:

- `Secret.spec.confirm` applies to requests that carry this secret's value.
  A pattern's host is `*` or a pattern that overlaps a host of
  `scope.hosts`; one that overlaps none could never match and is
  `invalid_field`. A change reaches every mounting sandbox's gateway as a
  map at a higher version, the way a value change does.
- `Sandbox.spec.network.egress.confirm` applies to every request of the
  sandbox that carries any swapped-in credential. It is a `narrow` field
  ([[003-manifest-contract]]): a workload may add a pattern and may never
  remove one (`boundary_widened`); the owner may do both. A pattern whose
  host overlaps no mounted secret's host is accepted with a line in
  `status.warnings`, since a later mount may give it a meaning.
- A spawned child's list is the union of its own and its parent's, written
  by the resolver, so a child is confirmed wherever its parent is. The
  boundary check of 003 gains a tenth rule, that a child's list holds its
  parent's, which the union makes true at create and the narrowing rule
  keeps true on a workload's update ([[022-mesh-and-spawn]]).
- At most 32 patterns per object, each at most 512 bytes; more is
  `invalid_field`.

A request carries a swapped-in credential when an entry was selected and
that entry's placeholder is present in that entry's header. Only such a
request is matched. A request with no credential in it is the workload's
own and the boundary's, and the gateway has no confirmation to ask for.

The request matches a pattern when all three hold:

- a member of its method set equals the pattern's method ignoring ASCII
  case, or the pattern's method is `*`; a `GET` pattern also matches
  `HEAD`, which servers answer with the GET handler;
- its host matches the pattern's host under the host rule, or the pattern's
  host is `*`;
- its path matches the pattern's glob in any of the eight readings.

A prefix is selected only when every reading agrees and a pattern matches
when any reading does: both lean toward refusing.

The method set is the request line's method and every value of
`X-HTTP-Method-Override`, `X-HTTP-Method`, `X-Method-Override`, and the
query parameter `_method`, since frameworks honor each and a POST can act as
a DELETE. The effective method is the one override value when exactly one
distinct value is present, and the request line's when none is. With two or
more distinct values the effective method is undefined, and no allowance
admits the request. A method carried in the body, a form field or a GraphQL
operation, is not read, because the body is not; an owner whose upstream
honors one writes the pattern with the request line's method, for example
`POST api.example.com/graphql`.

Per request, the order is: the host check (421), canonical path and
selection, credential presence, confirmation with a claim where a pattern
matches, substitution, forward, the upstream's answer, the record.

### The refusal

A request carrying a swapped-in credential that matches a pattern, and that
no allowance admits, is answered by the gateway and never forwarded:

```http
HTTP/1.1 403 Forbidden
Content-Type: application/json

{"error": {"code": "confirmation_required",
           "message": "This request needs confirmation from the sandbox's owner.",
           "details": {"request_id": "req_01J9ZK..."}}}
```

The envelope is `latere.ai/x/pkg/httpjson`'s, the one the API writes, and
`request_id` is the gateway's own id for the request. The body names
neither the pattern nor the refusal's id (D10): the sandbox learns that its
request needs confirmation and nothing of the list its owner reads. Both
doors write the same answer, the proxy door inside the TLS session.

The gateway records the refusal, sends it up the stream as a `refusal`
frame, and keeps it until the control plane answers `stored`. The
connection record of the request carries `decision: refused` and the reason
`ConfirmationRequired`.

```json
{"id": "rfl_01J9ZK...", "sandbox": "sbx_01J9...", "at": "2026-09-27T10:00:00Z",
 "source": "gateway", "door": "proxy",
 "method": "DELETE", "host": "api.example.com", "port": 443,
 "path": "/billing/invoices/42",
 "secret": "billing-api", "prefix": "/billing",
 "pattern": "DELETE api.example.com/billing/**", "patternOf": "secret"}
```

| Field | Meaning |
|---|---|
| `id` | `rfl_` and a ULID, minted by the gateway, so a frame sent again after a reconnect is stored once |
| `sandbox` | the sandbox's id |
| `at` | when the gateway answered |
| `source` | `gateway` for a pattern of this spec, `upstream` for a refusal read from the upstream's answer |
| `door` | `proxy` or `reverse` |
| `method` | the effective method; `requestMethod` carries the request line's when the two differ, and an undefined effective method leaves `method` the request line's with `methodAmbiguous: true` |
| `host`, `port` | the destination host and port the gateway dialed or would have dialed |
| `path` | the escaped path without the query string, at most 2048 bytes; `pathTruncated: true` when cut, and then no allowance admits the request |
| `secret`, `prefix` | the service behind the host: the Secret whose credential the request carried and the prefix that selected it |
| `pattern`, `patternOf` | the first pattern that matched, the secret's before the sandbox's, and `secret` or `sandbox`; gateway refusals only |
| `upstream` | `{action, resource}` read from the upstream's answer; upstream refusals only |

A refusal carries no header, no query string, no body, no value, no
placeholder, and not the credential. `Record.Normalize`'s rules apply to it:
a refusal whose host or path has the placeholder shape is dropped.

### Refusals from upstream services

A service behind the gateway may refuse a request on its own terms with the
same code. The gateway reads the answer to a request that carried a
swapped-in credential when its status is 403, its `Content-Type` is JSON
(`application/json` or a `+json` suffix), and its body, read up to 16 KiB,
parses as the shared envelope `{"error": {"code": "confirmation_required"}}`
or as a flat `{"code": "confirmation_required"}` (D5). It records a refusal
with `source: upstream` and the fields above less the pattern, adding
`upstream.action` and `upstream.resource` when the envelope's
`error.details` carries them as strings of at most 256 bytes each. They are
the upstream's text, stored as data and never interpreted. The response
reaches the sandbox unchanged, the bytes read for inspection first. Another
status, another type, or a body longer than 16 KiB is relayed without being
read.

Only a request carrying a swapped-in credential is read. The upstream is
then a service the Secret's owner scoped, so a workload cannot fill its
owner's list by sending requests to a server of its own that answers
`confirmation_required`.

An allowance never lifts an upstream's refusal. The gateway's allowance
lifts the gateway's own patterns; the service that refused holds its own
grant, issued for example through the installation's authorizer. The
client tells the two apart by `source`, and a placement that names an
upstream refusal is `invalid_field`.

### Reading refusals

`GET /v1/sandboxes/{id}/egress/refusals?after=<rfl_ id>&limit=<n>` answers
the sandbox's refusals of both sources, oldest first, after the one `after`
names, or from the oldest held when it is absent. `limit` is 1 to 200,
default 50. The answer is `{"items": [...], "next": "rfl_..."}`, where
`next` is the last item's id, or `after` when the page is empty, so a client
polls with the `next` it was given. An `after` the store does not hold,
because the retention or the cap evicted it or because it never existed, is
410 `cursor_expired`, and the client reads again from the oldest.

The order is the store's insertion order, not the ids': two gateways'
clocks may disagree, and a poller that advanced past an id must still see a
refusal stored after it.

The sandbox's owner reads them: `status.owner`, which in a spawn tree is
the root's owner ([[022-mesh-and-spawn]]), with the owner's own credential,
and any subject the installation's authorizer admits for the action
`sandbox.confirm` (D4); under the owner policy, the owner and the subjects of
`CELLA_ADMIN_SUBJECTS`. A workload subject, one whose subject carries the
`sandbox:` prefix, is refused `forbidden` before the authorizer is asked,
whichever sandbox it is. The owner policy lets a sandbox read itself, and an
authorizer may allow anything, so this is a rule of the API and not a
policy. The allowance routes below carry the same rule.

The store surface is `Refusals` ([[010-state]]), beside `Records`: a
Postgres table `egress_refusals` keyed by sandbox and insertion sequence,
kept for `CELLA_EGRESS_RECORDS_RETENTION`, and a memory ring of
`CELLA_EGRESS_RECORDS_CAP` per sandbox. The ring is its own, so a sandbox's
connection records never evict its refusals. A sandbox's refusals are
removed when its records are. The connection records route stays `sandbox.read`: a refused
request's record says `refused` and why, which the sandbox knows from the
answer it got, and carries no refusal id and no pattern.

### One-shot allowances

```http
POST /v1/sandboxes/{id}/egress/allowances
Content-Type: application/json

{"method": "DELETE", "host": "api.example.com", "path": "/billing/invoices/42",
 "ttl": "5m", "refusal": "rfl_01J9ZK..."}
```

```http
HTTP/1.1 201 Created

{"id": "alw_01J9...", "method": "DELETE", "host": "api.example.com",
 "path": "/billing/invoices/42", "refusal": "rfl_01J9ZK...",
 "placedBy": "https://login.example.com|alice",
 "placedAt": "2026-09-27T10:01:00Z", "expiresAt": "2026-09-27T10:06:00Z"}
```

Rules:

- `method` is a method token, stored upper-cased. `host` is one exact name
  under the host rule, without a wildcard. `path` is canonical under the
  rule above, at most 2048 bytes, and taken literally: a `*` in it is a
  character, not a glob. Its escapes' hexadecimal digits are stored
  upper-cased. No query string. A field outside these is `invalid_field`.
- `ttl` is a positive Go duration of at most `5m`, and `5m` when absent;
  above is `invalid_field`. `expiresAt` is the placement plus `ttl` by the
  control plane's clock.
- `refusal`, when present, names a refusal of this sandbox with `source:
  gateway` whose method, host and path equal the body's. The link is kept on
  the allowance and its events. An upstream refusal, a mismatch, or an
  unknown id is `invalid_field`.
- At most 16 live allowances per sandbox; a seventeenth is
  `ceiling_exceeded`.
- A sandbox in `Deleting` is `phase_conflict`.
- The owner places it with its own credential, or a subject the authorizer
  admits for `sandbox.confirm`; never a workload, as for refusals.

`GET /v1/sandboxes/{id}/egress/allowances` lists the live ones.
`DELETE /v1/sandboxes/{id}/egress/allowances/{alw_ id}` revokes one with
204, and answers 204 again for one already consumed, expired or revoked.

For `sandbox.confirm` on a placement, the authorizer's `resource` is the
sandbox's, as for every sandbox action of [[006-identity]], plus
`"egress": {"method", "host", "path", "secrets": ["sec_..."]}`, where
`secrets` names the mounted Secrets whose entry the allowance's host and
path select. An installation where one subject owns a Secret and another
owns the sandbox can then require the Secret owner's consent. Under the
owner policy every mounted secret is the owner's own, since that policy
lets a subject mount only its own.

Live allowances are desired state, `status.egressState.allowances`, which
the API strips from every response like the rest of `egressState`, so they
survive a restart and a writer handoff. They are not in the map: a gateway
never holds one.

Consumption (D3): when a request carrying a swapped-in credential matches a
pattern, the gateway sends `claim {principal, claim, method, host, path}`
with the effective method and the exact path, and waits at most
`CELLA_EGRESS_ACK_TIMEOUT` for `claimed {principal, claim, allowance}`. It
sends no claim for an undefined effective method or a truncated path. The
control plane looks for a live allowance of that sandbox whose method, host
and path equal the claim's and whose `expiresAt` is after its own clock.
Finding one, it removes it from desired state in one conditional write,
appends `sandbox.allowance_consumed`, and answers its id; finding none, it
answers an empty id. An id admits the request, which is then substituted
and forwarded like any other. An empty id, no answer in time, or no stream
refuses it, as above.

An allowance therefore admits exactly one request, whichever gateway of the
environment serves it: the write is a compare-and-swap on one row under the
writer lease, and two claims for one allowance receive one id. A claim is
sent only for a request the gateway would otherwise refuse, so an allowance
costs one round trip on the request it admits, and a request no pattern
matches costs nothing. An answer lost after the write consumed the
allowance leaves the request refused and the allowance spent; the owner
places another.

Matching is exact where patterns are loose. The method, host and path of
the request equal the allowance's, the path compared as it is, with case,
where a pattern matches in any reading. A pattern leans toward refusing and
an allowance toward not admitting. The query string and the body are not
part of an allowance.

An allowance past `expiresAt` admits nothing, since the control plane
compares at the claim, and the reaper's tick ([[005-lifecycle-controller]])
removes it and appends `sandbox.allowance_expired`. An allowance belongs to
one sandbox: one placed on a parent admits nothing of a child.

### Audit

Four event types join [[009-events]], each signed, with the sandbox as the
object, delivered to the sink:

| Type | When | `subject` | `data` |
|---|---|---|---|
| `sandbox.allowance_placed` | the `POST` | the placer, with its request id | `{allowance, method, host, path, expiresAt, refusal}` |
| `sandbox.allowance_consumed` | a claim took it | `sandbox:<id>`, whose request consumed it | `{allowance}` |
| `sandbox.allowance_expired` | the reaper removed it | `controller` | `{allowance}` |
| `sandbox.allowance_revoked` | the `DELETE` of a live one | the revoker, with its request id | `{allowance}` |

A refusal is not a mutation and emits no event, as [[009-events]] rules
for a denied action; the refusal store is its record. With
`CELLA_EVENTS_EGRESS=1` the connection record of each refused request
reaches the sink as `sandbox.egress`, as every record does.

The approval trail is the owner's. `GET /v1/sandboxes/{id}/events` and
`GET /v1/events` leave `sandbox.allowance_*` out of their answer to a
workload caller (D9); the sink receives every one.

Metrics ([[017-observability]]): `cella_egress_refusals_total` with a
`source` label, `cella_egress_allowances_total` with an `outcome` label of
`placed`, `consumed`, `expired` or `revoked`, and
`cella_egress_refusals_dropped_total` for refusals a gateway dropped from a
full buffer. The `decision` label of `cella_egress_connections_total`
gains `refused`.

### The gateway's identity is unchanged

At the gateway a sandbox is the principal `sandbox:<id>`, authenticated by
its gateway credential: `Proxy-Authorization: Basic` with the proxy URL's
userinfo on the proxy door, `Cella-Egress-Credential` on the reverse door,
as [[018-egress-and-secrets]] fixes. Neither changes. The workload token,
`sub` `sandbox:<id>`, does not change either. It is not a credential the
gateway accepts, substitutes or confirms, and it cannot reach the control
plane through the gateway, which refuses the control plane as a
destination. It is the one credential the refusal and allowance routes
refuse whatever an authorizer answers.

### The sync protocol, version 2

`egress.Protocol` becomes `cella.egress.v2`.

| Frame | Direction | Meaning |
|---|---|---|
| `claim {principal, claim, method, host, path}` | up | a request that matched a pattern asks for an allowance; `claim` is the gateway's id for the question |
| `claimed {principal, claim, allowance}` | down | the id of the allowance consumed for it, or empty |
| `refusal {refusal}` | up | one refusal record |
| `stored {refusal}` | down | the refusal of that id is stored; the gateway forgets it |

The map changes too. `Entry` gains `paths` and `confirm` and loses `query`
and `body`; `Map` gains `confirm`, the sandbox's patterns with the parent's.
A version 1 gateway would read a version 2 map as host-scoped and
substitute toward every path, so the control plane refuses a `hello` that
names another protocol, and a gateway refuses a server that answers with
another. An environment whose gateways and control plane run different
releases keeps enforcing the maps its gateways hold, and a create that
needs a gateway fails with `egress_gateway_unavailable` until both run one
release.

A gateway keeps at most 256 refusals the control plane has not answered
`stored`, sends them again on its next stream, and drops the oldest beyond
the bound, counting it.

The stream is authenticated by the environment key and names one
environment, so the control plane answers a `claim` only for a principal of
a sandbox in that environment and stores a `refusal` only for one; any
other claim is answered with an empty id and any other refusal is dropped.
A gateway of one environment can neither spend nor fill another's.

### Package layout and the pkg/egress seam

| Package | Adds |
|---|---|
| `manifest/v1` | `SecretScope.Paths`, `SecretSpec.Confirm`, `Egress.Confirm`, `SecretInject` without `Query` and `Body`, `EgressState.Allowances` and `Allowance`; `NormalizePathPrefix`, `PathPrefixCovers` and `ParseConfirmPattern` beside the host pattern algebra, because the resolver and the compiler share them |
| `manifest` | the prefix and pattern validation, the conflict rule, the narrowing and boundary rules for `confirm`, the clone rule |
| `egress` | `CanonicalPath` and its readings, `Map.Select(host, port, path)`, `Map.Terminates(host, port)`, pattern matching over a request's method set, host and readings, `Refusal` and its `Normalize`, the version 2 frames; it still dials nothing, so a client that runs its own local proxy can apply the same rule |
| `internal/egressd` | one per-request function both doors serve, the claim client, the upstream inspection, the refusal buffer |
| `internal/api` | the four routes, the workload rule, the claim and refusal handlers on the stream, the events filter |
| `internal/store` | `Refusals` |
| `controller` | allowances in desired state, the claim's conditional write, the reaper's expiry |
| `authorizer` | `sandbox.confirm` |
| `client` | `Refusals`, `PlaceAllowance`, `Allowances`, `RevokeAllowance` |

On the proxy door, terminated requests are served by `pkg/egress.Gateway`,
whose `forward` substitutes by host alone and round-trips through a
transport of its own. There is no place in it for a path, a pattern, a
claim, or a look at the answer. The proposed change to `pkg` is one
optional field (D2):

```go
// Forward, when set, receives every request read from a terminated
// connection, on HTTP/1.1 and on each HTTP/2 stream, in place of the
// gateway's own substitution and round trip; the response it returns is
// written back to the client.
Forward func(ctx context.Context, host, hostport string, req *http.Request) (*http.Response, error)
```

`pkg` keeps the CONNECT, the leaf, ALPN and both framings; the policy is
`internal/egressd`'s, which serves both doors through one function. The
same seam closes [[046-secret-kind]]'s two open rows, placement on the proxy
door and a terminated connection dialing through the operator's seam,
because the egress role round-trips with its own transport. The field is a
`pkg` release before this spec is dispatched.

### What breaks

Host-only Secrets need no change: they read as `paths: ["/"]` and substitute
toward every path of their hosts, in their one header. Everything else this
spec changes that a caller or an importer can see:

| Change | Who notices | What they do |
|---|---|---|
| `spec.inject.query` and `spec.inject.body` are gone | a manifest with either is `unknown_field`; a stored query secret is `notInjectable` wherever it is mounted; a stored body secret substitutes in its header only; `<env>_QUERY` is no longer projected | apply the secret again with a header |
| `secret_host_conflict` is `secret_scope_conflict`, with the sentence "Two mounted secrets apply to the same host and path."; secrets on one host with different prefixes, or with disjoint ports, are accepted | a client that matches the code | match the new code |
| the stream is `cella.egress.v2` | an environment whose gateways and control plane run different releases | roll both; held maps keep enforcing meanwhile |
| a terminated connection yields one record per request, and a record may say `refused` | a reader that counted connections | count by `door` and `decision` |
| requests on a terminated connection are held to the tunnel's host | a client that names another host inside a tunnel | 421 `misdirected_request` |
| a non-canonical or ambiguous path to a credential's host and port carries no credential | a client that writes `//`, `%25`, a dot segment, or a spelling some reading places under another prefix: an escaped letter, a case variant, a `;` parameter | the upstream answers unauthenticated, and the record names the reason |
| the root package `egress`: `Entry` loses `Query` and `Body` and gains `Paths` and `Confirm`; `Map.EntryFor` is `Map.Select` with a path; `SecretView.Inject` loses the two fields; `ErrSecretHostConflict` is `ErrSecretScopeConflict`; `Protocol` is version 2 | an importer | compile against the new names |
| the reverse door forwards the escaped path it was sent | a client whose escapes the gateway used to re-encode | none expected |

### Changes to other specs when this is built

This draft edits no other spec. The tables those specs' tests hold to the
code change together with the code:

| Spec | Change |
|---|---|
| [[003-manifest-contract]] | `network.egress.confirm`, a `narrow` field; the `secrets[]` conflict sentence; boundary rule 10; the clone rule's path; `secret_scope_conflict` in the error codes; `<env>_QUERY` leaves the companion rule |
| [[006-identity]] | the action `sandbox.confirm` and its `resource` with `egress`; the owner policy's owner and admins may `confirm`, a sandbox never |
| [[008-api]] | the four routes; `confirmation_required` 403 and `misdirected_request` 421 as codes the gateway answers; `secret_scope_conflict` 409 in place of `secret_host_conflict`; the stream row's version 2 frames |
| [[009-events]] | the four `sandbox.allowance_*` types; `paths` in the `secret.*` data; the workload feed rule |
| [[010-state]] | the `Refusals` surface and its table |
| [[013-security-and-threat-model]] | controls for a path trick, a value reflected through a body or a query string, a request aimed at another virtual host through a tunnel, a workload answering its own refusal, an allowance used twice, and a workload filling its owner's refusals |
| [[017-observability]] | the three counters and `refused` |
| [[018-egress-and-secrets]] | the Secret kind, the map, what is enforced, records per request, the stream's version; the index's decision on two secrets on one host becomes one host and one prefix |

### Decisions

| # | Question | Proposed | Alternative |
|---|---|---|---|
| D1 | Is substitution header-only for every Secret? | yes: remove `inject.body` and `inject.query`, since both let the workload make an upstream keep and echo the value | keep `inject.query` for a Secret with no `paths` and no `confirm`, for APIs that take a key only in the query string |
| D2 | The seam on the proxy door | `pkg/egress.Gateway.Forward`, one optional field | `internal/egressd` terminates TLS itself with a leaf-minting method `pkg/egress.CA` exports, and `pkg/egress.Gateway` leaves the egress role |
| D3 | How an allowance is consumed exactly once | a claim round trip to the control plane on each request a pattern refuses; allowances stay out of the map | allowances travel in the map and each gateway consumes locally, which is exact only with one gateway per sandbox |
| D4 | The action that guards refusals and allowances | a new `sandbox.confirm`, so an authorizer can let a subject answer refusals without editing the sandbox | reuse `sandbox.read` for the list and `sandbox.update` for allowances, with the same workload rule |
| D5 | The upstream answer the gateway recognizes | the shared envelope's `error.code`, and a flat top-level `code`; `details.action` and `details.resource` copied, bounded | the envelope only, and nothing copied |
| D6 | A non-canonical or ambiguous path toward a credential's host | forwarded as sent, with no credential | refused by the gateway with 400 |
| D7 | The conflict code | renamed `secret_scope_conflict` | keep `secret_host_conflict` with the new sentence |
| D8 | Case and parameters in paths | a prefix is selected only when every reading agrees, folded and cut ones included, a pattern matches in any, an allowance matches the raw path exactly | compare the raw path alone, accepting that a server routing without case reads `/Billing` as `/billing` and one stripping parameters reads `/billing;v=2` as `/billing` |
| D9 | The approval trail and the sandbox | a workload caller receives no refusal, no allowance, and no `sandbox.allowance_*` event | the events stay readable under `sandbox.read`, since they grant nothing |
| D10 | The refusal's id in the 403 body | absent | present, for a client that correlates the sandbox's output with the list |
| D11 | Repeated refusals of one method, host and path under a flood | none: the ring's cap and `cursor_expired` bound them | coalesce into one refusal with a count, which a poller of `after` would not see change |
| D12 | A scope conflict a Secret update creates on a running sandbox | both entries leave that sandbox's map and are `notInjectable` | refuse the update, which needs the Secret's owner to see sandboxes it does not own |

### Order of work

| Slice | Delivers |
|---|---|
| 1 | the `pkg` seam; `scope.paths`, the canonical path and readings, selection, one header, the termination and host rules, `secret_scope_conflict`, the version 2 map fields |
| 2 | patterns on both kinds, the refusal and its store and route, the claim frames, allowances and their events |
| 3 | refusals read from upstream services |

## Not in this spec

| Item | Why |
|---|---|
| A pattern over anything but method, host and path: a header value, a body field, a GraphQL operation | the gateway reads no body, and one grammar keeps one matcher |
| Binding an allowance to a query string or a body | an owner who confirms `DELETE /billing/invoices/42` confirms it with any query string; a digest of the query in the refusal and the allowance would narrow it, and is open |
| Coalescing repeated refusals | D11 |
| Unicode normalization as a reading | a server that folds full-width forms or applies NFKC to a path is outside the rule; bytes at or above `0x80` compare as bytes |
| Confirmation on a tunneled host | with no credential the gateway neither reads the request nor has one to withhold |
| Two secrets on one host and prefix writing two different headers | some services take two keys; refused as a conflict here, as two secrets on one host are today |
| The `cella` command's verbs for refusals and allowances | the client package carries them first |
| How a client turns a refusal into a question to a person, and an answer into an allowance | the client's |
| The grant an upstream service holds for its own refusals | the service's and the installation's authorizer's |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A prefix is normalized, a trailing `/` removed, duplicates dropped, sorted; an escape, a `;`, an empty, `.` or `..` segment, a missing leading `/`, more than 32 prefixes, or one over 256 bytes is `invalid_field` at its path; an absent list reads `["/"]`; every normalized prefix is its own canonical path | `TestPathPrefixGrammar`, `TestAPrefixIsItsOwnCanonicalPath` | not built |
| `inject.query` and `inject.body` are `unknown_field`; a stored query secret compiles to no entry and is `notInjectable` on every mounting sandbox; a stored body flag is dropped and the header substituted | `TestInjectIsOneHeader` | not built |
| Two mounted secrets conflict exactly when their hosts overlap, their ports intersect and a prefix is equal ignoring case: `secret_scope_conflict` at resolve naming both, and `ErrSecretScopeConflict` at compile; different prefixes on one host and disjoint ports on one host are accepted | `TestScopeConflictIsOneHostOnePortOnePrefix` | not built |
| A Secret update that gives a running sandbox two conflicting secrets leaves both out of its map and names both `notInjectable` | `TestAConflictARunningSandboxMeetsInjectsNeither` | not built |
| A host-only Secret stored before this spec compiles to `/` and substitutes toward every path of its hosts | `TestAHostOnlySecretCoversEveryPath` | not built |
| Each non-canonical form (`%5C`, a raw `\`, `%00`, a raw control byte, `%25`, `%zz`, a trailing `%4`, `//`, `/a%2F%2Fb`, `/./`, `/../`, `/%2e%2e/`, `/..;/`, `*`) is refused, and each canonical form (`%20`, `%C3%9F`, `%c3%9f`, `%61`, one `%2F` between two segments, a trailing `/`, a raw `;` in a segment) is accepted | `TestCanonicalPath` | not built |
| The longest covering prefix wins on segment boundaries; readings that disagree select nothing; a path no prefix covers selects nothing; no request selects two entries | `TestLongestPrefixWins`, `TestReadingsThatDisagreeSelectNothing` | not built |
| On both doors, against an upstream that reports which value reached which path, every path trick of the canonical table leaves the upstream with the value of the path's own prefix or none, never another prefix's | `TestAPathTrickCannotMoveACredential` | not built |
| The upstream receives the escaped path the gateway judged, byte for byte, on both doors | `TestTheUpstreamReceivesTheJudgedPath` | not built |
| Only the selected entry's placeholder in its one header is replaced; the same placeholder in another header, the query string and the body, and every other entry's placeholder anywhere, reach the upstream verbatim, on both doors | `TestSubstitutionIsOneHeaderOnly` | not built |
| A request to a credential's host that selects no entry is forwarded as sent, and its record names no secret and carries the reason where one applies | `TestARequestNoPrefixCoversGoesOutAsSent` | not built |
| The proxy door terminates a host and port some entry is scoped to and tunnels every other; a pattern that names a host no entry is scoped to never terminates it | `TestTerminationFollowsTheCredentialsHostAndPort` | not built |
| On a terminated connection a `Host`, an `:authority`, or an absolute-form authority naming another host is answered 421 and nothing is dialed; `CONNECT` inside is 405; the upstream's server name is the tunnel's host | `TestATerminatedRequestIsHeldToItsTunnelsHost` | not built |
| Requests on one HTTP/1.1 connection and streams on one HTTP/2 connection toward different prefixes each carry their own prefix's value and each yield one record | `TestEachRequestOnATunnelIsJudgedAlone` | not built |
| The pattern grammar: methods and `*`, hosts and `*`, `*` within a segment, `**` as a segment, normalization; `**` within a segment, `?`, a query, a missing path, a Secret pattern whose host overlaps no scope host, more than 32 patterns, or one over 512 bytes is `invalid_field`; a sandbox pattern no mounted host overlaps is a warning | `TestConfirmPatternGrammar` | not built |
| A request carrying a swapped-in credential that matches a secret's or the sandbox's pattern is answered 403 with the `confirmation_required` envelope naming no pattern and no refusal id, is not forwarded, and yields one refusal with the fields named; the same request without the placeholder is forwarded and yields none | `TestAConfirmPatternRefusesACredentialRequest` | not built |
| A lower-case method, `HEAD` under a `GET` pattern, each of the three override headers, and `_method` match; two distinct override values leave the method undefined and no claim is sent | `TestMethodOverridesAreMatched` | not built |
| A workload may add a sandbox pattern and may not remove one; the owner may do both; a child's list holds its parent's | `TestConfirmPatternsNarrowOnly` | not built |
| Refusals are read oldest first after `after`, with `next` the last id and `limit` bounded; an evicted or unknown `after` is `cursor_expired`; a refusal sent twice is stored once; connection records do not evict refusals | `TestRefusalsAreReadAfterAnID` | not built |
| A workload caller, of its own sandbox or another, is `forbidden` on the refusal and allowance routes under the owner policy and under an authorizer that allows everything; the owner is admitted; another subject is decided by the authorizer | `TestAWorkloadCannotReadOrAnswerRefusals` | not built |
| Placement holds the method, host, path and `ttl` rules; a `ttl` above `5m` is `invalid_field`; a seventeenth live allowance is `ceiling_exceeded`; an upstream refusal or a mismatched one is `invalid_field`; the authorizer's `resource` carries `egress` with the selected secrets | `TestPlacingAnAllowance` | not built |
| One allowance admits exactly one request: two gateways of one environment claiming it at once receive one id, and the second request is refused and recorded | `TestAnAllowanceAdmitsOneRequest` | not built |
| An allowance is exact: another method, host or path, a case variant of the path, and a truncated path are refused | `TestAnAllowanceIsExact` | not built |
| An allowance past `expiresAt` admits nothing and the reaper removes it; a revoked one admits nothing; no stream, or no answer within `CELLA_EGRESS_ACK_TIMEOUT`, refuses | `TestAnAllowanceExpiresAndIsRevoked`, `TestAClaimWithoutAnAnswerRefuses` | not built |
| `sandbox.allowance_placed`, `_consumed`, `_expired` and `_revoked` are signed, carry the subject and data named, reach the sink, and are left out of a workload caller's feed | `TestAllowanceEventsAreTheOwnersTrail` | not built |
| An upstream 403 `confirmation_required`, in the envelope and in the flat shape, to a request carrying a swapped-in credential yields one refusal with `source: upstream` and the upstream's action and resource bounded, and the sandbox receives the answer byte for byte; a longer body, another type, another status, or a request without a credential yields none | `TestUpstreamRefusalsAreRecorded`, `TestAWorkloadCannotFillItsOwnersRefusals` | not built |
| A version 1 hello is refused by a version 2 control plane and a version 1 answer by a version 2 gateway; `claim`, `claimed`, `refusal` and `stored` round-trip; an unknown frame is refused; unanswered refusals are sent again on the next stream; a claim or a refusal for a principal outside the stream's environment is answered empty or dropped | `TestTheSyncProtocolIsVersion2` | not built |
| A clone secret's scope covers the clone URL's host and path, else `secret_out_of_scope` | `TestTheCloneSecretCoversTheClonePath` | not built |
| The refusal, allowance and dropped counters move with their labels, and `refused` appears on the connections counter | `TestRefusalAndAllowanceCounters` | not built |
| The gateway's identity is unchanged: a request on either door without the sandbox's own gateway credential is refused before any dial, and that credential does not change while the workload token rotates | `TestProxyDoorNeedsTheSandboxsOwnCredential`, `TestReverseDoor`, `TestCredentialAuthenticate` | passing, from [[039-egress-gateway]]; this spec keeps them |
| `egress` imports no package that dials, with the matching rule in it | `TestRootPackagesDialNothing` | passing, from [[039-egress-gateway]]; this spec keeps it |
