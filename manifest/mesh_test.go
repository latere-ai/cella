// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// meshEnvironment is a container environment whose driver connects peers.
func meshEnvironment(name string) v1.Environment {
	env := container(name)
	env.Status.Capabilities.Mesh = true
	return env
}

// fixedNow is the instant every boundary fixture is resolved at, so a rule
// that reads a deadline reads the same one on every run.
var fixedNow = time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

// parentSandbox is a root with room on every boundary axis: a budget of four
// with one child already made, two generations, an allow list of two hosts,
// two mounted secrets, a compute envelope and an expiry a day out.
func parentSandbox() v1.Sandbox {
	obj := sandbox()
	obj.Metadata.Name = "root"
	obj.Spec.Environment = "default"
	obj.Spec.Image = fixtureImage
	obj.Spec.Resources = v1.Resources{CPU: "2", Memory: "4Gi", Disk: "10Gi"}
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com", "*.b.example.com"}}
	obj.Spec.Secrets = []v1.SecretMount{{Name: "one", Env: "ONE"}, {Name: "two", Env: "TWO"}}
	obj.Spec.Mesh = v1.Mesh{Enabled: true, Spawn: v1.Spawn{Budget: 4, Depth: 2}}
	obj.Status = v1.SandboxStatus{
		ID: "sbx_root", Owner: "alice", Environment: "default", Root: "sbx_root",
		Mesh:      "msh_01J9",
		Spawn:     v1.SpawnStatus{Budget: 4, Used: 1, Depth: 2},
		ExpiresAt: fixedNow.Add(24 * time.Hour),
	}
	return obj
}

// childSandbox is a manifest that conforms to parentSandbox on every rule.
func childSandbox() v1.Sandbox {
	obj := sandbox()
	obj.Metadata.Name = "child"
	obj.Spec.Environment = "default"
	obj.Spec.Image = fixtureImage
	obj.Spec.Resources = v1.Resources{CPU: "1", Memory: "2Gi", Disk: "5Gi"}
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}
	obj.Spec.Lifecycle.TTL = "1h"
	return obj
}

// spawnOptions resolves a child against one parent, with the secrets the
// parent mounts answerable and the clock fixed.
func spawnOptions(parent v1.Sandbox) Options {
	o := environmentOptions(meshEnvironment("default"))
	o.Actor = Actor{Subject: "sandbox:sbx_root", Workload: true}
	o.Parent = &parent
	o.Now = func() time.Time { return fixedNow }
	o.Lookup = WithSecrets(o.Lookup, func(_ context.Context, name string) (*v1.Secret, error) {
		return &v1.Secret{
			APIVersion: v1.APIVersion, Kind: "Secret",
			Metadata: v1.Metadata{Name: name},
			Spec:     v1.SecretSpec{Scope: v1.SecretScope{Hosts: []string{name + ".example.com"}}},
			Status:   v1.SecretStatus{ID: "sec_" + name, Owner: "alice"},
		}, nil
	})
	return o
}

// TestBoundaryCheckAdmitsAConformingChild is the positive half of stage 6:
// every rule holds, so the child resolves and carries no boundary error.
func TestBoundaryCheckAdmitsAConformingChild(t *testing.T) {
	parent := parentSandbox()
	got := resolve(t, childSandbox(), spawnOptions(parent))
	if got.Sandbox.Spec.Lifecycle.TTL != "1h" {
		t.Fatalf("ttl = %q, want the child's own 1h", got.Sandbox.Spec.Lifecycle.TTL)
	}
}

