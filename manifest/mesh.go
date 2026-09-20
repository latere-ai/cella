// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// MeshIDPrefix is the kind prefix of a mesh id. A mesh has no row and no
// kind: it is this id, minted at the create of a root that enabled one, and
// the set of live sandboxes whose status carries it.
const MeshIDPrefix = "msh_"

// The JSON paths of the mesh and spawn fields, named once so a refusal, a
// narrowing violation and a boundary violation point at the same string.
const (
	pathMeshEnabled = "spec.mesh.enabled"
	pathSpawnBudget = "spec.mesh.spawn.budget"
	pathSpawnDepth  = "spec.mesh.spawn.depth"
	pathEnvironment = "spec.environment"
	pathResources   = "spec.resources"
	pathTTL         = "spec.lifecycle.ttl"
)

// MeshMember reports whether a sandbox is in a mesh, which is what an
// expose: mesh port stands on. A root declares membership with
// spec.mesh.enabled and a child inherits it as status.mesh, so the two forms
// are one question and this is the one function that answers it.
func MeshMember(obj *v1.Sandbox) bool {
	return obj.Spec.Mesh.Enabled || obj.Status.Mesh != ""
}

// validateMesh is the structural half of the mesh rule: neither axis of the
// spawn budget counts backwards.
func validateMesh(m v1.Mesh) error {
	for _, f := range []struct {
		path  string
		value int
	}{{pathSpawnBudget, m.Spawn.Budget}, {pathSpawnDepth, m.Spawn.Depth}} {
		if f.value < 0 {
			return failAt("invalid_field", f.path, "The spawn budget cannot be negative.")
		}
	}
	return nil
}

// meshCapability is the mesh row of stage 7. An environment whose driver
// gives peers no way to reach each other refuses the field rather than
// recording it, because a manifest that asked for a mesh and got none would
// read as composed and run isolated.
func meshCapability(obj *v1.Sandbox, env *v1.Environment) error {
	if obj.Spec.Mesh.Enabled && !env.Status.Capabilities.Mesh {
		return failAt("capability_unsupported", pathMeshEnabled, "This environment does not connect sandboxes to each other.")
	}
	return nil
}

// meshNarrowing is the spawn half of the narrowing rule: a workload may lower
// either axis of its own budget and may not raise one. It returns the paths it
// found so the egress half and this one are named in one error.
func meshNarrowing(existing, obj *v1.Sandbox) []string {
	var paths []string
	was, now := existing.Spec.Mesh.Spawn, obj.Spec.Mesh.Spawn
	if now.Budget > was.Budget {
		paths = append(paths, pathSpawnBudget)
	}
	if now.Depth > was.Depth {
		paths = append(paths, pathSpawnDepth)
	}
	return paths
}

// defaultChildTTL is the stage 2 rule a spawned child takes: a child that
// named no time to live is cut to its parent's remaining life where the
// operator's default runs past it, so the common case conforms to rule 6
// without the caller computing anything. A caller that named a life of its
// own is left alone and rule 6 judges it. A parent that never expires, or one
// already past its own expiry, bounds nothing here: the first because nothing
// outlives it, the second because no life at all fits inside it and rule 6
// refuses the spawn.
//
// namedStop says whether the caller named an idle stop. Where it did not, the
// defaulted stop is cut with the life, because an idle stop later than the
// life is refused by the lifecycle rule and neither value was the caller's.
func defaultChildTTL(obj *v1.Sandbox, parent *v1.Sandbox, now time.Time, namedStop bool) {
	if parent == nil || parent.Status.ExpiresAt.IsZero() || now.IsZero() {
		return
	}
	remaining := parent.Status.ExpiresAt.Sub(now).Truncate(time.Second)
	if remaining <= 0 {
		return
	}
	current, never, err := ParseDuration(obj.Spec.Lifecycle.TTL)
	if err == nil && !never && current <= remaining {
		return
	}
	obj.Spec.Lifecycle.TTL = v1.Duration(remaining.String())
	if namedStop {
		return
	}
	stop, stopNever, err := ParseDuration(obj.Spec.Lifecycle.AutoStop)
	if err == nil && !stopNever && stop <= remaining {
		return
	}
	obj.Spec.Lifecycle.AutoStop = v1.Duration(remaining.String())
}

