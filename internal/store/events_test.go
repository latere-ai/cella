// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// labeled is one sandbox with the labels a plane stamped on it and an
// environment value that must not reach a record.
func labeled(id, phase, reason string) v1.Sandbox {
	obj := sandbox(id, "build", phase)
	obj.Metadata.Labels = map[string]string{"tenant": "acme"}
	obj.Spec.Image = "registry.example/base:1"
	obj.Spec.Env = map[string]string{"API_TOKEN": "sk-ant-shaped-like-a-secret"}
	obj.Status.Reason = reason
	return obj
}

// recordsOf reads one object's journal newest first and rebuilds each row.
func recordsOf(t *testing.T, s store.Store, id string) []events.Record {
	t.Helper()
	var out []events.Record
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		rows, _, err := tx.Journal().ByObject(t.Context(), id, store.Page{})
		if err != nil {
			return err
		}
		for _, row := range rows {
			record, err := events.Rebuild(row.Payload, row.ID, row.Seq, row.Type, row.At)
			if err != nil {
				return err
			}
			out = append(out, record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRecordCommitsWithTheMutation: the state and the record that explains it
// are one transaction, so the journal never holds an event for a write that
// did not land and never misses one that did.
func TestRecordCommitsWithTheMutation(t *testing.T) {
	c, s := bound(t)
	ctx := events.WithActor(t.Context(), events.Actor{Subject: "alice", RequestID: "req_1"})
	if err := c.Write(ctx, labeled("sbx_a", driver.Pending, ""), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	created := recordsOf(t, s, "sbx_a")
	if len(created) != 1 || created[0].Type != events.TypeCreated {
		t.Fatalf("the journal reads %+v", created)
	}
	if created[0].Seq != 1 || created[0].Subject != "alice" || created[0].RequestID != "req_1" {
		t.Errorf("the record is %+v", created[0])
	}
	if created[0].Object.Labels["tenant"] != "acme" || created[0].Object.Kind != events.KindSandbox {
		t.Errorf("the record's object is %+v", created[0].Object)
	}
	if strings.Contains(string(created[0].Data), "sk-ant-") {
		t.Errorf("an environment value reached the record: %s", created[0].Data)
	}

	// A second writer's conditional write fails, and no record survives it.
	other := store.ForController(s, "default", store.Delivered)
	if err := other.Write(ctx, labeled("sbx_a", driver.Running, ""), controller.MutationStarted); err == nil {
		t.Fatal("a write at the wrong version was accepted")
	}
	if held := recordsOf(t, s, "sbx_a"); len(held) != 1 {
		t.Errorf("a rolled back mutation left %d record(s)", len(held))
	}
}

// TestDeletedRecordCarriesTheObjectThatWent: the row is read inside the
// transaction that removes it, so the record names an object that no longer
// exists when the transaction ends.
func TestDeletedRecordCarriesTheObjectThatWent(t *testing.T) {
	c, s := bound(t)
	ctx := t.Context()
	if err := c.Write(ctx, labeled("sbx_a", driver.Pending, ""), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, labeled("sbx_a", "Deleting", "AutoDelete"), controller.MutationDeleting); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "sbx_a", controller.MutationDeleted); err != nil {
		t.Fatal(err)
	}
	held := recordsOf(t, s, "sbx_a")
	if len(held) != 3 || held[0].Type != events.TypeDeleted {
		t.Fatalf("the journal reads %+v", held)
	}
	deleted := held[0]
	if deleted.Object.Name != "build" || deleted.Object.Owner != "alice" ||
		deleted.Object.Labels["tenant"] != "acme" || deleted.Reason != events.ReasonAutoDelete {
		t.Errorf("the deleted record is %+v", deleted)
	}
	var data events.Phase
	if err := json.Unmarshal(deleted.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Phase != "Deleting" {
		t.Errorf("the deleted record carries phase %q", data.Phase)
	}
	// Removing a row another replica already took still records that this
	// replica ended the object.
	if err := c.Remove(ctx, "sbx_a", controller.MutationDeleted); err != nil {
		t.Fatal(err)
	}
	if again := recordsOf(t, s, "sbx_a"); len(again) != 4 || again[0].Object.ID != "sbx_a" {
		t.Errorf("a second removal wrote %+v", again)
	}
}

// TestJournaledActsAreNotAllDelivered: the deleting intent and the status
// write are rows and not deliveries; the acts design 009 names are both.
func TestJournaledActsAreNotAllDelivered(t *testing.T) {
	c, s := bound(t)
	ctx := t.Context()
	for _, tc := range []struct{ mutation, phase string }{
		{controller.MutationCreated, driver.Pending},
		{controller.MutationStatus, driver.Running},
		{controller.MutationStarted, driver.Running},
		{controller.MutationDeleting, "Deleting"},
	} {
		if err := c.Write(ctx, labeled("sbx_a", tc.phase, ""), tc.mutation); err != nil {
			t.Fatalf("%s: %v", tc.mutation, err)
		}
	}
	if held := recordsOf(t, s, "sbx_a"); len(held) != 4 {
		t.Fatalf("the journal holds %d row(s), want one per act", len(held))
	}
	journal := store.EventJournal(s, store.Delivered)
	var delivered []events.Type
	for range 4 {
		rows, err := journal.Pending(ctx, 10, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		delivered = append(delivered, rows[0].Record.Type)
		if err := journal.Acknowledge(ctx, rows[0].Record.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	want := []events.Type{events.TypeCreated, events.TypeStarted}
	if len(delivered) != len(want) {
		t.Fatalf("delivery took %v, want %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Fatalf("delivery took %v, want %v", delivered, want)
		}
	}
}

// TestSaveJournalsEveryObjectItWrote: the snapshot contract's whole-map write
// is one mutation per object, named as such and never delivered.
func TestSaveJournalsEveryObjectItWrote(t *testing.T) {
	c, s := bound(t)
	if err := c.Save(map[string]v1.Sandbox{"sbx_a": labeled("sbx_a", driver.Running, "")}); err != nil {
		t.Fatal(err)
	}
	held := recordsOf(t, s, "sbx_a")
	if len(held) != 1 || held[0].Type != events.Type(controller.MutationSaved) {
		t.Fatalf("the journal reads %+v", held)
	}
	rows, err := store.EventJournal(s, store.Delivered).Pending(t.Context(), 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a snapshot save queued %+v for the sink", rows)
	}
}

// TestOperationRecordsHaveNoMutation: an exec changes no desired state, so it
// is appended on its own and still takes the sandbox's next sequence.
func TestOperationRecordsHaveNoMutation(t *testing.T) {
	c, s := bound(t)
	ctx := t.Context()
	if err := c.Write(ctx, labeled("sbx_a", driver.Running, ""), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	record, err := events.Operation(events.TypeExec,
		events.OfSandbox(labeled("sbx_a", driver.Running, "")),
		events.Exec{ExitCode: 7, DurationMS: 12},
		events.Actor{Subject: "alice", RequestID: "req_2"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EventJournal(s, store.Delivered).Append(ctx, record); err != nil {
		t.Fatal(err)
	}
	held := recordsOf(t, s, "sbx_a")
	if len(held) != 2 || held[0].Type != events.TypeExec || held[0].Seq != 2 {
		t.Fatalf("the journal reads %+v", held)
	}
	if held[0].Sandbox == nil || held[0].Sandbox.ID != "sbx_a" {
		t.Errorf("the operation record names no sandbox: %+v", held[0])
	}
}

// TestAnIncompleteRecordIsRefused: a record the sink would refuse never
// reaches the journal.
func TestAnIncompleteRecordIsRefused(t *testing.T) {
	_, s := bound(t)
	if err := store.EventJournal(s, store.Delivered).Append(t.Context(), events.Record{}); err == nil {
		t.Fatal("a record with no id, type or object was journaled")
	}
}

// TestJournalDeliveryOutcomes: the three a delivery can end in, over the
// journal the deliverer holds rather than the store contract underneath.
func TestJournalDeliveryOutcomes(t *testing.T) {
	_, s := bound(t)
	ctx := t.Context()
	journal := store.EventJournal(s, store.Delivered)
	for _, kind := range []events.Type{events.TypeCreated, events.TypeStarted} {
		record, err := events.Mutation(kind, "", events.Object{
			Kind: events.KindSandbox, ID: "sbx_a", Name: "build", Owner: "alice",
		}, events.Phase{Phase: "Running"}, events.Actor{}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Append(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	head, err := journal.Pending(ctx, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != 1 || head[0].Record.Seq != 1 || head[0].Attempts != 0 {
		t.Fatalf("the queue reads %+v", head)
	}
	// Deferred, the head is not due and its successor does not overtake it.
	if err := journal.Defer(ctx, head[0].Record.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, err := journal.Pending(ctx, 10, now); err != nil || len(due) != 0 {
		t.Fatalf("a deferred head is due: %+v %v", due, err)
	}
	later, err := journal.Pending(ctx, 10, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("the deferred head reads %+v", later)
	}
	// Dropped, it leaves the queue and its successor becomes the head.
	if err := journal.Drop(ctx, head[0].Record.ID, now); err != nil {
		t.Fatal(err)
	}
	next, err := journal.Pending(ctx, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Record.Seq != 2 {
		t.Fatalf("after the drop the queue reads %+v", next)
	}
	if err := journal.Acknowledge(ctx, next[0].Record.ID, now); err != nil {
		t.Fatal(err)
	}
	if empty, err := journal.Pending(ctx, 10, now); err != nil || len(empty) != 0 {
		t.Fatalf("an acknowledged queue reads %+v %v", empty, err)
	}
}

// TestAMutationWithNoObjectIsRefused: a write whose record the sink could not
// store fails at the write, so the state and the journal stay in step.
func TestAMutationWithNoObjectIsRefused(t *testing.T) {
	c, _ := bound(t)
	nameless := labeled("", driver.Pending, "")
	nameless.Status.ID = ""
	if err := c.Write(t.Context(), nameless, controller.MutationCreated); err == nil {
		t.Fatal("a sandbox with no id was written")
	}
	if err := c.Remove(t.Context(), "", controller.MutationDeleted); err == nil {
		t.Fatal("a removal with no id was written")
	}
}

// TestAMutationOutsideTheEnumIsRefused: an act naming a reason design 009
// does not have fails rather than reaching a sink that would refuse it.
func TestAMutationOutsideTheEnumIsRefused(t *testing.T) {
	_, s := bound(t)
	journal := store.EventJournal(s, store.Delivered)
	if err := journal.Acknowledge(t.Context(), "evt_nothing", time.Now().UTC()); err == nil {
		t.Error("acknowledging an event no row holds passed")
	}
	if err := journal.Defer(t.Context(), "evt_nothing", time.Now().UTC()); err == nil {
		t.Error("deferring an event no row holds passed")
	}
	if err := journal.Drop(t.Context(), "evt_nothing", time.Now().UTC()); err == nil {
		t.Error("dropping an event no row holds passed")
	}
}

// TestByObjectRebuildsTheRecord: the read half of the journal hands back the
// record as it was written, with the columns authoritative. A row whose
// payload cannot be read is the error the caller sees and never a page with a
// record missing from it.
func TestByObjectRebuildsTheRecord(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	j := store.EventJournal(s, store.Delivered)
	at := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	record, err := events.Mutation(events.TypeCreated, "", events.Object{
		Kind: events.KindSandbox, ID: "sbx_a", Name: "work", Owner: "alice",
	}, events.Phase{Phase: "Running"}, events.Actor{Subject: "alice"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	page, next, err := j.ByObject(t.Context(), "sbx_a", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || next != "" {
		t.Fatalf("the page holds %d record(s) and the cursor %q", len(page), next)
	}
	if page[0].Seq != 1 || page[0].Type != events.TypeCreated || page[0].Object.ID != "sbx_a" {
		t.Fatalf("the record came back as %+v", page[0])
	}
	if !page[0].Time.Equal(at) {
		t.Errorf("the record's time is %s", page[0].Time)
	}
	// An object with no records is an empty page and not a refusal.
	page, next, err = j.ByObject(t.Context(), "sbx_nothing", "", 50)
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("an object with no records answered %d record(s), %q, %v", len(page), next, err)
	}
	// A cursor the journal cannot read is the journal's refusal.
	if _, _, err := j.ByObject(t.Context(), "sbx_a", "not a sequence", 50); err == nil {
		t.Error("a cursor that is not a sequence was read as one")
	}
}

// durable is a memory store that reports it outlives the process, which is
// the one fact Shared reads.
type durable struct{ *memory.Store }

func (durable) Durable() bool { return true }

// TestTheJournalFollowsWhatCommits: the follow half of the event journal. A
// read after a sequence and a subscription each hand back the record as it
// was written, with the sequence the journal assigned; a row whose payload
// cannot be read is an error on both; and a subscription that ended says why:
// its reader's cancellation, its reader's close, a record it dropped, or the
// store's close.
func TestTheJournalFollowsWhatCommits(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	j := store.EventJournal(s, store.Journaled)
	if j.Shared() || !store.EventJournal(durable{s}, store.Journaled).Shared() {
		t.Error("Shared does not follow whether the store outlives the process")
	}
	sub := j.Watch("sbx_a")
	record, err := events.Mutation(events.TypeCreated, "", events.Object{
		Kind: events.KindSandbox, ID: "sbx_a", Name: "work", Owner: "alice",
	}, events.Phase{Phase: "Running"}, events.Actor{Subject: "alice"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	live, err := sub.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	after, err := j.After(t.Context(), "sbx_a", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("After read %d records", len(after))
	}
	for name, got := range map[string]events.Record{"subscription": live, "read": after[0]} {
		if got.Seq != 1 || got.ID != record.ID || got.Type != events.TypeCreated || got.Object.Owner != "alice" {
			t.Errorf("the %s handed back %+v", name, got)
		}
	}

	// A row whose payload is not a record is an error on both halves.
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, err := tx.Journal().Append(t.Context(), store.Event{ObjectID: "sbx_a", Type: "sandbox.exec", Payload: []byte("not a record")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(t.Context()); err == nil {
		t.Error("the subscription handed over a row it could not rebuild")
	}
	if _, err := j.After(t.Context(), "sbx_a", 1, 10); err == nil {
		t.Error("After read a row it could not rebuild")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait answered %v", err)
	}
	sub.Close()
	if _, err := sub.Next(t.Context()); err == nil {
		t.Error("a subscription its reader closed handed over a record")
	}

	behind, open := j.Watch(""), j.Watch("sbx_c")
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		for range store.SubscriptionBuffer + 1 {
			if _, err := tx.Journal().Append(t.Context(), store.Event{ObjectID: "sbx_b", Type: "sandbox.exec", Payload: []byte(`{}`)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = behind.Next(t.Context()); err != nil {
			break
		}
	}
	if !errors.Is(err, events.ErrBehind) {
		t.Errorf("a subscription that dropped a record ended with %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := open.Next(t.Context()); !errors.Is(err, memory.ErrClosed) {
		t.Errorf("a subscription on a closed store ended with %v", err)
	}
}
