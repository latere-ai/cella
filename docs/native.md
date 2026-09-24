# Run a native sandbox

The native runtime runs a sandbox as ordinary processes on the machine
`cellad` runs on, with **no isolation**: no image, no container, no limit
on what the processes reach. It is for development and tests, and for
code you trust. Anything else belongs on the Podman or Kubernetes
runtime, which run each sandbox in a container.

## Start

You need Go 1.27 or newer, `openssl`, and `curl`. From a checkout:

```sh
make run
```

That starts the test stubs and `cellad serve` wired to three of them, an
issuer, an authorization endpoint, and an event sink, with the public listener on `127.0.0.1:8080` and the probes on
`127.0.0.1:8081`. It keeps the signing key, the workspaces, and the
control plane's records under `out/run/`, and prints two lines to paste
into another terminal:

```
export CELLA_URL=http://127.0.0.1:8080
export CELLA_TOKEN=$(curl -fsS -X POST <the stub issuer>/mint ...)
```

The token is minted by the stub issuer for the subject `dev`, and the stub
authorizer allows it. There is no anonymous access and no built-in
development token: every request carries a bearer.

`make run` sets `CELLA_RUNTIME=native` and `CELLA_ALLOW_UNSAFE_NATIVE=true`.
A `cellad` you start yourself with the native runtime must set the second
variable too, or it refuses to start: the consent is explicit because
nothing is confined.

## Create and run a command

```sh
curl --fail-with-body "$CELLA_URL/v1/sandboxes" \
  -H "Authorization: Bearer $CELLA_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"demo"},"spec":{"env":{"TASK":"hello"}}}'

curl --fail-with-body "$CELLA_URL/v1/sandboxes/demo/exec?wait=1" \
  -H "Authorization: Bearer $CELLA_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","printf %s \"$TASK\""],"timeout":"10s"}'
```

With `?wait=1` the answer is one JSON object carrying `exitCode`,
`stdout`, `stderr`, `truncated`, and `durationMs`. A command that fails
is an exit code; a request that fails is an HTTP error. Each output is
capped at 1 MiB. Without `?wait=1` the output streams as the command
produces it. Commands run as host processes, so they must exist on the
host.

The same through the client:

```sh
cella exec demo -- sh -c 'printf %s "$TASK"'
cella attach demo
```

`attach` and `exec -i` or `-t` open a real pseudo-terminal on Linux and
macOS.

## Lifecycle

`GET /v1/sandboxes` lists your sandboxes and `GET /v1/sandboxes/demo`
reads one. `POST /v1/sandboxes/demo/stop` and `/start` stop and start it,
`PUT /v1/sandboxes/demo` applies a changed manifest by name, and `DELETE
/v1/sandboxes/demo` removes it with its files. A name resolves within
what you own; the `status.id` the server returns addresses the same
sandbox.

A manifest that names a `command` runs it as the main process; `start`
runs it again after a stop. `spec.lifecycle` works here as on every
runtime: `autoStop` stops an idle sandbox, `ttl` deletes one that has
lived long enough, and `autoDelete` deletes one that has been stopped
long enough.

Restarting `cellad` with the same data directory keeps every sandbox and
its owner. Without `CELLA_DB_URL` the state is a snapshot under the data
directory that one process holds a lock on, so run one server against
it. The native runtime does not reattach to processes after a crash of
`cellad`: their sandboxes become `Lost`, a restart of them is refused,
and a process that survived is yours to stop. An orderly shutdown stops
and reaps every main process.

## What the native runtime takes from a manifest

| Field | Here |
|---|---|
| `metadata`, `env`, `command`, `args`, `workdir` | as written |
| `image` | refused: host processes run no image |
| `workspace.path` | `/workspace` only, and the workspace is a directory under the data directory |
| `resources`, `user` | recorded and not applied, with a warning on the answer |
| `secrets` | mounted as placeholders the gateway substitutes; needs `CELLA_SECRET_KEY`, which `make run` sets |
| `network.egress` | recorded and not enforced, with a warning |
| `network.ports` | yes, see below |
| `mesh.spawn` | yes: a sandbox may create sandboxes |
| `mesh.enabled`, `display` | refused: this runtime connects no peers and has no desktop |
| `lifecycle` | yes |
| `scheduling` | refused on the default `direct` environment |

## Files and logs

`PUT /v1/sandboxes/demo/files?dest=/workspace` with
`Content-Type: application/x-tar` extracts an uncompressed tar archive,
and `GET /v1/sandboxes/demo/files?path=/workspace` reads a tree back as
one. `/files/content`, `/files/stat`, `/files/list`, `/files/mkdir`, and
`/files/move` work one path at a time, and `PUT /files?path=` writes one
file of any type. Every file route works while the sandbox is stopped and
needs the right to run commands in it. An upload is staged before it is
extracted, and traversal, links, and special files are refused. An
invalid entry late in an archive can leave the complete entries before it
in place.

`CELLA_MAX_BODY_BYTES` bounds a JSON body, `65536` by default, and
`CELLA_MAX_UPLOAD_BYTES` an upload, `1Gi` by default; both take integer
bytes or a `Ki`, `Mi`, or `Gi` suffix. The native runtime also caps the
contents of one archive at 256 MiB.

`GET /v1/sandboxes/demo/logs` reads the main command's combined output,
with `tail=100`, `since=<RFC 3339>`, and `follow=1`. A stream that fails
after it started reports the failure in the `X-Cella-Error` trailer. The
output of an `exec` is in its own answer, not in these logs.

## Ports

A native sandbox's processes are host processes, so a port its main
process listens on is a port of this machine's loopback. Declare it by
name to reach it through the control plane:

```json
"spec": {
  "command": ["python3", "-m", "http.server", "3000"],
  "network": {"ports": [{"name": "web", "port": 3000}]}
}
```

`GET /v1/sandboxes/demo/ports` reports each declared port as `listening`
or `closed`. `/v1/sandboxes/demo/ports/web/` forwards any HTTP request to
it, WebSocket upgrades included, and answers `502` with
`upstream_unavailable` while nothing listens or the sandbox is stopped.
`cella port-forward demo 8080:3000` carries `127.0.0.1:8080` on your
machine to it. The dial socket at `/v1/sandboxes/demo/dial/3000` reaches
any port of this machine's loopback, declared or not: the native runtime
confines nothing, and a caller allowed to dial is one already allowed to
run a command here.
