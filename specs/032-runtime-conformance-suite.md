---
title: "Runtime conformance suite: one executable contract every driver passes"
status: in-progress
track: core
depends_on:
  - specs/004-runtime-contract.md
  - specs/031-hosted-sandbox-consolidation.md
affects: [runtime/runtimetest/, runtime/native/, runtime/, arch_test.go, specs/]
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Runtime conformance suite

## Overview

Slice 032 of [[031-hosted-sandbox-consolidation]]. It ports
`sandbox/internal/runtime/runtimetest` and the driver assertions spread
over the sandbox's native, podman and k8s test files into one package,
`latere.ai/x/cella/runtime/runtimetest`, over the `runtime.Driver` of
[[004-runtime-contract]] as it exists today. Every later driver port
(033 to 036) proves itself against this package, so it lands first.

The source `runtimetest` holds one thing, `Nop`, a no-op `Runtime` test
fakes embed. The sandbox never had a shared conformance suite; each
driver's tests restated the same lifecycle claims in their own words
(`TestCreateInspectLifecycle`, `TestStartIdempotentWhenAlreadyRunning`,
`TestTouchActivityReflectedInInspect`, `TestListFiltersByOwner`,
`TestExecCapturesOutputAndExit`, the tar traversal refusals of
`native/files.go`). This slice restates those claims once, as cases of
`runtimetest.Run`, and ports `Nop` over cella's interface.

Two acceptance criteria of [[001-architecture]] and one of 031 also land
here because they are tests over the packages this slice touches:
`TestRootPackagesDialNothing` and `TestNoLatereCoordinates`.

## Design

### Shape

```go
package runtimetest

// Options tells the suite what the driver under test can be asked for.
type Options struct {
	Image         string             // CreateSpec.Image; empty for a driver that runs host processes
	Shell         []string           // runs one script; ["sh", "-c"] when nil
	NoMainCommand bool               // the driver refuses CreateSpec.Command; LogsFollow is skipped
	Down, Up      func(t *testing.T) // make Ready fail and recover; nil skips that half of PreflightAndReady
}

// Run drives every case under t, one subtest each. open returns a fresh
// driver over the data plane under test; the suite calls it once per case.
func Run(t *testing.T, open func(t *testing.T) runtime.Driver, opts Options)
```

[[004-runtime-contract]] wrote the opener as `func() runtime.Driver`.
The opener takes the case's `*testing.T` so a driver can allocate a
per-case directory and register its `Close` with `t.Cleanup`; calling
the parent's `Fatal` from a subtest goroutine is undefined in
`testing`. Spec 004's sentence is amended in this slice.

The cases are written over a small interface (`Helper`, `Errorf`,
`Fatalf`, `Skipf`, `Logf`, `Cleanup`) that `*testing.T` satisfies, and
held in a registry `Run` iterates. `TestConformanceCatchesAFalseCapability`
drives the same registry with a recording implementation and asserts a
failure, which is how the suite proves it can fail; `testing.TB` cannot
be implemented outside `testing`, so the seam is the package's own.

### Cases

The names are spec 004's where 004 has one, so its table maps onto
`go test -run` output.

| Case | Asserts |
|---|---|
| `NameIsolationCapabilities` | `Name` is non-empty and stable over two calls; `Isolation` is one of `container`, `vm`, `process`, `none` and stable; `Capabilities` is equal over two calls |
| `PreflightAndReady` | both nil on a ready driver; with `Options.Down`, `Ready` returns an error, then `Options.Up` restores it |
| `CreateInspectDelete` | `Create` returns the requested id; `Inspect` round-trips id, name, owner, labels; `Phase` reaches `Running`; `CreatedAt`, `StartedAt`, `LastActivityAt` set, `StoppedAt` zero; `State.Isolation` equals `Isolation()`; a second `Create` of the id is `ErrAlreadyExists`; `Delete` then `Inspect` is `ErrNotFound`; a second `Delete` is nil or `ErrNotFound`; `Inspect`, `Start`, `Stop`, `Update`, `Touch` of an unknown id are `ErrNotFound` |
| `StopStartKeepsTheWorkspace` | `Stop` sets `Stopped` and `StoppedAt`; a second `Stop` changes nothing; `Start` sets `Running`, clears `StoppedAt`, advances `StartedAt`; a second `Start` changes nothing; a file imported before the cycle is read after it |
| `UpdateEveryMutableField` | `Labels` replace and are read back; `Env` is observed by `Exec`; `Lifecycle` sets `AutoStop`, `AutoDelete`, and `ExpiresAt = CreatedAt + TTL`; a zero `TTL` clears `ExpiresAt` |
| `ListReadsIdentityBack` | three sandboxes created; `List` with an empty filter contains each with name, owner, labels as written |
| `FilterSelectsOnLabels` | `Owner`, `Phase`, `IDs` each narrow: every returned state matches the filter and the expected ids are present; the three combined select one |
| `ExecStreamsAndExits` | exit 0 with stdout and stderr split; exit 3; request `Env` overrides the sandbox's; `Workdir` lands in a subdirectory; the first stdout byte arrives before the command finishes; `Timeout` ends `Wait` with `context.DeadlineExceeded`; cancelling the request context ends `Wait` with an error; `Close` ends a running command; empty `Command` is `ErrInvalid`; on a `Stopped` sandbox `ErrNotRunning`; with `Attach` declared, `Stdin` reaches the command, without it `Stdin` and `TTY` are `ErrUnsupported` |
| `LogsFollow` | skipped with `NoMainCommand`; a main command's output before and after `Logs` opened arrives with `Follow`; the reader ends when the process ends; `TailLines` and `Since` narrow the snapshot; the exit code lands in `State` |
| `TarOutAndIn` | a tree round-trips with modes; the imported tree is read by `Exec` in a second sandbox; entries `../x`, `/abs`, a symlink, a hard link are `ErrInvalid`; a destination outside `/workspace` is `ErrInvalid`; with `Files` declared both directions work while `Stopped` |
| `TouchStampsActivity` | `LastActivityAt` after `Touch` is later than before |
| `DetachRecovers` | with `Detach` declared, a second `open` inspects the sandbox the first created; skipped otherwise |
| `DeclaredWithoutCase` | one subtest per declared capability without an observable contract on today's `Driver` (`Dial`, `Volumes`, `Snapshots`, `Display`, `Input`, `Resize`, `Pool`, `Mesh`, `Ingress`, `Egress`): skipped with the name, so the report lists what the driver claims and the suite cannot yet check |

