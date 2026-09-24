// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// actorOf is the actor of design 009 for one request: the caller's rendered
// subject, the sandbox id where the bearer was that sandbox's own token, and
// the request id this response already carries.
func actorOf(w http.ResponseWriter, c auth.Caller) events.Actor {
	id, _ := c.Sandbox()
	return events.Actor{Subject: c.Subject, Workload: id, RequestID: w.Header().Get(RequestIDHeader)}
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

// eventFeed serves GET /v1/events: design 009's records for one object,
// newest first and paged by the sequence the journal assigned, or with
// follow=1 the following feed of follow.go. The handler reads the object,
// derives its kind from the id, and authorizes that kind's read, so a feed
// tells a caller nothing a read of the object would not.
//
// The route is mounted as a stream: a page negotiates its syntax here, and a
// following feed answers newline-delimited JSON whatever Accept names.
func (h *handler) eventFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("follow") {
	case "", "0":
	case "1":
		h.followFeed(w, r)
		return
	default:
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "follow", Detail: "follow must be 0 or 1"})
		return
	}
	if !acceptable(w, r) {
		return
	}
	object := q.Get("object")
	if object == "" {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "object",
			Detail: "a page is one object's; name one with ?object=, or follow every object with ?follow=1"})
		return
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			respondError(w, &manifest.Error{Code: "invalid_field", Path: "limit", Detail: "limit must be between 1 and 200"})
			return
		}
		limit = parsed
	}
	id, resource, action, err := h.feedObject(r, object)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, action, resource); err != nil {
		respondError(w, err)
		return
	}
	items, next, err := h.Events.Feed(r.Context(), id, q.Get("cursor"), limit)
	if err != nil {
		respondError(w, err)
		return
	}
	if items == nil {
		items = []events.Record{}
	}
	respond(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// feedObject is the object a feed names: its stored id, what the authorizer
// decides on, and the action of design 009's rule that a feed is read under
// the object's own kind. The kind comes from the prefix design 001 gives an
// id. An environment's id is its name (spec 021), so a key naming an
// environment this control plane holds is that environment; any other key is
// a sandbox's id or its name.
func (h *handler) feedObject(r *http.Request, key string) (string, authz.Resource, string, error) {
	if strings.HasPrefix(key, v1.SecretIDPrefix) {
		obj, err := h.Controller.GetSecret(r.Context(), key, caller(r).Subject)
		if err != nil {
			return "", authz.Resource{}, "", err
		}
		return obj.Status.ID, secretResource(obj), authorizer.ActionSecretRead, nil
	}
	switch env, err := h.Controller.GetEnvironment(key); {
	case err == nil:
		return environmentSubject(env), environmentResource(env), authorizer.ActionEnvironmentRead, nil
	case strings.HasPrefix(key, v1.EnvironmentIDPrefix):
		return "", authz.Resource{}, "", err
	}
	obj, err := h.Controller.Get(r.Context(), key, caller(r).Subject)
	if err != nil {
		return "", authz.Resource{}, "", err
	}
	return obj.Status.ID, resource(obj), authorizer.ActionSandboxRead, nil
}
