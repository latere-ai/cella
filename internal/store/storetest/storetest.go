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
	"encoding/json"
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	driver "latere.ai/x/cella/runtime"
)

// Opener returns an empty store sealing secret values under key, which is nil
// for a store opened without one. The suite closes what it opens.
type Opener func(t *testing.T, key []byte) store.Store

// Key is the secret key the suite opens its stores with: 32 bytes, fixed, so
// a failure is reproducible.
var Key = []byte("0123456789abcdef0123456789abcdef")

// Run applies the whole contract to one adapter.
func Run(t *testing.T, open Opener) {
	t.Helper()
	for _, c := range []struct {
		name string
		run  func(*testing.T, Opener)
	}{
		{"Versions", versions},
		{"Names", names},
		{"Count", count},
		{"Lists", lists},
		{"Status", status},
		{"Transactions", transactions},
		{"Observed", observed},
		{"Journal", journal},
		{"Leases", leases},
		{"Values", values},
		{"Ready", ready},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, open) })
	}
}

// versions: a conditional write refuses a row that moved.
func versions(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func names(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func count(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func lists(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
	for _, o := range []store.Object{
		labelled(object("sbx_a", "alice", "one"), "prod", "Running", "env_one"),
		labelled(object("sbx_b", "alice", "two"), "prod", "Stopped", "env_one"),
		labelled(object("sbx_c", "alice", "three"), "dev", "Running", "env_two"),
		labelled(object("sbx_d", "bob", "four"), "prod", "Running", "env_one"),
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
		t.Run(tc.name, func(t *testing.T) {
			with(t, s, func(tx store.Tx) error {
				rows, next, err := tx.Desired().List(ctx, store.KindSandbox, tc.f, store.Page{})
				if err != nil {
					return err
				}
				if next != "" {
					t.Errorf("one page held everything and handed out the cursor %q", next)
				}
				if got := ids(rows); !equal(got, tc.want) {
					t.Errorf("list = %v, want %v", got, tc.want)
				}
				return nil
			})
		})
	}
	t.Run("pages", func(t *testing.T) {
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
	})
}

// status: the controller's own column, and the last applied desired state.
func status(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func transactions(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func observed(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func journal(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
	t.Run("pages", func(t *testing.T) {
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
	})
	t.Run("prune", func(t *testing.T) {
		old := time.Now().UTC().Add(-48 * time.Hour)
		with(t, s, func(tx store.Tx) error {
			_, err := tx.Journal().Append(ctx, store.Event{ObjectID: "sbx_old", Type: "sandbox.created", At: old})
			return err
		})
		with(t, s, func(tx store.Tx) error {
			n, err := tx.Journal().Prune(ctx, time.Now().UTC().Add(-24*time.Hour))
			if err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("pruning dropped %d event(s), want the one older than the window", n)
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
	})
}

// leases: one holder at a time, renewal by the holder, and a term that lapses.
func leases(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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
func values(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := t.Context()
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

// ready: an open store answers its readiness check, and a closed one does not.
func ready(t *testing.T, open Opener) {
	s := open(t, Key)
	if err := s.Ready(t.Context()); err != nil {
		t.Errorf("an open store is not ready: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("closing the store: %v", err)
	}
	if err := s.Ready(t.Context()); err == nil {
		t.Error("a closed store reports itself ready")
	}
	if err := s.Tx(t.Context(), func(store.Tx) error { return nil }); err == nil {
		t.Error("a closed store ran a transaction")
	}
}

// opened returns a store the test closes.
func opened(t *testing.T, open Opener, key []byte) store.Store {
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
func with(t *testing.T, s store.Store, fn func(store.Tx) error) {
	t.Helper()
	if err := s.Tx(t.Context(), fn); err != nil {
		t.Fatalf("the transaction did not commit: %v", err)
	}
}

// fails runs one statement in its own transaction and asserts the error it
// reports. Every expected failure is its own transaction, because a statement
// that fails ends the transaction it ran in.
func fails(t *testing.T, s store.Store, want error, what string, fn func(store.Tx) error) {
	t.Helper()
	err := s.Tx(t.Context(), fn)
	if !errors.Is(err, want) {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// refuses is fails for a condition with no sentinel: the call reports
// something, and what it reports is the adapter's own sentence.
func refuses(t *testing.T, s store.Store, what string, fn func(store.Tx) error) {
	t.Helper()
	if err := s.Tx(t.Context(), fn); err == nil {
		t.Errorf("%s was accepted", what)
	}
}

// sameJSON compares two JSON columns as documents. A store keeps the value and
// not the spelling: Postgres re-renders jsonb, and a caller that compared
// bytes would be asserting the server's formatter.
func sameJSON(t *testing.T, got, want []byte, what string) {
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
func put(t *testing.T, s store.Store, obj store.Object, ifVersion int64) int64 {
	t.Helper()
	var version int64
	with(t, s, func(tx store.Tx) error {
		var err error
		version, err = tx.Desired().Put(t.Context(), obj, ifVersion)
		return err
	})
	return version
}

// get reads one object.
func get(t *testing.T, s store.Store, id string) store.Object {
	t.Helper()
	var obj store.Object
	with(t, s, func(tx store.Tx) error {
		var err error
		obj, err = tx.Desired().Get(t.Context(), store.KindSandbox, id)
		return err
	})
	return obj
}

// held asserts what one acquisition of a lease reports.
func held(t *testing.T, s store.Store, name, holder string, ttl time.Duration, want bool, what string) {
	t.Helper()
	var got bool
	with(t, s, func(tx store.Tx) error {
		var err error
		got, err = tx.Leases().Acquire(t.Context(), name, holder, ttl)
		return err
	})
	if got != want {
		t.Errorf("%s: %s holds %s = %v, want %v", what, holder, name, got, want)
	}
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

// labelled sets the columns the list filters read.
func labelled(o store.Object, tier, phase, environment string) store.Object {
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
