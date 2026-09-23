// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package storetest is the contract of design 010 as one suite. Every adapter
// runs it and the adapters are therefore interchangeable: what the memory
// store answers is what the Postgres store answers, and a controller written
// against one runs on the other.
//
// Two rules the suite holds every adapter to, because Postgres holds them and
// a memory store that did not would be the weaker contract. A statement that
// fails ends its transaction: the caller rolls back and retries rather than
// writing on. And a JSON column is a value and not a byte string: what comes
// back is the same document, not the same spelling of it.
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	driver "latere.ai/x/cella/runtime"
)

// TB is the part of testing.TB the cases use. testing.TB cannot be implemented
// outside package testing, so the suite's own test drives the cases through a
// recorder that implements this instead: a suite that only ever passes says
// nothing about the adapter it would catch.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Opener returns an empty store sealing secret values under key, which is nil
// for a store opened without one. The suite closes what it opens.
type Opener func(t TB, key []byte) store.Store

// Key is the secret key the suite opens its stores with: 32 bytes, fixed, so
// a failure is reproducible.
var Key = []byte("0123456789abcdef0123456789abcdef")

// cases is the contract, one function per part of it.
var cases = []struct {
	name string
	run  func(TB, Opener)
}{
	{"Versions", versions},
	{"Names", names},
	{"Count", count},
	{"Lists", lists},
	{"Status", status},
	{"Transactions", transactions},
	{"Observed", observed},
	{"Journal", journal},
	{"Follow", follow},
	{"Sequence", sequence},
	{"Delivery", delivery},
	{"Leases", leases},
	{"Revocations", revocations},
	{"Ledger", ledger},
	{"Operations", operations},
	{"Redelivery", redelivery},
	{"Workers", workers},
	{"Values", values},
	{"Rewrap", rewrap},
	{"Ready", ready},
}

// Run applies the whole contract to one adapter, one subtest per case.
func Run(t *testing.T, open Opener) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, open) })
	}
}

// versions: a conditional write refuses a row that moved.
func versions(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	first := put(t, s, object("sbx_a", "alice", "one"), 0)
	if first != 1 {
		t.Fatalf("a create took version %d, want 1", first)
	}
	second := put(t, s, object("sbx_a", "alice", "one"), first)
	if second != first+1 {
		t.Fatalf("a write at version %d took %d, want %d", first, second, first+1)
	}
	fails(t, s, store.ErrVersionConflict, "a write at a stale version", func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, object("sbx_a", "alice", "one"), first)
		return err
	})
	fails(t, s, store.ErrVersionConflict, "a create over a live row", func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, object("sbx_a", "alice", "one"), 0)
		return err
	})
	fails(t, s, store.ErrNotFound, "a write at a version of a row that is not there", func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, object("sbx_gone", "alice", "gone"), 7)
		return err
	})
	got := get(t, s, "sbx_a")
	if got.Version != second || got.Owner != "alice" || got.Name != "one" {
		t.Fatalf("the row reads back %+v", got)
	}
	fails(t, s, store.ErrNotFound, "a read of a row that is not there", func(tx store.Tx) error {
		_, err := tx.Desired().Get(ctx, store.KindSandbox, "sbx_gone")
		return err
	})
}

// names: one live name per owner and kind, reusable after a delete.
func names(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	put(t, s, object("sbx_a", "alice", "shared"), 0)
	put(t, s, object("sbx_b", "bob", "shared"), 0)
	fails(t, s, store.ErrNameTaken, "a second live row of one name", func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, object("sbx_c", "alice", "shared"), 0)
		return err
	})
	with(t, s, func(tx store.Tx) error {
		return tx.Desired().Delete(ctx, store.KindSandbox, "sbx_a")
	})
	put(t, s, object("sbx_c", "alice", "shared"), 0)
	fails(t, s, store.ErrNotFound, "a deleted row", func(tx store.Tx) error {
		_, err := tx.Desired().Get(ctx, store.KindSandbox, "sbx_a")
		return err
	})
	with(t, s, func(tx store.Tx) error {
		got, err := tx.Desired().ByName(ctx, store.KindSandbox, "alice", "shared")
		if err != nil {
			return err
		}
		if got.ID != "sbx_c" {
			t.Errorf("the live row of the reused name is %s", got.ID)
		}
		return nil
	})
	fails(t, s, store.ErrNotFound, "a second delete of one row", func(tx store.Tx) error {
		return tx.Desired().Delete(ctx, store.KindSandbox, "sbx_a")
	})
	fails(t, s, store.ErrNotFound, "a name nobody holds", func(tx store.Tx) error {
		_, err := tx.Desired().ByName(ctx, store.KindSandbox, "alice", "nothing")
		return err
	})
}

// count: the ceiling of design 007 counts live rows that are not on their way
// out.
func count(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	put(t, s, object("sbx_a", "alice", "one"), 0)
	put(t, s, object("sbx_b", "alice", "two"), 0)
	deleting := object("sbx_c", "alice", "three")
	deleting.Phase = store.PhaseDeleting
	put(t, s, deleting, 0)
	put(t, s, object("sbx_d", "bob", "one"), 0)
	with(t, s, func(tx store.Tx) error {
		return tx.Desired().Delete(ctx, store.KindSandbox, "sbx_b")
	})
	with(t, s, func(tx store.Tx) error {
		n, err := tx.Desired().Count(ctx, store.KindSandbox, "alice")
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("the owner's count is %d, want 1: a deleted row and a Deleting row are not counted", n)
		}
		return nil
	})
}

