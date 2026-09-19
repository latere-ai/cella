// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
)

// sink is the operator's endpoint as a test holds it: it verifies every
// signature with the mirror of the platform's verifier above, records what
// arrived in order, and answers whatever the case asks it to.
type sink struct {
	server  *httptest.Server
	secrets []string
	// now is the sink's clock. A test advances one clock for both ends,
	// because in a deployment the two read the same wall clock and the
	// freshness window is what holds them together.
	now func() time.Time

	mu      sync.Mutex
	got     []events.Record
	answers map[string]int
	failing map[string]int
}

func newSink(t *testing.T, c *clock, secrets ...string) *sink {
	t.Helper()
	s := &sink{secrets: secrets, now: c.Now, answers: map[string]int{}, failing: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sink) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "the delivery carried a bearer", http.StatusBadRequest)
		return
	}
	if err := verify(s.secrets, r.Header.Get(events.SignatureHeader), body, s.now()); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var record events.Record
	if err := json.Unmarshal(body, &record); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if left := s.failing[record.Object.ID]; left > 0 {
		s.failing[record.Object.ID] = left - 1
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if code, queued := s.answers[record.Object.ID]; queued {
		delete(s.answers, record.Object.ID)
		w.WriteHeader(code)
		return
	}
	s.got = append(s.got, record)
	w.WriteHeader(http.StatusOK)
}

