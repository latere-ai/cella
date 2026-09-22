// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellaclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// subprotocolDial is the one design 008 names for the dial socket.
const subprotocolDial = "cella.dial.v1"

// dialFrameBytes bounds one frame the client sends. The server reads frames
// of up to a mebibyte; a write larger than this is sent as several frames,
// which the byte stream does not tell apart.
const dialFrameBytes = 32 << 10

// codeUpstreamUnavailable is the code the server closes a dial socket with
// when nothing listens on the port, and the sentence the API gives it.
const (
	codeUpstreamUnavailable    = "upstream_unavailable"
	messageUpstreamUnavailable = "Nothing is listening on that port."
)

// Stream is one dial socket: a connection to a port inside a sandbox, its
// bytes both ways. Read answers io.EOF once the inside closed its end, and an
// Error naming the code the server closed with where the dial or the
// connection failed. The protocol has no half close, so Close ends both
// directions. One goroutine reads and one writes, as with a net.Conn.
type Stream struct {
	conn    *wsConn
	pending []byte
}

// Dial opens the dial socket to one port of a sandbox. A refusal before the
// upgrade, which is a sandbox that is missing, stopped, or on an environment
// that reaches no port, is an Error with its status. The connection inside is
// opened after the upgrade, so a port nothing listens on is a first Read that
// fails with upstream_unavailable.
func (c *Client) Dial(ctx context.Context, ref string, port int) (*Stream, error) {
	conn, err := c.dialSocket(ctx, socketPath(ref, "dial/"+strconv.Itoa(port)), nil, subprotocolDial)
	if err != nil {
		return nil, err
	}
	return &Stream{conn: conn}, nil
}

// Read returns the bytes the port sent. Frames that carry no bytes are passed
// over.
func (s *Stream) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		kind, payload, err := s.conn.read()
		if err != nil {
			return 0, streamEnd(err)
		}
		if kind == opBinary {
			s.pending = payload
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// Write sends bytes to the port, in frames of a bounded size.
func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), dialFrameBytes)]
		if err := s.conn.write(opBinary, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// Close ends the socket and with it the connection inside.
func (s *Stream) Close() error { return s.conn.close() }

// streamEnd reads how a socket ended: a normal close is the end of the
// stream, and any other close is the refusal whose code the server wrote as
// its reason.
func streamEnd(err error) error {
	var closed *closeError
	if !errors.As(err, &closed) {
		return err
	}
	if closed.Code == closeNormal {
		return io.EOF
	}
	// A socket that closed after the upgrade has no status of its own:
	// upstream_unavailable reads as the 502 its refusal carries, and any
	// other code as 500, which is what an error frame reads as.
	out := &Error{Status: http.StatusInternalServerError, Code: closed.Reason,
		Message: "The connection inside the sandbox ended: " + closed.Reason + "."}
	if closed.Reason == codeUpstreamUnavailable {
		out.Status, out.Message = http.StatusBadGateway, messageUpstreamUnavailable
	}
	return out
}
