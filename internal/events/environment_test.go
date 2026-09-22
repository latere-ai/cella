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

// anEnvironment is one Environment as the controller hands it over.
func anEnvironment(id string) v1.Environment {
	return v1.Environment{
		Metadata: v1.Metadata{Name: "eu-gpu", Labels: map[string]string{"region": "eu"}},
		Status:   v1.EnvironmentStatus{ID: id, Owner: "admin", Phase: "Ready"},
	}
}

// TestEnvironmentRecords is design 009's Environment row: every type, the
// object under its id or, before it has one, its name, and the workers, the
// phase and the reason the act carried.
func TestEnvironmentRecords(t *testing.T) {
	for _, kind := range events.EnvironmentTypes {
		if !events.Deliverable(kind) {
			t.Fatalf("%s is not delivered", kind)
		}
		record, err := events.EnvironmentMutation(kind, anEnvironment(""), events.Environment{Workers: 2, Phase: "Ready"},
			events.Actor{Subject: "admin"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if record.Object.Kind != events.KindEnvironment || record.Object.ID != "eu-gpu" || record.Object.Labels["region"] != "eu" {
			t.Fatalf("the object is %+v", record.Object)
		}
	}
	if got := events.OfEnvironment(anEnvironment("env_1")).ID; got != "env_1" {
		t.Fatalf("an environment with an id is filed under %q", got)
	}
}

// TestEmitJournalsTheOtherKinds: the controller's seams for an Environment
// and a Secret journal one record each, pass over a type design 009 does not
// deliver, and do nothing without a journal.
func TestEmitJournalsTheOtherKinds(t *testing.T) {
	s, j := journal(t)
	emitter := events.NewEmitter(j, slog.New(slog.DiscardHandler))
	emitter.EmitEnvironment(t.Context(), controller.EnvironmentAct{
		Type: string(events.TypeEnvironmentOffline), Object: anEnvironment("env_1"), Workers: 0, Reason: "NoWorker",
	})
	emitter.EmitEnvironment(t.Context(), controller.EnvironmentAct{Type: "environment.saved", Object: anEnvironment("env_1")})
	emitter.EmitSecret(t.Context(), controller.SecretAct{Type: string(events.TypeSecretCreated), Object: aSecret()})
	emitter.EmitSecret(t.Context(), controller.SecretAct{Type: "secret.saved", Object: aSecret()})
	rows, err := store.EventJournal(s, store.Delivered).Pending(t.Context(), 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var kinds []events.Type
	for _, row := range rows {
		kinds = append(kinds, row.Record.Type)
	}
	if len(rows) != 2 {
		t.Fatalf("the journal holds %v, want one environment and one secret record", kinds)
	}
	var offline events.Environment
	for _, row := range rows {
		if row.Record.Type != events.TypeEnvironmentOffline {
			continue
		}
		if err = json.Unmarshal(row.Record.Data, &offline); err != nil {
			t.Fatal(err)
		}
	}
	if offline.Phase != "Ready" || offline.Reason != "NoWorker" {
		t.Fatalf("the environment record carries %+v", offline)
	}

	var none *events.Emitter
	none.EmitEnvironment(t.Context(), controller.EnvironmentAct{Type: string(events.TypeEnvironmentCreated)})
	none.EmitSecret(t.Context(), controller.SecretAct{Type: string(events.TypeSecretCreated)})
}
