# Writing an authorizer

For whoever writes the endpoint `CELLA_AUTHORIZER_URL` points at. Cella
runs the sandboxes and keeps the records. Who may do what is yours: a
plan, a team, a quota, a role. Before a request acts on a sandbox, a
secret, a volume or an environment, `cellad` sends one question to your
endpoint and follows its answer. The endpoint is optional: without one,
`cellad` decides by the [built-in owner policy](#without-an-authorizer).
This page is the contract.

## The question

For each decision, `cellad` sends one request to `CELLA_AUTHORIZER_URL`,
presenting `CELLA_AUTHORIZER_TOKEN` as the bearer. Both are set in the
[configuration](configuration.md#identity), and the URL must be `https://`
unless the host is loopback.

```
POST <CELLA_AUTHORIZER_URL>
Authorization: Bearer <CELLA_AUTHORIZER_TOKEN>
Content-Type: application/json
Accept: application/json
```

A question for `sandbox.create`:

```json
{
  "subject": "https://login.example.com|alice",
  "issuer": "https://login.example.com",
  "sub": "alice",
  "claims": {"aud": "cella", "exp": 1793982589, "iss": "https://login.example.com", "sub": "alice"},
  "action": "sandbox.create",
  "resource": {"environment": "default", "kind": "Sandbox", "labels": {"team": "web"}, "name": "build-7", "owner": "https://login.example.com|alice"},
  "request": {"id": "", "ip": "203.0.113.4", "user_agent": "curl/8.7"}
}
```

- `subject` is the issuer and the `sub` joined by `|`, and `issuer` and
  `sub` are its two halves.
- `claims` is every claim of the caller's token, as it arrived. `cellad`
  reads none of them to decide, so a plan, a team or a role is read here.
- `workload` is present only when the caller is a sandbox, and absent
  otherwise. It carries the sandbox's `id`, `parent`, `root`,
  `environment`, `mesh` and `spawn`.
- `action` is one of [the vocabulary](#the-vocabulary).
- `resource` is one flat object: its `kind`, its `id` when it has one, and
  the fields of that kind. A create carries no `id`, because the object
  does not exist yet, and a list action carries the `kind` and nothing
  else. A field `cellad` does not know for a question is left out rather
  than sent empty.
  - A sandbox carries `name`, `owner`, `environment`, `parent`, `root` and
    `labels`. A secret carries `name`, `owner` and `labels`. A volume
    carries `name`, `owner`, `environment` and `labels`. An environment
    carries `name`, `owner`, `isolation`, `mode`, `workers` and `labels`,
    and adds `ttl` (in seconds) on `environment.key` and `registration`
    (the worker's `driver`, `isolation` and `host`) on
    `environment.serve`.
  - `owner` is the subject that created the object, and for a create it
    is the caller.
  - `proposed` is added on `sandbox.update`, `secret.update`,
    `volume.update` and `volume.create`. It holds the object the request
    would write: its `owner`, its `metadata` and its `spec`. [The
    answer](#updates-and-volume-creates-are-decided-on-the-change)
    says how to read it.
- `request` is the caller's address and user agent. Its `id` is sent
  empty in this release.

An answer is cached on everything in the question except `request.id`
([the answer](#the-answer) says for how long).

## The vocabulary

Thirty-two actions over five kinds. In Go, import
`latere.ai/x/cella/authorizer` for the action names rather than copying the
strings from this page. Every route's row in the [API guide](api.md#routes)
names the action it asks, and the table here groups them by action.

| Action | What it allows | Asked by |
|---|---|---|
| `sandbox.create` | creating a sandbox, or spawning a child of one | `POST /v1/sandboxes`, `PUT /v1/sandboxes/{name}` when the name is free |
| `sandbox.read` | reading a sandbox and what it records | `GET /v1/sandboxes/{id}`, its logs, ports, display and egress records, and each row of a sandbox list and of a sandbox feed |
| `sandbox.update` | changing, starting or stopping a sandbox | `PUT /v1/sandboxes/{name}` when you hold the name, `POST /v1/sandboxes/{id}/start` and `/stop` |
| `sandbox.delete` | deleting a sandbox | `DELETE /v1/sandboxes/{id}` |
| `sandbox.exec` | running a command in it, and reaching inside it | exec, attach, the files routes, port forwarding, dial, screenshot, screen and input |
| `sandbox.token` | in the vocabulary | no route of this release asks it |
| `sandbox.list` | listing sandboxes | `GET /v1/sandboxes`, and opening a feed of every object's changes |
| `secret.create` | creating a secret | `POST /v1/secrets`, `PUT /v1/secrets/{name}` when the name is free |
| `secret.read` | reading a secret without its value | `GET /v1/secrets/{name}`, and each row of a secret list and feed |
| `secret.update` | rotating or changing a secret | `PUT /v1/secrets/{name}` when you hold the name |
| `secret.delete` | deleting a secret | `DELETE /v1/secrets/{name}` |
| `secret.list` | listing secrets | `GET /v1/secrets` |
| `secret.mount` | mounting a secret in a sandbox | a create or apply of a sandbox whose manifest names the secret. A deny is `not_found` |
| `volume.create` | creating a volume | `PUT /v1/volumes/{name}` when the name is free, which also asks `environment.use` |
| `volume.read` | reading a volume | `GET /v1/volumes/{name}`, and each row of a volume list and feed |
| `volume.update` | changing a volume's labels, annotations and `retain` | `PUT /v1/volumes/{name}` when you hold the name |
| `volume.delete` | deleting a volume | `DELETE /v1/volumes/{name}` |
| `volume.list` | listing volumes | `GET /v1/volumes` |
| `volume.attach` | mounting a volume in a sandbox | a create or apply of a sandbox whose manifest names the volume. A deny is `not_found` |
| `volume.snapshot` | in the vocabulary | no route of this release asks it |
| `set.create`, `set.read`, `set.update`, `set.delete`, `set.list` | the actions of the `SandboxSet` kind | no route of this release asks them, because the API does not serve the kind |
| `environment.create` | creating an environment | `POST /v1/environments`, `PUT /v1/environments/{id}` when it is new |
| `environment.read` | reading an environment | `GET /v1/environments/{id}`, and each row of an environment list and feed |
| `environment.update` | changing an environment | `PUT /v1/environments/{id}` when it exists |
| `environment.delete` | deleting an environment | `DELETE /v1/environments/{id}` |
| `environment.list` | listing environments | `GET /v1/environments` |
| `environment.key` | minting, listing and revoking an environment's keys | the three routes under `/v1/environments/{id}/keys` |
| `environment.use` | placing a sandbox or a volume on an environment | a create of a sandbox, other than a spawn, and of a volume. A deny is `not_found` |
| `environment.serve` | a worker serving an environment | `POST /v1/environments/{id}/workers`, asked with the environment key's subject and again on every decision's TTL while the stream lives |

An action acts on exactly one kind: `Sandbox`, `Secret`, `Volume`,
`SandboxSet` or `Environment`, and `set.*` acts on `SandboxSet`.
`GET /v1/events` asks the read of the object's kind for the object it
names. A deny on the action a request asked is `403 forbidden`. A deny on
a lookup, which is `secret.mount`, `volume.attach` and `environment.use`,
is `404 not_found`, the same answer as an object that does not exist, so a
refusal never reveals that something is there. The endpoint's `reason`
reaches the caller as developer detail on the error, never in its message.

## The answer

The answer is `200` either way:

```
200 {"allow": true, "ttl": 60, "limits": {"max_sandboxes": 10}}
200 {"allow": true, "filter": {"owners": ["https://login.example.com|alice"]}}
200 {"allow": false, "reason": "not_the_owner"}
```

| Field | Meaning |
|---|---|
| `allow` | required; the decision |
| `reason` | on a deny, a short reason for the log |
| `ttl` | seconds `cellad` may reuse an allow for the same question; 60 when absent, at most 600. A deny is reused for 5 seconds. A question about a resource with no id, such as a create, is never reused |
| `limits` | the ceilings an allow carries, below |
| `filter` | on a list action, narrows the list to the named `owners` and `labels` |

An answer that failed is never kept, so a recovered endpoint is asked
again on the next request.

### Limits

Import `latere.ai/x/cella/authorizer` and write the figures through
`authorizer.WireLimits`, rather than through string literals. Leave a
figure out for no limit. Four figures are in the type, and two of them are
ones to leave out.

- `max_sandboxes` on the allow of `sandbox.create` and of
  `sandbox.update`. Carry your plan's sandbox limit there rather than
  enforcing it in a proxy in front: a create or a start past it is
  refused with `quota_exceeded`. The limit counts the sandboxes that run,
  so a stopped one holds no slot and starting it again takes one; bound
  what your subjects keep stopped with `ttl` or `autoDelete` from your
  admission endpoint.
- `max_volumes` on the allow of `volume.create`. A create past it is
  refused with `volume_quota_exceeded`. The limit counts every volume the
  subject holds that is not being deleted, a failed one included, so a
  subject frees a slot by deleting a volume.
- `max_priority` is not applied by this release. Leave it out.
- `requests_per_minute` is refused: a request whose allow carries a figure
  above zero is answered `capability_unsupported`. Leave it out.

A figure below zero, or a `limits` object that does not parse, is not a
decision: see [Failing closed](#failing-closed).

### Lists

A list is decided in two steps. The list action (`sandbox.list`,
`secret.list`, `volume.list`, `environment.list`) answers an allow that
may carry a `filter` of `owners` and `labels`. The core holds every object
to that filter, then asks the kind's read (`sandbox.read`, and so on) for
each object the filter lets through, and leaves out the ones the read
refuses. The filter narrows the list and the read decides each object, so
an object your filter excludes is never read. A read that fails to answer
fails the whole list rather than dropping one object from it.

The default environment is the one exception. Every subject may place a
sandbox in it, and it carries no caller's owner or labels, so no filter can
name it. The core lists it whenever your `environment.read` on it allows.
To show a tenant's members their own environments and the shared default,
narrow `environment.list` to the tenant and allow `environment.read` on the
default for them; to hide the default from someone, deny that read, which
hides it from a read by name as well.

### Updates and volume creates are decided on the change

`sandbox.update`, `secret.update` and `volume.update` carry, under
`resource.proposed`, the object the request would write: its `owner`, its
`metadata` and its `spec`. Compare it with the object to refuse a change
you do not allow, such as a label your tenancy is read from being dropped
or rewritten. The proposal's labels are the ones the request sent, because
an apply replaces the object, so a request that leaves a label out
proposes removing it. The body is read and validated before you are asked,
so a request that would be refused for its body never reaches you. On
`secret.update` the rest of the resource is the stored Secret, and the
proposed `spec` never carries the value.

`volume.create` carries `resource.proposed` too, the Volume as it will be
written: the defaults are filled, so `spec.source.kind` is `empty` or
`image` even when the request left it out, and an image source is the
digest-pinned reference your admission endpoint answered. To allow image
volumes of at most 10Gi, refuse a proposal whose `spec.source.kind` is not
`image`, or whose `spec.size` is above `10Gi`. The size is the Kubernetes
quantity the request wrote, such as `10Gi` or `10240Mi`, so compare it
parsed; in Go, `manifest.ParseQuantity` from `latere.ai/x/cella/manifest`
parses it the way the core does.

## Failing closed

An answer `cellad` cannot use is a refusal, and the client sees `503
authorizer_unavailable`. There is no fail-open, and nothing is changed by
a request the endpoint did not allow. These are all refusals:

- an error, or a connection that cannot be made;
- no answer within the timeout, 5 seconds by default, set by
  `CELLA_AUTHORIZER_TIMEOUT`, and covering the retry as well;
- a status other than `200`;
- a body that is not a JSON decision, a body without `allow`, or one
  longer than 64 KiB;
- an allow whose `limits` does not parse, or holds a figure below zero.

`cellad` asks a second time only when the connection failed before any
response began, such as a refused or reset connection, and never after a
response line arrived or after a timeout.

So answer a deny as a decision and not as an error: an error is read as
"no decision" and refuses the request either way. Treat the endpoint's
availability as Cella's: every request that is not answered from the
cache waits on it. Run it near `cellad` and answer from memory.

## Without an authorizer

With `CELLA_AUTHORIZER_URL` unset, `cellad` decides in process by the
built-in owner policy:

- A subject creates anything but an environment, and acts on what it owns.
- The subjects in `CELLA_ADMIN_SUBJECTS`, written `<issuer>|<sub>`, act on
  everything. They alone create, change, delete, key and use
  environments, except that every subject uses the default environment
  (`CELLA_DEFAULT_ENVIRONMENT`). With `CELLA_OWNED_ENVIRONMENTS=worker`
  any subject also creates and acts on worker environments of its own.
- A list is an allow whose filter narrows it to the caller's own objects,
  and an administrator's list has no filter.
- A sandbox's own token reads and execs itself, reads the sandboxes of the
  tree it roots, creates a child and lists. It does nothing else.
- An environment key does one thing: serve its own environment.
- The reserved probe and a request with no subject are denied.

The policy sends no `limits`, so there is no sandbox or volume limit, and
`CELLA_ADMIN_SUBJECTS` is unused once an endpoint is set. It is the right
choice for a single team. Run an authorizer when who may do what is a
decision your product makes.

A token minted from a stored key may do only what its grants name, and
the owner policy applies those grants to its own answer.

## The reserved probe and cellad check

The resource id `00000000-0000-0000-0000-000000000001` is reserved. Deny
it for every subject and every action. Deny the probe even for the
administrator: an endpoint that allows it is one that does not read the
request, and `cellad check` fails on it.

`cellad check` sends one question, `sandbox.read` on a `Sandbox` whose id
is the probe and which carries no subject, to the endpoint with its own
configuration. The `authorizer` line passes when the endpoint answers a
deny and fails when it answers an allow or gives no decision. With no
endpoint set it asks the owner policy, which denies it too. The
[installation check](install.md#check-the-installation) lists the other
lines. Run it before pointing `cellad` at a new endpoint.

## Writing one

Import `latere.ai/x/cella/authorizer` for the action table and
`latere.ai/x/pkg/authz/server` for the envelope, and the endpoint is about
twenty lines:

```go
package main

import (
	"context"
	"log"
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
	endpoint := server.New(server.Options{
		Bearer:     os.Getenv("AUTHZ_TOKEN"),
		Vocabulary: authorizer.Vocabulary(),
		Decider:    plans{},
	})
	log.Fatal(http.ListenAndServe(":8081", endpoint))
}
```

The scaffold checks the bearer, decodes the question, refuses an action
outside the vocabulary, denies the probe id, and applies a token's grants
to your decision.
`latere.ai/x/pkg/authz/conformance` is a test suite your endpoint can run
against the same vocabulary, with `conformance.WithVocabulary(authorizer.Vocabulary())`.

An endpoint in another language is any HTTP server that does what the
scaffold does: check the bearer, read the body of [the
question](#the-question), answer `200` with [the answer](#the-answer),
and deny the probe.
