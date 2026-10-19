// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// The variables that turn the test binary into a worker process holding one
// attached command, which a case then kills outright.
const (
	holderData    = "CELLA_LOCAL_TEST_HOLDER_DATA"
	holderHome    = "CELLA_LOCAL_TEST_HOLDER_HOME"
	holderStandIn = "CELLA_LOCAL_TEST_HOLDER_STAND_IN"
	holderSandbox = "CELLA_LOCAL_TEST_HOLDER_SANDBOX"
)

// beatCommand counts in a file in its working directory until it is killed,
// and never reads its input or writes its outputs, so closing its pipes does
// not end it.
var beatCommand = []string{"/bin/sh", "-c", `i=0; while :; do i=$((i+1)); echo $i > beat.tmp && mv beat.tmp beat; sleep 0.05; done`}

func TestMain(m *testing.M) {
	if os.Getenv(holderData) != "" {
		os.Exit(holdAnAttachedCommand())
	}
	os.Exit(m.Run())
}

// holdAnAttachedCommand is the worker process of the orphan cases: it opens
// a driver over the data directory it is given, starts the beat command with
// input that never ends in the sandbox it is given, says so, and waits to be
// killed.
func holdAnAttachedCommand() int {
	if path := os.Getenv(holderStandIn); path != "" {
		newSandbox = standIn(path)
	}
	d, err := New(Options{DataDir: os.Getenv(holderData), Home: os.Getenv(holderHome), Lookup: os.LookupEnv})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	input, _ := io.Pipe()
	if _, err = d.Exec(context.Background(), os.Getenv(holderSandbox), driver.ExecRequest{Command: beatCommand, Stdin: input}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("holding")
	time.Sleep(time.Hour)
	return 0
}

// holdingWorker starts the test binary as a worker process holding the beat
// command in the sandbox, and returns once the command runs.
func holdingWorker(t *testing.T, h host, data, standInPath, id string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), holderData+"="+data, holderHome+"="+h.home, holderStandIn+"="+standInPath, holderSandbox+"="+id)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "holding" {
		t.Fatalf("the worker process did not start its command: %q, %v", line, err)
	}
	return cmd
}

// kill ends the worker process outright, the way a crash or SIGKILL does,
// and reaps it.
func kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}

// beats reads the count the beat command last wrote.
func beats(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// waitBeating waits until the beat command counts on, and fails the case
// after ten seconds.
func waitBeating(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	first := beats(path)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if now := beats(path); now != "" && now != first {
			return
		}
	}
	t.Fatalf("%s: the command is not running", what)
}

// waitStill waits until the beat command has stopped counting, and fails the
// case after ten seconds.
func waitStill(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		before := beats(path)
		time.Sleep(300 * time.Millisecond)
		if beats(path) == before {
			return
		}
	}
	t.Fatalf("%s: the command is still running", what)
}

