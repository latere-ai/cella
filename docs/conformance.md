# Conformance

The `/v1` API is a contract, and the contract is a suite. `test/conformance`
runs one case per rule the API states against a server you point it at, and
prints which held, which failed with the request and the answer that
disagreed, and which were skipped and why.

Run it to check an installation of your own, or to check a server you wrote
that serves the same API.

## Run it against an installation

```sh
go test -tags=e2e -count=1 -timeout 30m -v -run '^TestContract$' ./test/conformance -args \
  -url https://cella.example.com \
  -token "$CELLA_TOKEN" \
  -capabilities files,attach
```

`-count=1` makes every run ask the server: without it, `go test` may replay
an earlier pass from its build cache. `-timeout 30m` leaves room for a run
against a cluster, which can take longer than the ten minutes `go test` allows
by default.

The run creates objects named `conformance-<run>-<n>`, records every id it
made, and deletes those ids and only those. It never deletes by name pattern
and never lists and removes what it did not create, so it is safe against a
server that is carrying other work. Two runs against one server do not meet.

Each case has two minutes. A whole run against a server with no image to pull
takes a few minutes; with images and a cluster, up to fifteen.

## What to give it

| Flag | What it is | Left out |
|---|---|---|
| `-url` | the server under test | the run skips and says so |
| `-token` | the caller's bearer | supply `-issuer` instead |
| `-issuer` | an issuer with a `POST /mint` route, so the run takes a token per subject | the cases that need a second subject skip |
| `-admin` | a bearer the server treats as an administrator | the environment cases skip |
| `-image` | the image every case creates from | the manifests carry a command and no image |
| `-capabilities` | what this environment provides, comma separated: `files`, `attach`, `dial`, `display`, `input`, `volumes`, `mesh`, `egress` | every capability case skips |
| `-sink` | the event sink's address, whose `/events` route the run reads back | the delivery case skips |
| `-authorizer-control`, `-admission-control` | the control address of the permission service and the policy service, which the run drives to reach their refusals and their outages | those cases skip |
| `-display-image`, `-upstream`, `-queued-environment`, `-worker-environment`, `-cella` | the inputs of the desktop, the network boundary, queued work, a second environment, and the command line client. `-upstream` is a `host:port` a sandbox may be allowed to reach, answering any request with at least one byte; with `egress` declared, the run checks that a sandbox reaches it through its gateway, is refused a host its allow list does not name, and reaches nothing around the gateway, using `nc` and `base64` inside the sandbox's image | each group skips naming what is missing |
| `-known` | a file declaring the cases this server fails and why | every failure is a failure |
| `-skip` | group or case names to leave out | nothing is left out |

Every flag has an environment variable of the same meaning, `CELLA_TEST_URL`,
`CELLA_TEST_TOKEN` and so on, so a stack that exports them runs the command
with no flag at all.

## Reading the report

```
conformance suite 1 against server v0.3.0

decode
  pass 003/UnsupportedVersion             1ms
  known 008/NotAcceptable                 1ms  no content negotiation yet
  fail 008/BodyTooLarge                   2ms
       POST /v1/sandboxes
         want: status 413
         got:  status 201
         body: {"apiVersion":"cella.latere.ai/v1beta1",...}

36 passed, 1 failed, 8 skipped, 1 declared gap(s), 34 object(s) created and deleted
```

The first line is the marker: the version the server reports and the version
of the suite. Two reports are comparable only when both match.

A case is one of four things.

- **pass**: the server answered what the API states.
- **fail**: it did not, and the lines under it are the call, what the rule
  says, and what arrived. Every failure is one exchange, so a report is a
  list of things to fix and not a stack trace.
- **skip**: an input or a capability was not supplied, named in the reason. A
  skipped case is never a pass; it is a case that could not be asked.
- **declared gap**: the server says it fails this case, with a reason, in the
  file `-known` names. The run stays green and the report names each one.

## Declaring what you do not serve yet

A server that is honest about an unfinished route can say so:

```json
{
  "note": "what this server does not answer yet",
  "cases": {
    "case008ExecStream": "the framed exec stream is not served; the bounded form is"
  }
}
```

The declaration is exact in both directions. A case that fails without being
declared fails the run, and a case that is declared and passes fails it too,
so a line cannot outlive the gap it describes. Every line needs a reason: a
declaration with no reason is refused when the file is read.

## Capabilities

An environment provides what its runtime can do, and the API refuses a route
whose capability it does not provide. Tell the suite what this environment
declares and it checks both directions: a route whose capability is declared
has to work, and a route whose capability is not declared has to answer *the
environment cannot provide this* rather than a surprise.

A capability you do not pass is a group that skips. Declaring one the server
does not honor is a failure, which is the point: the declaration is what a
caller reads before it writes a manifest.

## Run it from GitHub Actions

The repository carries a workflow, `conformance`, that runs the command above
from a clean checkout against an address you give it. In your fork, or in this
repository if you maintain it:

1. Add a repository secret `CONFORMANCE_TOKEN` holding a bearer the server
   accepts, and optionally `CONFORMANCE_ADMIN_TOKEN` holding an administrator's
   bearer, which the environment cases need.
2. Run the workflow from the Actions tab, or with
   `gh workflow run conformance.yml -f url=https://cella.example.com -f capabilities=files,attach`.
   The `image` input names the image every case creates from.

The bearer is a secret and never an input, because the inputs of a run are
shown to anyone who can read it. The run fails before the suite starts when
`CONFORMANCE_TOKEN` is not set, and otherwise passes or fails with the suite.

## Against a server you wrote

The suite reads the wire and nothing else: the routes, the status codes, the
error envelope with its fixed sentences, the list envelope, the stream
framing. It shares no type with any implementation, so a server written from
the API documentation alone passes it. Run it from a checkout of this
repository against your own address, with your own tokens.

Some defaults are part of the contract and some are yours. A sandbox applied
with no workspace, working directory, egress or spawn fields has to come back
with the fixed values the manifest contract states: the workspace at
`/workspace`, starting empty and used as the working directory, egress `open`,
and no mesh or spawn rights. The defaults an operator chooses, such as the
resources, the lifetimes and the image, differ between installations, and the
suite does not read them.

Two parts are not a black box and no case covers them: the generated API
document matching the served one, and a rate limit across replicas. Their
proofs live with the servers that hold them.
