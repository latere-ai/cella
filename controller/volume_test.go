// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/runtimetest"
)

// pinnedImage is a digest-pinned reference, the one form an image source is
// stored in.
const pinnedImage = "registry.example.com/grader@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// volumeController is a controller over the native driver wrapped with
// volumes kept in memory, on a snapshot store, with every act recorded.
func volumeController(t *testing.T) (*Controller, *runtimetest.Volumes, *actRecorder, Options) {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	vd := runtimetest.NewVolumes(d)
	rec := &actRecorder{}
	o := Options{DataDir: t.TempDir(), Driver: vd, Environment: "default", Events: rec}
	return openController(t, o), vd, rec, o
}

// aVolume is a resolved Volume as the API hands one to the controller.
func aVolume(name, access string, source v1.VolumeSource) v1.Volume {
	retain := true
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume, Metadata: v1.Metadata{Name: name},
		Spec: v1.VolumeSpec{Environment: "default", Size: "1Gi", Access: access, Source: source, Retain: &retain},
	}
}

var imageSource = v1.VolumeSource{Kind: v1.VolumeSourceImage, Image: pinnedImage}

// filled creates a volume and runs one pass of the volume loop to its end.
func filled(t *testing.T, c *Controller, obj v1.Volume) v1.Volume {
	t.Helper()
	created, err := c.CreateVolume(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	got, err := c.GetVolume(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// fillPass is one pass of the volume loop, waited out.
func fillPass(t *testing.T, c *Controller) {
	t.Helper()
	c.FillVolumes(t.Context())
	c.fillWork.Wait()
}

// mountingVolume is a resolved sandbox that mounts one volume.
func mountingVolume(name, volume string, readOnly bool) v1.Sandbox {
	obj := workspace()
	obj.Metadata.Name = name
	obj.Spec.Volumes = []v1.VolumeMount{{Name: "data", Path: "/data", Volume: volume, ReadOnly: readOnly}}
	return obj
}

// TestFillLifecycle: a volume is Pending from its create until the driver
// reports its fill done, and Available after; a fill still running stays
// Pending across passes; a fill that failed before the driver returned, and a
// source the driver cannot apply, are Failed with the reason; and a pass after
// a restart reads a volume the driver already holds.
func TestFillLifecycle(t *testing.T) {
	c, vd, _, o := volumeController(t)
	created, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status.Phase != v1.VolumePending || created.Status.ID == "" || created.Status.AttachedTo == nil {
		t.Fatalf("created %+v, want Pending with an id and an empty attachedTo", created.Status)
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a second volume of one name: %v", err)
	}
	fillPass(t, c)
	got, err := c.GetVolume(t.Context(), "grader", "alice")
	if err != nil || got.Status.Phase != v1.VolumeAvailable || got.Status.Capacity != "1Gi" {
		t.Fatalf("after the fill: %+v, %v", got.Status, err)
	}
	if spec, held := vd.Spec(created.Status.ID); !held || spec.Source.Image != pinnedImage || spec.Access != v1.VolumeAccessSharedRead {
		t.Fatalf("the driver was asked for %+v", spec)
	}

	// A driver that fills after it returns reports Pending until it is done.
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{Phase: driver.VolumePending}, nil
	}
	slow, err := c.CreateVolume(t.Context(), aVolume("slow", v1.VolumeAccessSingle, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	fillPass(t, c)
	if got, _ := c.GetVolume(t.Context(), slow.Status.ID, "alice"); got.Status.Phase != v1.VolumePending {
		t.Fatalf("a fill still running is %s", got.Status.Phase)
	}
	if n := vd.Creates(slow.Status.ID); n != 2 {
		t.Fatalf("CreateVolume was asked %d times over two passes, want once per pass", n)
	}
	vd.Set(driver.VolumeState{ID: slow.Status.ID, Phase: driver.VolumeAvailable, Capacity: "2Gi"})
	fillPass(t, c)
	if got, _ := c.GetVolume(t.Context(), slow.Status.ID, "alice"); got.Status.Phase != v1.VolumeAvailable || got.Status.Capacity != "2Gi" {
		t.Fatalf("a fill that finished is %+v", got.Status)
	}

	// Failures before the driver returns.
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"too large", &driver.FillError{Reason: driver.ReasonSourceTooLarge, Message: "the image holds 3Gi"}, driver.ReasonSourceTooLarge},
		{"unsupported", driver.ErrUnsupported, driver.ReasonSourceUnsupported},
	} {
		vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) { return driver.VolumeState{}, tc.err }
		obj, err := c.CreateVolume(t.Context(), aVolume("failing-"+tc.reason, v1.VolumeAccessSingle, imageSource), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		fillPass(t, c)
		got, _ := c.GetVolume(t.Context(), obj.Status.ID, "alice")
		if got.Status.Phase != v1.VolumeFailed || got.Status.Reason != tc.reason || got.Status.Message == "" {
			t.Errorf("%s: %+v, want Failed %s with a message", tc.name, got.Status, tc.reason)
		}
	}

	// A driver that cannot be reached leaves the volume Pending for the next
	// pass.
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{}, errors.New("the engine did not answer")
	}
	retried, err := c.CreateVolume(t.Context(), aVolume("retried", v1.VolumeAccessSingle, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	if got, _ := c.GetVolume(t.Context(), retried.Status.ID, "alice"); got.Status.Phase != v1.VolumePending {
		t.Fatalf("a fill the driver did not take is %s", got.Status.Phase)
	}

	// A process that restarts with a volume Pending and the driver holding
	// it filled reads it on its first pass.
	vd.Fill = nil
	vd.Set(driver.VolumeState{ID: retried.Status.ID, Phase: driver.VolumeAvailable, Capacity: "1Gi"})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again := openController(t, o)
	if got, _ := again.GetVolume(t.Context(), retried.Status.ID, "alice"); got.Status.Phase != v1.VolumePending {
		t.Fatalf("the reopened store holds the volume %s", got.Status.Phase)
	}
	fillPass(t, again)
	if got, _ := again.GetVolume(t.Context(), retried.Status.ID, "alice"); got.Status.Phase != v1.VolumeAvailable {
		t.Fatalf("after the restart's pass: %s", got.Status.Phase)
	}
	if len(again.ListVolumes()) != 5 {
		t.Fatalf("the reopened store lists %d volumes, want 5", len(again.ListVolumes()))
	}
}

// TestAccessModes is spec 019's access table at the controller, which is the
// authoritative attach: a single volume takes one read-write attach or any
// number of read-only ones and never both, and a shared-read volume takes
// read-only attaches alone. A refusal writes nothing.
func TestAccessModes(t *testing.T) {
	c, _, _, _ := volumeController(t)
	state := filled(t, c, aVolume("state", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	tools := filled(t, c, aVolume("tools", v1.VolumeAccessSharedRead, imageSource))

	if _, err := realized(t.Context(), c, mountingVolume("writer", "state", false), "alice", 0); err != nil {
		t.Fatal(err)
	}
	before := len(c.List())
	for _, tc := range []struct {
		name     string
		volume   string
		readOnly bool
	}{
		{"a second writer", "state", false},
		{"a reader beside a writer", "state", true},
		{"a writer of a shared read-only volume", tools.Status.ID, false},
	} {
		if _, err := c.Create(t.Context(), mountingVolume("refused", tc.volume, tc.readOnly), "alice", 0); !errors.Is(err, ErrVolumeBusy) {
			t.Errorf("%s: %v, want ErrVolumeBusy", tc.name, err)
		}
	}
	if len(c.List()) != before {
		t.Fatal("a refused attach wrote a sandbox")
	}
	for _, name := range []string{"reader-one", "reader-two"} {
		if _, err := realized(t.Context(), c, mountingVolume(name, tools.Status.ID, true), "alice", 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, _ := c.GetVolume(t.Context(), tools.Status.ID, "alice")
	if len(got.Status.AttachedTo) != 2 {
		t.Fatalf("attachedTo = %v, want both readers", got.Status.AttachedTo)
	}
	if got, _ := c.GetVolume(t.Context(), state.Status.ID, "alice"); len(got.Status.AttachedTo) != 1 {
		t.Fatalf("attachedTo = %v, want the writer", got.Status.AttachedTo)
	}

	// Read-only attaches of a single volume stack once no writer holds it.
	free := filled(t, c, aVolume("free", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	for _, name := range []string{"free-one", "free-two"} {
		if _, err := realized(t.Context(), c, mountingVolume(name, free.Status.ID, true), "alice", 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := c.Create(t.Context(), mountingVolume("free-writer", "free", false), "alice", 0); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("a writer beside readers: %v", err)
	}
	// A name names the caller's own volume only; another subject reaches it
	// by id alone.
	if _, err := c.Create(t.Context(), mountingVolume("stranger", "free", true), "bob", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another subject's volume by name: %v", err)
	}
}

// TestStoppedSandboxHoldsTheVolume: a stopped sandbox still holds its
// attachment, so the volume cannot be deleted under it; once the sandbox is
// deleted the volume is free.
func TestStoppedSandboxHoldsTheVolume(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	vol := filled(t, c, aVolume("state", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	obj, err := realized(t.Context(), c, mountingVolume("holder", "state", false), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if mounts := vd.Mounts(obj.Status.ID); len(mounts) != 1 || mounts[0].VolumeID != vol.Status.ID || mounts[0].Path != "/data" || mounts[0].ReadOnly {
		t.Fatalf("the driver was given %+v", mounts)
	}
	if len(obj.Status.Volumes) != 1 || !obj.Status.Volumes[0].Attached || obj.Status.Volumes[0].Volume != vol.Status.ID {
		t.Fatalf("status.volumes = %+v, want the bound volume attached", obj.Status.Volumes)
	}
	if cond := conditionOf(obj, v1.ConditionVolumesAttached); cond.Status != v1.ConditionTrue {
		t.Fatalf("VolumesAttached = %+v", cond)
	}
	if _, err := c.Act(t.Context(), obj.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteVolume(t.Context(), vol.Status.ID); !errors.Is(err, ErrVolumeBusy) {
		t.Fatalf("a delete under a stopped sandbox: %v", err)
	}
	if got, _ := c.GetVolume(t.Context(), vol.Status.ID, "alice"); !slices.Equal(got.Status.AttachedTo, []string{obj.Status.ID}) {
		t.Fatalf("attachedTo = %v, want the stopped sandbox", got.Status.AttachedTo)
	}
	if _, err := c.Act(t.Context(), obj.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := realized(t.Context(), c, mountingVolume("next", "state", false), "alice", 0); err != nil {
		t.Fatalf("a new writer once the old one is gone: %v", err)
	}
}

// TestAttachWaitsOnAvailable: a sandbox created while its volume is still
// being filled stays placed, and is created with the volume once the fill
// settles; one whose volume failed meanwhile fails with VolumesAttached False
// and nothing created.
func TestAttachWaitsOnAvailable(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{Phase: driver.VolumePending}, nil
	}
	ok, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := c.CreateVolume(t.Context(), aVolume("broken", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	waiting, err := realized(t.Context(), c, mountingVolume("waiting", "grader", true), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !placed(waiting) || len(vd.Mounts(waiting.Status.ID)) != 0 {
		t.Fatalf("a sandbox whose volume is filling is %+v and the driver holds %v", waiting.Status, vd.Mounts(waiting.Status.ID))
	}
	doomed, err := realized(t.Context(), c, mountingVolume("doomed", "broken", true), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}

	vd.Set(driver.VolumeState{ID: ok.Status.ID, Phase: driver.VolumeAvailable})
	vd.Set(driver.VolumeState{ID: bad.Status.ID, Phase: driver.VolumeFailed, Reason: driver.ReasonSourceUnreachable, Message: "the pull failed"})
	fillPass(t, c)
	started, err := finished(t.Context(), c, waiting, nil)
	if err != nil {
		t.Fatal(err)
	}
	if placed(started) || len(vd.Mounts(started.Status.ID)) != 1 || !started.Status.Volumes[0].Attached {
		t.Fatalf("after the fill: %+v, mounts %v", started.Status, vd.Mounts(started.Status.ID))
	}
	failed, err := finished(t.Context(), c, doomed, nil)
	if err == nil {
		t.Fatal("a sandbox bound to a failed volume was realized without an error")
	}
	failed, _ = c.Get(t.Context(), failed.Status.ID, "alice")
	cond := conditionOf(failed, v1.ConditionVolumesAttached)
	if failed.Status.Phase != PhaseFailed || failed.Status.Reason != ReasonCreateFailed ||
		cond.Status != v1.ConditionFalse || cond.Reason != ReasonVolumesUnavailable {
		t.Fatalf("a sandbox bound to a failed volume is %s %s with %+v", failed.Status.Phase, failed.Status.Reason, cond)
	}
	if _, err := vd.Inspect(t.Context(), failed.Status.ID); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the driver holds the failed sandbox: %v", err)
	}
}

// TestRetainAndDelete: a sandbox deleted with a retain volume leaves it
// Available; with retain false and no other attachment the volume goes with
// its last sandbox; a delete of a volume no sandbox holds removes it from the
// driver and the store.
func TestRetainAndDelete(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	kept := filled(t, c, aVolume("kept", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	scratch := aVolume("scratch", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty})
	off := false
	scratch.Spec.Retain = &off
	scratch = filled(t, c, scratch)

	one, err := realized(t.Context(), c, mountingVolume("one", "kept", false), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	two, err := realized(t.Context(), c, mountingVolume("two", "scratch", true), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	three, err := realized(t.Context(), c, mountingVolume("three", "scratch", true), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []v1.Sandbox{one, two} {
		if _, err := c.Act(t.Context(), obj.Status.ID, "delete"); err != nil {
			t.Fatal(err)
		}
	}
	fillPass(t, c)
	if got, err := c.GetVolume(t.Context(), kept.Status.ID, "alice"); err != nil || got.Status.Phase != v1.VolumeAvailable {
		t.Fatalf("the retained volume after its sandbox: %+v, %v", got.Status, err)
	}
	if got, err := c.GetVolume(t.Context(), scratch.Status.ID, "alice"); err != nil || got.Status.Phase != v1.VolumeAvailable {
		t.Fatalf("a volume another sandbox still holds went: %+v, %v", got.Status, err)
	}
	if _, err := c.Act(t.Context(), three.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetVolume(t.Context(), scratch.Status.ID, "alice"); got.Status.Phase != v1.VolumeDeleting {
		t.Fatalf("a volume that does not retain after its last sandbox is %s", got.Status.Phase)
	}
	fillPass(t, c)
	if _, err := c.GetVolume(t.Context(), scratch.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the volume outlived its last sandbox: %v", err)
	}
	if _, held := vd.Held()[scratch.Status.ID]; held {
		t.Fatal("the driver still holds the volume")
	}
	deleted, err := c.DeleteVolume(t.Context(), kept.Status.ID)
	if err != nil || deleted.Status.Phase != v1.VolumeDeleting || deleted.Status.AttachedTo == nil {
		t.Fatalf("the delete answered %+v, %v, want Deleting with an empty attachedTo", deleted.Status, err)
	}
	if _, err := c.GetVolume(t.Context(), kept.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted volume reads %v", err)
	}
	if _, held := vd.Held()[kept.Status.ID]; held {
		t.Fatal("the driver still holds the deleted volume")
	}
	if _, err := c.DeleteVolume(t.Context(), kept.Status.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second delete: %v", err)
	}
}

// TestADeleteDuringAFillRemovesWhatTheFillMade: a volume deleted while its
// fill is in the driver's hands is removed by the fill once it settles, and
// the driver is not left holding it.
func TestADeleteDuringAFillRemovesWhatTheFillMade(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	vd.Fill = func(spec driver.VolumeSpec) (driver.VolumeState, error) {
		close(entered)
		<-release
		return driver.VolumeState{Phase: driver.VolumeAvailable}, nil
	}
	obj, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	c.FillVolumes(t.Context())
	<-entered
	if got, err := c.DeleteVolume(t.Context(), obj.Status.ID); err != nil || got.Status.Phase != v1.VolumeDeleting {
		t.Fatalf("the delete answered %+v, %v", got.Status, err)
	}
	close(release)
	c.fillWork.Wait()
	if _, err := c.GetVolume(t.Context(), obj.Status.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the volume outlived its delete: %v", err)
	}
	if _, held := vd.Held()[obj.Status.ID]; held {
		t.Fatal("the driver holds what the fill made")
	}
}

// TestVolumeLost: an Available volume the driver no longer holds is Lost at
// the reaper's tick, and never recreated.
func TestVolumeLost(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	vol := filled(t, c, aVolume("state", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	vd.Lose(vol.Status.ID)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetVolume(t.Context(), vol.Status.ID, "alice"); got.Status.Phase != v1.VolumeLost {
		t.Fatalf("a volume the driver lost is %s", got.Status.Phase)
	}
	fillPass(t, c)
	if _, held := vd.Held()[vol.Status.ID]; held {
		t.Fatal("a lost volume was recreated")
	}
	if _, err := c.Create(t.Context(), mountingVolume("late", "state", false), "alice", 0); err == nil {
		t.Fatal("a sandbox was bound to a lost volume")
	}
}

// TestVolumeEvents: every transition of a volume is one act: the create, an
// update, an attach and a detach naming the sandbox and its mode, a failure
// with its reason, and the delete.
func TestVolumeEvents(t *testing.T) {
	c, vd, rec, _ := volumeController(t)
	vol := filled(t, c, aVolume("state", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	updated := vol
	updated.Metadata.Labels = map[string]string{"team": "evals"}
	if _, err := c.UpdateVolume(t.Context(), updated, vol.Status.ID); err != nil {
		t.Fatal(err)
	}
	obj, err := realized(t.Context(), c, mountingVolume("holder", "state", true), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Act(t.Context(), obj.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteVolume(t.Context(), vol.Status.ID); err != nil {
		t.Fatal(err)
	}
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{}, &driver.FillError{Reason: driver.ReasonSourceUnreachable, Message: "the pull failed"}
	}
	filled(t, c, aVolume("broken", v1.VolumeAccessSharedRead, imageSource))

	rec.mu.Lock()
	acts := slices.Clone(rec.volumes)
	rec.mu.Unlock()
	var types []string
	for _, a := range acts {
		if a.Type == MutationVolumeStatus {
			continue
		}
		types = append(types, a.Type)
		switch a.Type {
		case MutationVolumeAttached, MutationVolumeDetached:
			if a.Sandbox == nil || a.Sandbox.Status.ID != obj.Status.ID || a.Mode != VolumeModeReadOnly {
				t.Errorf("%s names %+v in %q", a.Type, a.Sandbox, a.Mode)
			}
		case MutationVolumeFailed:
			if a.Object.Status.Reason != driver.ReasonSourceUnreachable {
				t.Errorf("the failure carries %q", a.Object.Status.Reason)
			}
		}
	}
	want := []string{
		MutationVolumeCreated, MutationVolumeUpdated, MutationVolumeAttached, MutationVolumeDetached,
		MutationVolumeDeleted, MutationVolumeCreated, MutationVolumeFailed,
	}
	if !slices.Equal(types, want) {
		t.Fatalf("the acts were %v, want %v", types, want)
	}
}

// TestTheVolumeCeilingCountsTheOwnersVolumes: a create past the owner's
// ceiling is ErrVolumeQuota with the count in its detail and nothing written;
// an Available, a Failed and a Lost volume each hold a slot; another owner's
// volumes are not counted; 0 is no ceiling; and a delete gives its slot back.
func TestTheVolumeCeilingCountsTheOwnersVolumes(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	empty := v1.VolumeSource{Kind: v1.VolumeSourceEmpty}
	kept := filled(t, c, aVolume("kept", v1.VolumeAccessSingle, empty))
	lost := filled(t, c, aVolume("lost", v1.VolumeAccessSingle, empty))
	vd.Lose(lost.Status.ID)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{}, driver.ErrUnsupported
	}
	broken, err := c.CreateVolume(t.Context(), aVolume("broken", v1.VolumeAccessSingle, imageSource), "alice", 3)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	vd.Fill = nil
	for id, want := range map[string]string{kept.Status.ID: v1.VolumeAvailable, lost.Status.ID: v1.VolumeLost, broken.Status.ID: v1.VolumeFailed} {
		if got, err := c.GetVolume(t.Context(), id, "alice"); err != nil || got.Status.Phase != want {
			t.Fatalf("the volume %s is %+v, %v; want %s", id, got.Status, err, want)
		}
	}

	_, err = c.CreateVolume(t.Context(), aVolume("fourth", v1.VolumeAccessSingle, empty), "alice", 3)
	if !errors.Is(err, ErrVolumeQuota) || !strings.Contains(err.Error(), "holds 3 volumes of a ceiling of 3") {
		t.Fatalf("a create past the ceiling: %v", err)
	}
	if _, err := c.GetVolume(t.Context(), "fourth", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused create left a volume: %v", err)
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("fourth", v1.VolumeAccessSingle, empty), "bob", 1); err != nil {
		t.Fatalf("another owner's volumes counted against bob: %v", err)
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("unbounded", v1.VolumeAccessSingle, empty), "alice", 0); err != nil {
		t.Fatalf("a create with no ceiling: %v", err)
	}
	if _, err := c.DeleteVolume(t.Context(), broken.Status.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("fourth", v1.VolumeAccessSingle, empty), "alice", 4); err != nil {
		t.Fatalf("a create after a delete freed a slot: %v", err)
	}
}

// TestADeletingVolumeTakesNoSlot: a volume whose delete waits on a fill in
// flight is Deleting, and gives its slot up at once rather than when the fill
// settles.
func TestADeletingVolumeTakesNoSlot(t *testing.T) {
	c, vd, _, _ := volumeController(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		close(entered)
		<-release
		return driver.VolumeState{Phase: driver.VolumeAvailable}, nil
	}
	going, err := c.CreateVolume(t.Context(), aVolume("going", v1.VolumeAccessSharedRead, imageSource), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	c.FillVolumes(t.Context())
	<-entered
	if got, err := c.DeleteVolume(t.Context(), going.Status.ID); err != nil || got.Status.Phase != v1.VolumeDeleting {
		t.Fatalf("the delete answered %+v, %v", got.Status, err)
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("next", v1.VolumeAccessSharedRead, imageSource), "alice", 1); err != nil {
		t.Fatalf("a Deleting volume held the only slot: %v", err)
	}
	close(release)
	c.fillWork.Wait()
}

// TestTheVolumeCeilingHoldsUnderConcurrentCreates: creates racing for the
// last slots of a ceiling yield exactly that many volumes, since the count
// and the write are one act under the controller's lock.
func TestTheVolumeCeilingHoldsUnderConcurrentCreates(t *testing.T) {
	c, _, _, _ := volumeController(t)
	const ceiling, racers = 3, 16
	var created, refused atomic.Int32
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			obj := aVolume(fmt.Sprintf("racer-%d", i), v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty})
			_, err := c.CreateVolume(t.Context(), obj, "alice", ceiling)
			switch {
			case err == nil:
				created.Add(1)
			case errors.Is(err, ErrVolumeQuota):
				refused.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if created.Load() != ceiling || refused.Load() != racers-ceiling {
		t.Fatalf("%d creates, %d refusals; want %d and %d", created.Load(), refused.Load(), ceiling, racers-ceiling)
	}
	held := 0
	for _, v := range c.ListVolumes() {
		if v.Status.Owner == "alice" {
			held++
		}
	}
	if held != ceiling {
		t.Fatalf("alice holds %d volumes, want %d", held, ceiling)
	}
}

// TestVolumesNeedAStoreAndADriverThatKeepThem: a store that keeps no volumes
// refuses every volume act, and so does an environment whose driver keeps
// none.
func TestVolumesNeedAStoreAndADriverThatKeepThem(t *testing.T) {
	st := newDurable(true)
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := openController(t, Options{Store: st, Driver: runtimetest.NewVolumes(d)})
	if c.VolumesEnabled() {
		t.Fatal("a store with no volume seam reports volumes")
	}
	if _, err := c.CreateVolume(t.Context(), aVolume("v", v1.VolumeAccessSingle, imageSource), "alice", 0); !errors.Is(err, ErrNoVolumeStore) {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.UpdateVolume(t.Context(), aVolume("v", v1.VolumeAccessSingle, imageSource), "vol_x"); !errors.Is(err, ErrNoVolumeStore) {
		t.Fatalf("update: %v", err)
	}
	if _, err := c.DeleteVolume(t.Context(), "vol_x"); !errors.Is(err, ErrNoVolumeStore) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Create(t.Context(), mountingVolume("m", "v", true), "alice", 0); !errors.Is(err, ErrNoVolumeStore) {
		t.Fatalf("a mount: %v", err)
	}

	plain := openController(t, Options{Driver: d})
	if _, err := plain.CreateVolume(t.Context(), aVolume("v", v1.VolumeAccessSingle, imageSource), "alice", 0); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("a driver that keeps no volumes: %v", err)
	}
}

// volumeStore is the durable fake with the Volume kind beside it, for the
// recovery case.
type volumeStore struct {
	*durableStore
	mu      sync.Mutex
	volumes map[string]v1.Volume
}

func (s *volumeStore) LoadVolumes() (map[string]v1.Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]v1.Volume{}
	maps.Copy(out, s.volumes)
	return out, nil
}

func (s *volumeStore) WriteVolume(_ context.Context, act VolumeAct) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[act.Object.Status.ID] = act.Object
	return nil
}

func (s *volumeStore) RemoveVolume(_ context.Context, act VolumeAct) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.volumes, act.Object.Status.ID)
	return nil
}

// TestRecoveryFailsAVolumeMissing: a lost sandbox whose volume is gone with
// its content is not recreated without it: recovery fails it VolumeMissing,
// naming the volume, and the driver is not asked for it.
func TestRecoveryFailsAVolumeMissing(t *testing.T) {
	clock := newClock()
	fake := newDriver(clock)
	vd := runtimetest.NewVolumes(fake)
	st := &volumeStore{durableStore: newDurable(true), volumes: map[string]v1.Volume{}}
	c := withDriver(t, st, vd, clock, Options{})
	vol := filled(t, c, aVolume("state", v1.VolumeAccessSingle, v1.VolumeSource{Kind: v1.VolumeSourceEmpty}))
	obj, err := realized(t.Context(), c, mountingVolume("holder", "state", false), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	vd.Lose(vol.Status.ID)
	if err := c.observeVolumes(t.Context()); err != nil {
		t.Fatal(err)
	}
	vanish(fake, obj.Status.ID)
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(t.Context(), obj.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cond := conditionOf(got, v1.ConditionVolumesAttached)
	if got.Status.Phase != PhaseFailed || got.Status.Reason != ReasonVolumeMissing || cond.Status != v1.ConditionFalse {
		t.Fatalf("the lost sandbox is %s %s with %+v, want Failed VolumeMissing", got.Status.Phase, got.Status.Reason, cond)
	}
	if _, err := fake.Inspect(t.Context(), obj.Status.ID); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("recovery recreated the sandbox without its volume: %v", err)
	}
}

// TestRunVolumesFillsOnCreate: the loop takes a volume as soon as it is
// created rather than at its next tick.
func TestRunVolumesFillsOnCreate(t *testing.T) {
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := openController(t, Options{Driver: runtimetest.NewVolumes(d), VolumeInterval: 1 << 40})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunVolumes(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	obj, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the volume to fill", func() bool {
		got, err := c.GetVolume(t.Context(), obj.Status.ID, "alice")
		return err == nil && got.Status.Phase == v1.VolumeAvailable
	})
}

// TestAWaitingSandboxKeepsItsCreatesRequest: a sandbox that waited for its
// volume is started under the request that created it, as one that waited
// for nothing is, so its records name the caller and the request.
func TestAWaitingSandboxKeepsItsCreatesRequest(t *testing.T) {
	events := &originRecorder{seen: map[string]any{}}
	clock := newClock()
	vd := runtimetest.NewVolumes(newDriver(clock))
	vd.Fill = func(driver.VolumeSpec) (driver.VolumeState, error) {
		return driver.VolumeState{Phase: driver.VolumePending}, nil
	}
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := withDriver(t, st, vd, clock, Options{Events: events})
	vol, err := c.CreateVolume(t.Context(), aVolume("grader", v1.VolumeAccessSharedRead, imageSource), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	fillPass(t, c)
	request := context.WithValue(t.Context(), originKey{}, "req_1")
	if _, err := c.Create(request, mountingVolume("waiting", vol.Status.ID, true), "alice", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	events.mu.Lock()
	_, started := events.seen[MutationStarted]
	events.mu.Unlock()
	if started {
		t.Fatal("a sandbox started before its volume was filled")
	}
	vd.Set(driver.VolumeState{ID: vol.Status.ID, Phase: driver.VolumeAvailable})
	fillPass(t, c)
	if err := c.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if got := events.seen[MutationStarted]; got != "req_1" {
		t.Fatalf("the start was written under %v, want the create's request", got)
	}
}