// boundary is stage 6: the child's resolved manifest against the parent's
// desired spec and current status, as a containment on every field the
// contract marks a boundary. Every offending path is named in one error,
// because a caller that widened three fields learns all three in one round.
//
// The rules are numbered as spec 003 numbers them. Rule 4 has no field until
// the Volume kind lands.
func boundary(obj *v1.Sandbox, parent *v1.Sandbox, now time.Time) error {
	if parent == nil {
		return nil
	}
	var paths []string
	add := func(path string) { paths = append(paths, path) }

	// 1 and 2: the network boundary. A child reaches at most where its
	// parent reaches, by mode and by pattern. Each list is read under the
	// mode that owns it, because the other mode leaves it empty by the
	// exclusive-fields rule and an empty list is not a narrow one.
	child, over := obj.Spec.Network.Egress, parent.Spec.Network.Egress
	if v1.EgressModeRank(child.Mode) < v1.EgressModeRank(over.Mode) {
		add(pathEgressMode)
	}
	switch {
	case child.Mode == v1.EgressAllowlist && over.Mode == v1.EgressAllowlist:
		// An allow list widens when it names a destination the parent's
		// does not reach.
		for _, pattern := range child.AllowedHosts {
			if !v1.HostCovers(over.AllowedHosts, pattern) {
				add(pathAllowedHosts)
				break
			}
		}
	case child.Mode == v1.EgressAllowlist && over.Mode == v1.EgressOpen:
		// A child narrowing from open to an allow list may name any host
		// but one its parent refuses: the parent's deny list is a boundary
		// and narrowing the mode does not lift it.
		for _, pattern := range child.AllowedHosts {
			if v1.HostCovers(over.DeniedHosts, pattern) {
				add(pathAllowedHosts)
				break
			}
		}
	}
	// A deny list is the inverse of an allow list: the child keeps every
	// host the parent refuses and may refuse more. It is read only where
	// both sides are open, because that is the only mode that has one.
	if child.Mode == v1.EgressOpen && over.Mode == v1.EgressOpen {
		for _, pattern := range over.DeniedHosts {
			if !v1.HostCovers(child.DeniedHosts, pattern) {
				add(pathDeniedHosts)
				break
			}
		}
	}

	// 3: a child mounts only what its parent mounts. The name is the key
	// because the resolver has not yet bound either side to an id.
	mounted := map[string]bool{}
	for _, m := range parent.Spec.Secrets {
		mounted[m.Name] = true
	}
	for i, m := range obj.Spec.Secrets {
		if !mounted[m.Name] {
			add(pathSecretAt(i, "name"))
		}
	}

	// 5: the compute a child asks for is inside its parent's. A parent that
	// named no amount for an axis bounds nothing on it, which is the same
	// reading a ceiling of zero has.
	childRes, parentRes := obj.Spec.Resources, parent.Spec.Resources
	for _, f := range []struct {
		path         string
		child, above v1.Quantity
	}{
		{pathResources + ".cpu", childRes.CPU, parentRes.CPU},
		{pathResources + ".memory", childRes.Memory, parentRes.Memory},
		{pathResources + ".disk", childRes.Disk, parentRes.Disk},
	} {
		if f.child == "" || f.above == "" {
			continue
		}
		want, err := ParseQuantity(f.child)
		if err != nil {
			return failAt("invalid_field", f.path, upperFirst(err.Error())+".")
		}
		limit, err := ParseQuantity(f.above)
		if err != nil {
			continue
		}
		if want > limit {
			add(f.path)
		}
	}

	// 6: a child never outlives its parent.
	if ttlOutlivesParent(obj.Spec.Lifecycle.TTL, parent.Status.ExpiresAt, now) {
		add(pathTTL)
	}

	// 7: the two spawn axes. A parent with no generations left creates
	// nothing at all, which is this rule; a parent with no budget left is
	// the ledger's refusal at the debit and not a manifest that exceeds
	// anything, so the containment is read only where a unit remains.
	remaining := parent.Status.Spawn.Budget - parent.Status.Spawn.Used
	if parent.Status.Spawn.Depth <= 0 || obj.Spec.Mesh.Spawn.Depth > parent.Status.Spawn.Depth-1 {
		add(pathSpawnDepth)
	}
	if remaining > 0 && obj.Spec.Mesh.Spawn.Budget > remaining-1 {
		add(pathSpawnBudget)
	}

	// 8: a mesh never spans environments, so neither does a tree.
	if obj.Spec.Environment != parent.Spec.Environment {
		add(pathEnvironment)
	}

	// 9: the mesh is inherited, so a child that asks to join one is asking
	// for a membership it already has or one its parent does not.
	if obj.Spec.Mesh.Enabled {
		add(pathMeshEnabled)
	}

	if len(paths) == 0 {
		return nil
	}
	return failPaths("boundary_exceeded", "A spawned sandbox cannot exceed its parent's boundary: "+strings.Join(paths, ", ")+".", paths)
}

// ttlOutlivesParent reports whether the child's time to live ends after the
// parent's expiry. A parent with no expiry never ends, so nothing outlives it.
func ttlOutlivesParent(ttl v1.Duration, parentExpiry, now time.Time) bool {
	if parentExpiry.IsZero() || now.IsZero() {
		return false
	}
	life, never, err := ParseDuration(ttl)
	if err != nil {
		return false
	}
	if never {
		return true
	}
	return now.Add(life).After(parentExpiry)
}

// DescendantOutsideBoundary is the update half of the boundary rule: a
// non-workload actor may narrow a sandbox that already has children, and an
// update that would leave one of them outside is refused rather than applied.
// The error names the descendant so a caller learns which child blocked it,
// and the paths it exceeds.
//
// The caller passes the updated parent and each live immediate child; the
// control plane owns the tree and this package owns the rule.
func DescendantOutsideBoundary(updated *v1.Sandbox, child *v1.Sandbox, now time.Time) error {
	err := boundary(child, updated, now)
	if err == nil {
		return nil
	}
	var known *Error
	if !errors.As(err, &known) {
		return err
	}
	name := child.Status.ID
	if name == "" {
		name = child.Metadata.Name
	}
	return failPaths(known.Code, fmt.Sprintf("The sandbox %s is inside this one's boundary and this change would put it outside: %s.",
		name, strings.Join(known.Paths, ", ")), known.Paths)
}
