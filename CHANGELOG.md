# Changelog

What changed for whoever writes a manifest, runs `cellad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- `cellad` reports what it is doing. `GET /metrics` on the internal listener
  serves the Prometheus exposition: requests and their latency by route,
  sandboxes by phase, create time by driver and whether a prewarmed entry was
  adopted, reaper actions, recovery attempts, token re-mints, authorization
  and admission decisions with their latency, the event backlog and what
  delivery did with each record, store operation latency, which loop holds
  which lease, exec sessions by exit, and the boundary's connections, bytes
  and connected gateways. Labels are bounded: no series carries a sandbox id,
  a subject, a name or a path. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` also
  exports traces and logs, one span per request named after its route with
  the authorizer, admission and sink calls as children on the same trace.
  Every log line passes one redacting handler on both destinations, so a
  secret value, a token, a credential or an egress placeholder cannot reach
  a log. The start-up line says `telemetry=otlp` or `telemetry=off`, the
  `egress` role serves no scrape surface of its own, and
  `deploy/base/prometheusrule.yaml` now carries alert rules over the metrics
  that are emitted rather than placeholders. `docs/observability.md` is the
  page.

- A second binary, `cella`, speaks the API from a shell and from inside a
  sandbox: apply a Sandbox or a Secret, list and read objects, exec with or
  without a terminal, attach, follow logs, move files whole or one at a
  time, read the gateway's records, start, stop and delete. It reads
  `CELLA_URL` and `CELLA_TOKEN`, and inside a sandbox it needs neither: the
  address is injected and the token is read from `/run/cella/token` per
  request, so no flag and no login are involved. Every command answers
  columns or `--json`, a refusal is one sentence with the code behind `-v`,
  and the exit code says what happened without reading it: 2 a bad call, 3
  refused, 4 not found, 5 the wrong state, 7 unreachable, and under `exec`
  the code of the command that ran inside. It never retries. The release
  carries it as `cella_<tag>_<os>_<arch>.tar.gz` for the same four
  platforms as the server, `docs/cli.md` is the page for a person and
  `skills/cella/SKILL.md` the one an agent reads.

- `CELLA_DB_POOL_URL` names a pooled Postgres endpoint for the serving path,
  which falls back to `CELLA_DB_URL`; migrations keep the direct endpoint,
  because they hold a session-scoped lock a transaction-mode pooler drops.
  The pooled name without the direct one is a start-up failure.

- An agent that uses a computer gets one. A manifest asks for a virtual
  desktop with `spec.display: {width, height}`, and the environment runs an X
  server, a window manager and the capture and input tools beside the
  workload: a second container on Kubernetes sharing the sandbox's `/tmp`, a
  second process in the container on podman. `GET /v1/sandboxes/{id}/display`
  answers the geometry and whether the desktop is up, which
  `status.conditions` carries as `DisplayReady`;
  `GET /v1/sandboxes/{id}/screenshot` answers one frame as a PNG or a JPEG at
  a scale you choose; `GET /v1/sandboxes/{id}/screen` is a WebSocket of frames
  paced up to ten a second, where a viewer that falls behind loses the newest
  frame rather than a growing backlog; and `POST /v1/sandboxes/{id}/input`
  takes a batch of pointer and keyboard events, moves, clicks, drags, scrolls,
  keys, typed text and waits, validated whole against your own desktop before
  the first one runs, so a coordinate off the screen or a key that is not a
  key refuses the batch and changes nothing. A batch the desktop stops part
  way through answers with how much of it landed and where it stopped. A
  manifest also declares what runs inside with `spec.network.ports`, and
  `GET /v1/sandboxes/{id}/ports` and `status.ports` say which of them
  something is listening on. No screenshot, keystroke or character of typed
  text reaches an event or a log line: the records carry a frame's size, a
  batch's count and a session's duration and nothing else.

- `make run` needs no issuer of your own. It starts `cella-stubs`, a test
  binary that serves an OpenID Connect issuer, an authorization endpoint, an
  admission endpoint and an event sink on loopback, then starts `cellad serve`
  wired to them, and prints the one command that mints a token to call it
  with. `make test`, `make tier-podman` and `make tier-kind` are the tiers,
  and `deploy/examples/kind-stubs` runs the same stubs beside the control
  plane in a kind cluster, which is what lets
  [`docs/install.md`](docs/install.md) walk green on every push and on every
  tag rather than waiting for an issuer somebody has to supply.