// TestBoundaryCheck walks each rule of stage 6 that has a field, widening one
// at a time, and holds every refusal to boundary_exceeded naming its path.
func TestBoundaryCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		with func(*v1.Sandbox)
	}{
		{"mode wider than the parent's", pathEgressMode, func(s *v1.Sandbox) {
			s.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen}
		}},
		{"a host the parent does not reach", pathAllowedHosts, func(s *v1.Sandbox) {
			s.Spec.Network.Egress.AllowedHosts = []string{"a.example.com", "c.example.com"}
		}},
		{"a wildcard over a host the parent named", pathAllowedHosts, func(s *v1.Sandbox) {
			s.Spec.Network.Egress.AllowedHosts = []string{"*.example.com"}
		}},
		{"a secret the parent does not mount", pathSecretAt(0, "name"), func(s *v1.Sandbox) {
			s.Spec.Secrets = []v1.SecretMount{{Name: "three", Env: "THREE"}}
		}},
		{"more cpu than the parent", pathResources + ".cpu", func(s *v1.Sandbox) {
			s.Spec.Resources.CPU = "4"
		}},
		{"more memory than the parent", pathResources + ".memory", func(s *v1.Sandbox) {
			s.Spec.Resources.Memory = "8Gi"
		}},
		{"more disk than the parent", pathResources + ".disk", func(s *v1.Sandbox) {
			s.Spec.Resources.Disk = "20Gi"
		}},
		{"a life past the parent's expiry", pathTTL, func(s *v1.Sandbox) {
			s.Spec.Lifecycle.TTL = "48h"
		}},
		{"a life that never ends", pathTTL, func(s *v1.Sandbox) {
			s.Spec.Lifecycle.TTL = v1.DurationNever
			s.Spec.Lifecycle.AutoStop = v1.DurationNever
		}},
		{"more budget than the parent's remainder", pathSpawnBudget, func(s *v1.Sandbox) {
			s.Spec.Mesh.Spawn.Budget = 3
		}},
		{"more depth than the parent's remainder", pathSpawnDepth, func(s *v1.Sandbox) {
			s.Spec.Mesh.Spawn.Depth = 2
		}},
		{"another environment", pathEnvironment, func(s *v1.Sandbox) {
			s.Spec.Environment = "other"
		}},
		{"a mesh of its own", pathMeshEnabled, func(s *v1.Sandbox) {
			s.Spec.Mesh.Enabled = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := parentSandbox()
			o := spawnOptions(parent)
			if tc.path == pathEnvironment {
				// The other environment must exist for the rule to be the
				// one that refuses, rather than the lookup.
				o.Lookup = WithSecrets(FixedEnvironment(meshEnvironment("other")), nil)
			}
			obj := childSandbox()
			tc.with(&obj)
			got := refusal(t, obj, o)
			if got.Code != "boundary_exceeded" || !slices.Contains(got.Paths, tc.path) {
				t.Fatalf("got %s at %v, want boundary_exceeded at %s", got.Code, got.Paths, tc.path)
			}
		})
	}
}

// TestBoundaryNamesEveryPath is the one-round rule: a child that widened
// three fields learns all three from one refusal.
func TestBoundaryNamesEveryPath(t *testing.T) {
	parent := parentSandbox()
	obj := childSandbox()
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen}
	obj.Spec.Resources.CPU = "4"
	obj.Spec.Mesh.Spawn.Budget = 3
	got := refusal(t, obj, spawnOptions(parent))
	for _, path := range []string{pathEgressMode, pathResources + ".cpu", pathSpawnBudget} {
		if !slices.Contains(got.Paths, path) {
			t.Errorf("paths = %v, want %s among them", got.Paths, path)
		}
	}
}

// TestDeniedHostsAreInheritedByTheChild is rule 2 on the open mode: the child
// keeps every host its parent refuses and may refuse more.
func TestDeniedHostsAreInheritedByTheChild(t *testing.T) {
	parent := parentSandbox()
	parent.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"bad.example.com"}}
	obj := childSandbox()
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen}
	if got := refusal(t, obj, spawnOptions(parent)); !slices.Contains(got.Paths, pathDeniedHosts) {
		t.Fatalf("paths = %v, want %s among them", got.Paths, pathDeniedHosts)
	}
	obj.Spec.Network.Egress.DeniedHosts = []string{"bad.example.com", "worse.example.com"}
	resolve(t, obj, spawnOptions(parent))
}

// TestBoundaryReadsEachListUnderItsOwnMode: a child that narrows the mode is
// not refused for the list its own mode does not carry, and a narrowed mode
// does not lift the parent's deny list.
func TestBoundaryReadsEachListUnderItsOwnMode(t *testing.T) {
	parent := parentSandbox()
	parent.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"bad.example.com"}}

	// Narrowing from open to an allow list of a host the parent admits.
	obj := childSandbox()
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}
	resolve(t, obj, spawnOptions(parent))

	// Narrowing from open to none carries no list at all.
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressNone}
	resolve(t, obj, spawnOptions(parent))

	// A host the parent refuses is refused however the child narrows.
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"bad.example.com"}}
	got := refusal(t, obj, spawnOptions(parent))
	if got.Code != "boundary_exceeded" || !slices.Contains(got.Paths, pathAllowedHosts) {
		t.Fatalf("got %s at %v, want boundary_exceeded at %s", got.Code, got.Paths, pathAllowedHosts)
	}
}

