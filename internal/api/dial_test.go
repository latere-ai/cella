// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/runtime"
)

// dialDriver declares Dial and implements no Dialer, which is a driver whose
// capability set and whose methods disagree. It embeds the interface rather
// than the native driver so no method is promoted by accident.
type dialDriver struct{ runtime.Driver }

func (dialDriver) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true, Dial: true}
}

// noDialDriver declares no Dial and implements none, which is an environment
// that reaches no port inside a sandbox.
type noDialDriver struct{ runtime.Driver }

func (d noDialDriver) Capabilities() runtime.Capabilities {
	c := d.Driver.Capabilities()
	c.Dial = false
	return c
}

// dialCall is one Dial a driver was asked for.
type dialCall struct {
	id   string
	port int
}

// recordingDialer declares Dial and answers it with the connection its test
// chooses, and keeps every call it was asked for: the confinement rows are
// about what reached the driver, not about what a server answered.
type recordingDialer struct {
	runtime.Driver
	mu    sync.Mutex
	calls []dialCall
	dial  func(ctx context.Context, id string, port int) (net.Conn, error)
}

func (d *recordingDialer) Capabilities() runtime.Capabilities {
	c := d.Driver.Capabilities()
	c.Dial = true
	return c
}

func (d *recordingDialer) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, dialCall{id: id, port: port})
	dial := d.dial
	d.mu.Unlock()
	return dial(ctx, id, port)
}

func (d *recordingDialer) asked() []dialCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dialCall(nil), d.calls...)
}

// dialTo is a dial that reaches one address whatever it is asked for.
func dialTo(addr string) func(context.Context, string, int) (net.Conn, error) {
	return func(ctx context.Context, _ string, _ int) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// echoServer is a loopback listener that writes back what each connection
// sends, standing for a server inside a sandbox. closeAfter, when set, ends a
// connection after its first read, for the inside closing its end.
func echoServer(t *testing.T, closeAfter bool) (string, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// ended is closed when a connection's caller side has gone.
	ended := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if closeAfter {
					buf := make([]byte, 64)
					n, _ := conn.Read(buf)
					_, _ = conn.Write(buf[:n])
					return
				}
				_, _ = io.Copy(conn, conn)
				once.Do(func() { close(ended) })
			}()
		}
	}()
	return ln.Addr().String(), ended
}

// openDial dials the dial socket with the subprotocol design 008 names.
func (f *fixture) openDial(path, token string) (*websocket.Conn, *http.Response, error) {
	f.t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	dialer := websocket.Dialer{Subprotocols: []string{dialSubprotocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.Dial("ws"+strings.TrimPrefix(f.url, "http")+path, header)
	if conn != nil {
		f.t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, res, err
}

// closeCode reads until the socket closes and answers its close code and text.
func closeCode(t *testing.T, conn *websocket.Conn) (int, string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var closed *websocket.CloseError
			if !errors.As(err, &closed) {
				t.Fatalf("the socket ended without a close frame: %v", err)
			}
			return closed.Code, closed.Text
		}
	}
}

// TestDialGate proves the dial route answers the gate before any driver call:
// design 004's capability decides, then the driver's Dialer, then the port
// and the phase, and an environment without the capability reads the refusal
// rather than the mux's not-found.
func TestDialGate(t *testing.T) {
	t.Run("undeclared", func(t *testing.T) {
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return noDialDriver{d} })
		obj := f.sandbox("plain")
		body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.alice, "", 422)
		if !strings.Contains(string(body), "capability_unsupported") {
			t.Fatalf("the gate answered %q", body)
		}
		if !strings.Contains(string(body), "reaches no port") {
			t.Fatalf("the developer detail does not name the reason: %q", body)
		}
	})
	t.Run("declared and unimplemented", func(t *testing.T) {
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return dialDriver{d} })
		obj := f.sandbox("plain")
		body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.alice, "", 422)
		if !strings.Contains(string(body), "implements no Dialer") {
			t.Fatalf("the developer detail does not name what the environment declared: %q", body)
		}
	})
	t.Run("read and authorize before the gate", func(t *testing.T) {
		f := setup(t, nil)
		obj := f.sandbox("plain")
		f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.bob, "", 403)
		f.request("GET", "/v1/sandboxes/sbx_01j0000000000000000000000/dial/8080", f.alice, "", 404)
	})
	t.Run("a port outside the range", func(t *testing.T) {
		f := setup(t, nil)
		obj := f.sandbox("plain")
		for _, port := range []string{"0", "65536", "-1", "http", "8080.5"} {
			body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/"+port, f.alice, "", 400)
			if !strings.Contains(string(body), "invalid_field") {
				t.Fatalf("port %s answered %q", port, body)
			}
		}
	})
	t.Run("a stopped sandbox", func(t *testing.T) {
		f := setup(t, nil)
		obj := f.sandbox("plain")
		f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
		body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.alice, "", 409)
		if !strings.Contains(string(body), "phase_conflict") {
			t.Fatalf("a stopped sandbox answered %q", body)
		}
	})
}

