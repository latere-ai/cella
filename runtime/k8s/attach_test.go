// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/remotecommand"

	driver "latere.ai/x/cella/runtime"
)

// terminal is the double of a shell under a TTY: every line typed is written
// back, "exit N" ends it with N, and every window the executor sends is
// reported on sizes as "WxH". It is what the recorder runs for the attach
// tests, so a case reads the session the way a person at a shell would.
func terminal(sizes chan<- string) func(context.Context, execCall, io.Reader, io.Writer, io.Writer) error {
	return func(ctx context.Context, c execCall, stdin io.Reader, stdout, stderr io.Writer) error {
		if !c.tty || !c.stdin || stderr != nil || c.window == nil {
			return fmt.Errorf("the exec asked for tty %v, stdin %v, a stderr %v and a window %v", c.tty, c.stdin, stderr != nil, c.window != nil)
		}
		go func() {
			for size := c.window.Next(); size != nil; size = c.window.Next() {
				if sizes != nil {
					sizes <- fmt.Sprintf("%dx%d", size.Width, size.Height)
				}
			}
			if sizes != nil {
				close(sizes)
			}
		}()
		// The shell reads its input beside the context, as the executor
		// does: a caller that goes away ends the exec whether or not a line
		// is being read.
		ended := make(chan error, 1)
		go func() {
			lines := bufio.NewScanner(stdin)
			for lines.Scan() {
				if code, ok := strings.CutPrefix(lines.Text(), "exit "); ok {
					var n int
					if _, err := fmt.Sscan(code, &n); err != nil {
						ended <- err
					} else if n == 0 {
						ended <- nil
					} else {
						ended <- exited(n)
					}
					return
				}
				if _, err := io.WriteString(stdout, lines.Text()+"\r\n"); err != nil {
					ended <- err
					return
				}
			}
			// A shell whose input ends exits.
			ended <- lines.Err()
		}()
		select {
		case err := <-ended:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// readUntil reads the session until marker appears, bounded, and returns what
// it read.
func readUntil(t *testing.T, r io.Reader, marker string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 256)
		for !strings.Contains(seen.String(), marker) {
			n, err := r.Read(buf)
			seen.Write(buf[:n])
			if err != nil {
				break
			}
		}
		got <- seen.String()
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, marker) {
			t.Fatalf("the session ended without writing %q; it wrote %q", marker, s)
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("the session never wrote %q", marker)
		return ""
	}
}

// waited is Wait bounded, so a session that never ends fails the case rather
// than the run.
func waited(t *testing.T, s driver.Session) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return s.Wait(ctx)
}

