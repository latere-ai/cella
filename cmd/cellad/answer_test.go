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

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// TestCreateAnswersAtOnceEndToEnd is the create's two answers against a
// running node. A create answers 201 with the sandbox Pending at its own
// Location, and the node's scheduler loop brings it to Running with nothing
// else asked; a create with ?wait=1 answers once the sandbox runs. Both
// sandboxes run commands.
func TestCreateAnswersAtOnceEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS": issuer.URL(), "CELLA_OIDC_AUDIENCE": "cella", "CELLA_DATA_DIR": t.TempDir(),
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"cella"}})
	manifest := func(name string) string {
		return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},"spec":{}}`
	}
	read := func(id string) v1.Sandbox {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/sandboxes/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var obj v1.Sandbox
		if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &obj) != nil {
			t.Fatalf("GET %s: %d %s", id, resp.StatusCode, data)
		}
		return obj
	}

	answered := submit(t, base+"/v1/sandboxes", alice, manifest("at-once"))
	if answered.Status.Phase != runtime.Pending {
		t.Fatalf("the create answered %s, want Pending", answered.Status.Phase)
	}
	waitFor(t, "the loop to run the sandbox", func() bool { return read(answered.Status.ID).Status.Phase == runtime.Running })

	held := create(t, base, alice, manifest("held"))
	if held.Status.Phase != runtime.Running {
		t.Fatalf("the held create answered %s, want Running", held.Status.Phase)
	}
	for _, id := range []string{answered.Status.ID, held.Status.ID} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/sandboxes/"+id+"/exec?wait=1",
			strings.NewReader(`{"command":["true"]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("exec in %s answered %d", id, resp.StatusCode)
		}
	}
}
