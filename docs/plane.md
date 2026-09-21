# Building a plane on Cella

You sell sandboxes. You have accounts, plans, a console, and rules of your
own about who may run what. Cella is the control plane underneath that: the
manifest contract, the drivers, the lifecycle, the boundary. This page is
how to build on it without forking it.

There are two doors. Most platforms walk through the first and later add
the second, and nothing is rewritten in between: the packages are what
`cellad` is made of.

| Door | You run | You write | You get |
|---|---|---|---|
| webhooks | `cellad` as a service | an authorizer, an admission endpoint, an event sink, and the identity provider you already have | the whole core, upgraded by image tag, with your logic in your own service in any language |
| packages | your own binary that imports `manifest`, `runtime`, `controller` and `egress` | the server around them, your own identity and your own store | the contract and the drivers in-process, no HTTP hop, and an API of whatever shape you want |

## Door one: run cellad and answer three questions

`cellad` asks your services three questions and takes no policy of its own.

### Who may do what

`CELLA_AUTHORIZER_URL` points at an endpoint that answers one question per
request: may this subject take this action on this object? Import
`latere.ai/x/cella/authorizer` for the action table and
`latere.ai/x/pkg/authz/server` for the envelope, and the endpoint is about
twenty lines:

```go
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/server"
)

type plans struct{}

func (plans) Decide(_ context.Context, req authz.Request) (authz.Decision, error) {
	if req.Subject == "" || req.Resource.ID == authz.ProbeID {
		return authz.Decision{Reason: "no_subject"}, nil // the reserved probe is always denied
	}
	if owner := req.Resource.String("owner"); owner != "" && owner != req.Subject {
		return authz.Decision{Reason: "not_the_owner"}, nil
	}
	return authz.Decision{Allow: true, TTL: time.Minute}, nil
}

func main() {
	http.ListenAndServe(":8081", server.New(server.Options{
		Bearer:     os.Getenv("AUTHZ_TOKEN"),
		Vocabulary: authorizer.Vocabulary(),
		Decider:    plans{},
	}))
}
```

Three rules the core holds you to. Deny the reserved probe id: an endpoint
that allows it is one that does not read the request, and `cellad check`
fails on it. Answer a deny as a decision and not as an error, because an
error is read as "no decision" and refuses the request either way. Carry
your plan's ceilings on the allow, as `authorizer.WireLimits`, rather than
enforcing them in a proxy in front.

### What a manifest becomes

`CELLA_ADMISSION_URL` points at an endpoint that sees a resolved manifest
before it is created and returns the manifest to continue with. It is where
your image catalogue lives, where you stamp your own labels, and where you
translate a field of your own vocabulary into the kinds the core has. It
answers with the object, a list of warnings for the caller, or a refusal.
What it returns is validated again, so an endpoint that writes a field the
schema does not have is a refusal and not a surprise three steps later.

### What happened

`CELLA_EVENTS_URL` points at your sink, which receives one signed record per
mutation and per operation, ordered per object. That is your audit log and
your usage meter. Verify the signature with the secret you configured, and
answer 2xx once you have the record; the control plane retries what you did
not acknowledge.

## Door two: import the packages

A server of your own imports four packages and puts its own API and its own
identity around them. The whole of it:

```go
object, err := manifest.Decode(document, contentType)   // the schema
resolved, err := manifest.Resolve(ctx, &object, options) // defaults, admission, ceilings, boundary
created, err := core.Create(ctx, resolved.Sandbox, subject, plan.Sandboxes)
```

`manifest.Options` is where your platform goes: `Actor` is who is applying,
as your own identity rendered it; `Defaults` is what an absent field takes;
`Ceilings` is what the subject's plan may not exceed; `Admit` is the
admission step as a function in your process rather than a webhook. Nothing
else in the core reads a claim or a plan.

A complete, compiling server is in [`examples/plane/`](../examples/plane).
It is under three hundred lines: a native driver, a controller, three routes,
and the three places a platform fills, each marked. Run it with

```sh
go run ./examples/plane
```

and it listens on `127.0.0.1:8080` with its own basic-auth subject, its own
plans, and its own image catalogue.

## Where each concern goes

| Concern | Through the webhooks | Through the packages |
|---|---|---|
| accounts and organisations | the claims of your issuer, read by your authorizer | your own middleware, before `Resolve` |
| plans and quotas | the limits on an allow, and the ceilings your admission endpoint applies | `Options.Ceilings` and the count you pass to `Create` |
| an image catalogue | admission rewrites the image | an admission function in `Options` |
| secrets | the `Secret` kind holds the value; your authorizer decides who may mount one | the same kind through the store you construct |
| audit and usage | the event sink | your own implementation of the controller's event seam |
| a console | reads `/v1` | reads your own API |
| more than one region | one `cellad` per region behind your router | one controller per region |

## What the core promises you

The manifest contract's rules: a manifest accepted by one build of this
version is accepted by every later one and resolves to the same object,
defaults aside. The exported packages are additive within a major version.
The conformance suite passes against `cellad` on every release, and a
platform that passes it against its own front serves the same contract:

```sh
go test -tags=e2e -run '^TestContract$' ./test/conformance -args \
  -url https://your-plane.example -token "$TOKEN"
```

It does not promise the shape of anything under `internal/`, the test stub
binaries, or the deploy manifests beyond what a release carries.

## Migrating a platform that already has a manifest

Keep the group. A field that named one of your own services becomes the
kind the control plane has for it, a `Secret` for a vault entry, or an
annotation under your own prefix for what stays yours, and your admission
endpoint does the translation at the edge. The
control plane accepts one schema and carries no alias for a field, which is
what keeps two spellings of one thing from outliving the migration.
