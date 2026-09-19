// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// write records what the container's main process wrote.
func (f *fake) write(t *testing.T, id string, lines ...fakeLine) {
	t.Helper()
	c := f.container(t, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	c.logs = append(c.logs, lines...)
}

func snapshot(t *testing.T, d *Driver, id string, req driver.LogsRequest) string {
	t.Helper()
	r, err := d.Logs(t.Context(), id, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLogsSnapshotNarrowsBySinceAndTail(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Command: []string{"sh", "-c", "true"}})
	base := time.Now().Add(-time.Hour)
	f.write(t, "sbx_a", line(base, 1, "one\n"), line(base.Add(time.Minute), 2, "two\n"), line(base.Add(2*time.Minute), 1, "three\n"))
	if got := snapshot(t, d, "sbx_a", driver.LogsRequest{}); got != "one\ntwo\nthree\n" {
		t.Fatalf("the whole snapshot = %q", got)
	}
	if got := snapshot(t, d, "sbx_a", driver.LogsRequest{TailLines: 1}); got != "three\n" {
		t.Fatalf("TailLines 1 = %q", got)
	}
	if got := snapshot(t, d, "sbx_a", driver.LogsRequest{Since: base.Add(90 * time.Second)}); got != "three\n" {
		t.Fatalf("Since = %q", got)
	}
	if got := snapshot(t, d, "sbx_a", driver.LogsRequest{Since: time.Now().Add(time.Hour)}); got != "" {
		t.Fatalf("Since in the future = %q", got)
	}
	if _, err := d.Logs(t.Context(), "sbx_a", driver.LogsRequest{TailLines: -1}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a negative tail: %v", err)
	}
}

func TestLogsFollowEndsWithTheProcess(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Command: []string{"sh", "-c", "true"}})
	f.write(t, "sbx_a", line(time.Now(), 1, "one\n"))
	r, err := d.Logs(t.Context(), "sbx_a", driver.LogsRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, len("one\n"))
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("reading what was written before Logs opened: %v", err)
	}
	f.write(t, "sbx_a", line(time.Now(), 1, "two\n"))
	f.exit(t, "sbx_a", 4)
	tail, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading until the process ended: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if string(head)+string(tail) != "one\ntwo\n" {
		t.Fatalf("followed %q%q", head, tail)
	}
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "Failed" || state.ExitCode == nil || *state.ExitCode != 4 {
		t.Fatalf("after the process ended: %+v", state)
	}
}

func TestLogsCloseEndsAFollowNobodyReads(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Command: []string{"sh", "-c", "true"}})
	f.write(t, "sbx_a", line(time.Now(), 1, "one\n"))
	r, err := d.Logs(t.Context(), "sbx_a", driver.LogsRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a closed stream still reads")
	}
}

func TestLogsReportsEngineFaults(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	f.fault("GET "+libpod+"/containers/cella-sbx_a/logs", http.StatusInternalServerError)
	if _, err := d.Logs(t.Context(), "sbx_a", driver.LogsRequest{}); err == nil {
		t.Fatal("a refused log request read as success")
	}
	f.fault("GET "+libpod+"/containers/cella-sbx_a/logs", http.StatusNotFound)
	if _, err := d.Logs(t.Context(), "sbx_a", driver.LogsRequest{}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("logs of a container that went away: %v", err)
	}
}
