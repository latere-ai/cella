// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package remote is the driver of an environment a worker serves, and the
// vocabulary the control plane and that worker speak.
//
// Every driver call becomes one operation the control plane enqueues and a
// worker claims, executes with its own driver, and answers. The worker opens
// the connection and the control plane never dials it, which is invariant 10
// of design 001. This package holds both halves of the wire so the two sides
// cannot drift: the frames, the request and response shapes, the error
// vocabulary, and Execute, which turns one operation back into a driver call.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// Protocol is the version of the vocabulary below. It is the WebSocket
// subprotocol the worker asks for, so a control plane and a worker of
// different releases refuse one another rather than half understand.
const Protocol = "cella.worker.v1"

// DriverName is what an environment a worker serves reports as its driver to
// a caller of the runtime contract. The driver the worker actually runs is in
// the environment's status, recorded from the first registration.
const DriverName = "remote"

// The bounds of the stream. A frame is at most MaxFrameBytes, each side sends
// a heartbeat every HeartbeatInterval, and a connection with none for
// HeartbeatTimeout is closed by either side, which is the lease after which a
// worker's claims are redelivered.
const (
	MaxFrameBytes     = 1 << 20
	HeartbeatInterval = 15 * time.Second
	HeartbeatTimeout  = 45 * time.Second
)

// OperationIDLen is the fixed width of the operation id every frame opens
// with: a ULID in Crockford base32.
const OperationIDLen = 26

// The sub-streams one operation's frames carry. Control is JSON; the rest are
// bytes, and a zero-length frame closes the sub-stream it names.
const (
	StreamControl byte = 0
	StreamStdin   byte = 1
	StreamStdout  byte = 2
	StreamStderr  byte = 3
	StreamBytes   byte = 4
)

// NoOperation is the id a frame that belongs to no operation carries: the
// hello, the heartbeats, and the observed state a worker reports.
const NoOperation = "00000000000000000000000000"

// The control messages. Direction is fixed per type, and a side that receives
// one belonging the other way closes the connection.
const (
	// MessageHello is the worker's first frame: which registration this
	// connection is.
	MessageHello = "hello"
	// MessageHeartbeat keeps an idle stream alive, both ways.
	MessageHeartbeat = "heartbeat"
	// MessageOperation is one driver call, down.
	MessageOperation = "operation"
	// MessageResult is that call's answer, up.
	MessageResult = "result"
	// MessageCancel says the caller went away, down. The worker cancels the
	// operation's context.
	MessageCancel = "cancel"
	// MessageResize sets an attached terminal's window, down.
	MessageResize = "resize"
	// MessageState is what the worker's driver observes, up. It is how the
	// control plane reads a sandbox without waking the worker.
	MessageState = "state"
)

// The operation types. Each is a Driver or optional-interface method name, so
// a reader of a frame and a reader of the contract see the same word.
const (
	OpCreate    = "Create"
	OpStart     = "Start"
	OpStop      = "Stop"
	OpDelete    = "Delete"
	OpUpdate    = "Update"
	OpInspect   = "Inspect"
	OpList      = "List"
	OpExec      = "Exec"
	OpLogs      = "Logs"
	OpExportTar = "ExportTar"
	OpImportTar = "ImportTar"
	OpTouch     = "Touch"
	OpAttach    = "Attach"
	OpStat      = "Stat"
	OpReadDir   = "ReadDir"
	OpOpen      = "Open"
	OpWrite     = "Write"
	OpMkdir     = "Mkdir"
	OpRemove    = "Remove"
	OpMove      = "Move"
)

// Registration is what a worker declares about itself and the driver it runs.
// The control plane records the driver name from the first registration and
// refuses a later worker that reports another.
type Registration struct {
	Worker       string               `json:"worker,omitempty"`
	Driver       string               `json:"driver"`
	Isolation    string               `json:"isolation"`
	Capabilities runtime.Capabilities `json:"capabilities,omitzero"`
	Capacity     v1.Capacity          `json:"capacity,omitzero"`
	Labels       map[string]string    `json:"labels,omitempty"`
	Version      string               `json:"version,omitempty"`
}

