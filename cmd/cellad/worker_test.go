// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestWorkerEndToEnd runs both roles of spec 021 in one process over
// loopback: cellad serve with the native driver, and cellad worker with its
// own native driver, joined by a key minted through the route an operator
// uses. Nothing dials the worker; the worker opens the one connection, which
// is invariant 10 of spec 001.
func TestWorkerEndToEnd(t *testing.T) {
	p := startPlane(t, "", "")
	key := p.environmentKey(t)

	// The worker reads its own variables and none of the control plane's.
	first := startWorker(t, p, key, map[string]string{"CELLA_WORKER_LABELS": "region=eu"})

	// The worker announces what it runs before it connects, so an operator
	// reading the log sees the driver and the control plane it joined.
	waitFor(t, "the worker to announce itself",
		func() bool { return strings.Contains(first.out.String(), "worker driver=native") })

	// The environment reports the worker once its stream is up. This is the
	// whole of the seam: the key authenticated a registration and a stream,
	// and the control plane made no outbound connection at all.
	waitFor(t, "the environment to report the worker", func() bool { return p.environment(t).Status.Workers == 1 })
	obj := p.environment(t)
	if obj.Status.LastHeartbeat.IsZero() {
		t.Errorf("the environment reports a worker and no heartbeat: %+v", obj.Status)
	}

	// A worker that goes away leaves the environment with none, which is
	// what the phase of spec 021 is computed from.
	if code := first.stop(); code != 0 {
		t.Errorf("the worker exited %d; stderr %q", code, first.errOut.String())
	}
	waitFor(t, "the environment to report no worker", func() bool { return p.environment(t).Status.Workers == 0 })

	// And a worker that comes back registers again and the environment holds
	// one once more, without anything having dialed it.
	startWorker(t, p, key, nil)
	waitFor(t, "the environment to report the worker that came back",
		func() bool { return p.environment(t).Status.Workers == 1 })
}

// runningWorker is one cellad worker running in this process: its output, and
// the stop that ends it and reports the exit code once.
type runningWorker struct {
	out, errOut *syncBuffer
	stop        func() int
}

// startWorker runs the worker role against the plane with the key an
// administrator minted, under the variables the role reads and no others.
func startWorker(t *testing.T, p *plane, key string, extra map[string]string) *runningWorker {
	t.Helper()
	e := map[string]string{
		"CELLA_URL":                 p.url,
		"CELLA_ENVIRONMENT_KEY":     key,
		"CELLA_RUNTIME":             "native",
		"CELLA_ALLOW_UNSAFE_NATIVE": "true",
		"CELLA_DATA_DIR":            t.TempDir(),
	}
	maps.Copy(e, extra)
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	var out, errOut syncBuffer
	go func() { codec <- run(ctx, []string{"worker"}, env(e), &out, &errOut) }()
	w := &runningWorker{out: &out, errOut: &errOut}
	w.stop = sync.OnceValue(func() int {
		cancel()
		select {
		case code := <-codec:
			return code
		case <-time.After(30 * time.Second):
			t.Errorf("the worker did not stop; stdout %q stderr %q", out.String(), errOut.String())
			return -1
		}
	})
	t.Cleanup(func() { w.stop() })
	return w
}

// TestWorkerRefusesARevokedKey holds spec 021's rule at the seam: the key is
// what authenticates a worker, and revoking it ends the registration route
// for that worker at once.
func TestWorkerRefusesARevokedKey(t *testing.T) {
	p := startPlane(t, "", "")
	status, body := p.do(t, http.MethodPost, "/v1/environments/default/keys", nil)
	if status != http.StatusCreated {
		t.Fatalf("the key mint answered %d: %s", status, body)
	}
	var minted struct {
		Token string `json:"token"`
		JTI   string `json:"jti"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	status, body = p.do(t, http.MethodDelete, "/v1/environments/default/keys/"+minted.JTI, nil)
	if status != http.StatusNoContent {
		t.Fatalf("the revocation answered %d: %s", status, body)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		p.url+"/v1/environments/self/workers", strings.NewReader(`{"driver":"native","isolation":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked key registered a worker with %s", res.Status)
	}
}

// TestWorkerRoleRefusesItsConfiguration holds that the role fails to start
// rather than running half configured, which is spec 002's rule for a role.
func TestWorkerRoleRefusesItsConfiguration(t *testing.T) {
	var out, errOut syncBuffer
	code := run(t.Context(), []string{"worker"}, env(map[string]string{
		"CELLA_URL": "http://control.example.test", "CELLA_ENVIRONMENT_KEY": "k",
	}), &out, &errOut)
	if code != 1 {
		t.Errorf("a worker with an http control plane off loopback exited %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "CELLA_INSECURE_CONTROL_PLANE") {
		t.Errorf("the refusal does not name the hatch: %q", errOut.String())
	}
	var vOut, vErr syncBuffer
	if code = run(t.Context(), []string{"worker", "-version"}, env(nil), &vOut, &vErr); code != 0 {
		t.Errorf("cellad worker -version exited %d: %q", code, vErr.String())
	}
	if strings.TrimSpace(vOut.String()) == "" {
		t.Errorf("cellad worker -version printed nothing")
	}
}

// environment reads the environment object the control plane serves.
func (p *plane) environment(t *testing.T) v1.Environment {
	t.Helper()
	status, body := p.do(t, http.MethodGet, "/v1/environments/default", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the environment answered %d: %s", status, body)
	}
	var obj v1.Environment
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("the environment did not decode: %v", err)
	}
	return obj
}
