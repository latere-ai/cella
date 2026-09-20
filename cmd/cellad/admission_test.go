// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/internal/egressd"
)

const admissionToken = "admission-token"

// admissionEnvelope is the body the operator's endpoint decodes, copied
// field for field from the type the platform's own webhook declares. This
// tier drives the shape that endpoint reads and never imports it.
type admissionEnvelope struct {
	Subject     string          `json:"subject"`
	Issuer      string          `json:"issuer"`
	Sub         string          `json:"sub"`
	Claims      map[string]any  `json:"claims"`
	Workload    json.RawMessage `json:"workload"`
	Action      string          `json:"action"`
	Existing    *map[string]any `json:"existing"`
	Parent      *map[string]any `json:"parent"`
	Environment struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Isolation string `json:"isolation"`
	} `json:"environment"`
	Set        json.RawMessage `json:"set"`
	Manifest   map[string]any  `json:"manifest"`
	RequestRef struct {
		ID string `json:"id"`
	} `json:"request"`
}

// admissionEndpoint is an operator's policy endpoint as the platform's own
// is built: it decides from what it was sent, writes named fields into the
// document it received, and answers a refusal as a 200 with a code.
type admissionEndpoint struct {
	*httptest.Server
	seen  atomic.Pointer[admissionEnvelope]
	calls atomic.Int64
}

// The annotations a platform stamps. The prefix is one deployment's own
// and appears here because this tier stands in for that deployment.
const (
	stampImage = "platform.example.org/image"
	stampTier  = "platform.example.org/image-tier"
)

func startAdmission(t *testing.T) *admissionEndpoint {
	t.Helper()
	e := &admissionEndpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+admissionToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var in admissionEnvelope
		if err = json.Unmarshal(body, &in); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e.seen.Store(&in)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decide(&in))
	}))
	t.Cleanup(e.Close)
	return e
}

// decide is the policy: the plan's ceiling over the resolved figures, the
// plan's defaults into the absent ones, the tenant's egress floor, and the
// two annotations a catalogue stamps.
func decide(in *admissionEnvelope) map[string]any {
	spec, ok := in.Manifest["spec"].(map[string]any)
	if !ok {
		return map[string]any{"allow": false, "reason": "invalid_manifest"}
	}
	resources, _ := spec["resources"].(map[string]any)
	if resources == nil {
		resources = map[string]any{}
	}
	if cpu, _ := resources["cpu"].(string); cpu == "8" {
		return map[string]any{
			"allow":  false,
			"reason": "ceiling_exceeded: spec.resources.cpu is 8, above the plan's 4",
		}
	}
	if _, set := resources["memory"]; !set {
		resources["memory"] = "256Mi"
	}
	resources["cpu"] = "500m"
	spec["resources"] = resources
	spec["lifecycle"] = map[string]any{"autoStop": "15m", "ttl": "24h", "autoDelete": "72h"}
	// The plan's egress floor. Cella infers the mode before admission, so
	// a looser one is narrowed and never refused, and an allow list
	// subsumes the denied hosts that go with the open mode.
	egress, _ := spec["network"].(map[string]any)
	if egress == nil {
		egress = map[string]any{}
	}
	egress["egress"] = map[string]any{"mode": "allowlist", "allowedHosts": []string{"api.example.org"}}
	spec["network"] = egress
	metadata, _ := in.Manifest["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		in.Manifest["metadata"] = metadata
	}
	metadata["annotations"] = map[string]string{stampImage: "base", stampTier: "warm"}
	return map[string]any{
		"allow":    true,
		"manifest": in.Manifest,
		"warnings": []string{"Egress was narrowed to the mode this plan allows."},
	}
}

