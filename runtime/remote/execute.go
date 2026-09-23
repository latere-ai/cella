// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"latere.ai/x/cella/runtime"
)

// Sink is the worker's side of one operation's sub-streams: what it reads
// from the control plane and what it writes back. A worker implements it over
// the connection it opened; the conformance suite implements it over pipes.
type Sink interface {
	// Reader is a sub-stream coming down, ending in io.EOF at its
	// zero-length frame.
	Reader(stream byte) io.Reader
	// Writer is a sub-stream going up. Closing it writes the zero-length
	// frame that ends it.
	Writer(stream byte) io.WriteCloser
	// Resizes carries the windows a control message asked for. It is nil on
	// an operation that is not an attach.
	Resizes() <-chan [2]int
	// Accept says the driver call was created and the sub-streams are live,
	// or that the driver refused before any of them existed. An operation
	// that streams for its whole life calls it; one that answers at once
	// does not, because its answer is its acceptance.
	Accept(err error) error
	// Answer sends the operation's result. One operation answers once, so an
	// operation that must answer before it writes a sub-stream calls it
	// itself and the caller's later answer is the no-op it should be.
	Answer(res Response, err error) error
	// Event sends one change the driver observed, on the Watch operation.
	Event(e Event) error
}

// Execute runs one operation with the worker's own driver and returns its
// answer. It is the mirror of the Driver above: one function, one table, so
// an operation type added on one side fails to compile on the other.
//
// The error it returns is the driver's, to be carried across the seam by
// EncodeError. A type this worker does not know is ErrUnsupported, which is
// how a worker of an older release refuses work rather than dropping it.
func Execute(ctx context.Context, d runtime.Driver, opType string, req Request, sink Sink) (Response, error) {
	switch opType {
	case OpCreate:
		return create(ctx, d, req)
	case OpStart:
		return Response{}, d.Start(ctx, req.ID)
	case OpStop:
		return Response{}, d.Stop(ctx, req.ID)
	case OpDelete:
		return Response{}, d.Delete(ctx, req.ID)
	case OpTouch:
		return Response{}, d.Touch(ctx, req.ID)
	case OpUpdate:
		if req.Change == nil {
			return Response{}, fmt.Errorf("%w: an update carries a change", runtime.ErrInvalid)
		}
		return Response{}, d.Update(ctx, req.ID, *req.Change)
	case OpInspect:
		state, err := d.Inspect(ctx, req.ID)
		if err != nil {
			return Response{}, err
		}
		return Response{State: &state}, nil
	case OpList:
		var filter runtime.Filter
		if req.Filter != nil {
			filter = *req.Filter
		}
		states, err := d.List(ctx, filter)
		if err != nil {
			return Response{}, err
		}
		return Response{States: states}, nil
	case OpExec:
		return execute(ctx, d, req, sink)
	case OpLogs:
		return logs(ctx, d, req, sink)
	case OpExportTar:
		return Response{}, exportTar(ctx, d, req, sink)
	case OpImportTar:
		return Response{}, d.ImportTar(ctx, req.ID, req.Dest, sink.Reader(StreamBytes))
	case OpAttach:
		return attach(ctx, d, req, sink)
	case OpStat, OpReadDir, OpOpen, OpWrite, OpMkdir, OpRemove, OpMove:
		return files(ctx, d, opType, req, sink)
	case OpWatch:
		return watch(ctx, d, sink)
	}
	return Response{}, fmt.Errorf("%w: this worker does not run the operation %q", runtime.ErrUnsupported, opType)
}

func create(ctx context.Context, d runtime.Driver, req Request) (Response, error) {
	if req.Spec == nil {
		return Response{}, fmt.Errorf("%w: a create carries a spec", runtime.ErrInvalid)
	}
	spec := *req.Spec
	spec.Token = req.Token
	ref, err := d.Create(ctx, spec)
	if err != nil {
		return Response{}, err
	}
	return Response{Ref: &ref}, nil
}

