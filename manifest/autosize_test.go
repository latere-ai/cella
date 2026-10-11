// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// autosizing is an environment whose driver sizes the named resources inside
// a ceiling and changes nothing else after create.
func autosizing(flags v1.Autosize) v1.Environment {
	env := container("default")
	env.Status.Capabilities = v1.Capabilities{Autosize: flags}
	return env
}

// ceilinged is a manifest with every amount named and the ceilings a case
// sets over them.
func ceilinged(max v1.ResourceMax) v1.Sandbox {
	obj := sandbox()
	obj.Spec.Resources = v1.Resources{CPU: "500m", Memory: "1Gi", Disk: "5Gi", Max: max}
	return obj
}

// TestResolveAutosizeFields is spec 078's field table as refusals: a ceiling
// below the amount it bounds, above the server's own ceiling, or on a
// resource the environment's driver holds fixed is refused at the one field,
// and so is an update that puts a ceiling below what the sandbox holds.
func TestResolveAutosizeFields(t *testing.T) {
	all := v1.Autosize{CPU: true, Memory: true, Disk: true}
	for _, tc := range []struct {
		name     string
		max      v1.ResourceMax
		flags    v1.Autosize
		ceilings Ceilings
		code     string
		path     string
	}{
		{name: "every ceiling above its amount", max: v1.ResourceMax{CPU: "2", Memory: "6Gi", Disk: "50Gi"}, flags: all},
		{name: "a ceiling equal to its amount", max: v1.ResourceMax{Memory: "1Gi"}, flags: all},
		{name: "one resource with a ceiling and two without", max: v1.ResourceMax{CPU: "2"}, flags: v1.Autosize{CPU: true}},
		{name: "a cpu ceiling that is no quantity", max: v1.ResourceMax{CPU: "two"}, flags: all, code: "invalid_field", path: "spec.resources.max.cpu"},
		{name: "a memory ceiling of zero", max: v1.ResourceMax{Memory: "0"}, flags: all, code: "invalid_field", path: "spec.resources.max.memory"},
		{name: "a cpu ceiling below the amount", max: v1.ResourceMax{CPU: "250m"}, flags: all, code: "invalid_field", path: "spec.resources.max.cpu"},
		{name: "a memory ceiling below the amount", max: v1.ResourceMax{Memory: "512Mi"}, flags: all, code: "invalid_field", path: "spec.resources.max.memory"},
		{name: "a disk ceiling below the amount", max: v1.ResourceMax{Disk: "1Gi"}, flags: all, code: "invalid_field", path: "spec.resources.max.disk"},
		{name: "a cpu ceiling above the server's", max: v1.ResourceMax{CPU: "8"}, flags: all, ceilings: Ceilings{CPU: "4"}, code: "ceiling_exceeded", path: "spec.resources.max.cpu"},
		{name: "a memory ceiling above the server's", max: v1.ResourceMax{Memory: "16Gi"}, flags: all, ceilings: Ceilings{Memory: "8Gi"}, code: "ceiling_exceeded", path: "spec.resources.max.memory"},
		{name: "a disk ceiling above the server's", max: v1.ResourceMax{Disk: "50Gi"}, flags: all, ceilings: Ceilings{Disk: "20Gi"}, code: "ceiling_exceeded", path: "spec.resources.max.disk"},
		{name: "a cpu ceiling on a driver that sizes no cpu", max: v1.ResourceMax{CPU: "2"}, flags: v1.Autosize{Memory: true}, code: "capability_unsupported", path: "spec.resources.max.cpu"},
		{name: "a memory ceiling on a driver that sizes no memory", max: v1.ResourceMax{Memory: "2Gi"}, flags: v1.Autosize{CPU: true}, code: "capability_unsupported", path: "spec.resources.max.memory"},
		{name: "a disk ceiling on a driver that sizes no disk", max: v1.ResourceMax{CPU: "2", Memory: "2Gi", Disk: "10Gi"}, flags: v1.Autosize{CPU: true, Memory: true}, code: "capability_unsupported", path: "spec.resources.max.disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := environmentOptions(autosizing(tc.flags))
			o.Ceilings = tc.ceilings
			if tc.code == "" {
				got := resolve(t, ceilinged(tc.max), o).Sandbox
				if got.Spec.Resources.Max != tc.max {
					t.Fatalf("the resolved ceiling is %+v, want %+v", got.Spec.Resources.Max, tc.max)
				}
				return
			}
			if got := refusal(t, ceilinged(tc.max), o); got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("got %q at %q, want %q at %q", got.Code, got.Path, tc.code, tc.path)
			}
		})
	}
}