// TestServeWithAdmission is spec 047 over one running control plane: every
// create is one call to the operator's endpoint, the manifest that endpoint
// returns is what the caller reads back, a refusal carries the endpoint's
// own code, and an endpoint that has stopped answering fails every create
// closed rather than letting one through.
func TestServeWithAdmission(t *testing.T) {
	endpoint := startAdmission(t)
	proxyAddr, reverseAddr := freePort(t), freePort(t)
	plane := startPlaneWith(t, proxyAddr, reverseAddr, map[string]string{
		"CELLA_ADMISSION_URL":   endpoint.URL,
		"CELLA_ADMISSION_TOKEN": admissionToken,
	})
	if !strings.Contains(plane.out.String(), "admission=webhook") {
		t.Fatalf("the start-up line does not name the endpoint: %q", plane.out.String())
	}
	// The endpoint narrows the boundary to an allow list, which is a
	// boundary a gateway holds, so a gateway of the environment runs.
	ready := make(chan struct{})
	startGateway(t, plane, egressd.Options{
		ProxyAddr: proxyAddr, ReverseAddr: reverseAddr, Ready: func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	t.Run("theMutationIsWhatTheCallerReadsBack", func(t *testing.T) {
		status, answer := plane.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(
			`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",`+
				`"metadata":{"name":"admitted","labels":{"team":"research"}},"spec":{}}`))
		if status != http.StatusCreated {
			t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
		}
		var obj struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Resources map[string]string `json:"resources"`
				Lifecycle map[string]string `json:"lifecycle"`
				Network   struct {
					Egress struct {
						Mode         string   `json:"mode"`
						AllowedHosts []string `json:"allowedHosts"`
					} `json:"egress"`
				} `json:"network"`
			} `json:"spec"`
			Status struct {
				ID       string   `json:"id"`
				Warnings []string `json:"warnings"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(answer), &obj); err != nil {
			t.Fatal(err)
		}
		if obj.Metadata.Annotations[stampImage] != "base" || obj.Metadata.Annotations[stampTier] != "warm" {
			t.Errorf("annotations = %v, want the endpoint's stamp", obj.Metadata.Annotations)
		}
		if obj.Spec.Resources["cpu"] != "500m" || obj.Spec.Resources["memory"] != "256Mi" {
			t.Errorf("resources = %v, want the plan's", obj.Spec.Resources)
		}
		if obj.Spec.Lifecycle["ttl"] != "24h" {
			t.Errorf("lifecycle = %v, want the plan's", obj.Spec.Lifecycle)
		}
		if obj.Spec.Network.Egress.Mode != "allowlist" || len(obj.Spec.Network.Egress.AllowedHosts) != 1 {
			t.Errorf("egress = %+v, want the narrowed boundary", obj.Spec.Network.Egress)
		}
		if obj.Metadata.Labels["team"] != "research" {
			t.Errorf("a field the endpoint did not touch was lost: %v", obj.Metadata.Labels)
		}
		if !hasPrefix(obj.Status.Warnings, "Egress was narrowed") {
			t.Errorf("warnings = %v, want the endpoint's", obj.Status.Warnings)
		}
		// A read back over the wire carries the same object, so what the
		// endpoint decided is what the store holds.
		read := plane.get(t, "/v1/sandboxes/"+obj.Status.ID)
		if !strings.Contains(read, stampImage) || !strings.Contains(read, "allowlist") {
			t.Errorf("the object read back = %s", read)
		}

		// The endpoint was sent spec 007's envelope: the actor flattened,
		// the action, the environment summary and the request id.
		seen := endpoint.seen.Load()
		if seen == nil {
			t.Fatal("the endpoint was not called")
		}
		if seen.Action != "create" || seen.Existing != nil || seen.Parent != nil {
			t.Errorf("envelope = %+v", seen)
		}
		if seen.Sub != "alice" || seen.Issuer == "" || seen.Subject != seen.Issuer+"|alice" {
			t.Errorf("actor = %q %q %q", seen.Subject, seen.Issuer, seen.Sub)
		}
		if seen.Claims["sub"] != "alice" {
			t.Errorf("claims = %v", seen.Claims)
		}
		if seen.RequestRef.ID == "" {
			t.Error("the envelope carries no request id")
		}
		if seen.Environment.Name != "default" || seen.Environment.Isolation != "none" {
			t.Errorf("environment = %+v", seen.Environment)
		}
		if string(seen.Workload) != "null" || string(seen.Set) != "null" {
			t.Errorf("workload = %s, set = %s, want null on a person's create", seen.Workload, seen.Set)
		}
	})

	t.Run("aRefusalCarriesTheEndpointsCode", func(t *testing.T) {
		status, answer := plane.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(
			`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",`+
				`"metadata":{"name":"too-big"},"spec":{"resources":{"cpu":"8"}}}`))
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
		}
		if !strings.Contains(answer, "admission_refused") ||
			!strings.Contains(answer, "ceiling_exceeded: spec.resources.cpu is 8, above the plan's 4") {
			t.Fatalf("answer = %s", answer)
		}
	})

	t.Run("anEndpointThatStoppedFailsEveryCreateClosed", func(t *testing.T) {
		before := endpoint.calls.Load()
		endpoint.Close()
		status, answer := plane.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(
			`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"orphan"},"spec":{}}`))
		if status != http.StatusServiceUnavailable {
			t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
		}
		if !strings.Contains(answer, "admission_unavailable") {
			t.Fatalf("answer = %s", answer)
		}
		if endpoint.calls.Load() != before {
			t.Fatalf("calls = %d, want no retry against a stopped endpoint", endpoint.calls.Load()-before)
		}
		// The sandbox does not exist: a create that could not be decided on
		// is refused and never half made.
		if !strings.Contains(plane.get(t, "/v1/sandboxes"), `"items":[`) ||
			strings.Contains(plane.get(t, "/v1/sandboxes"), "orphan") {
			t.Fatal("the refused create left an object behind")
		}
	})
}

// TestServeWithoutAdmissionSaysBuiltin: an installation that configures no
// endpoint runs the identity step, and its start-up line says so.
func TestServeWithoutAdmissionSaysBuiltin(t *testing.T) {
	plane := startPlaneWith(t, freePort(t), freePort(t), nil)
	if !strings.Contains(plane.out.String(), "admission=builtin") {
		t.Fatalf("the start-up line = %q", plane.out.String())
	}
	status, answer := plane.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"plain"},"spec":{}}`))
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
	}
}

// TestServeRefusesAnEndpointWithoutABearer: a URL with no bearer stops the
// start rather than the first apply.
func TestServeRefusesAnEndpointWithoutABearer(t *testing.T) {
	var errOut syncBuffer
	code := run(t.Context(), nil, env(identity(t, map[string]string{
		"CELLA_DATA_DIR":      t.TempDir(),
		"CELLA_PUBLIC_ADDR":   "127.0.0.1:0",
		"CELLA_INTERNAL_ADDR": "127.0.0.1:0",
		"CELLA_ADMISSION_URL": "https://admission.example/hook",
	})), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "CELLA_ADMISSION_TOKEN is unset") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

func hasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