// execute runs one command and relays its outputs while it runs. The exit
// code is the answer, so both outputs are drained before it is read: a driver
// that buffers its output would otherwise lose whatever had not been read
// when the command ended.
func execute(ctx context.Context, d runtime.Driver, req Request, sink Sink) (Response, error) {
	if req.Exec == nil {
		return Response{}, fmt.Errorf("%w: an exec carries a request", runtime.ErrInvalid)
	}
	call := runtime.ExecRequest{
		Command: req.Exec.Command, Env: req.Exec.Env, Workdir: req.Exec.Workdir,
		TTY: req.Exec.TTY, Timeout: req.Exec.Timeout,
	}
	if req.Exec.Stdin {
		call.Stdin = sink.Reader(StreamStdin)
	}
	session, err := d.Exec(ctx, req.ID, call)
	if acceptErr := sink.Accept(err); acceptErr != nil {
		return Response{}, acceptErr
	}
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = session.Close() }()
	stdout, stderr := sink.Writer(StreamStdout), sink.Writer(StreamStderr)
	done := make(chan error, 2)
	go func() { _, copyErr := io.Copy(stdout, session.Stdout()); done <- copyErr }()
	go func() { _, copyErr := io.Copy(stderr, session.Stderr()); done <- copyErr }()
	code, waitErr := session.Wait(ctx)
	copyErr := errors.Join(<-done, <-done)
	_ = stdout.Close()
	_ = stderr.Close()
	if waitErr != nil {
		return Response{}, waitErr
	}
	if copyErr != nil {
		return Response{}, copyErr
	}
	return Response{Exit: &code}, nil
}

func logs(ctx context.Context, d runtime.Driver, req Request, sink Sink) (Response, error) {
	var request runtime.LogsRequest
	if req.Logs != nil {
		request = *req.Logs
	}
	reader, err := d.Logs(ctx, req.ID, request)
	if acceptErr := sink.Accept(err); acceptErr != nil {
		return Response{}, acceptErr
	}
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = reader.Close() }()
	out := sink.Writer(StreamStdout)
	_, err = io.Copy(out, reader)
	_ = out.Close()
	// A follow the caller stopped wanting ends as a closed pipe on this
	// side, which is the operation ending and not a failure to report.
	if err != nil && !errors.Is(err, io.ErrClosedPipe) && ctx.Err() == nil {
		return Response{}, err
	}
	return Response{}, nil
}

