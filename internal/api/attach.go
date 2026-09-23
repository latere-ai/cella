// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/metrics"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// subprotocol is the one design 008 names for the exec and attach sockets. A
// client that offers it gets it back in the handshake; one that offers nothing
// still connects, because the frames carry their own shape.
const subprotocol = "cella.exec.v1"

const (
	// sessionFrameBytes bounds one frame of what a person types, matching the
	// exec stream's own frame cap.
	sessionFrameBytes = 1 << 20
	// pingInterval is how often the server asks whether the client is there,
	// and idleTimeout how long it waits for any frame or pong before dropping
	// the connection. They are fixed: no design names an idle window, so the
	// connection carries a liveness check and no policy about typing speed.
	pingInterval = 30 * time.Second
	idleTimeout  = 90 * time.Second
	// writeTimeout bounds one frame reaching a client that stopped reading.
	writeTimeout = 10 * time.Second
	// waitTimeout bounds reading the exit code once the session's output ended.
	waitTimeout = 30 * time.Second
	// maxExecTimeout is the longest a socket-run command may take, as the
	// synchronous exec route bounds its own.
	maxExecTimeout = time.Hour
)

var upgrader = websocket.Upgrader{
	Subprotocols: []string{subprotocol},
	// The origin is not the boundary here: every frame travels under a bearer
	// the handshake already verified, and a console served from another origin
	// is the ordinary case.
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 10 * time.Second,
}

