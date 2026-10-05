// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// holding writes a sandbox of owner in phase straight into desired state, as
// the controller would have left it, so a case can put a phase in front of
// the count that no driver reaches on demand.
func holding(t *testing.T, c *Controller, owner, name, phase string) {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[id] = v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: name},
		Status: v1.SandboxStatus{ID: id, Owner: owner, Environment: c.environment, Phase: phase}}
}

// stoppedSandbox creates and realizes a sandbox of owner under name and stops
// it, with no ceiling, and answers its id.
func stoppedSandbox(t *testing.T, c *Controller, owner, name string) string {
	t.Helper()
	obj := workspace()
	obj.Metadata.Name = name
	made, err := realized(t.Context(), c, obj, owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := c.Act(t.Context(), made.Status.ID, "stop")
	if err != nil || stopped.Status.Phase != driver.Stopped {
		t.Fatalf("stop %s: %+v %v", name, stopped.Status, err)
	}
	return made.Status.ID
}

// phaseOf is the phase desired state holds for id.
func phaseOf(c *Controller, id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.objects[id].Status.Phase
}

// TestTheCountHoldsWhatRunsOrWillRun is spec 080's table: a create at a
// ceiling of one is refused behind a sandbox that runs or reaches running
// without a start, an unknown phase included, and admitted behind one that
// is stopped, failed or deleting.
func TestTheCountHoldsWhatRunsOrWillRun(t *testing.T) {
	for _, tc := range []struct {
		phase string
		holds bool
	}{
		{driver.Pending, true},
		{PhaseQueued, true},
		{PhaseStarting, true},
		{driver.Running, true},
		{"Stopping", true},
		{PhaseLost, true},
		{PhaseRecovering, true},
		{"Hibernating", true},
		{driver.Stopped, false},
		{PhaseFailed, false},
		{PhaseDeleting, false},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			c, _ := newController(t)
			holding(t, c, "alice", "old", tc.phase)
			// Another owner's sandbox is never this owner's count.
			holding(t, c, "bob", "other", driver.Running)
			_, err := realized(t.Context(), c, workspace(), "alice", 1)
			switch {
			case tc.holds && !errors.Is(err, ErrQuota):
				t.Fatalf("a create behind a %s sandbox at a ceiling of one: %v, want the quota refusal", tc.phase, err)
			case !tc.holds && err != nil:
				t.Fatalf("a create behind a %s sandbox at a ceiling of one: %v", tc.phase, err)
			}
		})
	}
}

// TestAStartPastTheCeilingIsRefused: with one sandbox running at a ceiling of
// one, a start of a stopped one is refused, says the count it would make, and
// leaves the sandbox stopped in desired state and on the runtime; under the
// ceiling the start runs and its sandbox holds the slot against a create.
func TestAStartPastTheCeilingIsRefused(t *testing.T) {
	c, _ := newController(t)
	ctx := t.Context()
	first := stoppedSandbox(t, c, "alice", "first")
	second := workspace()
	second.Metadata.Name = "second"
	running, err := realized(ctx, c, second, "alice", 1)
	if err != nil {
		t.Fatalf("a create beside a stopped sandbox at a ceiling of one: %v", err)
	}
	_, err = c.Start(ctx, first, 1)
	if !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "would make 2 running sandboxes of a ceiling of 1") {
		t.Fatalf("a start past the ceiling: %v", err)
	}
	if phaseOf(c, first) != driver.Stopped {
		t.Fatalf("the refused start left %s", phaseOf(c, first))
	}
	if state, err := openDriver(c).Inspect(ctx, first); err != nil || state.Phase != driver.Stopped {
		t.Fatalf("the refused start reached the runtime: %+v %v", state, err)
	}
	if _, err := c.Act(ctx, running.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	started, err := c.Start(ctx, first, 1)
	if err != nil || started.Status.Phase != driver.Running {
		t.Fatalf("a start under the ceiling: %+v %v", started.Status, err)
	}
	third := workspace()
	third.Metadata.Name = "third"
	if _, err := realized(ctx, c, third, "alice", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("a create behind the started sandbox: %v, want the quota refusal", err)
	}
}