// TestACeilingIsHeldToTheDefaultedAmount: the floor is read after
// defaulting, so a ceiling below an amount the manifest left to the server
// is refused, and a server default is never reported as the caller's fault.
func TestACeilingIsHeldToTheDefaultedAmount(t *testing.T) {
	o := environmentOptions(autosizing(v1.Autosize{Memory: true}))
	o.Defaults.Memory = "2Gi"
	obj := sandbox()
	obj.Spec.Resources.Max.Memory = "1Gi"
	if got := refusal(t, obj, o); got.Code != "invalid_field" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("got %q at %q, want invalid_field at spec.resources.max.memory", got.Code, got.Path)
	}
	obj.Spec.Resources.Max.Memory = "4Gi"
	if got := resolve(t, obj, o).Sandbox.Spec.Resources; got.Memory != "2Gi" || got.Max.Memory != "4Gi" {
		t.Fatalf("resources = %+v, want the default under the ceiling", got)
	}
}

// TestNoCeilingResolvesAsBefore: a manifest that names no ceiling resolves
// to bytes that never mention one, on an environment that sizes and on one
// that does not.
func TestNoCeilingResolvesAsBefore(t *testing.T) {
	for _, env := range []v1.Environment{container("default"), autosizing(v1.Autosize{CPU: true, Memory: true, Disk: true})} {
		obj := sandbox()
		obj.Spec.Resources = v1.Resources{CPU: "1", Memory: "1Gi", Disk: "5Gi"}
		got := resolve(t, obj, environmentOptions(env)).Sandbox
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, word := range []string{`"max"`, `"usage"`, `"moves"`} {
			if strings.Contains(string(encoded), word) {
				t.Errorf("a manifest with no ceiling resolved to %s, which carries %s", encoded, word)
			}
		}
		if !strings.Contains(string(encoded), `"resources":{"cpu":"1","memory":"1Gi","disk":"5Gi"}`) {
			t.Errorf("the resolved resources are not the three amounts alone: %s", encoded)
		}
	}
}

// TestCeilingOnUpdate: a ceiling is changed under its resource's Autosize
// flag and not under Resize; it may rise, and memory's and the workspace's
// may not fall below what the sandbox holds now, removed or lowered, while
// cpu's follows the field down to the amount it bounds.
func TestCeilingOnUpdate(t *testing.T) {
	all := v1.Autosize{CPU: true, Memory: true, Disk: true}
	o := environmentOptions(autosizing(all))
	existing := resolve(t, ceilinged(v1.ResourceMax{CPU: "2", Memory: "6Gi", Disk: "50Gi"}), o).Sandbox
	existing.Status.Resources = v1.Size{CPU: "2", Memory: "3Gi", Disk: "20Gi"}
	o.Existing = &existing
	change := func(mutate func(*v1.Resources)) v1.Sandbox {
		next := existing
		next.Status = v1.SandboxStatus{}
		mutate(&next.Spec.Resources)
		return next
	}
	// Raising every ceiling needs no Resize, which this environment lacks.
	resolve(t, change(func(r *v1.Resources) { r.Max = v1.ResourceMax{CPU: "4", Memory: "8Gi", Disk: "100Gi"} }), o)
	// A ceiling exactly at the size held is the lowest that is accepted.
	resolve(t, change(func(r *v1.Resources) { r.Max.Memory, r.Max.Disk = "3Gi", "20Gi" }), o)
	// cpu follows the field, down to the amount and away altogether.
	resolve(t, change(func(r *v1.Resources) { r.Max.CPU = "500m" }), o)
	resolve(t, change(func(r *v1.Resources) { r.Max.CPU = "" }), o)
	for _, tc := range []struct {
		name   string
		mutate func(*v1.Resources)
		code   string
		path   string
	}{
		{"memory's ceiling below the size held", func(r *v1.Resources) { r.Max.Memory = "2Gi" }, "ceiling_exceeded", "spec.resources.max.memory"},
		{"memory's ceiling removed above the amount", func(r *v1.Resources) { r.Max.Memory = "" }, "ceiling_exceeded", "spec.resources.max.memory"},
		{"the workspace's ceiling below the size held", func(r *v1.Resources) { r.Max.Disk = "10Gi" }, "ceiling_exceeded", "spec.resources.max.disk"},
		{"the workspace's ceiling removed above the amount", func(r *v1.Resources) { r.Max.Disk = "" }, "ceiling_exceeded", "spec.resources.max.disk"},
		{"an amount changed without Resize", func(r *v1.Resources) { r.Memory = "2Gi" }, "capability_unsupported", "spec.resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusal(t, change(tc.mutate), o); got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("got %q at %q, want %q at %q", got.Code, got.Path, tc.code, tc.path)
			}
		})
	}
	// A sandbox still at the amount it started at gives its ceiling up.
	existing.Status.Resources = v1.Size{CPU: "2", Memory: "1Gi", Disk: "5Gi"}
	resolve(t, change(func(r *v1.Resources) { r.Max = v1.ResourceMax{} }), o)
	// A size the control plane cannot read bounds nothing.
	existing.Status.Resources = v1.Size{Memory: "plenty"}
	resolve(t, change(func(r *v1.Resources) { r.Max.Memory = "1Gi" }), o)
	// A ceiling added to a sandbox that had none needs its flag.
	fixed := environmentOptions(container("default"))
	plain := resolve(t, ceilinged(v1.ResourceMax{}), fixed).Sandbox
	fixed.Existing = &plain
	next := plain
	next.Spec.Resources.Max.Memory = "4Gi"
	if got := refusal(t, next, fixed); got.Code != "capability_unsupported" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("got %q at %q, want capability_unsupported at spec.resources.max.memory", got.Code, got.Path)
	}
}

