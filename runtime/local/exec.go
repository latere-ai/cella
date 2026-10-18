// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	driver "latere.ai/x/cella/runtime"
)

// pollEvery is how often a reader of a stage's output looks for more, and a
// waiter for the stage's end.
const pollEvery = 20 * time.Millisecond

// stderrWrapper runs a detached command with its standard error in a file of
// its own, its first argument, so the caller reads the two streams apart:
// the stage's log carries standard output alone.
const stderrWrapper = `f=$1; shift; exec "$@" 2>>"$f"`

// Exec runs one command in a running sandbox. With no stdin it is a detached
// stage, which a crash of this process does not end; with stdin it is an
// attached stage whose pipes this driver holds, which ends with this process.
// A terminal is refused: the operating system's sandbox has no seam for one.
func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.TTY {
		return nil, fmt.Errorf("%w: the local driver has no terminal", driver.ErrUnsupported)
	}
	if len(req.Command) == 0 {
		return nil, fmt.Errorf("%w: an exec names a command", driver.ErrInvalid)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return nil, err
	}
	if _, err = d.refresh(ctx, &r); err != nil {
		return nil, err
	}
	if r.State.Phase != driver.Running {
		return nil, fmt.Errorf("%w: %s is %s", driver.ErrNotRunning, id, r.State.Phase)
	}
	workdir := r.Workdir
	if req.Workdir != "" {
		if workdir, err = r.hostPath(req.Workdir); err != nil {
			return nil, err
		}
	}
	env := maps.Clone(r.Env)
	maps.Copy(env, req.Env)
	r.Execs++
	name := execName(r.Execs)
	if req.Stdin != nil {
		if err = d.save(r); err != nil {
			return nil, err
		}
		return d.attach(ctx, id, d.stage(r, name, req.Command, env, workdir), req)
	}
	errPath := filepath.Join(d.paths(id).streams, name+".err")
	if err = os.WriteFile(errPath, nil, 0o600); err != nil {
		return nil, err
	}
	argv := append([]string{"/bin/sh", "-c", stderrWrapper, "sh", errPath}, req.Command...)
	spec := d.stage(r, name, argv, env, workdir)
	handle, err := d.sandbox.Launch(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("local: starting a command in %s: %w", id, err)
	}
	r.Stages = append(r.Stages, handle)
	if err = d.save(r); err != nil {
		_ = d.sandbox.Stop(context.WithoutCancel(ctx), handle)
		return nil, err
	}
	e := &detachedExec{d: d, id: id, handle: handle, errPath: errPath, done: make(chan struct{})}
	e.stdout = &follower{path: spec.LogPath, ended: e.ended, closed: e.done}
	e.stderr = &follower{path: errPath, ended: e.ended, closed: e.done}
	// The caller going away, or its timeout passing, ends the command: the
	// work is nobody's any longer. A crash of this process ends neither,
	// which is what a detached stage is for.
	go func() {
		var timeout <-chan time.Time
		if req.Timeout > 0 {
			timer := time.NewTimer(req.Timeout)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-ctx.Done():
			_ = e.end(ctx, ctx.Err())
		case <-timeout:
			_ = e.end(ctx, context.DeadlineExceeded)
		case <-e.done:
		}
	}()
	return e, nil
}

// detachedExec is a command running as a detached stage: its standard output
// is the stage's log and its standard error the file the wrapper writes, both
// read as they grow until the stage has ended.
type detachedExec struct {
	d              *Driver
	id             string
	handle         hostsandbox.StageHandle
	errPath        string
	stdout, stderr *follower

	once sync.Once
	done chan struct{}
	// why is what ended the command before it ended on its own: the
	// caller's context, its timeout, or a close.
	why error
}

func (e *detachedExec) Stdout() io.Reader { return e.stdout }
func (e *detachedExec) Stderr() io.Reader { return e.stderr }

// ended reports whether the stage has finished, so a reader at the end of its
// file knows nothing more is coming.
func (e *detachedExec) ended() bool {
	status, err := e.d.sandbox.Observe(context.Background(), e.handle)
	return err != nil || !status.Running
}

