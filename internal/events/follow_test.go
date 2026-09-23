// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
)

// ring opens a memory journal that keeps at most limit records per object,
// with zero keeping every one, and an emitter over it.
func ring(t *testing.T, limit int) (events.Journal, *events.Emitter) {
	t.Helper()
	s, err := memory.Open(memory.Options{JournalCap: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	j := store.EventJournal(s, store.Journaled)
	return j, events.NewEmitter(j, slog.New(slog.DiscardHandler))
}

// writeN appends n records for one object.
func writeN(t *testing.T, j events.Journal, object string, n int) {
	t.Helper()
	for range n {
		write(t, j, time.Now().UTC(), object, events.TypeExec)
	}
}

// collect reads from a follower until it holds n records. The deadline only
// turns a follower that stopped delivering into a failure rather than a hung
// test; every wait below ends on a record arriving.
func collect(t *testing.T, f *events.Follower, n int) []events.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var out []events.Record
	for len(out) < n {
		records, err := f.Next(ctx)
		if err != nil {
			t.Fatalf("the follower ended after %d of %d records: %v", len(out), n, err)
		}
		out = append(out, records...)
	}
	if len(out) != n {
		t.Fatalf("the follower sent %d records, want %d: %v", len(out), n, seqsOf(out))
	}
	return out
}

// until reads from a follower until it ends and returns what it sent and why
// it ended.
func until(t *testing.T, f *events.Follower) ([]events.Record, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var out []events.Record
	for {
		records, err := f.Next(ctx)
		if err != nil {
			return out, err
		}
		out = append(out, records...)
	}
}

// quiet asserts that a follower has nothing more to send: nothing arrives in
// the interval, which is the one wait here that cannot end on an event.
func quiet(t *testing.T, f *events.Follower) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if records, err := f.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the follower sent %v (%v) after everything had been sent", seqsOf(records), err)
	}
}

func seqsOf(records []events.Record) []int64 {
	out := make([]int64, 0, len(records))
	for _, r := range records {
		out = append(out, r.Seq)
	}
	return out
}

