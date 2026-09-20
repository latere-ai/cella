// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"net/http"
	"time"

	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// actorOf is the actor of design 009 for one request: the caller's rendered
// subject, the sandbox id where the bearer was that sandbox's own token, and
// the request id this response already carries.
func actorOf(w http.ResponseWriter, c auth.Caller) events.Actor {
	id, _ := c.Sandbox()
	return events.Actor{Subject: c.Subject, Workload: id, RequestID: w.Header().Get("X-Request-ID")}
}

// emit records one operation: an act on a sandbox that changes no desired
// state and still belongs in the feed. The record names the operation and
// never its content, which is why data is one of the fixed shapes of
// internal/events and never a request or response body.
//
// A record that cannot be journaled is logged and never returned: the
// operation the caller asked for has already happened.
func (h *handler) emit(r *http.Request, obj v1.Sandbox, kind events.Type, data any) {
	if h.Events == nil {
		return
	}
	record, err := events.Operation(kind, events.OfSandbox(obj), data,
		events.ActorFrom(r.Context()), time.Now().UTC())
	if err != nil {
		slog.WarnContext(r.Context(), "the operation event was not built",
			"type", kind, "sandbox", obj.Status.ID, "error", err)
		return
	}
	h.Events.Write(r.Context(), record)
}
