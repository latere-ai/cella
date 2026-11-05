# Security

What an installation of Cella guarantees the people who call it, what
the control plane can see, and what whoever runs `cellad` must keep
safe. It is for an operator and for a builder who calls the API. The
commitments, the table of each control with the test that holds it,
what is out of scope, and how to report a vulnerability are in
[`SECURITY.md`](../SECURITY.md).

## What Cella guarantees

Every `/v1` request carries a token from an issuer you listed or one
the control plane minted, and nothing acts before the authorizer has
decided. A refused object answers exactly as a missing one, so a caller
learns nothing about what it may not see.
[Authentication](api.md#authentication) says which tokens are accepted,
and [Writing an authorizer](authorizer.md) says what your endpoint
decides.

A decision the authorizer cannot give is a refusal, never an allow. A
late, malformed or missing answer refuses the request, and so does an
unavailable endpoint. [Refusals](api.md#refusals) lists the codes.

A sandbox reaches the network only through its egress gateway, which
admits what the manifest's mode and host lists allow, on every
environment that enforces egress. [What a sandbox can
reach](kubernetes.md#what-a-sandbox-can-reach) describes the boundary
on Kubernetes, and [What it does not defend](#what-it-does-not-defend)
names the environments that do not enforce it.

A workload token identifies one sandbox, and the control plane refuses
it once that sandbox is gone. [Authentication](api.md#authentication)
lists what it reaches.

[`SECURITY.md`](../SECURITY.md) names the test behind each of these.

## What Cella sees

The control plane keeps state in one of two places. With
`CELLA_DB_URL` set it is Postgres. Without it, it is a snapshot file
under `CELLA_DATA_DIR` and memory, held by one process
([State](configuration.md#state)). Beyond the store, `cellad` writes
logs, metrics and traces, sends records to the sink you name, and asks
your authorizer and admission endpoint questions. A reader of one of
these sees what that one carries and nothing from the others.

| What | Kept in | Who can read it |
|---|---|---|
| Sandboxes, secrets, environments and volumes | Postgres, in one table of objects: each row has its kind, id, owner, name, environment, phase and labels, the resolved object as JSON, the status the control plane writes, the desired state it last applied, a version and its times. The manifest a caller wrote is in it, including `command`, `args`, the image and the labels | whoever reads the database; callers see them through the API as the authorizer's decisions allow |
| A sandbox's environment values and gateway credential | Postgres, taken out of the row and sealed together under `CELLA_SECRET_KEY`. Without the key they stay in the row in the clear, and `cellad serve` says so at start | with the key, whoever holds the database and the key. Without it, whoever reads the database |
| A secret's value | Postgres, in its own table: a data key per secret under AES-256-GCM, wrapped by `CELLA_SECRET_KEY`. The secret's object is stored without the value, and a `Secret` is refused when no key is set | whoever holds the database and the key. The API never returns the value |
| What the runtime reports | Postgres, a table the control plane rebuilds from the runtime's own list | whoever reads the database |
| The journal | Postgres: one row per change and per operation, in the shape [a record](events.md#what-a-record-says) has, with the time of each delivery attempt | whoever reads the database; callers read it through the events routes as the authorizer allows |
| Leases, revocations, spawn counts and keys | Postgres: the lease name, its holder and address, and its expiry. The `jti` and expiry of each revoked token. A count of the children each sandbox has created. For each environment key, its `jti`, environment, subject, mint time, expiry and revocation time. The key itself is not stored | whoever reads the database |
| The snapshot, without a database | a file under `CELLA_DATA_DIR/controller`, rewritten whole on each change, holding sandboxes, secrets, environments, volumes and spawn counts. A secret value is sealed in it when `CELLA_SECRET_KEY` is set. The sandboxes are written as the controller holds them, so a sandbox's environment values and gateway credential are not sealed there | whoever can read that directory |
| A sandbox's gateway connections | the memory of the control plane, never the database. One record per connection: the sandbox, the time, the door, the host and port, the decision and the rule that made it, the names of the secrets substituted, the method, path and status where the gateway saw them, the bytes each way and the duration. The path is cut at 512 bytes and carries no query string. A record has no header, body, secret value or placeholder | a caller allowed `sandbox.read` on the sandbox, at `GET /v1/sandboxes/{id}/egress` |
| What the gateway holds | the gateway's memory: for each sandbox in its environment, its boundary, its credential and the value of each secret it may substitute | whoever runs the gateway's host |
| Request logs | JSON on standard error, one line per request: the route pattern, the status class, the error code, the duration, the subject, the sandbox the route names, the request id and the trace id. One more line when a stream opens and one when it closes. None per frame and none per egress connection | whoever reads standard error. With a collector set, the lines also go there |
| Metrics | `/metrics` on the internal listener: counts and timings by route pattern, status class and error code, sandbox counts by phase, environment and driver, and connection counts by decision and door. No sandbox id, subject, name or path is a label | whoever can reach the internal listener |
| Traces | with a collector set, one span per request, named for the method and route pattern, carrying the subject, the request id and the sandbox the route names, as the caller wrote it (an id or a name). The calls to your authorizer and admission endpoint are child spans | whoever reads your collector |
| What the sink receives | one signed record per change and per operation: [what a record says](events.md#what-a-record-says) | the sink you run |
| What the authorizer is asked | on every decision: the subject, its issuer and `sub`, every claim of the token as it arrived, the action, the resource (its kind and id, and for a sandbox its name, owner, environment, parent, root and labels), and the request's id, peer address and user agent. For a caller that is a sandbox, the sandbox's id, parent, root, environment, mesh and spawn budget, use and depth. On an update, and on a volume create, the proposed owner, metadata and spec as well, so a sandbox update sends its `spec.env` values | the authorizer you run |
| What the admission endpoint is asked | the subject, issuer, `sub` and claims, the action (`create` or `update`), the whole defaulted manifest including its environment values, the current object on an update, the calling sandbox's status when a sandbox applies, a summary of the environment (id, name, isolation, capabilities) and the request id. No peer address and no user agent | the admission endpoint you run |
| What a workload token carries | `iss`, `sub` naming the sandbox, `aud`, `iat`, `exp`, a `jti`, the `environment`, and a `spawn` member with its budget, depth and mesh name. No claim that grants membership or a role | whoever can read `/run/cella/token` in the sandbox, which is the workload |

A record the control plane writes holds no `exec` command, file
content, environment value, secret value or token. That rule is in how each type's
`data` is built: an `exec` record has the exit code and the duration,
and `files` has the operation, the workspace paths and the bytes. A
`sandbox.created` record is the one that reduces the manifest: it names
the image, `command`, `args`, working directory, user, resources,
workspace and lifecycle, the keys of the environment (not the values),
the names of mounted secrets, and the labels. A redaction pass runs over
every record's data behind that rule.

Logs pass one redacting handler before either destination sees them. An
attribute whose key contains `env`, `value`, `token`, `credential`,
`secret` or `authorization` is replaced by `[redacted]`, and an egress
placeholder anywhere in a message or value is replaced the same way. The
rule is wide on purpose, so a few names that are not secret are
redacted as well ([Logs](observability.md#logs)). The rule is on keys and
on the placeholder shape, so a secret value written into a log message
by new code would not be caught by it.

## How long it keeps things

| What | How long |
|---|---|
| A sandbox's row in Postgres | A delete marks the row deleted and takes the environment values and gateway credential out of it, sealed or not, in the same statement. The rest of the row, with its manifest, labels and status, stays. Nothing in `cellad` removes it, so it has no end |
| A sandbox in the snapshot | gone from the file at the first rewrite after the delete |
| A secret's value | deleted with the secret. The secret's own row stays in Postgres, marked deleted and without a value, with no end |
| The journal, with a database | a delivered or dropped record older than `CELLA_JOURNAL_RETENTION` (30 days by default, at least one hour) is removed on the reaper's tick. A record the sink has not taken is kept whatever its age, until `CELLA_EVENTS_RETRY_WINDOW` (24 hours by default) passes and it is dropped. Each object's newest record is always kept, so a deleted sandbox's last record has no end |
| The journal, without a database | each object keeps its newest `CELLA_JOURNAL_CAP` records (1000 by default), and none survive a restart |
| A sandbox's gateway connections | removed when the sandbox is deleted. Until then, the newest `CELLA_EGRESS_RECORDS_CAP` per sandbox (1000 by default). A restart loses them. A record that arrives after the delete is kept until the next restart |
| What a gateway holds for a sandbox | dropped, secret values included, when the control plane says the sandbox is gone, or when a snapshot after a reconnect leaves it out |
| A workload token | revoked when its sandbox is deleted. Its revocation row stays until the token's own expiry and is removed on the reaper's tick |
| An environment key's row | removed on the reaper's tick once the key's expiry has passed, whether or not it was revoked. A key lives `CELLA_ENVIRONMENT_KEY_TTL` (8760 hours by default) |
| A sandbox's spawn count | removed when the sandbox is deleted |
| A cached allow from your authorizer | in each replica's memory for the answer's `ttl`, at most 600 seconds, or `CELLA_AUTHORIZER_CACHE` (60 seconds by default, also at most 600) when the answer names none. A deny is kept for 5 seconds |
| Logs, traces and metrics | `cellad` removes none. Standard error, your collector and your scraper keep them as you configured them |
| Records at your sink, and questions at your authorizer and admission endpoint | whatever you write those to do |

[How long records are kept](events.md#how-long-records-are-kept) covers
the journal, and [State](configuration.md#state) covers the key.

## What it does not defend

[`SECURITY.md`](../SECURITY.md#what-is-not-defended) lists it in short.
This is what each position can still do, in full.

**A workload inside a sandbox.**

- On the native runtime it can do whatever the account running `cellad`
  can. That runtime enforces no boundary and no egress, and a manifest's
  boundary is recorded and not enforced. [Running it locally](local.md)
  says what it takes from a manifest.
- Outside Kubernetes, egress is not confined to the gateway. A process
  that ignores the proxy variables reaches whatever its network allows.
  On Kubernetes the same holds until the control plane is told which Pods
  are the gateway, and until then the sandbox reports `EgressEnforced`
  false. [Sandboxes on Kubernetes](kubernetes.md#what-a-sandbox-can-reach)
  has the setup.
- It holds its placeholder and not the value, and a request it sends
  toward a host the secret names leaves with the value in it. A secret is
  as safe as the hosts its owner scoped it to.
- It holds credentials it receives by any other route than a `Secret`.
  Those are not protected.
- On the local runtime with `CELLA_LOCAL_NETWORK=any`, everything
  readable in a granted folder can be sent to any public host. See [The
  network](local-runtime.md#the-network).
- A kernel escape the isolation class does not stop is out of scope, as
  are side channels between sandboxes on one node.
- With its own token it reads and runs commands in its own sandbox, reads
  the sandboxes below it, and creates children inside its budget.

**A caller with a valid token.** It does what your authorizer allows its
subject, and nothing else. With no authorizer the built-in owner policy
applies: it creates anything but an environment, acts on what it owns,
and, if listed in `CELLA_ADMIN_SUBJECTS`, acts on everything. An
authorizer that allows everything allows everything, and `cellad check`
fails an installation whose endpoint allows the reserved probe object.
A permission you revoke is still honored by a replica for as long as it
cached the allow, up to the window in [How long it keeps
things](#how-long-it-keeps-things). A caller's token is accepted up to 24
hours after it was minted.

**A network peer of the control plane.** `cellad` serves plain HTTP and
has no TLS listener of its own, so a peer on the path reads every bearer
and every body unless TLS ends in front of it. Put TLS in front of the
public listener. Worker, gateway and client URLs must be `https://`
unless they are loopback, and an authorizer, admission or sink URL must
be `https://` off loopback. A peer that can reach the internal listener
reads `/metrics` and the probes, and nothing protects that listener but
not routing to it. The base's NetworkPolicy admits the public listener
and leaves the internal one out. Cella has no defense against a peer that
floods it beyond what [the API](api.md#refusals) refuses.

**A self-hosted worker's operator.** The worker runs the sandboxes of
its environment on its own host, so its operator sees everything in them:
their files, commands, output, environment values and workload tokens. A
gateway of that environment holds the secret values of the sandboxes
placed there. What the key reaches is only that environment's
registration and stream, and an operation carries only that
environment's sandboxes, so it does not reach another environment's
sandboxes, the store or the secrets bound for another environment.
[Mint the key](workers.md#mint-the-key) and [What a worker never
does](workers.md#what-a-worker-never-does) say more. A worker accepts no
inbound connection ([The network rule](workers.md#the-network-rule)).
An operator who holds a gateway's authority can read the TLS of every
sandbox behind it.

## What an operator keeps safe

`cellad` reads each of these once, at start. The control plane's host
holds the signing key, the secret key and the bearers it sends to your
endpoints, which makes it the root of the installation. A compromised
host gives every sandbox identity and every secret value. Protect it
first.

| What | Why |
|---|---|
| `CELLA_TOKEN_KEY` | one or two RSA private keys. The first signs every sandbox's workload token and every environment key. Whoever holds it can mint an identity for any sandbox, and a key for any environment, which a worker or a gateway then accepts. Rotate by putting a new key first and removing the old one once the tokens it signed have expired ([Identity](configuration.md#identity)) |
| `CELLA_SECRET_KEY` | 32 bytes. It wraps the data key of every secret value, and with a database, seals each sandbox's environment values and gateway credential. Whoever holds it and the database opens every secret value. Once the database holds anything sealed, `cellad serve` refuses to start without it or with another key. `cellad rotate` and `cellad unseal` change it and read `CELLA_SECRET_KEY_NEXT` ([Changing the key](configuration.md#changing-the-key-and-going-back-to-a-release-that-does-not-seal)) |
| `CELLA_AUTHORIZER_TOKEN` | the bearer `cellad` sends your authorizer. Whoever holds it can put questions to your endpoint as the control plane. It is required with `CELLA_AUTHORIZER_URL` |
| `CELLA_ADMISSION_TOKEN` | the same for your admission endpoint, required with `CELLA_ADMISSION_URL`. Your endpoint sees the manifests, environment values included, so keep it behind this token |
| `CELLA_EVENTS_SECRET` | the secret or the two secrets that sign each delivery. A delivery has no `Authorization` header: the signature is the credential. Whoever holds the secret can forge records to your sink. Two are accepted so one can rotate with no record lost ([Events and the journal](configuration.md#events-and-the-journal)) |
| `CELLA_ENVIRONMENT_KEY` | read by `cellad worker` and `cellad egress`. It authorizes one environment's registration and stream, and a gateway's map of every secret value bound for it. Mint one per environment, revoke one that leaks, and note it lives `CELLA_ENVIRONMENT_KEY_TTL` ([Mint the key](workers.md#mint-the-key)) |
| `CELLA_EGRESS_CA_KEY` | the certificate and private key a gateway terminates TLS with, and every sandbox behind it trusts. Whoever holds it can intercept the TLS of those sandboxes. Unset, the gateway generates one that lives as long as the process ([The egress gateway](configuration.md#the-egress-gateway)) |
| `CELLA_EGRESS_PRIVATE_RANGES` | the private ranges a gateway dials. Unset, it connects only to public addresses. A sandbox whose boundary admits a host in a range you name reaches every service in it, so name the narrowest ranges |
| The database, `CELLA_DB_URL` and `CELLA_DB_POOL_URL` | the URL carries the database password. The database holds what [What Cella sees](#what-cella-sees) lists, including every sandbox's manifest. Without `CELLA_SECRET_KEY` it holds environment values and gateway credentials in the clear |
| `CELLA_DATA_DIR` | without a database it holds the snapshot, with environment values and gateway credentials unsealed, and, with the native runtime, each sandbox's files and token |
| `OTEL_EXPORTER_OTLP_HEADERS` | the credential for your collector, which receives the logs and traces above ([Observability](observability.md)) |
| `CELLA_ADMIN_SUBJECTS` | each subject listed acts on every object. Read only when no authorizer endpoint is set |
| `CELLA_OIDC_ISSUERS`, `CELLA_OIDC_AUDIENCE` | an issuer that signs a token for the wrong subject lets its holder act as that person. List only issuers you trust that far |
| `CELLA_OIDC_INSECURE_ISSUERS`, `CELLA_EVENTS_INSECURE_SINK`, `CELLA_INSECURE_CONTROL_PLANE` | each admits plain `http://` on a host that is not loopback. For a test stack only. Leave them unset |
| `CELLA_ALLOW_UNSAFE_NATIVE` | the consent the native runtime requires. It runs sandboxes as host processes with no isolation, so set it only for code you trust |
| The internal listener, `CELLA_INTERNAL_ADDR` | it serves the probes and `/metrics`, and nothing protects it but not routing to it. Do not publish its port |
| TLS in front of the public listener | `cellad` serves plain HTTP. Terminate TLS at your ingress and set `CELLA_PUBLIC_URL` to an `https` origin |

Run `cellad check` after any change to the authorizer, admission or
sink endpoint ([Check the installation](install.md#check-the-installation)).
