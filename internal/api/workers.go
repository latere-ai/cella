// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	"latere.ai/x/cella/runtime/remote"
)

// workerUpgrader turns the request into a WebSocket. The origin check is the
// environment key: a browser cannot hold one, so there is no cross-origin
// case to defend against here.
var workerUpgrader = websocket.Upgrader{
	Subprotocols: []string{remote.Protocol},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// ErrWorkerEnvironment is a worker that connected to an environment its key
// does not name.
var ErrWorkerEnvironment = errors.New("the key names another environment")

// WorkerRegistrations is the hub as the controller's phase loop reads it: the
// workers holding one environment's stream open, in the controller's own
// vocabulary. The controller names no transport of its own, so the two
// vocabularies meet here and nowhere else.
func WorkerRegistrations(hub *remote.Hub) func(string) []controller.Registration {
	return func(environment string) []controller.Registration {
		states := hub.Workers(environment)
		out := make([]controller.Registration, 0, len(states))
		for _, w := range states {
			out = append(out, controller.Registration{
				Worker: w.Worker, LastHeartbeat: w.LastHeartbeat, Connected: w.Connected,
			})
		}
		return out
	}
}

// workerRoute is one of the two routes an environment key reaches on behalf
// of a worker, matched before the mux because every route on the mux decides
// on a subject and an environment key names none.
type workerRoute struct {
	environment string
	register    bool
	stream      bool
}

// workerPath matches POST /v1/environments/{id}/workers and GET
// /v1/environments/{id}/operations. The id may be the word self, which is the
// environment the key already names: a worker learns its environment from its
// key and never has to be told it twice.
func workerPath(r *http.Request) (workerRoute, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/environments/")
	if !ok {
		return workerRoute{}, false
	}
	id, verb, ok := strings.Cut(rest, "/")
	if !ok || id == "" || strings.Contains(verb, "/") {
		return workerRoute{}, false
	}
	switch {
	case verb == "workers" && r.Method == http.MethodPost:
		return workerRoute{environment: id, register: true}, true
	case verb == "operations" && r.Method == http.MethodGet:
		return workerRoute{environment: id, stream: true}, true
	}
	return workerRoute{}, false
}

// serveWorkerRoute answers one of the worker routes for a caller holding an
// environment key. The key names the environment; a path naming another one
// is refused, which is the rule the gateway stream applies as well.
func (h *handler) serveWorkerRoute(w http.ResponseWriter, r *http.Request, route workerRoute, environment string) {
	if route.environment != "self" && route.environment != environment {
		respondError(w, &auth.Error{Code: auth.CodeForbidden, Detail: ErrWorkerEnvironment.Error()})
		return
	}
	if h.Workers == nil {
		respondError(w, &manifest.Error{Code: "capability_unsupported",
			Detail: "this control plane serves no worker environments"})
		return
	}
	if route.register {
		h.registerWorker(w, r, environment)
		return
	}
	h.workerStream(w, r, environment)
}

// registerWorker records one worker and its driver and answers with the id it
// claims under. A worker that restarts registers again and its old claims
// lapse, so nothing is held by an id nobody answers for.
func (h *handler) registerWorker(w http.ResponseWriter, r *http.Request, environment string) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, &manifest.Error{Code: "body_too_large", Detail: err.Error()})
		return
	}
	var registration remote.Registration
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&registration); err != nil {
		respondError(w, &manifest.Error{Code: "bad_request", Detail: err.Error()})
		return
	}
	// The id is the control plane's to mint, so a worker cannot claim
	// another worker's operations by naming it.
	registration.Worker = ""
	registered, err := h.Workers.Register(environment, registration)
	if err != nil {
		if errors.Is(err, remote.ErrRegistrationMismatch) {
			respondError(w, &manifest.Error{Code: "environment_mismatch", Detail: err.Error()})
			return
		}
		respondError(w, &manifest.Error{Code: "invalid_field", Detail: err.Error()})
		return
	}
	registered.Environment = environment
	respond(w, http.StatusCreated, registered)
}

// workerStream runs one worker's stream: the frames of spec 021 over one
// WebSocket the worker opened. The control plane never dials a worker, so
// everything it hands one travels here.
func (h *handler) workerStream(w http.ResponseWriter, r *http.Request, environment string) {
	conn, err := workerUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered
	}
	if conn.Subprotocol() != remote.Protocol {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "this server speaks "+remote.Protocol),
			time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	_ = h.Workers.Serve(r.Context(), environment, newWorkerSocket(conn))
}

// workerSocket is the worker protocol's frames over one WebSocket, the
// control plane's end. The link above it guarantees one writer; the mutex
// here is what makes that guarantee hold against the close frame.
type workerSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

var _ remote.FrameConn = (*workerSocket)(nil)

func newWorkerSocket(conn *websocket.Conn) *workerSocket {
	_ = conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout))
	})
	return &workerSocket{conn: conn}
}

func (s *workerSocket) ReadFrame() ([]byte, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(remote.HeartbeatTimeout)); err != nil {
		return nil, err
	}
	kind, raw, err := s.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if kind != websocket.BinaryMessage {
		return nil, errors.New("api: every frame of the worker protocol is binary")
	}
	return raw, nil
}

func (s *workerSocket) WriteFrame(raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, raw)
}

func (s *workerSocket) Close() error { return s.conn.Close() }