// socketRequest is the first text frame: what to run and how big its terminal
// is. An empty command runs the image's shell.
type socketRequest struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// clientFrame is a text frame after the first: the client resizes and nothing
// else, and a frame without a resize is ignored rather than refused.
type clientFrame struct {
	Resize *struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"resize"`
}

// exitFrame and errorFrame are the last text frames the server writes, each
// followed by a close.
type exitFrame struct {
	Exit int `json:"exit"`
}
type errorFrame struct {
	Error httpjson.Error `json:"error"`
}

// attachSocket serves GET /v1/sandboxes/{id}/attach: always a terminal.
func (h *handler) attachSocket(w http.ResponseWriter, r *http.Request) {
	h.socket(w, r, true)
}

// execSocket serves GET /v1/sandboxes/{id}/exec: a terminal when the request
// gives both a column and a row count, a command with stdin otherwise.
func (h *handler) execSocket(w http.ResponseWriter, r *http.Request) {
	h.socket(w, r, false)
}

// socket authorizes, gates on the capability, and only then upgrades: a
// refusal before the upgrade is an HTTP error envelope with its status, which
// a close code could no longer carry.
func (h *handler) socket(w http.ResponseWriter, r *http.Request, terminal bool) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return
	}
	if !h.Controller.CapabilitiesOf(obj.Status.Environment).Attach {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "the environment provides no terminal session"})
		return
	}
	// The request id is read before the upgrade: once the connection is the
	// client's, the response header is no longer reachable.
	requestID := w.Header().Get(RequestIDHeader)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has written the reply, so the caller already has the reason.
		return
	}
	defer func() { _ = conn.Close() }()
	h.touch(r, obj)
	h.session(r, conn, obj, terminal, requestID)
}

// session reads the request frame, opens the stream and drives it until one
// side ends.
func (h *handler) session(r *http.Request, conn *websocket.Conn, obj v1.Sandbox, terminal bool, requestID string) {
	writer := &frameWriter{conn: conn}
	req, err := readRequest(conn, h.MaxBodyBytes)
	if err != nil {
		closeWith(writer, websocket.ClosePolicyViolation, err.Error())
		return
	}
	stream, err := h.open(r.Context(), obj, req, terminal)
	if err != nil {
		writer.fail(err, requestID)
		return
	}
	defer func() { _ = stream.Close() }()
	h.drive(r, conn, writer, stream, obj, requestID)
}

// readRequest reads the first frame, which design 008 fixes as one text frame
// carrying the JSON request. The read limit is the body cap of every other
// route; a frame past it ends the connection with the frame limit's own code.
func readRequest(conn *websocket.Conn, limit int64) (socketRequest, error) {
	var req socketRequest
	conn.SetReadLimit(limit)
	if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
		return req, err
	}
	kind, data, err := conn.ReadMessage()
	if err != nil {
		return req, err
	}
	if kind != websocket.TextMessage {
		return req, errors.New("the first frame is the JSON request, as text")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		return req, err
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return req, errors.New("the first frame carries one JSON object")
	}
	return req, nil
}

// open turns the request into a running stream. A terminal is the Attach path;
// a command with stdin is the Exec path, whose two output streams interleave
// into the one byte stream design 008 states.
func (h *handler) open(ctx context.Context, obj v1.Sandbox, req socketRequest, terminal bool) (runtime.Session, error) {
	if req.Cols < 0 || req.Rows < 0 || req.Cols > 0xffff || req.Rows > 0xffff {
		return nil, &manifest.Error{Code: "invalid_field", Detail: "cols and rows are between 0 and 65535"}
	}
	// A terminal is what /attach always opens and what /exec opens when the
	// request gives both a column and a row count; otherwise the request runs
	// a command whose input the client writes.
	withTerminal := terminal || req.Cols > 0 && req.Rows > 0
	if len(req.Command) == 0 && !withTerminal {
		return nil, &manifest.Error{Code: "invalid_field", Detail: "command must not be empty"}
	}
	// ValidateExec requires a command; a session without one runs the image's
	// shell, so a stand-in carries the check and only env and workdir bind.
	command := req.Command
	if len(command) == 0 {
		command = []string{"sh"}
	}
	if err := manifest.ValidateExec(command, req.Env, req.Workdir); err != nil {
		return nil, err
	}
	if withTerminal {
		return h.Controller.Attach(ctx, obj.Status.ID, runtime.AttachRequest{
			Command: req.Command, Env: req.Env, Workdir: req.Workdir, Cols: req.Cols, Rows: req.Rows,
		})
	}
	timeout := 10 * time.Minute
	if req.Timeout != "" {
		parsed, err := time.ParseDuration(req.Timeout)
		if err != nil || parsed <= 0 || parsed > maxExecTimeout {
			return nil, &manifest.Error{Code: "invalid_field", Detail: "timeout must be positive and at most 1h"}
		}
		timeout = parsed
	}
	in, stdin := io.Pipe()
	running, err := h.Controller.Exec(ctx, obj.Status.ID, runtime.ExecRequest{
		Command: req.Command, Env: req.Env, Workdir: req.Workdir, Stdin: in, Timeout: timeout,
	})
	if err != nil {
		_ = stdin.Close()
		_ = in.Close()
		return nil, err
	}
	return newExecStream(running, stdin), nil
}

// execStream is a command without a terminal read as one byte stream: its two
// outputs interleave, and what the client types reaches its input.
type execStream struct {
	running runtime.Exec
	out     *io.PipeReader
	in      *io.PipeWriter
	once    sync.Once
}

func newExecStream(running runtime.Exec, stdin *io.PipeWriter) *execStream {
	out, merged := io.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(merged, running.Stdout()) })
	wg.Go(func() { _, _ = io.Copy(merged, running.Stderr()) })
	go func() {
		wg.Wait()
		_ = merged.Close()
	}()
	return &execStream{running: running, out: out, in: stdin}
}

func (s *execStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *execStream) Write(p []byte) (int, error) { return s.in.Write(p) }

// Resize is accepted and does nothing: a command without a terminal has no
// window, and design 008 has the client send resize frames either way.
func (s *execStream) Resize(int, int) error { return nil }

func (s *execStream) Wait(ctx context.Context) (int, error) { return s.running.Wait(ctx) }
func (s *execStream) Close() error {
	s.once.Do(func() {
		_ = s.in.Close()
		_ = s.out.Close()
		_ = s.running.Close()
	})
	return nil
}

// frameWriter serializes every frame: a WebSocket admits one writer at a time,
// and the output reader, the keepalive and the last frame all write.
type frameWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *frameWriter) write(kind int, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteMessage(kind, payload)
}

func (w *frameWriter) writeJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.write(websocket.TextMessage, payload)
}

// fail sends the error frame design 008 states and closes with 1011.
func (w *frameWriter) fail(err error, requestID string) {
	_, envelope := errorEnvelope(err, requestID)
	_ = w.writeJSON(errorFrame{Error: envelope})
	closeWith(w, websocket.CloseInternalServerErr, envelope.Code)
}

// closeWith sends the close frame and lets the client see it before the
// connection goes.
func closeWith(w *frameWriter, code int, reason string) {
	if len(reason) > 100 {
		reason = reason[:100]
	}
	_ = w.write(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
}

// inbound is one frame the client sent.
type inbound struct {
	kind int
	data []byte
}

// drive runs the session until the process ends, the client goes away, or a
// frame cannot be delivered. The three are distinct: only a process that ended
// has an exit code to wait for, and waiting on a shell the client abandoned
// would never return.
func (h *handler) drive(r *http.Request, conn *websocket.Conn, w *frameWriter, stream runtime.Session, obj v1.Sandbox, requestID string) {
	conn.SetReadLimit(sessionFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(idleTimeout)) })

	ended, undelivered := make(chan struct{}), make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := w.write(websocket.BinaryMessage, buf[:n]); werr != nil {
					close(undelivered)
					return
				}
			}
			if err != nil {
				close(ended)
				return
			}
		}
	}()

	frames, gone := make(chan inbound), make(chan struct{})
	go func() {
		defer close(gone)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case frames <- inbound{kind: kind, data: data}:
			case <-ended:
				return
			}
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ended:
			// The client reader is left blocked in ReadMessage until the
			// connection closes below: a frame it reads after this point is
			// discarded on the ended channel, so nothing is lost. Forcing it
			// out with a read deadline on the hijacked connection is not
			// safe: with gorilla/websocket v1.5.4 the deadline reaches the
			// server's own connection reader, which cancels the request
			// context and with it the session's process before its exit
			// code is read. TestAttachRoundTrip fails with the deadline.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), waitTimeout)
			code, err := stream.Wait(ctx)
			cancel()
			if err != nil {
				h.metrics.Exec(metrics.ExitFailed)
				w.fail(err, requestID)
				return
			}
			h.metrics.Exec(metrics.ExitOf(code))
			_ = w.writeJSON(exitFrame{Exit: code})
			closeWith(w, websocket.CloseNormalClosure, "")
			return
		case <-undelivered:
			return
		case <-gone:
			return
		case <-ping.C:
			if err := w.write(websocket.PingMessage, nil); err != nil {
				return
			}
		case frame := <-frames:
			_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
			if !h.applyFrame(r, w, stream, obj, frame) {
				return
			}
		}
	}
}

// applyFrame carries out one client frame and reports whether the session
// goes on.
// Every batch stamps activity, which the controller coalesces per sandbox, so
// a session holds its sandbox away from the idle rule while it is used.
func (h *handler) applyFrame(r *http.Request, w *frameWriter, stream runtime.Session, obj v1.Sandbox, frame inbound) bool {
	switch frame.kind {
	case websocket.BinaryMessage:
		if len(frame.data) == 0 {
			return true
		}
		if _, err := stream.Write(frame.data); err != nil {
			return false
		}
		h.touch(r, obj)
	case websocket.TextMessage:
		var parsed clientFrame
		if json.Unmarshal(frame.data, &parsed) != nil || parsed.Resize == nil {
			return true
		}
		if parsed.Resize.Cols <= 0 || parsed.Resize.Rows <= 0 {
			return true
		}
		if err := stream.Resize(parsed.Resize.Cols, parsed.Resize.Rows); err != nil {
			return true
		}
		h.touch(r, obj)
	}
	return true
}
