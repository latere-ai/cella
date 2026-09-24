// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

// act is one sandbox as the controller hands it over.
func act(kind, phase, reason string) controller.Act {
	return controller.Act{Type: kind, Object: v1.Sandbox{
		Metadata: v1.Metadata{Name: "build", Labels: map[string]string{"tenant": "acme"}},
		Spec:     v1.SandboxSpec{Image: "registry.example/base:1", Env: map[string]string{"API_TOKEN": "sk-ant-shaped-like-a-secret"}},
		Status:   v1.SandboxStatus{ID: "sbx_a", Owner: "alice", Phase: phase, Reason: reason},
	}}
}

// TestEmitJournalsAControllerAct: the seam the snapshot store's controller
// calls produces the same record the store bridge does.
func TestEmitJournalsAControllerAct(t *testing.T) {
	s, j := journal(t)
	var log bytes.Buffer
	emitter := events.NewEmitter(j, slog.New(slog.NewTextHandler(&log, nil)))
	emitter.Emit(events.WithActor(t.Context(), events.Actor{Subject: "alice", RequestID: "req_1"}),
		act("sandbox.created", "Pending", ""))
	emitter.Emit(t.Context(), act("sandbox.stopped", "Stopped", "AutoStop"))

	rows, err := store.EventJournal(s, store.Delivered).Pending(t.Context(), 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Record.Type != events.TypeCreated {
		t.Fatalf("the journal has %d pending record(s): %+v", len(rows), rows)
	}
	created := rows[0].Record
	if created.Seq != 1 || created.Subject != "alice" || created.RequestID != "req_1" {
		t.Errorf("the created record is %+v", created)
	}
	if created.Object.Labels["tenant"] != "acme" {
		t.Errorf("the record lost its labels: %+v", created.Object)
	}
	if strings.Contains(string(created.Data), "sk-ant-") {
		t.Errorf("an environment value reached the record: %s", created.Data)
	}
	var manifest struct {
		Env []string `json:"env"`
	}
	if err := json.Unmarshal(created.Data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Env) != 1 || manifest.Env[0] != "API_TOKEN" {
		t.Errorf("the created data names %v, want the key alone", manifest.Env)
	}
	// The reaper's act has no request behind it.
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		held, _, err := tx.Journal().ByObject(t.Context(), "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(held) != 2 {
			t.Fatalf("the journal holds %d row(s)", len(held))
		}
		record, err := events.Rebuild(held[0].Payload, held[0].ID, held[0].Seq, held[0].Type, held[0].At)
		if err != nil {
			return err
		}
		if record.Subject != events.SubjectController || record.Reason != events.ReasonAutoStop {
			t.Errorf("the reaper's record is %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEmitSkipsWhatDesign009DoesNotName: the two acts design 010 journals
// and design 009 names no type for reach the emitter and produce no record.
func TestEmitSkipsWhatDesign009DoesNotName(t *testing.T) {
	s, j := journal(t)
	events.NewEmitter(j, slog.New(slog.DiscardHandler)).Emit(t.Context(), act("sandbox.deleting", "Deleting", ""))
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		held, _, err := tx.Journal().ByObject(t.Context(), "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(held) != 0 {
			t.Errorf("an act design 009 does not name produced %+v", held)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEmitReportsWhatItCouldNotWrite: a record is a note about work and not
// the work, so a journal that refuses it is a log line and never an error
// back to the act.
func TestEmitReportsWhatItCouldNotWrite(t *testing.T) {
	s, j := journal(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	emitter := events.NewEmitter(j, slog.New(slog.NewTextHandler(&log, nil)))
	emitter.Emit(t.Context(), act("sandbox.created", "Pending", ""))
	if !strings.Contains(log.String(), "not journaled") {
		t.Errorf("a refused journal wrote %q", log.String())
	}
	// An act whose reason is outside the enum is refused before the write.
	log.Reset()
	emitter.Emit(t.Context(), controller.Act{Type: "sandbox.created"})
	if !strings.Contains(log.String(), "not built") {
		t.Errorf("an unbuildable record wrote %q", log.String())
	}
}

// TestEmitterWithNoJournalIsQuiet: a process with no journal and a nil
// emitter both do nothing rather than panic.
func TestEmitterWithNoJournalIsQuiet(t *testing.T) {
	var absent *events.Emitter
	absent.Emit(t.Context(), act("sandbox.created", "Pending", ""))
	absent.Write(t.Context(), events.Record{})
	events.NewEmitter(nil, nil).Emit(t.Context(), act("sandbox.created", "Pending", ""))
	events.NewEmitter(nil, nil).Write(t.Context(), events.Record{})
}

// TestMutationDataIsThePerTypeShape: one rule, read by both paths.
func TestMutationDataIsThePerTypeShape(t *testing.T) {
	obj := act("sandbox.created", "Pending", "").Object
	if _, ok := events.MutationData(events.TypeCreated, obj).(events.Created); !ok {
		t.Error("a create carries something other than the resolved manifest")
	}
	phase, ok := events.MutationData(events.TypeStopped, obj).(events.Phase)
	if !ok || phase.Phase != "Pending" {
		t.Errorf("a transition carries %+v", phase)
	}
}

// TestDeliveryUsesTheWallClockByDefault: a deliverer built with no clock
// reads this machine's, which is what a deployment runs on.
func TestDeliveryUsesTheWallClockByDefault(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	_, j := journal(t)
	write(t, j, time.Now().UTC(), "sbx_a", events.TypeCreated)
	d, err := events.NewDeliverer(events.DelivererOptions{
		Journal: j, Lease: lease{true}, URL: s.server.URL, Secrets: s.secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := d.Stats(); stats.Delivered != 1 {
		t.Errorf("the default clock delivered %+v", stats)
	}
}

// TestDeliveryDefersWhenTheSinkDoesNotAnswer: a connection failure is a
// retry, because the bytes were never judged.
func TestDeliveryDefersWhenTheSinkDoesNotAnswer(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	_, j := journal(t)
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	d, err := events.NewDeliverer(events.DelivererOptions{
		Journal: j, Lease: lease{true}, URL: url, Secrets: []string{"secret"},
		Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := d.Stats(); stats.Deferred != 1 || stats.Dropped != 0 {
		t.Errorf("an unanswered delivery counted %+v", stats)
	}
}

// TestBackoffDoublesToTheCeiling: the wait grows and then holds, so a sink
// down for a day is asked twelve times an hour and not once a millisecond.
func TestBackoffDoublesToTheCeiling(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	_, j := journal(t)
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	d := deliverer(t, j, s, c, true)
	const attempts = 12
	s.failNext("sbx_a", attempts)
	for range attempts {
		before := c.Now()
		if _, err := d.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The record is not due again at the instant of the attempt, and is
		// due once the ceiling has passed, whichever attempt this is.
		due, err := j.Pending(t.Context(), 10, before)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 0 {
			t.Fatalf("the record was due again at once after attempt %d", len(due))
		}
		if due, err = j.Pending(t.Context(), 10, before.Add(events.MaxBackoff+time.Second)); err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatal("the wait grew past the ceiling")
		}
		c.advance(events.MaxBackoff + time.Second)
	}
	if stats := d.Stats(); stats.Deferred != attempts {
		t.Errorf("the counts are %+v", stats)
	}
}

// refusing is a journal that answers every call with one error, so the
// delivery loop's own failure paths are exercised: what it could not read,
// could not acknowledge, could not defer and could not drop.
type refusing struct {
	rows []events.Pending
	err  error
}

func (r *refusing) Append(context.Context, events.Record) error { return r.err }
func (r *refusing) Undelivered(context.Context) (int, error)    { return len(r.rows), nil }
func (r *refusing) Pending(context.Context, int, time.Time) ([]events.Pending, error) {
	return r.rows, r.err
}
func (r *refusing) ByObject(context.Context, string, string, int) ([]events.Record, string, error) {
	return nil, "", r.err
}
func (r *refusing) After(context.Context, string, int64, int) ([]events.Record, error) {
	return nil, r.err
}
func (r *refusing) Watch(string) events.Subscription                     { return closedSubscription{r.err} }
func (r *refusing) Shared() bool                                         { return false }
func (r *refusing) Acknowledge(context.Context, string, time.Time) error { return r.err }
func (r *refusing) Defer(context.Context, string, time.Time) error       { return r.err }
func (r *refusing) Drop(context.Context, string, time.Time) error        { return r.err }

// closedSubscription is a subscription that has already ended with err.
type closedSubscription struct{ err error }

func (c closedSubscription) Next(context.Context) (events.Record, error) {
	return events.Record{}, c.err
}
func (closedSubscription) Close() {}

// failingLease reports an error rather than a verdict.
type failingLease struct{}

func (failingLease) Acquire(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("the lease table is unreachable")
}

// TestPassReportsWhatItCouldNotRead: a lease or a journal that fails ends the
// pass with the reason, and the loop tries again on the next tick.
func TestPassReportsWhatItCouldNotRead(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	unreadable, err := events.NewDeliverer(events.DelivererOptions{
		Journal: &refusing{err: errors.New("the journal is unreachable")}, Lease: lease{true},
		URL: s.server.URL, Secrets: s.secrets, Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unreadable.Pass(t.Context()); err == nil {
		t.Error("an unreadable journal passed")
	}
	leaseless, err := events.NewDeliverer(events.DelivererOptions{
		Journal: &refusing{}, Lease: failingLease{}, URL: s.server.URL,
		Secrets: s.secrets, Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaseless.Pass(t.Context()); err == nil {
		t.Error("an unreachable lease passed")
	}
	// Run logs the failure and keeps its schedule rather than exiting.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	leaseless.Run(ctx)
}

// TestOutcomesThatCannotBeRecordedAreNotCounted: a delivery whose result the
// journal refuses is logged and left pending, so the next pass tries again
// rather than the count claiming work that did not stick.
func TestOutcomesThatCannotBeRecordedAreNotCounted(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	record, err := events.Mutation(events.TypeCreated, "", events.Object{
		Kind: events.KindSandbox, ID: "sbx_a", Name: "build", Owner: "alice",
	}, events.Phase{Phase: "Pending"}, events.Actor{}, c.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, answer string }{
		{"acknowledged", "ok"}, {"deferred", "fail"}, {"dropped", "refuse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch tc.answer {
				case "fail":
					w.WriteHeader(http.StatusInternalServerError)
				case "refuse":
					w.WriteHeader(http.StatusBadRequest)
				default:
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer sink.Close()
			// The journal accepts the read and refuses the write.
			d, err := events.NewDeliverer(events.DelivererOptions{
				Journal: &halfRefusing{rows: []events.Pending{{Record: record}}},
				Lease:   lease{true}, URL: sink.URL, Secrets: []string{"secret"},
				Clock: c, Log: slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if stats := d.Stats(); stats != (events.Stats{}) {
				t.Errorf("an outcome the journal refused counted %+v", stats)
			}
		})
	}
}

// halfRefusing reads but does not write, which is the state a journal is in
// when the row is gone or the database went away between the two calls.
type halfRefusing struct{ rows []events.Pending }

func (h *halfRefusing) Append(context.Context, events.Record) error { return nil }
func (h *halfRefusing) Undelivered(context.Context) (int, error)    { return len(h.rows), nil }
func (h *halfRefusing) Pending(context.Context, int, time.Time) ([]events.Pending, error) {
	return h.rows, nil
}
func (h *halfRefusing) ByObject(context.Context, string, string, int) ([]events.Record, string, error) {
	return nil, "", nil
}
func (h *halfRefusing) After(context.Context, string, int64, int) ([]events.Record, error) {
	return nil, nil
}
func (h *halfRefusing) Watch(string) events.Subscription { return closedSubscription{} }
func (h *halfRefusing) Shared() bool                     { return false }
func (h *halfRefusing) Acknowledge(context.Context, string, time.Time) error {
	return errors.New("the row is gone")
}
func (h *halfRefusing) Defer(context.Context, string, time.Time) error {
	return errors.New("the row is gone")
}
func (h *halfRefusing) Drop(context.Context, string, time.Time) error {
	return errors.New("the row is gone")
}

// TestAnUnusableSinkURLDropsAtOnce: a URL this process cannot make a request
// from will not become one, so the record is ended rather than retried.
func TestAnUnusableSinkURLDropsAtOnce(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	_, j := journal(t)
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	d, err := events.NewDeliverer(events.DelivererOptions{
		Journal: j, Lease: lease{true}, URL: "https://sink.example/\x7f", Secrets: []string{"secret"},
		Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := d.Stats(); stats.Dropped != 1 {
		t.Errorf("an unusable URL counted %+v", stats)
	}
}
