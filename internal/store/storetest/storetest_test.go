// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"context"
	"errors"
	"fmt"
	gort "runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	driver "latere.ai/x/cella/runtime"
)

// TestSuiteHoldsTheMemoryAdapter is the suite against an adapter that is
// right: every case passes, which is what the adapters' own tests assert too
// and what makes the cases below meaningful.
func TestSuiteHoldsTheMemoryAdapter(t *testing.T) {
	Run(t, func(t TB, key []byte) store.Store {
		s, err := memory.Open(memory.Options{Key: key})
		if err != nil {
			t.Fatalf("opening the memory store: %v", err)
		}
		return s
	})
}

// TestSuiteCatchesAStoreThatAnswersNothing and
// TestSuiteCatchesAStoreThatFailsEverything drive every case against an
// adapter that is wrong in the two ways an adapter can be: one that reports
// success and keeps nothing, and one that reports a failure for every
// statement. A suite that only ever passes says nothing about what it would
// catch, so each case is required to fail against both.
func TestSuiteCatchesAStoreThatAnswersNothing(t *testing.T) {
	forEachCase(t, func(TB, []byte) store.Store { return &stubStore{} })
}

func TestSuiteCatchesAStoreThatFailsEverything(t *testing.T) {
	failing := errors.New("the store is unreachable")
	forEachCase(t, func(TB, []byte) store.Store { return &stubStore{err: failing} })
}

// forEachCase runs every case against one wrong adapter and requires each to
// report a failure.
func forEachCase(t *testing.T, open Opener) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{}
			r.run(func() { c.run(r, open) })
			if !r.failed() {
				t.Errorf("the case passed a store that does not hold the contract")
			}
		})
	}
}

// recorder implements TB so a case runs outside go test and its assertions are
// read back. Fatalf ends the case the way testing does, by leaving the
// goroutine that runs it.
type recorder struct {
	mu       sync.Mutex
	errors   []string
	fatal    string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.mu.Lock()
	r.fatal = fmt.Sprintf(format, args...)
	r.mu.Unlock()
	gort.Goexit()
}

func (r *recorder) Cleanup(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, f)
}

// run drives one case to its end, Fatalf included, and runs what it registered
// for cleanup afterwards, in the order testing runs them.
func (r *recorder) run(fn func()) {
	var wg sync.WaitGroup
	wg.Go(func() {
		fn()
	})
	wg.Wait()
	r.mu.Lock()
	cleanups := r.cleanups
	r.cleanups = nil
	r.mu.Unlock()
	for _, cleanup := range slices.Backward(cleanups) {
		cleanup()
	}
}

func (r *recorder) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errors) > 0 || r.fatal != ""
}

// stubStore holds the contract's shape and none of its behaviour: every write
// is accepted and forgotten, every read is empty, and every call reports err.
type stubStore struct{ err error }

func (s *stubStore) Tx(_ context.Context, fn func(store.Tx) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(stubTx{})
}
func (s *stubStore) Durable() bool               { return false }
func (s *stubStore) Ready(context.Context) error { return s.err }
func (s *stubStore) Close() error                { return s.err }

type stubTx struct{}

func (stubTx) Desired() store.Desired   { return stubDesired{} }
func (stubTx) Observed() store.Observed { return stubObserved{} }
func (stubTx) Journal() store.Journal   { return stubJournal{} }
func (stubTx) Values() store.Values     { return stubValues{} }
func (stubTx) Leases() store.Leases     { return stubLeases{} }

type stubDesired struct{}

func (stubDesired) Put(context.Context, store.Object, int64) (int64, error) { return 1, nil }
func (stubDesired) Get(context.Context, string, string) (store.Object, error) {
	return store.Object{}, nil
}
func (stubDesired) ByName(context.Context, string, string, string) (store.Object, error) {
	return store.Object{}, nil
}
func (stubDesired) List(context.Context, string, store.Filter, store.Page) ([]store.Object, string, error) {
	return nil, "", nil
}
func (stubDesired) Delete(context.Context, string, string) error            { return nil }
func (stubDesired) Count(context.Context, string, string) (int, error)      { return 0, nil }
func (stubDesired) PutStatus(context.Context, string, string, []byte) error { return nil }
func (stubDesired) LastApplied(context.Context, string) ([]byte, error)     { return []byte(`{}`), nil }
func (stubDesired) SetLastApplied(context.Context, string, []byte) error    { return nil }

type stubObserved struct{}

func (stubObserved) Put(context.Context, string, driver.State) error { return nil }
func (stubObserved) Get(context.Context, string) (driver.State, string, error) {
	return driver.State{}, "", nil
}
func (stubObserved) List(context.Context, store.Filter, store.Page) ([]driver.State, string, error) {
	return nil, "", nil
}
func (stubObserved) Rebuild(context.Context, string, []driver.State) error { return nil }

type stubJournal struct{}

func (stubJournal) Append(context.Context, store.Event) (int64, error) { return 1, nil }
func (stubJournal) ByObject(context.Context, string, store.Page) ([]store.Event, string, error) {
	return nil, "", nil
}
func (stubJournal) Prune(context.Context, time.Time) (int, error) { return 0, nil }
func (stubJournal) Pending(context.Context, int, time.Time) ([]store.Event, error) {
	return nil, nil
}
func (stubJournal) Acknowledge(context.Context, string, time.Time) error { return nil }
func (stubJournal) Defer(context.Context, string, time.Time) error       { return nil }
func (stubJournal) Drop(context.Context, string, time.Time) error        { return nil }

type stubValues struct{}

func (stubValues) Put(context.Context, string, []byte) (int, error) { return 1, nil }
func (stubValues) Open(context.Context, string) ([]byte, int, error) {
	return []byte("wrong"), 1, nil
}
func (stubValues) Delete(context.Context, string) error { return nil }

type stubLeases struct{}

func (stubLeases) Acquire(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (stubLeases) Release(context.Context, string, string) error { return nil }
