// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"

	"latere.ai/x/cella/runtime"
)

// remoteExec is one command running on a worker: the two output sub-streams
// as readers, and the exit code as the operation's result.
type remoteExec struct {
	stream Stream
	// stdinErr is what the copy of the caller's stdin failed with, reported
	// by Wait so a command whose input never arrived says why rather than
	// returning an exit code that means nothing.
	stdinErr atomic.Pointer[error]
	closed   atomic.Bool
}

func (e *remoteExec) Stdout() io.Reader { return e.stream.Up(StreamStdout) }
func (e *remoteExec) Stderr() io.Reader { return e.stream.Up(StreamStderr) }

// Wait returns the command's exit code. A result that carries none is a
// worker that answered without running the command, which is a defect on the
// far side and never a zero exit.
func (e *remoteExec) Wait(ctx context.Context) (int, error) {
	res, err := e.stream.Result(ctx)
	if err != nil {
		if stdinErr := e.stdinErr.Load(); stdinErr != nil {
			return 0, errors.Join(err, *stdinErr)
		}
		return 0, err
	}
	if res.Exit == nil {
		return 0, errors.New("remote: the worker answered the command without an exit code")
	}
	return *res.Exit, nil
}

// Close ends the command on the worker. It is idempotent, because a caller
// that drained the outputs and waited still closes.
func (e *remoteExec) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	return e.stream.Close()
}

// remoteSession is one terminal on a worker: bytes both ways, the window as a
// control message, and the exit code as the operation's result.
type remoteSession struct {
	stream Stream
	in     io.WriteCloser
	out    io.Reader
	closed atomic.Bool
}

func (s *remoteSession) Read(p []byte) (int, error) { return s.out.Read(p) }

// Write types into the terminal. A session the caller closed refuses, because
// the terminal is gone and bytes sent into it would be dropped in silence.
func (s *remoteSession) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	return s.in.Write(p)
}

// Resize sets the window the process inside reads. A resize on a session that
// has ended is not an error: the terminal is gone and the caller has nothing
// to do about it.
func (s *remoteSession) Resize(cols, rows int) error {
	if s.closed.Load() {
		return nil
	}
	return s.stream.Resize(cols, rows)
}

func (s *remoteSession) Wait(ctx context.Context) (int, error) {
	res, err := s.stream.Result(ctx)
	if err != nil {
		return 0, err
	}
	if res.Exit == nil {
		return 0, errors.New("remote: the worker ended the session without an exit code")
	}
	return *res.Exit, nil
}

// Close ends the session. The input sub-stream is closed first, so a shell
// reading from it sees the end of its input before the operation is
// cancelled.
func (s *remoteSession) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	_ = s.in.Close()
	return s.stream.Close()
}

// streamReader is one sub-stream a caller reads to its end and closes, which
// ends the operation behind it. Logs with follow and a file read are both
// this shape: bytes until the caller stops wanting them.
type streamReader struct {
	stream Stream
	r      io.Reader
	closed atomic.Bool
}

func (s *streamReader) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *streamReader) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.stream.Close()
}

// assert the handles satisfy the contract they stand in for.
var (
	_ runtime.Exec    = (*remoteExec)(nil)
	_ runtime.Session = (*remoteSession)(nil)
	_ io.ReadCloser   = (*streamReader)(nil)
)
