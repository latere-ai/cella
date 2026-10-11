// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// sizing is an environment whose driver sizes the named resources inside a
// ceiling, as the resolver looks it up.
func sizing(flags v1.Autosize) v1.Environment {
	env := manifest.NativeEnvironment("default")
	env.Spec.Isolation = v1.IsolationContainer
	env.Status = v1.EnvironmentStatus{
		ID: "env_01J9", Driver: "k8s", Isolation: v1.IsolationContainer,
		Capabilities: v1.Capabilities{Autosize: flags},
	}
	return env
}

// ceilingEndpoint is an operator's endpoint that gives every sandbox a small
// start and the ceiling it is told, where the environment's driver sizes
// that resource, and leaves the manifest alone where it does not. It reads
// the flags from the request's environment, which is the only place an
// endpoint learns them.
func ceilingEndpoint(t *testing.T, max map[string]any) func(http.ResponseWriter, []byte) {
	t.Helper()
	return func(w http.ResponseWriter, body []byte) {
		var in struct {
			Environment struct {
				Capabilities struct {
					Autosize map[string]bool `json:"autosize"`
				} `json:"capabilities"`
			} `json:"environment"`
			Manifest map[string]any `json:"manifest"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			t.Error(err)
			return
		}
		flags := in.Environment.Capabilities.Autosize
		if len(flags) != 3 {
			t.Errorf("the environment declares the autosize flags %v, want cpu, memory and disk", flags)
		}
		granted := map[string]any{}
		for resource, ceiling := range max {
			if flags[resource] {
				granted[resource] = ceiling
			}
		}
		if len(granted) > 0 {
			spec := in.Manifest["spec"].(map[string]any)
			spec["resources"] = map[string]any{"cpu": "50m", "memory": "256Mi", "max": granted}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": true, "manifest": in.Manifest})
	}
}

// TestAdmissionSetsTheCeiling is spec 078's admission row through the client
// and the whole of Resolve: a ceiling the endpoint adds to a manifest that
// named no size is accepted, held to the amount it bounds, to the server's
// ceiling and to the driver's capability, and kept in the resolved manifest;
// and where the capability is absent an endpoint that reads it leaves the
// sandbox fixed.
func TestAdmissionSetsTheCeiling(t *testing.T) {
	resolve := func(env v1.Environment, max map[string]any, ceilings manifest.Ceilings) (v1.Resources, error) {
		e := serve(t, ceilingEndpoint(t, max))
		obj := v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "session"}}
		got, err := manifest.Resolve(t.Context(), &obj, manifest.Options{
			Lookup:   manifest.FixedEnvironment(env),
			Defaults: manifest.Defaults{Image: "registry.example/base:1", CPU: "1", Memory: "1Gi", Disk: "5Gi"},
			Ceilings: ceilings,
			Admit:    client(t, e, Options{}).Admit,
		})
		if err != nil {
			return v1.Resources{}, err
		}
		return got.Sandbox.Spec.Resources, nil
	}
	refused := func(err error) (string, string) {
		var known *manifest.Error
		if !errors.As(err, &known) {
			t.Fatalf("got %v, want a manifest error", err)
		}
		return known.Code, known.Path
	}
	both := v1.Autosize{CPU: true, Memory: true}

	got, err := resolve(sizing(both), map[string]any{"cpu": "2", "memory": "4Gi"}, manifest.Ceilings{CPU: "4", Memory: "8Gi"})
	if err != nil {
		t.Fatal(err)
	}
	want := v1.Resources{CPU: "50m", Memory: "256Mi", Max: v1.ResourceMax{CPU: "2", Memory: "4Gi"}}
	if got != want {
		t.Fatalf("resources = %+v, want %+v", got, want)
	}

	_, err = resolve(sizing(both), map[string]any{"memory": "128Mi"}, manifest.Ceilings{})
	if code, path := refused(err); code != "invalid_field" || path != "spec.resources.max.memory" {
		t.Errorf("a ceiling below the amount: got %q at %q", code, path)
	}
	_, err = resolve(sizing(both), map[string]any{"memory": "16Gi"}, manifest.Ceilings{Memory: "8Gi"})
	if code, path := refused(err); code != "ceiling_exceeded" || path != "spec.resources.max.memory" {
		t.Errorf("a ceiling above the server's: got %q at %q", code, path)
	}

	// The driver sizes cpu and no memory: the endpoint grants the one, and
	// a manifest that had asked for both would have been refused at memory.
	got, err = resolve(sizing(v1.Autosize{CPU: true}), map[string]any{"cpu": "2", "memory": "4Gi"}, manifest.Ceilings{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Max != (v1.ResourceMax{CPU: "2"}) {
		t.Errorf("the ceiling is %+v, want cpu alone", got.Max)
	}

	// A driver that sizes nothing: the endpoint sets nothing and the
	// sandbox takes the server's defaults, fixed.
	got, err = resolve(sizing(v1.Autosize{}), map[string]any{"cpu": "2", "memory": "4Gi"}, manifest.Ceilings{})
	if err != nil {
		t.Fatal(err)
	}
	if want = (v1.Resources{CPU: "1", Memory: "1Gi", Disk: "5Gi"}); got != want {
		t.Errorf("resources = %+v, want %+v", got, want)
	}
}

// TestAnEndpointThatIgnoresTheCapabilityIsRefused: an endpoint that sets a
// ceiling without reading the flags is refused at the field on a driver that
// holds that resource fixed, so a sandbox is never created with a bound
// nothing grows toward.
func TestAnEndpointThatIgnoresTheCapabilityIsRefused(t *testing.T) {
	e := serve(t, allow(t, func(m map[string]any) {
		m["spec"].(map[string]any)["resources"] = map[string]any{
			"memory": "256Mi", "max": map[string]any{"memory": "1Gi"},
		}
	}))
	obj := v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "session"}}
	_, err := manifest.Resolve(t.Context(), &obj, manifest.Options{
		Lookup:   manifest.FixedEnvironment(sizing(v1.Autosize{CPU: true})),
		Defaults: manifest.Defaults{Image: "registry.example/base:1"},
		Admit:    client(t, e, Options{}).Admit,
	})
	var known *manifest.Error
	if !errors.As(err, &known) || known.Code != "capability_unsupported" || known.Path != "spec.resources.max.memory" {
		t.Fatalf("got %v, want capability_unsupported at spec.resources.max.memory", err)
	}
}
