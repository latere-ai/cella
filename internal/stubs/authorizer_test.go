// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/stubs"
)

// decide asks the authorizer one question the way the client does.
func decide(t *testing.T, url, token string, req authz.Request, header map[string]string) (int, authz.Decision, []byte) {
	t.Helper()
	if header == nil {
		header = map[string]string{}
	}
	header["Authorization"] = "Bearer " + token
	code, body := post(t, url, req, header)
	if code != http.StatusOK {
		return code, authz.Decision{}, body
	}
	d, err := authz.ParseDecision(body)
	if err != nil {
		t.Fatalf("the answer %s is no decision: %v", body, err)
	}
	return code, d, body
}

// request is one well-formed envelope over a sandbox.
func request(action, id string) authz.Request {
	return authz.Request{
		Subject: "https://issuer.example|alice", Issuer: "https://issuer.example", Sub: "alice",
		Claims: map[string]any{}, Action: action,
		Resource: authz.NewResource("sandbox", id, nil),
	}
}

// TestTheAuthorizerPassesTheSharedConformanceSuite: the endpoint a tier
// runs is held to the contract every core's endpoint is held to, driven
// from this core's own vocabulary rather than a list written by hand.
func TestTheAuthorizerPassesTheSharedConformanceSuite(t *testing.T) {
	s := start(t, stubs.Options{})
	conformance.Run(t, s.URL(stubs.RoleAuthorizer), stub.DefaultToken,
		conformance.WithVocabulary(authorizer.Vocabulary()))
}

// TestTheAuthorizerDeniesTheProbe is the rule `cellad check` reads: an
// endpoint that allows the reserved id is one that is not reading the
// request, and the check command says so.
func TestTheAuthorizerDeniesTheProbe(t *testing.T) {
	s := start(t, stubs.Options{})
	url := s.URL(stubs.RoleAuthorizer)
	for _, action := range authorizer.Actions() {
		req := request(action, authz.ProbeID)
		req.Resource = authz.NewResource(authorizer.Kind(action), authz.ProbeID, nil)
		if _, d, body := decide(t, url, stub.DefaultToken, req, nil); d.Allow {
			t.Fatalf("%s on the probe id was allowed: %s", action, body)
		}
	}
}

// TestTheAuthorizerAnswersWhatItsFlagsSay: each row of the flag table
// changes one part of one answer.
func TestTheAuthorizerAnswersWhatItsFlagsSay(t *testing.T) {
	s := start(t, stubs.Options{Authorizer: stubs.AuthorizerOptions{
		Token:  "a-bearer-of-this-run",
		Deny:   []string{authorizer.ActionSandboxDelete},
		Limits: `{"max_sandboxes": 3}`,
		Filter: `{"owners": ["https://issuer.example|alice"]}`,
		TTL:    30,
	}})
	url := s.URL(stubs.RoleAuthorizer)

	code, d, body := decide(t, url, "a-bearer-of-this-run", request(authorizer.ActionSandboxCreate, "sbx_1"), nil)
	if code != http.StatusOK || !d.Allow {
		t.Fatalf("the allow answered %d %s", code, body)
	}
	if d.TTL != 30*1e9 {
		t.Errorf("the answer holds for %s, and the flag said 30 seconds", d.TTL)
	}
	var limits struct {
		MaxSandboxes int `json:"max_sandboxes"`
	}
	if err := d.DecodeLimits(&limits); err != nil || limits.MaxSandboxes != 3 {
		t.Errorf("the limits are %s (%v), and the flag said three sandboxes", d.Limits, err)
	}
	if d.Filter == nil || len(d.Filter.Owners) != 1 {
		t.Errorf("the filter is %v, and the flag named one owner", d.Filter)
	}

	if _, d, body := decide(t, url, "a-bearer-of-this-run", request(authorizer.ActionSandboxDelete, "sbx_1"), nil); d.Allow {
		t.Errorf("the denied action was allowed: %s", body)
	}
	if code, _, _ := decide(t, url, "the-wrong-bearer", request(authorizer.ActionSandboxCreate, "sbx_1"), nil); code != http.StatusUnauthorized {
		t.Errorf("a wrong bearer answered %d, want 401", code)
	}
}

// TestTheAuthorizerDeniesOneRequestByHeader: a table-driven suite in
// another process refuses one row without changing what the endpoint
// answers the rows around it.
func TestTheAuthorizerDeniesOneRequestByHeader(t *testing.T) {
	s := start(t, stubs.Options{})
	url := s.URL(stubs.RoleAuthorizer)
	header := map[string]string{stubs.DenyHeader: authorizer.ActionSandboxCreate}

	_, d, body := decide(t, url, stub.DefaultToken, request(authorizer.ActionSandboxCreate, "sbx_1"), header)
	if d.Allow {
		t.Errorf("the header named the action and it was allowed: %s", body)
	}
	if _, d, _ := decide(t, url, stub.DefaultToken, request(authorizer.ActionSandboxRead, "sbx_1"), header); !d.Allow {
		t.Error("the header refused an action it did not name")
	}
	// The header is a refusal and not a way past the bearer.
	if code, _, _ := decide(t, url, "the-wrong-bearer", request(authorizer.ActionSandboxCreate, "sbx_1"), header); code != http.StatusUnauthorized {
		t.Errorf("a wrong bearer with the header answered %d, want 401", code)
	}
}

// TestTheAuthorizerProducesEachOutage: every form of unavailability spec
// 006 names is one flag, so a tier drives the client's fail-closed rule
// against a real peer.
func TestTheAuthorizerProducesEachOutage(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		status int
		verdict
	}{
		"a status":  {mode: "status:503", status: http.StatusServiceUnavailable},
		"malformed": {mode: stubs.FailMalformed, status: http.StatusOK, verdict: noDecision},
		"no allow":  {mode: stubs.FailNoAllow, status: http.StatusOK, verdict: noDecision},
	} {
		t.Run(name, func(t *testing.T) {
			s := start(t, stubs.Options{Authorizer: stubs.AuthorizerOptions{Fail: tc.mode}})
			header := map[string]string{"Authorization": "Bearer " + stub.DefaultToken}
			code, body := post(t, s.URL(stubs.RoleAuthorizer), request(authorizer.ActionSandboxCreate, "sbx_1"), header)
			if code != tc.status {
				t.Fatalf("the mode %s answered %d, want %d", tc.mode, code, tc.status)
			}
			if tc.verdict == noDecision && code == http.StatusOK {
				if _, err := authz.ParseDecision(body); err == nil && json.Valid(body) && hasAllow(body) {
					t.Errorf("the mode %s answered a decision: %s", tc.mode, body)
				}
			}
		})
	}
}

// verdict says whether an answer is expected to carry one.
type verdict int

const noDecision verdict = 1

// hasAllow reports whether a body carries the field that makes it a
// decision.
func hasAllow(body []byte) bool {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return false
	}
	_, ok := out["allow"]
	return ok
}
