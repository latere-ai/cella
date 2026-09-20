// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/manifest"
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

// boundary is what one compile produced: the map a gateway is handed, the
// environment the driver projects into the sandbox, the mounted and
// uninjectable names the status reports, and whether a gateway took it.
type boundary struct {
	Map     egress.Map
	Env     map[string]string
	Secrets v1.SecretsStatus
	Held    bool
}

// compileEgress mints what a sandbox's boundary needs and compiles its map.
// The credential and every placeholder are minted once here and live in
// desired state, so a recompile after a restart hands the gateway the same
// credential and the same placeholders the running sandbox already holds in
// its own environment.
//
// This is the one place a secret's value is read. What the value reaches is
// the map, which reaches the environment's gateways over the sync stream and
// nothing else.
func (c *Controller) compileEgress(ctx context.Context, obj *v1.Sandbox) (boundary, error) {
	if obj.Status.EgressState == nil {
		credential, err := egress.MintCredential()
		if err != nil {
			return boundary{}, err
		}
		obj.Status.EgressState = &v1.EgressState{Credential: credential}
	}
	obj.Status.EgressState.Version++
	views, status, env, err := c.secretViews(ctx, obj)
	if err != nil {
		return boundary{}, err
	}
	m, err := egress.Compile(*obj, views)
	if err != nil {
		return boundary{}, err
	}
	// A secret the boundary denies every host of is the compiler's answer;
	// one whose object is gone is this function's. Both mean the same thing
	// to the workload, so they are reported in one list.
	status.NotInjectable = append(status.NotInjectable, m.NotInjectable...)
	slices.Sort(status.NotInjectable)
	status.NotInjectable = slices.Compact(status.NotInjectable)
	return boundary{Map: m, Env: env, Secrets: status}, nil
}

// secretViews binds each mount to a Secret, mints a placeholder for one that
// has none, reads the value, and builds what the compiler needs. It also
// returns what the status reports and the environment the driver projects.
//
// Binding is by the id recorded in desired state: a secret deleted and
// recreated under one name is a different secret, and a sandbox that was
// started against the first is not silently handed the second. The record is
// kept even for a mount whose secret is gone, because the placeholder is in
// the workload's environment and must not change under it.
func (c *Controller) secretViews(ctx context.Context, obj *v1.Sandbox) ([]egress.SecretView, v1.SecretsStatus, map[string]string, error) {
	if obj.Status.EgressState == nil {
		// A sandbox written before its boundary was compiled, which is what
		// a crash between the two writes of a create leaves behind. It is
		// read here rather than refused, so one such row does not stop a
		// control plane from handing every other sandbox its map.
		obj.Status.EgressState = &v1.EgressState{}
	}
	mounts := obj.Spec.Secrets
	state := obj.Status.EgressState
	if len(mounts) == 0 {
		state.Secrets = nil
		return nil, v1.SecretsStatus{}, nil, nil
	}
	var (
		views  []egress.SecretView
		status v1.SecretsStatus
		env    = map[string]string{}
		bound  = make([]v1.MountedSecret, 0, len(mounts))
	)
	for _, mount := range mounts {
		record, err := c.bindMount(state.Secrets, mount, obj.Status.Owner)
		if err != nil {
			return nil, v1.SecretsStatus{}, nil, err
		}
		bound = append(bound, record)
		env[mount.Env] = record.Placeholder
		status.Mounted = append(status.Mounted, record.Name)

		secret, held := c.secretObjects[record.ID]
		if !held {
			// The secret was deleted under a running sandbox. The
			// placeholder stays where it is and leaves as the inert string
			// it always was.
			status.NotInjectable = append(status.NotInjectable, record.Name)
			continue
		}
		maps.Copy(env, manifest.CompanionEnv(mount.Env, secret.Spec.Inject))
		value, _, err := c.secrets.OpenValue(ctx, record.ID)
		if err != nil {
			c.log.ErrorContext(ctx, "a mounted secret has no value to substitute",
				"sandbox", obj.Status.ID, "secret", record.ID, "err", err)
			status.NotInjectable = append(status.NotInjectable, record.Name)
			continue
		}
		views = append(views, viewOf(secret, record.Placeholder, string(value)))
	}
	state.Secrets = bound
	return views, status, env, nil
}

