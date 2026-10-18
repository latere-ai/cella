// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/runtime/remote"
)

// writeDeadline bounds one frame's write, so a peer that stopped reading does
// not hold the writer forever.
const writeDeadline = 10 * time.Second

// writeDeadlineOverride replaces writeDeadline where it is set, which only
// this package's tests do, to reach the deadline without waiting for it.
var writeDeadlineOverride atomic.Int64

// frameDeadline is when a frame written now must have been written by.
func frameDeadline() time.Time {
	deadline := writeDeadline
	if override := writeDeadlineOverride.Load(); override > 0 {
		deadline = time.Duration(override)
	}
	return time.Now().Add(deadline)
}

// Socket is the worker protocol's frames over one WebSocket. The link above
// it guarantees a single writer, and the mutex here is what makes that
// guarantee hold for the pong the read pump sends back.
type Socket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

var _ remote.FrameConn = (*Socket)(nil)

// NewSocket wraps one connection. It sets the read deadline the heartbeat
// renews, so a peer that stopped sending is noticed within the lease rather
// than held open until the operating system gives up.
func NewSocket(conn *websocket.Conn) *Socket {
	s := &Socket{conn: conn}
	_ = conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout))
	})
	return s
}

// ReadFrame takes the next binary message. A text message is a peer speaking
// another protocol, which ends the connection rather than being skipped.
func (s *Socket) ReadFrame() ([]byte, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout)); err != nil {
		return nil, err
	}
	kind, raw, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if kind != websocket.BinaryMessage {
		return nil, errors.New("worker: every frame of this protocol is binary")
	}
	return raw, nil
}

func (s *Socket) WriteFrame(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(frameDeadline()); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, raw)
}

func (s *Socket) Close() error { return s.conn.Close() }