// lists: the filters and the page cursor.
func lists(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	for _, o := range []store.Object{
		labeled(object("sbx_a", "alice", "one"), "prod", "Running", "env_one"),
		labeled(object("sbx_b", "alice", "two"), "prod", "Stopped", "env_one"),
		labeled(object("sbx_c", "alice", "three"), "dev", "Running", "env_two"),
		labeled(object("sbx_d", "bob", "four"), "prod", "Running", "env_one"),
	} {
		put(t, s, o, 0)
	}
	with(t, s, func(tx store.Tx) error {
		return tx.Desired().Delete(ctx, store.KindSandbox, "sbx_d")
	})
	for _, tc := range []struct {
		name string
		f    store.Filter
		want []string
	}{
		{"everything live", store.Filter{}, []string{"sbx_a", "sbx_b", "sbx_c"}},
		{"owner", store.Filter{Owner: "alice"}, []string{"sbx_a", "sbx_b", "sbx_c"}},
		{"phase", store.Filter{Phase: "Running"}, []string{"sbx_a", "sbx_c"}},
		{"environment", store.Filter{Environment: "env_one"}, []string{"sbx_a", "sbx_b"}},
		{"labels", store.Filter{Labels: map[string]string{"tier": "prod"}}, []string{"sbx_a", "sbx_b"}},
		{"ids", store.Filter{IDs: []string{"sbx_b", "sbx_c"}}, []string{"sbx_b", "sbx_c"}},
		{"nothing matches", store.Filter{Owner: "nobody"}, nil},
	} {
		with(t, s, func(tx store.Tx) error {
			rows, next, err := tx.Desired().List(ctx, store.KindSandbox, tc.f, store.Page{})
			if err != nil {
				return err
			}
			if next != "" {
				t.Errorf("the %s list held everything and handed out the cursor %q", tc.name, next)
			}
			if got := ids(rows); !equal(got, tc.want) {
				t.Errorf("the %s list = %v, want %v", tc.name, got, tc.want)
			}
			return nil
		})
	}
	var seen []string
	cursor := ""
	for range 4 {
		with(t, s, func(tx store.Tx) error {
			rows, next, err := tx.Desired().List(ctx, store.KindSandbox, store.Filter{}, store.Page{Limit: 2, Cursor: cursor})
			if err != nil {
				return err
			}
			seen = append(seen, ids(rows)...)
			cursor = next
			return nil
		})
		if cursor == "" {
			break
		}
	}
	if want := []string{"sbx_a", "sbx_b", "sbx_c"}; !equal(seen, want) {
		t.Errorf("the pages read %v, want %v", seen, want)
	}
}

// status: the controller's own column, and the last applied desired state.
func status(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	version := put(t, s, object("sbx_a", "alice", "one"), 0)
	with(t, s, func(tx store.Tx) error {
		if err := tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_a", []byte(`{"phase":"Running"}`)); err != nil {
			return err
		}
		return tx.Desired().SetLastApplied(ctx, "sbx_a", []byte(`{"image":"one"}`))
	})
	got := get(t, s, "sbx_a")
	sameJSON(t, got.Status, []byte(`{"phase":"Running"}`), "the status")
	if got.Version != version {
		t.Errorf("a status write moved the version from %d to %d", version, got.Version)
	}
	with(t, s, func(tx store.Tx) error {
		applied, err := tx.Desired().LastApplied(ctx, "sbx_a")
		if err != nil {
			return err
		}
		sameJSON(t, applied, []byte(`{"image":"one"}`), "the last applied state")
		return nil
	})
	fails(t, s, store.ErrNotFound, "a status on a row that is not there", func(tx store.Tx) error {
		return tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_gone", nil)
	})
	fails(t, s, store.ErrNotFound, "the last applied state of a row that is not there", func(tx store.Tx) error {
		_, err := tx.Desired().LastApplied(ctx, "sbx_gone")
		return err
	})
	fails(t, s, store.ErrNotFound, "a last applied write on a row that is not there", func(tx store.Tx) error {
		return tx.Desired().SetLastApplied(ctx, "sbx_gone", nil)
	})
	// A write of the object keeps the last applied state, which the update
	// diff of design 005 reads after the object it is diffed against moved.
	put(t, s, object("sbx_a", "alice", "one"), got.Version)
	with(t, s, func(tx store.Tx) error {
		applied, err := tx.Desired().LastApplied(ctx, "sbx_a")
		if err != nil {
			return err
		}
		sameJSON(t, applied, []byte(`{"image":"one"}`), "the last applied state after a write of the object")
		return nil
	})
}

// transactions: everything inside one commits together or not at all.
func transactions(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	boom := errors.New("the caller failed between two writes")
	err := s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.Desired().Put(ctx, object("sbx_a", "alice", "one"), 0); err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: "sandbox.created"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the transaction returned %v, want the caller's error", err)
	}
	fails(t, s, store.ErrNotFound, "the object of a failed transaction", func(tx store.Tx) error {
		_, err := tx.Desired().Get(ctx, store.KindSandbox, "sbx_a")
		return err
	})
	with(t, s, func(tx store.Tx) error {
		events, _, err := tx.Journal().ByObject(ctx, "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(events) != 0 {
			t.Errorf("the journal of a failed transaction holds %d event(s)", len(events))
		}
		return nil
	})
	// The same two writes, committed.
	with(t, s, func(tx store.Tx) error {
		if _, err := tx.Desired().Put(ctx, object("sbx_a", "alice", "one"), 0); err != nil {
			return err
		}
		_, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: "sandbox.created"})
		return err
	})
	if got := get(t, s, "sbx_a"); got.Version != 1 {
		t.Errorf("the committed object is at version %d, want 1", got.Version)
	}
}

// observed: the index one environment's list rebuilds.
func observed(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	put(t, s, object("sbx_a", "alice", "one"), 0)
	with(t, s, func(tx store.Tx) error {
		return tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_a", []byte(`{"phase":"Running"}`))
	})
	with(t, s, func(tx store.Tx) error {
		if err := tx.Observed().Put(ctx, "env_one", state("sbx_a", "alice", "Running")); err != nil {
			return err
		}
		return tx.Observed().Put(ctx, "env_two", state("sbx_z", "alice", "Running"))
	})
	with(t, s, func(tx store.Tx) error {
		got, environment, err := tx.Observed().Get(ctx, "sbx_a")
		if err != nil {
			return err
		}
		if environment != "env_one" || got.Phase != "Running" || got.Labels["tier"] != "prod" {
			t.Errorf("the observed row is %+v on %q", got, environment)
		}
		return nil
	})
	fails(t, s, store.ErrNotFound, "an observed row that is not there", func(tx store.Tx) error {
		_, _, err := tx.Observed().Get(ctx, "sbx_gone")
		return err
	})
	// Rebuild replaces the rows of one environment and no other's.
	with(t, s, func(tx store.Tx) error {
		return tx.Observed().Rebuild(ctx, "env_one", []driver.State{state("sbx_b", "alice", "Stopped")})
	})
	with(t, s, func(tx store.Tx) error {
		rows, _, err := tx.Observed().List(ctx, store.Filter{}, store.Page{})
		if err != nil {
			return err
		}
		if got := stateIDs(rows); !equal(got, []string{"sbx_b", "sbx_z"}) {
			t.Errorf("after a rebuild of one environment the index holds %v", got)
		}
		rows, _, err = tx.Observed().List(ctx, store.Filter{Environment: "env_one"}, store.Page{})
		if err != nil {
			return err
		}
		if got := stateIDs(rows); !equal(got, []string{"sbx_b"}) {
			t.Errorf("the rebuilt environment holds %v", got)
		}
		return nil
	})
	after := get(t, s, "sbx_a")
	sameJSON(t, after.Status, []byte(`{"phase":"Running"}`), "the status after a rebuild")
	if after.Version != 1 {
		t.Errorf("a rebuild moved the desired row to version %d", after.Version)
	}
	// A rebuild with nothing empties the environment: an environment whose
	// driver reports no object has none, and the lost rule reads that.
	with(t, s, func(tx store.Tx) error {
		return tx.Observed().Rebuild(ctx, "env_one", nil)
	})
	with(t, s, func(tx store.Tx) error {
		rows, _, err := tx.Observed().List(ctx, store.Filter{Environment: "env_one"}, store.Page{})
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			t.Errorf("an empty rebuild left %d row(s)", len(rows))
		}
		return nil
	})
}

