// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/storetest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestVolumesOutliveTheProcess: a Volume is a row of the objects table, so
// it needed no migration, and a control plane that restarts over the same
// database reads every live volume back with its phase, its pinned image and
// its retain, and none that was deleted.
func TestVolumesOutliveTheProcess(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	first := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(first, "default", store.Journaled)
	retain := false
	image := "registry.example.com/grader@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	kept := v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume, Metadata: v1.Metadata{Name: "grader"},
		Spec: v1.VolumeSpec{Environment: "default", Size: "1Gi", Access: v1.VolumeAccessSharedRead, Retain: &retain,
			Source: v1.VolumeSource{Kind: v1.VolumeSourceImage, Image: image}},
		Status: v1.VolumeStatus{ID: "vol_kept", Owner: "alice", Phase: v1.VolumeAvailable, Capacity: "1Gi"},
	}
	gone := kept
	gone.Metadata.Name = "gone"
	gone.Status.ID = "vol_gone"
	for _, obj := range []v1.Volume{kept, gone} {
		if err := bridge.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeCreated, Object: obj}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bridge.RemoveVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeDeleted, Object: gone}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := open(t, dsn, storetest.Key, time.Hour)
	held, err := store.ForController(second, "default", store.Journaled).LoadVolumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Fatalf("the restarted process reads %d volumes, want the one that was not deleted", len(held))
	}
	got := held["vol_kept"]
	if got.Status.Phase != v1.VolumeAvailable || got.Spec.Source.Image != image || got.Spec.Retains() || got.Metadata.Name != "grader" {
		t.Fatalf("read back %+v", got)
	}
}
