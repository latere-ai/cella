// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRequestID proves design 008's rule for the id every answer carries: a
// client's own within the rule is echoed, anything else is replaced by a
// `req_` and a ULID, and the envelope of a refusal names the same id.
func TestRequestID(t *testing.T) {
	f := setup(t, nil)
	minted := func(sent string) (string, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, f.url+"/v1/sandboxes/sbx_01j0000000000000000000000", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		if sent != "" {
			req.Header.Set(RequestIDHeader, sent)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.Header.Get(RequestIDHeader), body
	}
	id, body := minted("")
	if !strings.HasPrefix(id, RequestIDPrefix) || len(id) != len(RequestIDPrefix)+26 {
		t.Fatalf("a minted id is %q, want %s and twenty-six characters", id, RequestIDPrefix)
	}
	if !strings.Contains(string(body), id) {
		t.Fatalf("the envelope names another id than the header %q: %s", id, body)
	}
	if second, _ := minted(""); second == id {
		t.Fatalf("two requests carry one id, %q", id)
	}
	for _, sent := range []string{"req_from-a-client", strings.Repeat("a", maxRequestIDBytes)} {
		if got, _ := minted(sent); got != sent {
			t.Errorf("a client id within the rule was replaced: sent %q, got %q", sent, got)
		}
	}
	for _, sent := range []string{strings.Repeat("a", maxRequestIDBytes+1), "aéb", "a\tb"} {
		got, _ := minted(sent)
		if got == sent {
			t.Errorf("a client id outside the rule was echoed: %q", sent)
		}
		if !strings.HasPrefix(got, RequestIDPrefix) {
			t.Errorf("a replaced id is %q", got)
		}
	}
}
