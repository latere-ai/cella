// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// environment is the smallest object that resolves: a worker's data plane
// with a container driver and a declared ceiling.
func environment(mutate ...func(*v1.Environment)) *v1.Environment {
	obj := &v1.Environment{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindEnvironment,
		Metadata:   v1.Metadata{Name: "eu-gpu"},
		Spec: v1.EnvironmentSpec{
			Isolation: v1.IsolationContainer,
			Capacity:  v1.Capacity{CPU: "512", Memory: "2Ti", Disk: "20Ti", Sandboxes: 400},
		},
	}
	for _, m := range mutate {
		m(obj)
	}
	return obj
}

func codeAndPath(t *testing.T, err error) (code, path string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("the refusal is not a manifest error: %v", err)
	}
	return e.Code, e.Path
}

func TestEnvironmentDefaults(t *testing.T) {
	out, err := ResolveEnvironment(environment(), EnvironmentOptions{})
	if err != nil {
		t.Fatalf("the smallest environment was refused: %v", err)
	}
	if out.Spec.Mode != v1.EnvironmentWorker {
		t.Errorf("an environment a caller applies defaults to %q, not %q", v1.EnvironmentWorker, out.Spec.Mode)
	}
	if out.Spec.Scheduling.Mode != v1.SchedulingDirect {
		t.Errorf("scheduling defaults to %q, not %q", v1.SchedulingDirect, out.Spec.Scheduling.Mode)
	}
	if !slices.Equal(out.Spec.Scheduling.Queues, []string{v1.DefaultQueueName}) {
		t.Errorf("the queues default to [default], not %v", out.Spec.Scheduling.Queues)
	}
	if out.Spec.Scheduling.DefaultQueue != v1.DefaultQueueName {
		t.Errorf("the default queue is %q, not %q", v1.DefaultQueueName, out.Spec.Scheduling.DefaultQueue)
	}
	if out.Status.Phase != "" {
		t.Errorf("a resolve writes no status, and this one carries phase %q", out.Status.Phase)
	}
}

// TestResolveEnvironmentNeverWritesThroughItsInput holds the determinism rule:
// the resolver copies every reference it is handed.
func TestResolveEnvironmentNeverWritesThroughItsInput(t *testing.T) {
	in := environment(func(e *v1.Environment) {
		e.Metadata.Labels = map[string]string{"region": "eu"}
		e.Spec.Scheduling.Queues = []string{"rollouts"}
	})
	before, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("the input did not marshal: %v", err)
	}
	out, err := ResolveEnvironment(in, EnvironmentOptions{})
	if err != nil {
		t.Fatalf("the environment was refused: %v", err)
	}
	out.Metadata.Labels["region"] = "us"
	out.Spec.Scheduling.Queues[0] = "other"
	after, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("the input did not marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the resolver wrote through its input:\n before %s\n after  %s", before, after)
	}
}

