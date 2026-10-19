// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	driver "latere.ai/x/cella/runtime"
)

// noRuntime stands in for a machine with no sandbox runtime installed: every
// program the driver looks up is missing.
func noRuntime(t *testing.T) {
	t.Helper()
	previous := newSandbox
	newSandbox = func(c hostsandbox.Config) *hostsandbox.Driver {
		// Missing programs are srt's, so the driver uses srt on every platform.
		c.Isolation = hostsandbox.IsolationSRT
		c.Look = func(string) (string, error) { return "", exec.ErrNotFound }
		return hostsandbox.New(c)
	}
	t.Cleanup(func() { newSandbox = previous })
}

// TestStagesWithNoRuntime: with no sandbox runtime, a sandbox with no main
// command is still made, and every command is refused with the runtime named:
// a main command at create, a detached command and an attached one; a create
// that fails leaves no directory behind.
func TestStagesWithNoRuntime(t *testing.T) {
	noRuntime(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_idle"})
	if _, err := d.Exec(context.Background(), "sbx_idle", driver.ExecRequest{Command: []string{"true"}}); err == nil {
		t.Error("a detached command ran with no runtime")
	}
	if _, err := d.Exec(context.Background(), "sbx_idle", driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("x")}); err == nil {
		t.Error("an attached command ran with no runtime")
	}
	if _, err := d.Create(context.Background(), driver.CreateSpec{ID: "sbx_main", Command: []string{"sleep", "30"}}); err == nil {
		t.Error("a main command ran with no runtime")
	}
	if _, err := os.Stat(d.dir("sbx_main")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed create left its directory: %v", err)
	}
	if err := d.Stop(context.Background(), "sbx_idle"); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(context.Background(), "sbx_idle"); err != nil {
		t.Errorf("a sandbox with no main command does not start again: %v", err)
	}
}

// TestAttachedCommands: an attached command answers its own exit code, ends
// with its caller and with its timeout, and a wait shorter than it answers
// the wait's own end.
func TestAttachedCommands(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_attached", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	ctx := context.Background()

	e, err := d.Exec(ctx, "sbx_attached", driver.ExecRequest{Command: []string{"/bin/sh", "-c", "cat; pwd; exit 3"}, Stdin: strings.NewReader("in\n"), Workdir: h.project})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(e.Stdout())
	if code, err := e.Wait(ctx); err != nil || code != 3 || string(out) != "in\n"+h.project+"\n" {
		t.Errorf("the command answered %d, %v with %q", code, err, out)
	}

	caller, cancel := context.WithCancel(ctx)
	stdin, keep := io.Pipe()
	defer func() { _ = keep.Close() }()
	e, err = d.Exec(caller, "sbx_attached", driver.ExecRequest{Command: []string{"sleep", "30"}, Stdin: stdin})
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err = e.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a wait shorter than the command answered %v", err)
	}
	cancel()
	if _, err = e.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a command whose caller left answered %v", err)
	}

	e, err = d.Exec(ctx, "sbx_attached", driver.ExecRequest{Command: []string{"sleep", "30"}, Stdin: stdin, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a command past its timeout answered %v", err)
	}
	if err = e.Close(); err != nil {
		t.Errorf("closing an ended command answered %v", err)
	}
}