// journal: one sequence per object, newest first, pruned by age.
func journal(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	for _, tc := range []struct {
		object, event string
		want          int64
	}{
		{"sbx_a", "sandbox.created", 1},
		{"sbx_a", "sandbox.started", 2},
		{"sbx_b", "sandbox.created", 1},
		{"sbx_a", "sandbox.stopped", 3},
	} {
		with(t, s, func(tx store.Tx) error {
			seq, err := tx.Journal().Append(ctx, store.Event{ObjectID: tc.object, Type: tc.event, Payload: []byte(`{"reason":"Test"}`)})
			if err != nil {
				return err
			}
			if seq != tc.want {
				t.Errorf("%s of %s took sequence %d, want %d", tc.event, tc.object, seq, tc.want)
			}
			return nil
		})
	}
	refuses(t, s, "an event with no object", func(tx store.Tx) error {
		_, err := tx.Journal().Append(ctx, store.Event{Type: "sandbox.created"})
		return err
	})
	with(t, s, func(tx store.Tx) error {
		events, next, err := tx.Journal().ByObject(ctx, "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(events) != 3 || events[0].Seq != 3 || events[2].Seq != 1 {
			t.Fatalf("one object's journal reads back %d event(s), newest first? %+v", len(events), events)
		}
		if next != "" {
			t.Errorf("one page held everything and handed out the cursor %q", next)
		}
		newest := events[0]
		if newest.Type != "sandbox.stopped" || newest.ID == "" || newest.At.IsZero() {
			t.Errorf("the newest event is %+v", newest)
		}
		sameJSON(t, newest.Payload, []byte(`{"reason":"Test"}`), "the event payload")
		return nil
	})
	var seen []int64
	cursor := ""
	for range 4 {
		with(t, s, func(tx store.Tx) error {
			events, next, err := tx.Journal().ByObject(ctx, "sbx_a", store.Page{Limit: 2, Cursor: cursor})
			if err != nil {
				return err
			}
			for _, e := range events {
				seen = append(seen, e.Seq)
			}
			cursor = next
			return nil
		})
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 || seen[0] != 3 || seen[2] != 1 {
		t.Errorf("the pages read %v, want 3, 2, 1", seen)
	}
	// Retention forgets a finished row and keeps one the sink has not taken:
	// an event older than the window is not an event that may be lost, and
	// design 009 decides when one is given up. sbx_old's second row is its
	// newest, which the retention keeps whatever its age.
	old := time.Now().UTC().Add(-48 * time.Hour)
	with(t, s, func(tx store.Tx) error {
		for _, kind := range []string{"sandbox.created", "sandbox.deleted"} {
			if _, err := tx.Journal().Append(ctx, store.Event{
				ObjectID: "sbx_old", Type: kind, At: old, AckedAt: old,
			}); err != nil {
				return err
			}
		}
		_, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_waiting", Type: "sandbox.created", At: old})
		return err
	})
	with(t, s, func(tx store.Tx) error {
		n, err := tx.Journal().Prune(ctx, time.Now().UTC().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("pruning dropped %d event(s), want the one finished row older than the window", n)
		}
		waiting, _, err := tx.Journal().ByObject(ctx, "sbx_waiting", store.Page{})
		if err != nil {
			return err
		}
		if len(waiting) != 1 {
			t.Errorf("pruning by age took an event the sink had not taken")
		}
		events, _, err := tx.Journal().ByObject(ctx, "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(events) != 3 {
			t.Errorf("pruning by age dropped %d of the three recent events", 3-len(events))
		}
		return nil
	})
}

// follow: the half of the journal a following reader of design 009 needs.
// A subscription receives the rows of each transaction once it committed,
// with the sequence each took, and nothing of a transaction that rolled back;
// an object's subscription receives that object's rows only; a closed store
// ends every subscription. After reads above a sequence, oldest first.
//
// The rows are published before Tx returns on both adapters, so the case
// reads what is buffered rather than waiting for anything.
func follow(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	every, one := s.Watch(""), s.Watch("sbx_a")
	t.Cleanup(every.Close)
	t.Cleanup(one.Close)
	with(t, s, func(tx store.Tx) error {
		for _, e := range []store.Event{
			{ObjectID: "sbx_a", Type: "sandbox.created"},
			{ObjectID: "sbx_b", Type: "sandbox.created"},
			{ObjectID: "sbx_a", Type: "sandbox.started"},
		} {
			if _, err := tx.Journal().Append(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
	refuses(t, s, "a transaction that rolls back after an append", func(tx store.Tx) error {
		if _, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: "sandbox.stopped"}); err != nil {
			return err
		}
		return errors.New("rolled back")
	})
	with(t, s, func(tx store.Tx) error {
		_, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: "sandbox.stopped", Payload: []byte(`{"phase":"Stopped"}`)})
		return err
	})

	with(t, s, func(tx store.Tx) error {
		rows, err := tx.Journal().After(ctx, "sbx_a", 0, 10)
		if err != nil {
			return err
		}
		if got := seqs(rows); !slices.Equal(got, []int64{1, 2, 3}) {
			t.Fatalf("After(0) read the sequences %v, want 1, 2, 3", got)
		}
		if rows[2].Type != "sandbox.stopped" || rows[2].ID == "" || rows[2].At.IsZero() {
			t.Errorf("After read the newest row as %+v", rows[2])
		}
		sameJSON(t, rows[2].Payload, []byte(`{"phase":"Stopped"}`), "the row After read")
		if rows, err = tx.Journal().After(ctx, "sbx_a", 1, 1); err != nil {
			return err
		}
		if got := seqs(rows); !slices.Equal(got, []int64{2}) {
			t.Errorf("After(1) with a limit of one read %v, want 2", got)
		}
		for _, tc := range []struct {
			object string
			after  int64
		}{{"sbx_a", 3}, {"sbx_none", 0}} {
			if rows, err = tx.Journal().After(ctx, tc.object, tc.after, 10); err != nil {
				return err
			}
			if len(rows) != 0 {
				t.Errorf("After(%s, %d) read %v, want nothing", tc.object, tc.after, seqs(rows))
			}
		}
		return nil
	})

	type seen struct {
		object string
		seq    int64
	}
	buffered := func(sub *store.Subscription) []seen {
		var out []seen
		for {
			select {
			case row, ok := <-sub.Rows():
				if !ok {
					t.Errorf("the subscription ended while open: %v", sub.Err())
					return out
				}
				if row.ID == "" || row.Type == "" || row.At.IsZero() {
					t.Errorf("a published row is missing its columns: %+v", row)
				}
				out = append(out, seen{row.ObjectID, row.Seq})
			default:
				return out
			}
		}
	}
	if got, want := buffered(every), []seen{{"sbx_a", 1}, {"sbx_b", 1}, {"sbx_a", 2}, {"sbx_a", 3}}; !slices.Equal(got, want) {
		t.Errorf("every object's subscription received %v, want %v", got, want)
	}
	if got, want := buffered(one), []seen{{"sbx_a", 1}, {"sbx_a", 2}, {"sbx_a", 3}}; !slices.Equal(got, want) {
		t.Errorf("sbx_a's subscription received %v, want %v", got, want)
	}

	// A closed store ends what is open and answers a later subscription
	// that has already ended, each with an error.
	late := s.Watch("")
	if err := s.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	for name, sub := range map[string]*store.Subscription{"open": late, "later": s.Watch("sbx_a")} {
		select {
		case _, ok := <-sub.Rows():
			if ok {
				t.Errorf("the %s subscription received a row from a closed store", name)
			}
		default:
			t.Errorf("the %s subscription is still open on a closed store", name)
		}
		if sub.Err() == nil {
			t.Errorf("the %s subscription ended with no reason", name)
		}
	}
}

// sequence: the retention keeps each object's newest row whatever its age,
// so the object's next append continues its sequence and a reader never sees
// a number twice.
func sequence(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	with(t, s, func(tx store.Tx) error {
		for _, kind := range []string{"sandbox.created", "sandbox.started"} {
			if _, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: kind, At: old, AckedAt: old}); err != nil {
				return err
			}
		}
		return nil
	})
	with(t, s, func(tx store.Tx) error {
		n, err := tx.Journal().Prune(ctx, time.Now().UTC().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("pruning two finished rows of one object dropped %d, want the older one", n)
		}
		return nil
	})
	with(t, s, func(tx store.Tx) error {
		rows, _, err := tx.Journal().ByObject(ctx, "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if got := seqs(rows); !slices.Equal(got, []int64{2}) {
			t.Errorf("the pruned object holds %v, want its newest row, 2", got)
		}
		seq, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_a", Type: "sandbox.stopped"})
		if err != nil {
			return err
		}
		if seq != 3 {
			t.Errorf("the append after a prune took the sequence %d, want 3", seq)
		}
		return nil
	})
}