// TestEnvironmentFieldRules is spec 021's field table, one refusing case per
// rule, each naming the code and the path the API answers with.
func TestEnvironmentFieldRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  *v1.Environment
		caps v1.Capabilities
		code string
		path string
	}{
		{
			name: "a mode outside the set",
			obj:  environment(func(e *v1.Environment) { e.Spec.Mode = "hosted" }),
			code: "invalid_field", path: "spec.mode",
		},
		{
			name: "no isolation class",
			obj:  environment(func(e *v1.Environment) { e.Spec.Isolation = "" }),
			code: "missing_field", path: "spec.isolation",
		},
		{
			name: "an isolation class outside the set",
			obj:  environment(func(e *v1.Environment) { e.Spec.Isolation = "jail" }),
			code: "invalid_field", path: "spec.isolation",
		},
		{
			name: "no capacity",
			obj:  environment(func(e *v1.Environment) { e.Spec.Capacity = v1.Capacity{} }),
			code: "missing_field", path: "spec.capacity",
		},
		{
			name: "auto on a worker environment",
			obj:  environment(func(e *v1.Environment) { e.Spec.Capacity = v1.Capacity{Auto: true} }),
			code: "invalid_field", path: "spec.capacity",
		},
		{
			name: "a capacity quantity that is not one",
			obj:  environment(func(e *v1.Environment) { e.Spec.Capacity.Memory = "two terabytes" }),
			code: "invalid_field", path: "spec.capacity.memory",
		},
		{
			name: "a capacity quantity at zero",
			obj:  environment(func(e *v1.Environment) { e.Spec.Capacity.CPU = "0" }),
			code: "invalid_field", path: "spec.capacity.cpu",
		},
		{
			name: "a negative sandbox count",
			obj:  environment(func(e *v1.Environment) { e.Spec.Capacity.Sandboxes = -1 }),
			code: "invalid_field", path: "spec.capacity.sandboxes",
		},
		{
			name: "a scheduling mode outside the set",
			obj:  environment(func(e *v1.Environment) { e.Spec.Scheduling.Mode = "fair" }),
			code: "invalid_field", path: "spec.scheduling.mode",
		},
		{
			name: "the queued mode, which no server runs yet",
			obj:  environment(func(e *v1.Environment) { e.Spec.Scheduling.Mode = v1.SchedulingQueued }),
			code: "capability_unsupported", path: "spec.scheduling.mode",
		},
		{
			name: "a queue name that is not a DNS label",
			obj: environment(func(e *v1.Environment) {
				e.Spec.Scheduling.Queues = []string{"Roll Outs"}
			}),
			code: "invalid_field", path: "spec.scheduling.queues",
		},
		{
			name: "more queues than one environment declares",
			obj: environment(func(e *v1.Environment) {
				for i := range MaxEnvironmentQueues + 1 {
					e.Spec.Scheduling.Queues = append(e.Spec.Scheduling.Queues, "q"+string(rune('a'+i%26))+string(rune('a'+i/26)))
				}
			}),
			code: "invalid_field", path: "spec.scheduling.queues",
		},
		{
			name: "a default queue that is not one of the queues",
			obj: environment(func(e *v1.Environment) {
				e.Spec.Scheduling.Queues = []string{"rollouts"}
				e.Spec.Scheduling.DefaultQueue = "default"
			}),
			code: "invalid_field", path: "spec.scheduling.defaultQueue",
		},
		{
			name: "a negative pool size",
			obj:  environment(func(e *v1.Environment) { e.Spec.Pool.Size = -1 }),
			code: "invalid_field", path: "spec.pool.size",
		},
		{
			name: "a pool on a driver that keeps no entries",
			obj:  environment(func(e *v1.Environment) { e.Spec.Pool = v1.PoolSpec{Size: 4, Image: "example.test/img:1"} }),
			code: "capability_unsupported", path: "spec.pool.size",
		},
		{
			name: "a pool with no image",
			obj:  environment(func(e *v1.Environment) { e.Spec.Pool = v1.PoolSpec{Size: 4} }),
			caps: v1.Capabilities{Pool: true},
			code: "missing_field", path: "spec.pool.image",
		},
		{
			name: "no gateway where the driver enforces egress",
			obj:  environment(),
			caps: v1.Capabilities{Egress: []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}},
			code: "missing_field", path: "spec.gateway",
		},
		{
			name: "a gateway written as a URL",
			obj:  environment(func(e *v1.Environment) { e.Spec.Gateway = "http://gateway.example.test:3128" }),
			code: "invalid_field", path: "spec.gateway",
		},
		{
			name: "a gateway port outside the range",
			obj:  environment(func(e *v1.Environment) { e.Spec.Gateway = "gateway.example.test:70000" }),
			code: "invalid_field", path: "spec.gateway",
		},
		{
			name: "no name",
			obj:  environment(func(e *v1.Environment) { e.Metadata.Name = "" }),
			code: "missing_field", path: "metadata.name",
		},
		{
			name: "a name that is not a DNS label",
			obj:  environment(func(e *v1.Environment) { e.Metadata.Name = "EU GPU" }),
			code: "invalid_field", path: "metadata.name",
		},
		{
			name: "another apiVersion",
			obj:  environment(func(e *v1.Environment) { e.APIVersion = "cella.latere.ai/v1" }),
			code: "unsupported_version", path: "apiVersion",
		},
		{
			name: "another kind",
			obj:  environment(func(e *v1.Environment) { e.Kind = "Sandbox" }),
			code: "unsupported_kind", path: "kind",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveEnvironment(tc.obj, EnvironmentOptions{Capabilities: tc.caps})
			if err == nil {
				t.Fatalf("this was accepted, and the rule says %s at %s", tc.code, tc.path)
			}
			code, path := codeAndPath(t, err)
			if code != tc.code || path != tc.path {
				t.Errorf("the refusal is %s at %q; the rule says %s at %q", code, path, tc.code, tc.path)
			}
		})
	}
}

