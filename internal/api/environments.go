// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// EnvironmentKeys is the mint and the revocation of the credential a data
// plane carries, as the two key routes use it. It is optional: a control
// plane with no signer serves no key route, and the data plane roles of an
// installation like that carry a key an operator minted elsewhere.
type EnvironmentKeys interface {
	Mint(ctx context.Context, environment string) (auth.Token, error)
	Revoke(ctx context.Context, jti string) error
}

// environmentItem answers one environment by name or id. The control plane
// drives one environment today and it is the default; a request for another
// is not_found, which is the same answer a caller of an environment it may
// not use receives.
func (h *handler) environmentItem(w http.ResponseWriter, r *http.Request) {
	obj, err := h.environment(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, authorizer.ActionEnvironmentRead, environmentResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, obj)
}

// environmentList answers the environments this control plane holds.
func (h *handler) environmentList(w http.ResponseWriter, r *http.Request) {
	if _, err := h.decide(r, authorizer.ActionEnvironmentList, auth.List(authorizer.ActionEnvironmentList)); err != nil {
		respondError(w, err)
		return
	}
	obj, err := h.environment(h.Controller.Environment())
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"items": []v1.Environment{obj}, "next": ""})
}

// environmentKeyMint signs one environment key and returns it once. It is the
// route a data plane is installed from: an administrator mints a key, hands
// it to the worker or the gateway as CELLA_ENVIRONMENT_KEY, and that role
// connects outbound with it. The control plane keeps no copy, so a key that
// is lost is replaced rather than retrieved.
func (h *handler) environmentKeyMint(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		respondError(w, &manifest.Error{Code: "capability_unsupported",
			Detail: "this control plane signs no environment keys; CELLA_TOKEN_KEY is what mints one"})
		return
	}
	obj, err := h.environment(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, authorizer.ActionEnvironmentKey, environmentResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	token, err := h.Keys.Mint(r.Context(), environmentSubject(obj))
	if err != nil {
		respondError(w, err)
		return
	}
	h.journal(r, obj, events.TypeEnvironmentKeyed, events.Environment{JTI: token.JTI})
	respond(w, http.StatusCreated, map[string]any{
		"token": token.Value,
		"jti":   token.JTI,
		"exp":   token.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// environmentKeyRevoke ends one key by the jti its mint returned. Every
// stream and every route reads the revocation list, so the key stops working
// on its next frame and its next request.
func (h *handler) environmentKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		respondError(w, &manifest.Error{Code: "capability_unsupported",
			Detail: "this control plane signs no environment keys, so it revokes none"})
		return
	}
	obj, err := h.environment(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, authorizer.ActionEnvironmentKey, environmentResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	jti := strings.TrimSpace(r.PathValue("jti"))
	if jti == "" {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "jti", Detail: "a revocation names the jti the mint returned"})
		return
	}
	if err = h.Keys.Revoke(r.Context(), jti); err != nil {
		respondError(w, err)
		return
	}
	h.journal(r, obj, events.TypeEnvironmentKeyRevoked, events.Environment{JTI: jti})
	w.WriteHeader(http.StatusNoContent)
}

// environment reads one environment by the name or id a route named. The
// empty name asks for the default, which is what a manifest with no
// spec.environment resolves against.
func (h *handler) environment(nameOrID string) (v1.Environment, error) {
	name := strings.TrimSpace(nameOrID)
	if name == "" || name == "default" && h.Controller.Environment() == "default" {
		name = h.Controller.Environment()
	}
	if name != h.Controller.Environment() {
		return v1.Environment{}, &manifest.Error{Code: "not_found", Detail: "no environment of that name"}
	}
	obj := v1.Environment{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindEnvironment,
		Metadata:   v1.Metadata{Name: name},
		Spec: v1.EnvironmentSpec{
			Mode:      v1.EnvironmentInprocess,
			Isolation: h.Controller.Isolation(),
		},
		Status: v1.EnvironmentStatus{
			ID:           name,
			Phase:        v1.EnvironmentReady,
			Driver:       h.Controller.Driver(),
			Isolation:    h.Controller.Isolation(),
			Capabilities: h.Controller.Capabilities(),
		},
	}
	// The workers holding a stream open are what a self-hosted environment's
	// phase is computed from, and what an operator reads to see that the
	// data plane arrived. A worker that registered and dropped its stream is
	// not counted: the environment cannot be placed on through it.
	if h.Workers != nil {
		for _, w := range h.Workers.Workers(environmentSubject(obj)) {
			if !w.Connected {
				continue
			}
			obj.Status.Workers++
			if w.LastHeartbeat.After(obj.Status.LastHeartbeat) {
				obj.Status.LastHeartbeat = w.LastHeartbeat
			}
		}
	}
	if h.Egress != nil {
		obj.Status.Gateways = h.Egress.Connected()
	}
	return obj, nil
}

// environmentSubject is what an environment key names in its sub. It is the
// environment's id, which is its name on a control plane that drives its own
// environment and has minted no id for it.
func environmentSubject(obj v1.Environment) string {
	if obj.Status.ID != "" {
		return obj.Status.ID
	}
	return obj.Metadata.Name
}

func environmentResource(obj v1.Environment) authz.Resource {
	return (auth.Environment{
		ID: environmentSubject(obj), Name: obj.Metadata.Name,
		Owner: obj.Status.Owner, Isolation: obj.Spec.Isolation, Labels: obj.Metadata.Labels,
	}).Resource()
}

// journal records one act on an environment. A control plane with no emitter
// records nothing, which is the same rule every other route follows.
func (h *handler) journal(r *http.Request, obj v1.Environment, kind events.Type, payload events.Environment) {
	if h.Events == nil {
		return
	}
	record, err := events.Operation(kind, events.OfEnvironment(obj), payload,
		events.ActorFrom(r.Context()), time.Now().UTC())
	if err != nil {
		slog.WarnContext(r.Context(), "the environment event was not built",
			"type", kind, "environment", obj.Metadata.Name, "error", err)
		return
	}
	h.Events.Write(r.Context(), record)
}
