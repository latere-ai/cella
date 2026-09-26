// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"sync"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestTheSnapshotStoreTakesWritersUnderDifferentLocks: the controller writes
// a sandbox under its own lock and the phase loop writes an environment's
// status under none, and both rewrite the one snapshot document. The store
// serializes them itself and keeps its own copy of the sandboxes, so a write
// of the environment never reads the map the controller is changing, and the
// file holds the last of each. Run under the race detector, the version
// before the fix reported the environment write marshaling the controller's
// map while a delete changed it.
func TestTheSnapshotStoreTakesWritersUnderDifferentLocks(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*fileStore)
	// The map Load returns is the one the controller keeps and changes.
	objects, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	environment := v1.Environment{Metadata: v1.Metadata{Name: "default"}}
	if _, err := s.WriteEnvironment(t.Context(), environment, 0, ""); err != nil {
		t.Fatal(err)
	}
	const rounds = 50
	var wg sync.WaitGroup
	var sandboxErr, environmentErr error
	wg.Go(func() {
		for i := range rounds {
			id := fmt.Sprintf("sbx_%02d", i)
			objects[id] = v1.Sandbox{Status: v1.SandboxStatus{ID: id, Owner: "alice", Environment: "default"}}
			if i > 0 {
				delete(objects, fmt.Sprintf("sbx_%02d", i-1))
			}
			if err := s.Save(objects); err != nil {
				sandboxErr = err
				return
			}
		}
	})
	wg.Go(func() {
		for i := range rounds {
			next := environment
			next.Status.Phase = fmt.Sprintf("Phase%02d", i)
			if err := s.WriteEnvironmentStatus(t.Context(), next, ""); err != nil {
				environmentErr = err
				return
			}
		}
	})
	wg.Wait()
	if sandboxErr != nil || environmentErr != nil {
		t.Fatalf("a write failed: sandbox %v, environment %v", sandboxErr, environmentErr)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	held, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	last := fmt.Sprintf("sbx_%02d", rounds-1)
	if _, ok := held[last]; len(held) != 1 || !ok {
		t.Errorf("the snapshot holds %d sandboxes, want %s alone", len(held), last)
	}
	environments, err := reopened.(Environments).LoadEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	if got := environments["default"].Status.Phase; got != fmt.Sprintf("Phase%02d", rounds-1) {
		t.Errorf("the snapshot holds the environment at %q, want the last phase written", got)
	}
}
