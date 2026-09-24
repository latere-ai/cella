// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// answer sends one request with an Accept header and returns the status, the
// answer's content type and its bytes.
func (f *fixture) answer(method, path, token, accept, body string) (int, string, []byte) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), raw
}

// TestNegotiate is design 008's rule over one header at a time: JSON wins
// wherever both syntaxes are acceptable, a YAML type alone is YAML, a weight
// of zero excludes, and a header that names neither is refused.
func TestNegotiate(t *testing.T) {
	for _, tc := range []struct {
		accept string
		form   int
		ok     bool
	}{
		{"", answerJSON, true},
		{"   ", answerJSON, true},
		{"*/*", answerJSON, true},
		{"application/*", answerJSON, true},
		{"application/json", answerJSON, true},
		{"application/json;q=0.2, application/yaml;q=0.9", answerJSON, true},
		{"application/yaml", answerYAML, true},
		{"application/x-yaml", answerYAML, true},
		{"text/yaml", answerYAML, true},
		{"application/json;q=0, application/yaml", answerYAML, true},
		{"application/xml", answerJSON, false},
		{"text/html, image/png", answerJSON, false},
		{"application/json;q=0", answerJSON, false},
		{"not a media type", answerJSON, false},
		{"not a media type, application/yaml", answerYAML, true},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			form, ok := negotiate(tc.accept)
			if form != tc.form || ok != tc.ok {
				t.Fatalf("negotiate(%q) = %d, %v; want %d, %v", tc.accept, form, ok, tc.form, tc.ok)
			}
		})
	}
}

// TestAcceptNegotiation proves the rule over real answers: a YAML type gets
// the same object as YAML, in the order the JSON carried it, and a header
// that names neither syntax is 406 with the table's sentence.
func TestAcceptNegotiation(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")

	status, contentType, asJSON := f.answer("GET", "/v1/sandboxes/"+obj.Status.ID, f.alice, "application/json", "")
	if status != 200 || !strings.Contains(contentType, "json") {
		t.Fatalf("a JSON read answered %d %q", status, contentType)
	}
	for _, accept := range []string{"application/yaml", "application/x-yaml", "text/yaml"} {
		status, contentType, asYAML := f.answer("GET", "/v1/sandboxes/"+obj.Status.ID, f.alice, accept, "")
		if status != 200 {
			t.Fatalf("%s answered %d: %s", accept, status, asYAML)
		}
		if !strings.Contains(contentType, "yaml") {
			t.Errorf("%s answered Content-Type %q", accept, contentType)
		}
		// The two syntaxes carry one object.
		var fromYAML, fromJSON any
		if err := yaml.Unmarshal(asYAML, &fromYAML); err != nil {
			t.Fatalf("the YAML answer does not parse: %v: %s", err, asYAML)
		}
		if err := json.Unmarshal(asJSON, &fromJSON); err != nil {
			t.Fatal(err)
		}
		left, err := json.Marshal(fromYAML)
		if err != nil {
			t.Fatal(err)
		}
		right, err := json.Marshal(fromJSON)
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Errorf("%s answered another object:\n%s\n%s", accept, left, right)
		}
		// The field order is the document's and not the alphabet's.
		if i, j := strings.Index(string(asYAML), "apiVersion"), strings.Index(string(asYAML), "kind"); i < 0 || j < 0 || i > j {
			t.Errorf("the YAML answer is not in the document's order: %s", asYAML)
		}
	}
	// A list answers in the negotiated syntax too.
	if status, contentType, _ := f.answer("GET", "/v1/sandboxes", f.alice, "application/yaml", ""); status != 200 || !strings.Contains(contentType, "yaml") {
		t.Errorf("a list answered %d %q", status, contentType)
	}
}

// TestNotAcceptableIsRefusedBeforeTheAct proves the refusal is the table's and
// that it reaches the caller before the handler acts: a create whose Accept
// this server cannot satisfy leaves no object behind.
func TestNotAcceptableIsRefusedBeforeTheAct(t *testing.T) {
	f := setup(t, nil)
	status, _, body := f.answer("GET", "/v1/sandboxes", f.alice, "application/xml", "")
	if status != http.StatusNotAcceptable {
		t.Fatalf("a read answered %d: %s", status, body)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the refusal is not the envelope: %s", body)
	}
	if envelope.Error.Code != "not_acceptable" || envelope.Error.Message != "This endpoint answers in JSON or YAML." {
		t.Fatalf("the refusal is %+v", envelope.Error)
	}
	status, _, body = f.answer("POST", "/v1/sandboxes?wait=1", f.alice, "application/xml", createBody)
	if status != http.StatusNotAcceptable {
		t.Fatalf("a create answered %d: %s", status, body)
	}
	f.request("GET", "/v1/sandboxes/work", f.alice, "", 404)
}

// TestStreamRoutesNegotiateNothing proves a route whose content type is its
// own is not refused for an Accept it never reads: an archive, a file body, a
// log and the bounded exec each answer, and the exec result still negotiates
// because it is one object.
func TestStreamRoutesNegotiateNothing(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")
	base := "/v1/sandboxes/" + obj.Status.ID
	for _, path := range []string{base + "/files?path=/workspace", base + "/logs?tail=1"} {
		if status, _, body := f.answer("GET", path, f.alice, "application/x-tar", ""); status != 200 {
			t.Errorf("%s answered %d: %s", path, status, body)
		}
	}
	// The bounded exec answers one object, so it reads Accept like every
	// other object route.
	if status, _, body := f.answer("POST", base+"/exec?wait=1", f.alice, "application/xml", `{"command":["true"]}`); status != http.StatusNotAcceptable {
		t.Errorf("the bounded exec answered %d: %s", status, body)
	}
	if status, contentType, body := f.answer("POST", base+"/exec?wait=1", f.alice, "text/yaml", `{"command":["true"]}`); status != 200 || !strings.Contains(contentType, "yaml") {
		t.Errorf("the bounded exec answered %d %q: %s", status, contentType, body)
	}
}
