// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// askingDriver is the native driver behind a host that holds each create
// until the test answers it, the way a worker holds one while its host asks.
type askingDriver struct {
	*native.Driver
	asked  chan string
	answer chan error
}

func (d askingDriver) Create(ctx context.Context, spec driver.CreateSpec) (driver.Ref, error) {
	d.asked <- spec.ID
	if err := <-d.answer; err != nil {
		return driver.Ref{}, err
	}
	return d.Driver.Create(ctx, spec)
}

// openAsking is a controller over an askingDriver, with the realize of one
// placed create running beside the test so it can mark the sandbox held.
func openAsking(t *testing.T) (*Controller, askingDriver) {
	t.Helper()
	n, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	d := askingDriver{Driver: n, asked: make(chan string, 1), answer: make(chan error, 1)}
	c, err := Open(t.Context(), Options{DataDir: t.TempDir(), Driver: d, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, d
}

// realizeAsked creates one sandbox and runs its realize until the driver has
// been asked, returning the sandbox and the channel the realize ends on.
func realizeAsked(t *testing.T, c *Controller, d askingDriver) (v1.Sandbox, <-chan error) {
	t.Helper()
	sb := workspace()
	sb.Spec.Command = []string{"sleep", "300"}
	out, err := c.Create(t.Context(), sb, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		done <- c.realize(context.Background(), out.Status.ID)
	}()
	select {
	case <-d.asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the driver was never asked to create")
	}
	return out, done
}

func approvalOf(t *testing.T, c *Controller, id string) (v1.Sandbox, v1.Condition) {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cond, _ := hostApprovalOf(obj)
	return obj, cond
}

// TestHostPendingThenApproved: a create the host holds is marked waiting for
// the host while it is Pending, once however often the hold is told, and
// becomes True when the host allows it and the sandbox runs.
func TestHostPendingThenApproved(t *testing.T) {
	c, d := openAsking(t)
	out, done := realizeAsked(t, c, d)
	for range 2 {
		if err := c.HostPending(t.Context(), "default", out.Status.ID); err != nil {
			t.Fatalf("HostPending: %v", err)
		}
	}
	obj, cond := approvalOf(t, c, out.Status.ID)
	if obj.Status.Phase != driver.Pending || cond.Status != v1.ConditionUnknown || cond.Reason != v1.ReasonHostPending {
		t.Fatalf("a held sandbox reads %s with %+v", obj.Status.Phase, cond)
	}
	d.answer <- nil
	if err := <-done; err != nil {
		t.Fatalf("the realize ended with %v", err)
	}
	obj, cond = approvalOf(t, c, out.Status.ID)
	if obj.Status.Phase != driver.Running || cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonHostApproved {
		t.Fatalf("an allowed sandbox reads %s with %+v", obj.Status.Phase, cond)
	}
	// A hold told after the create was answered changes nothing.
	if err := c.HostPending(t.Context(), "default", out.Status.ID); err != nil {
		t.Fatal(err)
	}
	if _, again := approvalOf(t, c, out.Status.ID); again.Status != v1.ConditionTrue {
		t.Errorf("a late hold rewrote the answered condition: %+v", again)
	}
}

// TestHostRefusalFailsTheSandbox: a create the host refused, or did not
// answer, fails the sandbox with the host's reason, and the condition
// carries what the host said; any other failure is a CreateFailed.
func TestHostRefusalFailsTheSandbox(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
		cond   string
	}{
		{fmt.Errorf("not now (%w) (%w)", driver.ErrHostRefused, driver.ErrHostRefused), ReasonHostRefused, v1.ReasonHostRefused},
		{fmt.Errorf("/srv is not granted (%w)", driver.ErrWorkspaceNotGranted), ReasonHostRefused, v1.ReasonHostRefused},
		{fmt.Errorf("no answer (%w)", driver.ErrHostTimeout), ReasonHostTimeout, v1.ReasonHostTimeout},
		{errors.New("the engine fell over"), ReasonCreateFailed, ""},
	} {
		t.Run(c.reason, func(t *testing.T) {
			ctl, d := openAsking(t)
			out, done := realizeAsked(t, ctl, d)
			d.answer <- c.err
			<-done
			obj, cond := approvalOf(t, ctl, out.Status.ID)
			if obj.Status.Phase != PhaseFailed || obj.Status.Reason != c.reason || cond.Reason != c.cond {
				t.Fatalf("the sandbox reads %s %s with %+v", obj.Status.Phase, obj.Status.Reason, cond)
			}
			if c.cond != "" && (cond.Status != v1.ConditionFalse || cond.Message == "" || cond.Message == c.err.Error()) {
				t.Errorf("the condition does not carry the host's own text: %+v", cond)
			}
		})
	}
}

// TestHostPendingIgnoresWhatItDoesNotHold: a hold for a sandbox that is not
// held here, or that lives on another environment, writes nothing.
func TestHostPendingIgnoresWhatItDoesNotHold(t *testing.T) {
	c, d := openAsking(t)
	if err := c.HostPending(t.Context(), "default", "sbx_nobody"); err != nil {
		t.Fatal(err)
	}
	out, done := realizeAsked(t, c, d)
	if err := c.HostPending(t.Context(), "elsewhere", out.Status.ID); err != nil {
		t.Fatal(err)
	}
	if _, cond := approvalOf(t, c, out.Status.ID); cond.Type != "" {
		t.Errorf("a hold from another environment marked the sandbox: %+v", cond)
	}
	d.answer <- nil
	<-done
	if _, cond := approvalOf(t, c, out.Status.ID); cond.Type != "" {
		t.Errorf("a sandbox its host was never asked about carries %+v", cond)
	}
}
