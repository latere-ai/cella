// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
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

// environmentItem answers or deletes one environment by name. A name this
// control plane does not hold is not_found, which is the same answer a caller
// of an environment it may not use receives.
func (h *handler) environmentItem(w http.ResponseWriter, r *http.Request) {
	obj, err := h.environment(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	action := authorizer.ActionEnvironmentRead
	if r.Method == http.MethodDelete {
		action = authorizer.ActionEnvironmentDelete
	}
	if _, err = h.decide(r, action, environmentResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		if err = h.Controller.DeleteEnvironment(r.Context(), obj.Metadata.Name); err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respondEnvironment(w, http.StatusOK, obj)
}

// environmentList answers the environments this control plane holds.
func (h *handler) environmentList(w http.ResponseWriter, r *http.Request) {
	if _, err := h.decide(r, authorizer.ActionEnvironmentList, auth.List(authorizer.ActionEnvironmentList)); err != nil {
		respondError(w, err)
		return
	}
	items := h.Controller.ListEnvironments()
	respond(w, http.StatusOK, map[string]any{"items": items, "next": ""})
}

// environmentApply is PUT /v1/environments/{name}: the create of an
// environment of that name or the update of the one that holds it, under the
// concurrency rule of design 008. The path names the object, so a body that
// names another is refused rather than quietly renamed.
func (h *handler) environmentApply(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("id"))
	existing, readErr := h.Controller.GetEnvironment(name)
	held := readErr == nil
	obj, ok := h.readEnvironment(w, r, name, existing, held)
	if !ok {
		return
	}
	action := authorizer.ActionEnvironmentCreate
	resource := environmentResource(obj)
	if held {
		action = authorizer.ActionEnvironmentUpdate
		resource = environmentResource(existing)
	} else {
		obj.Status.Owner = caller(r).Subject
	}
	if _, err := h.decide(r, action, resource); err != nil {
		respondError(w, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		respondError(w, err)
		return
	}
	stored, created, err := h.Controller.ApplyEnvironment(r.Context(), obj, version)
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/environments/"+stored.Metadata.Name)
	}
	respondEnvironment(w, status, stored)
}

// environmentCreate is POST /v1/environments: the body names the environment,
// and a name another environment already holds is name_taken.
func (h *handler) environmentCreate(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	decoded, err := manifest.DecodeEnvironment(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return
	}
	obj, err := h.resolveEnvironment(r, decoded, nil)
	if err != nil {
		respondError(w, err)
		return
	}
	obj.Status.Owner = caller(r).Subject
	if _, err = h.decide(r, authorizer.ActionEnvironmentCreate, environmentResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	// The name is checked after the decision, so a caller who may not create
	// an environment learns that rather than learning which names are taken.
	if _, err = h.Controller.GetEnvironment(obj.Metadata.Name); err == nil {
		respondError(w, controller.ErrNameTaken)
		return
	}
	stored, _, err := h.Controller.ApplyEnvironment(r.Context(), obj, 0)
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/environments/"+stored.Metadata.Name)
	respondEnvironment(w, http.StatusCreated, stored)
}

// readEnvironment decodes and resolves the body of one apply against the
// object the path names.
func (h *handler) readEnvironment(w http.ResponseWriter, r *http.Request, name string,
	existing v1.Environment, held bool,
) (v1.Environment, bool) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return v1.Environment{}, false
	}
	decoded, err := manifest.DecodeEnvironment(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return v1.Environment{}, false
	}
	if decoded.Metadata.Name == "" {
		decoded.Metadata.Name = name
	}
	if decoded.Metadata.Name != name {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "metadata.name", Detail: "the path names " + name})
		return v1.Environment{}, false
	}
	var previous *v1.Environment
	if held {
		previous = &existing
	}
	obj, err := h.resolveEnvironment(r, decoded, previous)
	if err != nil {
		respondError(w, err)
		return v1.Environment{}, false
	}
	return obj, true
}

// resolveEnvironment validates and defaults one applied environment against
// what the driver behind it declares. An environment no worker has registered
// on declares nothing, which is what defers the rules that read a capability
// to the registration that brings one.
func (h *handler) resolveEnvironment(r *http.Request, decoded v1.Environment, existing *v1.Environment) (v1.Environment, error) {
	resolved, err := manifest.ResolveEnvironment(&decoded, manifest.EnvironmentOptions{
		Actor:        manifestActor(r),
		Existing:     existing,
		Capabilities: h.Controller.CapabilitiesOf(decoded.Metadata.Name),
		Now:          time.Now,
	})
	if err != nil {
		return v1.Environment{}, err
	}
	return *resolved, nil
}

// ifMatch reads the version an apply is conditional on. No header is a
// read-modify-write at the version the control plane last saw, which design
// 008 answers with a retry rather than a refusal.
func ifMatch(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" || raw == "*" {
		return 0, nil
	}
	version, err := strconv.ParseInt(strings.Trim(raw, `"`), 10, 64)
	if err != nil || version < 0 {
		return 0, &manifest.Error{Code: "invalid_field", Path: "If-Match",
			Detail: "an If-Match carries the version the ETag of a read returned"}
	}
	return version, nil
}

// respondEnvironment writes one environment with the ETag an If-Match is
// compared against.
func respondEnvironment(w http.ResponseWriter, status int, obj v1.Environment) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(obj.Status.Version, 10)+`"`)
	respond(w, status, obj)
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

// environment reads one environment by the name a route named. The empty name
// asks for the default, which is what a manifest with no spec.environment
// resolves against.
func (h *handler) environment(nameOrID string) (v1.Environment, error) {
	return h.Controller.GetEnvironment(strings.TrimSpace(nameOrID))
}

// environmentSubject is what an environment key names in its sub, which is
// the environment's id. An environment's name is global and fixed at create,
// so the id is the name (spec 021).
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
