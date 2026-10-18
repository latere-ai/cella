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

// pinned is a digest-pinned reference, the one form an image source is
// stored in.
const pinned = "registry.example.com/grader@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// volumeEnvironment is a container environment that keeps volumes and fills
// them from images.
func volumeEnvironment(name string) v1.Environment {
	env := container(name)
	env.Status.Capabilities.Volumes = true
	env.Status.Capabilities.VolumeImages = true
	return env
}

func aVolume() v1.Volume {
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume, Metadata: v1.Metadata{Name: "grader"},
		Spec: v1.VolumeSpec{Size: "1Gi", Access: v1.VolumeAccessSharedRead,
			Source: v1.VolumeSource{Kind: v1.VolumeSourceImage, Image: pinned}},
	}
}

func volumeOptions() VolumeOptions {
	return VolumeOptions{Lookup: FixedEnvironment(volumeEnvironment("default"))}
}

func volumeRefusal(t *testing.T, obj v1.Volume, o VolumeOptions) *Error {
	t.Helper()
	_, err := ResolveVolume(t.Context(), &obj, o)
	var known *Error
	if !errors.As(err, &known) {
		t.Fatalf("ResolveVolume: got %v, want a manifest error", err)
	}
	return known
}

// TestVolumeFieldRules holds every field rule of the Volume to its refusing
// case: a size absent, unreadable or not positive, an unknown access, a named
// class, two source fields, a source kind not served, an image source with no
// image or one not pinned by digest, and an environment that keeps no volumes
// or fills none from an image.
func TestVolumeFieldRules(t *testing.T) {
	noImages := volumeEnvironment("default")
	noImages.Status.Capabilities.VolumeImages = false
	noVolumes := container("default")
	for _, tc := range []struct {
		name string
		with func(*v1.Volume)
		env  *v1.Environment
		code string
		path string
	}{
		{"no name", func(v *v1.Volume) { v.Metadata.Name = "" }, nil, "missing_field", "metadata.name"},
		{"no size", func(v *v1.Volume) { v.Spec.Size = "" }, nil, "missing_field", pathVolumeSize},
		{"a size that is no quantity", func(v *v1.Volume) { v.Spec.Size = "a lot" }, nil, "invalid_field", pathVolumeSize},
		{"a size that is not positive", func(v *v1.Volume) { v.Spec.Size = "0" }, nil, "invalid_field", pathVolumeSize},
		{"an unknown access", func(v *v1.Volume) { v.Spec.Access = "shared-write" }, nil, "invalid_field", pathVolumeAccess},
		{"a named class", func(v *v1.Volume) { v.Spec.Class = "fast" }, nil, "capability_unsupported", pathVolumeClass},
		{"an unknown source kind", func(v *v1.Volume) { v.Spec.Source = v1.VolumeSource{Kind: "git"} }, nil, "invalid_field", pathVolumeSourceKind},
		{"two source fields", func(v *v1.Volume) { v.Spec.Source.Archive = "https://files.example.com/a.tar" }, nil, "exclusive_fields", "spec.source.archive"},
		{"a digest beside an image", func(v *v1.Volume) { v.Spec.Source.SHA256 = strings.Repeat("a", 64) }, nil, "exclusive_fields", "spec.source.sha256"},
		{"an image on an empty source", func(v *v1.Volume) { v.Spec.Source.Kind = v1.VolumeSourceEmpty }, nil, "exclusive_fields", "spec.source.image"},
		{"a snapshot source", func(v *v1.Volume) {
			v.Spec.Source = v1.VolumeSource{Kind: v1.VolumeSourceSnapshot, Snapshot: "snp_01J9"}
		}, nil, "capability_unsupported", pathVolumeSourceKind},
		{"an archive source", func(v *v1.Volume) {
			v.Spec.Source = v1.VolumeSource{Kind: v1.VolumeSourceArchive, Archive: "https://files.example.com/a.tar"}
		}, nil, "capability_unsupported", pathVolumeSourceKind},
		{"an image source with no image", func(v *v1.Volume) { v.Spec.Source.Image = "" }, nil, "missing_field", pathVolumeImage},
		{"an image by tag", func(v *v1.Volume) { v.Spec.Source.Image = "registry.example.com/grader:1" }, nil, "invalid_field", pathVolumeImage},
		{"an image by a short digest", func(v *v1.Volume) { v.Spec.Source.Image = "registry.example.com/grader@sha256:0123" }, nil, "invalid_field", pathVolumeImage},
		{"an environment with no volumes", func(*v1.Volume) {}, &noVolumes, "capability_unsupported", pathVolumeEnvironment},
		{"an environment that fills none from an image", func(*v1.Volume) {}, &noImages, "capability_unsupported", pathVolumeSourceKind},
		{"an environment that does not exist", func(v *v1.Volume) { v.Spec.Environment = "elsewhere" }, nil, "not_found", "spec.environment"},
		{"an environment that is no name", func(v *v1.Volume) { v.Spec.Environment = "Not A Name" }, nil, "invalid_field", pathVolumeEnvironment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := aVolume()
			tc.with(&obj)
			o := volumeOptions()
			if tc.env != nil {
				o.Lookup = FixedEnvironment(*tc.env)
			}
			got := volumeRefusal(t, obj, o)
			if got.Code != tc.code || (got.Path != tc.path && !slices.Contains(got.Paths, tc.path)) {
				t.Fatalf("got %s at %s %v, want %s at %s", got.Code, got.Path, got.Paths, tc.code, tc.path)
			}
		})
	}
}

