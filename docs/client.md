# The Go client

`latere.ai/x/cella/client` is a typed client for a Cella control plane's
`/v1` API, for a Go program that creates and drives sandboxes: a sandbox
provider behind an agent framework, a command line of your own, a
dashboard's backend. It is the client the `cella` command is built on, so
every call it makes is one the command makes too.

```sh
go get latere.ai/x/cella/client
```

The package brings the standard library, Cella's manifest types
(`latere.ai/x/cella/manifest/v1`) and the small package that defines the
API's error envelope into your build, and nothing else. None of the
control plane's own code comes with it: no store, no driver, no policy.

## Connecting

```go
c, err := client.New(client.Config{
    URL:       "https://cella.example.com",
    Token:     client.StaticToken(token),
    UserAgent: "my-provider/1.4",
})
```

| Field | What it is |
|---|---|
| `URL` | The control plane's address, `http` or `https`. Required; nothing is read in its place. A path on it prefixes every route. |
| `Token` | Where each request's bearer comes from. `nil` sends no bearer. |
| `HTTPClient` | Carries every call, including the exec, attach and dial sockets. `nil` uses the package's own transport. |
| `RootCAs` | Certificate authorities the package's own transport trusts instead of the system roots. Ignored when you pass `HTTPClient`. |
| `UserAgent` | Sent on every request. Defaults to `cella-client`. |

`New` checks the address and nothing else; it does not contact the server.
A `Client` is safe to share between goroutines.

### Tokens

The token source is asked once per request, with that request's context:

| Source | Use it when |
|---|---|
| `client.StaticToken(s)` | You hold one token for the life of the client. |
| `client.TokenFile(path)` | The token is a file something else rewrites. The file is read on every request, so a long-running stream keeps working when the token is replaced under it. |
| `client.TokenFunc(f)` | You mint or refresh tokens yourself. `f` gets the request's context, so a slow issuer is bounded by the call. |

A token file that is missing or empty fails the call with a `*client.NoBearer`
before anything is sent.

### Inside a sandbox

Code running in a sandbox needs no configuration. The control plane gives
the sandbox its address in `CELLA_URL` and its own token at
`/run/cella/token`:

```go
c, err := client.New(client.Environment(os.Getenv))
```

`client.Environment` reads `CELLA_URL`, then takes the token from
`CELLA_TOKEN`, else from the file `CELLA_TOKEN_FILE` names, else from
`/run/cella/token`. It is the same order the `cella` command uses.
`client.New` never reads the environment by itself.

### Your own HTTP client

Pass `HTTPClient` to put the calls through your own transport: a proxy, a
custom dialer, mutual TLS, tracing. The sockets upgrade through the same
client, so they go through that transport too.

Do not set `Timeout` on it. A `Timeout` limits the whole exchange,
including the body, so it cuts off followed logs and events. It also
wraps the body in a way that makes a socket impossible to write to, and a
socket call fails with a message saying so. Put a deadline on the call's
context instead.

The package's own transport reads no proxy variable. A sandbox reaches its
control plane directly, not through its egress gateway.

## Manifests

A manifest is its bytes plus the syntax they are in. The server reads JSON
and YAML the same way:

```go
m := client.YAML(yamlBytes)                // or client.JSON(jsonBytes)
typed, err := client.Encode(v1.Sandbox{...}) // a typed object, sent as JSON
```

| Call | What it does |
|---|---|
| `CreateSandbox(ctx, m)` | Creates a sandbox. The manifest may leave the name out. |
| `ApplySandbox(ctx, name, m)` | Creates the sandbox under that name, or updates the one you hold, so applying the same manifest again makes no second sandbox. |
| `CreateSecret`, `ApplySecret` | The same for a Secret. No answer ever carries its value. |
| `CreateEnvironment`, `ApplyEnvironment` | The same for an Environment. |

Your bytes are sent unchanged. If the server refuses a field, the error
points at the field as you wrote it.

## Calls

A call that returns one object returns the decoded object and the
response's own bytes, for when you forward the API's JSON rather than
re-encode it:

