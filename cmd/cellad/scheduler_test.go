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

// TestPreemptionEndToEnd is spec 058 on a running node: the environment cellad
// drives itself is queued with room for one sandbox, a create of higher
// priority stops the preemptible one running there and takes its place, the
// scrape counts the stop, and once the higher one is deleted the stopped one
// runs again with the files it had.
func TestPreemptionEndToEnd(t *testing.T) {
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
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
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
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, data)
		}
		return data
	}
	read := func(id string) v1.Sandbox {
		t.Helper()
		var obj v1.Sandbox
		if err := json.Unmarshal(call(http.MethodGet, "/v1/sandboxes/"+id, alice, "", http.StatusOK), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	manifest := func(name, scheduling string) string {
		return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name +
			`"},"spec":{"command":["sh","-c","sleep 60"],"scheduling":` + scheduling + `}}`
	}

	low := create(t, base, alice, manifest("low", `{"preemptible":true}`))
	if low.Status.Phase != runtime.Running {
		t.Fatalf("the preemptible create on an empty queue is %s", low.Status.Phase)
	}
	exec(t, call, low.Status.ID, alice, "echo kept > marker")
	high := create(t, base, alice, manifest("high", `{"priority":5}`))
	if high.Status.Phase != "Queued" {
		t.Fatalf("the higher create is answered %s", high.Status.Phase)
	}
	// The create that was queued woke the loop, which stops the lower one
	// and places the higher one without waiting for a tick.
	waitFor(t, "the higher create to run", func() bool { return read(high.Status.ID).Status.Phase == runtime.Running })
	preempted := read(low.Status.ID)
	if preempted.Status.Phase != "Queued" || preempted.Status.Reason != v1.ReasonPreempted ||
		preempted.Status.Preemptions != 1 || reasonOf(preempted, v1.ConditionScheduled) != v1.ReasonPreempted {
		t.Fatalf("the preempted sandbox reads %s %s after %d with Scheduled %s", preempted.Status.Phase,
			preempted.Status.Reason, preempted.Status.Preemptions, reasonOf(preempted, v1.ConditionScheduled))
	}
	call(http.MethodPost, "/v1/sandboxes/"+low.Status.ID+"/exec?wait=1", alice, `{"command":["true"]}`, http.StatusConflict)
	out := scrape(t, internalURL)
	if v, ok := sample(out, "cella_preemptions_total"); !ok || v != 1 {
		t.Fatalf("the scrape counts %v preemptions, %v", v, ok)
	}
	if v, ok := sample(out, `cella_queue_depth{environment="default",queue="default"}`); !ok || v != 1 {
		t.Fatalf("the scrape reads the queue as %v, %v", v, ok)
	}

	call(http.MethodDelete, "/v1/sandboxes/"+high.Status.ID, alice, "", http.StatusAccepted)
	waitFor(t, "the preempted sandbox to run again", func() bool { return read(low.Status.ID).Status.Phase == runtime.Running })
	if got := exec(t, call, low.Status.ID, alice, "cat marker"); got != "kept\n" {
		t.Fatalf("the resumed sandbox reads %q from the file it wrote before its stop", got)
	}
}
