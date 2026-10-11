// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// failedOn makes the driver report a sandbox's workload as one that ran and
// ended in failure, with the reason and the exit code a runtime gives.
func failedOn(d *fakeDriver, id, reason string, code int) {
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Reason, state.StoppedAt = PhaseFailed, reason, f.clock.Now()
		state.ExitCode = nil
		if code != 0 {
			state.ExitCode = &code
		}
		f.states[id] = state
	})
}

// recordedAs writes the status this control plane leaves on a sandbox it
// failed itself, as the path that fails it does.
func recordedAs(t *testing.T, c *Controller, id, reason string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	obj := clone(c.objects[id])
	obj.Status.Phase, obj.Status.Reason = PhaseFailed, reason
	if err := c.persist(t.Context(), obj, MutationFailed); err != nil {
		t.Fatal(err)
	}
}

// TestAFailedSandboxStartsAgain: a sandbox whose workload ran and ended in
// failure is Failed with the reason its runtime gives, and a start runs the
// workload again on the workspace the runtime still holds. It holds for
// every reason a runtime reports, because what decides it is that the runtime
// holds the sandbox and not what the reason is called: a non-zero exit under
// the native runtime's name and under a container runtime's, a kill for
// memory, an eviction, and a runtime that names no reason at all. The start
// is recorded as one, and the sandbox reads Running with nothing left of the
// failure.
func TestAFailedSandboxStartsAgain(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		code         int
	}{
		{"a non-zero exit as the native runtime names it", "Exited", 3},
		{"a non-zero exit as a container runtime names it", "Error", 1},
		{"a kill for memory", reasonOOMKilled, killedExit},
		{"an eviction, with no exit code", "Evicted", 0},
		{"a main process that could not be run", "DriverFailed", 0},
		{"a runtime that names no reason", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &recorded{}
			c, d, _ := newFake(t, Options{Events: events})
			id := created(t, c, "work").Status.ID
			failedOn(d, id, tc.reason, tc.code)
			got, err := c.Refresh(t.Context(), stored(t, c, id))
			if err != nil || got.Status.Phase != PhaseFailed || got.Status.Reason != tc.reason {
				t.Fatalf("the sandbox reads %s %q, %v, want Failed %q", got.Status.Phase, got.Status.Reason, err, tc.reason)
			}
			before := len(events.of(MutationStarted))
			started, err := c.Start(t.Context(), id, 0)
			if err != nil {
				t.Fatalf("a start of a sandbox that failed with %q: %v", tc.reason, err)
			}
			if started.Status.Phase != driver.Running || started.Status.Reason != "" || started.Status.ExitCode != nil {
				t.Fatalf("the started sandbox answers %s %q with exit %v, want Running with nothing of the failure", started.Status.Phase, started.Status.Reason, started.Status.ExitCode)
			}
			if got := d.started(); !slices.Equal(got, []string{id}) {
				t.Fatalf("the driver was asked to start %v, want the sandbox once", got)
			}
			if n := len(events.of(MutationStarted)) - before; n != 1 {
				t.Fatalf("the start recorded %d sandbox.started, want 1", n)
			}
			if kept := stored(t, c, id); kept.Status.Phase != driver.Running {
				t.Fatalf("the record after the start says %s", kept.Status.Phase)
			}
		})
	}
}