// bindMount finds the record of one mount, or makes it. A mount already bound
// keeps its id and its placeholder; a new one is resolved the way the manifest
// resolver resolved it, by sec_ id or by a name among the owner's own.
func (c *Controller) bindMount(held []v1.MountedSecret, mount v1.SecretMount, owner string) (v1.MountedSecret, error) {
	for _, record := range held {
		if record.Env == mount.Env && record.Name == mount.Name {
			return record, nil
		}
	}
	record := v1.MountedSecret{Name: mount.Name, Env: mount.Env, Placeholder: egress.MintPlaceholder()}
	if id := c.secretIDLocked(mount.Name, owner); id != "" {
		record.ID = id
		record.Name = c.secretObjects[id].Metadata.Name
	}
	return record, nil
}

// viewOf is one Secret as the compiler reads it, with the value this compile
// decrypted.
func viewOf(secret v1.Secret, placeholder, value string) egress.SecretView {
	view := egress.SecretView{
		Name:        secret.Metadata.Name,
		Kind:        secret.Spec.Kind,
		Placeholder: placeholder,
		Hosts:       slices.Clone(secret.Spec.Scope.Hosts),
		Ports:       slices.Clone(secret.Spec.Scope.Ports),
		Value:       value,
		Inject: egress.Inject{
			Header: secret.Spec.Inject.Header,
			Query:  secret.Spec.Inject.Query,
			Scheme: secret.Spec.Inject.Scheme,
			Body:   secret.Spec.Inject.Body,
		},
	}
	if secret.Spec.OAuth != nil {
		view.OAuth = &egress.OAuth{
			TokenURL: secret.Spec.OAuth.TokenURL,
			Scope:    secret.Spec.OAuth.Scope,
			Audience: secret.Spec.OAuth.Audience,
		}
	}
	return view
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
func (c *Controller) pushEgress(ctx context.Context, obj *v1.Sandbox) (boundary, error) {
	b, err := c.compileEgress(ctx, obj)
	if err != nil {
		return boundary{}, err
	}
	if c.egress == nil {
		if needsGateway(b.Map) {
			return b, ErrNoGateway
		}
		return b, nil
	}
	if err = c.egress.Send(ctx, b.Map); err != nil {
		if needsGateway(b.Map) {
			return b, err
		}
		// Nothing to enforce and nothing to substitute: the sandbox is
		// within its declared boundary with or without a gateway, and the
		// condition says which it got.
		c.log.WarnContext(ctx, "the sandbox's map reached no gateway", "sandbox", obj.Status.ID, "err", err)
		return b, nil
	}
	b.Held = true
	return b, nil
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
func (c *Controller) EgressMaps(ctx context.Context) []egress.Map {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]egress.Map, 0, len(c.objects))
	for _, obj := range c.objects {
		if obj.Status.Phase == "Deleting" {
			continue
		}
		sb := clone(obj)
		views, _, _, err := c.secretViews(ctx, &sb)
		if err == nil {
			var m egress.Map
			m, err = egress.Compile(sb, views)
			if err == nil {
				out = append(out, m)
				continue
			}
		}
		{
			// A stored object the compiler refuses is a defect in what was
			// written, not a reason to hand the gateway a partial world: the
			// sandbox is left out and named, so it is visible in the log
			// rather than silently reachable.
			c.log.ErrorContext(ctx, "the stored boundary of a sandbox does not compile", "sandbox", obj.Status.ID, "err", err)
		}
	}
	slices.SortFunc(out, func(a, b egress.Map) int { return strings.Compare(a.Principal, b.Principal) })
	return out
}
