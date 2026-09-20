// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/cella/internal/stubs"
)

// envelope is one apply as spec 007 states it on the wire: the actor flat,
// the defaulted manifest whole, and the members that are usually absent
// written as null, because an endpoint reads a fixed shape.
func envelope(image string) map[string]any {
	return map[string]any{
		"subject":  "https://issuer.example|alice",
		"issuer":   "https://issuer.example",
		"sub":      "alice",
		"claims":   map[string]any{"org_id": "org-one", "roles": []string{"member"}},
		"workload": nil, "action": "create", "existing": nil, "parent": nil,
		"environment": map[string]any{"id": "env_01J9", "name": "default", "isolation": "container"},
		"set":         nil,
		"manifest": map[string]any{
			"apiVersion": "cella.latere.ai/v1beta1",
			"kind":       "Sandbox",
			"metadata":   map[string]any{"name": "dev"},
			"spec": map[string]any{
				"environment": "default",
				"image":       image,
				"command":     []string{"/bin/bash", "-l"},
			},
		},
		"request": map[string]any{"id": "req_01J9"},
	}
}

// answer is the 200 body of spec 007.
type answer struct {
	Allow    *bool           `json:"allow"`
	Manifest json.RawMessage `json:"manifest"`
	Reason   string          `json:"reason"`
	Warnings []string        `json:"warnings"`
}

// admit posts one envelope and reads the answer.
func admit(t *testing.T, url string, body any, header map[string]string) (int, answer, []byte) {
	t.Helper()
	code, read := post(t, url, body, header)
	var out answer
	if code == http.StatusOK {
		if err := json.Unmarshal(read, &out); err != nil {
			t.Fatalf("the answer %s does not decode: %v", read, err)
		}
	}
	return code, out, read
}

// spec reads the resolved manifest's spec out of an answer.
func spec(t *testing.T, a answer) map[string]any {
	t.Helper()
	var manifest struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(a.Manifest, &manifest); err != nil {
		t.Fatalf("the answer carries no manifest: %v", err)
	}
	return manifest.Spec
}

// TestTheAdmissionEndpointAnswersTheEnvelope: the manifest comes back
// with the operator's defaults filled in, the rewrites applied whatever
// the caller wrote, and the warnings the flag named.
func TestTheAdmissionEndpointAnswersTheEnvelope(t *testing.T) {
	s := start(t, stubs.Options{Admission: stubs.AdmissionOptions{
		Defaults: map[string]string{"image": "registry.example/base:1", "resources.memory": "512Mi"},
		Rewrites: map[string]string{"workdir": "/workspace"},
		Warnings: []string{"the memory ceiling of this installation was applied"},
	}})
	url := s.URL(stubs.RoleAdmission)

	// A manifest that names its own image keeps it, and the fields it
	// left out take the installation's defaults.
	code, out, body := admit(t, url, envelope("registry.example/mine:2"), nil)
	if code != http.StatusOK || out.Allow == nil || !*out.Allow {
		t.Fatalf("the apply answered %d %s", code, body)
	}
	got := spec(t, out)
	if got["image"] != "registry.example/mine:2" {
		t.Errorf("the image is %v; a default does not replace what the caller wrote", got["image"])
	}
	if got["workdir"] != "/workspace" {
		t.Errorf("the workdir is %v; a rewrite is applied whatever the manifest says", got["workdir"])
	}
	resources, _ := got["resources"].(map[string]any)
	if resources == nil || resources["memory"] != "512Mi" {
		t.Errorf("the resources are %v; a default reaches a field whose object the manifest did not carry", got["resources"])
	}
	if len(out.Warnings) != 1 {
		t.Errorf("the answer carries %v warnings", out.Warnings)
	}
	if command, ok := got["command"].([]any); !ok || len(command) != 2 {
		t.Errorf("the command came back as %v; what the endpoint did not change is returned unchanged", got["command"])
	}

	// A manifest that names no image takes the installation's.
	body2 := envelope("")
	if _, out, _ := admit(t, url, body2, nil); spec(t, out)["image"] != "registry.example/base:1" {
		t.Errorf("an empty image was not defaulted: %s", out.Manifest)
	}

	// Every envelope is readable afterwards, which is how a tier asserts
	// what the control plane sent.
	code, read := get(t, url+"/requests")
	if code != http.StatusOK {
		t.Fatalf("the request log answered %d", code)
	}
	var seen []json.RawMessage
	if err := json.Unmarshal(read, &seen); err != nil || len(seen) != 2 {
		t.Fatalf("the endpoint recorded %s (%v)", read, err)
	}
	var first map[string]any
	if err := json.Unmarshal(seen[0], &first); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"subject", "issuer", "sub", "claims", "action", "manifest", "request"} {
		if _, ok := first[field]; !ok {
			t.Errorf("the recorded envelope has no %s", field)
		}
	}
	if code, _ := postNoBody(t, url+"/requests", http.MethodDelete); code != http.StatusNoContent {
		t.Errorf("the request log was not cleared: %d", code)
	}
	if _, read := get(t, url+"/requests"); string(read) != "[]\n" {
		t.Errorf("the cleared log is %s", read)
	}
}