// TestLostStages: a main stage that left no status fails the sandbox with
// the reason Lost, a detached command that left none answers ErrLost, and a
// stage handle that does not read is an error on every call that observes it.
func TestLostStages(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	ctx := context.Background()
	sandbox(t, d, driver.CreateSpec{ID: "sbx_lost", Command: []string{"sleep", "30"}})
	r := mustLoad(t, d, "sbx_lost")
	main := *r.Main
	t.Cleanup(func() { _ = d.sandbox.Stop(context.Background(), main) })

	e, err := d.Exec(ctx, "sbx_lost", driver.ExecRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	detached, ok := e.(*detachedExec)
	if !ok {
		t.Fatalf("a command with no input is a %T, not a detached stage", e)
	}
	held := detached.handle
	detached.handle = hostsandbox.StageHandle{Driver: held.Driver, ID: "pid:999999@1:" + filepath.Join(d.paths("sbx_lost").logs, "gone.log")}
	if code, err := e.Wait(ctx); !errors.Is(err, ErrLost) || code != -1 {
		t.Errorf("a command that left no status answered %d, %v", code, err)
	}
	detached.handle = hostsandbox.StageHandle{Driver: held.Driver, ID: "not a handle"}
	if _, err = e.Wait(ctx); err == nil {
		t.Error("a handle that does not read was waited on")
	}
	detached.handle = held
	_ = e.Close()

	broken := r
	broken.Main = &hostsandbox.StageHandle{Driver: main.Driver, ID: "not a handle"}
	if err = d.save(broken); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"Inspect": func() error { _, err := d.Inspect(ctx, "sbx_lost"); return err },
		"List":    func() error { _, err := d.List(ctx, driver.Filter{}); return err },
		"Exec": func() error {
			_, err := d.Exec(ctx, "sbx_lost", driver.ExecRequest{Command: []string{"true"}})
			return err
		},
		"Start": func() error { return d.Start(ctx, "sbx_lost") },
		"Logs":  func() error { _, err := d.Logs(ctx, "sbx_lost", driver.LogsRequest{}); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s passed over a handle that does not read", name)
		}
	}

	gone := r
	gone.Main = &hostsandbox.StageHandle{Driver: main.Driver, ID: "pid:999999@1:" + filepath.Join(d.paths("sbx_lost").logs, "gone.log")}
	if err = d.save(gone); err != nil {
		t.Fatal(err)
	}
	state, err := d.Inspect(ctx, "sbx_lost")
	if err != nil || state.Phase != "Failed" || state.Reason != "Lost" || state.ExitCode != nil {
		t.Errorf("a main stage that left no status reads %+v, %v", state, err)
	}
	if err = d.save(r); err != nil {
		t.Fatal(err)
	}
}

// TestLogsAndFilesEdges: a since after the log's last write reads nothing, a
// sandbox that is not there has no logs, a directory does not open as a file
// and a file does not list as a directory, and a listing leaves out what the
// deny table names.
func TestLogsAndFilesEdges(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Deny: []string{"~/project/secret.txt"}, Lookup: os.LookupEnv})
	ctx := context.Background()
	sandbox(t, d, driver.CreateSpec{ID: "sbx_edges", Command: []string{"echo", "said"}, Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	if r, err := d.Logs(ctx, "sbx_edges", driver.LogsRequest{Since: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	} else if body, _ := io.ReadAll(r); len(body) != 0 {
		t.Errorf("a since after the last write read %q", body)
	}
	if _, err := d.Logs(ctx, "sbx_absent", driver.LogsRequest{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("the logs of nothing answered %v", err)
	}
	if _, _, err := d.Open(ctx, "sbx_edges", h.project); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a directory opened as a file: %v", err)
	}
	if _, _, err := d.Open(ctx, "sbx_edges", filepath.Join(h.project, "absent")); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a missing file answered %v", err)
	}
	if _, err := d.ReadDir(ctx, "sbx_edges", filepath.Join(h.project, "notes.txt")); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a file listed as a directory: %v", err)
	}
	if _, err := d.ReadDir(ctx, "sbx_edges", filepath.Join(h.project, "absent")); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a missing directory answered %v", err)
	}
	entries, err := d.ReadDir(ctx, "sbx_edges", h.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "secret.txt" {
			t.Error("the listing names a file on the deny table")
		}
	}
}

// TestTheTokenIsReplacedWhole: a token whose directory is gone, or whose
// path is taken by a directory, is not written, and no partial file is left.
func TestTheTokenIsReplacedWhole(t *testing.T) {
	dir := tempDir(t)
	if err := writeToken(filepath.Join(dir, "absent", "token"), []byte("t")); err == nil {
		t.Error("a token was written into a directory that is not there")
	}
	taken := filepath.Join(dir, "token")
	if err := os.MkdirAll(filepath.Join(taken, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeToken(taken, []byte("t")); err == nil {
		t.Error("a token replaced a directory")
	}
	if _, err := os.Stat(taken + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a partial token was left: %v", err)
	}
}

// TestReadPathsLinkedIntoTheHome: a file directly in the home is linked into
// the sandbox's home, and a directory or a path not there yet is not.
func TestReadPathsLinkedIntoTheHome(t *testing.T) {
	shim(t)
	h := newHost(t)
	gitconfig, config, absent := filepath.Join(h.home, ".gitconfig"), filepath.Join(h.home, ".config"), filepath.Join(h.home, ".editorconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	d := h.open(t, Options{ReadPaths: []string{gitconfig, config, absent}, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_links"})
	home := d.paths("sbx_links").home
	if _, err := os.Readlink(filepath.Join(home, ".gitconfig")); err != nil {
		t.Errorf("the file is not linked: %v", err)
	}
	for _, name := range []string{".config", ".editorconfig"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was linked: %v", name, err)
		}
	}
}
