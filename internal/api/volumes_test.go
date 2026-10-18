// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// pinnedGrader is a digest-pinned image reference.
const pinnedGrader = "registry.example.com/grader@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// setupVolumes is the recorded fixture over a driver that keeps volumes in
// memory, with the volume loop running beside the scheduler, as cellad runs
// it. A policy replaces the owner policy where one is given.
func setupVolumes(t *testing.T, policy authz.Authorizer) (*fixture, *runtimetest.Volumes) {
	t.Helper()
	var vd *runtimetest.Volumes
	o := recordedOptions{wrap: func(d runtime.Driver) runtime.Driver {
		vd = runtimetest.NewVolumes(d)
		return vd
	}}
	if policy != nil {
		o.configure = func(options *Options) { options.Authorizer = auth.NewAuthorizer(policy) }
	}
	f := setupRecordedWith(t, o).fixture
	runVolumes(t, f.c)
	return f, vd
}

// runVolumes runs the controller's volume loop for the life of the test. It
// stops before the controller closes.
func runVolumes(t *testing.T, c *controller.Controller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.RunVolumes(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// volumeBody is one Volume manifest as a caller writes it.
func volumeBody(name, access, image string) string {
	source := `{"kind":"empty"}`
	if image != "" {
		source = `{"kind":"image","image":"` + image + `"}`
	}
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Volume","metadata":{"name":"` + name + `"},` +
		`"spec":{"size":"1Gi","access":"` + access + `","source":` + source + `}}`
}

// mountBody is a sandbox that mounts one volume at /data.
func mountBody(name, volume string, readOnly bool) string {
	ro := "false"
	if readOnly {
		ro = "true"
	}
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
		`"spec":{"volumes":[{"name":"data","path":"/data","volume":"` + volume + `","readOnly":` + ro + `}]}}`
}

func decodeVolume(t *testing.T, body []byte) v1.Volume {
	t.Helper()
	var got v1.Volume
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the answer is %s: %v", body, err)
	}
	return got
}

// available waits until the volume reads Available.
func (f *fixture) available(key string) v1.Volume {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := decodeVolume(f.t, f.request(http.MethodGet, "/v1/volumes/"+key, f.alice, "", http.StatusOK))
		if got.Status.Phase == v1.VolumeAvailable {
			return got
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the volume %s is %s after ten seconds", key, got.Status.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestVolumeRoutes: a volume is applied, read and deleted through the grammar
// every kind shares: a create answers 201 Pending with its address and the
// loop fills it, a second apply updates it, a body naming another object is
// refused, the owner reads it by name and by id and nobody else does, the
// list is narrowed to what the caller may read, and a delete answers the
// volume Deleting and leaves nothing to read.
func TestVolumeRoutes(t *testing.T) {
	f, vd := setupVolumes(t, nil)
	res := f.proxied(http.MethodPut, "/v1/volumes/grader", f.alice,
		strings.NewReader(volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader)), http.Header{"Content-Type": {jsonMedia}})
	var created v1.Volume
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated || created.Status.Phase != v1.VolumePending ||
		!strings.HasSuffix(res.Header.Get("Location"), "/v1/volumes/"+created.Status.ID) {
		t.Fatalf("the create answered %d %+v at %q", res.StatusCode, created.Status, res.Header.Get("Location"))
	}
	if created.Spec.Retain == nil || !*created.Spec.Retain || created.Spec.Environment != "default" {
		t.Fatalf("the create defaulted %+v", created.Spec)
	}
	got := f.available(created.Status.ID)
	if got.Status.Capacity != "1Gi" || got.Status.AttachedTo == nil || len(got.Status.AttachedTo) != 0 {
		t.Fatalf("the filled volume reads %+v", got.Status)
	}
	if spec, held := vd.Spec(created.Status.ID); !held || spec.Source.Image != pinnedGrader {
		t.Fatalf("the driver was asked for %+v", spec)
	}

	labeled := strings.Replace(volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader),
		`"metadata":{"name":"grader"}`, `"metadata":{"name":"grader","labels":{"team":"evals"}}`, 1)
	updated := decodeVolume(t, f.request(http.MethodPut, "/v1/volumes/grader", f.alice, labeled, http.StatusOK))
	if updated.Metadata.Labels["team"] != "evals" || updated.Status.ID != created.Status.ID || updated.Status.Phase != v1.VolumeAvailable {
		t.Fatalf("the update answered %+v", updated)
	}
	f.request(http.MethodPut, "/v1/volumes/grader", f.alice,
		volumeBody("grader", v1.VolumeAccessSingle, pinnedGrader), http.StatusConflict)
	f.request(http.MethodPut, "/v1/volumes/other", f.alice,
		volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader), http.StatusBadRequest)
	f.request(http.MethodGet, "/v1/volumes/grader", f.bob, "", http.StatusNotFound)

	var page struct {
		Items []v1.Volume `json:"items"`
		Next  string      `json:"next"`
	}
	if err := json.Unmarshal(f.request(http.MethodGet, "/v1/volumes?label=team%3Devals", f.alice, "", http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Status.ID != created.Status.ID {
		t.Fatalf("alice lists %+v", page.Items)
	}
	if err := json.Unmarshal(f.request(http.MethodGet, "/v1/volumes", f.bob, "", http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("bob lists %+v", page.Items)
	}
	if err := json.Unmarshal(f.request(http.MethodGet, "/v1/volumes?label=team%3Dother", f.alice, "", http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("a selector that names nothing lists %+v", page.Items)
	}

	f.request(http.MethodDelete, "/v1/volumes/grader", f.bob, "", http.StatusNotFound)
	deleted := decodeVolume(t, f.request(http.MethodDelete, "/v1/volumes/"+created.Status.ID, f.alice, "", http.StatusOK))
	if deleted.Status.Phase != v1.VolumeDeleting {
		t.Fatalf("the delete answered %+v", deleted.Status)
	}
	f.request(http.MethodGet, "/v1/volumes/grader", f.alice, "", http.StatusNotFound)
}

// TestASandboxMountsAVolume: a sandbox mounts a volume read-only, the driver
// is given the mount, the volume names the sandbox in attachedTo and cannot
// be deleted under it, a read-write mount of a shared read-only volume is
// volume_busy, and once the sandbox is gone the volume can be deleted.
func TestASandboxMountsAVolume(t *testing.T) {
	f, vd := setupVolumes(t, nil)
	f.request(http.MethodPut, "/v1/volumes/grader", f.alice,
		volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader), http.StatusCreated)
	vol := f.available("grader")

	var obj v1.Sandbox
	if err := json.Unmarshal(f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice,
		mountBody("check", "grader", true), http.StatusCreated), &obj); err != nil {
		t.Fatal(err)
	}
	mounts := vd.Mounts(obj.Status.ID)
	if len(mounts) != 1 || mounts[0].VolumeID != vol.Status.ID || !mounts[0].ReadOnly || mounts[0].Path != "/data" {
		t.Fatalf("the driver was given %+v", mounts)
	}
	if len(obj.Status.Volumes) != 1 || !obj.Status.Volumes[0].Attached || obj.Status.Volumes[0].Volume != vol.Status.ID {
		t.Fatalf("status.volumes = %+v", obj.Status.Volumes)
	}
	held := decodeVolume(t, f.request(http.MethodGet, "/v1/volumes/grader", f.alice, "", http.StatusOK))
	if len(held.Status.AttachedTo) != 1 || held.Status.AttachedTo[0] != obj.Status.ID {
		t.Fatalf("attachedTo = %v", held.Status.AttachedTo)
	}
	body := f.request(http.MethodDelete, "/v1/volumes/grader", f.alice, "", http.StatusConflict)
	if !strings.Contains(string(body), "volume_busy") {
		t.Fatalf("the delete under a sandbox answered %s", body)
	}
	body = f.request(http.MethodPost, "/v1/sandboxes", f.alice, mountBody("writer", "grader", false), http.StatusConflict)
	if !strings.Contains(string(body), "volume_busy") {
		t.Fatalf("a write to a shared read-only volume answered %s", body)
	}
	body = f.request(http.MethodPost, "/v1/sandboxes", f.alice,
		strings.Replace(mountBody("nested", "grader", true), `"path":"/data"`, `"path":"/workspace/data"`, 1), http.StatusBadRequest)
	if !strings.Contains(string(body), "path_conflict") {
		t.Fatalf("a mount inside the workspace answered %s", body)
	}
	f.request(http.MethodPost, "/v1/sandboxes", f.alice, mountBody("absent", "nothing", true), http.StatusNotFound)

	f.request(http.MethodDelete, "/v1/sandboxes/"+obj.Status.ID, f.alice, "", http.StatusAccepted)
	f.request(http.MethodDelete, "/v1/volumes/grader", f.alice, "", http.StatusOK)
}

// TestAttachByIdUnderTheAuthorizer: a vol_ id in volumes[].volume attaches
// another subject's volume when the authorizer allows volume.attach, and is
// not_found when it does not, so a refusal does not say the volume exists.
func TestAttachByIdUnderTheAuthorizer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy authz.Authorizer
		status int
	}{
		{"the owner policy", nil, http.StatusNotFound},
		{"an authorizer that allows the attach", allowing(authorizerAttach), http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := setupVolumes(t, tc.policy)
			f.request(http.MethodPut, "/v1/volumes/grader", f.alice,
				volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader), http.StatusCreated)
			vol := f.available("grader")
			f.request(http.MethodPost, "/v1/sandboxes", f.bob, mountBody("check", vol.Status.ID, true), tc.status)
			// A name is the caller's own namespace, whatever the authorizer.
			f.request(http.MethodPost, "/v1/sandboxes", f.bob, mountBody("by-name", "grader", true), http.StatusNotFound)
		})
	}
}