// TestChildTTLIsBoundedByTheParent is the stage 2 half of rule 6: a child
// that names no life gets the lesser of the default and its parent's
// remainder, and one that names a longer life is refused.
func TestChildTTLIsBoundedByTheParent(t *testing.T) {
	parent := parentSandbox()
	parent.Status.ExpiresAt = fixedNow.Add(30 * time.Minute)
	o := spawnOptions(parent)
	o.Defaults.TTL = "24h"
	obj := childSandbox()
	obj.Spec.Lifecycle.TTL = ""
	got := resolve(t, obj, o).Sandbox
	if got.Spec.Lifecycle.TTL != "30m0s" {
		t.Fatalf("ttl = %q, want the parent's remaining 30m0s", got.Spec.Lifecycle.TTL)
	}
	// A default already inside the parent's life is left alone.
	o.Defaults.TTL = "10m"
	if got = resolve(t, obj, o).Sandbox; got.Spec.Lifecycle.TTL != "10m" {
		t.Fatalf("ttl = %q, want the shorter default 10m", got.Spec.Lifecycle.TTL)
	}
}

// TestChildOfAnExpiredParentIsRefused is the edge of rule 6: a parent already
// past its own expiry has no life for a child to fit inside, so every spawn
// from it is refused rather than given a life its parent does not have.
func TestChildOfAnExpiredParentIsRefused(t *testing.T) {
	parent := parentSandbox()
	parent.Status.ExpiresAt = fixedNow.Add(-time.Minute)
	o := spawnOptions(parent)
	o.Defaults.TTL = "24h"
	obj := childSandbox()
	obj.Spec.Lifecycle.TTL = ""
	got := refusal(t, obj, o)
	if got.Code != "boundary_exceeded" || !slices.Contains(got.Paths, pathTTL) {
		t.Fatalf("got %s at %v, want boundary_exceeded at %s", got.Code, got.Paths, pathTTL)
	}
}

// TestChildIdleStopIsCutWithItsLife holds the two lifecycle fields together: a
// life cut to the parent's remainder takes an unnamed idle stop with it, so
// the defaulted pair does not refuse itself.
func TestChildIdleStopIsCutWithItsLife(t *testing.T) {
	parent := parentSandbox()
	parent.Status.ExpiresAt = fixedNow.Add(5 * time.Minute)
	o := spawnOptions(parent)
	o.Defaults.TTL, o.Defaults.AutoStop = "24h", "15m"
	obj := childSandbox()
	obj.Spec.Lifecycle = v1.Lifecycle{}
	got := resolve(t, obj, o).Sandbox
	if got.Spec.Lifecycle.TTL != "5m0s" || got.Spec.Lifecycle.AutoStop != "5m0s" {
		t.Fatalf("lifecycle = %+v, want both cut to 5m0s", got.Spec.Lifecycle)
	}
	// An idle stop the caller named is the caller's, whatever the life
	// becomes.
	obj.Spec.Lifecycle.AutoStop = "1m"
	if got = resolve(t, obj, o).Sandbox; got.Spec.Lifecycle.AutoStop != "1m" {
		t.Fatalf("autoStop = %q, want the caller's 1m", got.Spec.Lifecycle.AutoStop)
	}
}

// TestParentThatNeverExpiresBoundsNoLife is the other edge: a root with no
// expiry leaves its children's lives to the operator's default.
func TestParentThatNeverExpiresBoundsNoLife(t *testing.T) {
	parent := parentSandbox()
	parent.Status.ExpiresAt = time.Time{}
	o := spawnOptions(parent)
	o.Defaults.TTL = "24h"
	obj := childSandbox()
	obj.Spec.Lifecycle.TTL = ""
	if got := resolve(t, obj, o).Sandbox; got.Spec.Lifecycle.TTL != "24h" {
		t.Fatalf("ttl = %q, want the default 24h", got.Spec.Lifecycle.TTL)
	}
}

// TestSpawnFieldsAndDepth is rule 7 read three ways: an absent spawn section
// is zero on both axes, the remainder is what a child may ask for, and a
// parent with no generations left creates nothing at all.
func TestSpawnFieldsAndDepth(t *testing.T) {
	parent := parentSandbox()
	got := resolve(t, childSandbox(), spawnOptions(parent)).Sandbox
	if got.Spec.Mesh.Spawn != (v1.Spawn{}) {
		t.Fatalf("spawn = %+v, want zero on both axes", got.Spec.Mesh.Spawn)
	}
	// The remainder is budget minus used minus one: four minus one minus one.
	obj := childSandbox()
	obj.Spec.Mesh.Spawn = v1.Spawn{Budget: 2, Depth: 1}
	resolve(t, obj, spawnOptions(parent))

	spent := parentSandbox()
	spent.Status.Spawn.Depth = 0
	if refused := refusal(t, childSandbox(), spawnOptions(spent)); !slices.Contains(refused.Paths, pathSpawnDepth) {
		t.Fatalf("paths = %v, want %s among them", refused.Paths, pathSpawnDepth)
	}
}

