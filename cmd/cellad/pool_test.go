// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// TestPoolEndToEnd is spec 020's pool on a running node: two entries the
// refill loop made, a create that adopts one and says so, and a create the
// entries cannot serve that the driver runs for real.
//
// The driver is read directly beside the API, because an entry is the control
// plane's own machinery and no route reports one. That is the point of the
// case: what a caller sees is a sandbox either way.
func TestPoolEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	data := t.TempDir()
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
		"CELLA_DATA_DIR":      data,
		"CELLA_POOL_SIZE":     "2",
		// The loop's own tick is the reaper's, so the pool fills within a
		// second of the node coming up rather than in half a minute.
		"CELLA_REAP_INTERVAL": "1s",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})

	// The node owns the driver's root while it runs, and the native driver
	// keeps everything on disk, so a second instance over the same root
	// reads the entries the first one made.
	reader, err := native.New(filepath.Join(data, "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	pool := true
	entries := func() []runtime.State {
		states, err := reader.List(t.Context(), runtime.Filter{Pool: &pool})
		if err != nil {
			t.Fatal(err)
		}
		return states
	}
	waitFor(t, "the refill loop to reach the size", func() bool { return len(entries()) == 2 })

	held := map[string]time.Time{}
	for _, entry := range entries() {
		if entry.Owner != "" || entry.Phase != runtime.Running {
			t.Fatalf("the entry %s is not an unowned running sandbox: %+v", entry.ID, entry)
		}
		held[entry.ID] = entry.CreatedAt
	}

	adopted := create(t, base, alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"from-pool"},"spec":{}}`)
	prewarmedAt, fromPool := held[adopted.Status.ID]
	switch {
	case !fromPool:
		t.Fatalf("the sandbox %s is not one of the entries %v", adopted.Status.ID, held)
	case reasonOf(adopted, v1.ConditionScheduled) != v1.ReasonFromPool:
		t.Errorf("Scheduled is %q, want FromPool", reasonOf(adopted, v1.ConditionScheduled))
	case !adopted.Status.CreatedAt.After(prewarmedAt):
		t.Errorf("createdAt %v is not after the prewarm's %v", adopted.Status.CreatedAt, prewarmedAt)
	case adopted.Status.Phase != runtime.Running:
		t.Errorf("the adopted sandbox is %q, want Running", adopted.Status.Phase)
	}
	// The sandbox is the caller's on the driver too, and it is out of the
	// pool: an owner filter answers it and the pool filter does not.
	state, err := reader.Inspect(t.Context(), adopted.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pool || state.Owner == "" || state.Name != "from-pool" {
		t.Errorf("the driver holds the adopted sandbox as %+v", state)
	}

	// A manifest the entries cannot carry is created for real, under an id
	// no entry had.
	placed := create(t, base, alice,
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"for-real"},"spec":{"command":["sh","-c","sleep 30"]}}`)
	if _, wasEntry := held[placed.Status.ID]; wasEntry {
		t.Errorf("a manifest with a command took the entry %s", placed.Status.ID)
	}
	if reasonOf(placed, v1.ConditionScheduled) != v1.ReasonPlaced {
		t.Errorf("Scheduled is %q, want Placed", reasonOf(placed, v1.ConditionScheduled))
	}
	// The loop replaces what the adoption took.
	waitFor(t, "the refill loop to replace the adopted entry", func() bool { return len(entries()) == 2 })
}

// create posts one manifest with the answer held until the sandbox has
// started, which is what a case that uses the sandbox next needs, and returns
// the sandbox the API answered with.
func create(t *testing.T, base, token, body string) v1.Sandbox {
	t.Helper()
	return submit(t, base+"/v1/sandboxes?wait=1", token, body)
}

// submit posts one manifest to a create route as it is and returns the
// sandbox the API answered with. Without the hold the answer is the sandbox
// as soon as it is recorded, which is the answer a queued create reads.
func submit(t *testing.T, route, token, body string) v1.Sandbox {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: %d %s", route, resp.StatusCode, data)
	}
	var obj v1.Sandbox
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("the answer is not a sandbox: %v: %s", err, data)
	}
	return obj
}

// reasonOf is one condition's reason, or the empty string where the status
// carries no such condition.
func reasonOf(obj v1.Sandbox, kind string) string {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == kind {
			return cond.Reason
		}
	}
	return ""
}

// waitFor polls until the condition holds or the test fails. The loops run on
// their own goroutines, so a fixed sleep would be a race either way.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