// Registered is what the control plane answers a registration with: the id
// this worker claims under, and the two timings it keeps to.
type Registered struct {
	Worker            string      `json:"worker"`
	HeartbeatInterval v1.Duration `json:"heartbeatInterval"`
	Lease             v1.Duration `json:"lease"`
	Environment       string      `json:"environment,omitempty"`
}

// Request is one operation's arguments: the method's, less the context, with
// every reader and writer replaced by a sub-stream. One type carries them all
// because the type name is in the operation and a field a method does not
// take is absent.
type Request struct {
	// ID is the sandbox the call is about, empty on List.
	ID     string                 `json:"id,omitempty"`
	Spec   *runtime.CreateSpec    `json:"spec,omitempty"`
	Change *runtime.Change        `json:"change,omitempty"`
	Filter *runtime.Filter        `json:"filter,omitempty"`
	Exec   *ExecRequest           `json:"exec,omitempty"`
	Attach *runtime.AttachRequest `json:"attach,omitempty"`
	Logs   *runtime.LogsRequest   `json:"logs,omitempty"`
	// Token is the workload token the worker's driver projects. CreateSpec
	// never serializes it, because a driver that keeps the spec beside the
	// sandbox must not keep the credential; on this wire it travels once,
	// on the one call that needs it.
	Token []byte `json:"token,omitempty"`
	// Paths, Path, Dest and To are the file operations' arguments.
	Paths    []string    `json:"paths,omitempty"`
	Path     string      `json:"path,omitempty"`
	Dest     string      `json:"dest,omitempty"`
	To       string      `json:"to,omitempty"`
	Mode     fs.FileMode `json:"mode,omitempty"`
	MaxBytes int64       `json:"maxBytes,omitempty"`
}