- An environment can keep sandboxes ready, so a create that matches one
  starts in milliseconds instead of waiting for an image pull and a container
  start. `CELLA_POOL_SIZE` with `CELLA_POOL_IMAGE`, `CELLA_POOL_CPU`,
  `CELLA_POOL_MEMORY` and `CELLA_POOL_DISK` says how many to keep and what
  shape they are, with `CELLA_POOL_IN_FLIGHT` and `CELLA_POOL_GRACE` bounding
  how fast the pool fills and how long an entry is left alone before it is
  read as one to give up; nothing is kept without a size. The acceleration is
  transparent: a manifest never asks for it and never refuses it, and a
  sandbox that came from a prewarmed one is the caller's own in every respect,
  with its own boundary, its own identity, its own labels and a creation time
  of its own. `status.conditions[Scheduled]` reads `FromPool` where it
  happened and `Placed` where the sandbox was created outright. A manifest
  that names a command, a user, another image, other resources or a workspace
  of its own is created outright, because none of those can be changed under a
  container that is already running. `CELLA_SCHEDULING_MODE` accepts `direct`,
  which is what this control plane does: a create starts now or fails.

- Your own policy decides every create. Point `CELLA_ADMISSION_URL` at an
  endpoint you write, give it the bearer in `CELLA_ADMISSION_TOKEN`, and
  `cellad` sends it the defaulted manifest, the verified caller and the
  environment on every apply; it answers with the manifest to run or with
  a refusal. That is where an image catalogue pins an alias to a digest,
  where a plan fills in resources and lifetimes and refuses what exceeds
  them, and where a boundary is narrowed before anything starts. What the
  endpoint returns is what the caller reads back and what the sandbox
  runs, and it is validated the same way a caller's manifest is, so an
  endpoint cannot write a field the schema does not have. A refusal is a
  422 carrying your endpoint's own reason; an endpoint that times out,
  answers anything else, or cannot be reached fails the create with a 503
  and never lets one through, and nothing is ever resent. Leave the URL
  unset and the step is the identity, as before. `spec.image` moves with
  it: it is now required after your endpoint has run rather than before,
  so a catalogue can supply it, and `CELLA_DEFAULT_IMAGE` fills it in on
  an installation with no endpoint. A manifest that names no image after
  both is refused with `missing_field`, and an environment that runs no
  image, such as the native one, still refuses an image whoever named it.

- A `v*` tag is a release. It publishes `ghcr.io/<owner>/cellad:<tag>`, a
  multi-architecture image built from the binary the pipeline compiled, signed
  with cosign and carrying an SPDX bill of materials and a build-provenance
  attestation; `cellad_<tag>_<os>_<arch>.tar.gz` for linux and darwin on amd64
  and arm64, with `checksums.txt` and a signature over it; and
  `deploy-<tag>.tar.gz`, the kustomize tree with that image pinned by digest.
  The namespace is the account that pushed the tag, so a fork's tag publishes
  under the fork. The release page's body is the CHANGELOG section for the
  tag, and a clean runner verifies every signature, checksum and attestation
  after it is published.

- `deploy/` is the installation: a base with the control plane's Deployment,
  its Service, the account and the Role the Kubernetes driver needs and
  nothing more, a network policy, a disruption budget and a check Job;
  `deploy/bootstrap` with the namespace and one Secret per dependency; and two
  overlays, a laptop cluster and a cluster an installation runs on. The base
  pins no namespace and no image registry. [`docs/install.md`](docs/install.md)
  walks it from an empty cluster to a running sandbox.

- `cellad check` reads the whole configuration and prints one line per
  requirement of an installation, exiting 1 on any failure: the configuration
  loads, every issuer answers and the signing key parses, the authorization
  endpoint denies the reserved probe, the backend answers with the accesses
  the driver uses, and the data directory is writable. Each optional
  dependency is answered for when its variable is set and reported as not
  configured otherwise. It is the start-up path of `cellad serve` asked one
  question at a time, so an installation that passes it is one that would have
  started. `cellad version` prints the same build identity as `cellad
  -version`, for a Job that is one image and one argument.

