// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The JSON paths of the Volume's fields, named once so a refusal and a test
// point at the same string.
const (
	pathVolumeEnvironment = "spec.environment"
	pathVolumeSize        = "spec.size"
	pathVolumeClass       = "spec.class"
	pathVolumeAccess      = "spec.access"
	pathVolumeSource      = "spec.source"
	pathVolumeSourceKind  = "spec.source.kind"
	pathVolumeImage       = "spec.source.image"
	pathVolumeRetain      = "spec.retain"
)

// DecodeVolume reads one Volume manifest, in any syntax design 003 admits,
// and discards the status a client sent.
func DecodeVolume(body []byte, contentType string) (v1.Volume, error) {
	var obj v1.Volume
	if err := decode(body, contentType, v1.KindVolume, &obj); err != nil {
		return v1.Volume{}, err
	}
	obj.Status = v1.VolumeStatus{}
	return obj, nil
}

// VolumeOptions are what ResolveVolume needs beyond the manifest: who is
// applying, the environments a volume may live in, the admission step its
// image source passes, and the object this one replaces.
type VolumeOptions struct {
	Actor  Actor
	Lookup Lookup
	// Admit is the admission step of spec 007. A Volume whose source is an
	// image passes it once, for its image alone, through the shape of a
	// Sandbox apply: an image catalog admits a volume's image exactly as it
	// admits a sandbox's.
	Admit     AdmitFunc
	Claims    map[string]any
	RequestID string
	Existing  *v1.Volume // the current object on update; nil on create
}

// ResolveVolume validates and defaults one Volume. The stages are those of
// Resolve where a Volume has them: the field rules, the defaults and the
// environment, the admission of an image source and the digest rule after
// it, the capability check, and immutability against the stored object.
//
// The returned object's spec.source.image is the reference the admission step
// answered, which is pinned by digest: a reader of the volume compares the
// digest it holds and never the name its caller wrote.
func ResolveVolume(ctx context.Context, in *v1.Volume, o VolumeOptions) (*v1.Volume, error) {
	if in == nil {
		return nil, errors.New("manifest: ResolveVolume needs a manifest")
	}
	if o.Lookup == nil {
		return nil, errors.New("manifest: ResolveVolume needs an environment lookup")
	}
	obj := cloneVolume(in)
	obj.Status = v1.VolumeStatus{}
	if obj.APIVersion != v1.APIVersion {
		return nil, failAt("unsupported_version", "apiVersion", "This server serves "+v1.APIVersion+".")
	}
	if obj.Kind != v1.KindVolume {
		return nil, failAt("unsupported_kind", "kind", "This server does not serve that kind.")
	}
	if err := validateMetadata(obj.Metadata); err != nil {
		return nil, err
	}
	if obj.Metadata.Name == "" {
		return nil, failAt("missing_field", "metadata.name", "A volume is named by the caller.")
	}
	if err := validateVolumeSpec(obj.Spec); err != nil {
		return nil, err
	}
	if o.Existing != nil && obj.Spec.Environment == "" {
		obj.Spec.Environment = o.Existing.Spec.Environment
	}
	env, err := lookupEnvironment(ctx, o.Lookup, obj.Spec.Environment)
	if err != nil {
		return nil, err
	}
	obj.Spec.Environment = env.Metadata.Name
	defaultVolume(&obj.Spec)
	if err := volumeCapability(obj.Spec, env); err != nil {
		return nil, err
	}
	if o.Existing != nil {
		if err := volumeUpdate(ctx, &obj, env, o); err != nil {
			return nil, err
		}
		return &obj, nil
	}
	if obj.Spec.Source.Kind == v1.VolumeSourceImage {
		image, err := admitVolumeImage(ctx, &obj, env, o)
		if err != nil {
			return nil, err
		}
		obj.Spec.Source.Image = image
	}
	if err := imagePinned(obj.Spec.Source); err != nil {
		return nil, err
	}
	return &obj, nil
}