// seqs is the sequence of each row, in order.
func seqs(rows []store.Event) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

// delivery: design 009's half of the journal. One event per object, the
// lowest unfinished sequence, and a deferred head holding its own object's
// successors and no other object's.
func delivery(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	now := time.Now().UTC()
	ids := map[string]string{}
	for _, tc := range []struct{ object, event string }{
		{"sbx_a", "sandbox.created"},
		{"sbx_a", "sandbox.started"},
		{"sbx_b", "sandbox.created"},
	} {
		id := store.EventID()
		ids[tc.object+"/"+tc.event] = id
		with(t, s, func(tx store.Tx) error {
			_, err := tx.Journal().Append(ctx, store.Event{
				ID: id, ObjectID: tc.object, Type: tc.event, At: now, Payload: []byte(`{}`),
			})
			return err
		})
	}
	// A mutation design 009 names no type for is stored finished and is
	// never pending.
	with(t, s, func(tx store.Tx) error {
		_, err := tx.Journal().Append(ctx, store.Event{
			ObjectID: "sbx_c", Type: "sandbox.deleting", At: now, AckedAt: now,
		})
		return err
	})
	pending(t, s, now, []string{ids["sbx_a/sandbox.created"], ids["sbx_b/sandbox.created"]},
		"the first pass reads each object's head")

	// The head of sbx_a is deferred: its successor waits and sbx_b does not.
	next := now.Add(time.Minute)
	with(t, s, func(tx store.Tx) error {
		return tx.Journal().Defer(ctx, ids["sbx_a/sandbox.created"], next)
	})
	pending(t, s, now, []string{ids["sbx_b/sandbox.created"]}, "a deferred head holds its own object")
	pending(t, s, next, []string{ids["sbx_a/sandbox.created"], ids["sbx_b/sandbox.created"]},
		"the deferred head is due again")

	// Acknowledged, the successor becomes the head.
	with(t, s, func(tx store.Tx) error {
		return tx.Journal().Acknowledge(ctx, ids["sbx_a/sandbox.created"], now)
	})
	pending(t, s, now, []string{ids["sbx_a/sandbox.started"], ids["sbx_b/sandbox.created"]},
		"the acknowledged head hands over")

	// Dropped, an event leaves the queue the same way.
	with(t, s, func(tx store.Tx) error {
		return tx.Journal().Drop(ctx, ids["sbx_b/sandbox.created"], now)
	})
	pending(t, s, now, []string{ids["sbx_a/sandbox.started"]}, "a dropped head leaves the queue")

	// The backlog is every unfinished row and not the heads one pass may
	// take: a deferred row is still owed to the sink, and a gauge that lost
	// it during a backoff would read zero on a queue that is not empty
	// ([[017-observability]]).
	undelivered(t, s, 1, "an acknowledged and a dropped row leave the backlog")

	// The attempt count is what the backoff reads, so it is on the row.
	with(t, s, func(tx store.Tx) error {
		return tx.Journal().Defer(ctx, ids["sbx_a/sandbox.started"], now)
	})

	with(t, s, func(tx store.Tx) error {
		rows, err := tx.Journal().Pending(ctx, 10, now)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Attempts != 1 {
			t.Errorf("the deferred event reads back %+v, want one failed attempt", rows)
		}
		return nil
	})

	// A row whose next attempt is in the future is out of the queue and
	// still owed to the sink. That difference is why the gauge of
	// [[017-observability]] reads the backlog and never what is due.
	with(t, s, func(tx store.Tx) error {
		return tx.Journal().Defer(ctx, ids["sbx_a/sandbox.started"], now.Add(time.Hour))
	})
	pending(t, s, now, nil, "a row deferred into the future is not due")
	undelivered(t, s, 1, "a row deferred into the future is still owed to the sink")
	for _, id := range []string{"evt_nothing"} {
		fails(t, s, store.ErrNotFound, "acknowledging an event no row holds", func(tx store.Tx) error {
			return tx.Journal().Acknowledge(ctx, id, now)
		})
		fails(t, s, store.ErrNotFound, "deferring an event no row holds", func(tx store.Tx) error {
			return tx.Journal().Defer(ctx, id, now)
		})
		fails(t, s, store.ErrNotFound, "dropping an event no row holds", func(tx store.Tx) error {
			return tx.Journal().Drop(ctx, id, now)
		})
	}
}

