// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// actor is the applying identity as the wire carries it. Spec 007 names
// it one type and the envelope flattens it into three members, which is
// what this embedded struct states: the Go value is an actor, and the
// JSON is subject, issuer and sub beside the rest of the body.
//
// It is not the authorizer's envelope, which one shared package declares
// and this repository never re-declares. It is spec 007's own body, whose
// members are the manifest, the action and what the manifest is about.
type actor struct {
	// Subject is the rendered subject, <iss>|<sub>, and Issuer and Sub
	// are its two halves as the token carried them.
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	Sub     string `json:"sub"`
}

// request is the body one apply POSTs. Every member is present on the
// wire, the four that are usually absent as null, because an endpoint
// reads a fixed shape and a fixture that drives it needs them written
// down.
type request struct {
	actor
	// Claims are the caller's verified claims, verbatim. The control
	// plane reads none of them; the endpoint reads whichever its policy
	// needs, which is where an organisation and a role live.
	Claims map[string]any `json:"claims"`
	// Workload is the calling sandbox's status when a workload applies
	// through its own token, and null otherwise.
	Workload json.RawMessage `json:"workload"`
	// Action is create or update.
	Action string `json:"action"`
	// Existing is the current object on an update and null on a create.
	Existing json.RawMessage `json:"existing"`
	// Parent is the spawning sandbox where this apply is a spawn. It is
	// null until the spawn of spec 022 exists.
	Parent json.RawMessage `json:"parent"`
	// Environment is the environment the manifest names, as the summary
	// spec 007 states rather than the whole object.
	Environment *environmentRef `json:"environment"`
	// Set names the replica index of a SandboxSet apply. It is null until
	// the sets of spec 020 exist.
	Set json.RawMessage `json:"set"`
	// Manifest is the defaulted object, the output of stage 2. Stage 1
	// refuses an unknown field before admission runs, so the typed object
	// is the whole of what the caller wrote and marshalling it loses
	// nothing an endpoint could have read.
	Manifest json.RawMessage `json:"manifest"`
	// Ref carries spec 008's X-Request-Id, which ties a refusal at the
	// endpoint to the apply that drew it. The peer address and the user
	// agent are the authorizer's envelope and not this one.
	Ref requestRef `json:"request"`
}

type requestRef struct {
	ID string `json:"id"`
}

// environmentRef is the environment as spec 007's envelope carries it:
// what it is called, what class of isolation it provides, and what its
// driver can enforce. The whole Environment object would carry the
// operator's own declaration twice and add nothing an endpoint reads.
type environmentRef struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Isolation    string          `json:"isolation"`
	Capabilities v1.Capabilities `json:"capabilities"`
}

// response is the 200 body. Allow is a pointer so that an answer naming
// no allow is told apart from one that denies: the first is no decision
// and the second is a refusal, and spec 007 answers them differently.
type response struct {
	Allow    *bool           `json:"allow"`
	Manifest json.RawMessage `json:"manifest"`
	Reason   string          `json:"reason"`
	Warnings []string        `json:"warnings"`
}

// envelopeOf renders one apply as the body spec 007 states.
func envelopeOf(in *v1.Sandbox, req manifest.AdmitRequest) ([]byte, error) {
	claims := req.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	body := request{
		Claims:      claims,
		Action:      req.Action,
		Environment: summaryOf(req.Environment),
		Ref:         requestRef{ID: req.RequestID},
	}
	body.Subject, body.Issuer, body.Sub = req.Actor.Subject, req.Actor.Issuer, req.Actor.Sub
	var err error
	if body.Manifest, err = json.Marshal(in); err != nil {
		return nil, err
	}
	if req.Workload != nil {
		if body.Workload, err = json.Marshal(req.Workload); err != nil {
			return nil, err
		}
	}
	if req.Existing != nil {
		if body.Existing, err = json.Marshal(req.Existing); err != nil {
			return nil, err
		}
	}
	return json.Marshal(body)
}

// summaryOf reduces an Environment to the four members the envelope
// carries. The isolation class is what the driver actually provides, and
// the operator's declaration where no driver has answered yet.
func summaryOf(env *v1.Environment) *environmentRef {
	if env == nil {
		return nil
	}
	isolation := env.Status.Isolation
	if isolation == "" {
		isolation = env.Spec.Isolation
	}
	return &environmentRef{
		ID:           env.Status.ID,
		Name:         env.Metadata.Name,
		Isolation:    isolation,
		Capabilities: env.Status.Capabilities,
	}
}
