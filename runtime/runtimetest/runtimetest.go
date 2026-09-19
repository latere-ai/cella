// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package runtimetest is the conformance suite of runtime.Driver and the Nop
// driver test fakes embed. A driver that passes Run works under the controller
// and under a worker; every driver package runs it in its own tests.
package runtimetest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
)

// Options tells the suite what the driver under test can be asked for.
type Options struct {
	// Image is what CreateSpec.Image carries; empty for a driver that runs host processes.
	Image string
	// Shell runs one script given as its last argument; ["sh", "-c"] when nil.
	Shell []string
	// NoMainCommand says the driver refuses CreateSpec.Command; LogsFollow is then skipped.
	NoMainCommand bool
	// Down makes Ready fail and Up restores it; nil skips that half of PreflightAndReady.
	Down, Up func()
}

// tb is the part of testing.TB the cases use. testing.TB cannot be implemented
// outside package testing, so the suite's own tests drive the cases through a
// recorder that implements this instead.
type tb interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(func())
}

type caseDef struct {
	name string
	run  func(t tb, open func() runtime.Driver, opts Options)
}

// cases is the registry Run iterates; the names are spec 004's.
var cases = []caseDef{
	{"NameIsolationCapabilities", nameIsolationCapabilities},
	{"PreflightAndReady", preflightAndReady},
	{"CreateInspectDelete", createInspectDelete},
	{"StopStartKeepsTheWorkspace", stopStartKeepsTheWorkspace},
	{"UpdateEveryMutableField", updateEveryMutableField},
	{"ListReadsIdentityBack", listReadsIdentityBack},
	{"FilterSelectsOnLabels", filterSelectsOnLabels},
	{"ExecStreamsAndExits", execStreamsAndExits},
	{"LogsFollow", logsFollow},
	{"TarOutAndIn", tarOutAndIn},
	{"TouchStampsActivity", touchStampsActivity},
	{"DetachRecovers", detachRecovers},
}

// pollTimeout bounds every wait on an observed state change.
var pollTimeout = 5 * time.Second

// Run drives every case under t, one subtest each. open returns a fresh driver
// over the data plane under test and registers its shutdown with t.Cleanup; the
// suite calls it once per case, and twice in DetachRecovers.
//
// A capability the driver does not declare skips its case with the name in the
// report. One it declares and answers with ErrUnsupported fails. A declared
// capability with no observable contract on today's Driver is reported under
// DeclaredWithoutCase as a skipped subtest per capability.
func Run(t *testing.T, open func(t *testing.T) runtime.Driver, opts Options) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, func() runtime.Driver { return open(t) }, opts)
		})
	}
	t.Run("DeclaredWithoutCase", func(t *testing.T) {
		for _, name := range declaredWithoutCase(open(t).Capabilities()) {
			t.Run(name, func(t *testing.T) { t.Skipf("%s is declared; this suite has no case for it yet", name) })
		}
	})
}

// declaredWithoutCase lists the declared capabilities the suite cannot check
// on the current Driver: their operations are not in the interface yet.
func declaredWithoutCase(c runtime.Capabilities) []string {
	var out []string
	for _, f := range []struct {
		name string
		on   bool
	}{{"Egress", len(c.Egress) > 0}, {"Mesh", c.Mesh}, {"Ingress", c.Ingress}, {"Volumes", c.Volumes}, {"Snapshots", c.Snapshots}, {"Dial", c.Dial}, {"Display", c.Display}, {"Input", c.Input}, {"Resize", c.Resize}, {"Pool", c.Pool}} {
		if f.on {
			out = append(out, f.name)
		}
	}
	return out
}

func (o Options) shell(script string) []string {
	sh := o.Shell
	if sh == nil {
		sh = []string{"sh", "-c"}
	}
	return append(slices.Clone(sh), script)
}

