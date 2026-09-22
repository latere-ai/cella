// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// decodeCases prove what a body may be: the content types the manifest
// contract names, one document per request, the version and the kind before
// any field, an unknown field at depth, and the two size and negotiation
// refusals of design 008.
func decodeCases() []Case {
	return []Case{
		{"decode", "case003ContentTypes", case003ContentTypes},
		{"decode", "case008UnsupportedMediaType", case008UnsupportedMediaType},
		{"decode", "case008NotAcceptable", case008NotAcceptable},
		{"decode", "case008BodyTooLarge", case008BodyTooLarge},
		{"decode", "case003MultiDocument", case003MultiDocument},
		{"decode", "case003UnsupportedVersion", case003UnsupportedVersion},
		{"decode", "case003UnsupportedKind", case003UnsupportedKind},
		{"decode", "case003UnknownField", case003UnknownField},
	}
}

// resolveCases prove what an apply makes of a manifest: the resolved object
// a read returns unchanged, the names, and the operator's admission step.
func resolveCases() []Case {
	return []Case{
		{"resolve", "case003DefaultsAreReturned", case003DefaultsAreReturned},
		{"resolve", "case008GeneratedName", case008GeneratedName},
		{"resolve", "case008NameTaken", case008NameTaken},
		{"resolve", "case008NameOnPathAndBody", case008NameOnPathAndBody},
		{"resolve", "case007AdmissionRefused", case007AdmissionRefused},
		{"resolve", "case007AdmissionUnavailable", case007AdmissionUnavailable},
	}
}

// case003ContentTypes: a manifest is accepted as JSON and as YAML, which is
// what the manifest contract says a body may be.
func case003ContentTypes(ctx context.Context, e *Env) error {
	name := e.name()
	yaml := fmt.Sprintf("apiVersion: %s\nkind: Sandbox\nmetadata:\n  name: %s\nspec:\n  command: [sleep, \"300\"]\n", APIVersion, name)
	if e.cfg.Image != "" {
		yaml += "  image: " + e.cfg.Image + "\n"
	}
	x, err := e.caller.send(ctx, http.MethodPost, "/v1/sandboxes", request{Body: []byte(yaml), ContentType: "application/yaml"})
	if err != nil {
		return err
	}
	if err := x.status(http.StatusCreated); err != nil {
		return err
	}
	obj, err := x.object()
	if err != nil {
		return err
	}
	e.record(e.caller, "/v1/sandboxes", obj.Status.ID)
	return nil
}

// case008UnsupportedMediaType: a body of any other type is 415 with the
// sentence of the table.
func case008UnsupportedMediaType(ctx context.Context, e *Env) error {
	return e.refuseApply(ctx, request{Body: e.manifest(e.name()), ContentType: "text/plain"}, "unsupported_media_type")
}

// case008NotAcceptable: an Accept that excludes JSON and YAML is 406.
func case008NotAcceptable(ctx context.Context, e *Env) error {
	x, err := e.caller.send(ctx, http.MethodGet, "/v1/sandboxes", request{Accept: "application/xml"})
	if err != nil {
		return err
	}
	return x.refusal("not_acceptable")
}

// case008BodyTooLarge: a manifest past the server's cap is 413 and no object
// is created.
func case008BodyTooLarge(ctx context.Context, e *Env) error {
	name := e.name()
	body := e.manifest(name, padding(1<<20))
	if err := e.refuseApply(ctx, request{Body: body, ContentType: "application/json"}, "body_too_large"); err != nil {
		return err
	}
	read, err := e.caller.get(ctx, "/v1/sandboxes/"+name)
	if err != nil {
		return err
	}
	if read.Status != http.StatusNotFound {
		return read.disagree("no object from a refused apply", fmt.Sprintf("status %d reading %s", read.Status, name))
	}
	return nil
}

// case003MultiDocument: two documents in one request are refused.
func case003MultiDocument(ctx context.Context, e *Env) error {
	two := append(append([]byte{}, e.manifest(e.name())...), e.manifest(e.name())...)
	return e.refuseApply(ctx, request{Body: two, ContentType: "application/json"}, "multi_document")
}