// Wait waits for the stage to end and answers its exit code. A stage that
// left no status was lost, which is exit code -1.
func (e *detachedExec) Wait(ctx context.Context) (int, error) {
	for {
		select {
		case <-e.done:
			return 0, e.why
		default:
		}
		status, err := e.d.sandbox.Observe(ctx, e.handle)
		if err != nil {
			return 0, err
		}
		switch {
		case status.Gone:
			return -1, ErrLost
		case !status.Running:
			e.forget()
			return status.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-e.done:
			return 0, e.why
		case <-time.After(pollEvery):
		}
	}
}

// Close ends the command, together with what it started, and forgets its
// files: the driver keeps no command's output.
func (e *detachedExec) Close() error {
	return e.end(context.Background(), fmt.Errorf("%w: the command was closed", driver.ErrNotRunning))
}

// end stops the stage once, for the reason given, which Wait answers. The
// stop outlives ctx, which has often ended already: it is what ended the
// command.
func (e *detachedExec) end(ctx context.Context, why error) error {
	var err error
	e.once.Do(func() {
		e.why = why
		err = e.d.sandbox.Stop(context.WithoutCancel(ctx), e.handle)
		e.forget()
		close(e.done)
	})
	return err
}

// forget drops the stage from the sandbox's record and removes its files.
func (e *detachedExec) forget() {
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	r, err := e.d.load(e.id)
	if err != nil {
		return
	}
	r.Stages = slices.DeleteFunc(r.Stages, func(h hostsandbox.StageHandle) bool { return h == e.handle })
	if e.d.save(r) == nil {
		_ = os.Remove(e.errPath)
	}
}

// follower reads a file as it grows, ending at its end once the writer has
// ended.
type follower struct {
	path   string
	ended  func() bool
	closed <-chan struct{}
	f      *os.File
}

func (f *follower) Read(p []byte) (int, error) {
	for {
		if f.f == nil {
			file, err := os.Open(f.path)
			if err == nil {
				f.f = file
			} else if !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
		}
		if f.f != nil {
			n, err := f.f.Read(p)
			if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
				return n, err
			}
		}
		// At the end of what is written: done when the writer has ended
		// and one more read finds nothing, which closes the race with a
		// last write.
		if f.ended() {
			if f.f != nil {
				if n, err := f.f.Read(p); n > 0 {
					return n, err
				}
				_ = f.f.Close()
			}
			return 0, io.EOF
		}
		select {
		case <-f.closed:
			if f.f != nil {
				_ = f.f.Close()
			}
			return 0, io.EOF
		case <-time.After(pollEvery):
		}
	}
}

// attach starts an attached stage: the rendered command under os/exec, its
// stdin the caller's and its outputs piped back, killed whole when it is
// closed, when its timeout passes, or when this driver closes.
func (d *Driver) attach(caller context.Context, id string, spec hostsandbox.StageSpec, req driver.ExecRequest) (driver.Exec, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(caller))
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.WithoutCancel(caller), req.Timeout)
	}
	stop := context.AfterFunc(caller, cancel)
	cmd, cleanup, err := d.sandbox.Command(ctx, spec)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("local: rendering a command in %s: %w", id, err)
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout, cmd.Stderr = outW, errW
	// The command's own pipe, not cmd.Stdin: os/exec would then copy the
	// reader in a goroutine cmd.Wait blocks on, and a stdin that never ends
	// would hold a killed command's exit behind it. Wait closes this pipe
	// once the command has exited, which ends the copy.
	in, err := cmd.StdinPipe()
	if err != nil {
		stop()
		cancel()
		cleanup()
		return nil, fmt.Errorf("local: piping the input of a command in %s: %w", id, err)
	}
	if err = cmd.Start(); err != nil {
		stop()
		cancel()
		cleanup()
		return nil, fmt.Errorf("local: starting a command in %s: %w", id, err)
	}
	go func() {
		_, _ = io.Copy(in, req.Stdin)
		_ = in.Close()
	}()
	e := &attachedExec{d: d, id: id, cancel: cancel, stdout: outR, stderr: errR, done: make(chan struct{})}
	if d.attached[id] == nil {
		d.attached[id] = map[*attachedExec]struct{}{}
	}
	d.attached[id][e] = struct{}{}
	go func() {
		err := cmd.Wait()
		stop()
		cleanup()
		e.code = -1
		var exit *exec.ExitError
		switch {
		case e.lost.Load():
			e.err = ErrLost
		case caller.Err() != nil:
			e.err = caller.Err()
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			e.err = context.DeadlineExceeded
		case err == nil:
			e.code = 0
		case errors.As(err, &exit) && exit.ExitCode() >= 0:
			e.code = exit.ExitCode()
		default:
			e.err = err
		}
		_ = outW.Close()
		_ = errW.Close()
		d.mu.Lock()
		delete(d.attached[id], e)
		d.mu.Unlock()
		close(e.done)
	}()
	return e, nil
}