// TestResizeChangesNoWorkspace: where a driver declares Resize, cpu and
// memory change on an update and the workspace's size does not, because no
// driver grows a claim through that capability.
func TestResizeChangesNoWorkspace(t *testing.T) {
	o := containerOptions()
	existing := resolve(t, ceilinged(v1.ResourceMax{}), o).Sandbox
	o.Existing = &existing
	next := existing
	next.Spec.Resources.CPU, next.Spec.Resources.Memory = "1", "2Gi"
	resolve(t, next, o)
	next.Spec.Resources.Disk = "10Gi"
	if got := refusal(t, next, o); got.Code != "capability_unsupported" || got.Path != "spec.resources.disk" {
		t.Fatalf("got %q at %q, want capability_unsupported at spec.resources.disk", got.Code, got.Path)
	}
}

// TestNativeRefusesACeiling: an environment that confines nothing records
// the amounts with a warning and refuses a ceiling, since nothing there
// would grow toward it.
func TestNativeRefusesACeiling(t *testing.T) {
	obj := sandbox()
	obj.Spec.Resources = v1.Resources{Memory: "1Gi", Max: v1.ResourceMax{Memory: "2Gi"}}
	if got := refusal(t, obj, nativeOptions()); got.Code != "capability_unsupported" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("got %q at %q, want capability_unsupported at spec.resources.max.memory", got.Code, got.Path)
	}
}

// TestAdmissionMayAddACeiling: a ceiling the admission step writes onto a
// manifest that named none is kept, and is held to the amount, the server's
// ceiling and the capability exactly as a caller's own would be.
func TestAdmissionMayAddACeiling(t *testing.T) {
	admit := func(max v1.ResourceMax) AdmitFunc {
		return func(_ context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error) {
			if !req.Environment.Status.Capabilities.Autosize.Memory {
				return in, nil, nil
			}
			out := *in
			out.Spec.Resources.Memory = "256Mi"
			out.Spec.Resources.Max = max
			return &out, nil, nil
		}
	}
	o := environmentOptions(autosizing(v1.Autosize{CPU: true, Memory: true}))
	o.Ceilings = Ceilings{Memory: "8Gi"}
	o.Admit = admit(v1.ResourceMax{CPU: "2", Memory: "4Gi"})
	got := resolve(t, sandbox(), o).Sandbox.Spec.Resources
	if got.Memory != "256Mi" || got.Max != (v1.ResourceMax{CPU: "2", Memory: "4Gi"}) {
		t.Fatalf("resources = %+v, want the step's amount and ceiling", got)
	}
	o.Defaults.CPU = "4"
	if got := refusal(t, sandbox(), o); got.Code != "invalid_field" || got.Path != "spec.resources.max.cpu" {
		t.Fatalf("a ceiling the step set below the amount: got %q at %q", got.Code, got.Path)
	}
	o.Defaults.CPU = ""
	o.Admit = admit(v1.ResourceMax{Memory: "16Gi"})
	if got := refusal(t, sandbox(), o); got.Code != "ceiling_exceeded" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("a ceiling the step set above the server's: got %q at %q", got.Code, got.Path)
	}
	o.Admit = admit(v1.ResourceMax{Memory: "4Gi", Disk: "50Gi"})
	if got := refusal(t, sandbox(), o); got.Code != "capability_unsupported" || got.Path != "spec.resources.max.disk" {
		t.Fatalf("a ceiling the step set on a fixed resource: got %q at %q", got.Code, got.Path)
	}
	// The step read the capability from its request: where the driver
	// sizes no memory it set nothing, and the manifest resolves fixed.
	fixed := environmentOptions(container("default"))
	fixed.Admit = admit(v1.ResourceMax{Memory: "4Gi"})
	if got := resolve(t, sandbox(), fixed).Sandbox.Spec.Resources.Max; got != (v1.ResourceMax{}) {
		t.Fatalf("the step set %+v where the driver sizes nothing", got)
	}
}