// TestVolumeDefaults: an absent access is single, an absent source empty, an
// absent retain true, and the environment the default one, with nothing the
// caller wrote changed and the input left alone.
func TestVolumeDefaults(t *testing.T) {
	in := v1.Volume{APIVersion: v1.APIVersion, Kind: v1.KindVolume, Metadata: v1.Metadata{Name: "state"},
		Spec: v1.VolumeSpec{Size: "20Gi"}}
	got, err := ResolveVolume(t.Context(), &in, volumeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Environment != "default" || got.Spec.Access != v1.VolumeAccessSingle ||
		got.Spec.Source.Kind != v1.VolumeSourceEmpty || !got.Spec.Retains() || got.Spec.Retain == nil {
		t.Fatalf("resolved %+v, want the default environment, single, empty and retain true", got.Spec)
	}
	if in.Spec.Access != "" || in.Spec.Retain != nil {
		t.Fatal("ResolveVolume wrote through its input")
	}
	off := false
	in.Spec.Retain = &off
	got, err = ResolveVolume(t.Context(), &in, volumeOptions())
	if err != nil || got.Spec.Retains() {
		t.Fatalf("retain false resolved to %v, %v", got.Spec.Retain, err)
	}
}

// TestVolumeImageIsAdmitted: an image source passes the admission step as a
// sandbox's image does, through the shape of a Sandbox that names the
// volume's environment, its labels and the image and nothing else, with the
// Volume beside it in the request; the reference the step answers is what is
// stored, so a catalog name comes back as the digest it pins; and the digest
// rule holds after the step.
func TestVolumeImageIsAdmitted(t *testing.T) {
	var seen []AdmitRequest
	var probes []v1.Sandbox
	catalog := func(_ context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = append(seen, req)
		probes = append(probes, *in)
		out := *in
		switch in.Spec.Image {
		case "grader":
			out.Spec.Image = pinned
		case "refused":
			return nil, nil, &Error{Code: CodeAdmissionRefused, Detail: "image_not_in_catalog: refused"}
		case "floating":
			out.Spec.Image = "registry.example.com/grader:latest"
		case "nothing":
			out.Spec.Image = ""
		case "broken":
			return nil, nil, errors.New("the step failed")
		}
		// A step that knows nothing of volumes defaults sandbox fields too;
		// none of them reaches the volume.
		out.Spec.Resources.CPU = "1"
		return &out, []string{"a warning about a sandbox"}, nil
	}
	o := volumeOptions()
	o.Admit = catalog
	o.Actor = Actor{Subject: "https://issuer.example.com|alice"}
	o.RequestID = "req_volume"

	obj := aVolume()
	obj.Metadata.Labels = map[string]string{"team": "evals"}
	obj.Spec.Source.Image = "grader"
	got, err := ResolveVolume(t.Context(), &obj, o)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Source.Image != pinned || v1.ImageDigest(got.Spec.Source.Image) == "" {
		t.Fatalf("stored image = %q, want the pinned reference the step answered", got.Spec.Source.Image)
	}
	probe := probes[0]
	if probe.Kind != v1.KindSandbox || probe.Spec.Image != "grader" || probe.Spec.Environment != "default" ||
		probe.Metadata.Labels["team"] != "evals" || probe.Metadata.Name != "" || probe.Spec.Resources != (v1.Resources{}) {
		t.Fatalf("the step was asked about %+v, want a Sandbox naming the environment, the labels and the image alone", probe)
	}
	req := seen[0]
	if req.Volume == nil || req.Volume.Metadata.Name != "grader" || req.Action != "create" ||
		req.Environment == nil || req.Environment.Metadata.Name != "default" || req.RequestID != "req_volume" {
		t.Fatalf("the request was %+v, want the volume beside the probe", req)
	}

	for _, tc := range []struct{ image, code string }{
		{"refused", CodeAdmissionRefused},
		{"floating", "invalid_field"},
		{"nothing", CodeAdmissionRefused},
		{"broken", CodeAdmissionRefused},
	} {
		obj := aVolume()
		obj.Spec.Source.Image = tc.image
		if got := volumeRefusal(t, obj, o); got.Code != tc.code {
			t.Errorf("%s: got %s, want %s", tc.image, got.Code, tc.code)
		}
	}

	// An empty volume names no image and asks no step.
	asked := len(seen)
	empty := aVolume()
	empty.Spec.Source = v1.VolumeSource{}
	if _, err := ResolveVolume(t.Context(), &empty, o); err != nil || len(seen) != asked {
		t.Fatalf("an empty volume asked the step: %v, %d calls", err, len(seen)-asked)
	}
}

// TestVolumeUpdate: the labels, the annotations and retain change; every
// other field is immutable; a size never shrinks and does not grow on this
// server; the same size in another unit keeps the stored spelling; and an
// image named as the caller first wrote it is admitted again and accepted
// when it pins the same reference.
func TestVolumeUpdate(t *testing.T) {
	o := volumeOptions()
	o.Admit = func(_ context.Context, in *v1.Sandbox, _ AdmitRequest) (*v1.Sandbox, []string, error) {
		out := *in
		if in.Spec.Image == "grader" {
			out.Spec.Image = pinned
		}
		return &out, nil, nil
	}
	created := aVolume()
	created.Spec.Source.Image = "grader"
	stored, err := ResolveVolume(t.Context(), &created, o)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status = v1.VolumeStatus{ID: "vol_01J9", Owner: "alice", Phase: v1.VolumeAvailable}
	o.Existing = stored

	again := aVolume()
	again.Spec.Source.Image = "grader"
	again.Spec.Size = "1024Mi"
	off := false
	again.Spec.Retain = &off
	again.Metadata.Labels = map[string]string{"team": "evals"}
	got, err := ResolveVolume(t.Context(), &again, o)
	if err != nil {
		t.Fatalf("a re-apply of the same manifest with a new retain and labels: %v", err)
	}
	if got.Spec.Source.Image != pinned || got.Spec.Size != "1Gi" || got.Spec.Retains() || got.Metadata.Labels["team"] != "evals" {
		t.Fatalf("updated %+v, want the pinned image, the stored size spelling and retain false", got.Spec)
	}

	for _, tc := range []struct {
		name string
		with func(*v1.Volume)
		code string
		path string
	}{
		{"another access", func(v *v1.Volume) { v.Spec.Access = v1.VolumeAccessSingle }, "immutable_field", pathVolumeAccess},
		{"another image", func(v *v1.Volume) {
			v.Spec.Source.Image = strings.Replace(pinned, "0123", "4567", 1)
		}, "immutable_field", pathVolumeSource},
		{"an empty source", func(v *v1.Volume) { v.Spec.Source = v1.VolumeSource{} }, "immutable_field", pathVolumeSource},
		{"a smaller size", func(v *v1.Volume) { v.Spec.Size = "512Mi" }, "invalid_field", pathVolumeSize},
		{"a larger size", func(v *v1.Volume) { v.Spec.Size = "2Gi" }, "capability_unsupported", pathVolumeSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := aVolume()
			tc.with(&obj)
			got := volumeRefusal(t, obj, o)
			if got.Code != tc.code || (got.Path != tc.path && !slices.Contains(got.Paths, tc.path)) {
				t.Fatalf("got %s at %s %v, want %s at %s", got.Code, got.Path, got.Paths, tc.code, tc.path)
			}
		})
	}
}