// case003UnsupportedVersion: the version is read before any field.
func case003UnsupportedVersion(ctx context.Context, e *Env) error {
	body := replaceField(e.manifest(e.name()), "apiVersion", "cella.latere.ai/v1alpha0")
	return e.refuseApply(ctx, request{Body: body, ContentType: "application/json"}, "unsupported_version")
}

// case003UnsupportedKind: a kind this server does not serve is refused, and
// the refusal is the kind's and not the schema's.
func case003UnsupportedKind(ctx context.Context, e *Env) error {
	body := replaceField(e.manifest(e.name()), "kind", "Widget")
	return e.refuseApply(ctx, request{Body: body, ContentType: "application/json"}, "unsupported_kind")
}

// case003UnknownField: a field the schema does not know is refused wherever
// it sits, and the refusal is not a silent drop.
func case003UnknownField(ctx context.Context, e *Env) error {
	body := e.manifest(e.name(), func(body map[string]any) {
		metadata, _ := body["metadata"].(map[string]any)
		metadata["nonesuch"] = "a field no schema knows"
	})
	return e.refuseApply(ctx, request{Body: body, ContentType: "application/json"}, "unknown_field")
}

// literalDefaults are the defaults the manifest contract states as literals,
// each with the value a manifest that names none of these fields, carries no
// host and mounts no secret resolves to. Every conforming server resolves them
// the same, whatever its operator configured, which is what lets a black-box
// case hold a server to them; the operator's own defaults (resources, the
// lifecycle, the image) are an installation's choice and no case reads them.
// A zero default may also be left out of the answer, which is how JSON
// carries a false or a 0 it omits.
var literalDefaults = []struct {
	path     string
	want     string
	orAbsent bool
}{
	{"workspace.path", "/workspace", false},
	{"workspace.source", "empty", false},
	{"workdir", "/workspace", false},
	{"network.egress.mode", "open", false},
	{"mesh.enabled", "false", true},
	{"mesh.spawn.budget", "0", true},
	{"mesh.spawn.depth", "0", true},
}

// case003DefaultsAreReturned: an apply answers the resolved manifest with
// the status the server keeps and the literal defaults of the manifest
// contract, and a read of the same object answers the same specification, so
// a resolve is what a caller can rely on.
func case003DefaultsAreReturned(ctx context.Context, e *Env) error {
	name := e.name()
	created, err := e.create(ctx, e.caller, e.manifest(name))
	if err != nil {
		return err
	}
	if created.APIVersion != APIVersion || created.Kind != "Sandbox" {
		return fmt.Errorf("the answer is %s %s, want %s Sandbox", created.APIVersion, created.Kind, APIVersion)
	}
	if created.Metadata.Name != name {
		return fmt.Errorf("the answer names %q, want %q", created.Metadata.Name, name)
	}
	for field, value := range map[string]string{
		"status.id":          created.Status.ID,
		"status.owner":       created.Status.Owner,
		"status.phase":       created.Status.Phase,
		"status.environment": created.Status.Environment,
		"status.driver":      created.Status.Driver,
		"status.isolation":   created.Status.Isolation,
	} {
		if value == "" {
			return fmt.Errorf("the created object carries no %s", field)
		}
	}
	if err := holdsLiteralDefaults(created.Spec); err != nil {
		return err
	}
	x, err := e.caller.get(ctx, "/v1/sandboxes/"+created.Status.ID)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	read, err := x.object()
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical(created.Spec), canonical(read.Spec)) {
		return x.disagree("the specification the apply resolved", "a read that differs: "+string(read.Spec))
	}
	return nil
}

// case008GeneratedName: an apply without a name gets one.
func case008GeneratedName(ctx context.Context, e *Env) error {
	body := e.manifest("placeholder", func(body map[string]any) { delete(body, "metadata") })
	x, err := e.caller.post(ctx, "/v1/sandboxes", body)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusCreated); err != nil {
		return err
	}
	obj, err := x.object()
	if err != nil {
		return err
	}
	e.record(e.caller, "/v1/sandboxes", obj.Status.ID)
	if obj.Metadata.Name == "" {
		return x.disagree("a generated name on an apply that carries none", "no name")
	}
	return nil
}

