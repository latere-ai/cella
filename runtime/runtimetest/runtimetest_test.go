// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"fmt"
	"io"
	"reflect"
	gort "runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// recorder implements tb so a case can be driven outside `go test` and its
// assertions read back. It is how the suite proves it fails a driver that
// breaks the contract: a passing suite alone says nothing about what the
// suite would catch.
type recorder struct {
	mu       sync.Mutex
	errors   []string
	logs     []string
	fatal    string
	skip     string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, sprintf(format, args...))
}

// Fatalf and Skipf end the case the way testing does: the goroutine running
// it exits, its deferred cleanups run, and nothing after the call executes.
func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatal = sprintf(format, args...)
	r.mu.Unlock()
	gort.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.mu.Lock()
	r.skip = sprintf(format, args...)
	r.mu.Unlock()
	gort.Goexit()
}

func (r *recorder) Cleanup(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, f)
}

// failures is every assertion the case recorded, the fatal one last.
func (r *recorder) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.errors...)
	if r.fatal != "" {
		out = append(out, r.fatal)
	}
	return out
}

func (r *recorder) failed() bool { return len(r.failures()) > 0 }

func (r *recorder) skipped() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skip
}

// report joins everything the case recorded, for a test's failure message.
func (r *recorder) report() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(append(append([]string(nil), r.logs...), append(r.errors, r.fatal, r.skip)...), "\n")
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// drive runs one case to its end on its own goroutine, so Fatalf and Skipf
// unwind it, and runs the cleanups it registered last in first out.
func drive(c caseDef, open func() runtime.Driver, opts Options) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			r.mu.Lock()
			cleanups := r.cleanups
			r.mu.Unlock()
			for _, cleanup := range slices.Backward(cleanups) {
				cleanup()
			}
		}()
		c.run(r, open, opts)
	}()
	<-done
	return r
}

// caseNamed returns the registered case, so a rename here is a compile-time
// or immediate test failure rather than a silently skipped assertion.
func caseNamed(t *testing.T, name string) caseDef {
	t.Helper()
	for _, c := range cases {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no case named %q is registered", name)
	return caseDef{}
}

// openNative builds one native driver over a per-test root and hands the
// suite an opener over it. The cases that call the opener twice read the same
// data plane, which is what a second driver instance over one root gives.
func openNative(t *testing.T) *native.Driver {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("native.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func opener(d runtime.Driver) func() runtime.Driver {
	return func() runtime.Driver { return d }
}

// attachLiar declares Attach without implementing it: the driver under it
// refuses Stdin. A suite that reports this driver as conforming would let a
// lying capability reach the controller, which branches on it.
type attachLiar struct{ *native.Driver }

func (attachLiar) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true, Attach: true}
}

// filesLiar declares Files, which promises transfers while the sandbox is
// Stopped, and refuses exactly those.
type filesLiar struct{ *native.Driver }

func (d filesLiar) ImportTar(ctx context.Context, id, dest string, r io.Reader) error {
	if d.isStopped(ctx, id) {
		return runtime.ErrUnsupported
	}
	return d.Driver.ImportTar(ctx, id, dest, r)
}

func (d filesLiar) ExportTar(ctx context.Context, id string, paths []string, w io.Writer) error {
	if d.isStopped(ctx, id) {
		return runtime.ErrUnsupported
	}
	return d.Driver.ExportTar(ctx, id, paths, w)
}

func (d filesLiar) isStopped(ctx context.Context, id string) bool {
	s, err := d.Inspect(ctx, id)
	return err == nil && s.Phase == runtime.Stopped
}

// isolationLiar reports a class outside the four the contract defines.
type isolationLiar struct{ *native.Driver }

func (isolationLiar) Isolation() string { return "sandboxed" }

// noCapabilities declares nothing, so every optional half of a case is
// skipped instead of asserted.
type noCapabilities struct{ *native.Driver }

func (noCapabilities) Capabilities() runtime.Capabilities { return runtime.Capabilities{} }

// detachClaimer declares Detach over a driver whose records are durable, so
// the DetachRecovers case runs instead of skipping.
type detachClaimer struct{ *native.Driver }

func (detachClaimer) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true, Detach: true}
}

