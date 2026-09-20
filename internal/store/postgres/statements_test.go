// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/storetest"
	driver "latere.ai/x/cella/runtime"
)

// TestStatementsReportFailures drives every statement against a caller whose
// context is already done. A database that does not answer, a caller that gave
// up, and a transaction another statement aborted all reach the same branch,
// and every one of them has to come back as an error rather than as an empty
// result the controller would act on.
func TestStatementsReportFailures(t *testing.T) {
	admin := server(t)
	s := open(t, database(t, admin), storetest.Key, time.Hour)
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	// The transaction itself is opened on a live context; the statements
	// inside it are the ones that fail.
	err := s.Tx(t.Context(), func(tx store.Tx) error {
		obj := store.Object{Kind: store.KindSandbox, ID: "sbx_a", Owner: "alice", Name: "one", Data: []byte(`{}`)}
		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"a create", func() error { _, err := tx.Desired().Put(gone, obj, 0); return err }},
			{"a conditional write", func() error { _, err := tx.Desired().Put(gone, obj, 1); return err }},
			{"a read", func() error { _, err := tx.Desired().Get(gone, store.KindSandbox, "sbx_a"); return err }},
			{"a read by name", func() error { _, err := tx.Desired().ByName(gone, store.KindSandbox, "alice", "one"); return err }},
			{"a list", func() error {
				_, _, err := tx.Desired().List(gone, store.KindSandbox, store.Filter{}, store.Page{})
				return err
			}},
			{"a delete", func() error { return tx.Desired().Delete(gone, store.KindSandbox, "sbx_a") }},
			{"a count", func() error { _, err := tx.Desired().Count(gone, store.KindSandbox, "alice"); return err }},
			{"a status", func() error { return tx.Desired().PutStatus(gone, store.KindSandbox, "sbx_a", []byte(`{}`)) }},
			{"the last applied state", func() error { _, err := tx.Desired().LastApplied(gone, "sbx_a"); return err }},
			{"a last applied write", func() error { return tx.Desired().SetLastApplied(gone, "sbx_a", []byte(`{}`)) }},
			{"an observed write", func() error { return tx.Observed().Put(gone, "env_one", driver.State{ID: "sbx_a"}) }},
			{"an observed read", func() error { _, _, err := tx.Observed().Get(gone, "sbx_a"); return err }},
			{"an observed list", func() error { _, _, err := tx.Observed().List(gone, store.Filter{}, store.Page{}); return err }},
			{"a rebuild", func() error { return tx.Observed().Rebuild(gone, "env_one", nil) }},
			{"a journal append", func() error {
				_, err := tx.Journal().Append(gone, store.Event{ObjectID: "sbx_a", Type: "sandbox.created"})
				return err
			}},
			{"a journal read", func() error { _, _, err := tx.Journal().ByObject(gone, "sbx_a", store.Page{}); return err }},
			{"a journal prune", func() error { _, err := tx.Journal().Prune(gone, time.Now()); return err }},
			{"a value write", func() error { _, err := tx.Values().Put(gone, "sec_a", []byte("value")); return err }},
			{"a value read", func() error { _, _, err := tx.Values().Open(gone, "sec_a"); return err }},
			{"a value delete", func() error { return tx.Values().Delete(gone, "sec_a") }},
			{"a lease", func() error { _, err := tx.Leases().Acquire(gone, "reaper", "replica-one", time.Second); return err }},
			{"a lease release", func() error { return tx.Leases().Release(gone, "reaper", "replica-one") }},
			{"a ledger debit", func() error { return tx.Ledger().Debit(gone, "sbx_a", 1) }},
			{"a ledger credit", func() error { return tx.Ledger().Credit(gone, "sbx_a") }},
			{"a ledger read", func() error { _, err := tx.Ledger().Used(gone, "sbx_a"); return err }},
			{"a ledger forget", func() error { return tx.Ledger().Forget(gone, "sbx_a") }},
			{"an enqueue", func() error {
				return tx.Operations().Enqueue(gone, store.Operation{ID: "op_a", Environment: "env_one", Type: "exec"})
			}},
			{"a claim", func() error {
				_, err := tx.Operations().Claim(gone, "env_one", "worker-one", 1, time.Now())
				return err
			}},
			{"an acknowledge", func() error { return tx.Operations().Acknowledge(gone, "op_a", nil) }},
			{"an operation read", func() error { _, err := tx.Operations().Get(gone, "op_a"); return err }},
			{"a registration", func() error {
				return tx.Operations().Register(gone, store.Worker{Environment: "env_one", Worker: "worker-one"})
			}},
			{"a heartbeat", func() error { return tx.Operations().Heartbeat(gone, "env_one", "worker-one", time.Now()) }},
			{"a worker list", func() error { _, err := tx.Operations().Workers(gone, "env_one"); return err }},
			{"a worker forget", func() error { return tx.Operations().Forget(gone, "env_one", "worker-one") }},
			{"an operation prune", func() error { _, err := tx.Operations().Prune(gone, time.Now()); return err }},
		} {
			if err := tc.call(); err == nil {
				t.Errorf("%s on a caller that gave up was reported as done", tc.name)
			}
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("the transaction returned %v", err)
	}
}

// errRollback ends the transaction above without committing it.
var errRollback = errors.New("the case is done")

// TestObservedStateThatIsNotJSON: a row nothing this binary wrote is a read
// failure and not a zero state, because a zero state would be a sandbox the
// lost rule reports as missing.
func TestObservedStateThatIsNotJSON(t *testing.T) {
	admin := server(t)
	dsn := database(t, admin)
	s := open(t, dsn, storetest.Key, time.Hour)
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		return tx.Observed().Put(t.Context(), "env_one", driver.State{ID: "sbx_a", Phase: driver.Running})
	}); err != nil {
		t.Fatal(err)
	}
	execute(t, dsn, `update observed set state = '"not a state"'::jsonb where id = 'sbx_a'`)
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, _, err := tx.Observed().Get(t.Context(), "sbx_a")
		return err
	}); err == nil {
		t.Error("a row that is not a state was read as one")
	}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, _, err := tx.Observed().List(t.Context(), store.Filter{}, store.Page{})
		return err
	}); err == nil {
		t.Error("a list read a row that is not a state")
	}
}

// TestJournalCursorIsASequence: a cursor a caller made up is refused rather
// than read as the beginning of the journal.
func TestJournalCursorIsASequence(t *testing.T) {
	admin := server(t)
	s := open(t, database(t, admin), storetest.Key, time.Hour)
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, _, err := tx.Journal().ByObject(t.Context(), "sbx_a", store.Page{Cursor: "the next page please"})
		return err
	}); err == nil {
		t.Fatal("a cursor that is not a sequence was accepted")
	}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, err := tx.Journal().Append(t.Context(), store.Event{ObjectID: "sbx_a"})
		return err
	}); err == nil {
		t.Fatal("an event with no type was appended")
	}
}
