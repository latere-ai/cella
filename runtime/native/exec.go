// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	driver "latere.ai/x/cella/runtime"
)

var (
	errNoPTY         = fmt.Errorf("%w: this build opens no pseudo-terminal", driver.ErrUnsupported)
	errInvalidWindow = fmt.Errorf("%w: a terminal window is positive and below 65536", driver.ErrInvalid)
)

// execution is one command the driver started. Under a terminal stdout is the
// controlling end of the pair and stderr is already at its end, because a
// terminal carries one stream; otherwise the two are pipes.
type execution struct {
	stdout, stderr io.ReadCloser
	pty            *os.File
	stdin          io.WriteCloser
	// closeWriters ends the pipes a command without a terminal wrote into;
	// nil under a terminal, where the pair itself carries the end.
	closeWriters func()
	cancel       context.CancelFunc
	done         chan struct{}
	code         int
	err          error
	once         sync.Once
	pid          int
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
		if e.stdin != nil {
			_ = e.stdin.Close()
		}
	})
	return nil
}

// closedStream is the second output stream of a command running under a
// terminal: a reader at its end, so a caller draining both never blocks.
type closedStream struct{}

func (closedStream) Read([]byte) (int, error) { return 0, io.EOF }
func (closedStream) Close() error             { return nil }

// Exec ports the native host-command and exit-status plumbing. It inherits only
// PATH; hosted service credentials in the daemon environment are never forwarded.
func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Command) == 0 || req.Timeout < 0 {
		return nil, driver.ErrInvalid
	}
	if (req.TTY || req.Stdin != nil) && !ptySupported {
		return nil, errNoPTY
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execLocked(ctx, id, req, 0, 0)
}

// execLocked starts one command. cols and rows are the terminal window the
// command starts with and are read only under req.TTY; zero leaves the
// kernel's default, which is what a caller that never resizes gets.
func (d *Driver) execLocked(ctx context.Context, id string, req driver.ExecRequest, cols, rows int) (*execution, error) {
	if err := d.mainError(id); err != nil {
		return nil, err
	}
	r, err := d.load(id)
	if err != nil {
		return nil, err
	}
	if r.State.Phase != driver.Running {
		return nil, driver.ErrNotRunning
	}
	cwd := req.Workdir
	if cwd == "" {
		cwd = r.Workdir
	}
	rel, err := workspacePath(cwd)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Join(d.dir(id), "workspace"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// Validate the requested directory before launching the unconfined process.
	dir, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	runctx, cancel := context.WithCancel(ctx)
	if req.Timeout > 0 {
		cancel()
		runctx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	cmd := exec.CommandContext(runctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = filepath.Join(d.dir(id), "workspace", rel)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(d.dir(id), "workspace")}
	maps.Copy(env, r.Env)
	maps.Copy(env, req.Env)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	e := &execution{cancel: cancel, done: make(chan struct{})}
	stop, err := d.wire(cmd, e, req, cols, rows)
	if err != nil {
		cancel()
		stop()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stop()
		return nil, err
	}
	// The parent's copy of the process end of the pair is closed once the
	// command holds its own, or the controlling end never sees the session end.
	stop()
	e.pid = cmd.Process.Pid
	if d.active[id] == nil {
		d.active[id] = make(map[*execution]struct{})
	}
	d.active[id][e] = struct{}{}
	go d.reap(runctx, id, cmd, e)
	return e, nil
}

// wire connects the command's three descriptors and fills the execution's
// streams. It returns the release for what the parent must drop after the
// command has started, which runs on the failure paths too.
func (d *Driver) wire(cmd *exec.Cmd, e *execution, req driver.ExecRequest, cols, rows int) (func(), error) {
	if req.TTY {
		controlling, other, err := openPTY()
		if err != nil {
			return func() {}, err
		}
		release := func() { _ = other.Close() }
		if cols > 0 && rows > 0 {
			if err = setWinsize(controlling, cols, rows); err != nil {
				_ = controlling.Close()
				return release, err
			}
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = other, other, other
		// Setsid and Setctty give the command the pair as its controlling
		// terminal. Setpgid is not set with them: a session leader refuses
		// setpgid, and after setsid the group id equals the pid, so the group
		// kill below still reaches every descendant.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
		e.pty = controlling
		e.stdout, e.stderr = &ptyReader{f: controlling}, closedStream{}
		return release, nil
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout, cmd.Stderr = outW, errW
	e.stdout, e.stderr = outR, errR
	e.closeWriters = func() { _ = outW.Close(); _ = errW.Close() }
	if req.Stdin == nil {
		return func() {}, nil
	}
	// The command's own pipe, not cmd.Stdin: os/exec would then copy the
	// reader in a goroutine that cmd.Wait blocks on, and a stdin that never
	// ends would hold the exit code behind WaitDelay.
	in, err := cmd.StdinPipe()
	if err != nil {
		e.closeWriters()
		_ = outR.Close()
		_ = errR.Close()
		return func() {}, err
	}
	e.stdin = in
	go func() {
		_, _ = io.Copy(in, req.Stdin)
		_ = in.Close()
	}()
	return func() {}, nil
}

// reap waits for the command, ends the streams and records the outcome. A
// command may exit while its descendants keep running with detached IO, so
// every execution owns the whole process group.
func (d *Driver) reap(runctx context.Context, id string, cmd *exec.Cmd, e *execution) {
	err := cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if runctx.Err() != nil {
		e.err = runctx.Err()
	} else if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		e.code = exit.ExitCode()
	} else {
		e.err = err
	}
	if e.closeWriters != nil {
		e.closeWriters()
	}
	e.cancel()
	close(e.done)
	d.mu.Lock()
	delete(d.active[id], e)
	d.mu.Unlock()
}

// shell is what a session runs when the request names no command. The native
// driver runs host processes, so it is the host's shell on PATH.
var shell = []string{"sh"}

// Attach opens one terminal inside the sandbox. The session is an execution
// under a pseudo-terminal, so Stop and Delete end it with every other command
// the sandbox is running.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ptySupported {
		return nil, errNoPTY
	}
	if req.Cols < 0 || req.Rows < 0 {
		return nil, errInvalidWindow
	}
	command := req.Command
	if len(command) == 0 {
		command = shell
	}
	if strings.TrimSpace(command[0]) == "" {
		return nil, driver.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.execLocked(ctx, id, driver.ExecRequest{Command: command, Env: req.Env, Workdir: req.Workdir, TTY: true}, req.Cols, req.Rows)
	if err != nil {
		return nil, err
	}
	return &session{e: e}, nil
}

// session is one attached terminal: reads are what it wrote, writes are what a
// person typed, and Close kills the process group behind it.
type session struct{ e *execution }

func (s *session) Read(p []byte) (int, error)  { return s.e.stdout.Read(p) }
func (s *session) Write(p []byte) (int, error) { return s.e.pty.Write(p) }
func (s *session) Close() error                { return s.e.Close() }
func (s *session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return errInvalidWindow
	}
	return setWinsize(s.e.pty, cols, rows)
}
func (s *session) Wait(ctx context.Context) (int, error) { return s.e.Wait(ctx) }