// expect records a failed assertion and continues; need stops the case.
func expect(t tb, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}
func need(t tb, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
func must(t tb, err error, what string) {
	t.Helper()
	need(t, err == nil, "%s: %v", what, err)
}
func wantErr(t tb, err, target error, what string) {
	t.Helper()
	expect(t, errors.Is(err, target), "%s: got %v, want %v", what, err, target)
}

// create creates the sandbox, waits for Running, and deletes it at cleanup so a
// shared data plane holds nothing after the case.
func create(t tb, d runtime.Driver, opts Options, spec runtime.CreateSpec) runtime.State {
	t.Helper()
	if spec.Image == "" {
		spec.Image = opts.Image
	}
	ref, err := d.Create(context.Background(), spec)
	must(t, err, "Create "+spec.ID)
	t.Cleanup(func() { _ = d.Delete(context.Background(), spec.ID) })
	need(t, ref.ID == spec.ID, "Create returned id %q, want %q", ref.ID, spec.ID)
	return waitPhase(t, d, spec.ID, runtime.Running)
}

// waitPhase polls Inspect until the phase is reached or pollTimeout passes.
func waitPhase(t tb, d runtime.Driver, id, phase string) runtime.State {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	var state runtime.State
	for {
		var err error
		state, err = d.Inspect(context.Background(), id)
		must(t, err, "Inspect "+id)
		if state.Phase == phase {
			return state
		}
		need(t, time.Now().Before(deadline), "%s stayed at %q, want %q", id, state.Phase, phase)
		time.Sleep(10 * time.Millisecond)
	}
}

// run executes one request, drains both streams, and waits with a bound.
func run(t tb, d runtime.Driver, id string, req runtime.ExecRequest) (stdout, stderr string, code int, err error) {
	t.Helper()
	e, err := d.Exec(context.Background(), id, req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = e.Close() }()
	out, errOut := drain(e)
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	code, err = e.Wait(ctx)
	return out, errOut, code, err
}

// drain reads both streams to their end, concurrently, as the contract asks.
func drain(e runtime.Exec) (stdout, stderr string) {
	var out, errOut []byte
	var wg sync.WaitGroup
	wg.Go(func() { out, _ = io.ReadAll(e.Stdout()) })
	wg.Go(func() { errOut, _ = io.ReadAll(e.Stderr()) })
	wg.Wait()
	return string(out), string(errOut)
}

// script runs one shell script and returns its stdout, failing on any error
// or nonzero exit.
func script(t tb, d runtime.Driver, opts Options, id, src string) string {
	t.Helper()
	out, errOut, code, err := run(t, d, id, runtime.ExecRequest{Command: opts.shell(src)})
	must(t, err, "Exec "+src)
	need(t, code == 0, "Exec %q exited %d: %s", src, code, errOut)
	return out
}

// entry is one tar entry the suite writes or reads back.
type entry struct {
	name, body, link string
	mode             int64
	typ              byte
}

func archive(t tb, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if typ != tar.TypeReg {
			size = 0
		}
		must(t, tw.WriteHeader(&tar.Header{Name: e.name, Mode: mode, Size: size, Typeflag: typ, Linkname: e.link}), "tar header")
		if size > 0 {
			_, err := tw.Write([]byte(e.body))
			must(t, err, "tar body")
		}
	}
	must(t, tw.Close(), "tar close")
	return b.Bytes()
}

func readArchive(t tb, r io.Reader) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		must(t, err, "tar next")
		body, err := io.ReadAll(tr)
		must(t, err, "tar read")
		out[h.Name] = entry{name: h.Name, body: string(body), mode: h.Mode, typ: h.Typeflag}
	}
}

func importFiles(t tb, d runtime.Driver, id, dest string, entries ...entry) {
	t.Helper()
	must(t, d.ImportTar(context.Background(), id, dest, bytes.NewReader(archive(t, entries...))), fmt.Sprintf("ImportTar %s into %s", id, dest))
}