// TestAChildsCeilingIsInsideItsParents: boundary rule 5 reads a parent's
// ceiling as the most it may hold, so a child may start and grow up to it,
// and a parent with no ceiling bounds its child at its own amounts.
func TestAChildsCeilingIsInsideItsParents(t *testing.T) {
	flags := func(o *Options) {
		env := meshEnvironment("default")
		env.Status.Capabilities.Autosize = v1.Autosize{CPU: true, Memory: true, Disk: true}
		o.Lookup = WithSecrets(FixedEnvironment(env), nil)
	}
	parent := parentSandbox()
	parent.Spec.Secrets = nil
	parent.Spec.Resources.Max = v1.ResourceMax{Memory: "8Gi"}
	o := spawnOptions(parent)
	flags(&o)
	child := childSandbox()
	child.Spec.Resources.Memory = "6Gi"
	child.Spec.Resources.Max = v1.ResourceMax{Memory: "8Gi"}
	resolve(t, child, o)
	child.Spec.Resources.Max.Memory = "16Gi"
	if got := refusal(t, child, o); got.Code != "boundary_exceeded" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("got %q at %q, want boundary_exceeded at spec.resources.max.memory", got.Code, got.Path)
	}
	// The parent's cpu has no ceiling, so its amount bounds the child's.
	child.Spec.Resources.Max = v1.ResourceMax{CPU: "4"}
	child.Spec.Resources.Memory = "2Gi"
	if got := refusal(t, child, o); got.Code != "boundary_exceeded" || got.Path != "spec.resources.max.cpu" {
		t.Fatalf("got %q at %q, want boundary_exceeded at spec.resources.max.cpu", got.Code, got.Path)
	}
}

// TestAPoolTakesNoCeiling: an environment's pool is one fixed shape.
func TestAPoolTakesNoCeiling(t *testing.T) {
	obj := environment(func(e *v1.Environment) {
		e.Spec.Pool.Resources.Max.Memory = "4Gi"
	})
	_, err := ResolveEnvironment(obj, EnvironmentOptions{Capabilities: v1.Capabilities{Pool: true}})
	if code, path := codeAndPath(t, err); code != "invalid_field" || path != "spec.pool.resources.max" {
		t.Fatalf("got %q at %q, want invalid_field at spec.pool.resources.max", code, path)
	}
}

// TestCapabilitiesCompareTheirAutosize: two declarations that differ in one
// autosize flag are different, and And keeps a flag only where both hold it.
func TestCapabilitiesCompareTheirAutosize(t *testing.T) {
	a := v1.Capabilities{Autosize: v1.Autosize{CPU: true, Memory: true}}
	b := v1.Capabilities{Autosize: v1.Autosize{CPU: true}}
	if a.Equal(b) || !a.Equal(a) {
		t.Fatal("Equal does not read the autosize flags")
	}
	if got := a.Autosize.And(v1.Autosize{Memory: true, Disk: true}); got != (v1.Autosize{Memory: true}) {
		t.Fatalf("And = %+v, want memory alone", got)
	}
	encoded, err := json.Marshal(v1.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"autosize":{"cpu":false,"memory":false,"disk":false}`) {
		t.Fatalf("a driver that sizes nothing declares %s, without the three flags", encoded)
	}
}