Gating rule: an optional operation the driver does not declare is
skipped and named in the report; one it declares and answers with
`ErrUnsupported` fails the case. Phase changes are polled through
`Inspect` with a deadline, never awaited with a fixed sleep.

### Nop

`Nop` implements every method of `runtime.Driver` and returns zero
values, with two exceptions so an embedder's caller does not
dereference nil: `Exec` returns an exec with empty streams and exit 0,
`Logs` an empty `io.ReadCloser`. `Name` is `nop`, `Isolation` is
`none`. Embed it and override the methods a test drives. It is the
port of the sandbox's `runtimetest.Nop`; controller and API tests may
adopt it later, this slice does not rewire them.

### Native under the suite

`runtime/native/conformance_test.go` holds `TestNativeConformance`:
`runtimetest.Run` with an opener of `native.New(t.TempDir())` closed in
`t.Cleanup`, `Down` removing the root and `Up` recreating it. Native
declares `Files` only, so `Attach` refusals, `Files` while stopped and
the `LogsFollow` main-command case run; `DetachRecovers` is skipped.

### The two architecture tests

`arch_test.go` at the module root, package `cella_test`, holds
`TestRootPackagesDialNothing`: for each of `manifest`, `manifest/v1`,
`runtime`, `runtime/native`, `controller`, `go list -deps -f
'{{.ImportPath}} {{.Standard}}'` and every non-standard import is in
`latere.ai/x/cella/{manifest,manifest/v1,runtime}` or the package's own
allow list of engine clients (none for `native`). The root is the
convention: the test is about the module's shape, not one package's,
and a test-only package holds no statements so the coverage gate skips
it. The hermetic gate puts the toolchain directory on `PATH`, so `go
list` runs under it.

`runtime/coordinates_test.go`, package `runtime_test`, holds
`TestNoLatereCoordinates`: every file under `runtime/` is free of a
bare `cella.latere.ai` (the group-qualified prefix `cella.latere.ai/`
is the API group 004 stamps and is allowed), `sandbox-base`,
`latere-k8s`, `sandbox-pool`, and `sandbox-workloads`. Later slices
extend its root list as they add packages.

## Not in this spec

The optional interfaces of [[004-runtime-contract]] (`Attacher`,
`Dialer`, `VolumeDriver`, `Snapshotter`, `DisplayDriver`,
`InputDriver`) and their cases; `Watch`; the 64 MiB streaming memory
check and `PhaseTableMatchesPackageDoc`; adopting `Nop` in controller
and API tests; any change to `runtime.Driver`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `native` passes every case of the suite in the unit run, `-race` on | `TestNativeConformance` | open |
| A driver that declares `Attach` and refuses `Stdin`, one that declares `Files` and refuses tar while `Stopped`, and one that reports an isolation outside the four classes each fail the suite; native unwrapped does not | `TestConformanceCatchesAFalseCapability` | open |
| `Nop` satisfies `runtime.Driver`, returns zero values, and an embedder's override is called while inherited methods still no-op | `TestNopReturnsZeroValues`, `TestNopEmbedOverride` | open |
| `manifest`, `manifest/v1`, `runtime`, `runtime/native`, `controller` import only the standard library, `latere.ai/x/cella/{manifest,manifest/v1,runtime}`, and their own engine client | `TestRootPackagesDialNothing` | open |
| No file under `runtime/` names a Latere host, image, pool, or namespace | `TestNoLatereCoordinates` | open |
| `runtimetest` and `runtime/native` statement coverage above 90% | `go tool lateregate cover` | open |