// validateVolumeSpec runs the field rules that need no defaults and no
// lookups: the shape of every field, exactly one source field for its kind,
// and the refusals of what this server does not build.
func validateVolumeSpec(s v1.VolumeSpec) error {
	if s.Environment != "" && (len(s.Environment) > maxNameLength || !namePattern.MatchString(s.Environment)) {
		return failAt("invalid_field", pathVolumeEnvironment, "The environment must be a DNS label of at most 63 characters.")
	}
	if s.Size == "" {
		return failAt("missing_field", pathVolumeSize, "A volume names its size.")
	}
	size, err := ParseQuantity(s.Size)
	if err != nil {
		return failAt("invalid_field", pathVolumeSize, upperFirst(err.Error())+".")
	}
	if size <= 0 {
		return failAt("invalid_field", pathVolumeSize, "The size must be positive.")
	}
	if s.Access != "" && !slices.Contains(v1.VolumeAccesses, s.Access) {
		return failAt("invalid_field", pathVolumeAccess, "The access is single or shared-read.")
	}
	if s.Class != "" {
		return failAt("capability_unsupported", pathVolumeClass, "This server provisions every volume on its environment's own storage class.")
	}
	return validateVolumeSource(s.Source)
}

// validateVolumeSource holds the source to exactly the field its kind names.
// The snapshot and archive sources are the contract's and are refused here
// as not served, so a manifest naming one is never accepted and left empty.
func validateVolumeSource(src v1.VolumeSource) error {
	kind := src.Kind
	if kind == "" {
		kind = v1.VolumeSourceEmpty
	}
	if !slices.Contains(v1.VolumeSources, kind) {
		return failAt("invalid_field", pathVolumeSourceKind, "The source is empty, snapshot, image, or archive.")
	}
	set := map[string]bool{
		v1.VolumeSourceSnapshot: src.Snapshot != "",
		v1.VolumeSourceImage:    src.Image != "",
		v1.VolumeSourceArchive:  src.Archive != "",
	}
	var extra []string
	for _, field := range slices.Sorted(maps.Keys(set)) {
		if set[field] && field != kind {
			extra = append(extra, pathVolumeSource+"."+field)
		}
	}
	if src.SHA256 != "" && kind != v1.VolumeSourceArchive {
		extra = append(extra, pathVolumeSource+".sha256")
	}
	if len(extra) > 0 {
		return failPaths("exclusive_fields", "A volume's source sets the field its kind names and no other.", extra)
	}
	switch kind {
	case v1.VolumeSourceSnapshot, v1.VolumeSourceArchive:
		return failAt("capability_unsupported", pathVolumeSourceKind, "This server fills a volume from empty or from an image.")
	case v1.VolumeSourceImage:
		if src.Image == "" {
			return failAt("missing_field", pathVolumeImage, "An image source names the image.")
		}
	}
	return nil
}

// defaultVolume fills every absent field the contract gives a default.
func defaultVolume(s *v1.VolumeSpec) {
	if s.Access == "" {
		s.Access = v1.VolumeAccessSingle
	}
	if s.Source.Kind == "" {
		s.Source.Kind = v1.VolumeSourceEmpty
	}
	if s.Retain == nil {
		retain := true
		s.Retain = &retain
	}
}

// volumeCapability holds the volume to what its environment's driver can
// keep: volumes at all, and an image source where the driver fills one.
func volumeCapability(s v1.VolumeSpec, env *v1.Environment) error {
	if !env.Status.Capabilities.Volumes {
		return failAt("capability_unsupported", pathVolumeEnvironment, "This environment keeps no volumes.")
	}
	if s.Source.Kind == v1.VolumeSourceImage && !env.Status.Capabilities.VolumeImages {
		return failAt("capability_unsupported", pathVolumeSourceKind, "This environment cannot fill a volume from an image.")
	}
	return nil
}

// imagePinned is the image rule of a volume, which holds with or without an
// admission step: the image a volume is filled from is pinned by digest, so
// the volume's content is named by what it holds and never by a tag a
// registry may move.
func imagePinned(src v1.VolumeSource) error {
	if src.Kind != v1.VolumeSourceImage || v1.PinnedImage(src.Image) {
		return nil
	}
	return failAt("invalid_field", pathVolumeImage, "The image is pinned by digest: a reference ending in @sha256: and 64 hex digits.")
}

