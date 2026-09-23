# Documentation

For people who run `cellad`, write manifests, or build a platform on the
packages.

## Running it

| Page | |
|---|---|
| [Native quickstart](native.md) | Create and execute trusted local workspaces with the implemented API |
| [The cella command](cli.md) | The client: reaching a plane, the commands, the exit codes, and what a refusal tells you |
| [Install](install.md) | A cluster, the manifests from the release's `deploy-<tag>.tar.gz`, the first sandbox, and `cellad check` |
| [Self-hosting a data plane](workers.md) | Running sandboxes on your own machines against somebody else's control plane: the key, the worker, and the outbound-only rule |
| [Capacity and queues](scheduling.md) | What an environment holds, what happens to a create it cannot fit, and how a queued environment orders what waits |
| Configuration | the table in the [repository scaffold spec](../specs/.archive/002-repository-scaffold.md) until `docs/configuration.md` is generated from the code |
| [Deploy reference](../deploy/README.md) | What each manifest and each Secret of `deploy/` is, and why the Role grants what it does |
| [Observability](observability.md) | The scrape surface and what it carries, traces and logs over OTLP, redaction, and the alert rules |
| Identity | the [section in the README](../README.md#identity) is what an operator needs: the issuers, the authorizer, the signing key and its rotation |

Trying it out before there is anything to install takes one command and
one issuer, `CELLA_OIDC_ISSUERS=<url> make run`: the server on loopback,
serving the native workspace API, probes, and the key set it signs with. `cellad` verifies
every caller, so it does not start without an issuer to verify against;
the stub issuer that makes `make run` self-contained is the
[test stubs spec](../specs/012-test-stubs-and-tiers.md)'s.

## Building against it

| Page | |
|---|---|
| Manifest reference | the schema in the [manifest contract spec](../specs/003-manifest-contract.md) |
| API | the endpoints and error codes in the [API spec](../specs/008-api.md), and the webhooks an operator writes in the [identity](../specs/006-identity.md) and [admission](../specs/007-admission.md) specs |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |
| [Following events](events.md) | An object's history, following it live from where you left off, following everything you can see, and what a proxy in front of the server must do |
| [Building a plane](plane.md) | Selling sandboxes on top of Cella: the two doors, the three endpoints you write, the packages in your own binary, and a running example |
| [Conformance](conformance.md) | The suite that checks a server against the API: what to give it, what the report says, and how to declare what you do not serve yet |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
