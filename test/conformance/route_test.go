// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestTheSuiteComposesUnderABasePath: a URL with a path is the base the server
// is served under, so a case's route reaches the server with the base in the
// place of /v1 and a document under the base, the query kept; a URL with no
// path reaches every route as written.
func TestTheSuiteComposesUnderABasePath(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		base string
		want []string
	}{
		{server.URL + "/v1/environments", []string{
			"/v1/environments/sandboxes?limit=5", "/v1/environments/environments/default",
			"/v1/environments/.well-known/jwks.json", "/v1/environments/openapi.yaml", "/v1/environments/version",
		}},
		{server.URL, []string{
			"/v1/sandboxes?limit=5", "/v1/environments/default",
			"/.well-known/jwks.json", "/openapi.yaml", "/version",
		}},
	} {
		mu.Lock()
		seen = nil
		mu.Unlock()
		c := newClient(tc.base, "a-token")
		for _, route := range []string{"/v1/sandboxes?limit=5", "/v1/environments/default", "/.well-known/jwks.json", "/openapi.yaml", "/version"} {
			if _, err := c.get(t.Context(), route); err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		got := append([]string(nil), seen...)
		mu.Unlock()
		if len(got) != len(tc.want) {
			t.Fatalf("under %s the server saw %v, want %v", tc.base, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("under %s the route reached %s, want %s", tc.base, got[i], tc.want[i])
			}
		}
		if c.base != tc.base {
			t.Errorf("the client keeps the address %s, want the one it was given, %s", c.base, tc.base)
		}
	}
}