// types is what arrived, in the order it did.
func (s *sink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.got))
	for _, r := range s.got {
		out = append(out, string(r.Object.ID)+"/"+string(r.Type)+"#"+itoa(r.Seq))
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// failNext makes the next n deliveries about one object answer 500. It is
// per object because one pass delivers several objects at once and a test
// that failed whichever arrived first would be racing itself.
func (s *sink) failNext(object string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[object] = n
}

// answer queues one status for the next delivery about one object.
func (s *sink) answer(object string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[object] = code
}

// alwaysHeld is the single-process lease; heldBy reports what a replica that
// lost the lease sees.
type lease struct{ held bool }

func (l lease) Acquire(context.Context, string, time.Duration) (bool, error) { return l.held, nil }

// clock is the deliverer's time under a test's control.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// journal opens a memory store and the journal over it.
func journal(t *testing.T) (store.Store, events.Journal) {
	t.Helper()
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, store.EventJournal(s, store.Delivered)
}

// write appends one record for one object, with the time the clock reads.
func write(t *testing.T, j events.Journal, at time.Time, object string, kind events.Type) {
	t.Helper()
	record, err := events.Mutation(kind, "", events.Object{
		Kind: events.KindSandbox, ID: object, Name: object, Owner: "alice",
		Labels: map[string]string{"tenant": "acme"},
	}, events.Phase{Phase: "Running"}, events.Actor{}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(t.Context(), record); err != nil {
		t.Fatal(err)
	}
}

// deliverer builds one over a journal and a sink.
func deliverer(t *testing.T, j events.Journal, s *sink, c *clock, held bool) *events.Deliverer {
	t.Helper()
	d, err := events.NewDeliverer(events.DelivererOptions{
		Journal: j, Lease: lease{held}, URL: s.server.URL, Secrets: s.secrets,
		Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// drain runs passes until nothing more moves or the bound is reached, so a
// test states the outcome rather than the number of rounds.
func drain(t *testing.T, d *events.Deliverer) {
	t.Helper()
	for range 20 {
		moved, err := d.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if moved == 0 {
			return
		}
	}
}

// TestDeliveryIsOrderedPerObject: one object's records arrive in sequence
// order, and a deferred head holds its own object's successors while another
// object's flow.
func TestDeliveryIsOrderedPerObject(t *testing.T) {
	_, j := journal(t)
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	for _, kind := range []events.Type{events.TypeCreated, events.TypeStarted, events.TypeStopped} {
		write(t, j, c.now, "sbx_a", kind)
	}
	write(t, j, c.now, "sbx_b", events.TypeCreated)
	d := deliverer(t, j, s, c, true)

	// The first pass takes each object's head, and sbx_a's fails.
	s.failNext("sbx_a", 1)
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.types(); len(got) != 1 || got[0] != "sbx_b/sandbox.created#1" {
		t.Fatalf("the first pass delivered %v, want only the other object's head", got)
	}
	// Until the backoff lapses, sbx_a delivers nothing: its successors wait
	// behind the record that failed.
	if _, err := d.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.types(); len(got) != 1 {
		t.Fatalf("a deferred head let its successors past: %v", got)
	}
	c.advance(2 * events.MinBackoff)
	drain(t, d)
	want := []string{
		"sbx_b/sandbox.created#1",
		"sbx_a/sandbox.created#1",
		"sbx_a/sandbox.started#2",
		"sbx_a/sandbox.stopped#3",
	}
	got := s.types()
	if len(got) != len(want) {
		t.Fatalf("the sink took %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the sink took %v, want %v", got, want)
		}
	}
	if stats := d.Stats(); stats.Delivered != 4 || stats.Dropped != 0 || stats.Deferred != 1 {
		t.Errorf("the counts are %+v", stats)
	}
}

// TestOrderUnderConcurrentMutations: many objects mutated at once each keep
// their own order, and every record arrives once.
func TestOrderUnderConcurrentMutations(t *testing.T) {
	_, j := journal(t)
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	kinds := []events.Type{events.TypeCreated, events.TypeStarted, events.TypeStopped, events.TypeDeleted}
	objects := []string{"sbx_a", "sbx_b", "sbx_c", "sbx_d", "sbx_e"}
	var wg sync.WaitGroup
	for _, object := range objects {
		wg.Go(func() {
			for _, kind := range kinds {
				write(t, j, c.now, object, kind)
			}
		})
	}
	wg.Wait()
	drain(t, deliverer(t, j, s, c, true))

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) != len(objects)*len(kinds) {
		t.Fatalf("the sink took %d record(s), want %d", len(s.got), len(objects)*len(kinds))
	}
	seen := map[string][]events.Record{}
	ids := map[string]bool{}
	for _, r := range s.got {
		if ids[r.ID] {
			t.Errorf("%s arrived twice", r.ID)
		}
		ids[r.ID] = true
		seen[r.Object.ID] = append(seen[r.Object.ID], r)
	}
	for object, records := range seen {
		for i, r := range records {
			if r.Seq != int64(i+1) || r.Type != kinds[i] {
				t.Errorf("%s took %s at sequence %d, position %d", object, r.Type, r.Seq, i)
			}
			if r.Object.Labels["tenant"] != "acme" {
				t.Errorf("%s arrived without its labels", r.ID)
			}
		}
	}
}

// TestDeliveryRetries: a sink that fails three times takes the record on the
// fourth attempt, and the object's later records follow it.
func TestDeliveryRetries(t *testing.T) {
	_, j := journal(t)
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	write(t, j, c.now, "sbx_a", events.TypeStarted)
	d := deliverer(t, j, s, c, true)
	s.failNext("sbx_a", 3)
	for range 3 {
		if _, err := d.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.advance(events.MaxBackoff)
	}
	if got := s.types(); len(got) != 0 {
		t.Fatalf("the failing sink recorded %v", got)
	}
	drain(t, d)
	got := s.types()
	if len(got) != 2 || got[0] != "sbx_a/sandbox.created#1" || got[1] != "sbx_a/sandbox.started#2" {
		t.Fatalf("after the sink recovered it took %v", got)
	}
	if stats := d.Stats(); stats.Delivered != 2 || stats.Deferred != 3 {
		t.Errorf("the counts are %+v", stats)
	}
}

// TestDropRules: a 400 drops at once, a 401 is held because the two ends
// disagree about the secret rather than about the bytes, and a record past
// the retry window is dropped and counted.
func TestDropRules(t *testing.T) {
	t.Run("a refused record drops at once", func(t *testing.T) {
		_, j := journal(t)
		c := &clock{now: time.Now().UTC()}
		s := newSink(t, c, "secret")
		write(t, j, c.now, "sbx_a", events.TypeCreated)
		write(t, j, c.now, "sbx_a", events.TypeStarted)
		d := deliverer(t, j, s, c, true)
		s.answer("sbx_a", http.StatusBadRequest)
		drain(t, d)
		if got := s.types(); len(got) != 1 || got[0] != "sbx_a/sandbox.started#2" {
			t.Fatalf("the sink took %v, want the successor of the dropped record", got)
		}
		if stats := d.Stats(); stats.Dropped != 1 || stats.Delivered != 1 {
			t.Errorf("the counts are %+v", stats)
		}
	})
	t.Run("a bad signature is held, not dropped", func(t *testing.T) {
		_, j := journal(t)
		c := &clock{now: time.Now().UTC()}
		s := newSink(t, c, "the sink's secret")
		write(t, j, c.now, "sbx_a", events.TypeCreated)
		d, err := events.NewDeliverer(events.DelivererOptions{
			Journal: j, Lease: lease{true}, URL: s.server.URL,
			Secrets: []string{"the core's secret"}, Clock: c, Log: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := d.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			c.advance(events.MaxBackoff)
		}
		if stats := d.Stats(); stats.Dropped != 0 || stats.Deferred != 3 {
			t.Fatalf("a signature fault dropped a record: %+v", stats)
		}
		// Once the secrets agree the held record is delivered, which is what
		// deferring rather than dropping preserved.
		s.secrets = []string{"the core's secret"}
		drain(t, d)
		if got := s.types(); len(got) != 1 {
			t.Fatalf("the repaired sink took %v", got)
		}
	})
	t.Run("a record past the window drops", func(t *testing.T) {
		_, j := journal(t)
		c := &clock{now: time.Now().UTC()}
		s := newSink(t, c, "secret")
		write(t, j, c.now, "sbx_a", events.TypeCreated)
		write(t, j, c.now, "sbx_a", events.TypeStarted)
		d, err := events.NewDeliverer(events.DelivererOptions{
			Journal: j, Lease: lease{true}, URL: s.server.URL, Secrets: s.secrets,
			RetryWindow: time.Hour, Clock: c, Log: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		s.failNext("sbx_a", 1)
		if _, err := d.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.advance(2 * time.Hour)
		drain(t, d)
		if stats := d.Stats(); stats.Dropped != 2 {
			t.Fatalf("the counts are %+v, want both records dropped past the window", stats)
		}
		if got := s.types(); len(got) != 0 {
			t.Fatalf("a record past the window reached the sink: %v", got)
		}
	})
}

// TestDeliveryNeedsTheLease: a replica that does not hold the lease posts
// nothing, and the records stay for the one that does.
func TestDeliveryNeedsTheLease(t *testing.T) {
	_, j := journal(t)
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	follower := deliverer(t, j, s, c, false)
	moved, err := follower.Pass(t.Context())
	if err != nil || moved != 0 {
		t.Fatalf("a replica without the lease moved %d record(s): %v", moved, err)
	}
	if got := s.types(); len(got) != 0 {
		t.Fatalf("a replica without the lease delivered %v", got)
	}
	drain(t, deliverer(t, j, s, c, true))
	if got := s.types(); len(got) != 1 {
		t.Fatalf("the lease holder delivered %v", got)
	}
}

// TestDeliveryRefusesAnIncompleteConfiguration: a deliverer with no sink or
// no secret is a build failure here, so a start-up cannot reach the loop.
func TestDeliveryRefusesAnIncompleteConfiguration(t *testing.T) {
	_, j := journal(t)
	for _, o := range []events.DelivererOptions{
		{Lease: lease{true}, URL: "https://sink.example", Secrets: []string{"s"}},
		{Journal: j, URL: "https://sink.example", Secrets: []string{"s"}},
		{Journal: j, Lease: lease{true}, Secrets: []string{"s"}},
		{Journal: j, Lease: lease{true}, URL: "https://sink.example"},
	} {
		if _, err := events.NewDeliverer(o); err == nil {
			t.Errorf("%+v built a deliverer", o)
		}
	}
}

// TestRunStopsWithItsContext: the loop ends when the process drains, and what
// it had not posted stays on the journal.
func TestRunStopsWithItsContext(t *testing.T) {
	_, j := journal(t)
	c := &clock{now: time.Now().UTC()}
	s := newSink(t, c, "secret")
	write(t, j, c.now, "sbx_a", events.TypeCreated)
	d := deliverer(t, j, s, c, true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for len(s.types()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the loop delivered nothing")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
}

// TestJournaledWithoutASinkIsNotPending: with no sink configured a record is
// stored acknowledged, so the per-object feed still reads it and the journal
// does not grow a row nothing will ever post.
func TestJournaledWithoutASinkIsNotPending(t *testing.T) {
	s, _ := journal(t)
	off := store.EventJournal(s, store.Journaled)
	write(t, off, time.Now().UTC(), "sbx_a", events.TypeCreated)
	rows, err := off.Pending(t.Context(), 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a record with no sink is pending: %+v", rows)
	}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		held, _, err := tx.Journal().ByObject(t.Context(), "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(held) != 1 {
			t.Errorf("the feed reads %d record(s)", len(held))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
