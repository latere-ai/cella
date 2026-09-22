// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRetention records the instants it was asked to prune at and answers
// what a test told it to.
type fakeRetention struct {
	mu    sync.Mutex
	asked []time.Time
	err   error
}

func (r *fakeRetention) Prune(_ context.Context, now time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, now)
	return len(r.asked), r.err
}

func (r *fakeRetention) calls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.asked...)
}

// TestReapPrunesTheJournal: the reaper's tick runs the store's retention once,
// at the controller's clock, and only while this replica holds the reaper
// lease, so one replica prunes.
func TestReapPrunesTheJournal(t *testing.T) {
	retention := &fakeRetention{}
	lease := &fakeLease{}
	c, _, clock := newFake(t, Options{Retention: retention, Lease: lease})
	c.tick(t.Context())
	if got := retention.calls(); len(got) != 0 {
		t.Fatalf("a replica without the reaper lease pruned %d times", len(got))
	}
	lease.answer(true, nil)
	c.tick(t.Context())
	got := retention.calls()
	if len(got) != 1 || !got[0].Equal(clock.Now()) {
		t.Fatalf("one tick pruned at %v, want once at %v", got, clock.Now())
	}
}

// TestReapReportsAFailedPrune: a prune the store cannot run is reported with
// the tick's other failures, and the rules the tick applies still run.
func TestReapReportsAFailedPrune(t *testing.T) {
	retention := &fakeRetention{err: errors.New("the journal table is locked")}
	c, _, _ := newFake(t, Options{Retention: retention})
	created(t, c, "work")
	acted, err := c.Reap(t.Context())
	if err == nil {
		t.Fatal("a failed prune was not reported")
	}
	if acted != 0 || len(retention.calls()) != 1 {
		t.Fatalf("the tick acted %d times and pruned %d", acted, len(retention.calls()))
	}
	if _, err := c.Get(t.Context(), "work", "alice"); err != nil {
		t.Fatalf("a failed prune touched a sandbox: %v", err)
	}
}