// TestAStopFreesASlot: at a ceiling of one a second create is refused while
// the first sandbox runs and admitted once it stops, and a stop of the second
// lets the first start again.
func TestAStopFreesASlot(t *testing.T) {
	c, _ := newController(t)
	ctx := t.Context()
	first, err := realized(ctx, c, workspace(), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	second := workspace()
	second.Metadata.Name = "second"
	if _, err := realized(ctx, c, second, "alice", 1); !errors.Is(err, ErrQuota) || !strings.Contains(err.Error(), "runs 1 sandboxes of a ceiling of 1") {
		t.Fatalf("a create at the ceiling: %v", err)
	}
	if _, err := c.Act(ctx, first.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	made, err := realized(ctx, c, second, "alice", 1)
	if err != nil {
		t.Fatalf("a create after the stop: %v", err)
	}
	if _, err := c.Start(ctx, first.Status.ID, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("a start while the second runs: %v", err)
	}
	if _, err := c.Act(ctx, made.Status.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(ctx, first.Status.ID, 1); err != nil {
		t.Fatalf("a start after the second stopped: %v", err)
	}
}

// laggingDriver answers Stopped for a sandbox it has started, as a runtime
// whose read does not yet show a start it took.
type laggingDriver struct {
	driver.Driver
	mu      sync.Mutex
	started map[string]bool
}

func (d *laggingDriver) Start(ctx context.Context, id string) error {
	if err := d.Driver.Start(ctx, id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started[id] = true
	return nil
}

func (d *laggingDriver) Inspect(ctx context.Context, id string) (driver.State, error) {
	s, err := d.Driver.Inspect(ctx, id)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil && d.started[id] {
		s.Phase = driver.Stopped
	}
	return s, err
}

// TestTwoStartsRaceForOneSlot: two stopped sandboxes started at once at a
// ceiling of one start one, on a runtime whose read still says Stopped after
// it took the start: the winner's record holds the slot as Starting, which is
// what the loser's count reads.
func TestTwoStartsRaceForOneSlot(t *testing.T) {
	c, _ := newController(t)
	ids := []string{stoppedSandbox(t, c, "alice", "first"), stoppedSandbox(t, c, "alice", "second")}
	c.setDriver(c.environment, &laggingDriver{Driver: openDriver(c), started: map[string]bool{}})
	var won, refused atomic.Int32
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			_, err := c.Start(t.Context(), id, 1)
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrQuota):
				refused.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 || refused.Load() != 1 {
		t.Fatalf("%d starts ran and %d were refused, want one each", won.Load(), refused.Load())
	}
	phases := []string{phaseOf(c, ids[0]), phaseOf(c, ids[1])}
	slices.Sort(phases)
	if !slices.Equal(phases, []string{PhaseStarting, driver.Stopped}) {
		t.Fatalf("desired state holds %v, want one Starting and one Stopped", phases)
	}
}

// TestAStartThatCannotBeRecordedIsStoppedAgain: a start whose record fails to
// write leaves no sandbox running that the count does not hold.
func TestAStartThatCannotBeRecordedIsStoppedAgain(t *testing.T) {
	c, _ := newController(t)
	id := stoppedSandbox(t, c, "alice", "first")
	c.store = &memoryStore{saveErr: errors.New("write failed")}
	if _, err := c.Start(t.Context(), id, 0); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("a start whose record failed: %v", err)
	}
	if phaseOf(c, id) != driver.Stopped {
		t.Fatalf("desired state holds %s", phaseOf(c, id))
	}
	if state, err := openDriver(c).Inspect(t.Context(), id); err != nil || state.Phase != driver.Stopped {
		t.Fatalf("the runtime holds %+v %v, want the sandbox stopped again", state, err)
	}
}

// TestActTakesNoStart: a start is Start, which takes the ceiling, and Act
// refuses one rather than start past it.
func TestActTakesNoStart(t *testing.T) {
	c, _ := newController(t)
	id := stoppedSandbox(t, c, "alice", "first")
	if _, err := c.Act(t.Context(), id, "start"); !errors.Is(err, ErrPhase) || !strings.Contains(err.Error(), "Controller.Start") {
		t.Fatalf("Act start: %v", err)
	}
	if phaseOf(c, id) != driver.Stopped {
		t.Fatalf("Act start left %s", phaseOf(c, id))
	}
}
