// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	driver "latere.ai/x/cella/runtime"
)

// execOpts is one command in one container. An empty container is the
// workload's, so every call that does not name one addresses the sandbox
// itself and only the desktop's own commands reach the display container.
type execOpts struct {
	argv      []string
	stdin     bool
	container string
}

// name is the container the command runs in.
func (o execOpts) name() string {
	if o.container == "" {
		return Container
	}
	return o.container
}

// streamer opens the exec subresource. It is an interface because the stream
// is the one part of this driver that needs a live API server: everything
// around it is exercised against a client double.
type streamer interface {
	stream(ctx context.Context, pod string, o execOpts, stdin io.Reader, stdout, stderr io.Writer) error
}

// spdy runs the command over the cluster's remote command protocol: a
// WebSocket stream where the API server offers one, and the older upgrade
// where it does not.
type spdy struct {
	cfg       *rest.Config
	cs        kubernetes.Interface
	namespace string
}

func (s *spdy) stream(ctx context.Context, pod string, o execOpts, stdin io.Reader, stdout, stderr io.Writer) error {
	req := s.cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(s.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: o.name(), Command: o.argv,
			Stdin: o.stdin, Stdout: stdout != nil, Stderr: stderr != nil,
		}, scheme.ParameterCodec)
	upgrade, err := remotecommand.NewSPDYExecutor(s.cfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec upgrade: %w", err)
	}
	socket, err := remotecommand.NewWebSocketExecutor(s.cfg, "GET", req.URL().String())
	if err != nil {
		return fmt.Errorf("exec websocket: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(socket, upgrade, httpstream.IsUpgradeFailure)
	if err != nil {
		return fmt.Errorf("exec executor: %w", err)
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

// execution is one running command. Both streams are pipes, so the caller
// reads the first byte while the command is still writing.
type execution struct {
	stdout, stderr *io.PipeReader
	cancel         context.CancelFunc
	done           chan struct{}
	code           int
	err            error
	once           sync.Once
}

func (e *execution) Stdout() io.Reader { return e.stdout }
func (e *execution) Stderr() io.Reader { return e.stderr }

func (e *execution) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-e.done:
		return e.code, e.err
	}
}

func (e *execution) Close() error {
	e.once.Do(func() {
		e.cancel()
		_ = e.stdout.Close()
		_ = e.stderr.Close()
	})
	return nil
}

// Exec runs one command in the sandbox's container. The exec subresource
// carries neither an environment nor a working directory, so the sandbox's own
// environment, the request's, and the directory are wrapped around the argv.
func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Command) == 0 || req.Timeout < 0 {
		return nil, fmt.Errorf("%w: a command is required", driver.ErrInvalid)
	}
	if req.TTY || req.Stdin != nil {
		// Both need a terminal session, which is the Attach capability.
		return nil, fmt.Errorf("%w: stdin and a tty need Attach", driver.ErrUnsupported)
	}
	spec, err := d.runningSpec(ctx, id)
	if err != nil {
		return nil, err
	}
	environment := maps.Clone(spec.Env)
	if environment == nil {
		environment = map[string]string{}
	}
	maps.Copy(environment, req.Env)
	workdir := req.Workdir
	if workdir == "" {
		workdir = spec.Workdir
	}
	if workdir == "" {
		workdir = workspacePath(spec)
	}
	return d.start(ctx, objectName(id), execOpts{argv: wrapArgv(req.Command, environment, workdir)}, req.Timeout)
}

// runningSpec is the read every execution path shares: the sandbox exists, it
// has a Pod, and the Pod is running.
func (d *Driver) runningSpec(ctx context.Context, id string) (driver.CreateSpec, error) {
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return driver.CreateSpec{}, err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return driver.CreateSpec{}, err
	}
	if pod == nil || pod.DeletionTimestamp != nil {
		return driver.CreateSpec{}, driver.ErrNotRunning
	}
	if _, _, ended := terminated(pod); ended {
		return driver.CreateSpec{}, driver.ErrNotRunning
	}
	return specOf(pvc)
}

// start runs one command and hands back its streams. A timeout and the
// caller's cancellation both reach Wait as the context error, so a caller
// tells a command that failed from one that never finished.
func (d *Driver) start(ctx context.Context, pod string, o execOpts, timeout time.Duration) (driver.Exec, error) {
	if d.stream == nil {
		return nil, fmt.Errorf("%w: this driver was built without a cluster connection", driver.ErrUnsupported)
	}
	runctx, cancel := context.WithCancel(ctx)
	if timeout > 0 {
		cancel()
		runctx, cancel = context.WithTimeout(ctx, timeout)
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	e := &execution{stdout: outR, stderr: errR, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(e.done)
		err := d.stream.stream(runctx, pod, o, nil, outW, errW)
		switch {
		case runctx.Err() != nil:
			e.err = runctx.Err()
		case err != nil:
			if code, ok := exitCode(err); ok {
				e.code = code
			} else {
				e.err = err
			}
		}
		_ = outW.Close()
		_ = errW.Close()
		cancel()
	}()
	return e, nil
}

// exitCode reads the command's status out of the cluster's error.
func exitCode(err error) (int, bool) {
	var coded interface{ ExitStatus() int }
	if errors.As(err, &coded) {
		return coded.ExitStatus(), true
	}
	return 0, false
}

// wrapArgv puts the environment and the working directory in front of the
// command, the way a shell would: the subresource offers no field for either.
func wrapArgv(argv []string, environment map[string]string, workdir string) []string {
	if len(environment) == 0 && workdir == "" {
		return argv
	}
	out := append([]string{"env"}, sortedEnv(environment)...)
	if workdir == "" {
		return append(out, argv...)
	}
	out = append(out, "sh", "-c", `cd "$0" && exec "$@"`, workdir)
	return append(out, argv...)
}
