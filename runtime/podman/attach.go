// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// defaultShell is what a session runs when the request names no command. The
// image decides what that is; every image this driver runs has a POSIX shell
// at this path.
var defaultShell = []string{"/bin/sh"}

// resizeWindow bounds the wait for a started session to accept its first
// resize. The start and the session's readiness are two steps in the engine.
const resizeWindow = 5 * time.Second

// Attach opens one terminal in the sandbox's container: an exec session with a
// TTY, started over a hijacked connection whose bytes are the terminal's.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Cols < 0 || req.Rows < 0 {
		return nil, errInvalidWindow
	}
	command := req.Command
	if len(command) == 0 {
		command = defaultShell
	}
	if strings.TrimSpace(command[0]) == "" {
		return nil, driver.ErrInvalid
	}
	execID, err := d.createExec(ctx, id, execCreate{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: true,
		Cmd: command, WorkingDir: req.Workdir,
	}, req.Env, req.Workdir)
	if err != nil {
		return nil, err
	}
	conn, br, err := d.client().hijack(ctx, d.client().libpodPath("/exec/"+execID+"/start"), execStart{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("podman: starting the attached exec session: %w", err)
	}
	s := &session{d: d, execID: execID, conn: conn, br: br}
	if req.Cols > 0 && req.Rows > 0 {
		// The window is set after the start, not before it: the engine has no
		// session to resize until the start has run, and a terminal that never
		// took a size reports none to the process reading it.
		if err = d.resizeStarted(ctx, execID, req.Cols, req.Rows); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// resizeStarted sets the window of a session the start has just begun,
// retrying while the engine still reports no session: the hijacked start and
// the session becoming resizable are not one step.
func (d *Driver) resizeStarted(ctx context.Context, execID string, cols, rows int) error {
	deadline := time.Now().Add(resizeWindow)
	for {
		err := d.resizeExec(ctx, execID, cols, rows)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("podman: sizing the session's terminal: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(execPoll):
		}
	}
}

// createExec validates the request and creates one exec session, merging the
// sandbox's own environment with the request's as Exec does.
func (d *Driver) createExec(ctx context.Context, id string, create execCreate, reqEnv map[string]string, workdir string) (string, error) {
	if workdir != "" && (!path.IsAbs(workdir) || strings.Contains(workdir, "..")) {
		return "", fmt.Errorf("%w: workdir %q", driver.ErrInvalid, workdir)
	}
	if err := d.exists(ctx, id); err != nil {
		return "", err
	}
	st, err := d.inspectContainer(ctx, id)
	if err != nil {
		return "", err
	}
	if phase, _ := phaseOf(st, false); phase != driver.Running {
		return "", driver.ErrNotRunning
	}
	rec, err := d.current(ctx, id)
	if err != nil {
		return "", err
	}
	env := maps.Clone(rec.env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, reqEnv)
	create.Env = envList(env)
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.client().json(ctx, http.MethodPost, "/containers/"+containerName(id)+"/exec", create, &created); err != nil {
		if notFound(err) {
			return "", driver.ErrNotFound
		}
		return "", fmt.Errorf("podman: creating the exec session: %w", err)
	}
	return created.ID, nil
}

// resizeExec sets the window of a session's terminal.
func (d *Driver) resizeExec(ctx context.Context, execID string, cols, rows int) error {
	q := query("h", strconv.Itoa(rows), "w", strconv.Itoa(cols))
	return d.client().json(ctx, http.MethodPost, "/exec/"+execID+"/resize?"+q, nil, nil)
}

// session is one attached terminal. Under a TTY podman's stream carries no
// frame headers, so the bytes both ways are the terminal's own.
type session struct {
	d      *Driver
	execID string
	conn   net.Conn
	br     *bufio.Reader
	once   sync.Once
}

func (s *session) Read(p []byte) (int, error)  { return s.br.Read(p) }
func (s *session) Write(p []byte) (int, error) { return s.conn.Write(p) }

// Close drops the connection. The engine has no exec kill and the pid it
// reports is its own, so the process inside keeps running until the sandbox
// stops or is deleted; a caller that needs it gone stops the sandbox.
func (s *session) Close() error {
	s.once.Do(func() { _ = s.conn.Close() })
	return nil
}

func (s *session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return errInvalidWindow
	}
	return s.d.resizeExec(context.Background(), s.execID, cols, rows)
}

// Wait polls the session until the engine reports that it has ended.
func (s *session) Wait(ctx context.Context) (int, error) {
	return s.d.waitExec(ctx, s.execID)
}
