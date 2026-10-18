// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// volumeResource renders one Volume for the authorizer.
func volumeResource(obj v1.Volume) authz.Resource {
	return (auth.Volume{ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner,
		Environment: obj.Spec.Environment, Labels: obj.Metadata.Labels}).Resource()
}

// applyVolume is PUT /v1/volumes/{key}: a create when the name is free and an
// update when the caller already holds it, which is the grammar every kind of
// this API shares. An update reads and resolves its body before it asks the
// authorizer, so volume.update is asked about the stored Volume together with
// the one the body would write.
func (h *handler) applyVolume(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	existing, err := h.Controller.GetVolume(r.Context(), key, caller(r).Subject)
	if errors.Is(err, controller.ErrNotFound) {
		h.createVolume(w, r, key)
		return
	}
	if err != nil {
		respondError(w, err)
		return
	}
	obj, ok := h.readVolume(w, r, key, &existing)
	if !ok {
		return
	}
	res := withProposal(volumeResource(existing), existing.Status.Owner, obj.Metadata, obj.Spec)
	if _, err = h.decide(r, authorizer.ActionVolumeUpdate, res); err != nil {
		respondError(w, err)
		return
	}
	stored, err := h.Controller.UpdateVolume(r.Context(), obj, existing.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, stored)
}

// createVolume is the create half of a PUT. volume.create is asked with the
// Volume the create would store as its proposal: readVolume has resolved the
// body, so the source kind and the other defaults are filled, the environment
// is named, and an image source is the digest-pinned reference admission
// answered, which is what lets an authorizer hold a create to a source, an
// image or a size. The volume lives in an environment, so the caller's use of
// it is asked as a sandbox's create asks it, besides volume.create.
func (h *handler) createVolume(w http.ResponseWriter, r *http.Request, key string) {
	obj, ok := h.readVolume(w, r, key, nil)
	if !ok {
		return
	}
	c := caller(r)
	obj.Status.Owner = c.Subject
	res := withProposal(volumeResource(obj), c.Subject, obj.Metadata, obj.Spec)
	if _, err := h.decide(r, authorizer.ActionVolumeCreate, res); err != nil {
		respondError(w, err)
		return
	}
	environment := (auth.Environment{ID: obj.Spec.Environment, Name: obj.Spec.Environment,
		Isolation: h.Controller.IsolationOf(obj.Spec.Environment)}).Resource()
	if _, err := h.Authorizer.Lookup(r.Context(), c, requestInfo(r), authorizer.ActionEnvironmentUse, environment); err != nil {
		respondError(w, err)
		return
	}
	stored, err := h.Controller.CreateVolume(r.Context(), obj, c.Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", h.public("/v1/volumes/"+stored.Status.ID))
	respond(w, http.StatusCreated, stored)
}

// readVolume decodes one body, holds it to the name the route named, and
// resolves it: the field rules, the environment, the admission of an image
// source and the digest rule, and immutability against the stored object.
// The path names the object, so a body that names another is refused rather
// than quietly renamed.
func (h *handler) readVolume(w http.ResponseWriter, r *http.Request, key string, existing *v1.Volume) (v1.Volume, bool) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return v1.Volume{}, false
	}
	decoded, err := manifest.DecodeVolume(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return v1.Volume{}, false
	}
	name := key
	if existing != nil {
		name = existing.Metadata.Name
	}
	if decoded.Metadata.Name == "" {
		decoded.Metadata.Name = name
	}
	if decoded.Metadata.Name != name {
		respondError(w, &manifest.Error{Code: "invalid_field", Path: "metadata.name", Detail: "the path names " + key})
		return v1.Volume{}, false
	}
	resolved, err := manifest.ResolveVolume(r.Context(), &decoded, manifest.VolumeOptions{
		Actor: manifestActor(r), Lookup: h.Controller.Lookup(), Admit: h.Admit,
		Claims: caller(r).Claims, RequestID: w.Header().Get(RequestIDHeader), Existing: existing,
	})
	if err != nil {
		respondError(w, err)
		return v1.Volume{}, false
	}
	return *resolved, true
}

// volumeItem is GET and DELETE of one Volume, by its id or by the name its
// owner gave it.
func (h *handler) volumeItem(w http.ResponseWriter, r *http.Request) {
	action := authorizer.ActionVolumeRead
	if r.Method == http.MethodDelete {
		action = authorizer.ActionVolumeDelete
	}
	obj, err := h.Controller.GetVolume(r.Context(), r.PathValue("key"), caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, action, volumeResource(obj)); err != nil {
		respondError(w, err)
		return
	}
	if r.Method != http.MethodDelete {
		respond(w, http.StatusOK, obj)
		return
	}
	deleted, err := h.Controller.DeleteVolume(r.Context(), obj.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, deleted)
}

// listVolumes is GET /v1/volumes, narrowed to what the actor may see. The
// owner and label selectors narrow the collection before the authorizer is
// asked, as on the secret list, so a selector never widens what the caller
// may read, and a row the authorizer refuses is left out rather than refused.
func (h *handler) listVolumes(w http.ResponseWriter, r *http.Request) {
	d, err := h.decide(r, authorizer.ActionVolumeList, auth.List(authorizer.ActionVolumeList))
	if err != nil {
		respondError(w, err)
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		respondError(w, err)
		return
	}
	labels, satisfiable, err := labelSelector(r)
	if err != nil {
		respondError(w, err)
		return
	}
	items := []v1.Volume{}
	next := ""
	if !satisfiable {
		respond(w, http.StatusOK, map[string]any{"items": items, "next": next})
		return
	}
	q := r.URL.Query()
	cursor, owner := q.Get("cursor"), q.Get("owner")
	for _, obj := range h.Controller.ListVolumes() {
		if obj.Status.ID <= cursor || (owner != "" && owner != obj.Status.Owner) || !carries(obj.Metadata.Labels, labels) {
			continue
		}
		if !admits(d.Filter, obj.Status.Owner, obj.Metadata.Labels) {
			continue
		}
		if _, err = h.decide(r, authorizer.ActionVolumeRead, volumeResource(obj)); err != nil {
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

// volumeLookup is the manifest resolver's Lookup half for one request: the
// volume this caller may attach, by vol_ id or by a name among its own, with
// the authorizer's volume.attach decision folded in. A refusal and an
// absence are the same answer, so a mount that is not allowed does not tell
// the caller that the object exists.
func (h *handler) volumeLookup(r *http.Request) manifest.VolumeFunc {
	return func(ctx context.Context, nameOrID string) (*v1.Volume, error) {
		obj, err := h.Controller.VolumeFor(ctx, nameOrID, caller(r).Subject)
		if err != nil {
			return nil, err
		}
		if _, err = h.Authorizer.Lookup(ctx, caller(r), requestInfo(r), authorizer.ActionVolumeAttach, volumeResource(*obj)); err != nil {
			if auth.CodeOf(err) == auth.CodeNotFound || auth.CodeOf(err) == auth.CodeForbidden {
				return nil, manifest.ErrNotFound
			}
			return nil, err
		}
		return obj, nil
	}
}