func TestConformanceCatchesAFalseCapability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kase  string
		wrap  func(*native.Driver) runtime.Driver
		wants string
	}{
		{
			name:  "AttachWithoutStdin",
			kase:  "ExecStreamsAndExits",
			wrap:  func(d *native.Driver) runtime.Driver { return attachLiar{d} },
			wants: "Stdin under Attach",
		},
		{
			name:  "FilesWithoutTransfersWhileStopped",
			kase:  "TarOutAndIn",
			wrap:  func(d *native.Driver) runtime.Driver { return filesLiar{d} },
			wants: "ImportTar while Stopped under Files",
		},
		{
			name:  "IsolationOutsideTheFourClasses",
			kase:  "NameIsolationCapabilities",
			wrap:  func(d *native.Driver) runtime.Driver { return isolationLiar{d} },
			wants: "is not one of",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := drive(caseNamed(t, tc.kase), opener(tc.wrap(openNative(t))), Options{})
			if !r.failed() {
				t.Fatalf("%s passed the lying driver: %s", tc.kase, r.report())
			}
			if !strings.Contains(strings.Join(r.failures(), "\n"), tc.wants) {
				t.Errorf("%s failed on %v, want a failure naming %q", tc.kase, r.failures(), tc.wants)
			}
		})
		// The control: the same case, the same harness, the driver unwrapped.
		// Without it a failure above could come from the harness.
		t.Run(tc.name+"/Unwrapped", func(t *testing.T) {
			if r := drive(caseNamed(t, tc.kase), opener(openNative(t)), Options{}); r.failed() {
				t.Errorf("%s failed the native driver: %v", tc.kase, r.failures())
			}
		})
	}
}

// TestUndeclaredCapabilitiesAreSkippedNotAsserted pins the gating rule from
// the other side: the optional halves are not asserted against a driver that
// declares nothing, and the case says so in its log.
func TestUndeclaredCapabilitiesAreSkippedNotAsserted(t *testing.T) {
	r := drive(caseNamed(t, "TarOutAndIn"), opener(noCapabilities{openNative(t)}), Options{})
	if r.failed() {
		t.Fatalf("TarOutAndIn failed a driver declaring no capabilities: %v", r.failures())
	}
	if !strings.Contains(r.report(), "Files is not declared") {
		t.Errorf("TarOutAndIn did not report the skipped half: %s", r.report())
	}

	r = drive(caseNamed(t, "DetachRecovers"), opener(openNative(t)), Options{})
	if r.skipped() == "" {
		t.Errorf("DetachRecovers did not skip without Detach: %s", r.report())
	}
	r = drive(caseNamed(t, "DetachRecovers"), opener(detachClaimer{openNative(t)}), Options{})
	if r.failed() || r.skipped() != "" {
		t.Errorf("DetachRecovers over a declared Detach: failures %v skip %q", r.failures(), r.skipped())
	}

	r = drive(caseNamed(t, "LogsFollow"), opener(openNative(t)), Options{NoMainCommand: true})
	if !strings.Contains(r.skipped(), "NoMainCommand") {
		t.Errorf("LogsFollow did not skip under NoMainCommand: %s", r.report())
	}

	r = drive(caseNamed(t, "PreflightAndReady"), opener(openNative(t)), Options{})
	if r.failed() || !strings.Contains(r.report(), "Options.Down is nil") {
		t.Errorf("PreflightAndReady without Down: failures %v log %s", r.failures(), r.report())
	}
}

// TestDeclaredWithoutCaseNamesEveryUncheckedCapability keeps the reported list
// equal to the capabilities today's Driver has no operation for. A capability
// that gains an operation is removed here and gains a case.
func TestDeclaredWithoutCaseNamesEveryUncheckedCapability(t *testing.T) {
	if got := declaredWithoutCase(runtime.Capabilities{Files: true, Attach: true, Detach: true}); len(got) != 0 {
		t.Errorf("the capabilities with cases are reported as unchecked: %v", got)
	}
	all := runtime.Capabilities{
		Egress: []v1.EgressMode{v1.EgressAllowlist}, Mesh: true, Ingress: true,
		Volumes: true, Snapshots: true, Attach: true, Dial: true, Display: true,
		Input: true, Resize: true, Pool: true, Files: true, Detach: true,
	}
	want := []string{"Egress", "Mesh", "Ingress", "Volumes", "Snapshots", "Dial", "Display", "Input", "Resize", "Pool"}
	if got := declaredWithoutCase(all); !slices.Equal(got, want) {
		t.Errorf("declaredWithoutCase reports %v, want %v", got, want)
	}
}