// TestASpentBudgetIsTheLedgersRefusal: a parent that has spent its budget
// resolves its child and is refused at the debit, because an exhausted budget
// is a count and not a manifest that exceeds a boundary. The two answers are
// apart: spawn_budget_exhausted names a race a caller retries and
// boundary_exceeded names a manifest a caller rewrites.
func TestASpentBudgetIsTheLedgersRefusal(t *testing.T) {
	spent := parentSandbox()
	spent.Status.Spawn.Used = spent.Status.Spawn.Budget
	obj := childSandbox()
	got := resolve(t, obj, spawnOptions(spent)).Sandbox
	if got.Spec.Mesh.Spawn != (v1.Spawn{}) {
		t.Fatalf("spawn = %+v", got.Spec.Mesh.Spawn)
	}
	// The containment on the axis still holds where a unit remains.
	almost := parentSandbox()
	almost.Status.Spawn.Used = almost.Status.Spawn.Budget - 1
	obj.Spec.Mesh.Spawn.Budget = 1
	if refused := refusal(t, obj, spawnOptions(almost)); !slices.Contains(refused.Paths, pathSpawnBudget) {
		t.Fatalf("paths = %v, want %s among them", refused.Paths, pathSpawnBudget)
	}
}

// TestSpawnBudgetIsNeverNegative is the structural rule: a manifest cannot
// ask for a negative number of children.
func TestSpawnBudgetIsNeverNegative(t *testing.T) {
	for _, tc := range []struct {
		path string
		with func(*v1.Sandbox)
	}{
		{pathSpawnBudget, func(s *v1.Sandbox) { s.Spec.Mesh.Spawn.Budget = -1 }},
		{pathSpawnDepth, func(s *v1.Sandbox) { s.Spec.Mesh.Spawn.Depth = -1 }},
	} {
		obj := sandbox()
		tc.with(&obj)
		got := refusal(t, obj, containerOptions())
		if got.Code != "invalid_field" || got.Path != tc.path {
			t.Errorf("got %s at %s, want invalid_field at %s", got.Code, got.Path, tc.path)
		}
	}
}

// TestMeshMembership is the one function an expose: mesh port and the mesh
// rule both read: a root declares membership and a child inherits it.
func TestMeshMembership(t *testing.T) {
	root := sandbox()
	if MeshMember(&root) {
		t.Error("a sandbox that enabled no mesh is a member")
	}
	root.Spec.Mesh.Enabled = true
	if !MeshMember(&root) {
		t.Error("a root that enabled a mesh is not a member")
	}
	child := sandbox()
	child.Status.Mesh = "msh_01J9"
	if !MeshMember(&child) {
		t.Error("a child that inherited a mesh is not a member")
	}
}

// TestMeshCapability is the mesh row of stage 7: an environment whose driver
// connects no peers refuses the field rather than recording it.
func TestMeshCapability(t *testing.T) {
	obj := sandbox()
	obj.Spec.Mesh.Enabled = true
	got := refusal(t, obj, containerOptions())
	if got.Code != "capability_unsupported" || got.Path != pathMeshEnabled {
		t.Fatalf("got %s at %s, want capability_unsupported at %s", got.Code, got.Path, pathMeshEnabled)
	}
	resolve(t, obj, environmentOptions(meshEnvironment("default")))
}

// TestMeshEnabledIsImmutable holds the field to the table: a mesh is joined
// at create and never after.
func TestMeshEnabledIsImmutable(t *testing.T) {
	existing := resolve(t, sandbox(), environmentOptions(meshEnvironment("default"))).Sandbox
	obj := sandbox()
	obj.Spec.Mesh.Enabled = true
	o := environmentOptions(meshEnvironment("default"))
	o.Existing = &existing
	got := refusal(t, obj, o)
	if got.Code != "immutable_field" || !slices.Contains(got.Paths, pathMeshEnabled) {
		t.Fatalf("got %s at %v, want immutable_field at %s", got.Code, got.Paths, pathMeshEnabled)
	}
}

