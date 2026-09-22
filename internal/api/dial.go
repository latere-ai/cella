// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// dialSubprotocol is the one design 008 names for the dial socket. A client
// that offers nothing still connects, because the frames are bytes and carry
// no shape of their own.
const dialSubprotocol = "cella.dial.v1"

// dialTimeout bounds opening the connection inside once the socket is up. A
// port on the sandbox's own network answers or refuses at once; a dial that
// takes longer is a data plane that is not answering.
const dialTimeout = 10 * time.Second

var dialUpgrader = websocket.Upgrader{
	Subprotocols: []string{dialSubprotocol},
	// The origin is not the boundary here: every frame travels under a bearer
	// the handshake already verified.
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 10 * time.Second,
}

// errUndelivered ends a session whose caller stopped reading.
var errUndelivered = errors.New("a frame could not be delivered to the caller")

// dialGate reads, authorizes and gates in one place for the two routes that
// reach a port inside a sandbox: the object, the action their rows name, and
// the Dial capability of the environment the sandbox runs on, before any
// driver call.
func (h *handler) dialGate(w http.ResponseWriter, r *http.Request) (v1.Sandbox, bool) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return obj, false
	}
	if !h.Controller.CapabilitiesOf(obj.Status.Environment).Dial {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "the environment reaches no port inside the sandbox"})
		return obj, false
	}
	return obj, true
}

// dialerOf is the driver's dial half for a sandbox that passed the gate. An
// environment whose declaration and driver disagree answers the same code as
// one that declares nothing, with the developer detail naming the driver.
func (h *handler) dialerOf(w http.ResponseWriter, obj v1.Sandbox) (runtime.Dialer, bool) {
	dialer, err := h.Controller.Dialer(obj.Status.ID)
	if errors.Is(err, runtime.ErrUnsupported) {
		err = &manifest.Error{Code: "capability_unsupported",
			Detail: "the " + h.Controller.DriverNameOf(obj.Status.Environment) + " driver declares Dial and implements no Dialer"}
	}
	if err != nil {
		respondError(w, err)
		return nil, false
	}
	return dialer, true
}

// dial serves GET /v1/sandboxes/{id}/dial/{port}: design 008's raw byte stream
// to a port inside the sandbox. Every refusal the route can know before the
// upgrade is an HTTP answer with its status; the dial itself runs after it, so
// a port that is closed now reads as a socket that closes with the reason,
// and an environment that cannot dial at all as a refusal.
func (h *handler) dial(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.dialGate(w, r)
	if !ok {
		return
	}
	port, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || port == 0 {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "port", Detail: "the port is a number between 1 and 65535"})
		return
	}
	if obj.Status.Phase != runtime.Running {
		respondError(w, &manifest.Error{Code: "phase_conflict", Detail: "the sandbox is " + obj.Status.Phase + ", and a port is reached only while it runs"})
		return
	}
	dialer, ok := h.dialerOf(w, obj)
	if !ok {
		return
	}
	requestID := w.Header().Get(RequestIDHeader)
	conn, err := dialUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has written the reply, so the caller already has the reason.
		return
	}
	defer func() { _ = conn.Close() }()
	writer := &frameWriter{conn: conn}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	inside, err := dialer.Dial(ctx, obj.Status.ID, int(port))
	cancel()
	if err != nil {
		_, envelope := errorEnvelope(upstreamError(err), requestID)
		closeWith(writer, websocket.CloseInternalServerErr, envelope.Code)
		return
	}
	defer func() { _ = inside.Close() }()
	h.touch(r, obj)
	h.relay(r, conn, writer, inside, obj)
}

// upstreamError keeps a refusal the contract names as it is, and reads any
// other failure of a dial as the port's own: nothing listening, or a
// connection the network dropped.
func upstreamError(err error) error {
	for _, named := range []error{runtime.ErrNotRunning, runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported} {
		if errors.Is(err, named) {
			return err
		}
	}
	return &manifest.Error{Code: "upstream_unavailable", Detail: err.Error()}
}

// relay carries bytes both ways until one end closes. The inside closing its
// end closes the socket with 1000 and a read that fails with 1011; the caller
// closing the socket, or a frame it stops reading, closes the connection
// inside, which the deferred close in dial does.
func (h *handler) relay(r *http.Request, conn *websocket.Conn, w *frameWriter, inside net.Conn, obj v1.Sandbox) {
	started := time.Now()
	var in, out atomic.Int64
	defer func() {
		// The record is written on a context detached from the request's,
		// because the usual way a session ends is the caller going away,
		// which cancels it.
		h.emit(r.WithContext(context.WithoutCancel(r.Context())), obj, events.TypeDial, events.Dial{
			DurationMS: time.Since(started).Milliseconds(), BytesIn: in.Load(), BytesOut: out.Load(),
		})
	}()
	conn.SetReadLimit(sessionFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(idleTimeout)) })

	// ended carries how the inside's end finished: io.EOF when it closed,
	// errUndelivered when the caller stopped reading, any other error when
	// the read failed. It has room for the one value its writer sends, so the
	// writer never waits on a session that already returned.
	ended := make(chan error, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := inside.Read(buf)
			if n > 0 {
				if werr := w.write(websocket.BinaryMessage, buf[:n]); werr != nil {
					ended <- errUndelivered
					return
				}
				out.Add(int64(n))
			}
			if err != nil {
				ended <- err
				return
			}
		}
	}()

	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
			if kind != websocket.BinaryMessage || len(data) == 0 {
				continue
			}
			if _, err = inside.Write(data); err != nil {
				return
			}
			in.Add(int64(len(data)))
			h.touch(r, obj)
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case err := <-ended:
			switch {
			case errors.Is(err, errUndelivered):
			case errors.Is(err, io.EOF):
				closeWith(w, websocket.CloseNormalClosure, "the port closed")
			default:
				closeWith(w, websocket.CloseInternalServerErr, "upstream_unavailable")
			}
			return
		case <-gone:
			return
		case <-ping.C:
			if err := w.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
