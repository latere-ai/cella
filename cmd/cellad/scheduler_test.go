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

// TestQueuedEnvironmentEndToEnd is spec 057 on a running node: the environment
// cellad drives itself is queued with room for one sandbox, a second create
// waits in the queue with its place, the scrape says so, and the delete of the
// first places the second without a caller asking again.
func TestQueuedEnvironmentEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, internalURL, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":       issuer.URL(),
		"CELLA_OIDC_AUDIENCE":      "cella,platform.example",
		"CELLA_DATA_DIR":           t.TempDir(),
		"CELLA_SCHEDULING_MODE":    "queued",
		"CELLA_CAPACITY_SANDBOXES": "1",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	manifest := func(name string) string {
		return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name +
			`"},"spec":{"command":["sh","-c","sleep 60"],"scheduling":{"priority":1}}}`
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

	first := create(t, base, alice, manifest("first"))
	if first.Status.Phase != runtime.Running {
		t.Fatalf("the first create on an empty queue is %s", first.Status.Phase)
	}
	second := create(t, base, alice, manifest("second"))
	if second.Status.Phase != "Queued" || second.Spec.Scheduling.Queue != v1.DefaultQueueName {
		t.Fatalf("the second create is %s in the queue %q", second.Status.Phase, second.Spec.Scheduling.Queue)
	}
	for _, cond := range read(second.Status.ID).Status.Conditions {
		if cond.Type == v1.ConditionScheduled && cond.Message != "Position 1 of 1 in the queue default." {
			t.Fatalf("the queued sandbox reads its place as %q", cond.Message)
		}
	}
	out := scrape(t, internalURL)
	if v, ok := sample(out, `cella_queue_depth{environment="default",queue="default"}`); !ok || v != 1 {
		t.Fatalf("the scrape reads the queue as %v, %v", v, ok)
	}
	if v, ok := sample(out, `cella_capacity{environment="default",kind="used",resource="sandboxes"}`); !ok || v != 1 {
		t.Fatalf("the scrape reads the capacity in use as %v, %v", v, ok)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, base+"/v1/sandboxes/"+first.Status.ID, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE the first: %d", resp.StatusCode)
	}
	waitFor(t, "the queued sandbox to run once the first is gone", func() bool {
		return read(second.Status.ID).Status.Phase == runtime.Running
	})
}
