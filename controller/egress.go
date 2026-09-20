// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// ErrNoGateway is a create whose boundary needs a gateway and found none. The
// gateway is part of the environment's data plane, so the API answers it the
// way it answers an unavailable driver: the environment is unavailable.
var ErrNoGateway = errors.New("no gateway of the environment acknowledged the sandbox's map")

// Egress is the control plane's half of the gateway protocol, as the
// controller needs it. The implementation holds the connected gateways of the
// environment and never dials one; this interface is what keeps the
// controller free of that, and of HTTP.
type Egress interface {
	// Send pushes one sandbox's map and returns when a gateway of the
	// environment has acknowledged it, or ErrNoGateway when none does.
	Send(ctx context.Context, m egress.Map) error
	// Purge drops a principal from every gateway of the environment. A
	// gateway that is not connected learns it from the next snapshot.
	Purge(ctx context.Context, principal string)
	// CA is the certificate authority the environment's gateways terminate
	// TLS with, in PEM, or empty when none has connected. The driver
	// projects it into the sandbox so the workload trusts that door.
	CA() string
}

// GatewayAddresses is where sandboxes of this environment reach the gateway's
// two doors. They are the operator's (CELLA_GATEWAY and
// CELLA_GATEWAY_REVERSE), not the gateway's own listen addresses, because
// what a sandbox dials is a question about the network it sits in.
type GatewayAddresses struct {
	Proxy   string
	Reverse string
}

// compileEgress mints what a new sandbox's boundary needs and compiles its
// map. The credential is minted once here and lives in desired state, so a
// recompile after a restart hands the gateway the same credential the running
// sandbox already holds in its own environment.
func (c *Controller) compileEgress(obj *v1.Sandbox) (egress.Map, error) {
	if obj.Status.EgressState == nil {
		credential, err := egress.MintCredential()
		if err != nil {
			return egress.Map{}, err
		}
		obj.Status.EgressState = &v1.EgressState{Credential: credential}
	}
	obj.Status.EgressState.Version++
	// The mounted secrets join here when the Secret kind lands: the views
	// come from the store, with one minted placeholder each, and the values
	// reach the gateway and nothing else.
	return egress.Compile(*obj, nil)
}

// needsGateway reports whether this boundary is one a gateway must hold
// before the sandbox exists. A sandbox that reaches everything and
// substitutes nothing has nothing for a gateway to enforce or inject, so it
// does not wait for one; any narrower boundary does, and fails rather than
// starting outside it.
func needsGateway(m egress.Map) bool {
	return m.Mode != v1.EgressOpen || len(m.Deny) > 0 || len(m.Entries) > 0 || len(m.NotInjectable) > 0
}

// pushEgress compiles the sandbox's map and puts it in a gateway before the
// driver is called, which is the order spec 018 fixes: a sandbox never starts
// before a gateway knows it. It reports whether a gateway holds the map, so
// the caller writes the EgressEnforced condition from what happened rather
// than from what was asked for.
func (c *Controller) pushEgress(ctx context.Context, obj *v1.Sandbox) (m egress.Map, held bool, err error) {
	m, err = c.compileEgress(obj)
	if err != nil {
		return egress.Map{}, false, err
	}
	if c.egress == nil {
		if needsGateway(m) {
			return m, false, ErrNoGateway
		}
		return m, false, nil
	}
	if err = c.egress.Send(ctx, m); err != nil {
		if needsGateway(m) {
			return m, false, err
		}
		// Nothing to enforce and nothing to substitute: the sandbox is
		// within its declared boundary with or without a gateway, and the
		// condition says which it got.
		c.log.WarnContext(ctx, "the sandbox's map reached no gateway", "sandbox", obj.Status.ID, "err", err)
		return m, false, nil
	}
	return m, true, nil
}

// egressSpec is what the driver projects into the sandbox: the boundary it
// enforces itself, the doors to point the workload at, the sandbox's own
// credential, and the authority that signs the proxy door's leaves.
func (c *Controller) egressSpec(m egress.Map) driver.Egress {
	spec := driver.Egress{
		Mode:         string(m.Mode),
		AllowedHosts: slices.Clone(m.Allow),
		DeniedHosts:  slices.Clone(m.Deny),
		Credential:   m.Credential,
		ProxyAddr:    c.gateway.Proxy,
		ReverseAddr:  c.gateway.Reverse,
	}
	if c.egress != nil {
		spec.CAPEM = c.egress.CA()
	}
	return spec
}

// egressCondition is the conjunction of the two enforcement points. The
// driver keeps the workload inside the boundary and the gateway substitutes
// within it; a boundary one of them does not hold is not enforced, and the
// condition says which one is missing rather than reporting true.
func (c *Controller) egressCondition(m egress.Map, held bool, now time.Time) v1.Condition {
	cond := v1.Condition{Type: v1.ConditionEgressEnforced, Status: v1.ConditionTrue, Reason: v1.ReasonEnforced, Since: now}
	switch {
	case !slices.Contains(c.driver.Capabilities().Egress, m.Mode):
		cond.Status = v1.ConditionFalse
		cond.Reason = v1.ReasonNotEnforcedByDriver
		cond.Message = "This environment records the boundary and does not confine the workload to it."
	case !held:
		cond.Status = v1.ConditionFalse
		cond.Reason = v1.ReasonNoGateway
		cond.Message = "No gateway of this environment holds the sandbox's map."
	}
	return cond
}

// setCondition replaces a condition of the same type, keeping the instant it
// last changed, so "since" says when the answer changed and not when it was
// last read.
func setCondition(conditions []v1.Condition, next v1.Condition) []v1.Condition {
	for i, c := range conditions {
		if c.Type != next.Type {
			continue
		}
		if c.Status == next.Status && c.Reason == next.Reason {
			next.Since = c.Since
		}
		conditions[i] = next
		return conditions
	}
	return append(conditions, next)
}

// EgressMaps is every live sandbox's map, for a gateway that connects
// holding nothing. It is a function of desired state alone, so the answer
// after a control plane restart is the answer before it: the credential and
// the placeholders live in the object, not in the gateway's memory or in
// this process's.
func (c *Controller) EgressMaps() []egress.Map {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]egress.Map, 0, len(c.objects))
	for _, obj := range c.objects {
		if obj.Status.Phase == "Deleting" {
			continue
		}
		m, err := egress.Compile(obj, nil)
		if err != nil {
			// A stored object the compiler refuses is a defect in what was
			// written, not a reason to hand the gateway a partial world: the
			// sandbox is left out and named, so it is visible in the log
			// rather than silently reachable.
			c.log.Error("the stored boundary of a sandbox does not compile", "sandbox", obj.Status.ID, "err", err)
			continue
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b egress.Map) int { return strings.Compare(a.Principal, b.Principal) })
	return out
}