// TestDecodeVolume: a Volume decodes from JSON and YAML alike, drops the
// status a client sent, and is refused as another kind.
func TestDecodeVolume(t *testing.T) {
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Volume","metadata":{"name":"v"},"spec":{"size":"1Gi"},"status":{"phase":"Available"}}`
	got, err := DecodeVolume([]byte(body), MediaJSON)
	if err != nil || got.Spec.Size != "1Gi" || got.Status.Phase != "" {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	yaml := "apiVersion: cella.latere.ai/v1beta1\nkind: Volume\nmetadata: {name: v}\nspec: {size: 1Gi, access: shared-read}\n"
	if got, err := DecodeVolume([]byte(yaml), MediaYAML); err != nil || got.Spec.Access != v1.VolumeAccessSharedRead {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	if _, err := DecodeVolume([]byte(strings.Replace(body, `"Volume"`, `"Secret"`, 1)), MediaJSON); err == nil {
		t.Fatal("a Secret decoded as a Volume")
	}
	var known *Error
	if _, err := DecodeVolume([]byte(strings.Replace(body, `"size"`, `"capacity"`, 1)), MediaJSON); !errors.As(err, &known) || known.Code != "unknown_field" {
		t.Fatalf("an unknown field: %v", err)
	}
}

// volumesOptions resolves a sandbox against an environment that keeps
// volumes, with three volumes the caller may attach.
func volumesOptions(volumes ...v1.Volume) Options {
	o := environmentOptions(volumeEnvironment("default"))
	o.Lookup = WithVolumes(o.Lookup, func(_ context.Context, nameOrID string) (*v1.Volume, error) {
		for _, v := range volumes {
			if v.Metadata.Name == nameOrID || v.Status.ID == nameOrID {
				out := v
				return &out, nil
			}
		}
		return nil, ErrNotFound
	})
	return o
}

func storedVolume(name, id, access, phase string) v1.Volume {
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume, Metadata: v1.Metadata{Name: name},
		Spec:   v1.VolumeSpec{Environment: "default", Size: "1Gi", Access: access, Source: v1.VolumeSource{Kind: v1.VolumeSourceEmpty}},
		Status: v1.VolumeStatus{ID: id, Owner: "alice", Phase: phase},
	}
}

// TestSandboxVolumeRules holds spec.volumes to its rules: a name, a volume and
// a clean path per mount, names and paths that do not collide or nest, no
// mount under the control plane's own path or inside the workspace, a volume
// that exists, lives in the sandbox's environment and is Available or being
// filled, no volume twice, no write to a shared read-only volume, an
// environment that keeps volumes, and no change of mounts on an update.
func TestSandboxVolumeRules(t *testing.T) {
	volumes := []v1.Volume{
		storedVolume("state", "vol_STATE", v1.VolumeAccessSingle, v1.VolumeAvailable),
		storedVolume("tools", "vol_TOOLS", v1.VolumeAccessSharedRead, v1.VolumeAvailable),
		storedVolume("filling", "vol_FILLING", v1.VolumeAccessSharedRead, v1.VolumePending),
		storedVolume("broken", "vol_BROKEN", v1.VolumeAccessSharedRead, v1.VolumeFailed),
	}
	elsewhere := storedVolume("far", "vol_FAR", v1.VolumeAccessSingle, v1.VolumeAvailable)
	elsewhere.Spec.Environment = "other"
	volumes = append(volumes, elsewhere)
	mount := func(name, path, volume string, readOnly bool) v1.VolumeMount {
		return v1.VolumeMount{Name: name, Path: path, Volume: volume, ReadOnly: readOnly}
	}
	for _, tc := range []struct {
		name   string
		mounts []v1.VolumeMount
		code   string
		path   string
	}{
		{"no name", []v1.VolumeMount{mount("", "/data", "state", false)}, "missing_field", "spec.volumes[0].name"},
		{"a name that is no label", []v1.VolumeMount{mount("Data", "/data", "state", false)}, "invalid_field", "spec.volumes[0].name"},
		{"two mounts of one name", []v1.VolumeMount{mount("a", "/a", "state", false), mount("a", "/b", "tools", true)}, "invalid_field", "spec.volumes[1].name"},
		{"no volume", []v1.VolumeMount{mount("a", "/data", "", false)}, "missing_field", "spec.volumes[0].volume"},
		{"no path", []v1.VolumeMount{mount("a", "", "state", false)}, "missing_field", "spec.volumes[0].path"},
		{"a relative path", []v1.VolumeMount{mount("a", "data", "state", false)}, "invalid_field", "spec.volumes[0].path"},
		{"the root", []v1.VolumeMount{mount("a", "/", "state", false)}, "invalid_field", "spec.volumes[0].path"},
		{"the control plane's own path", []v1.VolumeMount{mount("a", "/run/cella/x", "state", false)}, "path_conflict", "spec.volumes[0].path"},
		{"one mount inside another", []v1.VolumeMount{mount("a", "/data", "state", false), mount("b", "/data/tools", "tools", true)}, "path_conflict", "spec.volumes[1].path"},
		{"a mount inside the workspace", []v1.VolumeMount{mount("a", "/workspace/data", "state", false)}, "path_conflict", "spec.workspace.path"},
		{"a volume that does not exist", []v1.VolumeMount{mount("a", "/data", "absent", false)}, "not_found", "spec.volumes[0].volume"},
		{"a volume of another environment", []v1.VolumeMount{mount("a", "/data", "far", false)}, "invalid_field", "spec.volumes[0].volume"},
		{"a volume that failed", []v1.VolumeMount{mount("a", "/data", "broken", true)}, "invalid_field", "spec.volumes[0].volume"},
		{"one volume twice", []v1.VolumeMount{mount("a", "/a", "tools", true), mount("b", "/b", "vol_TOOLS", true)}, "invalid_field", "spec.volumes[1].volume"},
		{"a write to a shared read-only volume", []v1.VolumeMount{mount("a", "/opt/tools", "tools", false)}, "volume_busy", "spec.volumes[0].volume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			obj.Spec.Volumes = tc.mounts
			got := refusal(t, obj, volumesOptions(volumes...))
			if got.Code != tc.code || (got.Path != tc.path && !slices.Contains(got.Paths, tc.path)) {
				t.Fatalf("got %s at %s %v, want %s at %s", got.Code, got.Path, got.Paths, tc.code, tc.path)
			}
		})
	}

	obj := sandbox()
	obj.Spec.Volumes = []v1.VolumeMount{
		mount("state", "/data", "state", false),
		mount("tools", "/opt/tools", "vol_TOOLS", true),
		mount("filling", "/opt/grader", "filling", true),
	}
	got := resolve(t, obj, volumesOptions(volumes...))
	if len(got.Volumes) != 3 || got.Volumes[1].Status.ID != "vol_TOOLS" || got.Volumes[2].Status.Phase != v1.VolumePending {
		t.Fatalf("resolved volumes %+v, want the three in the manifest's order", got.Volumes)
	}

	// A lookup that cannot decide fails closed, a decided refusal keeps its
	// own code, and a lookup with no volume half finds nothing.
	for _, tc := range []struct {
		name string
		find VolumeFunc
		code string
	}{
		{"unavailable", func(context.Context, string) (*v1.Volume, error) { return nil, errors.New("timed out") }, "authorizer_unavailable"},
		{"decided", func(context.Context, string) (*v1.Volume, error) {
			return nil, &Error{Code: "forbidden", Path: "spec.volumes[0].volume"}
		}, "forbidden"},
		{"no answer", func(context.Context, string) (*v1.Volume, error) { return nil, nil }, "not_found"},
		{"no volume half", nil, "not_found"},
	} {
		o := environmentOptions(volumeEnvironment("default"))
		o.Lookup = WithVolumes(o.Lookup, tc.find)
		obj := sandbox()
		obj.Spec.Volumes = []v1.VolumeMount{mount("state", "/data", "state", false)}
		if got := refusal(t, obj, o); got.Code != tc.code {
			t.Errorf("%s: got %s, want %s", tc.name, got.Code, tc.code)
		}
	}

	// An environment that keeps no volumes refuses the field.
	o := volumesOptions(volumes...)
	o.Lookup = WithVolumes(FixedEnvironment(container("default")), func(context.Context, string) (*v1.Volume, error) {
		out := volumes[0]
		return &out, nil
	})
	single := sandbox()
	single.Spec.Volumes = []v1.VolumeMount{mount("state", "/data", "state", false)}
	if got := refusal(t, single, o); got.Code != "capability_unsupported" || got.Path != pathVolumes {
		t.Fatalf("got %s at %s, want capability_unsupported at spec.volumes", got.Code, got.Path)
	}

	// The mounts of a sandbox that exists are the ones it was created with.
	existing := got.Sandbox
	o = volumesOptions(volumes...)
	o.Existing = &existing
	changed := existing
	changed.Spec.Volumes = changed.Spec.Volumes[:1]
	if got := refusal(t, changed, o); got.Code != "capability_unsupported" || got.Path != pathVolumes {
		t.Fatalf("got %s at %s, want capability_unsupported at spec.volumes", got.Code, got.Path)
	}
	if _, err := Resolve(t.Context(), &existing, o); err != nil {
		t.Fatalf("an update that keeps the mounts: %v", err)
	}
	// A volume that failed after the sandbox was created does not refuse an
	// update that keeps the mounts, such as a change of labels.
	failed := slices.Clone(volumes)
	for i := range failed {
		failed[i].Status.Phase = v1.VolumeLost
	}
	o = volumesOptions(failed...)
	o.Existing = &existing
	relabeled := existing
	relabeled.Metadata.Labels = map[string]string{"team": "evals"}
	if _, err := Resolve(t.Context(), &relabeled, o); err != nil {
		t.Fatalf("an update of a sandbox whose volume was lost since: %v", err)
	}
}