// case008NameTaken: a name one live object of the caller holds is refused.
func case008NameTaken(ctx context.Context, e *Env) error {
	name := e.name()
	if _, err := e.create(ctx, e.caller, e.manifest(name)); err != nil {
		return err
	}
	return e.refuseApply(ctx, request{Body: e.manifest(name), ContentType: "application/json"}, "name_taken")
}

// case008NameOnPathAndBody: an apply by name whose body names another object
// is refused at the field, and never renamed.
func case008NameOnPathAndBody(ctx context.Context, e *Env) error {
	path, other := e.name(), e.name()
	x, err := e.caller.put(ctx, "/v1/sandboxes/"+path, e.manifest(other), "application/json")
	if err != nil {
		return err
	}
	if err := x.refusal("invalid_field"); err != nil {
		return err
	}
	if paths := x.paths(); !slices.Contains(paths, "metadata.name") {
		return x.disagree("details.paths naming metadata.name", fmt.Sprintf("paths %v", paths))
	}
	return nil
}

// case007AdmissionRefused: a policy refusal from the operator's endpoint is
// the caller's refusal, with the code the table names.
func case007AdmissionRefused(ctx context.Context, e *Env) error {
	if e.cfg.AdmissionControl == "" {
		return skipf("no admission control URL: set AdmissionControl to drive the policy endpoint")
	}
	if err := e.control(ctx, e.cfg.AdmissionControl, "refuse"); err != nil {
		return err
	}
	defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AdmissionControl, "") }()
	return e.refuseApply(ctx, request{Body: e.manifest(e.name()), ContentType: "application/json"}, "admission_refused")
}

// case007AdmissionUnavailable: an endpoint that does not answer fails
// closed, and the caller reads that the policy service is unavailable.
func case007AdmissionUnavailable(ctx context.Context, e *Env) error {
	if e.cfg.AdmissionControl == "" {
		return skipf("no admission control URL: set AdmissionControl to drive the policy endpoint")
	}
	if err := e.control(ctx, e.cfg.AdmissionControl, "unavailable"); err != nil {
		return err
	}
	defer func() { _ = e.control(context.WithoutCancel(ctx), e.cfg.AdmissionControl, "") }()
	return e.refuseApply(ctx, request{Body: e.manifest(e.name()), ContentType: "application/json"}, "admission_unavailable")
}

// holdsLiteralDefaults reads each literal default off a resolved
// specification and reports the first that differs, naming the field, the
// literal and what the apply answered.
func holdsLiteralDefaults(spec json.RawMessage) error {
	var resolved map[string]any
	if err := json.Unmarshal(spec, &resolved); err != nil {
		return &Disagreement{Method: http.MethodPost, Path: "/v1/sandboxes", Want: "a specification that is a JSON object", Got: err.Error(), Body: string(spec)}
	}
	for _, d := range literalDefaults {
		got, present := lookup(resolved, d.path)
		if present && fmt.Sprint(got) == d.want || !present && d.orAbsent {
			continue
		}
		want := "spec." + d.path + " " + d.want
		if d.orAbsent {
			want += " or absent"
		}
		answered := "absent"
		if present {
			answered = fmt.Sprint(got)
		}
		return &Disagreement{
			Method: http.MethodPost, Path: "/v1/sandboxes",
			Want: want + ", the default the manifest contract states",
			Got:  "spec." + d.path + " " + answered, Body: string(spec),
		}
	}
	return nil
}

// lookup reads one dotted path off a decoded JSON object.
func lookup(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// replaceField rewrites one top-level string field of a manifest.
func replaceField(body []byte, field, value string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m[field] = value
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// canonical re-encodes JSON so two bodies are compared by content and not by
// the order two encoders chose.
func canonical(raw json.RawMessage) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}
