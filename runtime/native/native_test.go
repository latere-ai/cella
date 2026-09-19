// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

func fresh(t *testing.T) (*Driver, string) {
	t.Helper()
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, root
}
func create(t *testing.T, d *Driver, id string) {
	t.Helper()
	_, err := d.Create(context.Background(), driver.CreateSpec{ID: id, Name: "test", Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func run(t *testing.T, d *Driver, id string, req driver.ExecRequest) (string, string, int, error) {
	t.Helper()
	e, err := d.Exec(context.Background(), id, req)
	if err != nil {
		return "", "", 0, err
	}
	defer e.Close()
	var out, stderr []byte
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); out, _ = io.ReadAll(e.Stdout()) }()
	go func() { defer wg.Done(); stderr, _ = io.ReadAll(e.Stderr()) }()
	wg.Wait()
	code, err := e.Wait(context.Background())
	return string(out), string(stderr), code, err
}
func archive(t *testing.T, name, body string, typ byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	size := int64(len(body))
	if typ != tar.TypeReg {
		size = 0
	}
	check(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0750, Size: size, Typeflag: typ, Linkname: "../../escape"}))
	if size > 0 {
		_, err := tw.Write([]byte(body))
		check(t, err)
	}
	check(t, tw.Close())
	return b.Bytes()
}

