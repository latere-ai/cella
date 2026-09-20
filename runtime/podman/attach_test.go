// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// terminal reads a session in the background so a test waits for what the
// engine wrote with a bound rather than blocking in Read.
type terminal struct {
	mu   sync.Mutex
	buf  []byte
	err  error
	done chan struct{}
}

func watch(r io.Reader) *terminal {
	tm := &terminal{done: make(chan struct{})}
	go func() {
		defer close(tm.done)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			tm.mu.Lock()
			tm.buf = append(tm.buf, b[:n]...)
			tm.err = err
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return tm
}
func (tm *terminal) text() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return string(tm.buf)
}
func (tm *terminal) readErr() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.err
}
func (tm *terminal) await(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(tm.text(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("the session never wrote %q; it wrote %q", marker, tm.text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// interactive arranges an engine whose exec sessions stay open and echo, which
// is what an attached terminal does.
func interactive(t *testing.T) (*fake, *Driver) {
	t.Helper()
	f := newFake(t)
	f.run = func([]string, []string, string) fakeExecResult { return fakeExecResult{code: 5, block: true} }
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"GREETING": "hello"}})
	return f, d
}

// TestAttachRoundTripOverTheFakeEngine drives a session end to end: the create
// body carries the TTY and the sandbox's own environment, bytes travel both
// ways over the hijacked connection, and a resize reaches the engine.
func TestAttachRoundTripOverTheFakeEngine(t *testing.T) {
	f, d := interactive(t)
	s, err := d.Attach(t.Context(), "sbx_a", driver.AttachRequest{Cols: 80, Rows: 24, Env: map[string]string{"TERM": "xterm"}, Workdir: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	e := f.session(t)
	if !e.tty || !e.stdin || !slices.Equal(e.cmd, defaultShell) || e.dir != "/workspace" {
		t.Fatalf("the created session is %+v", e)
	}
	if !slices.Equal(e.env, []string{"GREETING=hello", "TERM=xterm"}) {
		t.Fatalf("the session's environment is %v", e.env)
	}
	if got := f.windowOf(e); got != [2]int{24, 80} {
		t.Fatalf("the window at open is %v, want rows 24 columns 80", got)
	}
	tm := watch(s)
	if _, err = s.Write([]byte("printf hi\n")); err != nil {
		t.Fatal(err)
	}
	tm.await(t, "printf hi\n")
	if err = s.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.windowOf(e) != [2]int{43, 132} {
		if time.Now().After(deadline) {
			t.Fatalf("the resize did not reach the engine: %v", f.windowOf(e))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tm.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session's stream stayed open after Close")
	}
	if tm.readErr() == nil {
		t.Error("Read after Close reported no error")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	code, err := s.Wait(ctx)
	if err != nil || code != 5 {
		t.Fatalf("Wait: %d %v", code, err)
	}
	if got := f.typedInto(e); got != "printf hi\n" {
		t.Fatalf("the engine received %q", got)
	}
}

// TestAttachRefusals covers every way a session is refused before the engine
// is asked to start one.
func TestAttachRefusals(t *testing.T) {
	f, d := interactive(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Attach(ctx, "sbx_a", driver.AttachRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Attach with a cancelled context: %v", err)
	}
	for name, req := range map[string]driver.AttachRequest{
		"negativeColumns": {Cols: -1},
		"negativeRows":    {Rows: -1},
		"blankCommand":    {Command: []string{"  "}},
		"relativeWorkdir": {Workdir: "sub"},
		"traversal":       {Workdir: "/workspace/../etc"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Attach(t.Context(), "sbx_a", req); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("Attach %+v: %v, want ErrInvalid", req, err)
			}
		})
	}
	if _, err := d.Attach(t.Context(), "sbx_absent", driver.AttachRequest{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Attach to an unknown sandbox: %v", err)
	}
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Attach(t.Context(), "sbx_a", driver.AttachRequest{}); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("Attach to a stopped sandbox: %v", err)
	}
	if err := d.Start(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	f.fault("POST "+libpod+"/containers/cella-sbx_a/exec", http.StatusInternalServerError)
	if _, err := d.Attach(t.Context(), "sbx_a", driver.AttachRequest{}); err == nil {
		t.Error("Attach with the create refused returned no error")
	}
	f.fault("POST "+libpod+"/exec/", http.StatusInternalServerError)
	if _, err := d.Attach(t.Context(), "sbx_a", driver.AttachRequest{}); err == nil {
		t.Error("Attach with the start refused returned no error")
	}
	s, err := d.Attach(t.Context(), "sbx_a", driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Resize(0, 24); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("Resize to no columns: %v, want ErrInvalid", err)
	}
}

// TestExecWithStdinOverTheFakeEngine proves a command without a terminal still
// reads its input over the hijacked connection and keeps its two streams apart.
func TestExecWithStdinOverTheFakeEngine(t *testing.T) {
	f := newFake(t)
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: "out", stderr: "err", code: 3}
	}
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("ping")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	out, errOut := readBoth(t, e)
	if out != "out" || errOut != "err" {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if code, err := e.Wait(ctx); err != nil || code != 3 {
		t.Fatalf("Wait: %d %v", code, err)
	}
	session := f.session(t)
	if !session.stdin || session.tty {
		t.Fatalf("the created session is %+v", session)
	}
	if got := f.typedInto(session); got != "ping" {
		t.Fatalf("the engine received %q on stdin", got)
	}
}

// TestExecWithTTYOverTheFakeEngine proves a command under a terminal reads one
// unframed stream and that the second reader is at its end from the start.
func TestExecWithTTYOverTheFakeEngine(t *testing.T) {
	f := newFake(t)
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: "on the terminal\n", stderr: "also the terminal\n"}
	}
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"sh", "-c", "true"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	out, errOut := readBoth(t, e)
	if out != "on the terminal\nalso the terminal\n" || errOut != "" {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if code, err := e.Wait(ctx); err != nil || code != 0 {
		t.Fatalf("Wait: %d %v", code, err)
	}
	if session := f.session(t); !session.tty || session.stdin {
		t.Fatalf("the created session is %+v", session)
	}
}

// TestExecHijackRefused proves a start the engine turns away reaches the
// caller as a failed stream rather than an empty one.
func TestExecHijackRefused(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	f.fault("POST "+libpod+"/exec/", http.StatusInternalServerError)
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"sh"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if _, err = io.ReadAll(e.Stdout()); err == nil {
		t.Fatal("a refused start left the stream clean")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err = e.Wait(ctx); err == nil {
		t.Fatal("a refused start reported no error from Wait")
	}
}

// readBoth drains an execution's two streams concurrently, as the contract
// asks of a caller.
func readBoth(t *testing.T, e driver.Exec) (stdout, stderr string) {
	t.Helper()
	var out, errOut []byte
	var wg sync.WaitGroup
	wg.Go(func() { out, _ = io.ReadAll(e.Stdout()) })
	wg.Go(func() { errOut, _ = io.ReadAll(e.Stderr()) })
	wg.Wait()
	return string(out), string(errOut)
}
