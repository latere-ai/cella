// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// ErrNoGateway is a create or an apply whose boundary needs a gateway and
// found none connected, or none that acknowledged its map. The API answers it
// with a code of its own, since the remedy is a gateway or a wider boundary
// and not waiting for the environment.
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
	// TLS with, in PEM, or empty when none has connected. The driver writes
	// it into the sandbox's trust file after the public roots, so the
	// workload verifies that door and every host the door tunnels.
	CA() string
	// Connected is how many gateways of the environment hold a stream open,
	// which the phase loop writes into status.gateways (spec 021).
	Connected() int
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

// envDigest is the digest EgressState.EnvDigest holds: of the environment
// itself, so two environments that say the same thing have one digest
// however they were built.
func envDigest(env map[string]string) string {
	sum := sha256.New()
	for _, key := range slices.Sorted(maps.Keys(env)) {
		// Each name and value goes in with its length ahead of it, so no
		// two environments write the same bytes. A hash takes every write.
		_, _ = fmt.Fprintf(sum, "%d:%s%d:%s", len(key), key, len(env[key]), env[key])
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// handedEnv records on a sandbox the environment its driver was handed at a
// create, with the secret environment a compile produced.
func handedEnv(obj *v1.Sandbox, secrets map[string]string) {
	if obj.Status.EgressState != nil {
		obj.Status.EgressState.EnvDigest = envDigest(withSecretEnv(obj.Spec.Env, secrets))
	}
}

// carryEnv hands the driver a sandbox's environment when it is not the one
// the driver last took: a secret its owner mounted or dropped by an update,
// a secret whose inject moved to another header or parameter, or a hand-off
// that did not reach the driver before. A running process keeps the
// environment it started with, so the driver records the new one and the
// workload has it from its next start. A sandbox no driver holds now gets it
// from the create or the start that brings it back, which compile the
// boundary again. On success obj records what the driver took; the caller
// holds the lock and writes obj.
func (c *Controller) carryEnv(ctx context.Context, obj *v1.Sandbox, secrets map[string]string) error {
	state := obj.Status.EgressState
	env := maps.Clone(withSecretEnv(obj.Spec.Env, secrets))
	if env == nil {
		env = map[string]string{}
	}
	digest := envDigest(env)
	if state == nil || state.EnvDigest == digest {
		return nil
	}
	switch obj.Status.Phase {
	case PhaseQueued, PhaseLost, PhaseRecovering, PhaseDeleting:
		return nil
	}
	if placed(*obj) {
		return nil
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	if err := d.Update(ctx, obj.Status.ID, driver.Change{Env: &env}); err != nil {
		if errors.Is(err, driver.ErrNotFound) {
			return nil
		}
		return err
	}
	state.EnvDigest = digest
	return nil
}

// carryBoundary hands an update's boundary to a driver that confines the
// workload with its own proxy (EgressLocal). No gateway decides where such a
// sandbox connects, so a driver not handed the boundary goes on enforcing
// the one the sandbox was created with while its status names the new one.
// A sandbox no driver holds now gets it from the create or the start that
// brings it back, which compile the boundary again. The caller holds the
// lock.
func (c *Controller) carryBoundary(ctx context.Context, obj v1.Sandbox, m egress.Map) error {
	if !c.CapabilitiesOf(obj.Status.Environment).EgressLocal {
		return nil
	}
	switch obj.Status.Phase {
	case PhaseQueued, PhaseLost, PhaseRecovering, PhaseDeleting:
		return nil
	}
	if placed(obj) {
		return nil
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return err
	}
	spec := c.egressSpec(obj.Status.Environment, m)
	if err := d.Update(ctx, obj.Status.ID, driver.Change{Egress: &spec}); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return err
	}
	return nil
}

// restoreEgress puts back the boundary a sandbox had before an update that
// failed after its map was compiled, so every map a gateway holds is one
// desired state has, and answers the update's error with the restore's. A
// gateway takes no map numbered at or below one it holds, so the one put
// back is numbered past the one that failed, and the object held keeps that
// number for the compile after it. The caller holds the lock.
func (c *Controller) restoreEgress(ctx context.Context, held, failed v1.Sandbox, err error) error {
	previous := clone(held)
	if previous.Status.EgressState != nil && failed.Status.EgressState != nil {
		previous.Status.EgressState.Version = max(previous.Status.EgressState.Version, failed.Status.EgressState.Version)
	}
	_, restore := c.pushEgress(ctx, &previous)
	if held.Status.EgressState != nil && previous.Status.EgressState != nil {
		held.Status.EgressState.Version = previous.Status.EgressState.Version
	}
	if restore != nil {
		return errors.Join(err, restore)
	}
	return err
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
		if boundTo(record, mount) {
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

// boundTo reports whether a record is the binding of one mount: the same
// environment key, and the secret named the way the mount names it. A mount
// by sec_ id is recorded under the secret's own name, so its id is what
// matches it again.
func boundTo(record v1.MountedSecret, mount v1.SecretMount) bool {
	return record.Env == mount.Env && (record.Name == mount.Name || (record.ID != "" && record.ID == mount.Name))
}

// viewOf is one Secret as the compiler reads it, with the value this compile
// decrypted.
func viewOf(secret v1.Secret, placeholder, value string) egress.SecretView {
	view := egress.SecretView{
		ID:          secret.Status.ID,
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

// needsGatewayFor is needsGateway read from a manifest rather than a
// compiled map: an egress mode other than open, a denied host, or a mounted
// secret is something a gateway must enforce or substitute. It is what a
// create is refused on before it writes anything, when no gateway is
// connected to hold the map.
func needsGatewayFor(spec v1.SandboxSpec) bool {
	e := spec.Network.Egress
	return e.Mode != v1.EgressOpen || len(e.DeniedHosts) > 0 || len(spec.Secrets) > 0
}

// gatewayRetry is how often a request inside the gateway grace looks again for
// a connected gateway, and how soon the scheduler takes again a sandbox it
// kept placed for want of one.
const gatewayRetry = 100 * time.Millisecond

// sandboxEnvironment is the environment a sandbox runs on, or will: the one its
// status names once placed, the one its manifest names before, and the
// control plane's own where it names none.
func (c *Controller) sandboxEnvironment(obj v1.Sandbox) string {
	return cmp.Or(obj.Status.Environment, obj.Spec.Environment, c.environment)
}

// driverEnforced reports whether a boundary on one environment is held by
// its driver alone: an environment other than the control plane's own whose
// driver declares EgressLocal, no mounted secret, and a mode of none or an
// allow list with no denied host, or open where the driver declares open,
// which a host that admits any public host but those it blocks does. Such a
// boundary needs no gateway, and none is projected into it (spec 083).
func (c *Controller) driverEnforced(environment string, spec v1.SandboxSpec) bool {
	caps := c.CapabilitiesOf(environment)
	if environment == c.environment || !caps.EgressLocal || len(spec.Secrets) > 0 {
		return false
	}
	return localHolds(caps, spec.Network.Egress.Mode, len(spec.Network.Egress.DeniedHosts))
}

// localHolds reports whether a driver that declares EgressLocal holds a
// boundary of this mode and this many denied hosts with its own proxy: none
// and an allow list, which deny nothing, and open, denying any hosts, where
// the driver declares open.
func localHolds(caps driver.Capabilities, mode v1.EgressMode, denied int) bool {
	switch mode {
	case v1.EgressNone, v1.EgressAllowlist:
		return denied == 0
	case v1.EgressOpen:
		return slices.Contains(caps.Egress, v1.EgressOpen)
	}
	return false
}

// gatewayHolds reports whether a gateway can hold a boundary needing one on
// one environment: the hub serves the control plane's own environment alone
// (spec 021), so a sandbox on any other gets no gateway, connected or not.
func (c *Controller) gatewayHolds(environment string) bool {
	return environment == c.environment && c.egress != nil && c.egress.Connected() > 0
}

// needsGatewayOn is needsGatewayFor on one environment: a boundary its
// driver enforces alone needs none.
func (c *Controller) needsGatewayOn(environment string, spec v1.SandboxSpec) bool {
	return needsGatewayFor(spec) && !c.driverEnforced(environment, spec)
}

// awaitGateway holds a create, a spawn or an apply whose boundary needs a
// gateway while none is connected, for what is left of the grace since this
// controller opened, or until the caller leaves. It runs before the
// controller's lock is taken, so a wait holds no other request. What follows
// decides as it always did: a gateway that connected is used, and none at the
// end of the grace is ErrNoGateway.
func (c *Controller) awaitGateway(ctx context.Context, obj v1.Sandbox) {
	// A gateway serves the control plane's own environment alone, so a
	// boundary on any other waits for none.
	if c.egress == nil || c.gatewayGrace <= 0 || !needsGatewayFor(obj.Spec) || c.sandboxEnvironment(obj) != c.environment {
		return
	}
	until := c.opened.Add(c.gatewayGrace)
	for c.egress.Connected() == 0 {
		left := time.Until(until)
		if left <= 0 {
			return
		}
		timer := time.NewTimer(min(left, gatewayRetry))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// gatewayGraceHolds reports whether a push that found no gateway is inside the
// grace after Open with none connected, which the scheduler waits out rather
// than failing the create.
func (c *Controller) gatewayGraceHolds(err error) bool {
	return errors.Is(err, ErrNoGateway) && c.egress != nil && c.egress.Connected() == 0 &&
		c.gatewayGrace > 0 && time.Since(c.opened) < c.gatewayGrace
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
	// The gateway serves the control plane's own environment alone. A
	// sandbox on any other is held by its driver or by nothing, and its
	// map goes to no gateway.
	if environment := c.sandboxEnvironment(*obj); environment != c.environment {
		if needsGateway(b.Map) && !c.driverEnforced(environment, obj.Spec) {
			return b, ErrNoGateway
		}
		return b, nil
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
// credential, and the authority that signs the proxy door's leaves. A
// sandbox on an environment other than the control plane's own gets the
// boundary alone: no gateway serves it, so no door, credential or authority
// of one is projected (spec 083).
func (c *Controller) egressSpec(environment string, m egress.Map) driver.Egress {
	if environment != c.environment {
		return driver.Egress{Mode: string(m.Mode), AllowedHosts: slices.Clone(m.Allow), DeniedHosts: slices.Clone(m.Deny)}
	}
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
func (c *Controller) egressCondition(environment string, m egress.Map, held bool, now time.Time) v1.Condition {
	cond := v1.Condition{Type: v1.ConditionEgressEnforced, Status: v1.ConditionTrue, Reason: v1.ReasonEnforced, Since: now}
	caps := c.CapabilitiesOf(environment)
	switch {
	case !slices.Contains(caps.Egress, m.Mode):
		cond.Status = v1.ConditionFalse
		cond.Reason = v1.ReasonNotEnforcedByDriver
		cond.Message = "This environment records the boundary and does not confine the workload to it."
	case environment != c.environment && caps.EgressLocal && localHolds(caps, m.Mode, len(m.Deny)) &&
		len(m.Entries) == 0 && len(m.NotInjectable) == 0:
		// The driver's own proxy is the only route out, which stands in
		// for a gateway (spec 083).
		cond.Reason = v1.ReasonDriverEnforced
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
		// The gateway serves the control plane's own environment alone,
		// and a sandbox on any other is none of its business.
		if obj.Status.Phase == "Deleting" || c.sandboxEnvironment(obj) != c.environment {
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
