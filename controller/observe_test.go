// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestObserveEnvironmentWritesAtOnce: a worker that has registered and not
// yet opened its stream makes its environment Ready, with the worker counted
// and its host shown, the moment the status is observed, and one whose stream
// ended and that is no longer pending stops being counted the same way.
func TestObserveEnvironmentWritesAtOnce(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{})
	applyWorker(t, c, "laptop")
	host := v1.HostInfo{OS: "darwin", Arch: "arm64", Network: v1.HostNetwork{Mode: "none"}}
	w.set([]Registration{{Worker: "wrk_1", LastHeartbeat: c.clock.Now(), Pending: true, Host: &host}})
	if err := c.ObserveEnvironment(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	obj, err := c.GetEnvironment("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != v1.EnvironmentReady || obj.Status.Workers != 1 || len(obj.Status.Hosts) != 1 ||
		!obj.Status.Hosts[0].Host.Equal(host) {
		t.Fatalf("a registered worker leaves the environment at %s with %d workers and hosts %+v",
			obj.Status.Phase, obj.Status.Workers, obj.Status.Hosts)
	}

	w.set([]Registration{{Worker: "wrk_1", LastHeartbeat: c.clock.Now(), Host: &host}})
	if err = c.ObserveEnvironment(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	if obj, err = c.GetEnvironment("laptop"); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Workers != 0 || len(obj.Status.Hosts) != 0 {
		t.Errorf("a worker whose stream ended is still counted: %d workers, hosts %+v", obj.Status.Workers, obj.Status.Hosts)
	}
	if err = c.ObserveEnvironment(t.Context(), "absent"); err != nil {
		t.Errorf("an environment this control plane holds no object of answered %v", err)
	}
}

// TestObserveEnvironmentHoldsTheLease: a status written at once is written
// only by the replica holding the environments lease, as the loop's is, and a
// lease that cannot be read is an error.
func TestObserveEnvironmentHoldsTheLease(t *testing.T) {
	c, _, w, _ := twoEnvironments(t, Options{Lease: refusingLease{}})
	applyWorker(t, c, "laptop")
	w.set([]Registration{{Worker: "wrk_1", LastHeartbeat: c.clock.Now(), Pending: true}})
	if err := c.ObserveEnvironment(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	if phase(t, c, "laptop") != v1.EnvironmentPending {
		t.Errorf("a replica without the lease wrote %s", phase(t, c, "laptop"))
	}

	c, _, _, _ = twoEnvironments(t, Options{Lease: failingLease{}})
	applyWorker(t, c, "laptop")
	if err := c.ObserveEnvironment(t.Context(), "laptop"); err == nil {
		t.Error("a lease that could not be read wrote nonetheless")
	}
}
