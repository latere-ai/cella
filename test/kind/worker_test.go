// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package kind_test

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// workerEnvironment is the worker-mode Environment the overlay's
// worker.yaml declares, applied and keyed by up.sh the way the gateway's
// environment key is.
const workerEnvironment = "worker"

// TestClusterWorkerNoInbound is spec 013's control plane model, row 4, on
// the cluster the stack runs: the worker's Pod carries a NetworkPolicy that
// admits no ingress at all, and the whole lifecycle still runs on its
// environment, because nothing ever dials a worker (spec 021). Refusing
// every inbound connection to the real deployment changes nothing it
// needs: the worker connects outbound with its key and claims what the
// control plane enqueues for it over that one stream.
func TestClusterWorkerNoInbound(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}
	awaitLive(t, client, url)

	out, err := exec.Command("kubectl", "--context", "kind-"+clusterName(), "-n", "cella",
		"get", "networkpolicy", "cellad-worker", "-o", "json").Output()
	if err != nil {
		t.Fatalf("reading the worker's network policy: %v", err)
	}
	var policy struct {
		Spec struct {
			PolicyTypes []string `json:"policyTypes"`
			Ingress     []any    `json:"ingress"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &policy); err != nil {
		t.Fatalf("the network policy did not decode: %v", err)
	}
	if !slices.Contains(policy.Spec.PolicyTypes, "Ingress") || len(policy.Spec.Ingress) != 0 {
		t.Fatalf("the worker's network policy is %s, want Ingress with no rule, which admits no ingress at all", out)
	}

	// The native driver behind the worker environment refuses a manifest
	// that names an image (runtime/native.Create), so this sandbox names
	// none, exactly as an operator self-hosting on their own machine would
	// write one.
	const name = "tier-worker"
	manifest := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",
		"metadata":{"name":"` + name + `"},
		"spec":{"environment":"` + workerEnvironment + `","command":["sleep","600"]}}`
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes", token, []byte(manifest))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the create answered %d: %s", code, body)
	}
	t.Cleanup(func() {
		if code, body := call(t, client, http.MethodDelete, url+"/v1/sandboxes/"+name, token, nil); code >= 300 && code != http.StatusNotFound {
			t.Errorf("the delete answered %d: %s", code, body)
		}
	})
	awaitRunning(t, client, url, token, name)

	code, body = call(t, client, http.MethodPost, url+"/v1/sandboxes/"+name+"/exec?wait=1", token,
		[]byte(`{"command":["echo","from the worker"]}`))
	if code != http.StatusOK {
		t.Fatalf("the exec answered %d: %s", code, body)
	}
	if !strings.Contains(string(body), "from the worker") {
		t.Errorf("the exec answered %s", body)
	}
}
