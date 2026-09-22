// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// TestJournalRetentionEndToEnd is the memory journal's bound on a running
// node: with CELLA_JOURNAL_CAP at three, a sandbox that produced more records
// than that reads back its newest three through the feed, numbered as they
// were written.
func TestJournalRetentionEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
		"CELLA_DATA_DIR":      t.TempDir(),
		"CELLA_JOURNAL_CAP":   "3",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}
	obj := create(t, base, alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"busy"},"spec":{}}`)
	for range 5 {
		request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", `{"command":["true"]}`, http.StatusOK)
	}
	var page struct {
		Items []struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if err := json.Unmarshal(request("GET", "/v1/events?object="+obj.Status.ID+"&limit=50", "", http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("the feed holds %d records with a cap of three: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].Seq <= 3 || page.Items[0].Seq-page.Items[2].Seq != 2 {
		t.Fatalf("the ring keeps %+v, want the three newest in sequence", page.Items)
	}
}