// admitVolumeImage passes a volume's image through the admission step, and
// returns the reference it answered. The step is asked with a Sandbox that
// names the volume's environment, its labels and the image and nothing else,
// so an image catalog decides on the image exactly as it would for a sandbox
// and no rule of a sandbox's has a field to refuse on; AdmitRequest.Volume
// says which apply it is. Of the answer only spec.image is read.
func admitVolumeImage(ctx context.Context, obj *v1.Volume, env *v1.Environment, o VolumeOptions) (string, error) {
	if o.Admit == nil {
		return obj.Spec.Source.Image, nil
	}
	probe := v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata:   v1.Metadata{Labels: maps.Clone(obj.Metadata.Labels)},
		Spec:       v1.SandboxSpec{Environment: obj.Spec.Environment, Image: obj.Spec.Source.Image},
	}
	volume := cloneVolume(obj)
	out, _, err := o.Admit(ctx, &probe, AdmitRequest{
		Actor: o.Actor, Claims: o.Claims, Action: "create", Environment: env,
		RequestID: o.RequestID, Volume: &volume,
	})
	if err != nil {
		var known *Error
		if errors.As(err, &known) {
			return "", known
		}
		return "", fail("admission_refused", upperFirst(err.Error()))
	}
	if out == nil || out.Spec.Image == "" {
		return "", failAt("admission_refused", pathVolumeImage, "The admission step returned no image for the volume.")
	}
	return out.Spec.Image, nil
}

// volumeUpdate holds an update to the fields the contract lets change: the
// labels and annotations, retain, and a size that grows, which no driver of
// this server can apply yet. Every other field is immutable. An image named
// the way the caller first wrote it, such as a catalog name, is admitted
// again and compared by the reference it resolves to, so a manifest applied
// twice is not refused for naming what the server already pinned.
func volumeUpdate(ctx context.Context, obj *v1.Volume, env *v1.Environment, o VolumeOptions) error {
	existing := o.Existing
	if obj.Spec.Source.Kind == v1.VolumeSourceImage && obj.Spec.Source.Image != existing.Spec.Source.Image &&
		existing.Spec.Source.Kind == v1.VolumeSourceImage {
		image, err := admitVolumeImage(ctx, obj, env, o)
		if err != nil {
			return err
		}
		if image == existing.Spec.Source.Image {
			obj.Spec.Source.Image = image
		}
	}
	var paths []string
	for _, f := range []struct {
		path    string
		changed bool
	}{
		{"metadata.name", existing.Metadata.Name != obj.Metadata.Name},
		{pathVolumeEnvironment, existing.Spec.Environment != obj.Spec.Environment},
		{pathVolumeClass, existing.Spec.Class != obj.Spec.Class},
		{pathVolumeAccess, existing.Spec.Access != obj.Spec.Access},
		{pathVolumeSource, existing.Spec.Source != obj.Spec.Source},
	} {
		if f.changed {
			paths = append(paths, f.path)
		}
	}
	if len(paths) > 0 {
		return failPaths("immutable_field", "These fields cannot be changed after the volume is created: "+strings.Join(paths, ", ")+".", paths)
	}
	before, err := ParseQuantity(existing.Spec.Size)
	if err != nil {
		return errors.New("manifest: the stored volume's size is invalid: " + err.Error())
	}
	after, err := ParseQuantity(obj.Spec.Size)
	if err != nil {
		return failAt("invalid_field", pathVolumeSize, upperFirst(err.Error())+".")
	}
	switch {
	case after < before:
		return failAt("invalid_field", pathVolumeSize, "A volume grows and never shrinks.")
	case after > before:
		return failAt("capability_unsupported", pathVolumeSize, "This server cannot grow a volume that exists.")
	}
	// The stored spelling of the size is kept, so an update that names the
	// same amount in another unit changes nothing a reader sees.
	obj.Spec.Size = existing.Spec.Size
	return nil
}

// cloneVolume copies every reference one volume holds, so a resolve never
// writes through its caller's object.
func cloneVolume(in *v1.Volume) v1.Volume {
	out := *in
	out.Metadata.Labels = maps.Clone(in.Metadata.Labels)
	out.Metadata.Annotations = maps.Clone(in.Metadata.Annotations)
	out.Status.AttachedTo = slices.Clone(in.Status.AttachedTo)
	if in.Spec.Retain != nil {
		retain := *in.Spec.Retain
		out.Spec.Retain = &retain
	}
	return out
}

// CloneVolume is cloneVolume for a caller outside this package.
func CloneVolume(in v1.Volume) v1.Volume { return cloneVolume(&in) }