func TestAttachRoundTripOverTheExecStream(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_attach"
	s := spec(id)
	s.Env = map[string]string{"A": "1", "B": "sandbox"}
	h.created(t, s)
	h.exec.handle = terminal(nil)

	session, err := h.Attach(t.Context(), id, driver.AttachRequest{
		Env: map[string]string{"B": "request"}, Workdir: "/workspace/sub", Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := io.WriteString(session, "ROUNDTRIP\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, session, "ROUNDTRIP")
	if _, err := io.WriteString(session, "exit 7\n"); err != nil {
		t.Fatal(err)
	}
	if code, err := waited(t, session); err != nil || code != 7 {
		t.Fatalf("Wait = %d %v, want the shell's own exit code 7", code, err)
	}
	// The stream is over once the process is: what remains reads to its end.
	if rest, err := io.ReadAll(session); err != nil || strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("after the exit the session read %q %v, want its end", rest, err)
	}

	// The shell runs with the sandbox's environment under the request's, in
	// the request's directory, through the same wrapper as Exec.
	call := h.exec.last()
	want := []string{"env", "A=1", "B=request", "sh", "-c", `cd "$0" && exec "$@"`, "/workspace/sub", "sh"}
	if !reflect.DeepEqual(call.argv, want) {
		t.Fatalf("argv %q, want %q", call.argv, want)
	}
	if call.pod != objectName(id) || call.container != Container {
		t.Fatalf("the session reached %s/%s", call.pod, call.container)
	}

	// A named command runs as named, and an exit of zero is a code of zero.
	named, err := h.Attach(t.Context(), id, driver.AttachRequest{Command: []string{"bash", "-l"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = named.Close() })
	if _, err := io.WriteString(named, "exit 0\n"); err != nil {
		t.Fatal(err)
	}
	if code, err := waited(t, named); err != nil || code != 0 {
		t.Fatalf("Wait = %d %v, want 0", code, err)
	}
	// The exec is opened by the session's own goroutine, so its call is read
	// once the session has ended.
	if got := h.exec.last().argv; !reflect.DeepEqual(got[len(got)-2:], []string{"bash", "-l"}) {
		t.Fatalf("argv %q does not end in the named command", got)
	}
}

func TestAttachResizeReachesTheQueue(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_resize"
	h.created(t, spec(id))
	sizes := make(chan string, 4)
	h.exec.handle = terminal(sizes)

	session, err := h.Attach(t.Context(), id, driver.AttachRequest{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	next := func() string {
		t.Helper()
		select {
		case size, ok := <-sizes:
			if !ok {
				return "closed"
			}
			return size
		case <-time.After(5 * time.Second):
			t.Fatal("the executor read no window")
			return ""
		}
	}
	if got := next(); got != "80x24" {
		t.Fatalf("the first window the executor read is %s, want the request's 80x24", got)
	}
	if err := session.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if got := next(); got != "120x40" {
		t.Fatalf("the executor read %s after the resize, want 120x40", got)
	}
	for _, bad := range [][2]int{{0, 40}, {120, -1}, {70000, 40}, {120, 70000}} {
		if err := session.Resize(bad[0], bad[1]); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Resize(%d, %d) = %v, want ErrInvalid", bad[0], bad[1], err)
		}
	}

	if _, err := io.WriteString(session, "exit 0\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := waited(t, session); err != nil {
		t.Fatal(err)
	}
	// The queue ends with the session, so the executor's resize loop
	// returns, and a window for a terminal that has ended is moot.
	if got := next(); got != "closed" {
		t.Fatalf("the size queue answered %s after the session ended, want nil", got)
	}
	if err := session.Resize(100, 30); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Resize after the end = %v, want ErrNotRunning", err)
	}
}

// TestTheWindowKeepsTheNewestSize: a window the executor has not read yet is
// replaced, not queued behind, because the terminal only ever has one size.
func TestTheWindowKeepsTheNewestSize(t *testing.T) {
	w := newWindow()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		if err := w.set(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.Next(); got == nil || *got != (remotecommand.TerminalSize{Width: 120, Height: 40}) {
		t.Fatalf("Next = %v, want the newest window 120x40", got)
	}
	// Concurrent writers and one reader never block each other.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { _ = w.set(10+i, 10) })
	}
	wg.Wait()
	if got := w.Next(); got == nil {
		t.Fatal("Next after eight resizes answered nil")
	}
	w.end()
	w.end()
	if got := w.Next(); got != nil {
		t.Fatalf("Next after end = %v, want nil", got)
	}
}

func TestAttachCloseEndsTheSession(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_attachclose"
	h.created(t, spec(id))
	cancelled := make(chan struct{})
	h.exec.handle = func(ctx context.Context, c execCall, stdin io.Reader, stdout, _ io.Writer) error {
		go func() { _, _ = io.Copy(stdout, stdin) }()
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	session, err := h.Attach(t.Context(), id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(session, "ALIVE"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, session, "ALIVE")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the exec")
	}
	if _, err := session.Read(make([]byte, 8)); err == nil {
		t.Error("Read after Close reported no error")
	}
	if _, err := session.Write([]byte("x")); err == nil {
		t.Error("Write after Close reported no error")
	}
	if _, err := waited(t, session); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait after Close = %v, want the cancellation", err)
	}
	if err := session.Resize(80, 24); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("Resize after Close = %v, want ErrNotRunning", err)
	}
	if err := session.Close(); err != nil {
		t.Errorf("a second Close = %v", err)
	}
}

// TestAttachEndsWithItsContext: the session belongs to the context it was
// opened with, as a native session does, so a caller that goes away takes
// the terminal with it.
func TestAttachEndsWithItsContext(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_attachctx"
	h.created(t, spec(id))
	h.exec.handle = terminal(nil)
	ctx, cancel := context.WithCancel(t.Context())
	session, err := h.Attach(ctx, id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	cancel()
	if _, err := waited(t, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after the context ended = %v, want the cancellation", err)
	}
	if _, err := io.ReadAll(session); err != nil {
		t.Fatalf("the stream did not end with the context: %v", err)
	}
}

func TestAttachRefusals(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_attachrefuse"
	h.created(t, spec(id))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   string
		req  driver.AttachRequest
		want error
	}{
		{"negative columns", t.Context(), id, driver.AttachRequest{Cols: -1, Rows: 24}, driver.ErrInvalid},
		{"negative rows", t.Context(), id, driver.AttachRequest{Cols: 80, Rows: -1}, driver.ErrInvalid},
		{"a side past 65535", t.Context(), id, driver.AttachRequest{Cols: 80, Rows: 70000}, driver.ErrInvalid},
		{"a blank command", t.Context(), id, driver.AttachRequest{Command: []string{" "}}, driver.ErrInvalid},
		{"an unknown sandbox", t.Context(), "sbx_absent", driver.AttachRequest{}, driver.ErrNotFound},
		{"a cancelled context", cancelled, id, driver.AttachRequest{}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.exec.count()
			if _, err := h.Attach(tc.ctx, tc.id, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Attach = %v, want %v", err, tc.want)
			}
			if h.exec.count() != before {
				t.Fatal("a refused Attach opened an exec")
			}
		})
	}

	// A stopped sandbox has no Pod to exec into.
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Attach(t.Context(), id, driver.AttachRequest{}); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Attach while stopped = %v, want ErrNotRunning", err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// A driver built with no cluster connection has no exec to open.
	h.stream = nil
	if _, err := h.Attach(t.Context(), id, driver.AttachRequest{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Attach without a connection = %v, want ErrUnsupported", err)
	}
}
