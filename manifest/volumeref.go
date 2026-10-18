// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The JSON path of a sandbox's volume mounts.
const pathVolumes = "spec.volumes"

func pathVolumeAt(i int, field string) string {
	return pathVolumes + "[" + strconv.Itoa(i) + "]." + field
}

// VolumeFunc is the volume half of a Lookup, for a caller that composes one
// out of an environment lookup and a store.
type VolumeFunc func(ctx context.Context, nameOrID string) (*v1.Volume, error)

// WithVolumes is the lookup l with its volume half answered by f. A server
// composes its per-request lookup this way: the volumes come from its store
// through the authorizer's attach decision.
func WithVolumes(l Lookup, f VolumeFunc) Lookup { return volumeLookup{Lookup: l, volumes: f} }

type volumeLookup struct {
	Lookup
	volumes VolumeFunc
}

func (v volumeLookup) Volume(ctx context.Context, nameOrID string) (*v1.Volume, error) {
	if v.volumes == nil {
		return nil, ErrNotFound
	}
	return v.volumes(ctx, nameOrID)
}

// validateVolumes is the stage 1 half of spec.volumes: each mount names
// itself with a DNS label no other mount uses, a volume, and a clean absolute
// path that no other mount and no path the control plane owns equals or
// nests with. The workspace is compared once it is defaulted (volumePaths).
func validateVolumes(mounts []v1.VolumeMount) error {
	names := map[string]bool{}
	for i, m := range mounts {
		switch {
		case m.Name == "":
			return failAt("missing_field", pathVolumeAt(i, "name"), "A mount names itself.")
		case len(m.Name) > maxNameLength || !namePattern.MatchString(m.Name):
			return failAt("invalid_field", pathVolumeAt(i, "name"), "A mount's name is a DNS label of at most 63 characters.")
		case names[m.Name]:
			return failAt("invalid_field", pathVolumeAt(i, "name"), "Two mounts carry the name "+m.Name+".")
		case strings.TrimSpace(m.Volume) == "":
			return failAt("missing_field", pathVolumeAt(i, "volume"), "A mount names the volume it mounts.")
		case m.Path == "":
			return failAt("missing_field", pathVolumeAt(i, "path"), "A mount names where the volume appears.")
		case !absoluteCleanPath(m.Path) || m.Path == "/":
			return failAt("invalid_field", pathVolumeAt(i, "path"), "A mount's path must be a clean absolute path below the root.")
		case nests(m.Path, ReservedPathPrefix):
			return failAt("path_conflict", pathVolumeAt(i, "path"), "A mount cannot be at or under "+ReservedPathPrefix+", which the control plane owns.")
		}
		names[m.Name] = true
		for j := range i {
			if nests(m.Path, mounts[j].Path) {
				return failPaths("path_conflict", "Two mounts are at the same path or one inside the other.",
					[]string{pathVolumeAt(j, "path"), pathVolumeAt(i, "path")})
			}
		}
	}
	return nil
}

// volumePaths holds every mount apart from the workspace, which is defaulted
// by the time it runs.
func volumePaths(obj *v1.Sandbox) error {
	for i, m := range obj.Spec.Volumes {
		if nests(m.Path, obj.Spec.Workspace.Path) {
			return failPaths("path_conflict", "A mount cannot be at or inside the workspace, nor the workspace inside it.",
				[]string{pathVolumeAt(i, "path"), "spec.workspace.path"})
		}
	}
	return nil
}