// authorizerAttach is the action TestAttachByIdUnderTheAuthorizer opens.
const authorizerAttach = "volume.attach"

// allowing is the owner policy with one action allowed whoever asks.
func allowing(action string) authz.Authorizer {
	return allowingPolicy{inner: &auth.OwnerPolicy{DefaultEnvironment: "default"}, action: action}
}

type allowingPolicy struct {
	inner  authz.Authorizer
	action string
}

func (p allowingPolicy) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	if req.Action == p.action {
		return authz.Decision{Allow: true}, nil
	}
	return p.inner.Authorize(ctx, req)
}

// TestAVolumeImagePassesAdmission: the image a volume is filled from passes
// the admission step, which answers the pinned reference a catalog name
// resolves to and is what the volume stores; a refusal is admission_refused
// with the step's reason; and with no step an image that is not pinned by
// digest is invalid_field.
func TestAVolumeImagePassesAdmission(t *testing.T) {
	f, _ := setupVolumes(t, nil)
	f.request(http.MethodPut, "/v1/volumes/tagged", f.alice,
		volumeBody("tagged", v1.VolumeAccessSharedRead, "registry.example.com/grader:1"), http.StatusBadRequest)

	var asked []manifest.AdmitRequest
	admitting(f, func(_ context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		asked = append(asked, req)
		if in.Spec.Image != "grader" {
			return nil, nil, &manifest.Error{Code: manifest.CodeAdmissionRefused, Detail: "image_not_in_catalog: " + in.Spec.Image}
		}
		out := *in
		out.Spec.Image = pinnedGrader
		return &out, nil, nil
	})
	created := decodeVolume(t, f.request(http.MethodPut, "/v1/volumes/grader", f.alice,
		volumeBody("grader", v1.VolumeAccessSharedRead, "grader"), http.StatusCreated))
	if created.Spec.Source.Image != pinnedGrader {
		t.Fatalf("the volume stores %q, want the pinned reference", created.Spec.Source.Image)
	}
	if len(asked) != 1 || asked[0].Volume == nil || asked[0].Volume.Metadata.Name != "grader" || asked[0].Actor.Sub != "alice" {
		t.Fatalf("the step was asked %+v", asked)
	}
	// The same manifest applied again names the image as it first did and
	// is accepted, since it pins the reference the volume holds.
	f.request(http.MethodPut, "/v1/volumes/grader", f.alice, volumeBody("grader", v1.VolumeAccessSharedRead, "grader"), http.StatusOK)
	body := f.request(http.MethodPut, "/v1/volumes/outside", f.alice,
		volumeBody("outside", v1.VolumeAccessSharedRead, "registry.example.com/other@sha256:"+strings.Repeat("a", 64)), 422)
	if !strings.Contains(string(body), "admission_refused") || !strings.Contains(string(body), "image_not_in_catalog") {
		t.Fatalf("the refusal answered %s", body)
	}
}

// TestVolumesRefusedWhereTheyAreNotKept: an environment whose driver keeps no
// volumes refuses a volume and a mount with capability_unsupported.
func TestVolumesRefusedWhereTheyAreNotKept(t *testing.T) {
	f := setupRecorded(t).fixture
	body := f.request(http.MethodPut, "/v1/volumes/grader", f.alice,
		volumeBody("grader", v1.VolumeAccessSharedRead, pinnedGrader), 422)
	if !strings.Contains(string(body), "capability_unsupported") {
		t.Fatalf("the create answered %s", body)
	}
	body = f.request(http.MethodPost, "/v1/sandboxes", f.alice, mountBody("check", "grader", true), 422)
	if !strings.Contains(string(body), "capability_unsupported") {
		t.Fatalf("the mount answered %s", body)
	}
}
