// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
)

// deliveryRecorder is design 017's seam under test.
type deliveryRecorder struct {
	mu        sync.Mutex
	outcomes  []string
	durations int
	leases    [][2]any
}

func (r *deliveryRecorder) EventDelivered(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *deliveryRecorder) EventDeliveryDuration(time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations++
}

func (r *deliveryRecorder) LeaseHeld(name string, held bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases = append(r.leases, [2]any{name, held})
}

func (r *deliveryRecorder) read() ([]string, int, [][2]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outcomes), r.durations, slices.Clone(r.leases)
}

// measured builds a deliverer over a journal and a sink with a recorder.
func measured(t *testing.T, j events.Journal, s *sink, c *clock, held bool, rec *deliveryRecorder) *events.Deliverer {
	t.Helper()
	d, err := events.NewDeliverer(events.DelivererOptions{
		Journal: j, Lease: lease{held}, URL: s.server.URL, Secrets: s.secrets,
		Clock: c, Log: slog.New(slog.DiscardHandler), Metrics: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestDeliveryOutcomesAreCounted is design 017's delivery counter: one count
// per attempt, under the outcome the attempt reached, beside the counts the
// deliverer already keeps for Stats.
func TestDeliveryOutcomesAreCounted(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	st, j := journal(t)
	defer func() { _ = st.Close() }()
	rec := &deliveryRecorder{}
	d := measured(t, j, s, c, true, rec)

	write(t, j, c.Now(), "sbx_ack", events.TypeCreated)
	write(t, j, c.Now(), "sbx_retry", events.TypeCreated)
	write(t, j, c.Now(), "sbx_drop", events.TypeCreated)
	s.answer("sbx_retry", http.StatusInternalServerError)
	s.answer("sbx_drop", http.StatusBadRequest)
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	outcomes, durations, leases := rec.read()
	slices.Sort(outcomes)
	want := []string{events.MetricAcknowledged, events.MetricDeferred, events.MetricDropped}
	if !slices.Equal(outcomes, want) {
		t.Errorf("delivery outcomes %v, want %v", outcomes, want)
	}
	if durations != 3 {
		t.Errorf("observed %d attempts, want one per record posted", durations)
	}
	if !slices.Equal(leases, [][2]any{{events.MetricLeaseJournal, true}}) {
		t.Errorf("the pass reported %v, want the journal lease held", leases)
	}
	// The counts design 042 keeps and design 017's counter agree, because
	// they are incremented on the same three lines.
	stats := d.Stats()
	if stats.Delivered != 1 || stats.Deferred != 1 || stats.Dropped != 1 {
		t.Errorf("Stats reads %+v, want one of each", stats)
	}
}

// TestDeliveryWithoutAPostIsCountedAndNotTimed is the record that is dropped
// before a request was made: nothing was attempted, so nothing is timed.
func TestDeliveryWithoutAPostIsCountedAndNotTimed(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	st, j := journal(t)
	defer func() { _ = st.Close() }()
	rec := &deliveryRecorder{}
	d := measured(t, j, s, c, true, rec)

	write(t, j, c.Now(), "sbx_stale", events.TypeCreated)
	c.advance(events.DefaultRetryWindow + time.Minute)
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	outcomes, durations, _ := rec.read()
	if !slices.Equal(outcomes, []string{events.MetricDropped}) {
		t.Errorf("outcomes %v, want one drop", outcomes)
	}
	if durations != 0 {
		t.Errorf("observed %d attempts on a record that was never posted", durations)
	}
}

// TestLeaseNotHeldIsReported is the gauge an alert reads: a replica that does
// not hold the journal lease says so on every pass.
func TestLeaseNotHeldIsReported(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	st, j := journal(t)
	defer func() { _ = st.Close() }()
	rec := &deliveryRecorder{}
	d := measured(t, j, s, c, false, rec)
	if moved, err := d.Pass(t.Context()); err != nil || moved != 0 {
		t.Fatalf("a pass without the lease moved %d: %v", moved, err)
	}
	_, _, leases := rec.read()
	if !slices.Equal(leases, [][2]any{{events.MetricLeaseJournal, false}}) {
		t.Errorf("the pass reported %v, want the journal lease lost", leases)
	}
}

// TestNoRecorderDeliversTheSame is the default: a deliverer built with no
// recorder behaves as it did before design 017.
func TestNoRecorderDeliversTheSame(t *testing.T) {
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	st, j := journal(t)
	defer func() { _ = st.Close() }()
	d := deliverer(t, j, s, c, true)
	write(t, j, c.Now(), "sbx_plain", events.TypeCreated)
	drain(t, d)
	if got := d.Stats(); got.Delivered != 1 {
		t.Errorf("Stats reads %+v, want one delivered", got)
	}
}