// undelivered reads the backlog and holds it to the count expected.
func undelivered(t TB, s store.Store, want int, what string) {
	t.Helper()
	with(t, s, func(tx store.Tx) error {
		got, err := tx.Journal().Undelivered(context.Background())
		if err != nil {
			return err
		}
		if got != want {
			t.Errorf("%s: the journal holds %d undelivered, want %d", what, got, want)
		}
		return nil
	})
}

// pending reads what is due at now and holds it to the ids expected.
func pending(t TB, s store.Store, now time.Time, want []string, what string) {
	t.Helper()
	with(t, s, func(tx store.Tx) error {
		rows, err := tx.Journal().Pending(context.Background(), 10, now)
		if err != nil {
			return err
		}
		got := make([]string, 0, len(rows))
		for _, r := range rows {
			got = append(got, r.ID)
		}
		slices.Sort(got)
		sorted := slices.Sorted(slices.Values(want))
		if !equal(got, sorted) {
			t.Errorf("%s: pending reads %v, want %v", what, got, sorted)
		}
		return nil
	})
}

// leases: one holder at a time, renewal by the holder, and a term that lapses.
func leases(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	const term = 400 * time.Millisecond
	held(t, s, "reaper", "replica-one", term, true, "the first holder")
	held(t, s, "reaper", "replica-two", term, false, "a second holder while the first holds")
	held(t, s, "reaper", "replica-one", term, true, "the holder renewing")
	held(t, s, "scheduler", "replica-two", term, true, "another lease of the same replica")
	with(t, s, func(tx store.Tx) error {
		return tx.Leases().Release(ctx, "reaper", "replica-two")
	})
	held(t, s, "reaper", "replica-two", term, false, "a release by a holder that does not hold it")
	with(t, s, func(tx store.Tx) error {
		return tx.Leases().Release(ctx, "reaper", "replica-one")
	})
	held(t, s, "reaper", "replica-two", term, true, "after the holder released")
	// A term that lapses hands the lease to whoever asks next, which is what
	// a replica that died without releasing leaves behind.
	held(t, s, "pool:env_one", "replica-one", 150*time.Millisecond, true, "a short term")
	time.Sleep(300 * time.Millisecond)
	held(t, s, "pool:env_one", "replica-two", term, true, "after the term lapsed")
}

// values: the envelope, and a store with no key.
func values(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	secret := []byte("s3cret-value")
	with(t, s, func(tx store.Tx) error {
		version, err := tx.Values().Put(ctx, "sec_a", secret)
		if err != nil {
			return err
		}
		if version != 1 {
			t.Errorf("the first value took version %d, want 1", version)
		}
		plaintext, version, err := tx.Values().Open(ctx, "sec_a")
		if err != nil {
			return err
		}
		if !bytes.Equal(plaintext, secret) || version != 1 {
			t.Errorf("the value reads back %q at version %d", plaintext, version)
		}
		version, err = tx.Values().Put(ctx, "sec_a", []byte("rotated"))
		if err != nil {
			return err
		}
		if version != 2 {
			t.Errorf("the second value took version %d, want 2", version)
		}
		return nil
	})
	fails(t, s, store.ErrNotFound, "a value that is not there", func(tx store.Tx) error {
		_, _, err := tx.Values().Open(ctx, "sec_gone")
		return err
	})
	with(t, s, func(tx store.Tx) error {
		return tx.Values().Delete(ctx, "sec_a")
	})
	fails(t, s, store.ErrNotFound, "a second delete of one value", func(tx store.Tx) error {
		return tx.Values().Delete(ctx, "sec_a")
	})
	keyless := opened(t, open, nil)
	fails(t, keyless, store.ErrNoSecretKey, "a store with no key", func(tx store.Tx) error {
		_, err := tx.Values().Put(ctx, "sec_a", secret)
		return err
	})
}

// NextKey is the key the rotation case moves to, as fixed as Key is.
var NextKey = []byte("fedcba9876543210fedcba9876543210")

// rewrap: a rotation rewrites every wrapped data key, touches no value's own
// ciphertext, and leaves the store reading under the new key.
func rewrap(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	values := map[string][]byte{"sec_a": []byte("first-value"), "sec_b": []byte("second-value")}
	with(t, s, func(tx store.Tx) error {
		for id, plaintext := range values {
			if _, err := tx.Values().Put(ctx, id, plaintext); err != nil {
				return err
			}
		}
		return nil
	})
	with(t, s, func(tx store.Tx) error {
		n, err := tx.Values().Rewrap(ctx, Key, NextKey)
		if err != nil {
			return err
		}
		if n != len(values) {
			t.Errorf("the rotation moved %d rows, want %d", n, len(values))
		}
		return nil
	})
	// Every value still reads, at the version it held: a rotation is about
	// the key the data key is wrapped under and about nothing else.
	with(t, s, func(tx store.Tx) error {
		for id, want := range values {
			plaintext, version, err := tx.Values().Open(ctx, id)
			if err != nil {
				return err
			}
			if !bytes.Equal(plaintext, want) || version != 1 {
				t.Errorf("%s reads back %q at version %d after the rotation", id, plaintext, version)
			}
		}
		return nil
	})
	// The old key no longer opens a row, which is what a rotation means.
	fails(t, s, store.ErrNoSecretKey, "a rotation with no new key", func(tx store.Tx) error {
		_, err := tx.Values().Rewrap(ctx, Key, nil)
		return err
	})
	refuses(t, s, "a rotation from a key the rows were not sealed under", func(tx store.Tx) error {
		_, err := tx.Values().Rewrap(ctx, Key, NextKey)
		return err
	})
}

