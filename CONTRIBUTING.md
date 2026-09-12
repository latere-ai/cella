# Contributing

Thanks for looking. This file is for people and agents changing Cella.
Users read the [README](README.md) and [`docs/`](docs/README.md); the
design and the reasoning behind it live in [`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer and `git`. Then:

```sh
make run   # cellad on loopback with the native backend, state under out/
make       # the quality gate
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

Install the hooks once with `make hooks`. They run formatting and licence
checks before a commit and the linter before a push, so you see a finding
before CI does.

## Sending a change

Fork the repository, work on a branch, and open a pull request. Keep one
logical change per commit, stage the files explicitly, and write the
subject in the imperative, saying what changed for whoever reads the log.
Maintainers push to `main` directly; the pipeline runs the gate on every
push and pull request, and a `v*` tag cuts a release.

If you are planning something large, open an issue first. A design that
lands without a spec is harder to review than one that arrives with the
reasoning attached.

## The bar

`make` runs the whole gate (`go tool lateregate`): formatting, the
linter, modernization, known vulnerabilities, the suite with and without
the race detector, per-package coverage at 90% or more, the suite with
only the toolchain on `PATH`, the suite against an empty temporary
directory, the licence notice, the dependency allow list, and the spec
tree. `go tool lateregate list` names the gates and
`go tool lateregate <name>` runs one.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so
the exception is reviewable rather than invisible.

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. The implementation follows the spec, and a divergence is
recorded in the spec's Outcome section rather than left in the code.
Names in a spec (manifest fields, error codes, environment variables,
metrics) are the names the code uses.

Small fixes do not need a spec. Anything that changes the manifest
schema, the `/v1` API, a webhook or event payload, or a configuration
variable does.

## Where a package belongs

Three places, by who imports it:

- The module root (`manifest/`, `runtime/`, `controller/`) holds the
  packages a platform built on Cella imports. A change there keeps
  existing call sites compiling or names the break in the CHANGELOG.
- `internal/` holds what only `cellad` needs: the HTTP API, identity,
  configuration, the stores.
- A generic package with a plausible second consumer outside Cella
  belongs in [`latere.ai/x/pkg`](https://github.com/latere-ai/pkg), the
  shared library Cella already depends on. If you are unsure, put it in
  `internal/` and say so in the pull request.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields and the `cella` command's
output, the contributor in specs, this file, package documentation, and
commit messages, the developer in logs, `/readyz`, and error details. The
rule and the review checklist are in
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md).

## Reporting a vulnerability

Do not open an issue. [`SECURITY.md`](SECURITY.md) says where to send it.
