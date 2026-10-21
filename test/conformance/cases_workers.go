// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// workerProtocol is the subprotocol a worker's stream speaks (spec 021).
const workerProtocol = "cella.worker.v1"

// noOperation is the operation id a frame that belongs to no operation
// carries, the hello among them: 26 zeros.
const noOperation = "00000000000000000000000000"

// case083WorkerRefusals: on an environment that takes one worker, a second
// key's registration is refused with worker_limit; the live worker's key
// registering again from the same host replaces it, and its stream is closed
// with worker_replaced; revoking a key closes the stream it opened with
// key_revoked; and deleting the environment closes its stream with
// serve_refused. Each close is the policy code 1008 with the reason.
func case083WorkerRefusals(ctx context.Context, e *Env) error {
	if e.admin == nil {
		return skipf("no administrator token: set Admin to apply and key a worker environment")
	}
	name := e.name()
	body := []byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"` + name + `"},` +
		`"spec":{"mode":"worker","isolation":"none","capacity":{"sandboxes":1},"workers":{"max":1}}}`)
	if err := e.adminCall(ctx, http.MethodPut, "/v1/environments/"+name, body, http.StatusCreated); err != nil {
		return err
	}
	deleted := false
	defer func() {
		if !deleted {
			_, _ = e.admin.del(context.WithoutCancel(ctx), "/v1/environments/"+name)
		}
	}()
	first, firstJTI, err := e.mintKey(ctx, name)
	if err != nil {
		return err
	}
	second, _, err := e.mintKey(ctx, name)
	if err != nil {
		return err
	}
	registration := []byte(`{"driver":"conformance","isolation":"none"}`)
	register := func(key string) error {
		_, err := e.registerWorker(ctx, key, registration, http.StatusCreated)
		return err
	}

	// The first key takes the one place; the second is refused.
	worker, err := e.registerWorker(ctx, first, registration, http.StatusCreated)
	if err != nil {
		return err
	}
	x, err := e.admin.send(ctx, http.MethodPost, "/v1/environments/self/workers",
		request{Body: registration, ContentType: "application/json", Token: second})
	if err != nil {
		return err
	}
	if err = x.refusal("worker_limit"); err != nil {
		return err
	}
	// The first key registering again from the same host replaces it.
	if err = e.closesWith(ctx, name, first, worker, "worker_replaced", func() error { return register(first) }); err != nil {
		return err
	}
	// Revoking that key closes the stream it opened.
	if worker, err = e.registerWorker(ctx, first, registration, http.StatusCreated); err != nil {
		return err
	}
	revoke := func() error {
		return e.adminCall(ctx, http.MethodDelete, "/v1/environments/"+name+"/keys/"+firstJTI, nil, http.StatusNoContent)
	}
	if err = e.closesWith(ctx, name, first, worker, "key_revoked", revoke); err != nil {
		return err
	}
	// Deleting the environment closes the stream of the key left.
	if worker, err = e.registerWorker(ctx, second, registration, http.StatusCreated); err != nil {
		return err
	}
	return e.closesWith(ctx, name, second, worker, "serve_refused", func() error {
		deleted = true
		return e.adminCall(ctx, http.MethodDelete, "/v1/environments/"+name, nil, http.StatusNoContent)
	})
}

// adminCall is one call as the administrator, held to the status wanted.
func (e *Env) adminCall(ctx context.Context, method, path string, body []byte, want int) error {
	req := request{Body: body}
	if body != nil {
		req.ContentType = "application/json"
	}
	x, err := e.admin.send(ctx, method, path, req)
	if err != nil {
		return err
	}
	return x.status(want)
}

// closesWith opens a registered worker's stream, does what should end it,
// and holds the close to the reason wanted.
func (e *Env) closesWith(ctx context.Context, environment, key, worker, reason string, act func() error) error {
	live, err := e.openWorkerStream(ctx, environment, key, worker)
	if err != nil {
		return err
	}
	if err = act(); err != nil {
		_ = live.conn.Close()
		return err
	}
	return live.closedWith(ctx, reason)
}

// mintKey mints one key of an environment as the administrator and returns
// it and its jti.
func (e *Env) mintKey(ctx context.Context, environment string) (string, string, error) {
	x, err := e.admin.post(ctx, "/v1/environments/"+environment+"/keys", nil)
	if err != nil {
		return "", "", err
	}
	if err = x.status(http.StatusCreated); err != nil {
		return "", "", err
	}
	var minted struct {
		Token string `json:"token"`
		JTI   string `json:"jti"`
	}
	if err = json.Unmarshal(x.Body, &minted); err != nil || minted.Token == "" || minted.JTI == "" {
		return "", "", x.disagree("a key and its jti", string(x.Body))
	}
	return minted.Token, minted.JTI, nil
}

// registerWorker registers a worker with a key and returns the id it claims
// under.
func (e *Env) registerWorker(ctx context.Context, key string, registration []byte, want int) (string, error) {
	x, err := e.admin.send(ctx, http.MethodPost, "/v1/environments/self/workers",
		request{Body: registration, ContentType: "application/json", Token: key})
	if err != nil {
		return "", err
	}
	if err = x.status(want); err != nil {
		return "", err
	}
	var registered struct {
		Worker string `json:"worker"`
	}
	if err = json.Unmarshal(x.Body, &registered); err != nil || registered.Worker == "" {
		return "", x.disagree("the id the worker claims under", string(x.Body))
	}
	return registered.Worker, nil
}

// workerStream is one worker's stream, read only for how it ends.
type workerStream struct {
	conn   io.ReadWriteCloser
	reader *bufio.Reader
	path   string
}

// openWorkerStream opens a worker's stream with its key, says hello as the
// worker, and returns once the environment reports one worker. The
// registration alone already counts, so the hello may still be on its way
// when the case acts; a server closes the stream with the reason whichever
// arrives first.
func (e *Env) openWorkerStream(ctx context.Context, environment, key, worker string) (*workerStream, error) {
	path := "/v1/environments/self/operations"
	resp, err := e.admin.open(ctx, http.MethodGet, path, request{Token: key, Header: map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Protocol": workerProtocol,
	}})
	if err != nil {
		return nil, err
	}
	conn, upgraded := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !upgraded {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		x := &exchange{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Header: resp.Header, Body: answer}
		return nil, x.disagree("status 101 and the worker's stream", fmt.Sprintf("status %d", resp.StatusCode))
	}
	s := &workerStream{conn: conn, reader: bufio.NewReader(conn), path: path}
	// The worker id is the control plane's own token of letters, digits
	// and an underscore, so it is written into the hello as it is.
	hello := append([]byte(noOperation+"\x00"), `{"type":"hello","worker":"`+worker+`"}`...)
	if err = writeMasked(conn, []byte{0x5a, 0xa5, 0x3c, 0xc3}, hello); err == nil {
		err = e.awaitWorkers(ctx, environment, 1)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

// awaitWorkers polls the environment until it reports n workers: holding a
// stream, or registered and yet to open one.
func (e *Env) awaitWorkers(ctx context.Context, environment string, n int) error {
	deadline := time.Now().Add(CaseTimeout / 2)
	for {
		x, err := e.admin.get(ctx, "/v1/environments/"+environment)
		if err != nil {
			return err
		}
		var obj struct {
			Status struct {
				Workers int `json:"workers"`
			} `json:"status"`
		}
		if json.Unmarshal(x.Body, &obj) == nil && obj.Status.Workers == n {
			return nil
		}
		if time.Now().After(deadline) {
			return x.disagree(fmt.Sprintf("status.workers %d once the stream said hello", n), string(x.Body))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// closedWith reads the stream until the server closes it, and holds the
// close to the policy code and the reason wanted.
func (s *workerStream) closedWith(ctx context.Context, reason string) error {
	defer func() { _ = s.conn.Close() }()
	done := make(chan error, 1)
	go func() { done <- s.awaitClose(reason) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("the worker's stream was not closed with %s: %w", reason, ctx.Err())
	}
}

func (s *workerStream) awaitClose(reason string) error {
	for {
		opcode, payload, err := readFrame(s.reader)
		if err != nil {
			return fmt.Errorf("the worker's stream ended without a close frame naming %s: %w", reason, err)
		}
		if opcode != 8 {
			continue
		}
		if len(payload) < 2 {
			return &Disagreement{Method: http.MethodGet, Path: s.path, Want: "close 1008 " + reason, Got: "a close with no code"}
		}
		code, got := binary.BigEndian.Uint16(payload), string(payload[2:])
		if code != 1008 || got != reason {
			return &Disagreement{Method: http.MethodGet, Path: s.path, Want: "close 1008 " + reason, Got: fmt.Sprintf("close %d %s", code, got)}
		}
		return nil
	}
}

// writeMasked writes one final binary frame, masked as a client's frames
// are (RFC 6455).
func writeMasked(w io.Writer, mask []byte, payload []byte) error {
	header := []byte{0x82}
	switch n := len(payload); {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		return errors.New("a hello longer than 64 KiB")
	}
	header = append(header, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := w.Write(append(header, masked...))
	return err
}

// readFrame reads one unmasked server frame: its opcode and its payload.
func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	size := uint64(head[1] & 0x7F)
	switch size {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		size = binary.BigEndian.Uint64(ext[:])
	}
	if size > 1<<21 {
		return 0, nil, fmt.Errorf("a frame of %d bytes, past what the worker's stream carries", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return head[0] & 0x0F, payload, nil
}
