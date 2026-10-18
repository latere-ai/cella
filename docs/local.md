# Run sandboxes on your own computer

The local runtime runs each sandbox as a process on your own computer,
confined by the operating system's own sandbox: Seatbelt on macOS, and
bubblewrap with seccomp on Linux. A sandbox works in a folder you chose,
with the tools you already have installed, and reaches nothing else of
yours. It is what a worker on a person's computer runs, so that an agent
somewhere else can run commands and edit files in that folder.

It runs no image and no container. Where you want those, use the Podman
or Kubernetes runtime.

## What you need

- macOS or Linux.
- The sandbox runtime, `srt`, on `PATH`, and the programs it uses:

  ```sh
  npm install -g @anthropic-ai/sandbox-runtime
  brew install ripgrep                      # macOS
  sudo apt-get install -y bubblewrap socat ripgrep   # Debian, Ubuntu
  srt echo ok
  ```

  On Ubuntu 24.04 and later, AppArmor may block the unprivileged user
  namespaces bubblewrap needs, and `srt echo ok` fails until they are
  allowed.
- Your own account. A worker started as root refuses to start with
  `local_runs_as_root`: the local runtime confines a person's own
  account, and as root no folder you grant would hold anything back.

The worker checks all of this before it registers, and names what is
missing and how to install it.

## Run the worker

Apply an environment with `mode: worker` and `isolation: process`, mint
its key, and start `cellad worker` with the local runtime and the folders
you grant ([Self-hosting a data plane](workers.md) is the whole
walkthrough):

```sh
CELLA_URL=https://api.example.com/v1/environments \
CELLA_ENVIRONMENT_KEY=... \
CELLA_RUNTIME=local \
CELLA_LOCAL_GRANTS="$HOME/code/site:rw,$HOME/docs:ro" \
CELLA_LOCAL_READ_PATHS="$HOME/.gitconfig" \
CELLA_LOCAL_NETWORK=allowlist \
CELLA_LOCAL_HOSTS=github.com,registry.npmjs.org \
cellad worker
```

These variables are your computer's own policy, and nothing the control
plane sends widens them. [Configuration](configuration.md#local) has each
one. The worker registers your operating system, architecture, granted
folders and network, and the environment shows them in `status.hosts`.

## The folders you grant

A grant is a folder a sandbox may use as its workspace, read and write
(`:rw`) or read only (`:ro`). A manifest names one in
`spec.workspace.host`:

```json
{"apiVersion": "cella.latere.ai/v1beta1", "kind": "Sandbox",
 "metadata": {"name": "site"},
 "spec": {"environment": "laptop", "workspace": {"host": "/Users/dev/code/site"}}}
```

The folder is used where it is: never copied, never moved, and never
deleted. The workspace path and the working directory are the folder
itself. Deleting the sandbox deletes only its own scratch directories, so
the folder holds exactly what the sandbox last wrote. A folder outside
every grant is refused with `workspace_not_granted`, at the control plane
and again on your computer, which has the last word.

A start refuses a grant of the root, of your home directory itself, of a
system directory such as `/usr`, `/etc`, `/var` or `/Library`, of a
folder another user owns, or of anything on the deny table below. A
folder inside your home directory is the usual grant.

## What a command sees

Every command in a sandbox runs with:

- the granted folder of its workspace, read and write or read only as
  you granted it;
- its own home and temporary directories, read and write. `HOME` and
  `TMPDIR` point there, and so do the caches of the common tools
  (`XDG_CACHE_HOME`, `GOCACHE`, `GOMODCACHE`, `npm_config_cache`,
  `PIP_CACHE_DIR`), so a build never writes into your own home;
- each path of `CELLA_LOCAL_READ_PATHS`, read only where it is. A file
  directly in your home directory, such as `~/.gitconfig`, is also linked
  into the sandbox's home, so tools find it;
- each directory of your `PATH`, read only, so your own tools run. Only
  the directory itself: `~/.cargo/bin` is readable and `~/.cargo` is not,
  so a toolchain that needs more, such as `~/.rustup`, goes in
  `CELLA_LOCAL_READ_PATHS`;
- the system's own directories, read only.

Nothing else in your home directory is readable. A table of paths that
hold credentials is always denied: `~/.ssh`, `~/.aws`, `~/.config/gcloud`,
`~/.kube`, `~/.netrc`, `~/.gnupg`, `~/.docker/config.json`,
`~/.git-credentials`, `~/.npmrc`, `~/.pypirc` and `~/.env`. It holds
inside every grant, and through `..` or a link a command places. `CELLA_LOCAL_DENY` adds paths of your own, such as
a production secret inside a granted project. A secret file you did not
name inside a granted folder is part of what you granted.

The sandbox's scratch directories are reported in `status.paths.home`
and `status.paths.tmp`. The operating system's sandbox cannot move paths,
so a caller that places a file of its own, such as a helper program or
an attachment, writes it there through the file routes.

## The network

Your computer narrows what a sandbox asks for and never widens it:

| `CELLA_LOCAL_NETWORK` | Sandbox asks `none` | Sandbox asks `allowlist` | Sandbox asks `open` |
|---|---|---|---|
| `none` | no network | no network | no network |
| `allowlist` | no network | the sandbox's hosts that are also on your list | your list |

A sandbox that asks for more than your computer allows is created with
what it allows, and carries a warning naming what it got. There is no
`open` network: the sandbox runtime allows named hosts only. The sandbox
runtime's own proxy is the only route out, so an environment on the
local runtime needs no egress gateway and reports `EgressEnforced`
`True` with the reason `DriverEnforced`.

## Commands

A command with no input runs detached: it keeps running if the worker's
process ends unexpectedly, and a worker you stop on purpose stops it. A
command with input runs attached to the worker, which holds its pipes;
if the worker's process ends, the command ends with it and is reported
with exit code `-1`. There is no terminal: a request for one is refused
with `capability_unsupported`.

## Files

The file routes run in the worker, outside the operating system's
sandbox, and hold the same lines: a path outside the workspace and the
scratch directories is refused, a path on the deny table is refused even
inside a grant, and a write into a folder granted read only is refused
with `workspace_not_granted`. An archive is never extracted into a
granted folder, so one call cannot overwrite a folder of yours; extract
it into the sandbox's scratch and move what you need.

## What it does not do

No image, no container, no volumes (your granted folders take their
place), no terminal and no attach, no connection into a sandbox's ports,
no desktop, and no Windows.

## In your own program

A program that runs the worker itself, with its own prompts and its own
key store ([Embedding the worker](workers.md#embedding-the-worker)),
opens the local runtime with the same policy:

```go
d, err := local.New(local.Options{
	DataDir:   filepath.Join(stateDir, "sandboxes"),
	Grants:    []local.Grant{{Path: "/Users/dev/code/site", Write: true}},
	ReadPaths: []string{"/Users/dev/.gitconfig"},
	Network:   local.Network{Mode: local.NetworkAllowlist, Hosts: []string{"github.com"}},
})
if err != nil {
	return err
}
defer d.Close()
err = worker.Run(ctx, worker.Options{URL: url, Key: readKey, Driver: d, Policy: askThePerson})
```

When the program runs as root, `worker.Run` returns an error that
`errors.Is` matches to `local.ErrRunsAsRoot`.
