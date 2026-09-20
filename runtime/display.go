// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"

	"latere.ai/x/cella/runtime/display"
)

// The desktop vocabulary of spec 023, aliased so a caller of this contract
// names one type and a driver imports one package. The declarations, the
// rules a batch is held to, and the commands an event becomes are in
// runtime/display.
type (
	Geometry          = display.Geometry
	ScreenshotRequest = display.ScreenshotRequest
	Frame             = display.Frame
	InputEvent        = display.InputEvent
)

// ErrDisplayNotReady is an operation on a sandbox whose desktop has not come
// up. It is not a bring-up: the desktop is started by the lifecycle, and a
// caller that arrives before it is ready is told which condition it is
// waiting on rather than made to wait.
var ErrDisplayNotReady = errors.New("the sandbox's desktop is not ready: DisplayReady is false")

// DisplayDriver is implemented by a driver if and only if it declares
// Capabilities.Display; runtimetest checks both directions.
//
// Display answers the geometry the sandbox was created with, and ErrNotFound
// for a sandbox that asked for no desktop, so a caller tells an environment
// that cannot provide a screen from a sandbox that did not ask for one.
// Screenshot is one frame. Screen is a paced sequence of them: the channel
// has room for one frame and a frame that finds it full replaces nothing and
// is dropped, so a consumer that has fallen behind loses the newest frame and
// never queues. Cancelling the context ends the session and closes the
// channel; so does the sandbox stopping.
type DisplayDriver interface {
	Display(ctx context.Context, id string) (Geometry, error)
	Screenshot(ctx context.Context, id string, req ScreenshotRequest) (io.ReadCloser, error)
	Screen(ctx context.Context, id string, fps int, format string) (<-chan Frame, error)
}

// InputDriver is implemented by a driver if and only if it declares
// Capabilities.Input. The batch is validated whole before its first event
// runs, and execution stops at the first event the desktop refuses: the
// error names the index, so a caller knows how much of its gesture landed.
type InputDriver interface {
	Input(ctx context.Context, id string, events []InputEvent) error
}

// Dialer is implemented by a driver if and only if it declares
// Capabilities.Dial: one connection to one port inside the sandbox. The port
// is the sandbox's own, resolved by the caller against the sandbox's declared
// ports, so no address a request carried ever reaches a driver.
type Dialer interface {
	Dial(ctx context.Context, id string, port int) (net.Conn, error)
}

// InputFailure is the event a batch stopped at. A driver returns it when part
// of a batch ran, so the API answers with how much landed and where it
// stopped rather than with a failure that says nothing about the desktop's
// state.
type InputFailure struct {
	// Index is the event that failed and Executed is how many ran before it.
	Index    int
	Executed int
	Err      error
}

func (e *InputFailure) Error() string {
	return "the desktop refused event " + strconv.Itoa(e.Index) + ": " + e.Err.Error()
}
func (e *InputFailure) Unwrap() error { return e.Err }

// The ways a port may be reached from outside the sandbox (spec 023).
const (
	ExposeNone   = "none"
	ExposeMesh   = "mesh"
	ExposePublic = "public"
)

// Port is one port the manifest declares runs inside the sandbox.
type Port struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Expose string `json:"expose,omitempty"`
}

// The states a declared port is in, as the driver's probe finds it.
const (
	PortListening = "listening"
	PortClosed    = "closed"
)

// PortState is one declared port and whether anything inside the sandbox is
// listening on it. URL is set only where an Exposer gave a public port an
// endpoint.
type PortState struct {
	Name  string `json:"name"`
	Port  int    `json:"port"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}
