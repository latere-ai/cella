// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// MutationHostPending is the record of a create a worker holds while its host
// decides (spec 082): the sandbox stays Pending, and its HostApproval
// condition says it is waiting for the host.
const MutationHostPending = "sandbox.host_pending"

// The reasons a sandbox fails with when its host said no to the create, or
// did not answer within the time its worker holds one.
const (
	ReasonHostRefused = "HostRefused"
	ReasonHostTimeout = "HostTimeout"
)

// HostPending records that the worker serving a sandbox's environment holds
// its create while the host decides. It writes the HostApproval condition
// Unknown with the reason Pending and emits sandbox.host_pending, once. A
// sandbox that is gone, no longer Pending, on another environment, or
// already marked is left as it is, because the held frame can arrive after
// the create was answered or the sandbox was deleted.
func (c *Controller) HostPending(ctx context.Context, environment, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, held := c.objects[id]
	if !held || obj.Status.Phase != driver.Pending || obj.Status.Environment != environment {
		return nil
	}
	if cond, has := hostApprovalOf(obj); has && cond.Reason == v1.ReasonHostPending {
		return nil
	}
	obj = clone(obj)
	obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
		Type: v1.ConditionHostApproval, Status: v1.ConditionUnknown, Reason: v1.ReasonHostPending,
		Message: "Waiting for the host.", Since: c.clock.Now(),
	})
	return c.persist(ctx, obj, MutationHostPending)
}

// hostApproved turns a pending HostApproval into True once the create the
// host held has been answered with a sandbox. A sandbox the host was never
// asked about carries no condition and gets none.
func hostApproved(obj v1.Sandbox, now time.Time) v1.Sandbox {
	if cond, has := hostApprovalOf(obj); !has || cond.Status != v1.ConditionUnknown {
		return obj
	}
	obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
		Type: v1.ConditionHostApproval, Status: v1.ConditionTrue, Reason: v1.ReasonHostApproved, Since: now,
	})
	return obj
}

// hostRefusal is the failure a create the host refused, or did not answer in
// time, writes: the sandbox's reason and the HostApproval condition, whose
// message is the host's own text. Any other error is not the host's, and the
// sandbox fails as every create does.
func hostRefusal(obj v1.Sandbox, err error, now time.Time) (v1.Sandbox, bool) {
	var reason, condReason string
	switch {
	case errors.Is(err, driver.ErrHostTimeout):
		reason, condReason = ReasonHostTimeout, v1.ReasonHostTimeout
	case errors.Is(err, driver.ErrHostRefused), errors.Is(err, driver.ErrWorkspaceNotGranted):
		reason, condReason = ReasonHostRefused, v1.ReasonHostRefused
	default:
		return obj, false
	}
	obj.Status.Reason = reason
	obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
		Type: v1.ConditionHostApproval, Status: v1.ConditionFalse, Reason: condReason,
		Message: hostText(err), Since: now,
	})
	return obj, true
}

// hostText is what the host said, without the vocabulary the worker and the
// seam each wrapped it in, so the condition's message reads as the host
// wrote it.
func hostText(err error) string {
	text := err.Error()
	for {
		trimmed := text
		for _, sentinel := range []error{driver.ErrHostRefused, driver.ErrHostTimeout, driver.ErrWorkspaceNotGranted} {
			trimmed = strings.TrimSuffix(trimmed, " ("+sentinel.Error()+")")
		}
		if trimmed == text {
			return text
		}
		text = trimmed
	}
}

// hostApprovalOf is a sandbox's HostApproval condition, and whether it has one.
func hostApprovalOf(obj v1.Sandbox) (v1.Condition, bool) {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionHostApproval {
			return cond, true
		}
	}
	return v1.Condition{}, false
}
