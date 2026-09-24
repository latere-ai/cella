// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"latere.ai/x/pkg/httpjson"
)

// Session is one exec or attach socket: the bytes of the command both ways,
// the window, and the exit the server sends before it closes.
//
// Read returns the session's output and Write reaches its input. The
// protocol of design 008 has no half close, so a caller that reaches the end
// of its own input lets the command end on its own.
type Session struct {
	conn *wsConn
	out  *io.PipeReader
	in   *io.PipeWriter
	done chan struct{}

	mu      sync.Mutex
	exit    int
	err     error
	started bool
}

// clientFrame is a text frame the client sends after the first: the window,
// and nothing else.
type clientFrame struct {
	Resize resize `json:"resize"`
}
type resize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// serverFrame is a text frame the server sends: the exit code, or the error
// envelope of design 008 in the shape an HTTP refusal carries.
type serverFrame struct {
	Exit  *int            `json:"exit"`
	Error *httpjson.Error `json:"error"`
}

// ExecSession opens the exec socket: a command whose input the caller
// writes, and a terminal where the request carries a window.
func (c *Client) ExecSession(ctx context.Context, ref string, req ExecRequest) (*Session, error) {
	return c.session(ctx, socketPath(ref, "exec"), req)
}

// AttachSession opens the attach socket, which is always a terminal.
func (c *Client) AttachSession(ctx context.Context, ref string, req ExecRequest) (*Session, error) {
	return c.session(ctx, socketPath(ref, "attach"), req)
}

// session dials, sends the request as the first text frame, and starts
// reading. A refusal before the upgrade is an Error with its status; a
// refusal after it arrives as an error frame and reaches Wait.
func (c *Client) session(ctx context.Context, path string, req ExecRequest) (*Session, error) {
	conn, err := c.dialSocket(ctx, path, nil, subprotocolExec)
	if err != nil {
		return nil, err
	}
	first, err := json.Marshal(req)
	if err != nil {
		_ = conn.close()
		return nil, err
	}
	if err = conn.write(opText, first); err != nil {
		_ = conn.close()
		return nil, &Unreachable{Op: "the session request", Err: err}
	}
	out, in := io.Pipe()
	s := &Session{conn: conn, out: out, in: in, done: make(chan struct{})}
	go s.pump()
	return s, nil
}

// pump reads the socket until the session ends: binary frames are the
// output, a text frame is the exit or the error, and a close or a failure
// ends the read. The output pipe carries the same end to the reader.
func (s *Session) pump() {
	defer close(s.done)
	for {
		kind, payload, err := s.conn.read()
		if err != nil {
			s.end(err)
			return
		}
		switch kind {
		case opBinary:
			if len(payload) == 0 {
				continue
			}
			s.mu.Lock()
			s.started = true
			s.mu.Unlock()
			if _, err = s.in.Write(payload); err != nil {
				s.end(err)
				return
			}
		case opText:
			var frame serverFrame
			if json.Unmarshal(payload, &frame) != nil {
				continue
			}
			switch {
			case frame.Error != nil:
				s.end(frameError(*frame.Error))
				return
			case frame.Exit != nil:
				s.finish(*frame.Exit)
				return
			}
		}
	}
}

// frameError turns the envelope of an error frame into the same Error an
// HTTP refusal decodes to, so a caller reads one error shape whether the
// session failed before or after the upgrade. The status is 500: an error
// frame is what a failure that can no longer set one carries.
func frameError(e httpjson.Error) *Error {
	out := &Error{Status: 500, Code: e.Code, Message: e.Message}
	out.RequestID, _ = e.Details["request_id"].(string)
	out.Detail, _ = e.Details["detail"].(string)
	out.Paths = list(e.Details["paths"])
	return out
}

// end records a failure and ends the output.
func (s *Session) end(err error) {
	var closed *closeError
	if errors.As(err, &closed) && closed.Code == closeNormal {
		err = nil
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	if err == nil {
		_ = s.in.Close()
		return
	}
	_ = s.in.CloseWithError(err)
}

// finish records the exit code and ends the output cleanly.
func (s *Session) finish(code int) {
	s.mu.Lock()
	s.exit = code
	s.mu.Unlock()
	_ = s.in.Close()
}

// Read is the session's output.
func (s *Session) Read(p []byte) (int, error) { return s.out.Read(p) }

// Write reaches the command's input.
func (s *Session) Write(p []byte) (int, error) {
	if err := s.conn.write(opBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize sends the window. A command without a terminal accepts it and does
// nothing with it, which is what design 008 states.
func (s *Session) Resize(cols, rows int) error {
	frame, err := json.Marshal(clientFrame{Resize: resize{Cols: cols, Rows: rows}})
	if err != nil {
		return err
	}
	return s.conn.write(opText, frame)
}

// Wait returns the command's exit code once the session has ended. An error
// frame, a close that was not normal, or a connection that failed is the
// error; the exit code is meaningless then.
func (s *Session) Wait() (int, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit, s.err
}

// Started reports whether any output byte arrived. It is what separates a
// command that could not start from one that failed part way, which the
// exit scheme of design 011 separates as 126 from 125.
func (s *Session) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Close ends the session.
func (s *Session) Close() error {
	err := s.conn.close()
	<-s.done
	_ = s.out.Close()
	return err
}
