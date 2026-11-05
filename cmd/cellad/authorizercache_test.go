// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/server"

	"latere.ai/x/cella/authorizer"
)

// TestTheNodeHoldsAnAllowForTheConfiguredCache: an allow whose answer
// names no ttl is reused for the variable's time and asked again after it,
// rather than held for the shared sixty seconds.
func TestTheNodeHoldsAnAllowForTheConfiguredCache(t *testing.T) {
	const token = "a-bearer-for-the-endpoint"
	d := &recordingDecider{}
	endpoint := httptest.NewServer(server.New(server.Options{
		Bearer: token, Vocabulary: authorizer.Vocabulary(), Decider: d,
	}))
	t.Cleanup(endpoint.Close)
	p := startPlaneWith(t, "", "", map[string]string{
		"CELLA_AUTHORIZER_URL": endpoint.URL, "CELLA_AUTHORIZER_TOKEN": token,
		"CELLA_AUTHORIZER_CACHE": "1s",
	})
	read := func() {
		t.Helper()
		if status, answer := p.do(t, http.MethodGet, "/v1/environments/default", nil); status != http.StatusOK {
			t.Fatalf("the read answered %d: %s", status, answer)
		}
	}
	read()
	read()
	if n := len(d.of(authorizer.ActionEnvironmentRead)); n != 1 {
		t.Fatalf("the endpoint was asked %d time(s) for two reads inside the cache", n)
	}
	time.Sleep(1500 * time.Millisecond)
	read()
	if n := len(d.of(authorizer.ActionEnvironmentRead)); n != 2 {
		t.Fatalf("the endpoint was asked %d time(s); an allow outlived CELLA_AUTHORIZER_CACHE=1s", n)
	}
}
