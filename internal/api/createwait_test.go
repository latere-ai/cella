// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// gatedDriver holds every create until the case releases it, which is a
// runtime that provisions and attaches storage before a workload starts.
type gatedDriver struct {
	runtime.Driver
	release chan struct{}
}

func (d gatedDriver) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
		return runtime.Ref{}, ctx.Err()
	}
	return d.Driver.Create(ctx, s)
}

// createdAs is one create answered with its object and its Location.
func (f *fixture) createdAs(path, body string, status int) (v1.Sandbox, string) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.alice)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != status {
		f.t.Fatalf("POST %s answered %d, want %d", path, res.StatusCode, status)
	}
	var obj v1.Sandbox
	if status == http.StatusCreated {
		if err := json.NewDecoder(res.Body).Decode(&obj); err != nil {
			f.t.Fatal(err)
		}
	}
	return obj, res.Header.Get("Location")
}

// TestCreateWait holds the create's two answers. Without the hold, a create
// answers 201 Pending with its Location while the runtime still has the
// create in hand. With ?wait=1 the answer is held: at the bound it is the
// sandbox as it stands, a sandbox deleted while it is held answers
// not_found, and once the runtime answers it is Running. A bound that is not
// a positive duration of at most an hour refuses the request and records
// nothing.
func TestCreateWait(t *testing.T) {
	release := make(chan struct{})
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return gatedDriver{Driver: d, release: release} })

	first, location := f.createdAs("/v1/sandboxes", named("first"), http.StatusCreated)
	if first.Status.Phase != runtime.Pending || location != "/v1/sandboxes/"+first.Status.ID {
		t.Fatalf("the create answered %s at %q, want Pending at its own path", first.Status.Phase, location)
	}

	bounded, _ := f.createdAs("/v1/sandboxes?wait=1&timeout=200ms", named("bounded"), http.StatusCreated)
	if bounded.Status.Phase != runtime.Pending {
		t.Fatalf("a hold whose bound passed answered %s, want the Pending it stands in", bounded.Status.Phase)
	}

	for _, timeout := range []string{"0s", "-1s", "2h", "soon"} {
		f.request("POST", "/v1/sandboxes?wait=1&timeout="+timeout, f.alice, named("refused"), http.StatusBadRequest)
	}
	f.request("GET", "/v1/sandboxes/refused", f.alice, "", http.StatusNotFound)

	held := make(chan int, 1)
	go func() {
		status, _ := f.located(http.MethodPost, "/v1/sandboxes?wait=1", named("held"))
		held <- status
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _ := f.call("GET", "/v1/sandboxes/held", f.alice, "", "")
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the held create was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.request("DELETE", "/v1/sandboxes/held", f.alice, "", http.StatusAccepted)
	if status := <-held; status != http.StatusNotFound {
		t.Fatalf("a create deleted while its answer was held answered %d, want 404", status)
	}

	close(release)
	running, _ := f.createdAs("/v1/sandboxes?wait=1", named("running"), http.StatusCreated)
	if running.Status.Phase != runtime.Running {
		t.Fatalf("the held create answered %s, want Running", running.Status.Phase)
	}
	// The update half of an apply takes the hold as well, and answers a
	// sandbox that runs at once.
	var applied v1.Sandbox
	if err := json.Unmarshal(f.request("PUT", "/v1/sandboxes/first?wait=1", f.alice, named("first"), http.StatusOK), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Status.Phase != runtime.Running {
		t.Fatalf("the held apply answered %s, want Running", applied.Status.Phase)
	}
	f.request("PUT", "/v1/sandboxes/first?wait=1&timeout=soon", f.alice, named("first"), http.StatusBadRequest)
}

// TestNoGatewayHasItsOwnCode: a manifest whose egress boundary needs a
// gateway, on a control plane with none connected, is refused on the request
// with egress_gateway_unavailable, which names the remedy, rather than with
// driver_unavailable, which says the runtime is down. Nothing is recorded.
func TestNoGatewayHasItsOwnCode(t *testing.T) {
	f := setup(t, nil)
	bounded := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bounded"},` +
		`"spec":{"network":{"egress":{"mode":"allowlist","allowedHosts":["api.example.com"]}}}}`
	body := f.request("POST", "/v1/sandboxes", f.alice, bounded, http.StatusServiceUnavailable)
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "egress_gateway_unavailable" || !strings.Contains(envelope.Error.Message, "connect one, or open the boundary") {
		t.Fatalf("the refusal is %+v, want egress_gateway_unavailable with its remedy", envelope.Error)
	}
	f.request("GET", "/v1/sandboxes/bounded", f.alice, "", http.StatusNotFound)
}
