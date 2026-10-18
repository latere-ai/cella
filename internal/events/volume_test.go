// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

// aVolume is one Volume as the controller hands it over.
func aVolume(phase, reason string) v1.Volume {
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume,
		Metadata: v1.Metadata{Name: "grader", Labels: map[string]string{"tenant": "acme"}},
		Spec: v1.VolumeSpec{Size: "1Gi", Access: v1.VolumeAccessSharedRead,
			Source: v1.VolumeSource{Kind: v1.VolumeSourceImage, Image: "registry.example.com/grader@sha256:" +
				"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		Status: v1.VolumeStatus{ID: "vol_a", Owner: "alice", Phase: phase, Reason: reason},
	}
}

// TestVolumeRecords is design 009's Volume row: every type is delivered and
// none is terminal, the object carries its labels, an attach names the
// sandbox in context and the mode, and a failure carries a reason of the
// enum and never one outside it.
func TestVolumeRecords(t *testing.T) {
	for _, kind := range events.VolumeTypes {
		if !events.Deliverable(kind) || events.Terminal(kind) {
			t.Fatalf("%s is not delivered, or is terminal", kind)
		}
	}
	holder := v1.Sandbox{Metadata: v1.Metadata{Name: "check"}, Status: v1.SandboxStatus{ID: "sbx_a", Owner: "alice"}}
	attach, err := events.VolumeMutation(events.TypeVolumeAttached, aVolume(v1.VolumeAvailable, ""), &holder,
		controller.VolumeModeReadOnly, events.Actor{Subject: "alice"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if attach.Object.Kind != events.KindVolume || attach.Object.Labels["tenant"] != "acme" ||
		attach.Sandbox == nil || attach.Sandbox.ID != "sbx_a" || attach.Reason != "" {
		t.Fatalf("the attach is %+v", attach)
	}
	var data events.Volume
	if err := json.Unmarshal(attach.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Mode != controller.VolumeModeReadOnly || data.Source != v1.VolumeSourceImage || data.Phase != v1.VolumeAvailable {
		t.Fatalf("the data is %+v", data)
	}
	failed, err := events.VolumeMutation(events.TypeVolumeFailed, aVolume(v1.VolumeFailed, "SourceTooLarge"), nil, "",
		events.Actor{}, time.Now().UTC())
	if err != nil || failed.Reason != events.ReasonSourceTooLarge || failed.Subject != events.SubjectController {
		t.Fatalf("the failure is %+v, %v", failed, err)
	}
	other, err := events.VolumeMutation(events.TypeVolumeFailed, aVolume(v1.VolumeFailed, "SomethingElse"), nil, "",
		events.Actor{}, time.Now().UTC())
	if err != nil || other.Reason != "" {
		t.Fatalf("a reason outside the enum is %q, %v", other.Reason, err)
	}
}

// TestEmitVolumeJournalsTheAct: the snapshot store's seam journals a volume
// act as the store bridge does, and skips the status write, which is no
// event.
func TestEmitVolumeJournalsTheAct(t *testing.T) {
	s, j := journal(t)
	emitter := events.NewEmitter(j, slog.New(slog.DiscardHandler))
	emitter.EmitVolume(t.Context(), controller.VolumeAct{Type: controller.MutationVolumeStatus, Object: aVolume(v1.VolumeAvailable, "")})
	emitter.EmitVolume(t.Context(), controller.VolumeAct{Type: controller.MutationVolumeCreated, Object: aVolume(v1.VolumePending, "")})
	events.NewEmitter(nil, nil).EmitVolume(t.Context(), controller.VolumeAct{Type: controller.MutationVolumeCreated})
	rows, err := store.EventJournal(s, store.Delivered).Pending(t.Context(), 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Record.Type != events.TypeVolumeCreated || rows[0].Record.Object.ID != "vol_a" {
		t.Fatalf("the journal holds %+v", rows)
	}
}
