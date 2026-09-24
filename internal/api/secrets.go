// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// secretResource renders one Secret for the authorizer.
func secretResource(obj v1.Secret) authz.Resource {
	return (auth.Secret{ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner, Labels: obj.Metadata.Labels}).Resource()
}

// createSecret is POST /v1/secrets: a create whose name must be free.
func (h *handler) createSecret(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.readSecret(w, r, nil)
	if !ok {
		return
	}
	obj.Status.Owner = caller(r).Subject
	if _, err := h.decide(r, authorizer.ActionSecretCreate, secretResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	stored, err := h.Controller.CreateSecret(r.Context(), obj, caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", h.public("/v1/secrets/"+stored.Status.ID))
	respond(w, http.StatusCreated, stored)
}

// applySecret is PUT /v1/secrets/{key}: a create when the name is free and an
// update when the caller already holds it, which is the grammar every kind of
// this API shares.
func (h *handler) applySecret(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	existing, err := h.Controller.GetSecret(r.Context(), key, caller(r).Subject)
	if errors.Is(err, controller.ErrNotFound) {
		h.createNamedSecret(w, r, key)
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, authorizer.ActionSecretUpdate, secretResource(existing)); err != nil {
		respondError(w, err)
		return
	}
	obj, ok := h.readSecret(w, r, &existing)
	if !ok {
		return
	}
	if obj.Metadata.Name == "" {
		obj.Metadata.Name = existing.Metadata.Name
	}
	stored, err := h.Controller.UpdateSecret(r.Context(), obj, existing.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, stored)
}

// createNamedSecret is the create half of a PUT: the path names the object,
// so a body that names another is refused rather than quietly renamed.
func (h *handler) createNamedSecret(w http.ResponseWriter, r *http.Request, key string) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	decoded, err := manifest.DecodeSecret(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return
	}
	if decoded.Metadata.Name == "" {
		decoded.Metadata.Name = key
	}
	if decoded.Metadata.Name != key {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "metadata.name", Detail: "the path names " + key})
		return
	}
	resolved, err := manifest.ResolveSecret(&decoded, manifest.SecretOptions{Actor: manifestActor(r)})
	if err != nil {
		respondError(w, err)
		return
	}
	resolved.Status.Owner = caller(r).Subject
	if _, err = h.decide(r, authorizer.ActionSecretCreate, secretResource(*resolved)); err != nil {
		respondError(w, err)
		return
	}
	stored, err := h.Controller.CreateSecret(r.Context(), *resolved, caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", h.public("/v1/secrets/"+stored.Status.ID))
	respond(w, http.StatusCreated, stored)
}

// readSecret decodes and resolves one body. The object it returns carries the
// value the caller wrote, which the controller hands to the store and no
// response ever carries back.
func (h *handler) readSecret(w http.ResponseWriter, r *http.Request, existing *v1.Secret) (v1.Secret, bool) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return v1.Secret{}, false
	}
	decoded, err := manifest.DecodeSecret(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return v1.Secret{}, false
	}
	if existing != nil && decoded.Metadata.Name == "" {
		decoded.Metadata.Name = existing.Metadata.Name
	}
	resolved, err := manifest.ResolveSecret(&decoded, manifest.SecretOptions{Actor: manifestActor(r), Existing: existing})
	if err != nil {
		respondError(w, err)
		return v1.Secret{}, false
	}
	return *resolved, true
}

// secretItem is GET and DELETE of one Secret.
func (h *handler) secretItem(w http.ResponseWriter, r *http.Request) {
	action := authorizer.ActionSecretRead
	if r.Method == http.MethodDelete {
		action = authorizer.ActionSecretDelete
	}
	obj, err := h.Controller.GetSecret(r.Context(), r.PathValue("key"), caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, action, secretResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	if r.Method != http.MethodDelete {
		respond(w, http.StatusOK, obj)
		return
	}
	deleted, err := h.Controller.DeleteSecret(r.Context(), obj.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, deleted)
}

// listSecrets is GET /v1/secrets, narrowed to what the actor may see. A row
// the authorizer refuses is left out rather than refused, so a filter answers
// an empty page and never a 403, and no answer carries a value.
func (h *handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	d, err := h.decide(r, authorizer.ActionSecretList, auth.List(authorizer.ActionSecretList))
	if err != nil {
		respondError(w, err)
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, err = strconv.Atoi(q)
		if err != nil || limit < 1 || limit > 200 {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "limit must be between 1 and 200"})
			return
		}
	}
	items := []v1.Secret{}
	next := ""
	cursor := r.URL.Query().Get("cursor")
	for _, obj := range h.Controller.ListSecrets() {
		if obj.Status.ID <= cursor {
			continue
		}
		if !admits(d.Filter, obj.Status.Owner, obj.Metadata.Labels) {
			continue
		}
		if _, err = h.decide(r, authorizer.ActionSecretRead, secretResource(obj)); err != nil {
			if auth.CodeOf(err) == auth.CodeForbidden {
				continue
			}
			respondError(w, err)
			return
		}
		if len(items) == limit {
			next = items[len(items)-1].Status.ID
			break
		}
		items = append(items, obj)
	}
	respond(w, http.StatusOK, map[string]any{"items": items, "next": next})
}

// secretLookup is the manifest resolver's Lookup half for one request: the
// object this caller may mount, by sec_ id or by a name among its own, with
// the authorizer's secret.mount decision folded in. A refusal and an absence
// are the same answer, so a mount that is not allowed does not tell the
// caller that the object exists.
func (h *handler) secretLookup(r *http.Request) manifest.SecretFunc {
	return func(ctx context.Context, nameOrID string) (*v1.Secret, error) {
		obj, err := h.Controller.SecretFor(ctx, nameOrID, caller(r).Subject)
		if err != nil {
			return nil, err
		}
		if _, err = h.Authorizer.Lookup(ctx, caller(r), requestInfo(r), authorizer.ActionSecretMount, secretResource(*obj)); err != nil {
			if auth.CodeOf(err) == auth.CodeNotFound || auth.CodeOf(err) == auth.CodeForbidden {
				return nil, manifest.ErrNotFound
			}
			return nil, err
		}
		return obj, nil
	}
}

// manifestActor is who is applying, as the manifest package reads it: the
// rendered subject, the two halves it was rendered from, and whether the
// bearer was a sandbox's own token. The halves are the verified claims and
// not the rendered subject taken apart, so the admission step of spec 007
// and the authorizer of spec 006 are handed the same identity.
func manifestActor(r *http.Request) manifest.Actor {
	c := caller(r)
	_, workload := c.Sandbox()
	return manifest.Actor{Subject: c.Subject, Issuer: c.Issuer, Sub: c.Sub, Workload: workload}
}