// TestAFailureTheControlPlaneRecordedIsNotStarted: a sandbox this control
// plane failed itself has nothing to start, and a start of it is refused as a
// conflict with its phase, naming the reason: a create that made nothing, an
// environment with no room, a queue that never placed it, and a recovery that
// gave up. The refusal stands whatever a runtime still holds under the id,
// because the identity and the boundary of such a sandbox ended with it. A
// failed sandbox its runtime no longer holds is refused the same way,
// whatever its reason. No refused start reaches the runtime.
func TestAFailureTheControlPlaneRecordedIsNotStarted(t *testing.T) {
	refused := func(t *testing.T, c *Controller, d *fakeDriver, id, detail string) {
		t.Helper()
		got, err := c.Start(t.Context(), id, 0)
		if !errors.Is(err, ErrPhase) || !strings.Contains(err.Error(), detail) {
			t.Fatalf("a start answered %v, want the phase conflict that says %q", err, detail)
		}
		if got.Status.Phase != PhaseFailed {
			t.Fatalf("the refused start answers the sandbox as %s, want Failed", got.Status.Phase)
		}
		if n := len(d.started()); n != 0 {
			t.Fatalf("a refused start reached the runtime %d times", n)
		}
	}
	t.Run("a create the runtime refused", func(t *testing.T) {
		c, d, _ := newFake(t, Options{})
		d.set(func(f *fakeDriver) { f.createErr = errors.New("the image cannot be pulled") })
		obj, err := realized(t.Context(), c, workspace(), "alice", 0)
		if err == nil || obj.Status.Phase != PhaseFailed || obj.Status.Reason != ReasonCreateFailed {
			t.Fatalf("the create left %s %q, %v", obj.Status.Phase, obj.Status.Reason, err)
		}
		refused(t, c, d, obj.Status.ID, "reason "+ReasonCreateFailed)
	})
	for _, reason := range unstartedFailures {
		t.Run(reason+" with nothing held", func(t *testing.T) {
			c, d, _ := newFake(t, Options{})
			id := created(t, c, "work").Status.ID
			if err := d.Delete(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			recordedAs(t, c, id, reason)
			refused(t, c, d, id, "reason "+reason)
		})
		t.Run(reason+" with an object left under the id", func(t *testing.T) {
			c, d, _ := newFake(t, Options{})
			id := created(t, c, "work").Status.ID
			recordedAs(t, c, id, reason)
			failedOn(d, id, "Error", 1)
			refused(t, c, d, id, "reason "+reason)
		})
	}
	t.Run("a failed sandbox its runtime no longer holds", func(t *testing.T) {
		c, d, _ := newFake(t, Options{})
		id := created(t, c, "work").Status.ID
		failedOn(d, id, "Exited", 3)
		// The failure is read and written, and then the runtime loses it.
		c.mu.Lock()
		obj, err := c.refresh(t.Context(), clone(c.objects[id]))
		if err == nil {
			err = c.persist(t.Context(), obj, MutationStatus)
		}
		c.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Delete(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		refused(t, c, d, id, "holds nothing of this failed sandbox")
	})
}

// TestAStartOfAFailedSandboxIsHeldToTheCountCeiling: a failed sandbox takes
// no slot of its owner's count ceiling, and the start that brings it back
// takes one under the rule a start of a stopped sandbox is held to. At the
// ceiling it is refused and reaches no runtime; under it the sandbox runs and
// holds the slot against a create.
func TestAStartOfAFailedSandboxIsHeldToTheCountCeiling(t *testing.T) {
	c, d, _ := newFake(t, Options{})
	ctx := t.Context()
	failed := created(t, c, "failed").Status.ID
	failedOn(d, failed, "Exited", 3)
	// The failure is written by the act that reads it, as a stop's is.
	c.mu.Lock()
	obj, err := c.refresh(ctx, clone(c.objects[failed]))
	if err == nil {
		err = c.persist(ctx, obj, MutationStatus)
	}
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	second := workspace()
	second.Metadata.Name = "second"
	running, err := realized(ctx, c, second, "alice", 1)
	if err != nil {
		t.Fatalf("a create beside a failed sandbox at a ceiling of one: %v", err)
	}
	if _, err := c.Start(ctx, failed, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("a start of a failed sandbox past the ceiling: %v, want the quota refusal", err)
	}
	if n := len(d.started()); n != 0 {
		t.Fatalf("the refused start reached the runtime %d times", n)
	}
	if _, err := c.Act(ctx, running.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	started, err := c.Start(ctx, failed, 1)
	if err != nil || started.Status.Phase != driver.Running {
		t.Fatalf("a start of a failed sandbox under the ceiling: %s, %v", started.Status.Phase, err)
	}
	third := workspace()
	third.Metadata.Name = "third"
	if _, err := realized(ctx, c, third, "alice", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("a create behind the started sandbox: %v, want the quota refusal", err)
	}
}

// TestAStartOfAFailedSandboxIsOneStart: a start is the client's act and is
// made once. A workload that is killed again as it starts leaves the sandbox
// Failed again: the start answers that the runtime took it, the next read
// says what became of it, and nothing starts it a second time, the sizing
// loop included, since it has no ceiling to grow under. A start the runtime
// refuses is the runtime's error and leaves the sandbox as it was.
func TestAStartOfAFailedSandboxIsOneStart(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{CPU: "2"})
	d.set2(func(d *sizeDriver) { d.needs = gi })
	d.kill(id)
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed || got.Status.Reason != reasonOOMKilled {
		t.Fatalf("a sandbox with no memory ceiling killed for memory reads %s %q, want Failed OOMKilled", got.Status.Phase, got.Status.Reason)
	}
	started, err := c.Start(t.Context(), id, 0)
	if err != nil || started.Status.Phase != PhaseStarting {
		t.Fatalf("the start answered %s, %v, want Starting: the runtime took it", started.Status.Phase, err)
	}
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed || got.Status.Reason != reasonOOMKilled {
		t.Fatalf("after a start whose workload was killed again the sandbox reads %s %q, want Failed OOMKilled", got.Status.Phase, got.Status.Reason)
	}
	readSizes(t, c)
	readSizes(t, c)
	if n := len(d.started()); n != 1 {
		t.Fatalf("the workload was started %d times for one start", n)
	}
	if n := len(events.resized()); n != 0 {
		t.Fatalf("a sandbox with no memory ceiling was resized %d times", n)
	}
	// A second start is a second act, and is made once as well.
	if _, err := c.Start(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(d.started()); n != 2 {
		t.Fatalf("two starts started the workload %d times", n)
	}

	// The runtime refuses the start: its error, and the sandbox as it was.
	d.set(func(f *fakeDriver) { f.startErr = errors.New("no node has room") })
	if _, err := c.Start(t.Context(), id, 0); err == nil || errors.Is(err, ErrPhase) {
		t.Fatalf("a start the runtime refused answered %v, want the runtime's error", err)
	}
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed {
		t.Fatalf("after a start the runtime refused the sandbox reads %s, want Failed", got.Status.Phase)
	}
}

// TestAKillAtOrUnderAMemoryCeilingAndAStart: where the control plane is
// starting a sandbox again at a larger size, a client's start is refused,
// because the start is already being made; where it is not, at the ceiling,
// the sandbox is Failed and a client's start brings it back at the size it
// holds, which is what a start of a sandbox with no ceiling does.
func TestAKillAtOrUnderAMemoryCeilingAndAStart(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{Memory: "768Mi"})
	d.kill(id)
	if _, err := c.Start(t.Context(), id, 0); !errors.Is(err, ErrPhase) {
		t.Fatalf("a start of a sandbox the control plane is starting at a larger size: %v, want ErrPhase", err)
	}
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != driver.Running || got.Status.Resources.Memory != "768Mi" {
		t.Fatalf("the control plane's own restart left %s at %s", got.Status.Phase, got.Status.Resources.Memory)
	}
	d.kill(id)
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed || got.Status.Reason != reasonOOMKilled {
		t.Fatalf("killed at its ceiling the sandbox reads %s %q, want Failed OOMKilled", got.Status.Phase, got.Status.Reason)
	}
	started, err := c.Start(t.Context(), id, 0)
	if err != nil || started.Status.Phase != driver.Running || started.Status.Resources.Memory != "768Mi" || started.Status.Moves != 1 {
		t.Fatalf("a start of a sandbox killed at its ceiling: %s at %s with %d moves, %v, want Running at 768Mi",
			started.Status.Phase, started.Status.Resources.Memory, started.Status.Moves, err)
	}
	if got := d.state(id).Resources.Memory; got != "768Mi" {
		t.Fatalf("the start gave the sandbox %s, want the 768Mi it held", got)
	}
}
