// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bytes"
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

// read drains both streams and waits, which is the order the contract asks a
// caller to use.
func read(t *testing.T, e driver.Exec) (stdout, stderr string, code int, err error) {
	t.Helper()
	var out, errOut []byte
	var wg sync.WaitGroup
	wg.Go(func() { out, _ = io.ReadAll(e.Stdout()) })
	wg.Go(func() { errOut, _ = io.ReadAll(e.Stderr()) })
	wg.Wait()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	code, err = e.Wait(ctx)
	return string(out), string(errOut), code, err
}

func TestExecStreamsAndExits(t *testing.T) {
	f := newFake(t)
	f.run = func(cmd, env []string, dir string) fakeExecResult {
		return fakeExecResult{stdout: "out:" + strings.Join(cmd, ",") + "|" + strings.Join(env, ",") + "|" + dir, stderr: "err", code: 3}
	}
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"A": "1"}})
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"sh", "-c", "true"}, Env: map[string]string{"B": "2"}, Workdir: "/workspace/sub"})
	if err != nil {
		t.Fatal(err)
	}
	out, errOut, code, err := read(t, e)
	if err != nil || code != 3 || errOut != "err" {
		t.Fatalf("exec = %q %q %d %v", out, errOut, code, err)
	}
	if !strings.Contains(out, "sh,-c,true") || !strings.Contains(out, "A=1,B=2") || !strings.HasSuffix(out, "|/workspace/sub") {
		t.Fatalf("the request did not reach the engine as written: %q", out)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close after Wait: %v", err)
	}
}

func TestExecReadsTheRecordEnvironment(t *testing.T) {
	f := newFake(t)
	var seen []string
	f.run = func(cmd, env []string, dir string) fakeExecResult {
		seen = slices.Clone(env)
		return fakeExecResult{}
	}
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"GREETING": "hello"}})
	env := map[string]string{"GREETING": "changed"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &env}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := read(t, e); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"GREETING=changed"}) {
		t.Fatalf("the exec saw %v, want the environment Update wrote", seen)
	}
	if got := envList(nil); got != nil {
		t.Fatalf("envList of nothing = %v", got)
	}
}

func TestExecRefusals(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	for name, tc := range map[string]struct {
		req  driver.ExecRequest
		want error
	}{
		"empty":           {driver.ExecRequest{}, driver.ErrInvalid},
		"negativeTimeout": {driver.ExecRequest{Command: []string{"true"}, Timeout: -1}, driver.ErrInvalid},
		"relativeWorkdir": {driver.ExecRequest{Command: []string{"true"}, Workdir: "sub"}, driver.ErrInvalid},
		"traversal":       {driver.ExecRequest{Command: []string{"true"}, Workdir: "/workspace/../etc"}, driver.ErrInvalid},
		"stdin":           {driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("x")}, driver.ErrUnsupported},
		"tty":             {driver.ExecRequest{Command: []string{"true"}, TTY: true}, driver.ErrUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Exec(t.Context(), "sbx_a", tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Exec %+v: %v, want %v", tc.req, err, tc.want)
			}
		})
	}
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Exec while Stopped: %v", err)
	}
}

func TestExecReportsEngineFaults(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	f.fault("GET "+libpod+"/containers/cella-sbx_a/json", http.StatusInternalServerError)
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); err == nil {
		t.Fatal("a refused container inspect did not stop the exec")
	}
	f.fault("POST "+libpod+"/containers/cella-sbx_a/exec", http.StatusInternalServerError)
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); err == nil {
		t.Fatal("a refused exec create did not stop the exec")
	}
	f.fault("POST "+libpod+"/containers/cella-sbx_a/exec", http.StatusNotFound)
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("an exec create on a container that went away: %v", err)
	}
	// A refused start and a refused inspect each end Wait with the reason.
	f.fault("POST "+libpod+"/exec/", http.StatusInternalServerError)
	e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := read(t, e); err == nil || !strings.Contains(err.Error(), "starting the exec session") {
		t.Fatalf("a refused exec start: %v", err)
	}
	f.run = func([]string, []string, string) fakeExecResult { return fakeExecResult{} }
	e, err = d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	f.fault("GET "+libpod+"/exec/", http.StatusInternalServerError)
	if _, _, _, err := read(t, e); err == nil || !strings.Contains(err.Error(), "inspecting the exec session") {
		t.Fatalf("a refused exec inspect: %v", err)
	}
}

func TestExecTimeoutCancelAndClose(t *testing.T) {
	f := newFake(t)
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: "started", block: true}
	}
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})

	t.Run("timeout", func(t *testing.T) {
		e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"sleep"}, Timeout: 50 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := read(t, e); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait after the timeout: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		e, err := d.Exec(ctx, "sbx_a", driver.ExecRequest{Command: []string{"sleep"}})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, _, _, err := read(t, e); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait after the request was cancelled: %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		e, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"sleep"}})
		if err != nil {
			t.Fatal(err)
		}
		// The first byte is readable while the command is still running.
		first := make([]byte, 1)
		if _, err := io.ReadFull(e.Stdout(), first); err != nil {
			t.Fatalf("reading the first byte: %v", err)
		}
		early, cancelEarly := context.WithTimeout(t.Context(), 20*time.Millisecond)
		_, err = e.Wait(early)
		cancelEarly()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait before the command ended: %v", err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, err := e.Wait(bounded); err == nil {
			t.Fatal("Wait is nil after Close ended the command")
		}
		if err := e.Close(); err != nil {
			t.Fatalf("a second Close: %v", err)
		}
	})
}

func TestDemuxSplitsAndRefusesATruncatedFrame(t *testing.T) {
	var out, errOut bytes.Buffer
	body := append(frame(1, "one"), frame(2, "two")...)
	if err := demux(bytes.NewReader(body), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "one" || errOut.String() != "two" {
		t.Fatalf("demux = %q %q", out.String(), errOut.String())
	}
	if err := demux(bytes.NewReader(body[:6]), io.Discard, io.Discard); err == nil {
		t.Fatal("a stream cut inside a header read as complete")
	}
	if err := demux(bytes.NewReader(body[:10]), io.Discard, io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a stream cut inside a payload: %v", err)
	}
}
