// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// execPoll is how often a started session is asked whether it has ended. The
// libpod exec API reports the exit code only from the session's inspect.
const execPoll = 20 * time.Millisecond

type execCreate struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

type execStart struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

type execInspect struct {
	ExitCode int  `json:"ExitCode"`
	Running  bool `json:"Running"`
}

// execution is one running command. The caller reads both streams and waits;
// Close ends the wait and closes the streams.
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

// Wait returns the command's exit code, or the reason it did not produce one.
// A context that ends first ends the wait without ending the command.
func (e *execution) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-e.done:
		return e.code, e.err
	}
}

// Close ends the wait and the streams. It does not signal the process inside
// the container: libpod has no exec kill and the pid it reports is the
// engine's. The process is reaped when the sandbox stops or is deleted.
func (e *execution) Close() error {
	e.once.Do(func() {
		e.cancel()
		_ = e.stdout.Close()
		_ = e.stderr.Close()
	})
	return nil
}

// Exec runs one command in the sandbox's container. The sandbox's environment
// is what the record holds, which is what a later Update wrote, and the
// request's own entries sit on top of it.
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
	if req.Workdir != "" && (!path.IsAbs(req.Workdir) || strings.Contains(req.Workdir, "..")) {
		return nil, fmt.Errorf("%w: workdir %q", driver.ErrInvalid, req.Workdir)
	}
	if err := d.exists(ctx, id); err != nil {
		return nil, err
	}
	st, err := d.inspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	if phase, _ := phaseOf(st, false); phase != driver.Running {
		return nil, driver.ErrNotRunning
	}
	rec, err := d.current(ctx, id)
	if err != nil {
		return nil, err
	}
	env := maps.Clone(rec.env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, req.Env)

	var created struct {
		ID string `json:"Id"`
	}
	create := execCreate{AttachStdout: true, AttachStderr: true, Cmd: req.Command, Env: envList(env), WorkingDir: req.Workdir}
	if err := d.client().json(ctx, http.MethodPost, "/containers/"+containerName(id)+"/exec", create, &created); err != nil {
		if notFound(err) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("podman: creating the exec session: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	if req.Timeout > 0 {
		cancel()
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	e := &execution{stdout: outR, stderr: errR, cancel: cancel, done: make(chan struct{})}
	go d.runExec(runCtx, created.ID, e, outW, errW)
	return e, nil
}

// runExec starts the session, splits its stream into the two writers, and
// reads the exit code back. The context ending is the one reason reported in
// place of an exit code, so Close, a timeout and a cancelled request each
// reach the caller as themselves.
func (d *Driver) runExec(ctx context.Context, execID string, e *execution, outW, errW *io.PipeWriter) {
	defer close(e.done)
	fail := func(err error) {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		e.err = err
		_ = outW.CloseWithError(err)
		_ = errW.CloseWithError(err)
	}
	resp, err := d.client().do(ctx, http.MethodPost, d.client().libpodURL("/exec/"+execID+"/start"), execStart{})
	if err != nil {
		fail(fmt.Errorf("podman: starting the exec session: %w", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if serr := statusErr(resp); serr != nil {
		fail(fmt.Errorf("podman: starting the exec session: %w", serr))
		return
	}
	if derr := demux(resp.Body, outW, errW); derr != nil {
		fail(fmt.Errorf("podman: reading the exec stream: %w", derr))
		return
	}
	_ = outW.Close()
	_ = errW.Close()
	code, err := d.waitExec(ctx, execID)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		e.err = err
		return
	}
	e.code = code
}

// waitExec polls the session until it reports that it has ended.
func (d *Driver) waitExec(ctx context.Context, execID string) (int, error) {
	for {
		var ei execInspect
		if err := d.client().json(ctx, http.MethodGet, "/exec/"+execID+"/json", nil, &ei); err != nil {
			return 0, fmt.Errorf("podman: inspecting the exec session: %w", err)
		}
		if !ei.Running {
			return ei.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(execPoll):
		}
	}
}

// envList renders an environment as the KEY=VALUE list the API takes, sorted
// so one environment renders one way.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}

// demux splits podman's framed stream into the two writers: an 8-byte header
// whose first byte is the stream (2 is stderr) and whose last four are the
// payload length, then the payload. The end of a frame boundary is the clean
// end; a stream that ends inside a header or a payload was cut, and that
// surfaces so a caller never reads truncated output as complete.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	br := bufio.NewReader(r)
	var header [8]byte
	for {
		if _, err := io.ReadFull(br, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(header[4:])
		w := stdout
		if header[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, br, int64(n)); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
	}
}