func exportTar(ctx context.Context, d runtime.Driver, req Request, sink Sink) error {
	if err := sink.Accept(nil); err != nil {
		return err
	}
	out := sink.Writer(StreamBytes)
	err := d.ExportTar(ctx, req.ID, req.Paths, out)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// attach opens one terminal, relays it both ways, and applies every window
// the control plane asks for until the session ends.
func attach(ctx context.Context, d runtime.Driver, req Request, sink Sink) (Response, error) {
	attacher, ok := d.(runtime.Attacher)
	if !ok {
		_ = sink.Accept(runtime.ErrUnsupported)
		return Response{}, runtime.ErrUnsupported
	}
	var request runtime.AttachRequest
	if req.Attach != nil {
		request = *req.Attach
	}
	session, err := attacher.Attach(ctx, req.ID, request)
	if acceptErr := sink.Accept(err); acceptErr != nil {
		return Response{}, acceptErr
	}
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = session.Close() }()
	out := sink.Writer(StreamStdout)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, session)
		_ = out.Close()
	}()
	// What a person types, and the windows the terminal is resized to, both
	// arrive on this connection and both belong to this session.
	input := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(session, sink.Reader(StreamStdin))
		input <- copyErr
	}()
	resizes := sink.Resizes()
	waited := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, waitErr := session.Wait(ctx)
		waited <- struct {
			code int
			err  error
		}{code, waitErr}
	}()
	for {
		select {
		case window, open := <-resizes:
			if !open {
				resizes = nil
				continue
			}
			// A window that arrives as the session ends is moot: the
			// session's own end reports how it went, not the resize.
			if resizeErr := session.Resize(window[0], window[1]); resizeErr != nil && !errors.Is(resizeErr, runtime.ErrNotRunning) {
				return Response{}, resizeErr
			}
		case <-input:
			input = nil
		case end := <-waited:
			<-copied
			if end.err != nil {
				return Response{}, end.err
			}
			code := end.code
			return Response{Exit: &code}, nil
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
}

// files runs the per-file half of the capability. A driver that declares no
// Files has no FileStore, which crosses the seam as ErrUnsupported and is
// what the API reports as the capability the environment lacks.
func files(ctx context.Context, d runtime.Driver, opType string, req Request, sink Sink) (Response, error) {
	store, ok := d.(runtime.FileStore)
	if !ok {
		return Response{}, runtime.ErrUnsupported
	}
	switch opType {
	case OpStat:
		info, err := store.Stat(ctx, req.ID, req.Path)
		if err != nil {
			return Response{}, err
		}
		return Response{Info: &info}, nil
	case OpReadDir:
		infos, err := store.ReadDir(ctx, req.ID, req.Path)
		if err != nil {
			return Response{}, err
		}
		return Response{Infos: infos}, nil
	case OpMkdir:
		return Response{}, store.Mkdir(ctx, req.ID, req.Path)
	case OpRemove:
		return Response{}, store.Remove(ctx, req.ID, req.Path)
	case OpMove:
		return Response{}, store.Move(ctx, req.ID, req.Path, req.To)
	case OpWrite:
		written, err := store.Write(ctx, req.ID, runtime.WriteRequest{
			Path: req.Path, Mode: req.Mode, MaxBytes: req.MaxBytes, Body: sink.Reader(StreamBytes),
		})
		if err != nil {
			return Response{}, err
		}
		return Response{Written: written}, nil
	case OpOpen:
		return open(ctx, store, req, sink)
	}
	return Response{}, fmt.Errorf("%w: this worker does not run the operation %q", runtime.ErrUnsupported, opType)
}

// open answers with the entry and then streams the file. The entry is the
// result rather than a frame of its own, so a caller learns the size and the
// mode before the first byte and a file that cannot be opened costs no
// sub-stream at all.
//
// The answer is sent here rather than by the caller, and before the copy
// starts, because the far side waits for the result before it reads the
// bytes. On a connection without credit one sub-stream's bytes can hold the
// whole connection, and a result queued behind bytes nobody is draining yet
// would never arrive. The caller's later answer is the no-op one operation
// answering once makes it.
//
// The copy runs inside the operation rather than after it returned. The
// credit for its bytes arrives on the operation's channel, and a cancel from
// the caller who stopped reading does too, and both reach only an operation
// still running.
func open(ctx context.Context, store runtime.FileStore, req Request, sink Sink) (Response, error) {
	reader, info, err := store.Open(ctx, req.ID, req.Path)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = reader.Close() }()
	answer := Response{Info: &info}
	if sendErr := sink.Answer(answer, nil); sendErr != nil {
		return Response{}, sendErr
	}
	out := sink.Writer(StreamBytes)
	// The answer is already sent, so a copy that ends early is what the
	// caller reads against the size the entry reported.
	_, _ = io.Copy(out, reader)
	_ = out.Close()
	return answer, nil
}

// rewatchDelay is how long a worker waits before it watches again after its
// driver's channel closed, so a driver that closes at once does not turn the
// watch into a loop.
const rewatchDelay = time.Second

// watch relays what the driver observes for as long as the connection lasts.
// A channel the driver closes is followed by a relist, which is design 004's
// rule: the worker watches again first and sends the relist after, so the
// List the control plane issues on it covers everything the gap missed.
func watch(ctx context.Context, d runtime.Driver, sink Sink) (Response, error) {
	watcher, ok := d.(Watcher)
	if !ok {
		if err := sink.Accept(runtime.ErrUnsupported); err != nil {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("%w: this worker's driver does not watch", runtime.ErrUnsupported)
	}
	events, err := watcher.Watch(ctx)
	if acceptErr := sink.Accept(err); acceptErr != nil {
		return Response{}, acceptErr
	}
	if err != nil {
		return Response{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case e, open := <-events:
			if open {
				if err = sink.Event(e); err != nil {
					return Response{}, err
				}
				continue
			}
			select {
			case <-ctx.Done():
				return Response{}, ctx.Err()
			case <-time.After(rewatchDelay):
			}
			if events, err = watcher.Watch(ctx); err != nil {
				return Response{}, err
			}
			if err = sink.Event(Event{Type: EventRelist}); err != nil {
				return Response{}, err
			}
		}
	}
}
