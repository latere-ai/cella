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
	"net/http"
	"slices"
	"sync"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// execPoll is how often a started session is asked whether it has ended. The
// libpod exec API reports the exit code only from the session's inspect.
const execPoll = 20 * time.Millisecond

type execCreate struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

// errInvalidWindow refuses a terminal window the engine cannot hold.
var errInvalidWindow = fmt.Errorf("%w: a terminal window is positive", driver.ErrInvalid)

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
// request's own entries sit on top of it. A request with a TTY or a stdin
// travels over a hijacked connection; without either, the reply's body is the
// whole of the stream.
func (d *Driver) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Command) == 0 || req.Timeout < 0 {
		return nil, driver.ErrInvalid
	}
	create := execCreate{
		AttachStdin: req.Stdin != nil, AttachStdout: true, AttachStderr: !req.TTY,
		Tty: req.TTY, Cmd: req.Command, WorkingDir: req.Workdir,
	}
	execID, err := d.createExec(ctx, id, create, req.Env, req.Workdir)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	if req.Timeout > 0 {
		cancel()
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	e := &execution{stdout: outR, stderr: errR, cancel: cancel, done: make(chan struct{})}
	if req.TTY {
		// A terminal carries one stream, so the second reader is at its end
		// from the first read and a caller draining both never blocks.
		_ = errW.Close()
	}
	go d.runExec(runCtx, execID, e, outW, errW, req)
	return e, nil
}

// runExec starts the session, splits its stream into the two writers, and
// reads the exit code back. The context ending is the one reason reported in
// place of an exit code, so Close, a timeout and a cancelled request each
// reach the caller as themselves. A session with a TTY or a stdin travels over
// a connection the engine speaks both ways on, which net/http does not give
// for a reply whose body has no framing; every other session reads the reply.
func (d *Driver) runExec(ctx context.Context, execID string, e *execution, outW, errW *io.PipeWriter, req driver.ExecRequest) {
	defer close(e.done)
	fail := func(err error) {
		if cause := ctx.Err(); cause != nil {
			err = cause
		}
		e.err = err
		_ = outW.CloseWithError(err)
		_ = errW.CloseWithError(err)
	}
	if req.TTY || req.Stdin != nil {
		conn, br, err := d.client().hijack(ctx, d.client().libpodPath("/exec/"+execID+"/start"), execStart{Tty: req.TTY})
		if err != nil {
			fail(fmt.Errorf("podman: starting the exec session: %w", err))
			return
		}
		defer func() { _ = conn.Close() }()
		if req.Stdin != nil {
			go func() {
				_, _ = io.Copy(conn, req.Stdin)
				// Half-closing tells the command its input ended. A connection
				// that cannot be half-closed leaves the command reading, which
				// is the gap a caller works around with a command that does not.
				if half, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = half.CloseWrite()
				}
			}()
		}
		d.pipeExec(ctx, execID, e, outW, errW, br, req.TTY, fail)
		return
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
	d.pipeExec(ctx, execID, e, outW, errW, resp.Body, false, fail)
}

// pipeExec reads one started session's stream into the caller's writers and
// then its exit code. Under a TTY the stream is raw and everything is stdout;
// without one it keeps podman's framing and the demultiplexer splits it.
func (d *Driver) pipeExec(ctx context.Context, execID string, e *execution, outW, errW *io.PipeWriter, stream io.Reader, tty bool, fail func(error)) {
	var err error
	if tty {
		_, err = io.Copy(outW, stream)
	} else {
		err = demux(stream, outW, errW)
	}
	if err != nil {
		fail(fmt.Errorf("podman: reading the exec stream: %w", err))
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
