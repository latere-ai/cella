// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// attached creates a sandbox and opens one session in it.
func attached(t *testing.T, req driver.AttachRequest) (*Driver, driver.Session) {
	t.Helper()
	d, _ := fresh(t)
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_att", Name: "att", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	s, err := d.Attach(t.Context(), "sbx_att", req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return d, s
}

// tail reads a session in the background so a test waits for a marker with a
// bound rather than blocking in Read.
type tail struct {
	mu   sync.Mutex
	buf  []byte
	done chan struct{}
}

func follow(r io.Reader) *tail {
	tl := &tail{done: make(chan struct{})}
	go func() {
		defer close(tl.done)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			tl.mu.Lock()
			tl.buf = append(tl.buf, b[:n]...)
			tl.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return tl
}
func (tl *tail) text() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return string(tl.buf)
}
func (tl *tail) await(t *testing.T, marker string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(tl.text(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("the session never wrote %q; it wrote %q", marker, tl.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return tl.text()
}

// alive reports whether the process is still there. Signal 0 checks for it
// without delivering anything.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// gone waits for a process to leave the table, which a kill does not do
// synchronously.
func gone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pidFrom reads the decimal after marker out of what the session wrote.
func pidFrom(t *testing.T, text, marker string) int {
	t.Helper()
	_, rest, ok := strings.Cut(text, marker)
	if !ok {
		t.Fatalf("%q does not carry %q", text, marker)
	}
	digits := strings.TrimSpace(strings.Split(strings.TrimSpace(rest), "\n")[0])
	pid, err := strconv.Atoi(digits)
	if err != nil {
		t.Fatalf("%q after %q is not a pid: %v", digits, marker, err)
	}
	return pid
}

// TestAttachCloseKillsTheProcessGroup pins what Close owns: the command the
// session runs and every descendant it left behind. The command is not an
// interactive shell, so the background child has no job control to put it in a
// group of its own and stays in the session's.
func TestAttachCloseKillsTheProcessGroup(t *testing.T) {
	_, s := attached(t, driver.AttachRequest{
		Command: []string{"sh", "-c", `sleep 30 & printf 'SHELL:%s\nCHILD:%s\n' "$$" "$!"; wait`},
		Cols:    80, Rows: 24,
	})
	tl := follow(s)
	text := tl.await(t, "CHILD:")
	shell, child := pidFrom(t, text, "SHELL:"), pidFrom(t, text, "CHILD:")
	if !alive(shell) || !alive(child) {
		t.Fatalf("the session's processes are not running: shell %d child %d", shell, child)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	gone(t, shell)
	gone(t, child)
}

// TestAttachResizeReachesTheProcess proves the window is not only recorded:
// the process inside is signalled and reads the new size.
func TestAttachResizeReachesTheProcess(t *testing.T) {
	_, s := attached(t, driver.AttachRequest{
		Command: []string{"sh", "-c", `trap 'stty size' WINCH; printf 'READY\n'; while true; do sleep 0.1; done`},
		Cols:    80, Rows: 24,
	})
	tl := follow(s)
	tl.await(t, "READY")
	if err := s.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	tl.await(t, "43 132")
}

// TestAttachWindowIsValidated keeps a window the kernel cannot hold out of the
// ioctl, on the request and on a later resize.
func TestAttachWindowIsValidated(t *testing.T) {
	d, s := attached(t, driver.AttachRequest{Cols: 80, Rows: 24})
	for _, size := range [][2]int{{0, 24}, {80, 0}, {-1, 24}, {1 << 20, 24}} {
		if err := s.Resize(size[0], size[1]); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Resize%v: %v, want ErrInvalid", size, err)
		}
	}
	if _, err := d.Attach(t.Context(), "sbx_att", driver.AttachRequest{Cols: -1}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("Attach with a negative window: %v, want ErrInvalid", err)
	}
	if _, err := d.Attach(t.Context(), "sbx_att", driver.AttachRequest{Command: []string{" "}}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("Attach with a blank command: %v, want ErrInvalid", err)
	}
	if err := setWinsize(os.Stdin, 1<<20, 24); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("setWinsize past the field width: %v, want ErrInvalid", err)
	}
}

// TestAttachRefusals covers the sandbox a session cannot be opened in.
func TestAttachRefusals(t *testing.T) {
	d, _ := fresh(t)
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_ref", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Attach(t.Context(), "sbx_absent", driver.AttachRequest{}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Attach to an unknown sandbox: %v, want ErrNotFound", err)
	}
	if err := d.Stop(t.Context(), "sbx_ref"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Attach(t.Context(), "sbx_ref", driver.AttachRequest{}); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("Attach to a stopped sandbox: %v, want ErrNotRunning", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Attach(ctx, "sbx_ref", driver.AttachRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Attach with a cancelled context: %v, want context.Canceled", err)
	}
	if _, err := d.Attach(t.Context(), "sbx_ref", driver.AttachRequest{Command: []string{"/no-such-command"}}); err == nil {
		t.Error("Attach to a command that does not exist returned no error")
	}
}

// TestStopEndsEverySession pins that a session is one of the sandbox's
// executions: stopping the sandbox ends it as it ends a running command.
func TestStopEndsEverySession(t *testing.T) {
	d, s := attached(t, driver.AttachRequest{Cols: 80, Rows: 24})
	tl := follow(s)
	if _, err := s.Write([]byte("printf 'UP%s\\n' HERE\n")); err != nil {
		t.Fatal(err)
	}
	tl.await(t, "UPHERE")
	if err := d.Stop(t.Context(), "sbx_att"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tl.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session stayed open after the sandbox stopped")
	}
}

// TestExecStdinEndsWithTheReader proves the stdin copy closes the pipe when
// the reader ends, so a command that reads to the end of its input finishes.
func TestExecStdinEndsWithTheReader(t *testing.T) {
	d, _ := fresh(t)
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_in", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Exec(t.Context(), "sbx_in", driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("one\ntwo\n")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	out, err := io.ReadAll(e.Stdout())
	if err != nil || string(out) != "one\ntwo\n" {
		t.Fatalf("stdout %q: %v", out, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if code, err := e.Wait(ctx); err != nil || code != 0 {
		t.Fatalf("Wait: %d %v", code, err)
	}
}

// TestAResizeAfterTheSessionEndedIsNotRunning: a window that arrives once the
// session has closed its terminal answers ErrNotRunning, which is what the
// session is, and never reaches a descriptor the close has taken away.
func TestAResizeAfterTheSessionEndedIsNotRunning(t *testing.T) {
	_, s := attached(t, driver.AttachRequest{Cols: 80, Rows: 24})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(100, 30); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a resize after the session ended answered %v, want ErrNotRunning", err)
	}
}