// TestDialSocket is the dial stream of design 008 over real HTTP: bytes both
// ways in binary frames, a close with 1000 when the inside closes, a close
// with 1011 and the code when the dial fails, the connection inside closed
// when the caller goes, and one sandbox.dial record per session.
func TestDialSocket(t *testing.T) {
	var d *recordingDialer
	f := setupRecordedDriver(t, func(base runtime.Driver) runtime.Driver {
		d = &recordingDialer{Driver: base}
		return d
	})
	obj := f.sandbox("plain")
	path := "/v1/sandboxes/" + obj.Status.ID + "/dial/"

	t.Run("bytes both ways", func(t *testing.T) {
		addr, ended := echoServer(t, false)
		d.mu.Lock()
		d.dial = dialTo(addr)
		d.mu.Unlock()
		conn, res, err := f.openDial(path+"5432", f.alice)
		if err != nil {
			t.Fatalf("the socket did not open: %v", err)
		}
		if got := res.Header.Get("Sec-WebSocket-Protocol"); got != dialSubprotocol {
			t.Fatalf("the handshake agreed on %q, want %q", got, dialSubprotocol)
		}
		for _, line := range []string{"hello", "again"} {
			if err = conn.WriteMessage(websocket.BinaryMessage, []byte(line)); err != nil {
				t.Fatal(err)
			}
			// A text frame and an empty frame carry no bytes and are
			// passed over, so the next read is the echo of the line.
			if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"resize":{}}`)); err != nil {
				t.Fatal(err)
			}
			if err = conn.WriteMessage(websocket.BinaryMessage, nil); err != nil {
				t.Fatal(err)
			}
			if err = conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			kind, data, err := conn.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage || string(data) != line {
				t.Fatalf("read %d %q %v, want the echo of %q", kind, data, err, line)
			}
		}
		asked := d.asked()
		if last := asked[len(asked)-1]; last != (dialCall{id: obj.Status.ID, port: 5432}) {
			t.Fatalf("the driver was asked for %+v", last)
		}
		// The caller going away closes the connection inside.
		_ = conn.Close()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the connection inside outlived the socket")
		}
		dialRecord(t, f, obj.Status.ID)
	})

	t.Run("the inside closes", func(t *testing.T) {
		addr, _ := echoServer(t, true)
		d.mu.Lock()
		d.dial = dialTo(addr)
		d.mu.Unlock()
		conn, _, err := f.openDial(path+"5432", f.alice)
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.WriteMessage(websocket.BinaryMessage, []byte("bye")); err != nil {
			t.Fatal(err)
		}
		if code, _ := closeCode(t, conn); code != websocket.CloseNormalClosure {
			t.Fatalf("the socket closed with %d, want 1000", code)
		}
	})

	t.Run("nothing listens", func(t *testing.T) {
		d.mu.Lock()
		d.dial = dialTo(closedAddr(t))
		d.mu.Unlock()
		conn, _, err := f.openDial(path+"5432", f.alice)
		if err != nil {
			t.Fatalf("the socket did not open: %v", err)
		}
		code, text := closeCode(t, conn)
		if code != websocket.CloseInternalServerErr || text != "upstream_unavailable" {
			t.Fatalf("the socket closed with %d %q, want 1011 upstream_unavailable", code, text)
		}
	})

	t.Run("a refusal the contract names", func(t *testing.T) {
		d.mu.Lock()
		d.dial = func(context.Context, string, int) (net.Conn, error) { return nil, runtime.ErrNotFound }
		d.mu.Unlock()
		conn, _, err := f.openDial(path+"5432", f.alice)
		if err != nil {
			t.Fatal(err)
		}
		if code, text := closeCode(t, conn); code != websocket.CloseInternalServerErr || text != "not_found" {
			t.Fatalf("the socket closed with %d %q, want 1011 not_found", code, text)
		}
	})

	t.Run("a read inside that fails", func(t *testing.T) {
		d.mu.Lock()
		d.dial = func(context.Context, string, int) (net.Conn, error) {
			return failingConn{}, nil
		}
		d.mu.Unlock()
		conn, _, err := f.openDial(path+"5432", f.alice)
		if err != nil {
			t.Fatal(err)
		}
		if code, text := closeCode(t, conn); code != websocket.CloseInternalServerErr || text != "upstream_unavailable" {
			t.Fatalf("the socket closed with %d %q, want 1011 upstream_unavailable", code, text)
		}
	})
}

// failingConn is a connection whose first read fails with something other
// than the end of the stream.
type failingConn struct{ net.Conn }

func (failingConn) Read([]byte) (int, error)  { return 0, errors.New("connection reset by peer") }
func (failingConn) Write([]byte) (int, error) { return 0, errors.New("connection reset by peer") }
func (failingConn) Close() error              { return nil }

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// dialRecord waits for the session's record and checks it names bytes each
// way and no byte of the connection.
func dialRecord(t *testing.T, f *recorded, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, record := range f.records(id) {
			if record.Type != events.TypeDial {
				continue
			}
			var data events.Dial
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.BytesIn != int64(len("hello")+len("again")) || data.BytesOut != data.BytesIn || data.DurationMS < 0 {
				t.Fatalf("the session record is %+v", data)
			}
			if strings.Contains(string(record.Data), "hello") {
				t.Fatalf("the record carries the connection's bytes: %s", record.Data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed session wrote no record")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUpstreamErrorKeepsTheContractsRefusals: a refusal the driver names
// keeps its code, and any other dial failure is the port's own.
func TestUpstreamErrorKeepsTheContractsRefusals(t *testing.T) {
	for _, err := range []error{runtime.ErrNotRunning, runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported} {
		if got := upstreamError(err); !errors.Is(got, err) {
			t.Errorf("%v became %v", err, got)
		}
	}
	if code, _ := errorEnvelope(upstreamError(errors.New("connection refused")), "req_x"); code != http.StatusBadGateway {
		t.Errorf("a refused connection answers %d, want 502", code)
	}
}