// TestRunReportsEveryCase drives Run itself, which is what a driver package
// calls, so the registry, the per-case opener and the DeclaredWithoutCase
// report are exercised here and not only through a driver's own test.
func TestRunReportsEveryCase(t *testing.T) {
	Run(t, func(t *testing.T) runtime.Driver { return openNative(t) }, Options{})
}

// TestNopReturnsZeroValues checks the base fakes embed: every method answers
// without an error and without a nil a caller would dereference.
func TestNopReturnsZeroValues(t *testing.T) {
	var d runtime.Driver = Nop{}
	ctx := context.Background()
	if d.Name() != "nop" || d.Isolation() != runtime.IsolationNone {
		t.Errorf("Nop names itself %q with isolation %q", d.Name(), d.Isolation())
	}
	if caps := d.Capabilities(); caps.Attach || caps.Files || caps.Detach || len(caps.Egress) > 0 {
		t.Errorf("Nop declares %+v, want nothing", caps)
	}
	for name, err := range map[string]error{
		"Preflight": d.Preflight(ctx),
		"Ready":     d.Ready(ctx),
		"Start":     d.Start(ctx, "id"),
		"Stop":      d.Stop(ctx, "id"),
		"Delete":    d.Delete(ctx, "id"),
		"Update":    d.Update(ctx, "id", runtime.Change{}),
		"ExportTar": d.ExportTar(ctx, "id", nil, io.Discard),
		"ImportTar": d.ImportTar(ctx, "id", "/workspace", strings.NewReader("")),
		"Touch":     d.Touch(ctx, "id"),
	} {
		if err != nil {
			t.Errorf("Nop.%s: %v", name, err)
		}
	}
	ref, err := d.Create(ctx, runtime.CreateSpec{ID: "id"})
	if err != nil || ref != (runtime.Ref{}) {
		t.Errorf("Nop.Create: %+v, %v", ref, err)
	}
	state, err := d.Inspect(ctx, "id")
	if err != nil || !reflect.DeepEqual(state, runtime.State{}) {
		t.Errorf("Nop.Inspect: %+v, %v", state, err)
	}
	list, err := d.List(ctx, runtime.Filter{})
	if err != nil || len(list) != 0 {
		t.Errorf("Nop.List: %v, %v", list, err)
	}
	logs, err := d.Logs(ctx, "id", runtime.LogsRequest{})
	if err != nil {
		t.Fatalf("Nop.Logs: %v", err)
	}
	body, err := io.ReadAll(logs)
	if err != nil || len(body) != 0 {
		t.Errorf("Nop.Logs read %q, %v", body, err)
	}
	if err := logs.Close(); err != nil {
		t.Errorf("Nop.Logs close: %v", err)
	}
	e, err := d.Exec(ctx, "id", runtime.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("Nop.Exec: %v", err)
	}
	out, errOut := drain(e)
	code, err := e.Wait(ctx)
	if out != "" || errOut != "" || code != 0 || err != nil {
		t.Errorf("Nop.Exec streams %q %q exit %d, %v", out, errOut, code, err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("NopExec.Close: %v", err)
	}
}

// countingNop is the shape an embedder writes: one method overridden, the
// rest inherited.
type countingNop struct {
	Nop
	created int
}

func (c *countingNop) Create(context.Context, runtime.CreateSpec) (runtime.Ref, error) {
	c.created++
	return runtime.Ref{ID: "sbx_counted"}, nil
}

func TestNopEmbedOverride(t *testing.T) {
	var d runtime.Driver = &countingNop{}
	ref, err := d.Create(context.Background(), runtime.CreateSpec{ID: "ignored"})
	if err != nil || ref.ID != "sbx_counted" {
		t.Fatalf("the override was not called: %+v, %v", ref, err)
	}
	if got := d.(*countingNop).created; got != 1 {
		t.Errorf("the override ran %d times, want 1", got)
	}
	if err := d.Touch(context.Background(), "sbx_counted"); err != nil {
		t.Errorf("the inherited Touch: %v", err)
	}
	if d.Name() != "nop" {
		t.Errorf("the inherited Name is %q", d.Name())
	}
}