// TestVolumeBoundary is rule 4 of the boundary check: a child mounts only
// volumes its parent mounts, by the name its parent wrote or the id its
// parent was bound to, and never writes one its parent holds read-only.
func TestVolumeBoundary(t *testing.T) {
	parent := parentSandbox()
	parent.Spec.Volumes = []v1.VolumeMount{
		{Name: "state", Path: "/data", Volume: "state"},
		{Name: "tools", Path: "/opt/tools", Volume: "tools", ReadOnly: true},
	}
	parent.Status.Volumes = []v1.VolumeAttachment{{Name: "state", Volume: "vol_STATE"}, {Name: "tools", Volume: "vol_TOOLS"}}
	volumes := []v1.Volume{
		storedVolume("state", "vol_STATE", v1.VolumeAccessSingle, v1.VolumeAvailable),
		storedVolume("tools", "vol_TOOLS", v1.VolumeAccessSingle, v1.VolumeAvailable),
		storedVolume("other", "vol_OTHER", v1.VolumeAccessSingle, v1.VolumeAvailable),
	}
	options := func() Options {
		o := spawnOptions(parent)
		env := volumeEnvironment("default")
		env.Status.Capabilities.Mesh = true
		o.Lookup = WithVolumes(WithSecrets(FixedEnvironment(env), nil), volumesOptions(volumes...).Lookup.Volume)
		o.Now = func() time.Time { return fixedNow }
		return o
	}
	for _, tc := range []struct {
		name  string
		mount v1.VolumeMount
		path  string
	}{
		{"a volume the parent does not mount", v1.VolumeMount{Name: "x", Path: "/x", Volume: "other", ReadOnly: true}, "spec.volumes[0].volume"},
		{"a write the parent does not hold", v1.VolumeMount{Name: "x", Path: "/x", Volume: "tools"}, "spec.volumes[0].readOnly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := childSandbox()
			obj.Spec.Volumes = []v1.VolumeMount{tc.mount}
			got := refusal(t, obj, options())
			if got.Code != "boundary_exceeded" || !slices.Contains(got.Paths, tc.path) {
				t.Fatalf("got %s at %v, want boundary_exceeded at %s", got.Code, got.Paths, tc.path)
			}
		})
	}
	obj := childSandbox()
	obj.Spec.Volumes = []v1.VolumeMount{
		{Name: "tools", Path: "/opt/tools", Volume: "vol_TOOLS", ReadOnly: true},
		{Name: "state", Path: "/data", Volume: "state", ReadOnly: true},
	}
	resolve(t, obj, options())
}
