# Changelog

What changed for whoever writes a manifest, runs `cellad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- A terminal inside a sandbox. `GET /v1/sandboxes/{id}/attach` is a WebSocket
  carrying a shell: the first text frame says what to run and how big the
  window is, binary frames are bytes both ways, `{"resize":{"cols","rows"}}`
  changes the window, and the session ends with `{"exit": n}` and a normal
  close. `GET /v1/sandboxes/{id}/exec` is the same socket for one command: give
  it a column and a row count for a terminal, or neither to write the command's
  input and read its output. Both need the environment to provide a terminal
  and answer `capability_unsupported` where it does not. The native runtime
  opens a real pseudo-terminal on Linux and macOS, and the podman runtime runs
  the session in the container; `POST /v1/sandboxes/{id}/exec?wait=1` is
  unchanged. Closing the connection ends the process on the native runtime; on
  podman the engine keeps the session until the sandbox stops.

- `CELLA_RUNTIME=podman` runs each sandbox as a container on a podman engine,
  reached over the libpod API at `CELLA_PODMAN_SOCKET`. Unset, the socket is
  the rootless one and then the system one, and a start-up with neither
  answering says which it tried. A sandbox is a container with its workspace on
  a named volume at `workspace.path`, `resources.cpu` and `resources.memory` as
  cgroup limits, `user`, `env` and the command as written, and its identity and
  lifecycle stamped on the engine's own objects, so `cellad` reads every
  sandbox back after a restart. Exec, logs and file transfer work, and file
  transfer works while the sandbox is stopped. `resources.disk` is recorded and
  not enforced, and attach, a desktop, egress rules and a mesh are not in this
  backend yet.

- `cellad` enforces the lifecycle its runtime reports. A sandbox past its
  expiry is deleted, one stopped for longer than its auto-delete window is
  deleted, and one idle for longer than its auto-stop window is stopped, each
  with the reason in `status.reason`. The exec and file routes stamp activity,
  coalesced per sandbox so a busy session does not write the substrate per
  request. `CELLA_REAP_INTERVAL` (default `30s`) sets how often the rules run
  and `CELLA_TOUCH_INTERVAL` (default `1m`) how often one sandbox's activity
  reaches the runtime. No manifest field carries a lifecycle yet, so a sandbox
  takes the deadline set its environment was opened with, and a sandbox with no
  deadline is never ended.

- `runtime/runtimetest` is the conformance suite a driver passes: `Run` drives
  every `Driver` method, skips an operation the driver does not declare, and
  fails one it declares but refuses. `Nop` is a driver for embedders' fakes.
  `runtime/native` passes it. The tree refuses Latere coordinates under
  `runtime/` and the exported packages are checked to import no client.
- Native workloads can start a main command, report its exit, restart it, and
  stream timestamp-filtered or tailed logs. File routes import and export tar
  archives with authorization, upload bounds, and path containment.
- The first native workspace API is executable: create, list, inspect, execute,
  stop, start, and delete through authenticated `/v1/sandboxes` routes. The
  controller preserves ownership and desired objects across a server restart.
  This is a single-node development store; replicated state and recovery remain
  unimplemented.
- `runtime/native` provides process-group cancellation, durable workspace
  metadata, and confined archive paths. Native execution has no isolation and
  now requires `CELLA_ALLOW_UNSAFE_NATIVE=true`. Unimplemented runtime selections
  fail startup. `make run` explicitly opts into trusted native execution.
- JSON request bodies default to 64 KiB and tar uploads to 1 GiB, configurable
  with `CELLA_MAX_BODY_BYTES` and `CELLA_MAX_UPLOAD_BYTES`.

- `CELLA_OIDC_AUDIENCE` accepts a comma-separated set for external callers.
  The first entry remains the audience of locally signed workload and
  environment tokens. Empty and repeated entries fail startup.

## v0.1.0 - 2026-09-17

- A personal access key can now be narrower than the person who holds
  it, and `cellad` holds it to that. When you create a key you choose
  what it may do: read these two sandboxes, run anything in this
  environment, and nothing else. Those grants ride on every token the
  key mints, and `cellad` applies them on top of the answer its
  authorizer already gave. A key narrowed to some sandboxes is refused
  everywhere else, and the refusal names the reason `grant`.

  A grant only ever takes away. It cannot reach a sandbox you could not
  reach yourself, because the authorizer answers first and the grants
  narrow that answer: a key on an object you do not own reaches nothing,
  and a key can never do more than you can. A token that is not from a
  personal access key is unaffected, whatever it carries.

  Two things to know before you point a narrowed key at `cellad`. A key
  that carries no grants at all is refused everywhere: the grants are
  what a key may do, so a key that says nothing may do nothing. And a
  narrowed key reaching a service that does not read grants is refused
  at the door with a 401 rather than quietly given the person's full
  reach; `cellad` reads them, so a key narrowed for Cella works against
  Cella.

  **If you run your own authorizer endpoint, read this one.** From this
  version a narrowed key reaches `cellad`, and `cellad` asks your
  endpoint what the caller may do. An endpoint written on
  `latere.ai/x/pkg/authz/server` applies the grants for you: bump the
  package to v0.75.0 or later and there is nothing else to do. An
  endpoint written by hand applies them itself, `authz.Restrict` over
  its own answer. An endpoint that does neither answers about the person
  and not about the key, and gives a narrowed key the person's full
  reach. Point `CELLA_TEST_AUTHORIZER_URL` at your endpoint and run the
  conformance suite to see which one yours is. The built-in owner
  policy, which is what you run with `CELLA_AUTHORIZER_URL` unset,
  applies them already.

- `cellad` reads every issuer's keys into its verifier at start, so the
  first request after a start waits for no fetch. It already read them
  to check the issuer; now it keeps them. An issuer that answers that
  check and then stops answering refuses the start, naming
  `CELLA_OIDC_ISSUERS`, which is the same rule as before: at start every
  issuer answers, or `cellad` does not start.

- `latere.ai/x/cella/authorizer` publishes a heading for each resource
  kind beside the table, read with `Vocabulary().Label(kind)`:
  Sandboxes, Secrets, Volumes, Sandbox sets, Environments. Anything that
  offers a person a choice of what a key may do groups the actions by
  kind and takes the heading from here, rather than spelling
  `SandboxSet` at somebody.

- A caller's token is refused once it is more than 24 hours old, even
  when its `exp` is still in the future, so a caller that works for days
  re-mints at its own issuer instead of holding one token. A token that
  does not say when it was minted, carrying no `iat`, is refused for the
  same reason. This is the same rule Origo and Lux apply, one age bound
  across the three. The keys `cellad` mints for environments are
  unaffected: one lives `CELLA_ENVIRONMENT_KEY_TTL`, a year by default,
  and its `exp` is its only bound.
- `cellad` knows who is calling. `CELLA_OIDC_ISSUERS` lists the OpenID
  Connect issuers you trust, any of them; at start `cellad` reads each
  one's discovery document and key set and refuses to start when one does
  not answer, names another issuer, or publishes no `RS256` or `ES256`
  key. A bearer is accepted when a listed issuer signed it, its `aud`
  contains `CELLA_OIDC_AUDIENCE` (default `cella`), it has not expired,
  and it was minted less than 24 hours ago; nothing else about it is
  interpreted. A subject is the issuer and the `sub` joined,
  `https://login.example.com|alice`, so two issuers that agree on a `sub`
  are two subjects, and every claim of the token reaches your authorizer
  verbatim. There is no anonymous access and no
  API key.