- A sandbox reaches the services it needs and holds none of their
  credentials. The new `Secret` kind at `/v1/secrets` takes a value, the hosts
  that value may be sent to, and where in a request it goes: a header, a query
  parameter, verbatim or base64 of a `user:pass` pair, or an OAuth 2.0
  client-credentials grant the gateway mints a token from. The value is
  write-only: it is accepted on apply and absent from every read, every list,
  every event and every log line, and it is stored under envelope encryption
  with the key in `CELLA_SECRET_KEY`. A manifest mounts one with
  `spec.secrets: [{name, env}]`, and what the sandbox's environment carries is
  an opaque placeholder, plus `<env>_HEADER` or `<env>_QUERY` where the secret
  injects somewhere a client would not look by itself. The gateway swaps the
  placeholder for the value on the way out, toward a host the secret's own
  owner named and nowhere else: sent anywhere else it leaves verbatim, so an
  exfiltration attempt carries an opaque token. Two mounted secrets that apply
  to one host are refused rather than silently collapsed, and a mount with no
  allowed host of its own puts the secret's scope on the sandbox's allow list.
  Rotating a value reaches a running sandbox's next request without restarting
  it; deleting the secret marks the sandbox `notInjectable` and its next
  request leaves unauthenticated. `status.secrets` says which placeholders are
  mounted and which of them the gateway will not substitute.

- Every sandbox carries an identity of its own. The control plane mints a
  workload token at create, the driver projects it read-only at
  `/run/cella/token` and names the path in `CELLA_TOKEN_FILE`, and a process
  inside the sandbox calls `/v1` back with it: it reads and execs the sandbox
  it belongs to and reaches nothing else. The token expires with its sandbox
  or a day after it was minted, whichever is sooner, and the control plane
  re-mints and re-projects it once two thirds of that has passed, without
  restarting the workload. A token that is replaced, and one whose sandbox is
  deleted, is refused from that moment rather than when it expires. Any
  service can verify one offline against `/.well-known/jwks.json`.

- A sandbox's workspace answers one file at a time, not only whole archives.
  `GET /v1/sandboxes/{id}/files/content?path=` streams one file,
  `.../files/stat?path=` describes it, `.../files/list?path=` lists a
  directory sorted by name, `PUT /v1/sandboxes/{id}/files?path=[&mode=]`
  writes one file of any content type, `DELETE .../files?path=` removes a file
  or a tree, and `POST .../files/mkdir` and `POST .../files/move` make a
  directory and rename one path onto another. A write is staged and renamed,
  so a body that ends early or passes `CELLA_MAX_UPLOAD_BYTES` leaves the
  previous file as it was, and a move takes the exact destination rather than
  nesting the source inside an existing directory. Every path is absolute and
  inside the workspace: one that leaves it, by traversal or through a symbolic
  link the workload planted, is refused. Names arrive as they are, spaces,
  unicode, percent signs, pipes and newlines included. The native, podman and
  k8s drivers all serve the routes; the podman driver needs the sandbox
  running and answers `phase_conflict` while it is stopped.

- Every change to a sandbox and every operation on one produces a signed
  record, delivered to the endpoint `CELLA_EVENTS_URL` names. A record says
  who did what to which object, when, and why, with the object's labels and a
  sequence per object; it never carries a command line, a file's content, an
  environment or secret value, or a token. `CELLA_EVENTS_SECRET` holds one
  secret or two separated by a comma, and each signs the delivery, so a secret
  is rotated without losing a record. Delivery is at least once and in order
  per object: a sink that fails is retried with a growing wait for
  `CELLA_EVENTS_RETRY_WINDOW` (default `24h`), a sink that refuses a record
  ends it, and a sink that rejects the signature holds it until the two ends
  agree again. A record commits with the change it describes, so a sandbox
  that started has a record that says so. With the URL unset nothing is
  delivered and the journal still holds the history.

