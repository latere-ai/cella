// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
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
		// The driver is the environment's, recorded from the first
		// registration, which is what spec 021 puts in status.driver.
		driverName := ""
		if r, held := hub.Transport(environment).Registration(); held {
			driverName = r.Driver
		}
		states := hub.Workers(environment)
		out := make([]controller.Registration, 0, len(states))
		for _, w := range states {
			out = append(out, controller.Registration{
				Worker: w.Worker, Driver: driverName,
				LastHeartbeat: w.LastHeartbeat, Connected: w.Connected, Pending: w.Pending, Host: w.Host,
			})
		}
		return out
	}
}

// hostPending writes, on the sandbox a held create is for, that the host has
// not decided yet. It runs on the stream's read pump, so a failure is logged
// rather than returned: the create goes on either way, and the next write of
// the sandbox says how it ended.
func (h *handler) hostPending(environment, sandbox string) {
	if err := h.Controller.HostPending(context.Background(), environment, sandbox); err != nil {
		h.log.Warn("the sandbox could not be marked as waiting for its host", "sandbox", sandbox, "err", err)
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
func (h *handler) serveWorkerRoute(w http.ResponseWriter, r *http.Request, route workerRoute, key auth.Caller) {
	environment, _ := key.Environment()
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
		h.registerWorker(w, r, key, environment)
		return
	}
	h.workerStream(w, r, key, environment)
}

// keyID is the jti of the key a caller authenticated with.
func keyID(key auth.Caller) string {
	jti, _ := key.Claims["jti"].(string)
	return jti
}

// serveResource is environment.serve's resource: the environment and the
// registration the worker declared, its host rendered as the registration
// carried it on the wire.
func serveResource(obj v1.Environment, r remote.Registration) authz.Resource {
	return environmentResourceWith(obj, func(e *auth.Environment) {
		registration := &auth.Registration{Driver: r.Driver, Isolation: r.Isolation}
		if r.Host != nil {
			if raw, err := json.Marshal(r.Host); err == nil {
				var host map[string]any
				if json.Unmarshal(raw, &host) == nil {
					registration.Host = host
				}
			}
		}
		e.Registration = registration
	})
}

// decideServe asks environment.serve for one worker of one environment. A
// refusal, and an environment that no longer exists, is ErrServeRefused; a
// question that produced no decision is the authorizer's error.
func (h *handler) decideServe(ctx context.Context, r *http.Request, key auth.Caller, environment string, reg remote.Registration) (auth.Decision, v1.Environment, error) {
	obj, err := h.Controller.GetEnvironment(environment)
	if err != nil {
		return auth.Decision{}, obj, fmt.Errorf("%w: the environment %s is gone", remote.ErrServeRefused, environment)
	}
	d, err := h.Authorizer.Decide(ctx, key, requestInfo(r), authorizer.ActionEnvironmentServe, serveResource(obj, reg))
	if auth.CodeOf(err) == auth.CodeForbidden {
		return d, obj, fmt.Errorf("%w: %w", remote.ErrServeRefused, err)
	}
	return d, obj, err
}

// registerWorker records one worker and its driver and answers with the id it
// claims under. A worker that restarts registers again and its old claims
// lapse, so nothing is held by an id nobody answers for.
func (h *handler) registerWorker(w http.ResponseWriter, r *http.Request, key auth.Caller, environment string) {
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
	// The authorizer decides whether this key may serve the environment now
	// (spec 083): a removed environment or a closed account is refused here.
	d, obj, err := h.decideServe(r.Context(), r, key, environment, registration)
	if errors.Is(err, remote.ErrServeRefused) {
		respondError(w, &manifest.Error{Code: "serve_refused", Detail: err.Error()})
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	registered, replacement, err := h.Workers.RegisterWith(environment, registration, remote.Admission{
		JTI: keyID(key), MaxWorkers: obj.Spec.Workers.Max, TTL: d.TTL,
	})
	if replacement != nil {
		h.recordReplacement(r, obj, replacement)
	}
	if err != nil {
		switch {
		case errors.Is(err, remote.ErrRegistrationMismatch):
			respondError(w, &manifest.Error{Code: "environment_mismatch", Detail: err.Error()})
		case errors.Is(err, remote.ErrWorkerLimit):
			respondError(w, &manifest.Error{Code: "worker_limit", Detail: err.Error()})
		default:
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: err.Error()})
		}
		return
	}
	registered.Environment = environment
	// The environment reads Ready with this worker's host before the worker
	// learns it registered, so whoever reads it next sees it.
	h.observeEnvironment(r.Context(), environment)
	respond(w, http.StatusCreated, registered)
}

// observeEnvironment writes an environment's status after one of its workers
// registered or its stream ended, rather than at the phase loop's next tick.
// A failure is logged and not answered: the registration or the stream's end
// stands, and the next tick writes the status.
func (h *handler) observeEnvironment(ctx context.Context, environment string) {
	if err := h.Controller.ObserveEnvironment(ctx, environment); err != nil {
		h.log.WarnContext(ctx, "the environment's status was not written at once", "environment", environment, "err", err)
	}
}

// recordReplacement journals a registration that met a live worker holding
// the same key: environment.worker_replaced, with both declarations and
// whether it replaced that worker or was refused.
func (h *handler) recordReplacement(r *http.Request, obj v1.Environment, rep *remote.Replacement) {
	facts := func(reg remote.Registration) *events.WorkerFacts {
		return &events.WorkerFacts{Driver: reg.Driver, Isolation: reg.Isolation, Host: reg.Host}
	}
	outcome := events.OutcomeRefused
	if rep.Replaced {
		outcome = events.OutcomeReplaced
	}
	h.journal(r, obj, events.TypeEnvironmentWorkerReplaced, events.Environment{
		Worker: rep.Worker, Previous: facts(rep.Previous), Next: facts(rep.Next), Outcome: outcome,
	})
}

// workerStream runs one worker's stream: the frames of spec 021 over one
// WebSocket the worker opened. The control plane never dials a worker, so
// everything it hands one travels here.
func (h *handler) workerStream(w http.ResponseWriter, r *http.Request, key auth.Caller, environment string) {
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
	defer h.goingAway(conn)()
	// environment.serve is asked again on every decision TTL while the
	// stream lives, so a removed environment or a closed account stops the
	// worker within one TTL even with a copied key (spec 083).
	recheck := func(ctx context.Context, reg remote.Registration) (time.Duration, error) {
		d, _, err := h.decideServe(ctx, r, key, environment, reg)
		return d.TTL, err
	}
	_ = h.Workers.ServeWith(r.Context(), environment, newWorkerSocket(conn), remote.ServeOptions{Recheck: recheck})
	// The stream is gone, and the environment no longer counts its worker.
	// The request's context ended with the stream, so the write runs under
	// one of its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), observeTimeout)
	defer cancel()
	h.observeEnvironment(ctx, environment)
}

// observeTimeout bounds the status write after a worker's stream ended,
// which runs after the request that held the stream has ended.
const observeTimeout = 10 * time.Second

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

// CloseWith ends the stream with a close code and the reason a worker reads
// to decide whether to reconnect, which is how the hub refuses to go on
// serving one (spec 083). The close frame is a control frame, which gorilla
// lets a second goroutine write beside the one that writes the stream.
func (s *workerSocket) CloseWith(code int, reason string) error {
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason),
		time.Now().Add(time.Second))
	return s.conn.Close()
}
