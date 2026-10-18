// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// frame is one unmasked server frame as the suite reads it.
func frame(opcode byte, payload []byte) []byte {
	out := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		out = append(out, byte(n))
	case n <= 0xFFFF:
		out = append(out, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		out = append(append(out, 127), ext[:]...)
	}
	return append(out, payload...)
}

// closeFrame is a close frame with a code and a reason.
func closeFrame(code uint16, reason string) []byte {
	payload := binary.BigEndian.AppendUint16(nil, code)
	return frame(8, append(payload, reason...))
}

// TestTheWorkerStreamsFramesAreRFC6455: a server frame of each length form
// reads back whole, a frame past the bound and a cut frame are errors, and a
// masked client frame unmasks to what was written.
func TestTheWorkerStreamsFramesAreRFC6455(t *testing.T) {
	for _, size := range []int{5, 300, 70000} {
		payload := bytes.Repeat([]byte("x"), size)
		opcode, got, err := readFrame(bufio.NewReader(bytes.NewReader(frame(2, payload))))
		if err != nil || opcode != 2 || !bytes.Equal(got, payload) {
			t.Errorf("a %d byte frame read back as %d, %d bytes, %v", size, opcode, len(got), err)
		}
	}
	huge := []byte{0x82, 127, 0, 0, 0, 0, 0x10, 0, 0, 0}
	if _, _, err := readFrame(bufio.NewReader(bytes.NewReader(huge))); err == nil {
		t.Error("a frame past the bound was read")
	}
	for _, cut := range [][]byte{{0x82}, {0x82, 126, 1}, {0x82, 127, 0}, {0x82, 5, 'a'}} {
		if _, _, err := readFrame(bufio.NewReader(bytes.NewReader(cut))); err == nil {
			t.Errorf("the cut frame %v was read", cut)
		}
	}

	mask := []byte{1, 2, 3, 4}
	for _, size := range []int{10, 200} {
		var out bytes.Buffer
		payload := bytes.Repeat([]byte("y"), size)
		if err := writeMasked(&out, mask, payload); err != nil {
			t.Fatal(err)
		}
		raw := out.Bytes()
		head := 2
		if size >= 126 {
			head = 4
		}
		if raw[0] != 0x82 || raw[1]&0x80 == 0 || !bytes.Equal(raw[head:head+4], mask) {
			t.Fatalf("the %d byte frame's header is %v", size, raw[:head+4])
		}
		for i, b := range raw[head+4:] {
			if b^mask[i%4] != 'y' {
				t.Fatalf("byte %d of the %d byte frame does not unmask", i, size)
			}
		}
	}
	if err := writeMasked(io.Discard, mask, make([]byte, 70000)); err == nil {
		t.Error("a hello past 64 KiB was written")
	}
}

