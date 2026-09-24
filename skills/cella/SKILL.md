---
name: cella
description: Drive Cella sandboxes with the cella command: apply, get, exec, files, logs, delete over /v1.
---

# cella

`cella` is one binary that speaks a Cella control plane's `/v1` API. Use it
to create a sandbox, run commands inside it, move files in and out, read
its output and delete it.

## Reaching the control plane

Inside a sandbox the token is already there: the sandbox's own token is at
`/run/cella/token`, which is where the command looks when `CELLA_TOKEN` is
unset, so only `CELLA_URL` needs setting. Outside one, set both:

```sh
export CELLA_URL=https://control-plane.example.com
export CELLA_TOKEN=<a token from the issuer this plane verifies>
```

There is no login and no configuration file. `--url`, `--token` and
`--token-file` override the variables.

## A sandbox

A manifest is JSON. `-w` waits until it runs.

```sh
cat > sandbox.json <<'JSON'
{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",
 "metadata":{"name":"work"},
 "spec":{"command":["sh","-c","sleep infinity"]}}
JSON
cella apply -f sandbox.json -w
```

`spec.image` names the image on a plane that runs containers; leave it out
on one that runs processes, which refuses an image it cannot run.

## The commands you need

```sh
cella get sandboxes                       # every sandbox you own
cella get sandbox work --json             # one object, the API's own bytes
cella exec work -- sh -c 'ls /workspace'  # run something, its exit code is yours
printf 'hi\n' | cella exec work -i -- sh -c 'read l; echo "got $l"'
cella files put ./plan.md work:/workspace/plan.md
cella files get work:/workspace/out.txt   # to standard output
cella files ls work:/workspace
cella cp ./project work:/workspace        # a whole tree, either direction
cella logs work --tail 50
cella port-forward work 8080:3000         # port 3000 inside, at 127.0.0.1:8080
cella stop work; cella start work
cella delete sandbox work
```

Every path inside a sandbox is absolute and inside its workspace,
`/workspace` unless the manifest moved it. Every command takes `--json` and
answers the API's own shape.

`-i` sends your standard input, and the end of it is not sent: run a
command that ends by itself, not one that reads until end of file.

## Reading a failure

A refusal is one sentence on standard error. `-v` adds the code, the fields
it names and the request id:

```
$ cella apply -f sandbox.json -v
A field has a value it cannot take.
code: invalid_field
paths: spec.command
request: 01J9Z2Q0M8
```

Decide on the exit code, not the sentence:

| Exit | Means | Do |
|---|---|---|
| 0 | it worked | go on |
| 1 | the server failed | retry once, then report |
| 2 | your flags or document | fix the call |
| 3 | refused | read `-v`; the request was wrong or not allowed |
| 4 | no such object | check the name |
| 5 | wrong state | start or stop it first |
| 7 | unreachable | check `CELLA_URL` |

Under `exec` the exit code is the command's own inside the sandbox. `125`,
`126` and `127` are the session's own: it failed, it could not start, the
server was not reachable.

The command never retries by itself, and it prints no token and no secret
value on any stream.
