// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

func waitState(t *testing.T, d *Driver, id, phase string) driver.State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, err := d.Inspect(context.Background(), id)
		check(t, err)
		if state.Phase == phase {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %s", id, phase)
	return driver.State{}
}
func readLogs(t *testing.T, d *Driver, id string, req driver.LogsRequest) string {
	t.Helper()
	r, err := d.Logs(context.Background(), id, req)
	check(t, err)
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	check(t, err)
	return string(b)
}
func TestMainProcessEndToEnd(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	_, err := d.Create(ctx, driver.CreateSpec{ID: "app", Command: []string{"sh", "-c"}, Args: []string{"printf 'ready\\n'; sleep 60"}})
	check(t, err)
	reader, err := d.Logs(ctx, "app", driver.LogsRequest{Follow: true})
	check(t, err)
	got := make([]byte, 6)
	_, err = io.ReadFull(reader, got)
	check(t, err)
	if string(got) != "ready\n" {
		t.Fatal(string(got))
	}
	check(t, d.Stop(ctx, "app"))
	rest, err := io.ReadAll(reader)
	check(t, err)
	check(t, reader.Close())
	if len(rest) != 0 {
		t.Fatal(string(rest))
	}
	waitState(t, d, "app", driver.Stopped)
	check(t, d.Start(ctx, "app"))
	check(t, d.Start(ctx, "app"))
	check(t, d.Close())
	reopened, err := New(root)
	check(t, err)
	state, err := reopened.Inspect(ctx, "app")
	check(t, err)
	if state.Phase != driver.Stopped {
		t.Fatal(state)
	}
	check(t, reopened.Start(ctx, "app"))
	check(t, reopened.Delete(ctx, "app"))
	check(t, reopened.Close())
	if _, err = os.Stat(filepath.Join(root, "app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func TestMainExitStatusAndLogs(t *testing.T) {
	ctx := context.Background()
	d, _ := fresh(t)
	for _, tc := range []struct {
		id, script, phase string
		code              int
	}{{"ok", "printf 'first\\nsecond\\nthird\\n'; printf error >&2", driver.Stopped, 0}, {"failed", "printf failure; exit 9", "Failed", 9}} {
		_, err := d.Create(ctx, driver.CreateSpec{ID: tc.id, Command: []string{"sh", "-c", tc.script}})
		check(t, err)
		state := waitState(t, d, tc.id, tc.phase)
		if state.Reason != "Exited" || state.ExitCode == nil || *state.ExitCode != tc.code {
			t.Fatal(state)
		}
		logs := readLogs(t, d, tc.id, driver.LogsRequest{})
		if tc.id == "failed" && logs != "failure" {
			t.Fatal(logs)
		}
	}
	if got := readLogs(t, d, "ok", driver.LogsRequest{Since: time.Now().Add(time.Hour)}); got != "" {
		t.Fatal(got)
	}
	if got := readLogs(t, d, "failed", driver.LogsRequest{TailLines: 1}); got != "failure" {
		t.Fatal(got)
	}
	if got := readLogs(t, d, "failed", driver.LogsRequest{Follow: true}); got != "failure" {
		t.Fatal(got)
	}
	_, err := d.Create(ctx, driver.CreateSpec{ID: "bad", Command: []string{"/no-such-executable"}})
	if err == nil {
		t.Fatal("accepted unavailable executable")
	}
	if _, err = d.Inspect(ctx, "bad"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
}
func TestLostMainCannotRestart(t *testing.T) {
	d, root := fresh(t)
	create(t, d, "one")
	r, err := d.load("one")
	check(t, err)
	r.Command = []string{"sh"}
	r.PID = 999999
	check(t, d.save("one", r))
	reopened, err := New(root)
	check(t, err)
	state, err := reopened.Inspect(context.Background(), "one")
	check(t, err)
	if state.Phase != "Lost" || state.Reason != "ProcessUnrecoverable" {
		t.Fatal(state)
	}
	if err = reopened.Start(context.Background(), "one"); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatal(err)
	}
	if err = reopened.Stop(context.Background(), "one"); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatal(err)
	}
	check(t, reopened.Delete(context.Background(), "one"))
	check(t, reopened.Close())
}
func TestCompletedExecTerminatesDetachedChildren(t *testing.T) {
	d, root := fresh(t)
	create(t, d, "one")
	// The descendant detaches its IO, so Wait can finish before it writes a marker.
	_, _, _, err := run(t, d, "one", driver.ExecRequest{Command: []string{"sh", "-c", "(sleep 0.2; printf escaped > escaped) </dev/null >/dev/null 2>&1 &"}})
	check(t, err)
	time.Sleep(300 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(root, "one", "workspace", "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed execution left a descendant running", err)
	}
}
func TestLogFiltersAndErrors(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	create(t, d, "one")
	if _, err := d.Logs(ctx, "missing", driver.LogsRequest{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := d.Logs(ctx, "one", driver.LogsRequest{TailLines: -1}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.Logs(cancelled, "one", driver.LogsRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "one", "logs.jsonl")
	f, err := os.Create(logPath)
	check(t, err)
	encoder := json.NewEncoder(f)
	before := time.Now().Add(-time.Minute)
	after := time.Now()
	for _, chunk := range []logChunk{{At: before, Data: []byte("old\n")}, {At: after, Data: []byte("one\ntwo\n")}, {At: after, Data: []byte("three\n")}} {
		check(t, encoder.Encode(chunk))
	}
	check(t, f.Close())
	if got := readLogs(t, d, "one", driver.LogsRequest{Since: after, TailLines: 2}); got != "two\nthree\n" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		in, want string
		n        int
	}{{"a\nb\nc\n", "b\nc\n", 2}, {"a\nb\nc", "c", 1}, {"abc", "abc", 1}, {"", "", 1}} {
		if got := string(lastLines([]byte(tc.in), tc.n)); got != tc.want {
			t.Fatalf("%q", got)
		}
	}
	check(t, os.WriteFile(logPath, []byte("broken\n"), 0600))
	reader, err := d.Logs(ctx, "one", driver.LogsRequest{})
	check(t, err)
	if _, err = io.ReadAll(reader); err == nil {
		t.Fatal("accepted corrupt log")
	}
	check(t, reader.Close())
	check(t, os.WriteFile(logPath, []byte("{partial"), 0600))
	reader, err = d.Logs(ctx, "one", driver.LogsRequest{})
	check(t, err)
	_, err = io.ReadAll(reader)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	check(t, reader.Close())
	check(t, os.Remove(logPath))
	check(t, os.Mkdir(logPath, 0700))
	reader, err = d.Logs(ctx, "one", driver.LogsRequest{})
	if err == nil {
		_, err = io.ReadAll(reader)
		_ = reader.Close()
	}
	if err == nil {
		t.Fatal("accepted log directory")
	}
}
func TestFollowCancellationAndLogFailures(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	_, err := d.Create(ctx, driver.CreateSpec{ID: "one", Command: []string{"sleep", "60"}})
	check(t, err)
	follow, cancel := context.WithCancel(ctx)
	reader, err := d.Logs(follow, "one", driver.LogsRequest{Follow: true})
	check(t, err)
	cancel()
	if _, err = io.ReadAll(reader); err == nil {
		t.Fatal("ignored cancellation")
	}
	check(t, reader.Close())
	reader, err = d.Logs(ctx, "one", driver.LogsRequest{Follow: true})
	check(t, err)
	check(t, reader.Close())
	check(t, d.Stop(ctx, "one"))
	// A log append failure must stop the process rather than block its output.
	f, err := os.OpenFile(filepath.Join(root, "one", "logs.jsonl"), os.O_RDONLY, 0)
	check(t, err)
	writer := &logWriter{file: f}
	if _, err = writer.Write([]byte("failure")); err == nil {
		t.Fatal("ignored write error")
	}
	check(t, f.Close())
	check(t, os.Remove(filepath.Join(root, "one", "logs.jsonl")))
	check(t, os.Mkdir(filepath.Join(root, "one", "logs.jsonl"), 0700))
	if err = d.Start(ctx, "one"); err == nil {
		t.Fatal("accepted unwritable logs")
	}
	// Exercise the reader's write failure without relying on pipe scheduling.
	data, _ := json.Marshal(logChunk{At: time.Now(), Data: []byte("line\n")})
	path := filepath.Join(t.TempDir(), "log")
	check(t, os.WriteFile(path, append(data, '\n'), 0600))
	f, err = os.Open(path)
	check(t, err)
	defer func() { _ = f.Close() }()
	if err = streamLogs(ctx, f, brokenWriter{}, driver.LogsRequest{}, int64(len(data)+1), nil); err == nil {
		t.Fatal("ignored output error")
	}
	if err = streamLogs(ctx, f, brokenWriter{}, driver.LogsRequest{TailLines: 1}, int64(len(data)+1), nil); err == nil {
		t.Fatal("ignored tail output error")
	}
	if err = streamLogs(follow, f, io.Discard, driver.LogsRequest{}, int64(len(data)+1), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	check(t, f.Close())
	if err = streamLogs(ctx, f, &bytes.Buffer{}, driver.LogsRequest{Follow: true}, 0, &mainProcess{done: make(chan struct{})}); err == nil {
		t.Fatal("ignored file stat error")
	}
}
func TestKilledMainIsFailed(t *testing.T) {
	d, _ := fresh(t)
	_, err := d.Create(context.Background(), driver.CreateSpec{ID: "one", Command: []string{"sleep", "60"}})
	check(t, err)
	d.mu.Lock()
	pid := d.mains["one"].exec.pid
	d.mu.Unlock()
	check(t, syscall.Kill(pid, syscall.SIGKILL))
	state := waitState(t, d, "one", "Failed")
	if state.Reason != "Exited" || state.ExitCode == nil {
		t.Fatal(state)
	}
	if got := readLogs(t, d, "one", driver.LogsRequest{}); strings.TrimSpace(got) != "" {
		t.Fatal(got)
	}
}

func TestFollowRejectsFinalPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs")
	check(t, os.WriteFile(path, []byte("{partial"), 0600))
	f, err := os.Open(path)
	check(t, err)
	defer func() { _ = f.Close() }()
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = streamLogs(ctx, f, io.Discard, driver.LogsRequest{Follow: true}, 0, &mainProcess{done: done})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("incomplete final record must fail without spinning: %v", err)
	}
}

func TestCloseReportsMainStatePersistenceFailure(t *testing.T) {
	d, root := fresh(t)
	_, err := d.Create(context.Background(), driver.CreateSpec{ID: "one", Command: []string{"sleep", "60"}})
	check(t, err)
	blocker := filepath.Join(root, "one", "record.json.tmp")
	check(t, os.Mkdir(blocker, 0700))
	if err = d.Close(); err == nil {
		t.Fatal("close reported persisted shutdown while metadata write failed")
	}
	check(t, os.Remove(blocker))
}

func TestMainOutlivesCreateRequest(t *testing.T) {
	d, _ := fresh(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := d.Create(ctx, driver.CreateSpec{ID: "one", Command: []string{"sleep", "60"}})
	check(t, err)
	cancel()
	state, err := d.Inspect(context.Background(), "one")
	check(t, err)
	if state.Phase != driver.Running {
		t.Fatal(state)
	}
	check(t, d.Stop(context.Background(), "one"))
}

func TestLogTailRejectsOversizedUnterminatedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f, err := os.Create(path)
	check(t, err)
	encoder := json.NewEncoder(f)
	for range 66 {
		check(t, encoder.Encode(logChunk{At: time.Now(), Data: bytes.Repeat([]byte("x"), 16<<10)}))
	}
	check(t, f.Close())
	f, err = os.Open(path)
	check(t, err)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	check(t, err)
	var output bytes.Buffer
	err = streamLogs(context.Background(), f, &output, driver.LogsRequest{TailLines: 1}, info.Size(), nil)
	if !errors.Is(err, driver.ErrInvalid) || output.Len() != 0 {
		t.Fatalf("oversized tail must fail before buffering/emitting it: bytes=%d error=%v", output.Len(), err)
	}
}
