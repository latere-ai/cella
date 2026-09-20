// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package kind_test is the kind tier of specs 012 and 049: the lifecycle of
// a sandbox through the API of a `cellad` running in a cluster, with the
// stubs of spec 012 beside it, a token from the stub issuer, and the
// records of spec 009 read back from the stub sink.
//
// It runs two ways. With CELLA_TEST_KIND=1 it brings the stack up through
// deploy/examples/kind-stubs/up.sh and takes it down after. With
// CELLA_TEST_URL and CELLA_TEST_TOKEN it runs against a stack somebody else
// brought up, which is what the workflow does with the cluster the install
// document's walk left standing. With neither it skips and says which
// variable turns it on.
package kind_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ready bounds how long a Pod and its claim have to reach Ready on a
// cluster that may be pulling the sandbox's image.
const ready = 5 * time.Minute

// TestClusterLifecycle is the tier: create, read, exec, delete, with the
// events each act produced at the sink and the check Job green.
func TestClusterLifecycle(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}

	if code, body := call(t, client, http.MethodGet, url+"/livez", "", nil); code != http.StatusOK {
		t.Fatalf("the control plane answered %d at /livez: %s", code, body)
	}
	const name = "tier"
	manifest := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",
		"metadata":{"name":"` + name + `"},
		"spec":{"image":"docker.io/library/alpine:3.22","command":["sleep","600"]}}`
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes", token, []byte(manifest))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the create answered %d: %s", code, body)
	}
	t.Cleanup(func() {
		if code, body := call(t, client, http.MethodDelete, url+"/v1/sandboxes/"+name, token, nil); code >= 300 && code != http.StatusNotFound {
			t.Errorf("the delete answered %d: %s", code, body)
		}
	})
	id := statusField(t, body, "id")
	if id == "" {
		t.Fatalf("the create answered no id: %s", body)
	}

	deadline := time.Now().Add(ready)
	var phase string
	for time.Now().Before(deadline) {
		_, body = call(t, client, http.MethodGet, url+"/v1/sandboxes/"+name, token, nil)
		// Running is the phase of spec 005; Ready is a condition, not a
		// phase, and a sandbox never reaches a phase by that name.
		if phase = statusField(t, body, "phase"); phase == "Running" || phase == "Failed" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if phase != "Running" {
		t.Fatalf("the sandbox is %q after %s: %s", phase, ready, body)
	}

	code, body = call(t, client, http.MethodPost, url+"/v1/sandboxes/"+name+"/exec?wait=1", token,
		[]byte(`{"command":["echo","from the cluster"]}`))
	if code != http.StatusOK {
		t.Fatalf("the exec answered %d: %s", code, body)
	}
	if !strings.Contains(string(body), "from the cluster") {
		t.Errorf("the exec answered %s", body)
	}

	// The records of spec 009 reached the sink the control plane was
	// pointed at, which is the delivery half proved through a deployment
	// rather than in one process.
	if sink := os.Getenv("CELLA_TEST_SINK"); sink != "" {
		waitForRecord(t, client, sink, id)
	}
}

// stack is the URL and the token of the cluster under test, brought up
// here when the tier was told to.
func stack(t *testing.T) (url, token string) {
	t.Helper()
	if url, token = os.Getenv("CELLA_TEST_URL"), os.Getenv("CELLA_TEST_TOKEN"); url != "" && token != "" {
		return url, token
	}
	if os.Getenv("CELLA_TEST_KIND") != "1" {
		t.Skip("set CELLA_TEST_KIND=1 to bring a kind cluster up here, or CELLA_TEST_URL and CELLA_TEST_TOKEN to run against one")
	}
	for _, tool := range []string{"kind", "kubectl", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the kind tier needs %s on PATH: %v", tool, err)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(root, "deploy", "examples", "kind-stubs")
	cluster := os.Getenv("CELLA_TEST_CLUSTER")
	if cluster == "" {
		cluster = "cella-tier"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	up := exec.CommandContext(ctx, "bash", filepath.Join(overlay, "up.sh"), "-name", cluster)
	up.Dir = root
	up.Stderr = os.Stderr
	out, err := up.Output()
	t.Cleanup(func() {
		down := exec.Command("bash", filepath.Join(overlay, "down.sh"), "-name", cluster)
		down.Stderr, down.Stdout = os.Stderr, os.Stderr
		if err := down.Run(); err != nil {
			t.Errorf("the cluster was not deleted: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("the stack did not come up: %v", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch name {
		case "CELLA_TEST_URL":
			url = value
		case "CELLA_TEST_TOKEN":
			token = value
		}
	}
	if url == "" || token == "" {
		t.Fatalf("the stack printed %q", out)
	}
	t.Setenv("CELLA_TEST_SINK", "http://localhost:30082")
	return url, token
}

// call sends one request and returns the status and the body.
func call(t *testing.T, client *http.Client, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// The cleanup's delete runs after the test's context is done, so the
	// request carries the client's timeout and not the test's cancellation.
	req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), method, url, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, read
}

// statusField reads one member of a manifest's status.
func statusField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var out struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return fmt.Sprint(out.Status[field])
}

// waitForRecord waits until the sink holds a record about one object. The
// journal delivers in its own loop, so the tier waits rather than reading
// once and calling the absence a failure.
func waitForRecord(t *testing.T, client *http.Client, sink, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		code, body := call(t, client, http.MethodGet, sink+"/events?object="+id, "", nil)
		if code == http.StatusOK {
			var records []json.RawMessage
			if err := json.Unmarshal(body, &records); err == nil && len(records) > 0 {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("the sink holds no record about %s, and every act of spec 009 is delivered", id)
}
