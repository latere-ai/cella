// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"

	"latere.ai/x/cella/runtime"
)

// ErrNoWorker is every call on an environment no worker serves. It wraps the
// contract's ErrNotRunning, so a caller that asks a driver for a sandbox it
// cannot reach reads one answer whichever half of the seam is missing.
var ErrNoWorker = fmt.Errorf("%w: no worker holds this environment's stream", runtime.ErrNotRunning)

// Transport is the control plane's side of one environment's workers: what
// they declared, whether one is live, the operations they claim, and the
// state they report. The driver below is the runtime contract over it.
//
// Nothing here dials a worker. Every method is answered from the connection a
// worker opened, which is invariant 10 of design 001.
type Transport interface {
	// Registration is what the environment's workers declared, and false
	// when none ever has.
	Registration() (Registration, bool)
	// Live reports whether a worker has heartbeat inside the offline window.
	Live() bool
	// Open enqueues one operation and returns its live handle. A context
	// that ends cancels the operation on the worker.
	Open(ctx context.Context, opType string, req Request) (Stream, error)
	// Observed is the last state a worker reported for one sandbox, and
	// false where none has been reported.
	Observed(id string) (runtime.State, bool)
	// ObservedList is every state the workers reported, and false where the
	// control plane has not been told anything yet, which is when a caller
	// must ask the worker instead.
	ObservedList() ([]runtime.State, bool)
}

// Stream is one operation in flight: its sub-streams and its answer.
type Stream interface {
	// Down opens the writer of one sub-stream toward the worker. Closing it
	// writes the zero-length frame that ends the sub-stream.
	Down(stream byte) io.WriteCloser
	// Up is the reader of one sub-stream from the worker. It ends with
	// io.EOF at the sub-stream's zero-length frame.
	Up(stream byte) io.Reader
	// Resize sets an attached terminal's window.
	Resize(cols, rows int) error
	// Result waits for the operation's answer.
	Result(ctx context.Context) (Response, error)
	// Close cancels the operation on the worker and releases the streams. It
	// is safe on an operation that already answered.
	Close() error
}

// Options builds the driver of one environment.
type Options struct {
	// Environment is the env_ id this driver serves, for the messages it
	// writes.
	Environment string
	Transport   Transport
}

// Driver is the runtime contract of an environment a worker serves. Every
// call is one operation on the worker's stream, except the four a
// registration answers without waking anybody.
type Driver struct {
	environment string
	transport   Transport
}

// The interfaces the remote driver implements. It implements every optional
// one unconditionally and reports the worker's capabilities, so a caller
// gated on a capability is gated on what the worker actually provides rather
// than on what this type happens to embed.
var (
	_ runtime.Driver    = (*Driver)(nil)
	_ runtime.Attacher  = (*Driver)(nil)
	_ runtime.FileStore = (*Driver)(nil)
)

// New returns the driver of one worker environment.
func New(o Options) (*Driver, error) {
	if o.Transport == nil {
		return nil, errors.New("remote: a driver needs the transport its environment's workers connect on")
	}
	return &Driver{environment: o.Environment, transport: o.Transport}, nil
}

// Name is the driver of spec 004's table. The driver the worker actually runs
// is in the environment's status, recorded from the first registration.
func (d *Driver) Name() string { return DriverName }

// Isolation is what the worker declared. An environment nothing has
// registered on confines nothing, which is the honest answer until one does.
func (d *Driver) Isolation() string {
	if r, held := d.transport.Registration(); held {
		return r.Isolation
	}
	return runtime.IsolationNone
}

// Capabilities are the worker's own. An environment with no registration
// declares none, so every capability gate refuses before an operation is
// enqueued for nobody.
func (d *Driver) Capabilities() runtime.Capabilities {
	if r, held := d.transport.Registration(); held {
		return r.Capabilities
	}
	return runtime.Capabilities{}
}

// Preflight passes once a worker has registered. What the worker's own
// driver needs is the worker's preflight, which it runs before it registers
// at all.
func (d *Driver) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, held := d.transport.Registration(); !held {
		return fmt.Errorf("%w: no worker has registered on %s", runtime.ErrNotRunning, d.environment)
	}
	return nil
}

// Ready passes while a worker is inside the offline window.
func (d *Driver) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !d.transport.Live() {
		return ErrNoWorker
	}
	return nil
}

// call runs one operation that carries no sub-stream and returns its answer.
func (d *Driver) call(ctx context.Context, opType string, req Request) (Response, error) {
	stream, err := d.transport.Open(ctx, opType, req)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = stream.Close() }()
	return stream.Result(ctx)
}

