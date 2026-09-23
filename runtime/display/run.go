// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package display

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrNotReady is an operation on a desktop that has not come up. Package
// runtime re-exports it as ErrDisplayNotReady.
var ErrNotReady = errors.New("the sandbox's desktop is not ready: DisplayReady is false")

// InputFailure is the event a batch stopped at. A driver returns it when part
// of a batch ran, so a caller learns how much of its gesture landed rather
// than a failure that says nothing about the desktop's state.
type InputFailure struct {
	// Index is the event that failed and Executed is how many ran before it.
	Index    int
	Executed int
	Err      error
}

func (e *InputFailure) Error() string {
	return fmt.Sprintf("the desktop refused event %d: %v", e.Index, e.Err)
}
func (e *InputFailure) Unwrap() error { return e.Err }

// Runner runs one command inside a sandbox to its end and writes what it
// printed to stdout. A command that exits nonzero is an error. Each driver
// supplies its own, so the desktop's behavior lives here once and the way
// into the sandbox stays the driver's.
type Runner func(ctx context.Context, argv []string, stdout io.Writer) error

// Opener starts one command inside a sandbox and hands back its output as it
// is produced. Closing the reader, or ending the context, ends the command's
// stream.
type Opener func(ctx context.Context, argv []string) (io.ReadCloser, error)

// Shell is a script as the argument vector a POSIX shell takes.
func Shell(script string) []string { return []string{"sh", "-c", script} }

// Ready reports whether the desktop is up, as ErrNotReady where it is not, so
// every driver answers an operation on a desktop that has not come up with one
// error a caller can test for.
func Ready(ctx context.Context, run Runner) error {
	if err := run(ctx, Shell(ReadyScript()), io.Discard); err != nil {
		return fmt.Errorf("%w: %w", ErrNotReady, err)
	}
	return nil
}

// Install writes the supervisor into the sandbox and starts it, once.
func Install(ctx context.Context, run Runner, g Geometry) error {
	if err := run(ctx, Shell(InstallScript(g)), io.Discard); err != nil {
		return fmt.Errorf("starting the desktop: %w", err)
	}
	return nil
}

// Capture is one frame of the desktop, encoded as the request asks.
func Capture(ctx context.Context, run Runner, req ScreenshotRequest) ([]byte, error) {
	req, err := req.Normalize()
	if err != nil {
		return nil, err
	}
	var frame bytes.Buffer
	if err = run(ctx, Shell(CaptureScript(req)), &frame); err != nil {
		return nil, fmt.Errorf("capturing the screen: %w", err)
	}
	if frame.Len() == 0 {
		return nil, errors.New("the capture produced no frame")
	}
	return frame.Bytes(), nil
}

// Perform runs a validated batch in order and stops at the first event the
// desktop refuses. A wait is the control plane's own pause: it runs no command
// inside the sandbox, so a batch that pauses costs the sandbox nothing.
func Perform(ctx context.Context, run Runner, events []InputEvent) error {
	for i, e := range events {
		if e.Type == TypeWait {
			timer := time.NewTimer(time.Duration(e.Ms) * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return &InputFailure{Index: i, Executed: i, Err: ctx.Err()}
			}
			continue
		}
		for _, argv := range Argv(e) {
			if err := run(ctx, argv, io.Discard); err != nil {
				return &InputFailure{Index: i, Executed: i, Err: err}
			}
		}
	}
	return nil
}

// PortsListening is the ports of the kernel's socket table inside the sandbox.
func PortsListening(ctx context.Context, run Runner) (map[int]bool, error) {
	var table bytes.Buffer
	if err := run(ctx, Shell(PortsScript()), &table); err != nil {
		return nil, fmt.Errorf("reading the socket table: %w", err)
	}
	return Listening(table.String()), nil
}

// NewSession is one screen session's name, which is a file name inside the
// sandbox, so it carries no byte a path cannot.
func NewSession() string { return strings.ToLower(rand.Text()[:16]) }

// Frames reads length-prefixed frames off one screen session and sends them
// on a channel with room for one. A frame that finds the channel full is
// dropped rather than queued, which is the rule spec 023 states: a consumer
// behind by one frame loses the frame that arrived while it was behind, and
// the session never builds a backlog of frames nobody will look at.
//
// The channel closes when the stream ends, which is the session ending, the
// sandbox stopping or the context being cancelled. end is the driver's own
// clean-up, run once the stream is done.
func Frames(ctx context.Context, src io.ReadCloser, format string, end func()) <-chan Frame {
	out := make(chan Frame, 1)
	go func() {
		defer close(out)
		defer func() {
			_ = src.Close()
			if end != nil {
				end()
			}
		}()
		for {
			data, err := ReadFrame(src)
			if err != nil {
				return
			}
			select {
			case out <- Frame{At: time.Now().UTC(), Format: format, Data: data}:
			default:
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return out
}