```go
sandbox, raw, err := c.GetSandbox(ctx, "agent-7")
```

| Call | What it does |
|---|---|
| `GetSandbox`, `GetSecret`, `GetEnvironment` | Read one object by name or id. |
| `ListSandboxes`, `ListSecrets`, `ListEnvironments` | List with `ListOptions`: labels (`team=core`), phase, owner, environment, and a total `Limit`. The client follows the pages for you. |
| `GetAs`, `ListAs` | The same reads in a syntax you name with an `Accept` value, such as `application/yaml`, returned as the server wrote it. |
| `StartSandbox`, `StopSandbox` | Start a stopped sandbox or stop a running one. |
| `Delete(ctx, kind, ref)` | Delete an object of `client.KindSandbox`, `KindSecret` or `KindEnvironment`. |
| `Exec(ctx, ref, req)` | Run a command and wait for it: exit code, stdout, stderr. Each output is capped at 1 MiB. |
| `ExecSession`, `AttachSession` | A live session. Read its output and write its input; `Resize` sets the terminal window, and `Wait` returns the exit code. |
| `Logs(ctx, ref, opts)` | The main process's output, followed live when `Follow` is set. |
| `ExportTar`, `ImportTar` | Move a directory tree out of or into the workspace as a tar stream, with no temporary file. |
| `FileList`, `FileStat`, `FileGet`, `FilePut`, `FileMkdir`, `FileRemove`, `FileMove` | One file operation on one path. |
| `Dial(ctx, ref, port)` | A byte stream to a port inside the sandbox. To forward a local port, accept connections yourself and call `Dial` once for each. |
| `EgressRecords(ctx, ref, n)` | The connections the egress gateway recorded for the sandbox, newest first. |
| `MintEnvironmentKey`, `RevokeEnvironmentKey` | Issue a key that a worker or a gateway of an environment connects with, and end it by its `jti`. The key's value is returned once, when it is minted. |
| `ServerVersion` | The server's version. No token needed. |

## Events

Every change to an object, and every operation on a sandbox, is one
record. `Events` reads one page of an object's history, newest first.
`FollowEvents` keeps the connection open and returns each record as it
happens:

```go
page, _, err := c.Events(ctx, "agent-7", client.EventOptions{Limit: 1})
cursor := ""
if len(page.Items) > 0 {
    cursor = client.After(page.Items[0].Seq)
}
feed, err := c.FollowEvents(ctx, client.FollowOptions{Object: "agent-7", Cursor: cursor})
defer feed.Close()
for {
    event, err := feed.Next()
    if errors.Is(err, io.EOF) {
        break // the sandbox was deleted, or the server closed the feed
    }
    if err != nil {
        return err
    }
    // event.Type, event.Seq, event.Reason, event.Raw
}
```

Following from the newest record you read closes the gap between the read
and the follow: anything that happened in between is in the feed. With no
cursor the feed starts from now. With no `Object` it follows every object
you may read, from now; a cursor is a position in one object's history, so
it needs an `Object`. Reconnect with `client.After(lastSeq)` and nothing is
sent twice or skipped.

A failure after the feed opened, such as your token expiring, arrives from
`Next` as a `*client.Error`.

## Errors

| Type | Meaning |
|---|---|
| `*client.Error` | The server refused. `Code` is what your program checks, `Message` is a sentence for a person, `RequestID` identifies the call in the server's records, and `Details` holds everything else the refusal carried. `Status` is the HTTP status, or 500 for a failure inside a stream or a session. |
| `*client.Unreachable` | No answer arrived: the dial, the TLS handshake or the wait for the first byte failed. |
| `*client.NoBearer` | The token file could not be read or was empty. |
| `*client.StreamError` | A transfer failed after its first byte; `Code` is the server's reason where it gave one. |

`client.CodeOf(err)` returns the code of a refusal and the empty string for
anything else:

```go
switch client.CodeOf(err) {
case "not_found":
    // gone already
case "phase_conflict":
    // not in a state that allows this; read it and decide
}
```

The client never retries. A caller that wants to retry decides by the code
or by the type.