// ExecRequest is runtime.ExecRequest with the reader replaced by a flag: the
// bytes travel on the stdin sub-stream and the flag says whether any will.
type ExecRequest struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	TTY     bool              `json:"tty,omitempty"`
	Stdin   bool              `json:"stdin,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// Response is one operation's non-stream returns. A field a method does not
// return is absent.
type Response struct {
	Ref     *runtime.Ref       `json:"ref,omitempty"`
	State   *runtime.State     `json:"state,omitempty"`
	States  []runtime.State    `json:"states,omitempty"`
	Info    *runtime.FileInfo  `json:"info,omitempty"`
	Infos   []runtime.FileInfo `json:"infos,omitempty"`
	Exit    *int               `json:"exit,omitempty"`
	Written int64              `json:"written,omitempty"`
}

// Error carries a driver's refusal across the seam in the driver's own
// vocabulary, so a caller on the control plane reads the same sentinel it
// would from a driver in the same process.
type Error struct {
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
}

// The kinds an Error takes, one per sentinel of the runtime contract. A
// refusal that is none of them crosses as its message alone.
const (
	KindNotFound      = "notFound"
	KindAlreadyExists = "alreadyExists"
	KindNotRunning    = "notRunning"
	KindUnsupported   = "unsupported"
	KindInvalid       = "invalid"
	KindTooLarge      = "tooLarge"
)

// sentinels maps each kind to the error the contract declares. It is the one
// table both directions read, so an error that crosses and comes back is the
// error it started as.
var sentinels = map[string]error{
	KindNotFound:      runtime.ErrNotFound,
	KindAlreadyExists: runtime.ErrAlreadyExists,
	KindNotRunning:    runtime.ErrNotRunning,
	KindUnsupported:   runtime.ErrUnsupported,
	KindInvalid:       runtime.ErrInvalid,
	KindTooLarge:      runtime.ErrTooLarge,
}

// EncodeError renders one driver error for the wire. A nil error is nil.
func EncodeError(err error) *Error {
	if err == nil {
		return nil
	}
	out := &Error{Message: err.Error()}
	for kind, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			out.Kind = kind
			break
		}
	}
	return out
}

// Err rebuilds the driver error one side sent, wrapping the contract's
// sentinel so errors.Is holds on the other side.
func (e *Error) Err() error {
	if e == nil {
		return nil
	}
	message := e.Message
	if message == "" {
		message = "the worker refused the operation"
	}
	if sentinel, held := sentinels[e.Kind]; held {
		// The message already opens with the sentinel's text where the
		// driver wrapped it; repeating it would read twice.
		if wrapped := fmt.Errorf("%w", sentinel); wrapped.Error() == message {
			return sentinel
		}
		return fmt.Errorf("%s (%w)", message, sentinel)
	}
	return errors.New(message)
}

// Message is one control frame. The type names which fields carry the
// payload; every other field is absent.
type Message struct {
	Type string `json:"type"`
	// Worker is the registration a hello names.
	Worker string `json:"worker,omitempty"`
	// Operation and Request are one call, down; Response, OK and Error its
	// answer, up.
	Operation string    `json:"operation,omitempty"`
	Request   *Request  `json:"request,omitempty"`
	OK        bool      `json:"ok,omitempty"`
	Response  *Response `json:"response,omitempty"`
	Error     *Error    `json:"error,omitempty"`
	// Cols and Rows are a resize.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// States is what the worker's driver observes, and Relist asks the
	// control plane to read the whole environment again.
	States []runtime.State `json:"states,omitempty"`
	Relist bool            `json:"relist,omitempty"`
}

// ErrFrame is a frame this protocol cannot read. Every one of them closes the
// connection: a side that cannot parse a frame cannot know what it missed.
var ErrFrame = errors.New("remote: the frame is not one this protocol carries")

// EncodeFrame writes one frame: the operation id, the sub-stream, and the
// payload. An id of another width, or a payload past the bound, is ErrFrame,
// because a frame that cannot be written is a defect on this side and never
// something to send half of.
func EncodeFrame(operation string, stream byte, payload []byte) ([]byte, error) {
	if len(operation) != OperationIDLen {
		return nil, fmt.Errorf("%w: an operation id is %d bytes, not %d", ErrFrame, OperationIDLen, len(operation))
	}
	if len(payload) > MaxFrameBytes {
		return nil, fmt.Errorf("%w: a payload is at most %d bytes", ErrFrame, MaxFrameBytes)
	}
	raw := make([]byte, 0, OperationIDLen+1+len(payload))
	raw = append(raw, operation...)
	raw = append(raw, stream)
	return append(raw, payload...), nil
}

// DecodeFrame reads one frame back. The payload aliases raw, so a caller that
// keeps it past the read copies it.
func DecodeFrame(raw []byte) (operation string, stream byte, payload []byte, err error) {
	if len(raw) < OperationIDLen+1 {
		return "", 0, nil, fmt.Errorf("%w: a frame is at least %d bytes", ErrFrame, OperationIDLen+1)
	}
	if len(raw) > MaxFrameBytes+OperationIDLen+1 {
		return "", 0, nil, fmt.Errorf("%w: a frame is at most %d bytes", ErrFrame, MaxFrameBytes)
	}
	return string(raw[:OperationIDLen]), raw[OperationIDLen], raw[OperationIDLen+1:], nil
}

// EncodeMessage writes one control frame.
func EncodeMessage(operation string, m Message) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFrame, err)
	}
	return EncodeFrame(operation, StreamControl, payload)
}

// DecodeMessage reads one control frame's message.
func DecodeMessage(payload []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return Message{}, fmt.Errorf("%w: %w", ErrFrame, err)
	}
	if m.Type == "" {
		return Message{}, fmt.Errorf("%w: a control message names its type", ErrFrame)
	}
	return m, nil
}

// Streaming reports whether an operation carries a sub-stream for its whole
// life. Such an operation is never redelivered: a caller's exec that was
// halfway through a worker that vanished has lost output nobody can
// reconstruct, and running the command again would run it twice.
func Streaming(opType string) bool {
	switch opType {
	case OpExec, OpAttach, OpLogs, OpExportTar, OpImportTar, OpOpen, OpWrite:
		return true
	}
	return false
}