// ready: an open store answers its readiness check, and a closed one does not.
func ready(t TB, open Opener) {
	ctx := context.Background()
	s := open(t, Key)
	if err := s.Ready(ctx); err != nil {
		t.Errorf("an open store is not ready: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("closing the store: %v", err)
	}
	if err := s.Ready(ctx); err == nil {
		t.Errorf("a closed store reports itself ready")
	}
	if err := s.Tx(ctx, func(store.Tx) error { return nil }); err == nil {
		t.Errorf("a closed store ran a transaction")
	}
}

// opened returns a store the test closes.
func opened(t TB, open Opener, key []byte) store.Store {
	t.Helper()
	s := open(t, key)
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	return s
}

// with runs one transaction and fails the test when it does not commit.
func with(t TB, s store.Store, fn func(store.Tx) error) {
	t.Helper()
	if err := s.Tx(context.Background(), fn); err != nil {
		t.Fatalf("the transaction did not commit: %v", err)
	}
}

// fails runs one statement in its own transaction and asserts the error it
// reports. Every expected failure is its own transaction, because a statement
// that fails ends the transaction it ran in.
func fails(t TB, s store.Store, want error, what string, fn func(store.Tx) error) {
	t.Helper()
	err := s.Tx(context.Background(), fn)
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// refuses is fails for a condition with no sentinel: the call reports
// something, and what it reports is the adapter's own sentence.
func refuses(t TB, s store.Store, what string, fn func(store.Tx) error) {
	t.Helper()
	if err := s.Tx(context.Background(), fn); err == nil {
		t.Errorf("%s was accepted", what)
	}
}

// sameJSON compares two JSON columns as documents. A store keeps the value and
// not the spelling: Postgres re-renders jsonb, and a caller that compared
// bytes would be asserting the server's formatter.
func sameJSON(t TB, got, want []byte, what string) {
	t.Helper()
	var read, expected any
	if err := json.Unmarshal(got, &read); err != nil {
		t.Errorf("%s is not JSON: %q", what, got)
		return
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatalf("the expected %s is not JSON: %q", what, want)
	}
	left, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("re-encoding %s: %v", what, err)
	}
	right, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("re-encoding the expected %s: %v", what, err)
	}
	if !bytes.Equal(left, right) {
		t.Errorf("%s reads back %s, want %s", what, left, right)
	}
}

// put writes one object and returns the version it took.
func put(t TB, s store.Store, obj store.Object, ifVersion int64) int64 {
	t.Helper()
	var version int64
	with(t, s, func(tx store.Tx) error {
		var err error
		version, err = tx.Desired().Put(context.Background(), obj, ifVersion)
		return err
	})
	return version
}

// get reads one object.
func get(t TB, s store.Store, id string) store.Object {
	t.Helper()
	var obj store.Object
	with(t, s, func(tx store.Tx) error {
		var err error
		obj, err = tx.Desired().Get(context.Background(), store.KindSandbox, id)
		return err
	})
	return obj
}

// held asserts what one acquisition of a lease reports.
func held(t TB, s store.Store, name, holder string, ttl time.Duration, want bool, what string) {
	t.Helper()
	var got bool
	with(t, s, func(tx store.Tx) error {
		var err error
		got, err = tx.Leases().Acquire(context.Background(), name, holder, ttl)
		return err
	})
	if got != want {
		t.Errorf("%s: %s holds %s = %v, want %v", what, holder, name, got, want)
	}
}

// revoked asks the list about one jti and reports what it answered.
func revoked(t TB, s store.Store, jti string) bool {
	t.Helper()
	var got bool
	with(t, s, func(tx store.Tx) error {
		var err error
		got, err = tx.Revocations().Revoked(context.Background(), jti)
		return err
	})
	return got
}

// object is one desired sandbox with the fields every case reads.
func object(id, owner, name string) store.Object {
	return store.Object{
		Kind: store.KindSandbox, ID: id, Owner: owner, Name: name,
		Environment: "env_one", Phase: "Pending",
		Labels: map[string]string{"tier": "prod"},
		Data:   []byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox"}`),
		Status: []byte(`{}`),
	}
}

// labeled sets the columns the list filters read.
func labeled(o store.Object, tier, phase, environment string) store.Object {
	o.Labels = map[string]string{"tier": tier}
	o.Phase = phase
	o.Environment = environment
	return o
}