// TestWorkloadCannotRaiseItsOwnBudget is the narrowing rule on the two spawn
// axes: a sandbox lowers its own budget and never raises it, and its owner
// may do either.
func TestWorkloadCannotRaiseItsOwnBudget(t *testing.T) {
	base := sandbox()
	base.Spec.Mesh.Spawn = v1.Spawn{Budget: 2, Depth: 1}
	existing := resolve(t, base, containerOptions()).Sandbox

	for _, tc := range []struct {
		path  string
		spawn v1.Spawn
	}{
		{pathSpawnBudget, v1.Spawn{Budget: 4, Depth: 1}},
		{pathSpawnDepth, v1.Spawn{Budget: 2, Depth: 3}},
	} {
		obj := sandbox()
		obj.Spec.Mesh.Spawn = tc.spawn
		o := containerOptions()
		o.Existing = &existing
		o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
		got := refusal(t, obj, o)
		if got.Code != "boundary_widened" || !slices.Contains(got.Paths, tc.path) {
			t.Errorf("got %s at %v, want boundary_widened at %s", got.Code, got.Paths, tc.path)
		}
		// The owner raising the same field is accepted.
		o.Actor = Actor{Subject: "alice"}
		resolve(t, obj, o)
	}

	// Lowering is the workload's own to do.
	lower := sandbox()
	lower.Spec.Mesh.Spawn = v1.Spawn{Budget: 1}
	o := containerOptions()
	o.Existing = &existing
	o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
	resolve(t, lower, o)
}

// TestDescendantOutsideBoundary is the update half of the rule: a root
// narrowed below a live child is refused, and the error names the child.
func TestDescendantOutsideBoundary(t *testing.T) {
	parent := parentSandbox()
	child := childSandbox()
	child.Status.ID = "sbx_child"
	if err := DescendantOutsideBoundary(&parent, &child, fixedNow); err != nil {
		t.Fatalf("a child inside its parent's boundary was refused: %v", err)
	}
	narrowed := parent
	narrowed.Spec.Network.Egress.AllowedHosts = []string{"*.b.example.com"}
	err := DescendantOutsideBoundary(&narrowed, &child, fixedNow)
	var known *Error
	if !errors.As(err, &known) {
		t.Fatalf("got %v, want a manifest error", err)
	}
	if known.Code != "boundary_exceeded" || !slices.Contains(known.Paths, pathAllowedHosts) {
		t.Fatalf("got %s at %v, want boundary_exceeded at %s", known.Code, known.Paths, pathAllowedHosts)
	}
	if !strings.Contains(known.Detail, "sbx_child") {
		t.Fatalf("detail = %q, want the descendant named", known.Detail)
	}
	// A child with no id yet is named by the name its author gave it.
	child.Status.ID = ""
	err = DescendantOutsideBoundary(&narrowed, &child, fixedNow)
	if !errors.As(err, &known) || !strings.Contains(known.Detail, "child") {
		t.Fatalf("detail = %v, want the descendant's name", err)
	}
}

// TestBoundaryRefusesAnUnparsableChildQuantity is the one rule that answers
// with a code of its own: a quantity that does not parse is invalid_field
// rather than a containment nobody can decide.
func TestBoundaryRefusesAnUnparsableChildQuantity(t *testing.T) {
	parent := parentSandbox()
	child := childSandbox()
	child.Spec.Resources.CPU = "nonsense"
	err := boundary(&child, &parent, fixedNow)
	var known *Error
	if !errors.As(err, &known) || known.Code != "invalid_field" {
		t.Fatalf("got %v, want invalid_field", err)
	}
	// A parent whose own quantity does not parse bounds nothing on that
	// axis: the child's own validation already refused a bad value.
	parent.Spec.Resources.Memory = "nonsense"
	child.Spec.Resources.CPU = "1"
	child.Spec.Resources.Memory = "64Gi"
	if err = boundary(&child, &parent, fixedNow); err != nil {
		t.Fatalf("boundary: %v", err)
	}
}

// TestNoParentIsNoBoundary is the rule's gate: an apply with no parent runs
// no containment at all.
func TestNoParentIsNoBoundary(t *testing.T) {
	obj := childSandbox()
	obj.Spec.Mesh.Spawn = v1.Spawn{Budget: 9, Depth: 9}
	if err := boundary(&obj, nil, fixedNow); err != nil {
		t.Fatalf("boundary with no parent: %v", err)
	}
}