// TestAWorkerStreamIsHeldToItsClose: the close the case waits for passes
// with the policy code and the reason, and another code, another reason, a
// close with no code, a stream that ends without a close and a case whose
// time ran out each fail.
func TestAWorkerStreamIsHeldToItsClose(t *testing.T) {
	stream := func(frames ...[]byte) *workerStream {
		raw := bytes.Join(frames, nil)
		return &workerStream{conn: nopConn{bytes.NewReader(raw)}, reader: bufio.NewReader(bytes.NewReader(raw)), path: "/v1/environments/self/operations"}
	}
	if err := stream(frame(2, []byte("noise")), closeFrame(1008, "key_revoked")).closedWith(context.Background(), "key_revoked"); err != nil {
		t.Errorf("the right close failed: %v", err)
	}
	for name, s := range map[string]*workerStream{
		"another code":   stream(closeFrame(1001, "key_revoked")),
		"another reason": stream(closeFrame(1008, "serve_refused")),
		"no code":        stream(frame(8, nil)),
		"no close":       stream(frame(2, []byte("noise"))),
	} {
		if err := s.closedWith(context.Background(), "key_revoked"); err == nil {
			t.Errorf("%s passed as key_revoked", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := &workerStream{conn: nopConn{blockingReader{}}, reader: bufio.NewReader(blockingReader{}), path: "/x"}
	if err := blocked.closedWith(ctx, "key_revoked"); !errors.Is(err, context.Canceled) {
		t.Errorf("a case out of time answered %v", err)
	}
}

// nopConn is a stream whose writes go nowhere.
type nopConn struct{ io.Reader }

func (nopConn) Write(p []byte) (int, error) { return len(p), nil }
func (nopConn) Close() error                { return nil }

// blockingReader never returns.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

// TestTheWorkerCaseReadsWhatTheServerAnswers: a mint, a registration and an
// upgrade answered with something else than the contract states are each
// reported as the disagreement they are, and the case skips with no
// administrator.
func TestTheWorkerCaseReadsWhatTheServerAnswers(t *testing.T) {
	answer := map[string]func(http.ResponseWriter){
		"/v1/environments/e/keys": func(w http.ResponseWriter) { w.WriteHeader(http.StatusCreated); _, _ = io.WriteString(w, `{}`) },
		"/v1/environments/self/workers": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		},
		"/v1/environments/self/operations": func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) },
		"/v1/environments/e": func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"status":{"workers":0}}`)
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reply, ok := answer[r.URL.Path]; ok {
			reply(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	e := &Env{admin: newClient(server.URL, "admin")}
	ctx := context.Background()
	var disagreement *Disagreement
	if _, _, err := e.mintKey(ctx, "e"); !errors.As(err, &disagreement) {
		t.Errorf("a mint with no key answered %v", err)
	}
	if _, err := e.registerWorker(ctx, "k", []byte(`{}`), http.StatusCreated); !errors.As(err, &disagreement) {
		t.Errorf("a registration naming no worker answered %v", err)
	}
	if _, err := e.registerWorker(ctx, "k", []byte(`{}`), http.StatusOK); !errors.As(err, &disagreement) {
		t.Errorf("a registration answered with the wrong status answered %v", err)
	}
	if _, err := e.openWorkerStream(ctx, "e", "k", "wrk_1"); !errors.As(err, &disagreement) || !strings.Contains(err.Error(), "101") {
		t.Errorf("an upgrade answered 403 answered %v", err)
	}
	if _, _, err := e.mintKey(ctx, "absent"); err == nil {
		t.Error("a mint answered 404 passed")
	}
	short, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.awaitWorkers(short, "e", 1); err == nil {
		t.Error("waiting on a worker that never comes passed")
	}
	if err := case083WorkerRefusals(ctx, &Env{}); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("the case with no administrator answered %v", err)
	}
}

// scriptedPlane serves the routes case083WorkerRefusals reaches the way the
// contract states them, with one worker stream at a time, so the case's
// whole sequence runs here without a control plane.
type scriptedPlane struct {
	mu        chan struct{} // a one-slot lock that a hijacked stream can hold across writes
	registers int
	connected int
	closeWith chan string
}

func newScriptedPlane() *scriptedPlane {
	p := &scriptedPlane{mu: make(chan struct{}, 1), closeWith: make(chan string, 1)}
	return p
}

func (p *scriptedPlane) lock()   { p.mu <- struct{}{} }
func (p *scriptedPlane) unlock() { <-p.mu }

func (p *scriptedPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/v1/environments/"):
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/keys"):
		p.lock()
		p.registers++
		n := p.registers
		p.unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"key-`+string(rune('0'+n))+`","jti":"jti-`+string(rune('0'+n))+`"}`)
	case path == "/v1/environments/self/workers":
		p.lock()
		p.registers++
		n := p.registers
		p.unlock()
		if n == 4 {
			// The second key's registration on a full environment.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"worker_limit","message":"`+errorTable["worker_limit"].Message+`","details":{"request_id":"req_1"}}}`)
			return
		}
		if n == 5 {
			p.closeWith <- "worker_replaced"
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"worker":"wrk_`+string(rune('0'+n))+`"}`)
	case path == "/v1/environments/self/operations":
		p.stream(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/environments/"):
		p.lock()
		connected := p.connected
		p.unlock()
		_, _ = io.WriteString(w, `{"status":{"workers":`+string(rune('0'+connected))+`}}`)
	case r.Method == http.MethodDelete && strings.Contains(path, "/keys/"):
		p.closeWith <- "key_revoked"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		p.closeWith <- "serve_refused"
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// stream takes the upgrade, reads the hello, counts the worker connected,
// and closes the stream with the next reason a route asks for.
func (p *scriptedPlane) stream(w http.ResponseWriter, _ *http.Request) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: cella.worker.v1\r\n\r\n")
	_ = rw.Flush()
	if _, _, err = readClientFrame(rw.Reader); err != nil {
		return
	}
	p.lock()
	p.connected = 1
	p.unlock()
	reason := <-p.closeWith
	p.lock()
	p.connected = 0
	p.unlock()
	_, _ = rw.Write(frame(2, []byte("a frame the case skips")))
	_, _ = rw.Write(closeFrame(1008, reason))
	_ = rw.Flush()
}

// readClientFrame reads one masked client frame.
func readClientFrame(r *bufio.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	size := int(head[1] & 0x7F)
	if size == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		size = int(binary.BigEndian.Uint16(ext[:]))
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return head[0] & 0x0F, payload, nil
}

// TestTheWorkerCaseRunsItsWholeSequence: against a server that answers each
// step as the contract states, the case passes.
func TestTheWorkerCaseRunsItsWholeSequence(t *testing.T) {
	server := httptest.NewServer(newScriptedPlane())
	defer server.Close()
	e := &Env{admin: newClient(server.URL, "admin"), run: "abcdef"}
	if err := case083WorkerRefusals(t.Context(), e); err != nil {
		t.Fatalf("the case against a conforming server failed: %v", err)
	}
}

// TestAStepThatFailsEndsTheStream: a stream that will not open, and a step
// meant to end it that fails, each end the case with that failure and leave
// no stream open.
func TestAStepThatFailsEndsTheStream(t *testing.T) {
	plane := newScriptedPlane()
	server := httptest.NewServer(plane)
	defer server.Close()
	e := &Env{admin: newClient(server.URL, "admin")}
	failed := errors.New("the step failed")
	if err := e.closesWith(t.Context(), "e", "key", "wrk_1", "key_revoked", func() error { return failed }); !errors.Is(err, failed) {
		t.Errorf("a failed step answered %v", err)
	}
	plane.closeWith <- "key_revoked"

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer refusing.Close()
	e = &Env{admin: newClient(refusing.URL, "admin")}
	if err := e.closesWith(t.Context(), "e", "key", "wrk_1", "key_revoked", func() error { return nil }); err == nil {
		t.Error("a stream that never opened passed")
	}
}
