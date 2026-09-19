// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	utilexec "k8s.io/client-go/util/exec"

	driver "latere.ai/x/cella/runtime"
)

// drain reads both streams to their end at once. A caller that reads one to
// EOF before the other stalls the command, which is the contract the
// conformance suite states and this package's own tests keep to.
func drain(e driver.Exec) (stdout, stderr string) {
	var out, errOut []byte
	var wg sync.WaitGroup
	wg.Go(func() { out, _ = io.ReadAll(e.Stdout()) })
	wg.Go(func() { errOut, _ = io.ReadAll(e.Stderr()) })
	wg.Wait()
	return string(out), string(errOut)
}

// exited is the error the cluster returns for a command that ran and failed.
func exited(code int) error {
	return utilexec.CodeExitError{Err: errors.New("command terminated"), Code: code}
}

func TestExecWrapsAndExits(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_exec"
	s := spec(id)
	s.Env = map[string]string{"B": "2", "A": "1"}
	h.created(t, s)
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "out")
		_, _ = io.WriteString(stderr, "err")
		return exited(3)
	}
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"sh", "-c", "exit 3"}, Env: map[string]string{"B": "override"}})
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := drain(e)
	code, err := e.Wait(t.Context())
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d %v, want the command's own exit code", code, err)
	}
	if out != "out" || errOut != "err" {
		t.Fatalf("streams %q %q", out, errOut)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// The sandbox's environment, the request's overrides and the working
	// directory all travel in the argv, sorted.
	want := []string{"env", "A=1", "B=override", "sh", "-c", `cd "$0" && exec "$@"`, driver.DefaultWorkdir, "sh", "-c", "exit 3"}
	if got := h.exec.last().argv; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
	if got := h.exec.last().pod; got != objectName(id) {
		t.Fatalf("exec reached pod %q", got)
	}
}

func TestExecRequestWorkdir(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_execdir"
	h.created(t, spec(id))
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"cat", "marker"}, Workdir: "/workspace/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.exec.last().argv; got[len(got)-3] != "/workspace/sub" {
		t.Fatalf("argv %q", got)
	}
}

func TestExecRefusals(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_execrefuse"
	h.created(t, spec(id))
	for _, tc := range []struct {
		name string
		req  driver.ExecRequest
		want error
	}{
		{"no command", driver.ExecRequest{}, driver.ErrInvalid},
		{"negative timeout", driver.ExecRequest{Command: []string{"true"}, Timeout: -time.Second}, driver.ErrInvalid},
		{"stdin", driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("x")}, driver.ErrUnsupported},
		{"tty", driver.ExecRequest{Command: []string{"sh"}, TTY: true}, driver.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.Exec(t.Context(), id, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Exec = %v, want %v", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.Exec(ctx, id, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec with a cancelled context = %v", err)
	}
}

func TestExecNeedsARunningPod(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_execstopped"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Exec while stopped = %v, want ErrNotRunning", err)
	}
	// A Pod whose process has ended is not a Pod to exec into either.
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pod := h.podOf(t, id)
	pod.Status.Phase = corev1.PodSucceeded
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Exec after the process ended = %v, want ErrNotRunning", err)
	}
}

func TestExecStreamsWhileTheCommandRuns(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_stream"
	h.created(t, spec(id))
	release := make(chan struct{})
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "first")
		<-release
		_, _ = io.WriteString(stdout, "second")
		return nil
	}
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, len("first"))
	if _, err := io.ReadFull(e.Stdout(), head); err != nil {
		t.Fatal(err)
	}
	// The command has not finished, so a bounded Wait times out rather than
	// answering.
	early, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, err = e.Wait(early)
	cancel()
	if err == nil {
		t.Fatal("Wait answered before the command finished")
	}
	close(release)
	rest, err := io.ReadAll(e.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	code, err := e.Wait(t.Context())
	if err != nil || code != 0 {
		t.Fatalf("Wait = %d %v", code, err)
	}
	if string(head)+string(rest) != "firstsecond" {
		t.Fatalf("streamed %q%q", head, rest)
	}
}

func TestExecTimeoutAndCancellation(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_execctx"
	h.created(t, spec(id))
	// The command runs until the driver's own context ends, which is what a
	// timeout and a cancellation each have to reach.
	h.exec.handle = func(ctx context.Context, _ execCall, _ io.Reader, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"sleep", "60"}, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait after the request timeout = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	e, err = h.Exec(ctx, id, driver.ExecRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := e.Wait(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after the caller cancelled = %v", err)
	}
	// Close ends the command, and a Wait after it answers with why.
	e, err = h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err == nil {
		t.Fatal("Wait after Close answered as if the command had finished")
	}
	if err := e.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
}

func TestExecReportsAStreamFailure(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_execfail"
	h.created(t, spec(id))
	h.exec.handle = func(context.Context, execCall, io.Reader, io.Writer, io.Writer) error {
		return errors.New("the connection went away")
	}
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("Wait = %v, want the stream's own failure", err)
	}
}

func TestExecNeedsAClusterConnection(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_noconn"
	h.created(t, spec(id))
	h.stream = nil
	if _, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Exec without a connection = %v, want ErrUnsupported", err)
	}
}

func TestWrapArgv(t *testing.T) {
	for _, tc := range []struct {
		name        string
		argv        []string
		environment map[string]string
		workdir     string
		want        []string
	}{
		{"nothing to wrap", []string{"true"}, nil, "", []string{"true"}},
		{"environment only", []string{"true"}, map[string]string{"B": "2", "A": "1"}, "", []string{"env", "A=1", "B=2", "true"}},
		{"directory only", []string{"true"}, nil, "/workspace", []string{"env", "sh", "-c", `cd "$0" && exec "$@"`, "/workspace", "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapArgv(tc.argv, tc.environment, tc.workdir); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wrapArgv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExitCodeReadsTheClusterError(t *testing.T) {
	if code, ok := exitCode(exited(7)); !ok || code != 7 {
		t.Fatalf("exitCode = %d %v", code, ok)
	}
	if _, ok := exitCode(errors.New("something else")); ok {
		t.Fatal("a plain error was read as an exit code")
	}
}

// The remote command path is the one part that needs a live API server. It is
// driven here as far as the dial, so the request it builds is exercised.
func TestRemoteCommandDials(t *testing.T) {
	d, err := New(Options{Namespace: namespace, REST: &rest.Config{Host: "https://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = d.stream.stream(ctx, "pod", execOpts{argv: []string{"true"}}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("a stream to a closed port succeeded")
	}
}