// TestEnvironmentAccepts holds the other half: every shape the table admits
// resolves rather than being refused with a rule meant for another field.
func TestEnvironmentAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  *v1.Environment
		caps v1.Capabilities
	}{
		{"auto on the control plane's own environment",
			environment(func(e *v1.Environment) {
				e.Spec.Mode = v1.EnvironmentInprocess
				e.Spec.Capacity = v1.Capacity{Auto: true}
			}), v1.Capabilities{}},
		{"a sandbox count alone",
			environment(func(e *v1.Environment) { e.Spec.Capacity = v1.Capacity{Sandboxes: 10} }), v1.Capabilities{}},
		{"a pool on a driver that keeps entries",
			environment(func(e *v1.Environment) { e.Spec.Pool = v1.PoolSpec{Size: 4, Image: "example.test/img:1"} }),
			v1.Capabilities{Pool: true}},
		{"a gateway host without a port",
			environment(func(e *v1.Environment) { e.Spec.Gateway = "gateway.example.test" }), v1.Capabilities{}},
		{"a gateway host with a port",
			environment(func(e *v1.Environment) { e.Spec.Gateway = "gateway.example.test:3128" }), v1.Capabilities{}},
		{"a bare address with a port",
			environment(func(e *v1.Environment) { e.Spec.Gateway = "10.0.0.1:3128" }), v1.Capabilities{}},
		{"named queues with one of them the default",
			environment(func(e *v1.Environment) {
				e.Spec.Scheduling.Queues = []string{"default", "rollouts"}
				e.Spec.Scheduling.DefaultQueue = "rollouts"
			}), v1.Capabilities{}},
		{"a workspace class",
			environment(func(e *v1.Environment) { e.Spec.WorkspaceClass = "fast" }), v1.Capabilities{}},
		{"every isolation class",
			environment(func(e *v1.Environment) { e.Spec.Isolation = v1.IsolationVM }), v1.Capabilities{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveEnvironment(tc.obj, EnvironmentOptions{Capabilities: tc.caps}); err != nil {
				t.Errorf("this shape is admitted by the table and was refused: %v", err)
			}
		})
	}
}

// TestEnvironmentImmutableFields holds the two fields an update never moves:
// the mode decides which half of the control plane serves the environment,
// and the isolation class is what every manifest on it resolved against.
func TestEnvironmentImmutableFields(t *testing.T) {
	existing, err := ResolveEnvironment(environment(), EnvironmentOptions{})
	if err != nil {
		t.Fatalf("the first apply was refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		obj  *v1.Environment
		path string
	}{
		{"the mode", environment(func(e *v1.Environment) { e.Spec.Mode = v1.EnvironmentInprocess }), "spec.mode"},
		{"the isolation class", environment(func(e *v1.Environment) { e.Spec.Isolation = v1.IsolationVM }), "spec.isolation"},
		{"the name", environment(func(e *v1.Environment) { e.Metadata.Name = "us-cpu" }), "metadata.name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveEnvironment(tc.obj, EnvironmentOptions{Existing: existing})
			if err == nil {
				t.Fatalf("this changed %s and was accepted", tc.path)
			}
			code, path := codeAndPath(t, err)
			if code != "immutable_field" || path != tc.path {
				t.Errorf("the refusal is %s at %q; the rule says immutable_field at %q", code, path, tc.path)
			}
		})
	}
	// An update that changes a mutable field is an update, not a refusal.
	grown := environment(func(e *v1.Environment) { e.Spec.Capacity.Sandboxes = 800 })
	if _, err := ResolveEnvironment(grown, EnvironmentOptions{Existing: existing}); err != nil {
		t.Errorf("growing the capacity is an update and was refused: %v", err)
	}
}

