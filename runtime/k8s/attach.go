// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"k8s.io/client-go/tools/remotecommand"

	driver "latere.ai/x/cella/runtime"
)

var _ driver.Attacher = (*Driver)(nil)

// errInvalidWindow refuses a terminal window the exec's size message cannot
// carry: each side is an unsigned 16-bit count.
var errInvalidWindow = fmt.Errorf("%w: a terminal window is positive and below 65536", driver.ErrInvalid)

// shell is what a session runs when the request names no command. The wrapper
// around every argv ends in exec "$@", so the image's PATH resolves it.
var shell = []string{"sh"}

// Attach opens one terminal in the sandbox's container: an exec with stdin and
// a TTY, the environment and the directory wrapped around the command as Exec
// wraps them, and the window carried by the executor's terminal size queue.
func (d *Driver) Attach(ctx context.Context, id string, req driver.AttachRequest) (driver.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Cols < 0 || req.Rows < 0 {
		return nil, errInvalidWindow
	}
	w := newWindow()
	if req.Cols > 0 && req.Rows > 0 {
		// The first size the executor reads is the one it sends before any
		// byte of the session, so the process starts with this window.
		if err := w.set(req.Cols, req.Rows); err != nil {
			return nil, err
		}
	}
	command := req.Command
	if len(command) == 0 {
		command = shell
	}
	if strings.TrimSpace(command[0]) == "" {
		return nil, fmt.Errorf("%w: the command's first word is blank", driver.ErrInvalid)
	}
	spec, err := d.runningSpec(ctx, id)
	if err != nil {
		return nil, err
	}
	inR, inW := io.Pipe()
	o := execOpts{argv: wrapped(spec, command, req.Env, req.Workdir), stdin: true, tty: true, window: w}
	e, err := d.launch(ctx, objectName(id), o, inR, 0, func() {
		// The executor copies stdin until its reader ends; a session whose
		// process exited ends the reader here rather than at the next
		// keystroke, and the size queue answers nil so its loop returns.
		_ = inR.Close()
		w.end()
	})
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	return &session{e: e, in: inW, window: w}, nil
}

// session is one attached terminal: reads are what it wrote, writes are what a
// person typed, and Close cancels the exec, which closes its connection.
type session struct {
	e      *execution
	in     *io.PipeWriter
	window *window
}

func (s *session) Read(p []byte) (int, error)  { return s.e.stdout.Read(p) }
func (s *session) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close ends the session for its caller: the exec's context is cancelled, so
// the connection to the API server closes and with it the exec's stdin and
// terminal, and both pipes close, so a Read or Write after it fails. What the
// process inside does when its terminal closes is the container runtime's;
// Stop deletes the Pod, which ends it either way.
func (s *session) Close() error {
	_ = s.in.Close()
	return s.e.Close()
}

// Resize queues the window for the executor, which sends it on the exec's
// resize stream. A window for a session that has ended is ErrNotRunning,
// which is what the session is.
func (s *session) Resize(cols, rows int) error { return s.window.set(cols, rows) }

func (s *session) Wait(ctx context.Context) (int, error) { return s.e.Wait(ctx) }

// window is a terminal size queue holding at most one size: a resize the
// executor has not read yet is replaced by the next, because only the newest
// window is the terminal's. It answers nil once the session has ended.
type window struct {
	mu    sync.Mutex
	sizes chan remotecommand.TerminalSize
	ended chan struct{}
	over  bool
}

func newWindow() *window {
	return &window{sizes: make(chan remotecommand.TerminalSize, 1), ended: make(chan struct{})}
}

// set replaces the pending size. The lock orders it against end and against
// another set, so the send below always finds the one slot free.
func (w *window) set(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 0xffff || rows > 0xffff {
		return errInvalidWindow
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over {
		return fmt.Errorf("%w: the session has ended", driver.ErrNotRunning)
	}
	select {
	case <-w.sizes:
	default:
	}
	w.sizes <- remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}
	return nil
}

// end closes the queue. Next answers nil from then on, which ends the
// executor's resize loop.
func (w *window) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.over {
		w.over = true
		close(w.ended)
	}
}

// Next is the executor's read: the next size, or nil once the session ended.
func (w *window) Next() *remotecommand.TerminalSize {
	select {
	case <-w.ended:
		return nil
	case size := <-w.sizes:
		return &size
	}
}
