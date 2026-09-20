// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"io"
)

// AttachRequest opens one interactive session inside a running sandbox. An
// empty Command runs the image's shell. Cols and Rows are the window the
// terminal starts with; zero leaves the driver's default.
type AttachRequest struct {
	Command []string
	Env     map[string]string
	Workdir string
	Cols    int
	Rows    int
}

// Session is one terminal inside a sandbox: Read is what it writes, Write is
// what a person types, Close ends it. Read reports the end of the session as
// io.EOF whatever end-of-terminal error the operating system underneath
// produced.
type Session interface {
	io.ReadWriteCloser

	// Resize sets the window the process inside reads and signals the change.
	Resize(cols, rows int) error

	// Wait returns the session's exit code, or the reason it produced none. A
	// context that ends first ends the wait, not the session.
	Wait(ctx context.Context) (int, error)
}

// Attacher is implemented by a driver if and only if it declares
// Capabilities.Attach; runtimetest checks both directions. A driver that
// declares it also accepts ExecRequest.Stdin and ExecRequest.TTY.
type Attacher interface {
	Attach(ctx context.Context, id string, req AttachRequest) (Session, error)
}
