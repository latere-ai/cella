# The cella command

`cella` speaks a Cella control plane's `/v1` API from your shell and from
inside a sandbox. It is one binary, it needs no configuration file and no
login, and everything it can do it does through the same API a program
would call.

Get it from the release page as `cella_<tag>_<os>_<arch>.tar.gz`, beside
the server's own archive, or build it from a checkout with `go build
./cmd/cella`.

## Reaching a control plane

Two values, and nothing else:

```sh
export CELLA_URL=https://cella.example.com
export CELLA_TOKEN="$(your-issuer print-token)"
cella get sandboxes
```

The token comes from the OpenID Connect issuer the control plane verifies
against; there is no `cella login`. `--url` and `--token` override the two
variables, `--token-file` reads the token from a file instead, and `--ca`
adds one certificate authority to the system roots.

**Inside a sandbox nothing needs to be set.** The control plane injects the
address, and the sandbox's own token is at `/run/cella/token`, which is
where `cella` looks when `CELLA_TOKEN` is unset. The file is read for every
request, so a command that runs for hours keeps working when the token is
replaced under it. A sandbox's token reaches its own sandbox and the
objects its owner holds, and nothing else.

## The commands

```
apply    -f <file> [-w]                          apply a Sandbox or a Secret manifest
get      <kind> [<ref>] [-o json|yaml|wide|name] read one object or list a kind
delete   <kind> <ref>                            delete one object
start    <ref>                                   start a stopped sandbox
stop     <ref>                                   stop a running sandbox
exec     <ref> [-i] [-t] -- <command>            run a command inside a sandbox
attach   <ref> [-- <command>]                    open a terminal inside a sandbox
logs     <ref> [-f] [--since t] [--tail n]       write the main process output
cp       <ref>:<src> <dest>                      copy a tree out of a sandbox
cp       <src> <ref>:<dest>                      copy a tree into a sandbox
files    ls|stat|get|put|mkdir|rm|mv             one file operation inside a sandbox
egress   <ref> [--limit n]                       the connections the gateway recorded
port-forward <ref> <local>:<port>                a local port carried to a port inside a sandbox
version                                          the client's identity, and the server's
```

`<kind>` is `sandbox` or `secret`, written singular or plural. `<ref>` is an
object's name or its id; a name resolves among the objects you own.

### Making a sandbox

A manifest is JSON:

```sh
cat > sandbox.json <<'JSON'
{
  "apiVersion": "cella.latere.ai/v1beta1",
  "kind": "Sandbox",
  "metadata": {"name": "dev", "labels": {"team": "core"}},
  "spec": {"command": ["sh", "-c", "sleep infinity"]}
}
JSON
cella apply -f sandbox.json -w
```

`-w` waits until the sandbox is running. `-f -` reads the manifest from
standard input, which is what a program that builds one does.

### Looking at what you have

```sh
cella get sandboxes
cella get sandboxes -l team=core --phase Running
cella get sandbox dev -o wide
cella get sandbox dev --json
```

`cella get` lists by default and follows the server's pages to the end;
`--limit` stops it earlier. `-o name` prints `sandbox/<name>` per line, for
a shell loop. `--json` and `-o json` write the API's own bytes, so what you
pipe into `jq` is exactly what the server sent. `-o yaml` asks the server for
the same object as YAML and writes that through; a list under `-o yaml` is
the one page the server answered, with its cursor, because the pages are
joined by re-encoding an envelope and the command does not re-encode what the
server rendered.

### Running something inside

```sh
cella exec dev -- ls -la
printf 'a line\n' | cella exec dev -i -- sh -c 'read line; echo "got $line"'
cella exec dev -t -- top
cella attach dev
```

Without `-i` and `-t` the command runs and its output arrives when it
finishes. `-i` sends your standard input to the command and `-t` runs it
under a terminal; either opens a session, and a session carries your input
either way. `attach` is a terminal on the sandbox with your window
following it, and your terminal is restored however the session ends.

The end of your input is not sent: the session has no frame that says it,
so the command inside must end by itself. `sh -c 'read line; ...'` ends,
`cat` waits.

**The exit code of `cella exec` is the exit code of the command inside.**
That is what makes it usable in a script:

```sh
if cella exec dev -- test -f /workspace/ready; then echo ready; fi
```

### Moving files

```sh
cella cp ./project dev:/workspace          # a tree in
cella cp dev:/workspace ./out              # a tree out
cella files put ./one.txt dev:/workspace/one.txt --mode 0600
cella files get dev:/workspace/one.txt ./one.txt
cella files ls dev:/workspace
cella files mkdir dev:/workspace/sub
cella files mv dev:/workspace/one.txt dev:/workspace/sub/one.txt
cella files rm dev:/workspace/sub
```

`cp` streams a whole tree as one archive in either direction and nothing is
written to a temporary file on the way. `files` is one operation on one
path, which is what you want when the tree is large and the change is
small. Every path inside a sandbox is absolute and inside its workspace.

### Starting, stopping and ending one

```sh
cella stop dev       # the workspace is kept
cella start dev
cella delete sandbox dev
cella version        # this client, and the server when it answers
```

A stopped sandbox keeps its files and runs nothing. `cella delete` is
accepted in every phase.

### Reaching a port inside

```sh
cella port-forward dev 8080:3000
```

A server the sandbox runs on port 3000 is then at `127.0.0.1:8080` on your
machine, until you interrupt the command. Each connection you open there is
carried by its own connection to the control plane, and bytes flow both
ways as they are written. `0` as the local port lets your system pick a
free one, and the line the command prints names it. The local port is on
loopback only, so nothing else on your network reaches it.

A sandbox that is missing, stopped, or on an environment that reaches no
port is refused at start, with the exit code of the refusal. A port that
nothing inside listens on yet is not a refusal: the command listens, and
each connection to it is closed with a line on standard error that says so.

A server that speaks HTTP is also reachable without this command, by the
name the manifest gave its port: `/v1/sandboxes/dev/ports/web/` on the
control plane, under your bearer, forwards every method, path and query to
it, and a WebSocket upgrade as well. The path without its trailing slash
answers a `307` whose `Location` is relative, `web/` with the query kept,
so it resolves against the address you used and keeps working behind a
proxy that serves the control plane under a path of its own.

### Logs, secrets and the boundary

```sh
cella logs dev -f --tail 100
cella apply -f secret.json --value-from-env API_TOKEN
cella get secrets
cella egress dev
```

A `Secret`'s value is written and never read back: no command prints it, no
answer carries it, and the sandbox that uses it holds a placeholder rather
than the value. `cella egress` shows the connections the gateway recorded
for a sandbox: where they went, whether they were allowed, and which
secrets were substituted, by name.

## What a failure tells you

A refusal is one sentence on standard error:

```
$ cella apply -f sandbox.json
This field cannot be changed after the object is created.
$ cella apply -f sandbox.json -v
This field cannot be changed after the object is created.
code: immutable_field
paths: spec.image
detail: image changed after create
request: 01J9Z2Q0M8
```

`-v` adds the machine code, the fields it names, a developer sentence, and
the request id to quote when you report it. Your token and a secret's value
are in none of them.

The exit code says what happened without reading the sentence:

| Exit | |
|---|---|
| 0 | it worked, or the command inside exited 0 |
| 1 | the server failed, or a stream ended before its end |
| 2 | a flag, a reference or a document this command could not read |
| 3 | the server refused the request |
| 4 | there is no such object |
| 5 | the object is in a state that does not allow this |
| 7 | the server could not be reached |

Under `exec` and `attach` the command inside sets the exit code, and three
codes are the client's own: `125` the command failed, `126` it could not
start, `127` the server was not reachable.

The command never retries. A caller that wants one has the exit code, and
`3`, `4` and `5` are answers rather than accidents.

## What it does not do yet

A command whose route this server does not serve is not in the table above:
port forwarding, screenshots and input, events, volumes, sets and
environments arrive with the routes that serve them. `cella --help` lists
what the binary you have can do.
