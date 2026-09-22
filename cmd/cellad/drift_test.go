// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestTheDriftMovesOneDefault: the seam moves the named field from its
// literal default to one before the admission step reads it, leaves a value
// the manifest already set, touches no other field, and is the admission step
// unchanged when no field is named.
func TestTheDriftMovesOneDefault(t *testing.T) {
	var seen v1.Spawn
	refusal := errors.New("refused by the step")
	step := func(_ context.Context, in *v1.Sandbox, _ manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = in.Spec.Mesh.Spawn
		if in.Metadata.Name == "refused" {
			return nil, nil, refusal
		}
		return in, []string{"from the step"}, nil
	}
	sandbox := func(budget, depth int) *v1.Sandbox {
		obj := &v1.Sandbox{Metadata: v1.Metadata{Name: "drifted"}}
		obj.Spec.Mesh.Spawn = v1.Spawn{Budget: budget, Depth: depth}
		return obj
	}

	for _, tc := range []struct {
		name          string
		field         string
		budget, depth int
		want          v1.Spawn
	}{
		{"no field is the step itself", "", 0, 0, v1.Spawn{}},
		{"the budget at its default", config.DriftSpawnBudget, 0, 0, v1.Spawn{Budget: 1}},
		{"the depth at its default", config.DriftSpawnDepth, 0, 0, v1.Spawn{Depth: 1}},
		{"a budget the manifest set", config.DriftSpawnBudget, 3, 0, v1.Spawn{Budget: 3}},
		{"a depth the manifest set", config.DriftSpawnDepth, 2, 4, v1.Spawn{Budget: 2, Depth: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = v1.Spawn{Budget: -1}
			out, warnings, err := driftDefault(tc.field, step)(t.Context(), sandbox(tc.budget, tc.depth), manifest.AdmitRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if seen != tc.want || out.Spec.Mesh.Spawn != tc.want {
				t.Fatalf("the step saw %+v and returned %+v, want %+v", seen, out.Spec.Mesh.Spawn, tc.want)
			}
			if len(warnings) != 1 {
				t.Fatalf("the step's warnings did not come back: %v", warnings)
			}
		})
	}

	// No admission step configured: the seam alone is stage 3.
	if driftDefault("", nil) != nil {
		t.Error("with no field and no step the resolve gained a step")
	}
	out, warnings, err := driftDefault(config.DriftSpawnBudget, nil)(t.Context(), sandbox(0, 0), manifest.AdmitRequest{})
	if err != nil || len(warnings) != 0 || out.Spec.Mesh.Spawn != (v1.Spawn{Budget: 1}) {
		t.Fatalf("with no step: %+v %v %v", out.Spec.Mesh.Spawn, warnings, err)
	}

	// The step's refusal is the resolve's.
	refused := sandbox(0, 0)
	refused.Metadata.Name = "refused"
	if _, _, err := driftDefault(config.DriftSpawnDepth, step)(t.Context(), refused, manifest.AdmitRequest{}); !errors.Is(err, refusal) {
		t.Fatalf("the step's refusal became %v", err)
	}
}
