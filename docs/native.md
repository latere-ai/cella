# Run a native workspace

Native execution runs trusted commands on the server host with **no isolation**.
Use it for development and tests. Hosted applications and untrusted agents
need an isolated runtime, which is not implemented in this release.

## Start

Install Go 1.27, configure a reachable OpenID Connect issuer, and run:

```sh
CELLA_OIDC_ISSUERS=https://login.example.com make run
```

This starts the public listener on `127.0.0.1:8080`, with probes on port 8081.
It keeps the signing key, workspace data, and controller records in `out/run`.
`make run` explicitly sets `CELLA_ALLOW_UNSAFE_NATIVE=true`. A direct launch
with `CELLA_RUNTIME=native` must set that variable too. Selecting an
unimplemented runtime fails startup; it never falls back to native.

Obtain an access token from your issuer for audience `cella` and put it in
`CELLA_ACCESS_TOKEN`. There is no anonymous API or built-in development token.

## Create and execute

```sh
curl --fail-with-body http://127.0.0.1:8080/v1/sandboxes \
  -H "Authorization: Bearer $CELLA_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"demo"},"spec":{"env":{"TASK":"hello"}}}'

curl --fail-with-body 'http://127.0.0.1:8080/v1/sandboxes/demo/exec?wait=1' \
  -H "Authorization: Bearer $CELLA_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","printf %s \"$TASK\""],"timeout":"10s"}'
```

The response contains `exitCode`, `stdout`, `stderr`, `truncated`, and
`durationMs`. Command failure is an exit code; request failure is an HTTP
error. Each output is capped at 1 MiB. Commands must exist on the host.
The packaged minimal server image includes no shell or workload tools.

## Lifecycle

Use `GET /v1/sandboxes` to list your workspaces and
`GET /v1/sandboxes/demo` to inspect one. `POST` to `/demo/stop` and
`/demo/start` stops and starts the workspace. `DELETE` on `/demo` removes
it and its files. These abbreviated paths follow `/v1/sandboxes`.
Every request needs the same bearer header. Names resolve within the caller's
ownership; the returned `status.id` also addresses the object.

Restarting `cellad` with the same data directory retains workspaces and
ownership. The local controller store has an exclusive process lock: run
one server against it. This is a provisional development store, not the
memory/Postgres store contract or replicated recovery of spec 010.

## Supported manifest

Only JSON `Sandbox` objects are currently implemented. `metadata` accepts
`name`, `labels`, and `annotations`; `spec` accepts the configured
`environment`, `env`, `command`, `args`, and `/workspace` as `workdir`.
A command starts as the main process; start reruns it after stop. Image
execution, volumes, secrets, networking, resource limits, scheduling fields,
and lifecycle timers are not yet available and are refused. Status belongs
to the server. There is no apply/update route yet.

## Files and logs

Upload an uncompressed tar body with `PUT /v1/sandboxes/demo/files?dest=/workspace`
and `Content-Type: application/x-tar`. Download a tar stream with
`GET /v1/sandboxes/demo/files?path=/workspace`. Both operations also work while
stopped and require permission to execute on the workspace. Uploads are staged
before extraction; traversal, links, and special-file entries are rejected.
An invalid later entry may leave earlier complete entries in place.

`CELLA_MAX_BODY_BYTES` defaults to `65536`; `CELLA_MAX_UPLOAD_BYTES` defaults
to `1Gi`. Both accept positive integer bytes or `Ki`, `Mi`, and `Gi` suffixes.
Native imports additionally cap the sum of file contents at 256 MiB.

Read a main command's combined output at `GET /v1/sandboxes/demo/logs`, with
optional `tail=100`, `since=<RFC3339>`, and `follow=1`. Follow streams flush
output as it arrives. If a file or log stream fails after it starts, inspect
the `X-Cella-Error` HTTP trailer. `exec?wait=1` output is returned in its own
response rather than these main-process logs.

Orderly shutdown stops and reaps main processes. This driver does not recover
active processes after a daemon crash: their records become `Lost`, restart
is refused, and an operator must clean up any surviving host process. Use an
isolated runtime with the full recovery contract for hosted applications.

Interactive execution streams, PTY, detached recovery, and ports remain to be
implemented.