// orphanCases: a worker process killed outright leaves its attached command
// running, recorded in the sandbox's record. The next driver over the data
// directory ends it when it starts. A driver that started while the holder
// still ran left it to its holder, and ends it when it stops the sandbox.
func orphanCases(t *testing.T, standInPath string, open func(*testing.T, host, Options) *Driver) {
	h := newHost(t)
	data := tempDir(t)
	d := open(t, h, Options{DataDir: data})
	t.Run("the next driver ends it when it starts", func(t *testing.T) {
		sandbox(t, d, driver.CreateSpec{ID: "sbx_orphan"})
		beat := filepath.Join(d.paths("sbx_orphan").work, "beat")
		worker := holdingWorker(t, h, data, standInPath, "sbx_orphan")
		waitBeating(t, beat, "while its worker runs")
		kill(t, worker)
		waitBeating(t, beat, "after its worker was killed")
		if r := mustLoad(t, d, "sbx_orphan"); len(r.Attached) != 1 {
			t.Fatalf("the record names %d attached commands, want the one left running", len(r.Attached))
		}
		open(t, h, Options{DataDir: data})
		waitStill(t, beat, "after the next driver started")
		if r := mustLoad(t, d, "sbx_orphan"); len(r.Attached) != 0 {
			t.Errorf("the record still names %+v", r.Attached)
		}
	})
	t.Run("a driver leaves it to a holder that runs, and a stop ends it", func(t *testing.T) {
		sandbox(t, d, driver.CreateSpec{ID: "sbx_held"})
		beat := filepath.Join(d.paths("sbx_held").work, "beat")
		worker := holdingWorker(t, h, data, standInPath, "sbx_held")
		waitBeating(t, beat, "while its worker runs")
		other := open(t, h, Options{DataDir: data})
		waitBeating(t, beat, "after another driver started beside its worker")
		kill(t, worker)
		waitBeating(t, beat, "after its worker was killed")
		if err := other.Stop(context.Background(), "sbx_held"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitStill(t, beat, "after the sandbox was stopped")
		if r := mustLoad(t, d, "sbx_held"); len(r.Attached) != 0 {
			t.Errorf("the stopped sandbox's record still names %+v", r.Attached)
		}
	})
}

// TestLocalEndsAnOrphanedAttachedCommand is the orphan cases under the
// stand-in for the sandbox runtime, so it runs on every machine.
func TestLocalEndsAnOrphanedAttachedCommand(t *testing.T) {
	path := shim(t)
	orphanCases(t, path, func(t *testing.T, h host, o Options) *Driver {
		o.Lookup = os.LookupEnv
		return h.open(t, o)
	})
}

// TestLocalEndsAnOrphanedAttachedCommandUnderSRT is the orphan cases under the
// real sandbox runtime, where the command's process group is led by the
// runtime and its command runs inside Seatbelt or bubblewrap. Where no runtime
// is installed it is skipped with what to install.
func TestLocalEndsAnOrphanedAttachedCommandUnderSRT(t *testing.T) {
	orphanCases(t, "", func(t *testing.T, h host, o Options) *Driver {
		return underSRT(t, h, o)
	})
}

// TestAttachedCommandsThatCannotBeRecorded: a command whose process cannot be
// named, or whose record cannot be written, is ended and refused rather than
// left running where nothing would find it; and a record that names a group
// that has ended, or one whose leader is another process now, signals nothing.
func TestAttachedCommandsThatCannotBeRecorded(t *testing.T) {
	shim(t)
	h := newHost(t)
	ctx := context.Background()
	failing := func(after int) func() {
		previous, calls := startTime, 0
		startTime = func(ctx context.Context, pid int) (int64, error) {
			calls++
			if calls > after {
				return 0, errors.New("ps is not there")
			}
			return previous(ctx, pid)
		}
		return func() { startTime = previous }
	}

	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_unnamed"})
	restore := failing(0)
	_, err := d.Exec(ctx, "sbx_unnamed", driver.ExecRequest{Command: beatCommand, Stdin: strings.NewReader("")})
	restore()
	if err == nil || !strings.Contains(err.Error(), "naming this process") {
		t.Errorf("a command whose holder could not be named answered %v", err)
	}

	restore = failing(1)
	_, err = d.Exec(ctx, "sbx_unnamed", driver.ExecRequest{Command: beatCommand, Stdin: strings.NewReader("")})
	restore()
	if err == nil || !strings.Contains(err.Error(), "recording a command") {
		t.Errorf("a command whose group could not be named answered %v", err)
	}
	beat := filepath.Join(d.paths("sbx_unnamed").work, "beat")
	waitStill(t, beat, "after its record could not be written")

	if err = os.Chmod(d.dir("sbx_unnamed"), 0o500); err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(ctx, "sbx_unnamed", driver.ExecRequest{Command: beatCommand, Stdin: strings.NewReader("")})
	if chmodErr := os.Chmod(d.dir("sbx_unnamed"), 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "recording a command") {
		t.Errorf("a command whose record could not be written answered %v", err)
	}
	waitStill(t, beat, "after its record could not be written")

	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = other.Process.Kill()
		_ = other.Wait()
	})
	leader, err := identify(ctx, other.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]attachedStage{
		"a group that has ended":           {Name: "exec-gone", Group: process{PID: 999999, Start: 1}},
		"a leader that is another process": {Name: "exec-other", Group: process{PID: leader.PID, Start: leader.Start - 60}},
		"no process at all":                {Name: "exec-none"},
	} {
		if err := s.end(ctx); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := syscall.Kill(leader.PID, 0); err != nil {
		t.Errorf("a group whose leader is another process was signaled: %v", err)
	}
}

// TestDriverStartsOverRecordsItCannotEnd: a driver starts over a sandbox whose
// record does not read and over one naming a command it cannot end, keeps the
// second's entry for its stop, and refuses only a data directory it cannot
// read.
func TestDriverStartsOverRecordsItCannotEnd(t *testing.T) {
	shim(t)
	h := newHost(t)
	data := tempDir(t)
	d := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_unreadable"})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_ended"})
	if err := os.WriteFile(d.record("sbx_unreadable"), []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustLoad(t, d, "sbx_ended")
	r.Attached = []attachedStage{{Name: "exec-1", Group: process{PID: 999999, Start: 1}, Holder: process{PID: 999999, Start: 1}}}
	if err := d.save(r); err != nil {
		t.Fatal(err)
	}
	h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
	if r = mustLoad(t, d, "sbx_ended"); len(r.Attached) != 0 {
		t.Errorf("an entry whose group had ended was kept: %+v", r.Attached)
	}

	locked := tempDir(t)
	if err := os.MkdirAll(filepath.Join(locked, "sandboxes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(locked, "sandboxes"), 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "sandboxes"), 0o700) })
	if _, err := New(Options{DataDir: locked, Home: h.home, Lookup: os.LookupEnv}); err == nil {
		t.Error("a driver started over a data directory it cannot read")
	}
}

// TestStartTimeReadsPs: the start time of this process reads, and one of a
// process that does not exist is an error.
func TestStartTimeReadsPs(t *testing.T) {
	ctx := context.Background()
	if started, err := startTime(ctx, os.Getpid()); err != nil || started <= 0 || started > time.Now().Unix() {
		t.Errorf("this process started at %d, %v", started, err)
	}
	if _, err := startTime(ctx, 999999); err == nil {
		t.Error("a process that does not exist has a start time")
	}
}