func (d *Driver) Create(ctx context.Context, spec runtime.CreateSpec) (runtime.Ref, error) {
	if err := spec.CheckPrewarm(); err != nil {
		return runtime.Ref{}, err
	}
	// The token is carried beside the spec: CreateSpec never serializes it,
	// and the worker's driver is what projects it inside the sandbox.
	res, err := d.call(ctx, OpCreate, Request{Spec: &spec, Token: spec.Token})
	if err != nil {
		return runtime.Ref{}, err
	}
	if res.Ref == nil {
		return runtime.Ref{}, fmt.Errorf("%w: the worker created a sandbox and named none", runtime.ErrInvalid)
	}
	return *res.Ref, nil
}

func (d *Driver) Start(ctx context.Context, id string) error {
	_, err := d.call(ctx, OpStart, Request{ID: id})
	return err
}

func (d *Driver) Stop(ctx context.Context, id string) error {
	_, err := d.call(ctx, OpStop, Request{ID: id})
	return err
}

func (d *Driver) Delete(ctx context.Context, id string) error {
	_, err := d.call(ctx, OpDelete, Request{ID: id})
	return err
}

func (d *Driver) Touch(ctx context.Context, id string) error {
	_, err := d.call(ctx, OpTouch, Request{ID: id})
	return err
}

func (d *Driver) Update(ctx context.Context, id string, change runtime.Change) error {
	if _, err := change.Adoption(); err != nil {
		return err
	}
	_, err := d.call(ctx, OpUpdate, Request{ID: id, Change: &change})
	return err
}

// Inspect answers from the state the workers reported, and asks the worker
// where the control plane has been told nothing about this sandbox. Reading a
// sandbox therefore costs no operation on a worker holding a stream open and
// reporting, which is every worker inside its window.
func (d *Driver) Inspect(ctx context.Context, id string) (runtime.State, error) {
	if state, held := d.transport.Observed(id); held {
		return state, nil
	}
	res, err := d.call(ctx, OpInspect, Request{ID: id})
	if err != nil {
		return runtime.State{}, err
	}
	if res.State == nil {
		return runtime.State{}, runtime.ErrNotFound
	}
	return *res.State, nil
}

