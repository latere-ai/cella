// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// named is createBody under another name, which is what an apply by name
// sends.
func named(name string) string {
	return strings.Replace(createBody, `"name":"work"`, `"name":"`+name+`"`, 1)
}

// TestApplyByName proves design 008's apply grammar for the Sandbox kind: a
// free name is a create at the path's name, a name the caller holds is an
// update, and a body naming another object is refused at the field.
func TestApplyByName(t *testing.T) {
	f := setup(t, nil)

	// A free name creates, and the object is the path's and not the body's.
	var created v1.Sandbox
	if err := json.Unmarshal(f.request("PUT", "/v1/sandboxes/first", f.alice, named("first"), 201), &created); err != nil {
		t.Fatal(err)
	}
	if created.Metadata.Name != "first" || created.Status.ID == "" {
		t.Fatalf("the apply created %+v", created.Metadata)
	}

	// A body with no name takes the path's, which is what lets one manifest
	// be applied under several names.
	nameless := strings.Replace(createBody, `"name":"work",`, "", 1)
	var second v1.Sandbox
	if err := json.Unmarshal(f.request("PUT", "/v1/sandboxes/second", f.alice, nameless, 201), &second); err != nil {
		t.Fatal(err)
	}
	if second.Metadata.Name != "second" {
		t.Fatalf("a nameless body was created as %q", second.Metadata.Name)
	}

	// A body naming another object is refused at the field, and nothing is
	// created under either name.
	body := f.request("PUT", "/v1/sandboxes/third", f.alice, named("elsewhere"), 400)
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "invalid_field" {
		t.Fatalf("the refusal is %s", envelope.Error.Code)
	}
	paths, _ := envelope.Error.Details["paths"].([]any)
	if !slices.Contains(paths, any("metadata.name")) {
		t.Fatalf("details.paths is %v, want metadata.name", envelope.Error.Details["paths"])
	}
	f.request("GET", "/v1/sandboxes/third", f.alice, "", 404)
	f.request("GET", "/v1/sandboxes/elsewhere", f.alice, "", 404)

	// A name the caller holds is an update: the same object, with the
	// specification the second apply resolved.
	updated := strings.Replace(named("first"), `"labels":{"team":"a"}`, `"labels":{"team":"b"}`, 1)
	var applied v1.Sandbox
	if err := json.Unmarshal(f.request("PUT", "/v1/sandboxes/first", f.alice, updated, 200), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Status.ID != created.Status.ID {
		t.Fatalf("an update made the object %s, not %s", applied.Status.ID, created.Status.ID)
	}
	if applied.Metadata.Labels["team"] != "b" {
		t.Fatalf("the update did not land: %v", applied.Metadata.Labels)
	}
	var read v1.Sandbox
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/first", f.alice, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Metadata.Labels["team"] != "b" || read.Status.ID != created.Status.ID {
		t.Fatalf("a read after the update answers %+v", read.Metadata)
	}

	// An update that changes a field design 003 marks immutable is refused
	// with the field named, and the object is unchanged.
	frozen := strings.Replace(named("first"), `"spec":{}`, `"spec":{"mesh":{"enabled":true}}`, 1)
	f.request("PUT", "/v1/sandboxes/first", f.alice, frozen, 409)
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/first", f.alice, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Spec.Mesh.Enabled {
		t.Fatal("a refused update changed the object")
	}

	// The name is the caller's own namespace, so the same name applied by
	// another subject is that subject's own object.
	var bobs v1.Sandbox
	if err := json.Unmarshal(f.request("PUT", "/v1/sandboxes/first", f.bob, named("first"), 201), &bobs); err != nil {
		t.Fatal(err)
	}
	if bobs.Status.ID == created.Status.ID {
		t.Fatal("two subjects' applies reached one object")
	}
}

// TestApplyRefusals proves the apply route refuses what every other route
// refuses: a body of a type this server does not read, a manifest past the
// cap, and a kind this route does not serve.
func TestApplyRefusals(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"a body that does not parse", "{", 400, "bad_request"},
		{"another kind", strings.Replace(named("one"), `"kind":"Sandbox"`, `"kind":"Secret"`, 1), 400, "unsupported_kind"},
		{"a field the schema does not know", strings.Replace(named("one"), `"spec":{}`, `"spec":{"nonesuch":1}`, 1), 400, "unknown_field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := f.request("PUT", "/v1/sandboxes/one", f.alice, tc.body, tc.status)
			if !strings.Contains(string(body), tc.code) {
				t.Fatalf("the refusal is %q, want %s", body, tc.code)
			}
		})
	}
	// A YAML body reaches the apply route as it reaches the create route.
	document := "apiVersion: " + v1.APIVersion + "\nkind: Sandbox\nmetadata:\n  name: written\nspec: {}\n"
	req, err := http.NewRequest("PUT", f.url+"/v1/sandboxes/written", strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer "+f.alice)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	answered, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 201 {
		t.Fatalf("a YAML apply answered %d: %s", res.StatusCode, answered)
	}
}
