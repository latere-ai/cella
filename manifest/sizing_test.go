// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// sizedOptions resolves against an environment that sizes memory, with the
// installation's small start and ceiling set.
func sizedOptions(env v1.Environment) Options {
	o := environmentOptions(env)
	o.Defaults.StartMemory, o.Defaults.MemoryCeiling = "128Mi", "1Gi"
	return o
}

// TestASandboxThatNamesNoMemoryStartsSmallAndGrows is the installation's
// default as the caller sees it: what the manifest names is kept, and only
// a manifest that names no memory size at all takes the small start.
func TestASandboxThatNamesNoMemoryStartsSmallAndGrows(t *testing.T) {
	memory := autosizing(v1.Autosize{Memory: true})
	for _, tc := range []struct {
		name       string
		env        v1.Environment
		resources  v1.Resources
		start, max v1.Quantity
	}{
		{"nothing named takes both defaults", memory, v1.Resources{}, "128Mi", "1Gi"},
		{"cpu and disk named, memory is still defaulted", memory, v1.Resources{CPU: "2", Disk: "10Gi"}, "128Mi", "1Gi"},
		{"a memory named stays fixed", memory, v1.Resources{Memory: "2Gi"}, "2Gi", ""},
		{"a memory named under a ceiling keeps both", memory, v1.Resources{Memory: "512Mi", Max: v1.ResourceMax{Memory: "4Gi"}}, "512Mi", "4Gi"},
		{"a ceiling named alone starts at the default", memory, v1.Resources{Max: v1.ResourceMax{Memory: "4Gi"}}, "128Mi", "4Gi"},
		{"a ceiling named below the default start starts at the ceiling", memory, v1.Resources{Max: v1.ResourceMax{Memory: "64Mi"}}, "64Mi", "64Mi"},
		{"an environment that holds memory fixed is left alone", container("default"), v1.Resources{}, "", ""},
		{"an environment that sizes only cpu is left alone", autosizing(v1.Autosize{CPU: true}), v1.Resources{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			obj.Spec.Resources = tc.resources
			got := resolve(t, obj, sizedOptions(tc.env)).Sandbox.Spec.Resources
			if got.Memory != tc.start || got.Max.Memory != tc.max {
				t.Fatalf("memory %q and ceiling %q, want %q and %q", got.Memory, got.Max.Memory, tc.start, tc.max)
			}
		})
	}
}

// TestNoSizingDefaultsResolvesAsBefore: an installation that sets neither
// amount resolves a sandbox that names no memory exactly as it did.
func TestNoSizingDefaultsResolvesAsBefore(t *testing.T) {
	got := resolve(t, sandbox(), environmentOptions(autosizing(v1.Autosize{Memory: true}))).Sandbox.Spec.Resources
	if got.Memory != "" || got.Max.Memory != "" {
		t.Fatalf("memory %q and ceiling %q, want neither", got.Memory, got.Max.Memory)
	}
}

// TestAnUpdateKeepsTheMemoryTheSandboxHolds: an update is not defaulted
// again. A body that leaves memory out keeps the sandbox's start and
// ceiling, also after the installation changes its defaults, and a sandbox
// that was created at a fixed size is not given a ceiling.
func TestAnUpdateKeepsTheMemoryTheSandboxHolds(t *testing.T) {
	env := autosizing(v1.Autosize{Memory: true})
	created := resolve(t, sandbox(), sizedOptions(env)).Sandbox
	changed := sizedOptions(env)
	changed.Defaults.StartMemory, changed.Defaults.MemoryCeiling = "256Mi", "2Gi"
	changed.Existing = &created
	body := sandbox()
	body.Spec.Lifecycle.TTL = "2h"
	got := resolve(t, body, changed).Sandbox.Spec.Resources
	if got.Memory != "128Mi" || got.Max.Memory != "1Gi" {
		t.Fatalf("memory %q and ceiling %q, want the 128Mi and 1Gi it holds", got.Memory, got.Max.Memory)
	}

	named := sandbox()
	named.Spec.Resources.Max.Memory = "512Mi"
	if got := resolve(t, named, changed).Sandbox.Spec.Resources; got.Memory != "128Mi" || got.Max.Memory != "512Mi" {
		t.Fatalf("memory %q and ceiling %q, want the start it holds under the ceiling named", got.Memory, got.Max.Memory)
	}

	fixed := sandbox()
	fixed.Spec.Resources.Memory = "2Gi"
	held := resolve(t, fixed, sizedOptions(env)).Sandbox
	changed.Existing = &held
	if got := resolve(t, fixed, changed).Sandbox.Spec.Resources; got.Memory != "2Gi" || got.Max.Memory != "" {
		t.Fatalf("memory %q and ceiling %q, want the fixed 2Gi alone", got.Memory, got.Max.Memory)
	}
}

// TestTheServersCeilingHoldsTheDefaults: a default above the server's memory
// ceiling refuses the sandbox that took it, at the field the default filled,
// and one a caller named is refused as it always was.
func TestTheServersCeilingHoldsTheDefaults(t *testing.T) {
	o := sizedOptions(autosizing(v1.Autosize{Memory: true}))
	o.Ceilings.Memory = "512Mi"
	if got := refusal(t, sandbox(), o); got.Code != "ceiling_exceeded" || got.Path != "spec.resources.max.memory" {
		t.Fatalf("got %q at %q, want ceiling_exceeded at spec.resources.max.memory", got.Code, got.Path)
	}
	o.Ceilings.Memory = "1Gi"
	resolve(t, sandbox(), o)
}

// TestAChildTakesNoMoreThanItsParentHolds: a child that names no memory is
// bounded by its parent instead of being refused for the installation's
// ceiling.
func TestAChildTakesNoMoreThanItsParentHolds(t *testing.T) {
	parent := parentSandbox()
	parent.Spec.Resources.Memory = "512Mi"
	o := spawnOptions(parent)
	env := meshEnvironment("default")
	env.Status.Capabilities.Autosize.Memory = true
	o.Lookup = FixedEnvironment(env)
	o.Defaults.StartMemory, o.Defaults.MemoryCeiling = "128Mi", "1Gi"
	child := childSandbox()
	child.Spec.Resources.Memory = ""
	got := resolve(t, child, o).Sandbox.Spec.Resources
	if got.Memory != "128Mi" || got.Max.Memory != "512Mi" {
		t.Fatalf("memory %q and ceiling %q, want 128Mi under the parent's 512Mi", got.Memory, got.Max.Memory)
	}
}