- A manifest declares the network boundary its sandbox lives inside, and
  `cellad egress` enforces it. `spec.network.egress` takes a `mode` of `none`,
  `allowlist` or `open`, with `allowedHosts` for the first and `deniedHosts`
  for the last, each an exact name or one leading `*.` wildcard; the mode is
  inferred from whichever list is set. A sandbox may narrow its own boundary
  and never widen it. At create, the control plane compiles the boundary into
  the map its gateway holds and waits for a gateway to acknowledge it before
  the sandbox exists, so nothing runs outside the boundary it declared; a
  boundary of `open` with no denied host needs no gateway. The gateway is the
  new `cellad egress` role: it connects outbound to `CELLA_URL` with
  `CELLA_ENVIRONMENT_KEY`, receives every map of its environment and every
  change after, and serves two doors, a CONNECT proxy on
  `CELLA_EGRESS_PROXY_ADDR` and a reverse door on `CELLA_EGRESS_REVERSE_ADDR`,
  each authenticated by the sandbox's own credential. Every connection is
  decided before any dial and reported back as one record, which
  `GET /v1/sandboxes/{id}/egress` serves newest first. The native and podman
  drivers point the workload at the gateway through the proxy and trust
  variables and project the gateway's authority into the sandbox. Secrets and
  their values are not in this release: a boundary is enforced, and nothing is
  substituted yet.

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

- `CELLA_RUNTIME=k8s` runs each sandbox on a Kubernetes cluster: one
  PersistentVolumeClaim and one Pod per sandbox, where a stop deletes the Pod
  and keeps the claim, and a start renders a new Pod against it. Every Pod runs
  as a non-root uid with a read-only root file system, dropped capabilities, the
  default seccomp profile, no privilege escalation, no host namespaces and no
  service account token, with the workspace and `/tmp` as the only writable
  mounts. Identity, the deadlines and the activity stamp live on the two
  objects, so listing a namespace is enough to see every sandbox after a
  restart. Exec, logs and archive transfer go through the cluster, and a
  transfer into a stopped sandbox runs in a short-lived helper Pod. The cluster
  is named by `CELLA_K8S_KUBECONFIG` (empty means in-cluster) and
  `CELLA_K8S_NAMESPACE` (default `cella`); `CELLA_K8S_STORAGE_CLASS`,
  `CELLA_K8S_NODE_SELECTOR`, `CELLA_K8S_TOLERATIONS`,
  `CELLA_K8S_IMAGE_PULL_SECRETS`, `CELLA_K8S_RUN_AS_USER`,
  `CELLA_K8S_RUN_AS_GROUP`, `CELLA_K8S_CPU_REQUEST_RATIO`,
  `CELLA_K8S_MEMORY_REQUEST_RATIO`, `CELLA_K8S_DEFAULT_CPU`,
  `CELLA_K8S_DEFAULT_MEMORY`, `CELLA_K8S_DEFAULT_DISK`,
  `CELLA_K8S_READY_TIMEOUT` and `CELLA_K8S_GRACE_PERIOD` are what one
  installation supplies. Egress rules, the mesh, attach, ports, display and a
  warm pool are not on this driver yet.
- Desired state can live in Postgres. `CELLA_DB_URL` selects it, bounded by
  `CELLA_DB_MAX_CONNS` (default `4`), with the schema applied at start-up and
  a schema this binary does not know refused rather than served. With it, a
  sandbox the runtime lost is recreated from its desired state with the same
  id, name, labels and lifecycle; without it, a lost sandbox is reported and
  deleted after `CELLA_LOST_GRACE` (default `10m`), which the start-up line
  says out loud. `CELLA_SECRET_KEY` is read where it is set: 32 bytes, base64,
  the key every secret value's data key is sealed under. Several `cellad`
  replicas on one database take one writer's lease each.
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
  reaches the runtime. A sandbox with no deadline is never ended.

- A manifest carries the fields that decide what a sandbox gets and how long
  it lives: `spec.user`, `spec.resources` with `cpu`, `memory` and `disk`,
  `spec.workspace.path`, and `spec.lifecycle` with `ttl`, `autoStop` and
  `autoDelete`, each a duration or `never`. A resolved manifest is returned in
  the caller's own spelling, with `status.expiresAt` and a warning for every
  field the environment records and does not enforce. The operator's defaults
  and ceilings apply in one staged resolve, so a manifest above a ceiling is
  refused with `ceiling_exceeded` and the path that exceeded it, and a field
  the contract fixes is refused with `immutable_field`.

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