func TestNativeEndToEnd(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	if d.Name() != "native" || d.Isolation() != driver.IsolationNone || !d.Capabilities().Files || !d.Capabilities().Attach || d.Capabilities().Detach {
		t.Fatal("capabilities")
	}
	check(t, d.Preflight(ctx))
	_, err := d.Create(ctx, driver.CreateSpec{ID: "sbx_one", Name: "agent", Owner: "alice", Env: map[string]string{"TASK": "one"}, Labels: map[string]string{"purpose": "test"}, Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute, AutoDelete: time.Minute}})
	check(t, err)
	before, err := d.Inspect(ctx, "sbx_one")
	check(t, err)
	if before.Name != "agent" || before.Owner != "alice" || before.ExpiresAt.Sub(before.CreatedAt) != time.Hour {
		t.Fatal(before)
	}
	check(t, d.ImportTar(ctx, "sbx_one", "/workspace", bytes.NewReader(archive(t, "input/data.txt", "hello", tar.TypeReg))))
	out, stderr, code, err := run(t, d, "sbx_one", driver.ExecRequest{Command: []string{"sh", "-c", "cat input/data.txt; printf '%s' \"$TASK\"; printf problem >&2; exit 7"}})
	check(t, err)
	if out != "helloone" || stderr != "problem" || code != 7 {
		t.Fatalf("%q %q %d", out, stderr, code)
	}
	check(t, d.Stop(ctx, "sbx_one"))
	stopped, err := d.Inspect(ctx, "sbx_one")
	check(t, err)
	check(t, d.Stop(ctx, "sbx_one"))
	again, _ := d.Inspect(ctx, "sbx_one")
	if stopped.StoppedAt.IsZero() || stopped.StoppedAt != again.StoppedAt {
		t.Fatal("stop not idempotent")
	}
	var export bytes.Buffer
	check(t, d.ExportTar(ctx, "sbx_one", []string{"/workspace/input"}, &export))
	tr := tar.NewReader(&export)
	_, err = tr.Next()
	check(t, err)
	h, err := tr.Next()
	check(t, err)
	body, err := io.ReadAll(tr)
	check(t, err)
	if h.Name != "input/data.txt" || h.Mode != 0750 || string(body) != "hello" {
		t.Fatal(h, string(body))
	}
	reopened, err := New(root)
	check(t, err)
	loaded, err := reopened.Inspect(ctx, "sbx_one")
	check(t, err)
	if loaded.Owner != before.Owner || loaded.Phase != driver.Stopped || loaded.CreatedAt != before.CreatedAt {
		t.Fatal(loaded)
	}
	check(t, reopened.Start(ctx, "sbx_one"))
	started, _ := reopened.Inspect(ctx, "sbx_one")
	check(t, reopened.Start(ctx, "sbx_one"))
	same, _ := reopened.Inspect(ctx, "sbx_one")
	if started.StartedAt != same.StartedAt || !started.StoppedAt.IsZero() {
		t.Fatal("start not idempotent")
	}
	check(t, reopened.Touch(ctx, "sbx_one"))
	labels := map[string]string{"changed": "yes"}
	env := map[string]string{"TASK": "two"}
	check(t, reopened.Update(ctx, "sbx_one", driver.Change{Labels: &labels, Env: &env, Lifecycle: &driver.Lifecycle{TTL: 2 * time.Hour}}))
	updated, _ := reopened.Inspect(ctx, "sbx_one")
	if updated.Labels["changed"] != "yes" || updated.ExpiresAt.Sub(updated.CreatedAt) != 2*time.Hour {
		t.Fatal(updated)
	}
	out, _, _, err = run(t, reopened, "sbx_one", driver.ExecRequest{Command: []string{"sh", "-c", "printf '%s' \"$TASK\""}})
	check(t, err)
	if out != "two" {
		t.Fatal(out)
	}
	check(t, reopened.Delete(ctx, "sbx_one"))
	_, err = reopened.Inspect(ctx, "sbx_one")
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
}
func TestInvalidAndUnsupported(t *testing.T) {
	ctx := context.Background()
	d, _ := fresh(t)
	create(t, d, "one")
	for _, id := range []string{"", "..", "../other", "/absolute", "bad.id"} {
		if _, err := d.Create(ctx, driver.CreateSpec{ID: id}); !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(id, err)
		}
		if _, err := d.Inspect(ctx, id); !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(err)
		}
	}
	for _, s := range []driver.CreateSpec{{ID: "one"}, {ID: "two", Image: "image"}, {ID: "two", Args: []string{"arg"}}, {ID: "two", Workdir: "/tmp"}, {ID: "two", Lifecycle: driver.Lifecycle{TTL: -1}}} {
		if _, err := d.Create(ctx, s); err == nil {
			t.Fatal(s)
		}
	}
	for _, req := range []driver.ExecRequest{{}, {Command: []string{"true"}, Timeout: -1}, {Command: []string{"true"}, Workdir: "/outside"}, {Command: []string{"true"}, Workdir: "/workspace/missing"}, {Command: []string{"/no-such-command"}}} {
		if _, err := d.Exec(ctx, "one", req); err == nil {
			t.Fatal(req)
		}
	}
	_, err := d.Exec(ctx, "absent", driver.ExecRequest{Command: []string{"true"}})
	if !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	check(t, d.Stop(ctx, "one"))
	_, err = d.Exec(ctx, "one", driver.ExecRequest{Command: []string{"true"}})
	if !errors.Is(err, driver.ErrNotRunning) {
		t.Fatal(err)
	}
	logs, err := d.Logs(ctx, "one", driver.LogsRequest{})
	check(t, err)
	check(t, logs.Close())
	if err = d.Update(ctx, "one", driver.Change{Lifecycle: &driver.Lifecycle{AutoStop: -1}}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal(err)
	}
	check(t, d.Update(ctx, "one", driver.Change{Lifecycle: &driver.Lifecycle{}}))
	for _, fn := range []func() error{func() error { return d.Start(ctx, "absent") }, func() error { return d.Stop(ctx, "absent") }, func() error { return d.Delete(ctx, "absent") }} {
		if !errors.Is(fn(), driver.ErrNotFound) {
			t.Fatal("expected not found")
		}
	}
}
func TestListAndCancellation(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	create(t, d, "one")
	create(t, d, "two")
	check(t, d.Stop(ctx, "two"))
	check(t, os.WriteFile(filepath.Join(root, "unrelated"), nil, 0600))
	check(t, os.Mkdir(filepath.Join(root, "bad.name"), 0700))
	for _, f := range []driver.Filter{{Owner: "alice", Phase: driver.Running}, {IDs: []string{"two"}}, {Owner: "bob"}, {IDs: []string{"absent"}}} {
		states, err := d.List(ctx, f)
		check(t, err)
		want := 1
		if f.Owner == "bob" || len(f.IDs) > 0 && f.IDs[0] == "absent" {
			want = 0
		}
		if len(states) != want {
			t.Fatal(states)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, fn := range []func() error{func() error { return d.Ready(cancelled) }, func() error { _, e := d.Create(cancelled, driver.CreateSpec{}); return e }, func() error { _, e := d.Inspect(cancelled, "one"); return e }, func() error { _, e := d.List(cancelled, driver.Filter{}); return e }, func() error { return d.Touch(cancelled, "one") }, func() error { return d.Delete(cancelled, "one") }, func() error { _, e := d.Exec(cancelled, "one", driver.ExecRequest{}); return e }, func() error { return d.ExportTar(cancelled, "one", nil, io.Discard) }, func() error { return d.ImportTar(cancelled, "one", "/workspace", strings.NewReader("")) }} {
		if !errors.Is(fn(), context.Canceled) {
			t.Fatal("missing cancellation")
		}
	}
}
func TestExecutionTimeoutAndStop(t *testing.T) {
	d, _ := fresh(t)
	create(t, d, "one")
	out, _, code, err := run(t, d, "one", driver.ExecRequest{Command: []string{"sh", "-c", "printf '%s' \"$VALUE\""}, Env: map[string]string{"VALUE": "overridden"}})
	check(t, err)
	if out != "overridden" || code != 0 {
		t.Fatal(out, code)
	}
	t.Setenv("SHOULD_NOT_LEAK", "secret")
	out, _, _, err = run(t, d, "one", driver.ExecRequest{Command: []string{"sh", "-c", "printf '%s' \"${SHOULD_NOT_LEAK-unset}\""}})
	check(t, err)
	if out != "unset" {
		t.Fatal("host credential leaked")
	}
	_, _, _, err = run(t, d, "one", driver.ExecRequest{Command: []string{"sh", "-c", "sleep 60 & wait"}, Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	e, err := d.Exec(context.Background(), "one", driver.ExecRequest{Command: []string{"sh", "-c", "sleep 60 & wait"}})
	check(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = e.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	check(t, d.Stop(context.Background(), "one"))
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = e.Wait(deadline)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	check(t, e.Close())
	check(t, d.Start(context.Background(), "one"))
	e, err = d.Exec(context.Background(), "one", driver.ExecRequest{Command: []string{"sleep", "60"}})
	check(t, err)
	check(t, d.Close())
	_, err = e.Wait(deadline)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A stopped/deleted workspace must not be reported complete while a managed
// process can still mutate it. A controlled reap makes this regression repeatable.
func TestLifecycleWaitsForReap(t *testing.T) {
	for _, operation := range []string{"stop", "delete", "close"} {
		t.Run(operation, func(t *testing.T) {
			d, _ := fresh(t)
			create(t, d, "one")
			cancelled := make(chan struct{})
			done := make(chan struct{})
			out, _ := io.Pipe()
			stderr, _ := io.Pipe()
			e := &execution{stdout: out, stderr: stderr, cancel: func() { close(cancelled) }, done: done}
			d.active["one"] = map[*execution]struct{}{e: {}}
			returned := make(chan error, 1)
			go func() {
				switch operation {
				case "stop":
					returned <- d.Stop(context.Background(), "one")
				case "delete":
					returned <- d.Delete(context.Background(), "one")
				case "close":
					returned <- d.Close()
				}
			}()
			<-cancelled
			select {
			case err := <-returned:
				close(done)
				t.Fatalf("%s returned before process reap: %v", operation, err)
			case <-time.After(20 * time.Millisecond):
			}
			close(done)
			check(t, <-returned)
		})
	}
}
