// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"slices"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestWorkloadCannotWiden is spec 013's control plane model, row 2: whatever
// a sandbox's own token tries against its boundary, the refusal names the
// control that caught it. Each case is the narrow slice of an existing rule
// (TestNarrowingIsForWorkloads, TestAWorkloadCannotMountASecret, TestBoundaryCheck,
// TestParentCannotBeNarrowedBelowAChild) read together as the one list the
// acceptance criterion names.
func TestWorkloadCannotWiden(t *testing.T) {
	t.Run("addingAHost", func(t *testing.T) {
		existing := resolve(t, withEgress(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}),
			enforcingOptions()).Sandbox
		o := enforcingOptions()
		o.Existing = &existing
		o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
		next := withEgress(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com", "b.example.com"}})
		err := refusal(t, next, o)
		if err.Code != "boundary_widened" || !slices.Contains(err.Paths, pathAllowedHosts) {
			t.Fatalf("adding a host answered %+v", err)
		}
	})

	t.Run("mountingASecret", func(t *testing.T) {
		github := scoped(t, "github", "sec_1", "api.github.com")
		openai := scoped(t, "openai", "sec_2", "api.openai.com")
		existing := resolve(t, func() v1.Sandbox {
			obj := sandbox()
			obj.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN")}
			return obj
		}(), withSecrets(containerOptions(), github)).Sandbox

		both := sandbox()
		both.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN"), mounted("openai", "OPENAI_API_KEY")}
		o := withSecrets(containerOptions(), github, openai)
		o.Existing = &existing
		o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
		err := refusal(t, both, o)
		if err.Code != "boundary_widened" || !slices.Contains(err.Paths, pathSecrets) {
			t.Fatalf("mounting a secret answered %+v", err)
		}
	})

	t.Run("spawningPastItsBudget", func(t *testing.T) {
		parent := parentSandbox()
		o := spawnOptions(parent)
		next := childSandbox()
		// The parent's budget of 4 has used 1, leaving 3; a child asking
		// for 3 of its own would let the tree spawn more than the parent
		// was granted, so the ceiling is the remainder less one.
		next.Spec.Mesh.Spawn.Budget = 3
		err := refusal(t, next, o)
		if err.Code != "boundary_exceeded" || !slices.Contains(err.Paths, pathSpawnBudget) {
			t.Fatalf("spawning past the budget answered %+v", err)
		}
	})

	// A parent's update and a child's spawn are the same containment read
	// the other way round, and it holds for whoever submits the update: a
	// workload has no route to its own parent at all (TestWorkloadIsLeastPrivileged),
	// so the shape this proves is the structural one Controller.Update
	// enforces regardless of actor, which is what refuses the request a
	// workload's own apply would take if it ever reached this far.
	t.Run("narrowingItsParentBelowItself", func(t *testing.T) {
		parent := parentSandbox()
		live := childSandbox()
		live.Status.ID = "sbx_child"
		live.Spec.Resources.CPU = "1"

		shrunk := parent
		shrunk.Spec.Resources.CPU = "0.5"
		err := DescendantOutsideBoundary(&shrunk, &live, fixedNow)
		var known *Error
		if !errors.As(err, &known) || known.Code != "boundary_exceeded" || !slices.Contains(known.Paths, pathResources+".cpu") {
			t.Fatalf("narrowing the parent below its live child answered %v", err)
		}
	})
}
