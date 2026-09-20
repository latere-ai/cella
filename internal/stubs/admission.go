// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// AdmissionOptions configures the admission role: the endpoint of spec
// 007 that reads a defaulted manifest with the identity that applied it
// and answers with the manifest an installation actually runs.
//
// The envelope is decoded into this package's own struct rather than the
// client's. The client is what this stub stands opposite, and a
// counterparty built from it would agree with every shape it sent,
// including a wrong one.
type AdmissionOptions struct {
	// Addr is the listen address, empty to turn the role off.
	Addr string
	// Token is the bearer the endpoint requires, empty for none. A bad
	// bearer is the 401 of spec 007, which is the one answer that is
	// unavailability and not a refusal.
	Token string
	// Defaults are operator defaults applied where the manifest leaves
	// the field empty, keyed by a dotted path under `spec`.
	Defaults map[string]string
	// Rewrites are applied whatever the manifest says, keyed the same
	// way: the image an installation pins is the case spec 007 names.
	Rewrites map[string]string
	// Warnings are carried on every allow.
	Warnings []string
	// Refuse is the reason every apply is refused with, empty to admit.
	Refuse string
	// RefuseImage refuses an apply naming that image, which is how a tier
	// drives one refusal without refusing the applies around it.
	RefuseImage string
	// Fail is one of the outage modes.
	Fail string
}

// admissionRequest is the body spec 007 states, as this endpoint reads
// it. The actor is flat on the wire, and the manifest is kept as it
// arrived so what is answered differs from what was sent only where this
// stub changed it.
type admissionRequest struct {
	applier
	// Claims are the caller's verified claims, verbatim. This endpoint
	// reads none of them; a real one reads whichever its policy needs.
	Claims map[string]any `json:"claims"`
	// Action is create or update, Existing the object an update replaces,
	// and Manifest the defaulted object stage 2 produced.
	Action   string          `json:"action"`
	Existing json.RawMessage `json:"existing"`
	Manifest json.RawMessage `json:"manifest"`
	Ref      struct {
		ID string `json:"id"`
	} `json:"request"`
}

// applier is the identity that applied, as spec 007 flattens it onto the
// wire. It is not the authorizer's envelope, which one shared package
// declares and this repository never re-declares.
type applier struct {
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	Sub     string `json:"sub"`
}

// admission is the role's state: what it was told to answer, and every
// envelope it has been sent.
type admission struct {
	o        AdmissionOptions
	outage   *outage
	mu       sync.Mutex
	requests []json.RawMessage
}

// newAdmission builds the admission endpoint's handler.
func newAdmission(o AdmissionOptions) (http.Handler, func(), error) {
	closed := make(chan struct{})
	out, err := parseOutage(o.Fail, closed)
	if err != nil {
		return nil, nil, err
	}
	for _, field := range slices.Sorted(fieldNames(o.Defaults, o.Rewrites)) {
		if strings.TrimSpace(field) == "" {
			return nil, nil, fmt.Errorf("a default or a rewrite names no field; the form is <field>=<value>")
		}
	}
	a := &admission{o: o, outage: out}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", a.admit)
	mux.HandleFunc("GET /requests", a.list)
	mux.HandleFunc("DELETE /requests", a.clear)
	var once sync.Once
	return mux, func() { once.Do(func() { close(closed) }) }, nil
}

// admit answers one apply.
func (a *admission) admit(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, a.o.Token) {
		http.Error(w, "bearer required", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.requests = append(a.requests, json.RawMessage(slices.Clone(body)))
	a.mu.Unlock()
	if a.outage.answer(w, r) {
		return
	}
	var req admissionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var manifest map[string]any
	if err := json.Unmarshal(req.Manifest, &manifest); err != nil || manifest == nil {
		http.Error(w, "the envelope carries no manifest object", http.StatusBadRequest)
		return
	}
	if a.o.Refuse != "" {
		writeJSON(w, http.StatusOK, map[string]any{"allow": false, "reason": a.o.Refuse})
		return
	}
	if image, _ := field(manifest, "image").(string); a.o.RefuseImage != "" && image == a.o.RefuseImage {
		writeJSON(w, http.StatusOK, map[string]any{"allow": false, "reason": "the image " + image + " is not in this installation's catalog"})
		return
	}
	for f, v := range a.o.Defaults {
		setField(manifest, f, v, true)
	}
	for f, v := range a.o.Rewrites {
		setField(manifest, f, v, false)
	}
	out := map[string]any{"allow": true, "manifest": manifest}
	if len(a.o.Warnings) > 0 {
		out["warnings"] = a.o.Warnings
	}
	writeJSON(w, http.StatusOK, out)
}

// list serves every envelope the endpoint has read, in order.
func (a *admission) list(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.requests == nil {
		writeJSON(w, http.StatusOK, []json.RawMessage{})
		return
	}
	writeJSON(w, http.StatusOK, a.requests)
}

// clear forgets them.
func (a *admission) clear(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.requests = nil
	a.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// field reads a dotted path under the manifest's spec, and nil where any
// step is missing or is not an object.
func field(manifest map[string]any, path string) any {
	node := any(manifest)
	for _, step := range append([]string{"spec"}, strings.Split(path, ".")...) {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = object[step]
	}
	return node
}

// setField writes a dotted path under the manifest's spec, making the
// objects on the way. onlyIfEmpty is the difference between a default,
// which fills a field the caller left out, and a rewrite, which replaces
// what the caller wrote.
func setField(manifest map[string]any, path, value string, onlyIfEmpty bool) {
	steps := append([]string{"spec"}, strings.Split(path, ".")...)
	node := manifest
	for _, step := range steps[:len(steps)-1] {
		next, ok := node[step].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[step] = next
		}
		node = next
	}
	last := steps[len(steps)-1]
	if onlyIfEmpty {
		switch got := node[last].(type) {
		case nil:
			// The field is absent, which is what a default fills.
		case string:
			if got != "" {
				return
			}
		default:
			// A value of another shape is the caller's own and a default
			// is not a rewrite.
			return
		}
	}
	node[last] = value
}

// fieldNames yields the keys of several flag maps, for one validation
// pass over every field a flag named.
func fieldNames(ms ...map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, m := range ms {
			for k := range m {
				if !yield(k) {
					return
				}
			}
		}
	}
}