// attachedExec is a command running as an attached stage.
type attachedExec struct {
	d      *Driver
	id     string
	cancel context.CancelFunc
	stdout *io.PipeReader
	stderr *io.PipeReader
	lost   atomic.Bool

	done chan struct{}
	code int
	err  error
}

func (e *attachedExec) Stdout() io.Reader { return e.stdout }
func (e *attachedExec) Stderr() io.Reader { return e.stderr }

// Wait waits for the command to end and answers its exit code; one lost with
// this driver's process answers -1 and ErrLost.
func (e *attachedExec) Wait(ctx context.Context) (int, error) {
	select {
	case <-e.done:
		return e.code, e.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Close kills the command and what it started.
func (e *attachedExec) Close() error {
	e.end()
	<-e.done
	return nil
}

// end kills the command's process group without waiting.
func (e *attachedExec) end() { e.cancel() }

// lose ends the command as lost with the process that held it.
func (e *attachedExec) lose() {
	e.lost.Store(true)
	e.cancel()
	<-e.done
}

// Logs is the main stage's output from the start, the last lines of it, or
// all of it followed until the stage ends. The log carries no times, so Since
// answers the whole log or nothing by when it was last written.
func (d *Driver) Logs(ctx context.Context, id string, req driver.LogsRequest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.TailLines < 0 {
		return nil, driver.ErrInvalid
	}
	if _, err := d.Inspect(ctx, id); err != nil {
		return nil, err
	}
	logPath := filepath.Join(d.paths(id).logs, "main.log")
	if !req.Follow {
		body, err := os.ReadFile(logPath)
		if errors.Is(err, os.ErrNotExist) {
			body = nil
		} else if err != nil {
			return nil, err
		}
		// The log carries no times, so Since is held to the log's last
		// write: a log written to before it holds nothing after it.
		if info, err := os.Stat(logPath); !req.Since.IsZero() && (err != nil || info.ModTime().Before(req.Since)) {
			body = nil
		}
		return io.NopCloser(bytes.NewReader(lastLines(body, req.TailLines))), nil
	}
	// The follow outlives the call that opened it, so the stage is read
	// under a context that the caller's ending does not cancel.
	closed, background := make(chan struct{}), context.WithoutCancel(ctx)
	ended := func() bool {
		state, err := d.Inspect(background, id)
		return err != nil || state.Phase != driver.Running
	}
	return &followed{follower: follower{path: logPath, ended: ended, closed: closed}, closed: closed}, nil
}

// followed is a followed log its reader closes to stop.
type followed struct {
	follower
	closed chan struct{}
	once   sync.Once
}

func (f *followed) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// lastLines is the last n lines of a log, or all of it for n zero.
func lastLines(body []byte, n int) []byte {
	if n == 0 {
		return body
	}
	var lines [][]byte
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		lines = append(lines, append(slices.Clone(scanner.Bytes()), '\n'))
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return bytes.Join(lines, nil)
}