// TestTheAdmissionEndpointRefuses: a refusal is a 200 with no allow, and
// a bad bearer or an unreadable envelope is the transport failure spec
// 007 tells apart from it.
func TestTheAdmissionEndpointRefuses(t *testing.T) {
	s := start(t, stubs.Options{Admission: stubs.AdmissionOptions{
		Token:       "an-admission-bearer",
		RefuseImage: "registry.example/forbidden:1",
	}})
	url := s.URL(stubs.RoleAdmission)
	bearer := map[string]string{"Authorization": "Bearer an-admission-bearer"}

	code, out, body := admit(t, url, envelope("registry.example/forbidden:1"), bearer)
	if code != http.StatusOK {
		t.Fatalf("a policy refusal answered %d: %s", code, body)
	}
	if out.Allow == nil || *out.Allow {
		t.Fatalf("the refused image was admitted: %s", body)
	}
	if out.Reason == "" {
		t.Error("the refusal carries no reason")
	}
	if _, out, _ := admit(t, url, envelope("registry.example/allowed:1"), bearer); out.Allow == nil || !*out.Allow {
		t.Error("an image the endpoint does not refuse was refused")
	}
	if code, _ := post(t, url, envelope("registry.example/allowed:1"), nil); code != http.StatusUnauthorized {
		t.Errorf("an apply with no bearer answered %d, want 401", code)
	}
	if code, _ := post(t, url, "{not json", bearer); code != http.StatusBadRequest {
		t.Errorf("an unreadable envelope answered %d, want 400", code)
	}
	if code, _ := post(t, url, map[string]any{"action": "create"}, bearer); code != http.StatusBadRequest {
		t.Errorf("an envelope with no manifest answered %d, want 400", code)
	}
}

// TestTheAdmissionEndpointRefusesEveryApply: the blanket refusal, which
// is how a tier proves the control plane fails closed on a policy no.
func TestTheAdmissionEndpointRefusesEveryApply(t *testing.T) {
	s := start(t, stubs.Options{Admission: stubs.AdmissionOptions{Refuse: "this installation admits nothing today"}})
	_, out, body := admit(t, s.URL(stubs.RoleAdmission), envelope("registry.example/base:1"), nil)
	if out.Allow == nil || *out.Allow {
		t.Fatalf("the apply was admitted: %s", body)
	}
	if out.Reason != "this installation admits nothing today" {
		t.Errorf("the reason is %q", out.Reason)
	}
}

// TestTheAdmissionEndpointProducesEachOutage: the endpoint answers each
// form of unavailability spec 007 names, and records the envelope it was
// sent even then, so a tier reads what the control plane tried to send.
func TestTheAdmissionEndpointProducesEachOutage(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		status int
	}{
		"a status":  {mode: "status:500", status: http.StatusInternalServerError},
		"malformed": {mode: stubs.FailMalformed, status: http.StatusOK},
		"no allow":  {mode: stubs.FailNoAllow, status: http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			s := start(t, stubs.Options{Admission: stubs.AdmissionOptions{Fail: tc.mode}})
			url := s.URL(stubs.RoleAdmission)
			code, body := post(t, url, envelope("registry.example/base:1"), nil)
			if code != tc.status {
				t.Fatalf("the mode %s answered %d, want %d", tc.mode, code, tc.status)
			}
			if tc.mode != stubs.FailMalformed && json.Valid(body) {
				var out answer
				if err := json.Unmarshal(body, &out); err == nil && out.Allow != nil {
					t.Errorf("the mode %s answered a verdict: %s", tc.mode, body)
				}
			}
			if _, read := get(t, url+"/requests"); string(read) == "[]\n" {
				t.Error("an outage swallowed the envelope instead of recording it")
			}
		})
	}
}

// postNoBody sends a method with no body, for the control routes.
func postNoBody(t *testing.T, url, method string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