// nests reports whether two clean absolute paths are equal or one is inside
// the other.
func nests(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// volumeReferences is the volume half of stage 4: every mount's volume looked
// up through the actor's own view, in the sandbox's environment, Available or
// still being filled, which the sandbox waits for before it starts, and
// attached in a way its access allows. A shared-read volume is never
// written by a sandbox, so a read-write mount of one is volume_busy whatever
// else holds it. Whether a single volume is free is decided by the
// controller under its lock at create, which is the authoritative attach.
func volumeReferences(ctx context.Context, obj *v1.Sandbox, env *v1.Environment, o Options) ([]v1.Volume, error) {
	if len(obj.Spec.Volumes) == 0 {
		return nil, nil
	}
	// The mounts of a sandbox that exists were bound at its create, and an
	// update that keeps them asks nothing of them again: a volume lost or
	// failed since is the sandbox's own condition to report, not a reason to
	// refuse a change of its labels or its lifecycle. An update that changes
	// them is refused at stage 7.
	if o.Existing != nil && slices.Equal(o.Existing.Spec.Volumes, obj.Spec.Volumes) {
		return nil, nil
	}
	// An environment that keeps no volumes refuses the field before any
	// volume is looked up, so the answer is the same whatever the caller
	// holds.
	if !env.Status.Capabilities.Volumes {
		return nil, failAt("capability_unsupported", pathVolumes, "This environment keeps no volumes.")
	}
	if err := volumePaths(obj); err != nil {
		return nil, err
	}
	volumes := make([]v1.Volume, 0, len(obj.Spec.Volumes))
	for i, m := range obj.Spec.Volumes {
		path := pathVolumeAt(i, "volume")
		volume, err := lookupVolume(ctx, o.Lookup, path, m.Volume)
		if err != nil {
			return nil, err
		}
		switch {
		case volume.Spec.Environment != obj.Spec.Environment:
			return nil, failAt("invalid_field", path, "The volume lives in the environment "+volume.Spec.Environment+", and a sandbox mounts volumes of its own environment only.")
		case volume.Status.Phase != v1.VolumeAvailable && volume.Status.Phase != v1.VolumePending:
			return nil, failAt("invalid_field", path, "The volume is "+volume.Status.Phase+"; a sandbox mounts a volume that is Available or still being filled.")
		case volume.Spec.Access == v1.VolumeAccessSharedRead && !m.ReadOnly:
			return nil, failAt("volume_busy", path, "The volume is shared read-only, so a mount of it sets readOnly.")
		}
		for j := range i {
			if volumes[j].Status.ID == volume.Status.ID {
				return nil, failPaths("invalid_field", "Two mounts name the same volume.",
					[]string{pathVolumeAt(j, "volume"), path})
			}
		}
		volumes = append(volumes, *volume)
	}
	return volumes, nil
}

// lookupVolume asks the actor's own view for one volume. A volume that does
// not exist and one the authorizer refuses to attach are the same answer, so
// existence does not leak to a caller that may not use it.
func lookupVolume(ctx context.Context, lookup Lookup, path, nameOrID string) (*v1.Volume, error) {
	volume, err := lookup.Volume(ctx, nameOrID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, failAt("not_found", path, "There is no such volume.")
	case err != nil:
		var known *Error
		if errors.As(err, &known) {
			return nil, known
		}
		return nil, failAt("authorizer_unavailable", path, "The permission service is unavailable; retry shortly.")
	case volume == nil:
		return nil, failAt("not_found", path, "There is no such volume.")
	}
	return volume, nil
}

// volumeCapabilities holds spec.volumes to what this server does: the mounts
// of a sandbox that exists are the ones it was created with, since it
// changes no mount after create.
func volumeCapabilities(obj *v1.Sandbox, o Options) error {
	if o.Existing != nil && !slices.Equal(o.Existing.Spec.Volumes, obj.Spec.Volumes) {
		return failAt("capability_unsupported", pathVolumes, "This server cannot change the volumes of a sandbox that exists.")
	}
	return nil
}

// volumeBoundary is rule 4 of the boundary check: a child mounts only volumes
// its parent mounts, and a volume its parent holds read-only it holds
// read-only too. A child's reference is compared with what the parent wrote
// and with the id the parent was bound to, so a child may name a volume
// either way its parent's mount reaches it.
func volumeBoundary(obj, parent *v1.Sandbox) []string {
	writable := map[string]bool{}
	held := map[string]bool{}
	for _, m := range parent.Spec.Volumes {
		held[m.Volume] = true
		writable[m.Volume] = writable[m.Volume] || !m.ReadOnly
		for _, bound := range parent.Status.Volumes {
			if bound.Name == m.Name && bound.Volume != "" {
				held[bound.Volume] = true
				writable[bound.Volume] = writable[bound.Volume] || !m.ReadOnly
			}
		}
	}
	var paths []string
	for i, m := range obj.Spec.Volumes {
		switch {
		case !held[m.Volume]:
			paths = append(paths, pathVolumeAt(i, "volume"))
		case !m.ReadOnly && !writable[m.Volume]:
			paths = append(paths, pathVolumeAt(i, "readOnly"))
		}
	}
	return paths
}
