// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

// volume is one Volume as the controller hands it to the store.
func volume(id, name string) v1.Volume {
	retain := true
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume,
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"team": "a"}},
		Spec: v1.VolumeSpec{Environment: "default", Size: "1Gi", Access: v1.VolumeAccessSharedRead, Retain: &retain,
			Source: v1.VolumeSource{Kind: v1.VolumeSourceImage,
				Image: "registry.example.com/grader@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		Status: v1.VolumeStatus{ID: id, Owner: "alice", Phase: v1.VolumePending, AttachedTo: []string{"sbx_derived"}},
	}
}

// TestVolumeCollectionRoundTrips is the Volume half of the bridge: the object
// goes in as a row of the objects table and comes back as the controller
// wrote it, less attachedTo, which is derived at read; each act is a record
// of the journal that commits with the row, an attach naming its sandbox and
// mode; and a write is conditional on the row this process last saw.
func TestVolumeCollectionRoundTrips(t *testing.T) {
	bridge, s := bound(t)
	ctx := t.Context()
	obj := volume("vol_a", "grader")
	if err := bridge.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeCreated, Object: obj}); err != nil {
		t.Fatal(err)
	}
	obj.Status.Phase = v1.VolumeAvailable
	obj.Status.Capacity = "1Gi"
	if err := bridge.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeStatus, Object: obj}); err != nil {
		t.Fatal(err)
	}
	holder := sandbox("sbx_holder", "holder", "Running")
	if err := bridge.WriteVolume(ctx, controller.VolumeAct{
		Type: controller.MutationVolumeAttached, Object: obj, Sandbox: &holder, Mode: controller.VolumeModeReadOnly,
	}); err != nil {
		t.Fatal(err)
	}
	held, err := store.ForController(s, "default", store.Delivered).LoadVolumes()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := held["vol_a"]
	if !ok {
		t.Fatalf("the collection is %+v", held)
	}
	if got.Status.Phase != v1.VolumeAvailable || got.Status.Capacity != "1Gi" || got.Spec.Source != obj.Spec.Source ||
		got.Metadata.Labels["team"] != "a" || !got.Spec.Retains() {
		t.Fatalf("read back %+v", got)
	}
	if got.Status.AttachedTo != nil {
		t.Fatalf("attachedTo was stored: %v", got.Status.AttachedTo)
	}

	records := recordsOf(t, s, "vol_a")
	var types []events.Type
	for _, r := range records {
		types = append(types, r.Type)
	}
	// The status write is journaled and is no event; the attach names the
	// sandbox in context and the mode.
	want := []events.Type{events.TypeVolumeAttached, events.Type(controller.MutationVolumeStatus), events.TypeVolumeCreated}
	if !slices.Equal(types, want) {
		t.Fatalf("the journal holds %v, want %v", types, want)
	}
	attach := records[0]
	var data events.Volume
	if err := json.Unmarshal(attach.Data, &data); err != nil {
		t.Fatal(err)
	}
	want2 := events.Volume{Phase: v1.VolumeAvailable, Access: v1.VolumeAccessSharedRead, Source: v1.VolumeSourceImage,
		Image: obj.Spec.Source.Image, Mode: controller.VolumeModeReadOnly}
	if attach.Sandbox == nil || attach.Sandbox.ID != "sbx_holder" || attach.Object.Kind != events.KindVolume || data != want2 {
		t.Fatalf("the attach record is %+v with %+v", attach, data)
	}

	// A second bridge that never read the row cannot overwrite it.
	stale := store.ForController(s, "default", store.Delivered)
	if err := stale.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeUpdated, Object: obj}); !errors.Is(err, controller.ErrVersionConflict) {
		t.Fatalf("a write over a row this bridge never read: %v", err)
	}

	if err := bridge.RemoveVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeDeleted, Object: obj}); err != nil {
		t.Fatal(err)
	}
	if held, err := bridge.LoadVolumes(); err != nil || len(held) != 0 {
		t.Fatalf("after the delete: %v, %v", held, err)
	}
	if records := recordsOf(t, s, "vol_a"); records[0].Type != events.TypeVolumeDeleted {
		t.Fatalf("the delete is not the newest record: %v", records[0].Type)
	}
	// A row another replica already deleted is gone either way.
	if err := bridge.RemoveVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeDeleted, Object: obj}); err != nil {
		t.Fatalf("a delete of a deleted row: %v", err)
	}
}

// TestAFailedVolumeCarriesItsReason: volume.failed carries the reason of the
// failed fill where design 009's enum has it.
func TestAFailedVolumeCarriesItsReason(t *testing.T) {
	bridge, s := bound(t)
	obj := volume("vol_f", "broken")
	obj.Status.Phase = v1.VolumeFailed
	obj.Status.Reason = string(events.ReasonSourceTooLarge)
	if err := bridge.WriteVolume(t.Context(), controller.VolumeAct{Type: controller.MutationVolumeFailed, Object: obj}); err != nil {
		t.Fatal(err)
	}
	if records := recordsOf(t, s, "vol_f"); records[0].Reason != events.ReasonSourceTooLarge {
		t.Fatalf("the failure carries %q", records[0].Reason)
	}
}
