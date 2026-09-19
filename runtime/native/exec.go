// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	driver "latere.ai/x/cella/runtime"
)

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
	e.once.Do(func() { e.cancel(); _ = e.stdout.Close(); _ = e.stderr.Close() })
	return nil
}

// Exec ports the native host-command and exit-status plumbing. It inherits only
// PATH; hosted service credentials in the daemon environment are never forwarded.
func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Command) == 0 || req.Timeout < 0 {
		return nil, driver.ErrInvalid
	}
	if req.TTY || req.Stdin != nil {
		return nil, driver.ErrUnsupported
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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
	defer root.Close()
	// Validate the requested directory before launching the unconfined process.
	dir, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	runctx, cancel := context.WithCancel(ctx)
	if req.Timeout > 0 {
		cancel()
		runctx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	cmd := exec.CommandContext(runctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = filepath.Join(d.dir(id), "workspace", rel)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(d.dir(id), "workspace")}
	for k, v := range r.Env {
		env[k] = v
	}
	for k, v := range req.Env {
		env[k] = v
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd.Stdout = outW
	cmd.Stderr = errW
	e := &execution{stdout: outR, stderr: errR, cancel: cancel, done: make(chan struct{})}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, err
	}
	if d.active[id] == nil {
		d.active[id] = make(map[*execution]struct{})
	}
	d.active[id][e] = struct{}{}
	go func() {
		err := cmd.Wait()
		if runctx.Err() != nil {
			e.err = runctx.Err()
		} else if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.code = exit.ExitCode()
		} else {
			e.err = err
		}
		_ = outW.Close()
		_ = errW.Close()
		cancel()
		close(e.done)
		d.mu.Lock()
		delete(d.active[id], e)
		d.mu.Unlock()
	}()
	return e, nil
}
