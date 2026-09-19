---
title: "Runtime conformance suite: one executable contract every driver passes"
status: complete
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
	Image         string   // CreateSpec.Image; empty for a driver that runs host processes
	Shell         []string // runs one script; ["sh", "-c"] when nil
	NoMainCommand bool     // the driver refuses CreateSpec.Command; LogsFollow is skipped
	Down, Up      func()   // make Ready fail and recover; nil skips that half of PreflightAndReady
}

// Run drives every case under t, one subtest each. open returns a fresh
// driver over the data plane under test; the suite calls it once per case.
func Run(t *testing.T, open func(t *testing.T) runtime.Driver, opts Options)
```

[[004-runtime-contract]] wrote the opener as `func() runtime.Driver`.
The opener takes the case's `*testing.T` so a driver can allocate a
per-case directory and register its `Close` with `t.Cleanup`; calling
the parent's `Fatal` from a subtest goroutine is undefined in
`testing`. Spec 004's sentence is amended in this slice. `Down` and `Up`
take no argument: they act on the data plane the opener was given, not on
the case.

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
`TestNoLatereCoordinates`: every file under `runtime/`, not only every
Go file, is free of `sandbox-base`, `latere-k8s`, `sandbox-pool`, and
`sandbox-workloads`, and names `cella.latere.ai` only as the API group
004 stamps. The two bytes around the name separate the group from the
host: a following `/` makes it the group-qualified prefix and is
allowed, a preceding `/` makes it a URL authority and is a finding, as
is the bare name. The needles are assembled from pieces, so the file
is not its own finding. Later slices extend its root list as they add
packages.

## Not in this spec

The optional interfaces of [[004-runtime-contract]] (`Attacher`,
`Dialer`, `VolumeDriver`, `Snapshotter`, `DisplayDriver`,
`InputDriver`) and their cases; `Watch`; the 64 MiB streaming memory
check and `PhaseTableMatchesPackageDoc`; adopting `Nop` in controller
and API tests; any change to `runtime.Driver`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `native` passes every case of the suite in the unit run, `-race` on | `TestNativeConformance` | passing |
| A driver that declares `Attach` and refuses `Stdin`, one that declares `Files` and refuses tar while `Stopped`, and one that reports an isolation outside the four classes each fail the suite; native unwrapped does not | `TestConformanceCatchesAFalseCapability` | passing |
| `Nop` satisfies `runtime.Driver`, returns zero values, and an embedder's override is called while inherited methods still no-op | `TestNopReturnsZeroValues`, `TestNopEmbedOverride` | passing |
| `manifest`, `manifest/v1`, `runtime`, `runtime/native`, `controller` import only the standard library, `latere.ai/x/cella/{manifest,manifest/v1,runtime}`, and their own engine client | `TestRootPackagesDialNothing` | passing |
| No file under `runtime/` names a Latere host, image, pool, or namespace | `TestNoLatereCoordinates`, `TestNoLatereCoordinatesCatchesEachName` | passing |
| `runtimetest` and `runtime/native` statement coverage above 90% | `go tool lateregate cover` | passing |

## Outcome

`latere.ai/x/cella/runtime/runtimetest` holds the twelve cases of the
table above plus `DeclaredWithoutCase`, over `runtime.Driver` as
`runtime/driver.go` declares it today. The interface did not change.
`Nop` and `NopExec` are the port of the sandbox's `runtimetest.Nop`
over cella's types; nothing was rewired to use them in this slice.

`runtime/native` runs the suite as `TestNativeConformance`, opening a
driver over a per-case `t.TempDir()`, with `Down` removing the root and
`Up` recreating it. Eleven cases run and pass; `DetachRecovers` skips,
because native declares `Files` alone, and `DeclaredWithoutCase` reports
nothing. `ExecStreamsAndExits` exercises the `Attach` refusal path, and
`TarOutAndIn` the `Files` transfers while `Stopped`.

The suite proves it can fail. The cases are written over `tb`, the part
of `testing.TB` they use, and `TestConformanceCatchesAFalseCapability`
drives the registry through a recorder that implements it: a case runs
on its own goroutine so `Fatalf` and `Skipf` unwind it through
`runtime.Goexit` and its cleanups still run, exactly as `testing` does.
Three wrappers over the native driver each fail their case with the
assertion named, and the same case over the same harness with the driver
unwrapped records nothing:

| Wrapper | Case | Assertion that fails |
|---|---|---|
| declares `Attach`, refuses `Stdin` | `ExecStreamsAndExits` | `Exec with Stdin under Attach` |
| declares `Files`, refuses tar while `Stopped` | `TarOutAndIn` | `ImportTar while Stopped under Files` |
| reports `Isolation` outside the four classes | `NameIsolationCapabilities` | `Isolation "sandboxed" is not one of [container vm process none]` |

`TestUndeclaredCapabilitiesAreSkippedNotAsserted` pins the other half of
the gating rule: over a driver declaring nothing, `TarOutAndIn` reports
the skipped `Files` half instead of asserting it, `DetachRecovers` skips
without `Detach` and runs with it, `LogsFollow` skips under
`NoMainCommand`, and `PreflightAndReady` reports the missing `Down`.

`arch_test.go` at the module root, package `cella_test`, reads the whole
build list of `manifest`, `manifest/v1`, `runtime`, `runtime/native` and
`controller` through `go list -deps` and admits the standard library,
`latere.ai/x/cella/{manifest,manifest/v1,runtime}`, and a per-package
list of engine clients, empty for all five today. An import under
`internal/` is named as its own failure. The package holds no source
file, so it adds no statement to the coverage gate and no import to any
build list.

Statement coverage: `runtime/runtimetest` 98.9% (555/561),
`runtime/native` 93.4% (566/606). Six statements are left: the timeout
arms of `waitPhase` and `touchStampsActivity`, which a driver that
reaches its phase does not take, and `Run`'s per-capability subtest,
which a driver declaring nothing without a case does not open.
`go test -race ./runtime/...` passes. On the whole bar, `go tool
lateregate`, 16 gates pass and 3 are skipped for features this
repository does not have.

Two divergences from the design above, both amended in it: `Options.Down`
and `Options.Up` take no argument, because they act on the data plane the
opener was given rather than on the case; and `TestNoLatereCoordinates`
reads the byte before the API group as well as the byte after it, so a
URL authority is a finding while the group-qualified prefix is not.