// List answers from the reported states where the control plane holds them,
// narrowed by the filter here rather than on the worker, and asks the worker
// for the whole environment otherwise.
func (d *Driver) List(ctx context.Context, f runtime.Filter) ([]runtime.State, error) {
	if states, held := d.transport.ObservedList(); held {
		out := make([]runtime.State, 0, len(states))
		for _, s := range states {
			if f.Selects(s) {
				out = append(out, s)
			}
		}
		return out, nil
	}
	res, err := d.call(ctx, OpList, Request{Filter: &f})
	if err != nil {
		return nil, err
	}
	out := make([]runtime.State, 0, len(res.States))
	for _, s := range res.States {
		if f.Selects(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Exec runs one command on the worker and relays its three sub-streams. The
// exit code is the operation's result, so a caller waits on the answer rather
// than on the output ending.
func (d *Driver) Exec(ctx context.Context, id string, req runtime.ExecRequest) (runtime.Exec, error) {
	wire := &ExecRequest{
		Command: req.Command, Env: req.Env, Workdir: req.Workdir,
		TTY: req.TTY, Stdin: req.Stdin != nil, Timeout: req.Timeout,
	}
	stream, err := d.transport.Open(ctx, OpExec, Request{ID: id, Exec: wire})
	if err != nil {
		return nil, err
	}
	e := &remoteExec{stream: stream}
	if req.Stdin != nil {
		// The copy runs until the caller's reader ends, then closes the
		// sub-stream, which is what tells the worker's driver that stdin is
		// done. A failed copy closes it too: a command waiting on input that
		// will never arrive would hang both sides.
		go func() {
			down := stream.Down(StreamStdin)
			_, copyErr := io.Copy(down, req.Stdin)
			_ = down.Close()
			if copyErr != nil {
				e.stdinErr.Store(&copyErr)
			}
		}()
	} else {
		_ = stream.Down(StreamStdin).Close()
	}
	return e, nil
}

// Logs relays the sandbox's own output on the stdout sub-stream. Closing the
// reader ends the operation, which is how a follow is stopped.
func (d *Driver) Logs(ctx context.Context, id string, req runtime.LogsRequest) (io.ReadCloser, error) {
	stream, err := d.transport.Open(ctx, OpLogs, Request{ID: id, Logs: &req})
	if err != nil {
		return nil, err
	}
	return &streamReader{stream: stream, r: stream.Up(StreamStdout)}, nil
}

// ExportTar writes the archive the worker produced. The result is awaited
// after the copy, so an archive that ended early because the driver refused
// carries that refusal rather than a short tar.
func (d *Driver) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	stream, err := d.transport.Open(ctx, OpExportTar, Request{ID: id, Paths: paths})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	if _, err = io.Copy(dst, stream.Up(StreamBytes)); err != nil {
		return err
	}
	_, err = stream.Result(ctx)
	return err
}

// ImportTar sends the archive down and waits for the worker to say it landed.
func (d *Driver) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	stream, err := d.transport.Open(ctx, OpImportTar, Request{ID: id, Dest: dest})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	down := stream.Down(StreamBytes)
	if _, err = io.Copy(down, src); err != nil {
		_ = down.Close()
		return err
	}
	if err = down.Close(); err != nil {
		return err
	}
	_, err = stream.Result(ctx)
	return err
}

// Attach opens one terminal on the worker: bytes down the stdin sub-stream,
// bytes up the stdout one, the window as a control message, and the exit code
// as the operation's result.
func (d *Driver) Attach(ctx context.Context, id string, req runtime.AttachRequest) (runtime.Session, error) {
	stream, err := d.transport.Open(ctx, OpAttach, Request{ID: id, Attach: &req})
	if err != nil {
		return nil, err
	}
	return &remoteSession{stream: stream, in: stream.Down(StreamStdin), out: stream.Up(StreamStdout)}, nil
}

func (d *Driver) Stat(ctx context.Context, id, path string) (runtime.FileInfo, error) {
	res, err := d.call(ctx, OpStat, Request{ID: id, Path: path})
	if err != nil {
		return runtime.FileInfo{}, err
	}
	if res.Info == nil {
		return runtime.FileInfo{}, runtime.ErrNotFound
	}
	return *res.Info, nil
}

func (d *Driver) ReadDir(ctx context.Context, id, path string) ([]runtime.FileInfo, error) {
	res, err := d.call(ctx, OpReadDir, Request{ID: id, Path: path})
	if err != nil {
		return nil, err
	}
	return res.Infos, nil
}

// Open streams one file. The entry is the operation's first answer and the
// bytes follow it, so a caller learns the size before it reads.
func (d *Driver) Open(ctx context.Context, id, path string) (io.ReadCloser, runtime.FileInfo, error) {
	stream, err := d.transport.Open(ctx, OpOpen, Request{ID: id, Path: path})
	if err != nil {
		return nil, runtime.FileInfo{}, err
	}
	res, err := stream.Result(ctx)
	if err != nil {
		_ = stream.Close()
		return nil, runtime.FileInfo{}, err
	}
	if res.Info == nil {
		_ = stream.Close()
		return nil, runtime.FileInfo{}, runtime.ErrNotFound
	}
	return &streamReader{stream: stream, r: stream.Up(StreamBytes)}, *res.Info, nil
}

// Write sends one file's body down and returns what the worker wrote.
func (d *Driver) Write(ctx context.Context, id string, req runtime.WriteRequest) (int64, error) {
	stream, err := d.transport.Open(ctx, OpWrite,
		Request{ID: id, Path: req.Path, Mode: req.Mode, MaxBytes: req.MaxBytes})
	if err != nil {
		return 0, err
	}
	defer func() { _ = stream.Close() }()
	down := stream.Down(StreamBytes)
	if req.Body != nil {
		// A body past the bound is the worker's refusal, not this side's:
		// the copy ends when the worker stops reading, and the result
		// carries ErrTooLarge with the file left whole.
		if _, err = io.Copy(down, req.Body); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			_ = down.Close()
			return 0, err
		}
	}
	if err = down.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return 0, err
	}
	res, err := stream.Result(ctx)
	if err != nil {
		return 0, err
	}
	return res.Written, nil
}

func (d *Driver) Mkdir(ctx context.Context, id, path string) error {
	_, err := d.call(ctx, OpMkdir, Request{ID: id, Path: path})
	return err
}

func (d *Driver) Remove(ctx context.Context, id, path string) error {
	_, err := d.call(ctx, OpRemove, Request{ID: id, Path: path})
	return err
}

func (d *Driver) Move(ctx context.Context, id, from, to string) error {
	_, err := d.call(ctx, OpMove, Request{ID: id, Path: from, To: to})
	return err
}
