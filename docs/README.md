# Documentation

For people who run `cellad`, write manifests, call the API, or build a
platform on the packages.

## Trying it

| Page | |
|---|---|
| [Native quickstart](native.md) | `make run`, and a sandbox on your own machine with no cluster and no isolation |
| [The cella command](cli.md) | the client: reaching a control plane, the commands, and what an exit code means |

## Running it

| Page | |
|---|---|
| [Install](install.md) | a cluster, the manifests from a release's deploy archive, the first sandbox, and `cellad check` |
| [Configuration](configuration.md) | every environment variable, its default, and which role reads it |
| [Deploy reference](../deploy/README.md) | what each manifest and each Secret of `deploy/` is, and why the Role grants what it does |
| [Sandboxes on Kubernetes](kubernetes.md) | what a sandbox on a cluster can do, reaching a server inside one, and what the Role has to allow |
| [Self-hosting a data plane](workers.md) | running sandboxes on your own machines against somebody else's control plane: the key, the worker, and the outbound-only rule |
| [Capacity and queues](scheduling.md) | what an environment holds, what happens to a create it cannot fit, priority, preemption, and warm pools |
| [Observability](observability.md) | the scrape surface, traces and logs over OTLP, redaction, and the alert rules |
| [Security](../SECURITY.md) | what the project protects, the test that proves each control, and how to report a vulnerability |

## Building against it

| Page | |
|---|---|
| [Manifests](manifest.md) | every field of `Sandbox`, `Secret`, and `Environment`, with its default |
| [API](api.md) | authentication, addressing, the refusal envelope and its codes, and every `/v1` route with the action it asks for |
| [The Go client](client.md) | driving a control plane from a Go program: connecting, tokens, manifests in JSON or YAML, sessions, files, following events, and the errors you decide on |
| [Following events](events.md) | an object's history, following it live from where you left off, and what a proxy in front of the server must do |
| [Building a plane](plane.md) | a platform on top of Cella: the two doors, the three endpoints you write, the packages in your own binary, and a running example |
| [Conformance](conformance.md) | the suite that checks a server against the API: what to give it, what the report says, and how to declare what you do not serve |

The OpenAPI document is [`api/openapi.yaml`](../api/openapi.yaml), and
every installation serves it at `GET /openapi.yaml`.

## Changing it

[`CONTRIBUTING.md`](../CONTRIBUTING.md) is how to build and test a change
and how the code is organized. The design, and the reasoning behind each
decision, is in [`specs/`](../specs/README.md).
