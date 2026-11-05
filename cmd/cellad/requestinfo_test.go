// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/requestid"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/client"
)

// TestTheAuthorizerHearsTheCallerBehindAProxy: with CELLA_TRUSTED_PROXIES
// naming the proxy a request came through, the question's request block
// carries the address that proxy heard from and the id the answer carries,
// read back from the JSON the endpoint received. A header from a peer outside
// the ranges is not read.
func TestTheAuthorizerHearsTheCallerBehindAProxy(t *testing.T) {
	const token = "a-bearer-for-the-endpoint"
	ask := func(t *testing.T, trusted string) string {
		t.Helper()
		d := &recordingDecider{}
		endpoint := httptest.NewServer(server.New(server.Options{
			Bearer: token, Vocabulary: authorizer.Vocabulary(), Decider: d,
		}))
		t.Cleanup(endpoint.Close)
		p := startPlaneWith(t, "", "", map[string]string{
			"CELLA_AUTHORIZER_URL": endpoint.URL, "CELLA_AUTHORIZER_TOKEN": token,
			"CELLA_TRUSTED_PROXIES": trusted,
		})
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, p.origin+client.Route(p.base, "/v1/sandboxes"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+p.bearer)
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("the list answered %d", res.StatusCode)
		}
		asked := d.of(authorizer.ActionSandboxList)
		if len(asked) == 0 {
			t.Fatal("the list asked the endpoint nothing")
		}
		last := asked[len(asked)-1].Request
		if last.ID == "" || last.ID != res.Header.Get(requestid.Header) {
			t.Errorf("the endpoint heard the request id %q, the answer carries %q", last.ID, res.Header.Get(requestid.Header))
		}
		return last.IP
	}
	if ip := ask(t, "127.0.0.0/8"); ip != "198.51.100.7" {
		t.Errorf("behind a trusted proxy the endpoint heard the caller as %q, want the address the proxy heard from", ip)
	}
	if ip := ask(t, "10.0.0.0/8"); ip != "127.0.0.1" {
		t.Errorf("from a peer outside the ranges the endpoint heard the caller as %q, want the peer", ip)
	}
}