func TestDecodeEnvironment(t *testing.T) {
	const object = `{"apiVersion":"` + v1.APIVersion + `","kind":"Environment",` +
		`"metadata":{"name":"eu-gpu"},"spec":{"isolation":"container","capacity":{"sandboxes":10}}}`
	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		code        string
	}{
		{"a well-formed object", object, "application/json", ""},
		{"capacity as the word auto",
			`{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"d"},` +
				`"spec":{"isolation":"none","capacity":"auto","mode":"inprocess"}}`, "application/json", ""},
		{"capacity as another word",
			`{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"d"},` +
				`"spec":{"isolation":"none","capacity":"unlimited"}}`, "application/json", "invalid_field"},
		{"a field the kind does not have",
			`{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"d"},"spec":{"driver":"k8s"}}`,
			"application/json", "unknown_field"},
		{"two documents", object + object, "application/json", "multi_document"},
		{"another media type", object, "text/plain", "unsupported_media_type"},
		{"another apiVersion",
			`{"apiVersion":"v1","kind":"Environment","metadata":{"name":"d"}}`, "application/json", "unsupported_version"},
		{"another kind",
			`{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"d"}}`, "application/json", "unsupported_kind"},
		{"a body that is not JSON", "{", "application/json", "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := DecodeEnvironment([]byte(tc.body), tc.contentType)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("this body is well formed and was refused: %v", err)
				}
				if obj.Metadata.Name == "" {
					t.Errorf("the decoded object carries no name")
				}
				return
			}
			if err == nil {
				t.Fatalf("this body was accepted, and the rule says %s", tc.code)
			}
			code, _ := codeAndPath(t, err)
			if code != tc.code {
				t.Errorf("the refusal is %s; the rule says %s", code, tc.code)
			}
		})
	}
}

// TestDecodeEnvironmentDiscardsClientStatus holds that a caller cannot write
// a phase: the status is the control plane's and a body's is dropped.
func TestDecodeEnvironmentDiscardsClientStatus(t *testing.T) {
	body := `{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"d"},` +
		`"spec":{"isolation":"none","capacity":{"sandboxes":1}},"status":{"phase":"Ready","workers":9}}`
	obj, err := DecodeEnvironment([]byte(body), "application/json")
	if err != nil {
		t.Fatalf("the body was refused: %v", err)
	}
	if obj.Status.Phase != "" || obj.Status.Workers != 0 {
		t.Errorf("the client's status survived the decode: %+v", obj.Status)
	}
}

// TestCapacityRoundTrips holds that a read returns what an apply wrote, in
// both of the field's two forms.
func TestCapacityRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   v1.Capacity
		want string
	}{
		{"the word auto", v1.Capacity{Auto: true}, `"auto"`},
		{"an object", v1.Capacity{CPU: "512", Sandboxes: 400}, `{"cpu":"512","sandboxes":400}`},
		{"nothing declared", v1.Capacity{}, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("the capacity did not marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("the capacity wrote %s, not %s", raw, tc.want)
			}
			var back v1.Capacity
			if err = json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("the capacity did not unmarshal: %v", err)
			}
			if back != tc.in {
				t.Errorf("the capacity read back as %+v, not %+v", back, tc.in)
			}
		})
	}
	var c v1.Capacity
	if err := json.Unmarshal([]byte(`"unlimited"`), &c); err == nil {
		t.Errorf("the word unlimited was accepted as a capacity")
	}
	if err := json.Unmarshal([]byte(`4`), &c); err == nil {
		t.Errorf("a number was accepted as a capacity")
	}
}