// span is from..to inclusive.
func span(from, to int64) []int64 {
	var out []int64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// TestFollowReplaysThenStaysLive: a follower sends the records after its
// cursor, in more than one read of the journal, then the ones committed while
// it caught up, each once, and then a record committed while it waits.
func TestFollowReplaysThenStaysLive(t *testing.T) {
	j, e := ring(t, 0)
	writeN(t, j, "sbx_a", events.FollowBatch+50)
	writeN(t, j, "sbx_b", 1)
	f, err := e.Follow(t.Context(), "sbx_a", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Two records committed after the follow opened and before it read
	// anything: the history read and the subscription each carry them.
	writeN(t, j, "sbx_a", 2)
	replayed := collect(t, f, events.FollowBatch+50)
	if got, want := seqsOf(replayed), span(3, events.FollowBatch+52); !slices.Equal(got, want) {
		t.Fatalf("the replay sent %v, want 3 to %d once each", got, events.FollowBatch+52)
	}
	for _, r := range replayed {
		if r.Object.ID != "sbx_a" {
			t.Fatalf("the follower of sbx_a sent a record of %s", r.Object.ID)
		}
	}
	quiet(t, f)

	next := make(chan []events.Record, 1)
	go func() {
		records, err := f.Next(t.Context())
		if err != nil {
			t.Errorf("the live read ended: %v", err)
		}
		next <- records
	}()
	writeN(t, j, "sbx_a", 1)
	if got := seqsOf(<-next); !slices.Equal(got, []int64{events.FollowBatch + 53}) {
		t.Fatalf("the live read sent %v", got)
	}
	quiet(t, f)
}

// TestFollowFromNow: without a cursor a follower starts at the object's
// newest record, including for an object that has none yet.
func TestFollowFromNow(t *testing.T) {
	j, e := ring(t, 0)
	writeN(t, j, "sbx_a", 3)
	for object, want := range map[string]int64{"sbx_a": 4, "sbx_new": 1} {
		f, err := e.Follow(t.Context(), object, events.FromNow)
		if err != nil {
			t.Fatal(err)
		}
		writeN(t, j, object, 1)
		if got := seqsOf(collect(t, f, 1)); !slices.Equal(got, []int64{want}) {
			t.Errorf("a follower of %s from now sent %v, want %d", object, got, want)
		}
		quiet(t, f)
		f.Close()
	}
	for _, kind := range []events.Type{events.TypeDeleted, events.TypeSecretDeleted, events.TypeEnvironmentDeleted} {
		if !events.Ends(kind) {
			t.Errorf("%s does not end its object's feed", kind)
		}
	}
	if events.Ends(events.TypeStopped) {
		t.Error("a stop ends its object's feed")
	}
}

// TestFollowRestoresTheSequence: records handed over in the other order than
// they committed, which a store that publishes after its commit can do, are
// sent in sequence order, and the late copy is not sent again.
func TestFollowRestoresTheSequence(t *testing.T) {
	j := newTail()
	e := events.NewEmitter(j, slog.New(slog.DiscardHandler))
	f, err := e.Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	one, two := j.commit("sbx_a"), j.commit("sbx_a")
	j.publish(two)
	if got := seqsOf(collect(t, f, 2)); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("the follower sent %v, want 1 and 2 in order", got)
	}
	j.publish(one)
	j.publish(j.commit("sbx_a"))
	if got := seqsOf(collect(t, f, 1)); !slices.Equal(got, []int64{3}) {
		t.Fatalf("after the late copy of 1 the follower sent %v, want 3", got)
	}
	quiet(t, f)
}

// TestFollowResubscribesWhenBehind: a follower whose subscription dropped
// records because it did not read reads them from the journal, in order and
// without a gap.
func TestFollowResubscribesWhenBehind(t *testing.T) {
	j, e := ring(t, 0)
	f, err := e.Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := store.SubscriptionBuffer + 44
	writeN(t, j, "sbx_a", n)
	if got := seqsOf(collect(t, f, n)); !slices.Equal(got, span(1, int64(n))) {
		t.Fatalf("the follower sent %v, want 1 to %d", got, n)
	}
	quiet(t, f)
}

// TestFollowRefusesAPositionItCannotServe: a cursor whose next record the ring
// dropped is expired, one above the newest is ahead, one at the edge of what
// is held is served, and a follower that falls behind its subscription and
// the ring ends as expired after the records it did receive.
func TestFollowRefusesAPositionItCannotServe(t *testing.T) {
	j, e := ring(t, 3)
	writeN(t, j, "sbx_a", 10)
	for cursor, want := range map[int64]error{2: events.ErrExpired, 11: events.ErrAhead, -5: events.ErrAhead} {
		if _, err := e.Follow(t.Context(), "sbx_a", cursor); !errors.Is(err, want) {
			t.Errorf("a follow from %d answered %v, want %v", cursor, err, want)
		}
	}
	f, err := e.Follow(t.Context(), "sbx_a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(collect(t, f, 3)); !slices.Equal(got, []int64{8, 9, 10}) {
		t.Errorf("a follow from 7 sent %v, want 8, 9, 10", got)
	}
	f.Close()

	f, err = e.Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeN(t, j, "sbx_a", store.SubscriptionBuffer+50)
	sent, err := until(t, f)
	if !errors.Is(err, events.ErrExpired) {
		t.Fatalf("a follower a ring behind ended with %v, want ErrExpired", err)
	}
	if got, want := seqsOf(sent), span(11, 10+int64(store.SubscriptionBuffer)); !slices.Equal(got, want) {
		t.Fatalf("before it ended the follower sent %v, want what its subscription held", got)
	}

	// A gap inside one read is served up to the gap, and the gap is expired.
	gapped := newTail()
	gapped.commit("sbx_a")
	gapped.commit("sbx_a")
	gapped.seq["sbx_a"]++
	gapped.commit("sbx_a")
	g, err := events.NewEmitter(gapped, slog.New(slog.DiscardHandler)).Follow(t.Context(), "sbx_a", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	sent, err = until(t, g)
	if !errors.Is(err, events.ErrExpired) || !slices.Equal(seqsOf(sent), []int64{1, 2}) {
		t.Fatalf("a journal missing 3 sent %v and ended with %v", seqsOf(sent), err)
	}

	// A journal that cannot answer is an error at open and during the read.
	broken := errors.New("the journal is gone")
	failing := newTail()
	failing.afterErr = broken
	if _, err := events.NewEmitter(failing, slog.New(slog.DiscardHandler)).Follow(t.Context(), "sbx_a", 0); !errors.Is(err, broken) {
		t.Errorf("a follow over a journal that cannot read answered %v", err)
	}
	if _, err := events.NewEmitter(&refusing{err: broken}, slog.New(slog.DiscardHandler)).Follow(t.Context(), "sbx_a", 0); !errors.Is(err, broken) {
		t.Errorf("a follow over a journal that cannot page answered %v", err)
	}
	late := newTail()
	h, err := events.NewEmitter(late, slog.New(slog.DiscardHandler)).Follow(t.Context(), "sbx_a", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	late.afterErr = broken
	if _, err := h.Next(t.Context()); !errors.Is(err, broken) {
		t.Errorf("a read the journal refused answered %v", err)
	}

	var none *events.Emitter
	if _, err := none.Follow(t.Context(), "sbx_a", 0); !errors.Is(err, events.ErrNoJournal) {
		t.Errorf("a follow of no emitter answered %v", err)
	}
	if _, err := none.FollowAll(); !errors.Is(err, events.ErrNoJournal) {
		t.Errorf("a follow of every object of no emitter answered %v", err)
	}
}

// TestFollowAllFromNow: a follower of every object sends each object's
// records committed after it opened, in the order they committed, and ends
// when it falls behind, because what it dropped cannot be read back.
func TestFollowAllFromNow(t *testing.T) {
	j, e := ring(t, 0)
	writeN(t, j, "sbx_a", 1)
	f, err := e.FollowAll()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeN(t, j, "sbx_a", 1)
	writeN(t, j, "sbx_b", 1)
	writeN(t, j, "sbx_a", 1)
	var got []string
	for _, r := range collect(t, f, 3) {
		got = append(got, fmt.Sprintf("%s:%d", r.Object.ID, r.Seq))
	}
	if want := []string{"sbx_a:2", "sbx_b:1", "sbx_a:3"}; !slices.Equal(got, want) {
		t.Fatalf("the follower of every object sent %v, want %v", got, want)
	}
	writeN(t, j, "sbx_c", store.SubscriptionBuffer+1)
	sent, err := until(t, f)
	if !errors.Is(err, events.ErrBehind) || len(sent) != store.SubscriptionBuffer {
		t.Fatalf("a follower of every object that fell behind sent %d records and ended with %v", len(sent), err)
	}
}

// TestFollowReadsASharedJournal: a follower of one object on a journal
// another process appends to reads the journal on its interval, so a record
// nothing published here still arrives; a cancelled read is the caller's
// cancellation and not the interval.
func TestFollowReadsASharedJournal(t *testing.T) {
	j := newTail()
	j.shared = true
	e := events.NewEmitter(j, slog.New(slog.DiscardHandler))
	events.SetFollowPoll(e, 5*time.Millisecond)
	f, err := e.Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	j.commit("sbx_a")
	if got := seqsOf(collect(t, f, 1)); !slices.Equal(got, []int64{1}) {
		t.Fatalf("the follower of a shared journal sent %v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read answered %v", err)
	}
	// The default interval applies where no test shortened it.
	d, err := events.NewEmitter(j, slog.New(slog.DiscardHandler)).Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

// tail is a journal whose commits and publications a test makes separately,
// which is the one way to hand a follower records in an order no store
// produces on demand.
type tail struct {
	mu       sync.Mutex
	records  map[string][]events.Record
	seq      map[string]int64
	subs     []*channelSubscription
	shared   bool
	afterErr error
}

func newTail() *tail {
	return &tail{records: map[string][]events.Record{}, seq: map[string]int64{}}
}

// commit stores the object's next record without publishing it, as another
// replica's commit reaches this process.
func (j *tail) commit(object string) events.Record {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq[object]++
	r := events.Record{ID: events.NewID(), Seq: j.seq[object], Type: events.TypeExec,
		Object: events.Object{Kind: events.KindSandbox, ID: object}}
	j.records[object] = append(j.records[object], r)
	return r
}

// publish hands one record to every open subscription of its object.
func (j *tail) publish(r events.Record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.subs {
		if s.object == "" || s.object == r.Object.ID {
			s.c <- r
		}
	}
}

func (j *tail) After(_ context.Context, object string, seq int64, limit int) ([]events.Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.afterErr != nil {
		return nil, j.afterErr
	}
	var out []events.Record
	for _, r := range j.records[object] {
		if r.Seq > seq && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (j *tail) ByObject(_ context.Context, object, _ string, limit int) ([]events.Record, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := slices.Clone(j.records[object])
	slices.Reverse(out)
	return out[:min(limit, len(out))], "", nil
}

func (j *tail) Watch(object string) events.Subscription {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := &channelSubscription{object: object, c: make(chan events.Record, 16)}
	j.subs = append(j.subs, s)
	return s
}

func (j *tail) Shared() bool                                { return j.shared }
func (j *tail) Append(context.Context, events.Record) error { return nil }
func (j *tail) Pending(context.Context, int, time.Time) ([]events.Pending, error) {
	return nil, nil
}
func (j *tail) Undelivered(context.Context) (int, error)             { return 0, nil }
func (j *tail) Acknowledge(context.Context, string, time.Time) error { return nil }
func (j *tail) Defer(context.Context, string, time.Time) error       { return nil }
func (j *tail) Drop(context.Context, string, time.Time) error        { return nil }

type channelSubscription struct {
	object string
	c      chan events.Record
}

func (s *channelSubscription) Next(ctx context.Context) (events.Record, error) {
	select {
	case r := <-s.c:
		return r, nil
	case <-ctx.Done():
		return events.Record{}, ctx.Err()
	}
}

func (s *channelSubscription) Close() {}
