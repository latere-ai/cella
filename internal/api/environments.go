// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// EnvironmentKeys is the mint, the revocation and the listing of the
// credential a data plane carries, as the three key routes use them. It is
// optional: a control plane with no signer serves no key route, and the data
// plane roles of an installation like that carry a key an operator minted
// elsewhere.
type EnvironmentKeys interface {
	// Lifetime is the lifetime a mint asking for ttl gives the key: ttl,
	// the configured lifetime for zero, and auth.ErrKeyTTL beyond it.
	Lifetime(ttl time.Duration) (time.Duration, error)
	// Mint signs one key for the environment on behalf of the subject that
	// asked, with the lifetime asked for, which the registry records with
	// the key's minter.
	Mint(ctx context.Context, environment, subject string, ttl time.Duration) (auth.Token, error)
	Revoke(ctx context.Context, jti string) error
	// List is one page of the environment's keys, oldest first, and the
	// cursor of the next page.
	List(ctx context.Context, environment, cursor string, limit int) ([]auth.KeyRecord, string, error)
	// Find is one key's record by its jti, and false where none is kept.
	Find(ctx context.Context, jti string) (auth.KeyRecord, bool, error)
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
		// A worker environment's keys end with it, before the object goes,
		// so no worker registers on a name that is about to be free; its
		// streams close with serve_refused as the controller releases it
		// (spec 083).
		if obj.Spec.Mode == v1.EnvironmentWorker {
			if err = h.revokeAll(r, obj); err != nil {
				respondError(w, err)
				return
			}
		}
		if err = h.Controller.DeleteEnvironment(r.Context(), obj.Metadata.Name); err != nil {
			respondError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	respondEnvironment(w, http.StatusOK, obj)
}

// environmentList answers the environments the caller may read, under the list
// rule every list route follows: the list decision, its filter over each
// environment's owner and labels, then environment.read on each environment
// the filter admits. A refused read leaves the environment out; a read that
// produced no decision refuses the page, which is never answered around it.
// The default environment is decided by its read alone (admitsEnvironment).
func (h *handler) environmentList(w http.ResponseWriter, r *http.Request) {
	d, err := h.decide(r, authorizer.ActionEnvironmentList, auth.List(authorizer.ActionEnvironmentList))
	if err != nil {
		respondError(w, err)
		return
	}
	items := []v1.Environment{}
	for _, obj := range h.Controller.ListEnvironments() {
		if !admitsEnvironment(d.Filter, h.Controller.Environment(), obj.Metadata.Name, obj.Status.Owner, obj.Metadata.Labels) {
			continue
		}
		if _, err = h.decide(r, authorizer.ActionEnvironmentRead, environmentResource(obj)); err != nil {
			if auth.CodeOf(err) == auth.CodeForbidden {
				continue
			}
			respondError(w, err)
			return
		}
		items = append(items, obj)
	}
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
		w.Header().Set("Location", h.public("/v1/environments/"+stored.Metadata.Name))
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
	w.Header().Set("Location", h.public("/v1/environments/"+stored.Metadata.Name))
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
	asked, err := h.keyLifetime(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	// The question carries the lifetime the key will get, so an authorizer
	// can refuse a long one (spec 083).
	resource := environmentResourceWith(obj, func(e *auth.Environment) { e.TTL = int64(asked / time.Second) })
	if _, err = h.decide(r, authorizer.ActionEnvironmentKey, resource); err != nil {
		respondError(w, err)
		return
	}
	token, err := h.Keys.Mint(r.Context(), environmentSubject(obj), caller(r).Subject, asked)
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

// keyItem is one key as the listing answers it. The token is not a field: the
// control plane keeps none, and a listing is not a way to recover one. exp is
// the name the mint's own answer uses, so a console joins the two by it.
type keyItem struct {
	JTI       string `json:"jti"`
	MintedAt  string `json:"mintedAt"`
	Exp       string `json:"exp"`
	Revoked   bool   `json:"revoked"`
	RevokedAt string `json:"revokedAt,omitempty"`
	MintedBy  string `json:"mintedBy,omitempty"`
}

// environmentKeyList is GET /v1/environments/{id}/keys: the keys minted for
// one environment, oldest first and a page at a time, with when each was
// minted, when it expires, whether it is revoked and who minted it. Listing an
// environment's credentials is an administrator's act on them, the same one
// the mint and the revocation are, so it is authorized as environment.key.
func (h *handler) environmentKeyList(w http.ResponseWriter, r *http.Request) {
	if h.Keys == nil {
		respondError(w, &manifest.Error{Code: "capability_unsupported",
			Detail: "this control plane signs no environment keys, so it holds none to list"})
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
	limit, err := pageLimit(r)
	if err != nil {
		respondError(w, err)
		return
	}
	records, next, err := h.Keys.List(r.Context(), environmentSubject(obj), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		respondError(w, err)
		return
	}
	items := make([]keyItem, 0, len(records))
	for _, k := range records {
		item := keyItem{
			JTI: k.JTI, MintedAt: k.MintedAt.UTC().Format(time.RFC3339), Exp: k.ExpiresAt.UTC().Format(time.RFC3339),
			Revoked: !k.RevokedAt.IsZero(), MintedBy: k.Subject,
		}
		if item.Revoked {
			item.RevokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	respond(w, http.StatusOK, map[string]any{"items": items, "next": next})
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
	// A key recorded for another environment is not this one's to revoke,
	// which matters once a subject that owns an environment revokes its
	// keys. A jti with no record is a key minted before its record could
	// be kept, and is revoked as before.
	record, held, err := h.Keys.Find(r.Context(), jti)
	if err != nil {
		respondError(w, err)
		return
	}
	if held && record.Environment != environmentSubject(obj) {
		respondError(w, &manifest.Error{Code: "not_found", Path: "jti", Detail: "the key belongs to another environment"})
		return
	}
	if err = h.Keys.Revoke(r.Context(), jti); err != nil {
		respondError(w, err)
		return
	}
	// The stream the key opened closes at once with key_revoked.
	if h.Workers != nil {
		h.Workers.Revoke(jti)
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
	return environmentResourceWith(obj, nil)
}

// environmentResourceWith is the environment as the envelope carries it,
// with what one question adds: a key's lifetime, a worker's registration.
func environmentResourceWith(obj v1.Environment, add func(*auth.Environment)) authz.Resource {
	e := auth.Environment{
		ID: environmentSubject(obj), Name: obj.Metadata.Name,
		Owner: obj.Status.Owner, Isolation: obj.Spec.Isolation, Mode: obj.Spec.Mode,
		Workers: obj.Spec.Workers.Max, Labels: obj.Metadata.Labels,
	}
	if add != nil {
		add(&e)
	}
	return e.Resource()
}

// keyRequest is a key mint's optional body: the lifetime it asks for, a
// duration such as 720h.
type keyRequest struct {
	TTL string `json:"ttl"`
}

// keyLifetime reads the lifetime a key mint asks for: the body's optional
// ttl, a duration such as 720h, held to CELLA_ENVIRONMENT_KEY_TTL. An empty
// body asks for the configured lifetime, which is what the answer is.
func (h *handler) keyLifetime(w http.ResponseWriter, r *http.Request) (time.Duration, error) {
	body, err := h.readBody(w, r)
	if err != nil {
		return 0, err
	}
	var asked time.Duration
	if strings.TrimSpace(string(body)) != "" {
		var request keyRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&request); err != nil {
			return 0, &manifest.Error{Code: "bad_request", Detail: err.Error()}
		}
		if request.TTL != "" {
			if asked, err = time.ParseDuration(request.TTL); err != nil {
				return 0, &manifest.Error{Code: "invalid_field", Path: "ttl", Detail: "a ttl is a duration such as 720h: " + err.Error()}
			}
			if asked <= 0 {
				return 0, &manifest.Error{Code: "invalid_field", Path: "ttl", Detail: "a ttl is positive"}
			}
		}
	}
	lifetime, err := h.Keys.Lifetime(asked)
	if errors.Is(err, auth.ErrKeyTTL) {
		return 0, &manifest.Error{Code: "ceiling_exceeded", Path: "ttl", Detail: err.Error()}
	}
	return lifetime, err
}

// revokeAll revokes every key an environment holds that is not revoked yet,
// a page at a time.
func (h *handler) revokeAll(r *http.Request, obj v1.Environment) error {
	if h.Keys == nil {
		return nil
	}
	cursor := ""
	for {
		records, next, err := h.Keys.List(r.Context(), environmentSubject(obj), cursor, maxPageLimit)
		if err != nil {
			return err
		}
		for _, k := range records {
			if !k.RevokedAt.IsZero() {
				continue
			}
			if err = h.Keys.Revoke(r.Context(), k.JTI); err != nil {
				return err
			}
			h.journal(r, obj, events.TypeEnvironmentKeyRevoked, events.Environment{JTI: k.JTI})
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
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
