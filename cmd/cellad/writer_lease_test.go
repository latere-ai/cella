// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/")

// fakeClock is time the test advances. A timer or ticker fires when advance
// reaches it, in order.
type fakeClock struct {
	mu     sync.Mutex
	at     time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at     time.Time
	period time.Duration
	ch     chan time.Time
	dead   bool
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) arm(d, period time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.at.Add(d), period: period, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.ch, func() {
		c.mu.Lock()
		t.dead = true
		c.mu.Unlock()
	}
}

func (c *fakeClock) clock() writerClock {
	return writerClock{
		now:    c.now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) { return c.arm(d, d) },
		timer:  func(d time.Duration) (<-chan time.Time, func()) { return c.arm(d, 0) },
	}
}

// advance moves time forward one step at a time, firing what falls due and
// giving the goroutines it wakes a moment to run.
func (c *fakeClock) advance(d time.Duration) {
	const step = 100 * time.Millisecond
	for ; d > 0; d -= step {
		c.mu.Lock()
		c.at = c.at.Add(min(step, d))
		var due []*fakeTimer
		for _, t := range c.timers {
			if !t.dead && !t.at.After(c.at) {
				due = append(due, t)
			}
		}
		sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
		for _, t := range due {
			select {
			case t.ch <- c.at:
			default:
			}
			if t.period > 0 {
				t.at = t.at.Add(t.period)
			} else {
				t.dead = true
			}
		}
		c.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

// storedLease is the writer lease as the store records it: the expiry is set
// from the instant a renewal is received, after the request's delay, and the
// answer travels back for a delay of its own. Once down is set the store
// answers nothing.
type storedLease struct {
	clk       *fakeClock
	mu        sync.Mutex
	requests  int
	delays    map[int][2]time.Duration
	expiry    time.Time
	down      bool
	downAfter int
}

func (l *storedLease) Holder() string { return "this-process" }

func (l *storedLease) Lease(context.Context, string) (store.Lease, error) {
	return store.Lease{}, nil
}

func (l *storedLease) wait(ctx context.Context, d time.Duration) error {
	if d == 0 {
		return nil
	}
	fire, stop := l.clk.arm(d, 0)
	defer stop()
	select {
	case <-fire:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *storedLease) Acquire(ctx context.Context, _ string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	l.requests++
	n := l.requests
	delays := l.delays[n]
	down := l.down || (l.downAfter > 0 && n > l.downAfter)
	l.mu.Unlock()
	if down {
		return false, errors.New("the store did not answer")
	}
	if err := l.wait(ctx, delays[0]); err != nil {
		return false, err
	}
	l.mu.Lock()
	l.expiry = l.clk.now().Add(ttl)
	l.mu.Unlock()
	if err := l.wait(ctx, delays[1]); err != nil {
		return false, err
	}
	return true, nil
}

// TestTheWriterStopsBeforeTheExpiryTheStoreRecorded: a renewal whose request
// and answer are both slow is granted, with the store's expiry counted from
// its receipt; the store then stops answering. The writer's deadline is
// counted from the send, so it demotes before that expiry, where a deadline
// counted from the answer would have outlived it.
func TestTheWriterStopsBeforeTheExpiryTheStoreRecorded(t *testing.T) {
	ttl := controller.LeaseTTL
	clk := &fakeClock{at: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	lease := &storedLease{
		clk: clk, downAfter: 2,
		// The first renewal is prompt. The second is received 5s after it
		// is sent and answered 5s after that.
		delays: map[int][2]time.Duration{2: {5 * time.Second, 5 * time.Second}},
	}
	start := clk.now()
	demoted := make(chan error, 1)
	go func() {
		demoted <- watchWriterLeaseOn(t.Context(), clk.clock(), lease, ttl, start, nil, slog.New(slog.DiscardHandler))
	}()
	var err error
	for range 40 {
		clk.advance(time.Second)
		select {
		case err = <-demoted:
		default:
			continue
		}
		break
	}
	if !errors.Is(err, errDemoted) {
		t.Fatalf("the writer was not demoted after the store went silent: %v", err)
	}
	lease.mu.Lock()
	expiry := lease.expiry
	lease.mu.Unlock()
	stopped := clk.now()
	// The writer decided at its deadline, and stops writing writerStopBy
	// later; both fall before the expiry the store recorded.
	if latest := stopped.Add(writerStopBy); latest.After(expiry) {
		t.Errorf("the writer stopped at %s, and its last write at %s is after the expiry %s the store recorded",
			stopped.Sub(start), latest.Sub(start), expiry.Sub(start))
	}
	// The renewal that was slow was sent 2s after the start: the deadline
	// is counted from there, not from its answer 10s later.
	if want := start.Add(2*time.Second + ttl - writerMargin(ttl)); stopped.Before(want) || stopped.After(want.Add(time.Second)) {
		t.Errorf("the writer stopped at %s, want its deadline %s", stopped.Sub(start), want.Sub(start))
	}
}

// TestTheWriterMarginCoversItsStop keeps the margin at or above the time the
// process takes to stop, which the model of the lease assumes.
func TestTheWriterMarginCoversItsStop(t *testing.T) {
	if writerMargin(controller.LeaseTTL) < writerStopBy {
		t.Fatalf("the margin %v is below the time to stop %v", writerMargin(controller.LeaseTTL), writerStopBy)
	}
	if leasePoll >= controller.LeaseTTL-writerMargin(controller.LeaseTTL) {
		t.Fatalf("the poll %v does not fit before the deadline", leasePoll)
	}
}

// writerLeaseConstants is formal/WriterLeaseConstants.tla: the writer lease's
// term, poll, margin and stop bound, in the whole seconds the TLA+ model
// counts in.
func writerLeaseConstants() string {
	return fmt.Sprintf(`\* Code generated by TestWriterLeaseConstants in cmd/cellad. DO NOT EDIT.
------------------------- MODULE WriterLeaseConstants -------------------------
TTL == %d
Poll == %d
Margin == %d
StopBy == %d
=============================================================================
`, controller.LeaseTTL/time.Second, leasePoll/time.Second, writerMargin(controller.LeaseTTL)/time.Second, writerStopBy/time.Second)
}

// TestWriterLeaseConstants holds formal/WriterLeaseConstants.tla to the Go
// constants, so the tla gate checks formal/WriterLease.tla against the term,
// poll and margin the writer runs with. Rerun with -update after changing one.
func TestWriterLeaseConstants(t *testing.T) {
	path := filepath.Join("..", "..", "formal", "WriterLeaseConstants.tla")
	want := writerLeaseConstants()
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; rerun with -update", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s differs from the writer lease constants; rerun go test ./cmd/cellad -run TestWriterLeaseConstants -update\n--- want\n%s--- got\n%s", path, want, got)
	}
}
