// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	driver "latere.ai/x/cella/runtime"
)

// TestTheFenceRefusesAWriterThatLostItsLease: an adapter fenced on the writer
// lease writes while its process holds that lease, and once the term lapsed
// and another process took it, every write it tries is ErrNotWriter and
// changes nothing, while the new writer writes. An adapter with no fence is
// a deployment of one process and writes without a lease.
func TestTheFenceRefusesAWriterThatLostItsLease(t *testing.T) {
	old, s := bound(t)
	old.Fence(store.WriterLease)
	successor := store.ForController(s, "default", store.Delivered).Fence(store.WriterLease)
	const term = 150 * time.Millisecond

	if err := old.Write(t.Context(), sandbox("sbx_a", "work", driver.Pending), controller.MutationCreated); !errors.Is(err, controller.ErrNotWriter) {
		t.Fatalf("a write before the lease was taken answered %v, want ErrNotWriter", err)
	}
	if held, err := old.Acquire(t.Context(), store.WriterLease, term); err != nil || !held {
		t.Fatalf("taking the writer lease: %v, %v", held, err)
	}
	if err := old.Write(t.Context(), sandbox("sbx_a", "work", driver.Pending), controller.MutationCreated); err != nil {
		t.Fatalf("the writer's write: %v", err)
	}
	if _, err := old.WriteSecret(t.Context(), secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated); err != nil {
		t.Fatalf("the writer's secret: %v", err)
	}
	time.Sleep(2 * term)
	if held, err := successor.Acquire(t.Context(), store.WriterLease, time.Minute); err != nil || !held {
		t.Fatalf("the successor taking the lapsed lease: %v, %v", held, err)
	}
	for name, write := range map[string]func() error{
		"a write": func() error {
			return old.Write(t.Context(), sandbox("sbx_a", "work", driver.Running), controller.MutationStarted)
		},
		"a removal": func() error { return old.Remove(t.Context(), "sbx_a", controller.MutationDeleted) },
		"a rebuild": func() error { return old.Rebuild(t.Context(), "default", nil) },
		"a secret's last use": func() error {
			used := secret("sec_a", "github")
			used.Status.LastUsedAt = time.Now().UTC()
			return old.WriteSecretUse(t.Context(), used)
		},
	} {
		if err := write(); !errors.Is(err, controller.ErrNotWriter) {
			t.Errorf("%s by the demoted writer answered %v, want ErrNotWriter", name, err)
		}
	}
	held, err := successor.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := held["sbx_a"].Status.Phase; got != driver.Pending {
		t.Errorf("the row reads %q after the demoted writer's writes, want the Pending it wrote while it held the lease", got)
	}
	obj := held["sbx_a"]
	obj.Status.Phase = driver.Running
	if err := successor.Write(t.Context(), obj, controller.MutationStarted); err != nil {
		t.Errorf("the new writer's write: %v", err)
	}

	unfenced, _ := bound(t)
	if err := unfenced.Write(t.Context(), sandbox("sbx_b", "work", driver.Pending), controller.MutationCreated); err != nil {
		t.Errorf("an adapter with no fence refused a write: %v", err)
	}
}

// TestTheStoresRefusalsAreTheControllers: a name a live row holds and a row
// that moved since it was read reach the controller as its own errors, which
// the API answers name_taken and version_conflict, and still match the
// store's.
func TestTheStoresRefusalsAreTheControllers(t *testing.T) {
	first, s := bound(t)
	second := store.ForController(s, "default", store.Delivered)
	if err := first.Write(t.Context(), sandbox("sbx_a", "dev", driver.Pending), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	err := second.Write(t.Context(), sandbox("sbx_b", "dev", driver.Pending), controller.MutationCreated)
	if !errors.Is(err, controller.ErrNameTaken) || !errors.Is(err, store.ErrNameTaken) {
		t.Errorf("a second live row of one name answered %v, want the controller's and the store's name taken", err)
	}
	if _, err := second.Load(); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(t.Context(), sandbox("sbx_a", "dev", driver.Running), controller.MutationStarted); err != nil {
		t.Fatal(err)
	}
	err = first.Write(t.Context(), sandbox("sbx_a", "dev", driver.Stopped), controller.MutationStopped)
	if !errors.Is(err, controller.ErrVersionConflict) || !errors.Is(err, store.ErrVersionConflict) {
		t.Errorf("a write over a row that moved answered %v, want the controller's and the store's version conflict", err)
	}
}

// TestReleaseAllFreesEveryLeaseTheWriterLast: a handoff frees the loops'
// leases and the writer's, so a successor takes each at once.
func TestReleaseAllFreesEveryLeaseTheWriterLast(t *testing.T) {
	old, s := bound(t)
	old.Advertise("http://10.0.0.1:8080")
	for _, name := range []string{store.WriterLease, controller.ReaperLease, controller.SchedulerLease} {
		if held, err := old.Acquire(t.Context(), name, time.Minute); err != nil || !held {
			t.Fatalf("taking %s: %v, %v", name, held, err)
		}
	}
	lease, err := old.Lease(t.Context(), store.WriterLease)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != old.Holder() || lease.Address != "http://10.0.0.1:8080" || !lease.Live {
		t.Errorf("the writer's row reads %+v", lease)
	}
	if err := old.ReleaseAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	successor := store.ForController(s, "default", store.Delivered)
	for _, name := range []string{store.WriterLease, controller.ReaperLease, controller.SchedulerLease} {
		if held, err := successor.Acquire(t.Context(), name, time.Minute); err != nil || !held {
			t.Errorf("the successor could not take %s after the release: %v, %v", name, held, err)
		}
	}
}