// state is one observed sandbox.
func state(id, owner, phase string) driver.State {
	return driver.State{
		ID: id, Owner: owner, Name: id, Phase: phase,
		Labels:    map[string]string{"tier": "prod"},
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func ids(rows []store.Object) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func stateIDs(rows []driver.State) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// revocations is the list the verifier of design 006 asks before it trusts a
// token cellad signed: a jti that was revoked is refused from that moment,
// one that never was is not, and a row whose exp has passed is swept, because
// a token nobody can still present needs no row.
func revocations(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	now := time.Now().UTC()
	with(t, s, func(tx store.Tx) error {
		return tx.Revocations().Revoke(ctx, "01JLIVE", now.Add(time.Hour))
	})
	if !revoked(t, s, "01JLIVE") {
		t.Errorf("a revoked jti reads as live")
	}
	if revoked(t, s, "01JOTHER") {
		t.Errorf("a jti nobody revoked reads as revoked")
	}
	// A rotation or a recovery that retried revokes the same jti twice, and
	// the row keeps the later expiry so it outlives every token that could
	// present it.
	with(t, s, func(tx store.Tx) error {
		return tx.Revocations().Revoke(ctx, "01JLIVE", now.Add(2*time.Hour))
	})
	if !revoked(t, s, "01JLIVE") {
		t.Errorf("a repeated revocation dropped the row")
	}
	// The later expiry is the one the row keeps, so a revocation outlives
	// every token that could present it and not merely the first.
	var kept int
	with(t, s, func(tx store.Tx) error {
		var err error
		kept, err = tx.Revocations().Forget(ctx, now.Add(90*time.Minute))
		return err
	})
	if kept != 0 || !revoked(t, s, "01JLIVE") {
		t.Errorf("the sweep dropped %d rows at an instant inside the later expiry", kept)
	}
	// A revocation with no jti revokes nothing and says so, rather than
	// writing a row every token would match on an empty claim.
	if err := s.Tx(ctx, func(tx store.Tx) error {
		return tx.Revocations().Revoke(ctx, "", now.Add(time.Hour))
	}); err == nil {
		t.Errorf("a revocation with no jti was accepted")
	}
	with(t, s, func(tx store.Tx) error {
		return tx.Revocations().Revoke(ctx, "01JPAST", now.Add(-time.Minute))
	})
	var swept int
	with(t, s, func(tx store.Tx) error {
		var err error
		swept, err = tx.Revocations().Forget(ctx, now)
		return err
	})
	if swept != 1 {
		t.Errorf("the sweep dropped %d rows, want the one whose exp had passed", swept)
	}
	if revoked(t, s, "01JPAST") {
		t.Errorf("the expired revocation is still in the list")
	}
	if !revoked(t, s, "01JLIVE") {
		t.Errorf("the sweep dropped a revocation whose exp has not passed")
	}
}

// ledger: the spawn budget of design 022. A debit is conditional on the
// budget it carries, exhaustion is a sentinel and not a silent no-op, a
// credit is its undo, and the count a read returns is what the next debit is
// judged against.
func ledger(t TB, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	const parent = "sbx_parent"

	if used := spawned(t, s, parent); used != 0 {
		t.Errorf("a sandbox that created nothing has used %d", used)
	}
	with(t, s, func(tx store.Tx) error { return tx.Ledger().Debit(ctx, parent, 2) })
	with(t, s, func(tx store.Tx) error { return tx.Ledger().Debit(ctx, parent, 2) })
	if used := spawned(t, s, parent); used != 2 {
		t.Errorf("used = %d after two debits, want 2", used)
	}
	fails(t, s, store.ErrBudgetExhausted, "a third debit against a budget of two",
		func(tx store.Tx) error { return tx.Ledger().Debit(ctx, parent, 2) })
	// A budget narrowed below what is already used refuses at once, and a
	// budget of nothing refuses without a row.
	fails(t, s, store.ErrBudgetExhausted, "a debit against a narrowed budget",
		func(tx store.Tx) error { return tx.Ledger().Debit(ctx, parent, 1) })
	fails(t, s, store.ErrBudgetExhausted, "a debit against no budget",
		func(tx store.Tx) error { return tx.Ledger().Debit(ctx, "sbx_none", 0) })

	// The credit is the undo of a create that did not complete, and it never
	// takes the count below zero.
	with(t, s, func(tx store.Tx) error { return tx.Ledger().Credit(ctx, parent) })
	if used := spawned(t, s, parent); used != 1 {
		t.Errorf("used = %d after one credit, want 1", used)
	}
	with(t, s, func(tx store.Tx) error { return tx.Ledger().Credit(ctx, "sbx_none") })
	if used := spawned(t, s, "sbx_none"); used != 0 {
		t.Errorf("crediting a sandbox with no row left used = %d", used)
	}
	// The unit the credit returned is available again.
	with(t, s, func(tx store.Tx) error { return tx.Ledger().Debit(ctx, parent, 2) })

	refuses(t, s, "a debit that names no sandbox",
		func(tx store.Tx) error { return tx.Ledger().Debit(ctx, "", 1) })

	with(t, s, func(tx store.Tx) error { return tx.Ledger().Forget(ctx, parent) })
	if used := spawned(t, s, parent); used != 0 {
		t.Errorf("used = %d after the row was forgotten, want 0", used)
	}

	// The whole point of the ledger: several callers debiting one remaining
	// unit at once yield one success. A count read and written in two
	// statements would yield several.
	const racers = 8
	var wg sync.WaitGroup
	won := make([]error, racers)
	for i := range won {
		wg.Go(func() {
			won[i] = s.Tx(ctx, func(tx store.Tx) error {
				return tx.Ledger().Debit(ctx, "sbx_race", 1)
			})
		})
	}
	wg.Wait()
	granted, refused := 0, 0
	for _, err := range won {
		switch {
		case err == nil:
			granted++
		case errors.Is(err, store.ErrBudgetExhausted):
			refused++
		default:
			t.Fatalf("a racing debit answered %v", err)
		}
	}
	if granted != 1 || refused != racers-1 {
		t.Errorf("%d debits of one unit were granted and %d refused", granted, refused)
	}
	if used := spawned(t, s, "sbx_race"); used != 1 {
		t.Errorf("the race left used = %d, want 1", used)
	}
}

// spawned reads one sandbox's ledger count in its own transaction.
func spawned(t TB, s store.Store, id string) int {
	t.Helper()
	var used int
	with(t, s, func(tx store.Tx) error {
		var err error
		used, err = tx.Ledger().Used(context.Background(), id)
		return err
	})
	return used
}

// operations: the worker queue of design 021. Enqueue, claim, acknowledge,
// and the registrations an environment's phase is computed from.
func operations(t TB, open Opener) {
	ctx := context.Background()
	s := opened(t, open, nil)
	now := time.Now().UTC().Truncate(time.Second)
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_one", Replica: "r1", LastHeartbeat: now})

	with(t, s, func(tx store.Tx) error {
		for _, id := range []string{"op_1", "op_2", "op_3"} {
			err := tx.Operations().Enqueue(ctx, store.Operation{
				ID: id, Environment: "env_a", SandboxID: "sbx_1", Type: "Inspect",
				Payload: []byte(`{"id":"sbx_1"}`), CreatedAt: now.Add(time.Duration(id[3]) * time.Millisecond),
			})
			if err != nil {
				return err
			}
		}
		// One row on another environment, which no claim of env_a takes.
		return tx.Operations().Enqueue(ctx, store.Operation{ID: "op_9", Environment: "env_b", Type: "Inspect"})
	})
	fails(t, s, store.ErrVersionConflict, "an operation id already in the table", func(tx store.Tx) error {
		return tx.Operations().Enqueue(ctx, store.Operation{ID: "op_1", Environment: "env_a", Type: "Inspect"})
	})
	refuses(t, s, "an operation with no type", func(tx store.Tx) error {
		return tx.Operations().Enqueue(ctx, store.Operation{ID: "op_x", Environment: "env_a"})
	})

	claimed := claim(t, s, "env_a", "wrk_one", 2, now.Add(-time.Minute))
	if len(claimed) != 2 || claimed[0].ID != "op_1" || claimed[1].ID != "op_2" {
		t.Fatalf("the claim took %v, want the two oldest rows of the environment", operationIDs(claimed))
	}
	for _, op := range claimed {
		if op.State != store.OperationClaimed || op.ClaimedBy != "wrk_one" || op.Attempts != 1 {
			t.Errorf("a claimed row reads %s/%s at attempt %d, want claimed/wrk_one at 1", op.State, op.ClaimedBy, op.Attempts)
		}
		sameJSON(t, op.Payload, []byte(`{"id":"sbx_1"}`), "the claimed row's payload")
	}
	// A second claim by the same worker takes what is left and never its own
	// rows again, however long it has held them.
	again := claim(t, s, "env_a", "wrk_one", 10, now.Add(-time.Minute))
	if len(again) != 1 || again[0].ID != "op_3" {
		t.Errorf("the second claim took %v, want only the row nobody held", operationIDs(again))
	}

	with(t, s, func(tx store.Tx) error {
		return tx.Operations().Acknowledge(ctx, "op_1", []byte(`{"ok":true}`))
	})
	done := operation(t, s, "op_1")
	if done.State != store.OperationDone {
		t.Errorf("the acknowledged row reads %s, want %s", done.State, store.OperationDone)
	}
	sameJSON(t, done.Result, []byte(`{"ok":true}`), "the acknowledged row's result")
	// An acknowledgment that raced a redelivery never overwrites the answer
	// the caller was already given.
	with(t, s, func(tx store.Tx) error {
		return tx.Operations().Acknowledge(ctx, "op_1", []byte(`{"ok":false}`))
	})
	sameJSON(t, operation(t, s, "op_1").Result, []byte(`{"ok":true}`),
		"the result after a second acknowledgment")
	fails(t, s, store.ErrNotFound, "acknowledging an operation nobody enqueued", func(tx store.Tx) error {
		return tx.Operations().Acknowledge(ctx, "op_absent", nil)
	})
	fails(t, s, store.ErrNotFound, "reading an operation nobody enqueued", func(tx store.Tx) error {
		_, err := tx.Operations().Get(ctx, "op_absent")
		return err
	})

	swept := prune(t, s, now.Add(time.Hour))
	if swept != 1 {
		t.Errorf("the sweep dropped %d rows, want the one that was done", swept)
	}
	if left := claim(t, s, "env_a", "wrk_one", 10, now.Add(-time.Minute)); len(left) != 0 {
		t.Errorf("the sweep left claimable rows behind: %v", operationIDs(left))
	}
}

// redelivery: a claim held by a worker whose heartbeat lapsed goes to a live
// worker, exactly once, which is what a dropped connection costs.
func redelivery(t TB, open Opener) {
	ctx := context.Background()
	s := opened(t, open, nil)
	now := time.Now().UTC().Truncate(time.Second)
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_gone", LastHeartbeat: now.Add(-time.Hour)})
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_live", LastHeartbeat: now})
	with(t, s, func(tx store.Tx) error {
		return tx.Operations().Enqueue(ctx, store.Operation{ID: "op_1", Environment: "env_a", Type: "Start", CreatedAt: now})
	})

	if got := claim(t, s, "env_a", "wrk_gone", 10, now.Add(-time.Minute)); len(got) != 1 {
		t.Fatalf("the first worker claimed %d rows, want the one that was queued", len(got))
	}
	// While the first worker is inside its lease the row is its own.
	if got := claim(t, s, "env_a", "wrk_live", 10, now.Add(-2*time.Hour)); len(got) != 0 {
		t.Errorf("a live claim was redelivered: %v", operationIDs(got))
	}
	// Past the lease it is redelivered, once, with the attempt counted.
	got := claim(t, s, "env_a", "wrk_live", 10, now.Add(-time.Minute))
	if len(got) != 1 || got[0].ID != "op_1" {
		t.Fatalf("the lapsed claim was not redelivered: %v", operationIDs(got))
	}
	if got[0].ClaimedBy != "wrk_live" || got[0].Attempts != 2 {
		t.Errorf("the redelivered row reads %s at attempt %d, want wrk_live at 2", got[0].ClaimedBy, got[0].Attempts)
	}
	if twice := claim(t, s, "env_a", "wrk_gone", 10, now.Add(-time.Minute)); len(twice) != 0 {
		t.Errorf("the row was delivered a second time to the worker that lost it: %v", operationIDs(twice))
	}

	// A worker the control plane forgot holds nothing: its rows are claimable
	// at once rather than at the end of a lease it will never renew.
	with(t, s, func(tx store.Tx) error { return tx.Operations().Forget(ctx, "env_a", "wrk_live") })
	back := claim(t, s, "env_a", "wrk_gone", 10, now.Add(-time.Minute))
	if len(back) != 1 {
		t.Errorf("a forgotten worker's row was not claimable: %v", operationIDs(back))
	}
}

// workers: registration, the heartbeat that renews it, and the read the
// environment's phase is computed from.
func workers(t TB, open Opener) {
	ctx := context.Background()
	s := opened(t, open, nil)
	now := time.Now().UTC().Truncate(time.Second)
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_b", Replica: "r2", LastHeartbeat: now})
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_a", Replica: "r1", LastHeartbeat: now})
	register(t, s, store.Worker{Environment: "env_b", Worker: "wrk_c", LastHeartbeat: now})

	rows := workerRows(t, s, "env_a")
	if len(rows) != 2 || rows[0].Worker != "wrk_a" || rows[1].Worker != "wrk_b" {
		t.Fatalf("the environment's workers read %v, want wrk_a then wrk_b", rows)
	}
	if rows[0].Replica != "r1" || !rows[0].LastHeartbeat.Equal(now) {
		t.Errorf("the registration reads replica %q at %s", rows[0].Replica, rows[0].LastHeartbeat)
	}

	later := now.Add(time.Minute)
	with(t, s, func(tx store.Tx) error { return tx.Operations().Heartbeat(ctx, "env_a", "wrk_a", later) })
	if got := workerRows(t, s, "env_a")[0]; !got.LastHeartbeat.Equal(later) {
		t.Errorf("the heartbeat left the stamp at %s, want %s", got.LastHeartbeat, later)
	}
	// A registration replaces the row rather than adding one, so a worker
	// that restarted under the same id is one row and not two.
	register(t, s, store.Worker{Environment: "env_a", Worker: "wrk_a", Replica: "r3", LastHeartbeat: later})
	if rows = workerRows(t, s, "env_a"); len(rows) != 2 || rows[0].Replica != "r3" {
		t.Errorf("re-registering wrote %d rows with replica %q", len(rows), rows[0].Replica)
	}
	fails(t, s, store.ErrNotFound, "stamping a worker that never registered", func(tx store.Tx) error {
		return tx.Operations().Heartbeat(ctx, "env_a", "wrk_absent", later)
	})
	refuses(t, s, "a registration that names no worker", func(tx store.Tx) error {
		return tx.Operations().Register(ctx, store.Worker{Environment: "env_a"})
	})
	// Forgetting a worker nobody registered is not an error: a disconnect
	// that races the sweep writes the same call twice.
	with(t, s, func(tx store.Tx) error { return tx.Operations().Forget(ctx, "env_a", "wrk_absent") })
}

func register(t TB, s store.Store, w store.Worker) {
	t.Helper()
	with(t, s, func(tx store.Tx) error { return tx.Operations().Register(context.Background(), w) })
}

func claim(t TB, s store.Store, environment, worker string, n int, lapsed time.Time) []store.Operation {
	t.Helper()
	var out []store.Operation
	with(t, s, func(tx store.Tx) error {
		var err error
		out, err = tx.Operations().Claim(context.Background(), environment, worker, n, lapsed)
		return err
	})
	return out
}

func operation(t TB, s store.Store, id string) store.Operation {
	t.Helper()
	var out store.Operation
	with(t, s, func(tx store.Tx) error {
		var err error
		out, err = tx.Operations().Get(context.Background(), id)
		return err
	})
	return out
}

func workerRows(t TB, s store.Store, environment string) []store.Worker {
	t.Helper()
	var out []store.Worker
	with(t, s, func(tx store.Tx) error {
		var err error
		out, err = tx.Operations().Workers(context.Background(), environment)
		return err
	})
	return out
}

func prune(t TB, s store.Store, before time.Time) int {
	t.Helper()
	var n int
	with(t, s, func(tx store.Tx) error {
		var err error
		n, err = tx.Operations().Prune(context.Background(), before)
		return err
	})
	return n
}

func operationIDs(rows []store.Operation) []string {
	out := make([]string, len(rows))
	for i, op := range rows {
		out[i] = op.ID
	}
	return out
}