- `cellad` signs the identities it hands out. `CELLA_TOKEN_KEY` is one or
  two PEM RSA private keys of at least 2048 bits: the first signs a
  sandbox's token and an environment's key, every one is published at
  `/.well-known/jwks.json`, and each carries its RFC 7638 thumbprint as
  its `kid`, so anything verifies a sandbox's identity offline with a
  stock JWT library. Rotation is yours and lives in the configuration:
  put a new key in front, and remove the old block once the tokens it
  signed have expired. `CELLA_PUBLIC_URL` is now required, because it is
  the issuer those tokens name.
- `cellad` asks somebody else what a caller may do.
  `CELLA_AUTHORIZER_URL` and `CELLA_AUTHORIZER_TOKEN` point at the
  endpoint you write, with `CELLA_AUTHORIZER_TIMEOUT` (`5s`) bounding one
  call and `CELLA_AUTHORIZER_CACHE` (`60s`) holding an allow whose answer
  names no `ttl`. An allow may carry the three ceilings and, on a list, a
  filter. Anything that is not a decision fails the request and never
  allows it, and a connection that died before a response line is retried
  once and nothing else is. With the URL unset, the built-in owner policy
  decides from `CELLA_ADMIN_SUBJECTS` and the owner of each object: you
  create anything but an environment, you act on what you own, a list
  returns your own, admins act on everything and alone make environments,
  and a sandbox's own identity reads and execs itself, reads the tree
  below it, creates a child and nothing else.
- Running `cellad` therefore needs three variables it did not need
  before, `CELLA_OIDC_ISSUERS`, `CELLA_PUBLIC_URL` and `CELLA_TOKEN_KEY`,
  and `make run` needs an issuer named on the command line until the test
  stubs land. The README has the whole of it.

- The vocabulary an authorization endpoint for Cella speaks is an
  importable package, `latere.ai/x/cella/authorizer`. Write the endpoint
  `CELLA_AUTHORIZER_URL` points at in Go and take the thirty-two actions
  of the identity spec's table as constants, the resource kind each one
  acts on from `Kind`, the whole table from `Vocabulary()`, and the three
  ceilings an allow may carry from `WireLimits`, rather than keeping a
  copy of the strings. The envelope on the wire is
  `latere.ai/x/pkg/authz`'s, and so are the stub and the conformance
  suite an endpoint passes; an endpoint built on
  `latere.ai/x/pkg/authz/server` with nothing behind it but a decider
  passes that suite with this vocabulary. `cellad` asks no authorizer
  yet: the configuration, the client and the guard are the identity
  spec's and are not built.

- The repository: the `cellad` binary serving its probes on two listeners,
  typed configuration from `CELLA_*` variables, the quality gate, and the
  design specs. Nothing creates a sandbox yet; the specs say what will.
- The design, revised after review: Cella is a control plane whose data
  plane is a driver in-process or a worker on your own infrastructure;
  the API group is `cella.latere.ai/v1`; five kinds (`Sandbox`, `Secret`,
  `Volume`, `SandboxSet`, `Environment`); an egress gateway that keeps
  secret values out of sandboxes; volumes for persistent state; mesh and
  spawn with a boundary a child cannot widen; three scheduling
  strategies and sets for rollouts; desired state that recovers a lost
  sandbox; six drivers across four isolation classes.
