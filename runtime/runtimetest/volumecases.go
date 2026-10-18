// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"strings"
	"time"

	"latere.ai/x/cella/runtime"
)

// The volume cases of spec 004 hold on the Driver as it is: a volume's own
// lifecycle with what a sandbox writes into it outliving that sandbox, and an
// attach that fails leaving no sandbox. A change of a stopped sandbox's
// mounts has no operation on the Driver yet, so
// VolumeAttachDetachWhileStopped is not among them. Both are in cases and
// skip on a driver that declares no Volumes.

// volumeDriverOf is the driver as a keeper of volumes. A driver that declares
// no Volumes skips the case; one that declares it and does not implement
// runtime.VolumeDriver fails it, because the controller branches on the
// declaration alone.
func volumeDriverOf(t tb, d runtime.Driver) runtime.VolumeDriver {
	t.Helper()
	vd, keeps := d.(runtime.VolumeDriver)
	switch {
	case !d.Capabilities().Volumes:
		t.Skipf("the driver declares no Volumes")
	case !keeps:
		t.Fatalf("the driver declares Volumes and does not implement runtime.VolumeDriver")
	}
	return vd
}

// waitVolume polls InspectVolume until the volume reaches the phase, and fails
// on a fill that failed or a wait past pollTimeout.
func waitVolume(t tb, vd runtime.VolumeDriver, id, phase string) runtime.VolumeState {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		state, err := vd.InspectVolume(context.Background(), id)
		must(t, err, "InspectVolume "+id)
		if state.Phase == phase {
			return state
		}
		need(t, state.Phase != runtime.VolumeFailed, "%s failed its fill: %s %s", id, state.Reason, state.Message)
		need(t, time.Now().Before(deadline), "%s stayed at %q, want %q", id, state.Phase, phase)
		time.Sleep(10 * time.Millisecond)
	}
}

// volumeLifecycle: an empty volume becomes Available, a second create of it
// is ErrAlreadyExists, a delete makes it ErrNotFound and a second delete is
// not an error. Where the suite runs images, a file one sandbox writes
// through a read-write mount is read by a second sandbox through a read-only
// mount after the first is deleted, and the second cannot write.
func volumeLifecycle(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	vd := volumeDriverOf(t, d)
	ctx := context.Background()
	spec := runtime.VolumeSpec{ID: "vol_cnf_life", Name: "life", Owner: "conformance", Size: "64Mi",
		Access: runtime.AccessSingle, Source: runtime.VolumeSource{Kind: runtime.SourceEmpty}}
	_, err := vd.CreateVolume(ctx, spec)
	must(t, err, "CreateVolume")
	t.Cleanup(func() { _ = vd.DeleteVolume(context.Background(), spec.ID) })
	state := waitVolume(t, vd, spec.ID, runtime.VolumeAvailable)
	expect(t, state.ID == spec.ID, "InspectVolume names %q, want %q", state.ID, spec.ID)
	_, err = vd.CreateVolume(ctx, spec)
	wantErr(t, err, runtime.ErrAlreadyExists, "a second CreateVolume")

	if opts.Image != "" {
		const dir = "/cella-volume"
		writer := runtime.CreateSpec{ID: "sbx_cnf_vol_writer", Owner: "conformance",
			Volumes: []runtime.Mount{{Name: "life", VolumeID: spec.ID, Path: dir}}}
		create(t, d, opts, writer)
		script(t, d, opts, writer.ID, "echo kept > "+dir+"/kept")
		must(t, d.Delete(ctx, writer.ID), "Delete "+writer.ID)
		reader := runtime.CreateSpec{ID: "sbx_cnf_vol_reader", Owner: "conformance",
			Volumes: []runtime.Mount{{Name: "life", VolumeID: spec.ID, Path: dir, ReadOnly: true}}}
		create(t, d, opts, reader)
		got := script(t, d, opts, reader.ID, "cat "+dir+"/kept")
		expect(t, strings.TrimSpace(got) == "kept", "the second sandbox reads %q from the volume, want kept", got)
		_, _, code, err := run(t, d, reader.ID, runtime.ExecRequest{Command: opts.shell("echo no > " + dir + "/refused")})
		must(t, err, "Exec through a read-only mount")
		expect(t, code != 0, "a read-only mount took a write")
		must(t, d.Delete(ctx, reader.ID), "Delete "+reader.ID)
	}

	must(t, vd.DeleteVolume(ctx, spec.ID), "DeleteVolume")
	_, err = vd.InspectVolume(ctx, spec.ID)
	wantErr(t, err, runtime.ErrNotFound, "InspectVolume after DeleteVolume")
	must(t, vd.DeleteVolume(ctx, spec.ID), "a second DeleteVolume")
}

// attachFailureIsClean: a create that mounts a volume the driver does not
// hold fails, and leaves no sandbox for Inspect or List to find.
func attachFailureIsClean(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	volumeDriverOf(t, d)
	ctx := context.Background()
	spec := runtime.CreateSpec{ID: "sbx_cnf_attach", Owner: "conformance", Image: opts.Image,
		Volumes: []runtime.Mount{{Name: "absent", VolumeID: "vol_cnf_absent", Path: "/cella-absent"}}}
	_, err := d.Create(ctx, spec)
	t.Cleanup(func() { _ = d.Delete(context.Background(), spec.ID) })
	need(t, err != nil, "a create that mounts a volume the driver does not hold succeeded")
	_, err = d.Inspect(ctx, spec.ID)
	wantErr(t, err, runtime.ErrNotFound, "Inspect after a refused attach")
	states, err := d.List(ctx, runtime.Filter{})
	must(t, err, "List")
	for _, s := range states {
		expect(t, s.ID != spec.ID, "List holds %s after its attach was refused", spec.ID)
	}
}
