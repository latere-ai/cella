// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// The opcodes of RFC 6455 this client speaks. The standard library has no
// WebSocket and the build list of the command admits no module, so the
// exec and attach streams of design 008 are framed here.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// acceptGUID is the constant RFC 6455 defines the handshake's answer over.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxMessageBytes bounds one message the client accepts. The server writes
// output in 32 KiB frames and control messages that are one JSON object, so
// this is a bound on a peer that misbehaves and never on a real session.
const maxMessageBytes = 1 << 20

// closeNormal and closeInternal are the two codes design 008 states the
// server ends a session with.
const (
	closeNormal   = 1000
	closeInternal = 1011
)

// wsConn is one WebSocket over the connection an upgrade handed back. One
// writer at a time, which the mutex keeps: the input pump, a resize and a
// pong all write.
type wsConn struct {
	conn io.ReadWriteCloser
	br   *bufio.Reader
	// stop ends the watch that closes the connection with the caller's
	// context.
	stop func()
	wmu  sync.Mutex
	// closing is set once a close frame has been written, so the deferred
	// close of a session that already ended sends nothing.
	closing bool
}

// errNoUpgrade is an HTTP client that handed back a connection it cannot
// write to. A client with a Timeout does: it wraps the body to cancel it, and
// the wrapper reads only.
var errNoUpgrade = errors.New("the HTTP client handed back an upgraded connection that cannot be written to; " +
	"a Client.Timeout does that, so bound the call with its context instead")

// dialSocket opens one WebSocket under the control plane's address. The
// upgrade is an ordinary request through the caller's HTTP client, so the
// bearer, the proxy, the trust and the dialer of every other call are the
// socket's too, and a refusal before the upgrade is an HTTP response carrying
// the error envelope, which is what this returns. The transport keeps an
// upgrade on HTTP/1.1, and its answer's body is the connection.
func (c *Client) dialSocket(ctx context.Context, path string, query url.Values, subprotocol string) (*wsConn, error) {
	req, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	nonce := base64.StdEncoding.EncodeToString(key)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", nonce)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Protocol", subprotocol)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unreachable(req, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return nil, errorFrom(resp, body)
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, errNoUpgrade
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), accept(nonce); got != want {
		_ = conn.Close()
		return nil, errors.New("the server answered no WebSocket handshake")
	}
	// The session that follows carries no deadline, because a terminal is
	// idle by nature; the caller's context is what ends it.
	return &wsConn{conn: conn, br: bufio.NewReader(conn), stop: watchContext(ctx, conn)}, nil
}

// watchContext closes the connection when the caller's context ends, which
// is what cancels a read that is waiting on the far end. The returned
// function ends the watch.
func watchContext(ctx context.Context, conn io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// accept is the handshake answer for one nonce.
func accept(nonce string) string {
	sum := sha1.Sum([]byte(nonce + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// closeError says the peer closed the session, with the code it sent.
type closeError struct {
	Code   int
	Reason string
}

func (e *closeError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("the session closed with code %d", e.Code)
	}
	return fmt.Sprintf("the session closed with code %d: %s", e.Code, e.Reason)
}

// read returns one whole message. Control frames are answered here and
// never returned: a ping is ponged, a pong is dropped, and a close ends the
// read with a closeError.
func (w *wsConn) read() (int, []byte, error) {
	var message []byte
	kind := 0
	for {
		final, opcode, payload, err := w.frame()
		if err != nil {
			return 0, nil, err
		}
		switch opcode {
		case opPing:
			if err = w.write(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code, reason := closeReason(payload)
			return 0, nil, &closeError{Code: code, Reason: reason}
		case opText, opBinary:
			if message != nil {
				return 0, nil, errors.New("a new message began before the last one ended")
			}
			kind, message = opcode, payload
		case opContinuation:
			if message == nil {
				return 0, nil, errors.New("a continuation frame began a message")
			}
			message = append(message, payload...)
		default:
			return 0, nil, fmt.Errorf("the server sent opcode %d", opcode)
		}
		if len(message) > maxMessageBytes {
			return 0, nil, errors.New("the server sent a message past the client's bound")
		}
		if final {
			return kind, message, nil
		}
	}
}

// frame reads one frame's header and payload.
func (w *wsConn) frame() (final bool, opcode int, payload []byte, err error) {
	var header [2]byte
	if _, err = io.ReadFull(w.br, header[:]); err != nil {
		return false, 0, nil, err
	}
	final = header[0]&0x80 != 0
	opcode = int(header[0] & 0x0f)
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(w.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(w.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended[:]) & 0x7fffffffffffffff)
	}
	if length > maxMessageBytes {
		return false, 0, nil, errors.New("the server sent a frame past the client's bound")
	}
	var mask [4]byte
	if masked {
		// A server frames unmasked. One that masks is not this protocol.
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return final, opcode, payload, nil
}

// closeReason reads the close frame's payload, which is a code and an
// optional sentence.
func closeReason(payload []byte) (int, string) {
	if len(payload) < 2 {
		return closeNormal, ""
	}
	return int(binary.BigEndian.Uint16(payload[:2])), string(payload[2:])
}

// write sends one message as one masked frame. Every frame a client sends
// is masked, which RFC 6455 requires and a server enforces.
func (w *wsConn) write(opcode int, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header := []byte{byte(0x80 | opcode)}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80|length))
	case length <= 0xffff:
		header = append(header, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(length))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(length))
	}
	header = append(header, mask[:]...)
	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

// close sends the close frame and ends the connection. It is safe to call
// more than once.
func (w *wsConn) close() error {
	w.wmu.Lock()
	sent := w.closing
	w.closing = true
	w.wmu.Unlock()
	if !sent {
		payload := make([]byte, 2)
		binary.BigEndian.PutUint16(payload, closeNormal)
		_ = w.write(opClose, payload)
		w.stop()
	}
	return w.conn.Close()
}

// subprotocolExec is the one design 008 names for the exec and attach
// sockets.
const subprotocolExec = "cella.exec.v1"

// socketPath is the exec or attach route of one sandbox.
func socketPath(ref, verb string) string {
	return KindSandbox.item(ref) + "/" + verb
}
