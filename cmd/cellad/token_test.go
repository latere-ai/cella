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

	driver "latere.ai/x/cella/runtime"
)

// TestWorkloadTokenEndToEnd is spec 006's identity through a running node: a
// sandbox created over the API finds its own token at the path the projection
// names, calls the control plane back with it, reaches itself and nothing
// else, and holds nothing once it is deleted.
func TestWorkloadTokenEndToEnd(t *testing.T) {
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS": issuer.URL(),
		"CELLA_DATA_DIR":     t.TempDir(),
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice"})
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
	create := func(name string) string {
		t.Helper()
		body := call("POST", "/v1/sandboxes", alice,
			`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"`+name+`"},"spec":{}}`, 201)
		var obj struct {
			Status struct {
				ID         string          `json:"id"`
				TokenState json.RawMessage `json:"tokenState"`
			} `json:"status"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Status.ID == "" {
			t.Fatalf("the create answered %s", body)
		}
		if len(obj.Status.TokenState) != 0 {
			t.Errorf("the answer carries the control plane's token record: %s", obj.Status.TokenState)
		}
		return obj.Status.ID
	}

	mine, other := create("holder"), create("neighbor")
	// The token is read the way the agent client of spec 011 reads it:
	// through the variable the driver set, because the native driver has no
	// mount namespace to put the reserved path in.
	token := strings.TrimSpace(exec(t, call, mine, alice, `cat "$`+driver.TokenFileEnv+`"`))
	if strings.Count(token, ".") != 2 {
		t.Fatalf("the projection holds %q, want a compact JWS", token)
	}

	var read struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}
	if err := json.Unmarshal(call("GET", "/v1/sandboxes/"+mine, token, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Status.ID != mine {
		t.Fatalf("the sandbox read %s with its own token, want %s", read.Status.ID, mine)
	}
	// Its own routes and no others: a sibling is another sandbox.
	call("GET", "/v1/sandboxes/"+other, token, "", 403)
	call("DELETE", "/v1/sandboxes/"+other, token, "", 403)

	// The identity ends with the sandbox: the token is refused on the
	// request after the delete, not when it expires a day later.
	call("DELETE", "/v1/sandboxes/"+mine, alice, "", 202)
	call("GET", "/v1/sandboxes/"+other, token, "", 401)
}

// exec runs one command inside a sandbox and returns its stdout, failing on a
// nonzero exit.
func exec(t *testing.T, call func(method, path, token, body string, want int) []byte, id, caller, script string) string {
	t.Helper()
	request, err := json.Marshal(map[string]any{"command": []string{"sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(call("POST", "/v1/sandboxes/"+id+"/exec?wait=1", caller, string(request), 200), &out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("%s exited %d: %s", script, out.ExitCode, out.Stderr)
	}
	return out.Stdout
}
