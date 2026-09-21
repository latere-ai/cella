// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

// environment is one Environment as the controller holds it.
func environment(name string) v1.Environment {
	return v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"region": "eu"}},
		Spec: v1.EnvironmentSpec{
			Mode: v1.EnvironmentWorker, Isolation: v1.IsolationContainer,
			Capacity:   v1.Capacity{CPU: "8", Memory: "16Gi", Sandboxes: 10},
			Scheduling: v1.SchedulingSpec{Mode: v1.SchedulingDirect, Queues: []string{"default"}, DefaultQueue: "default"},
		},
		Status: v1.EnvironmentStatus{ID: name, Owner: "alice", Phase: v1.EnvironmentPending},
	}
}

// TestBridgeWritesTheEnvironmentKind: the Environment kind round-trips
// through design 010's store with the row version an If-Match is compared
// against, and a write at a version the row has moved past is refused.
func TestBridgeWritesTheEnvironmentKind(t *testing.T) {
	c, _ := bound(t)
	ctx := t.Context()
	obj := environment("eu-gpu")

	first, err := c.WriteEnvironment(ctx, obj, 0, controller.MutationEnvironmentCreated)
	if err != nil {
		t.Fatalf("the create was not written: %v", err)
	}
	if first == 0 {
		t.Fatalf("the create returned version %d", first)
	}
	obj.Spec.Capacity.Sandboxes = 25
	second, err := c.WriteEnvironment(ctx, obj, first, controller.MutationEnvironmentUpdated)
	if err != nil {
		t.Fatalf("the update at the version read was refused: %v", err)
	}
	if second <= first {
		t.Errorf("the update did not move the version: %d then %d", first, second)
	}
	if _, err = c.WriteEnvironment(ctx, obj, first, controller.MutationEnvironmentUpdated); !errors.Is(err, controller.ErrVersionConflict) {
		t.Errorf("a stale version answered %v", err)
	}

	held, err := c.LoadEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	read, ok := held["eu-gpu"]
	switch {
	case !ok:
		t.Fatalf("the load answered %v", held)
	case read.Status.Version != second:
		t.Errorf("the load carries version %d, want the row's %d", read.Status.Version, second)
	case read.Spec.Capacity.Sandboxes != 25:
		t.Errorf("the load answers capacity %+v", read.Spec.Capacity)
	case read.Metadata.Labels["region"] != "eu":
		t.Errorf("the load lost the labels: %v", read.Metadata.Labels)
	case read.Status.Owner != "alice":
		t.Errorf("the load answers owner %q", read.Status.Owner)
	}

	// Every act is journaled in the transaction that wrote it.
	records, err := c.Events(ctx, "eu-gpu", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Errorf("the two writes journaled %d records", len(records))
	}
}

// TestBridgeWritesTheEnvironmentStatus: the phase loop's write touches the
// status and not the object or its version, and records a transition only
// where design 009 names a type for it.
func TestBridgeWritesTheEnvironmentStatus(t *testing.T) {
	c, _ := bound(t)
	ctx := t.Context()
	obj := environment("eu-gpu")
	version, err := c.WriteEnvironment(ctx, obj, 0, controller.MutationEnvironmentCreated)
	if err != nil {
		t.Fatal(err)
	}
	obj.Status.Version = version

	// A tick that found no transition writes state and no record.
	obj.Status.Workers = 1
	if err = c.WriteEnvironmentStatus(ctx, obj, ""); err != nil {
		t.Fatalf("the status write failed: %v", err)
	}
	records, err := c.Events(ctx, "eu-gpu", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Errorf("a status write with no transition journaled %d records", len(records))
	}

	// One with a transition writes both, in one transaction.
	obj.Status.Phase = v1.EnvironmentReady
	if err = c.WriteEnvironmentStatus(ctx, obj, controller.MutationEnvironmentRegistered); err != nil {
		t.Fatalf("the transition was not written: %v", err)
	}
	if records, err = c.Events(ctx, "eu-gpu", 10); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Errorf("the transition journaled %d records", len(records))
	}
	held, err := c.LoadEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	switch read := held["eu-gpu"]; {
	case read.Status.Phase != v1.EnvironmentReady || read.Status.Workers != 1:
		t.Errorf("the status write did not land: %+v", read.Status)
	case read.Status.Version != version:
		t.Errorf("the status write moved the version to %d, want %d", read.Status.Version, version)
	}

	// A status write for an environment the store does not hold is not found
	// rather than a row this process invented.
	absent := environment("us-east")
	if err = c.WriteEnvironmentStatus(ctx, absent, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a status write on an absent environment answered %v", err)
	}
}

// TestBridgeRemovesTheEnvironmentKind: a delete ends the row and records it,
// and a delete of a row another replica already removed still records that
// this one ended the object.
func TestBridgeRemovesTheEnvironmentKind(t *testing.T) {
	c, _ := bound(t)
	ctx := t.Context()
	if _, err := c.WriteEnvironment(ctx, environment("eu-gpu"), 0, controller.MutationEnvironmentCreated); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveEnvironment(ctx, "eu-gpu", controller.MutationEnvironmentDeleted); err != nil {
		t.Fatalf("the delete failed: %v", err)
	}
	held, err := c.LoadEnvironments()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Errorf("the load answers %v after the delete", held)
	}
	if err = c.RemoveEnvironment(ctx, "eu-gpu", controller.MutationEnvironmentDeleted); err != nil {
		t.Errorf("a second delete answered %v", err)
	}
}
