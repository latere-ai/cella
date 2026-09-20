// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// errorCases prove the envelope itself: every refusal class the suite can
// provoke carries the code's status and its fixed sentence, and the two
// documents outside the API need no bearer.
func errorCases() []Case {
	return []Case{
		{"errors", "case008ErrorEnvelope", case008ErrorEnvelope},
		{"errors", "case008PublicDocuments", case008PublicDocuments},
	}
}

// capabilityCases prove the gate: a route whose capability the environment
// does not declare is refused before any driver call, and a route whose
// capability it declares is not refused for that reason.
func capabilityCases() []Case {
	return []Case{{"capability", "case004CapabilityGates", case004CapabilityGates}}
}

// gate is the route one capability guards.
type gate struct {
	capability string
	method     string
	suffix     string
	body       []byte
}

// gates is one route per capability of design 004, each the cheapest call
// that reaches the gate.
func gates() []gate {
	return []gate{
		{"attach", http.MethodGet, "/attach", nil},
		{"files", http.MethodGet, "/files?path=/workspace", nil},
		{"display", http.MethodGet, "/display", nil},
		{"input", http.MethodPost, "/input", []byte(`{"events":[{"type":"key","key":"a"}]}`)},
		{"dial", http.MethodGet, "/dial/8080", nil},
	}
}

// case008ErrorEnvelope: every refusal class the suite can reach arrives as
// the envelope of design 008, with the code's status, the code's fixed
// sentence, and a request id in the details.
func case008ErrorEnvelope(ctx context.Context, e *Env) error {
	name := e.name()
	if _, err := e.create(ctx, e.caller, e.manifest(name)); err != nil {
		return err
	}
	type provocation struct {
		code string
		call func() (*exchange, error)
	}
	provocations := []provocation{
		{"invalid_field", func() (*exchange, error) { return e.caller.get(ctx, "/v1/sandboxes?limit=300") }},
		{"unauthenticated", func() (*exchange, error) {
			return e.caller.send(ctx, http.MethodGet, "/v1/sandboxes", request{NoBearer: true})
		}},
		{"not_found", func() (*exchange, error) { return e.caller.get(ctx, "/v1/sandboxes/"+missingID) }},
		{"name_taken", func() (*exchange, error) { return e.caller.post(ctx, "/v1/sandboxes", e.manifest(name)) }},
		{"body_too_large", func() (*exchange, error) {
			return e.caller.post(ctx, "/v1/sandboxes", e.manifest(e.name(), padding(1<<20)))
		}},
		{"unsupported_media_type", func() (*exchange, error) {
			return e.caller.send(ctx, http.MethodPost, "/v1/sandboxes", request{Body: e.manifest(e.name()), ContentType: "text/plain"})
		}},
	}
	// The class a capability gate answers, where the environment leaves one
	// capability undeclared. Where it declares every one, no call provokes it
	// and the class is not asserted here.
	if undeclared, ok := e.undeclared(); ok {
		obj, err := e.sandbox(ctx, e.caller)
		if err != nil {
			return err
		}
		provocations = append(provocations, provocation{"capability_unsupported", func() (*exchange, error) {
			return e.caller.send(ctx, undeclared.method, "/v1/sandboxes/"+obj.Status.ID+undeclared.suffix,
				request{Body: undeclared.body, ContentType: "application/json"})
		}})
	}
	if e.cfg.AuthorizerControl != "" {
		provocations = append(provocations, provocation{"authorizer_unavailable", func() (*exchange, error) {
			if err := e.control(ctx, e.cfg.AuthorizerControl, "unavailable"); err != nil {
				return nil, err
			}
			defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AuthorizerControl, "") }()
			return e.caller.get(ctx, "/v1/sandboxes")
		}})
		provocations = append(provocations, provocation{"forbidden", func() (*exchange, error) {
			if err := e.control(ctx, e.cfg.AuthorizerControl, "deny:sandbox.list"); err != nil {
				return nil, err
			}
			defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AuthorizerControl, "") }()
			return e.caller.get(ctx, "/v1/sandboxes")
		}})
	}
	for _, p := range provocations {
		x, err := p.call()
		if err != nil {
			return err
		}
		if err := x.refusal(p.code); err != nil {
			return err
		}
	}
	return nil
}

// case008PublicDocuments: the key set and the API document are outside the
// API and carry no bearer, so a client generator and a verifier reach them
// with no credential at all.
func case008PublicDocuments(ctx context.Context, e *Env) error {
	x, err := e.caller.send(ctx, http.MethodGet, "/.well-known/jwks.json", request{NoBearer: true})
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(x.Body, &set); err != nil || len(set.Keys) == 0 {
		return x.disagree("a key set with at least one key", fmt.Sprintf("%q", x.Body))
	}
	x, err = e.caller.send(ctx, http.MethodGet, "/openapi.yaml", request{NoBearer: true})
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	if len(x.Body) == 0 {
		return x.disagree("the API document", "an empty body")
	}
	return nil
}

// case004CapabilityGates: a route whose capability the environment does not
// declare answers capability_unsupported before any driver call, and a route
// whose capability it declares never answers that code.
func case004CapabilityGates(ctx context.Context, e *Env) error {
	if len(e.caps) == 0 {
		return skipf("the environment declares no capability set; pass Capabilities to run the gates")
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	for _, g := range gates() {
		x, err := e.caller.send(ctx, g.method, "/v1/sandboxes/"+obj.Status.ID+g.suffix,
			request{Body: g.body, ContentType: "application/json"})
		if err != nil {
			return err
		}
		var env envelope
		_ = json.Unmarshal(x.Body, &env)
		refused := env.Error.Code == "capability_unsupported"
		switch {
		case e.caps[g.capability] && refused:
			return x.disagree("no capability refusal where the environment declares "+g.capability,
				"capability_unsupported")
		case !e.caps[g.capability] && !refused:
			return x.disagree("capability_unsupported where the environment does not declare "+g.capability,
				fmt.Sprintf("status %d and code %q", x.Status, cmpOr(env.Error.Code, "none")))
		}
	}
	return nil
}

// undeclared is a gate the environment does not declare, for the case that
// needs one refusal of that class.
func (e *Env) undeclared() (gate, bool) {
	if len(e.caps) == 0 {
		return gate{}, false
	}
	for _, g := range gates() {
		if !e.caps[g.capability] {
			return g, true
		}
	}
	return gate{}, false
}

// padding fills a manifest past any reasonable body cap.
func padding(size int) func(map[string]any) {
	return func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["env"] = map[string]any{"PADDING": strings.Repeat("p", size)}
	}
}
