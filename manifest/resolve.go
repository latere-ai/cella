// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The sentences an environment that confines nothing returns in
// status.warnings instead of refusing a field it cannot honour.
const (
	WarningResourcesNotEnforced = "The native environment does not limit cpu, memory or disk; the requested resources are recorded and not enforced."
	WarningUserNotApplied       = "The native environment runs the workload as the server's own user; spec.user is not applied."
)

// Actor is who is applying: the subject, and whether it is a sandbox acting
// through its workload token.
type Actor struct {
	Subject  string
	Workload bool
}

// Lookup answers the references a manifest names, scoped to the actor. It
// returns ErrNotFound for an object that does not exist and for one the
// authorizer refuses, so existence does not leak, and any other error when it
// cannot decide. The empty environment name asks for the default environment.
// Volume joins this interface with the manifest field that names it.
type Lookup interface {
	Environment(ctx context.Context, name string) (*v1.Environment, error)
	// Secret answers a mount by name among the actor's own or by sec_ id. A
	// Secret comes back without spec.value: a read never returns one.
	Secret(ctx context.Context, nameOrID string) (*v1.Secret, error)
}

// SecretFunc is the secret half of a Lookup, for a caller that composes one
// out of a fixed environment and a store.
type SecretFunc func(ctx context.Context, nameOrID string) (*v1.Secret, error)

// ErrNotFound is a Lookup's answer for an object that does not exist or that
// the actor may not use.
var ErrNotFound = errors.New("object not found")

// Defaults are the values an absent field takes, from the operator's
// configuration. An empty value leaves the field absent.
type Defaults struct {
	CPU, Memory, Disk         v1.Quantity
	AutoStop, TTL, AutoDelete v1.Duration
}

// Ceilings are the values a resolved field may not exceed. An empty value is
// no ceiling.
type Ceilings struct {
	CPU, Memory, Disk v1.Quantity
	TTL               v1.Duration
}

// Limits are what the authorizer granted this actor.
type Limits struct {
	MaxPriority int
}

// AdmitRequest is what an admission step knows beyond the manifest. Claims,
// Workload, Parent, Set and RequestID join it with the specs that carry them.
type AdmitRequest struct {
	Actor       Actor
	Action      string // create or update
	Existing    *v1.Sandbox
	Environment *v1.Environment
}

// AdmitFunc returns the object to continue with, warnings for status.warnings,
// or an error. A nil AdmitFunc is the identity.
type AdmitFunc func(ctx context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error)

// Options are what Resolve needs beyond the manifest: who is applying, what
// the references resolve to, what an absent field takes, what a resolved field
// may not exceed, and the seams an operator supplies.
type Options struct {
	Actor    Actor
	Lookup   Lookup
	Defaults Defaults
	Ceilings Ceilings
	Limits   Limits
	Admit    AdmitFunc
	Existing *v1.Sandbox // the current object on update; nil on create
	// Now is the clock a stage that computes a deadline reads. The spawn
	// boundary check is its first reader.
	Now     func() time.Time
	NewName func() string // the generator for an absent metadata.name
}

// Resolved is a fully defaulted manifest and what the environment could not
// honour.
type Resolved struct {
	Sandbox v1.Sandbox // spec and metadata fully resolved; status empty but warnings
	// Secrets are the objects spec.secrets names, in the manifest's order
	// and without their values, as the actor's own Lookup answered them.
	Secrets  []v1.Secret
	Warnings []string
}

// Resolve turns a decoded manifest into the fully defaulted, validated form
// the data plane is asked for. The stages run in order, each total before the
// next begins: structural validation, defaulting, admission and its
// revalidation, reference resolution, semantic validation, and the capability
// check. It never mutates its input and is deterministic: the same manifest,
// options, and lookup answers produce byte-identical output.
func Resolve(ctx context.Context, in *v1.Sandbox, o Options) (*Resolved, error) {
	if in == nil {
		return nil, errors.New("manifest: Resolve needs a manifest")
	}
	if o.Lookup == nil {
		return nil, errors.New("manifest: Resolve needs an environment lookup")
	}
	obj := clone(in)
	obj.Status = v1.SandboxStatus{} // status is the server's; a caller may send it and it is ignored
	if obj.APIVersion != v1.APIVersion {
		return nil, failAt("unsupported_version", "apiVersion", "This server serves "+v1.APIVersion+".")
	}
	if obj.Kind != "Sandbox" {
		return nil, failAt("unsupported_kind", "kind", "This server does not serve that kind.")
	}
	if err := validate(&obj); err != nil {
		return nil, err
	}
	env, err := defaulting(ctx, &obj, o)
	if err != nil {
		return nil, err
	}
	warnings, err := admit(ctx, &obj, env, o)
	if err != nil {
		return nil, err
	}
	secrets, err := references(ctx, &obj, o)
	if err != nil {
		return nil, err
	}
	if err = semantic(&obj, o); err != nil {
		return nil, err
	}
	admitted, err := capabilities(&obj, env, o)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, admitted...)
	obj.Status.Warnings = warnings
	return &Resolved{Sandbox: obj, Secrets: secrets, Warnings: slices.Clone(warnings)}, nil
}

// defaulting fills every absent field that has a default and resolves the
// environment, which both this stage and the admission step read.
func defaulting(ctx context.Context, obj *v1.Sandbox, o Options) (*v1.Environment, error) {
	if obj.Metadata.Name == "" && o.NewName != nil {
		obj.Metadata.Name = o.NewName()
		if err := validateMetadata(obj.Metadata); err != nil {
			return nil, err
		}
	}
	env, err := lookupEnvironment(ctx, o.Lookup, obj.Spec.Environment)
	if err != nil {
		return nil, err
	}
	obj.Spec.Environment = env.Metadata.Name
	for _, field := range resourceFields(&obj.Spec.Resources) {
		if *field.value == "" {
			*field.value = defaultQuantity(field.path, o.Defaults)
		}
	}
	for _, field := range lifecycleFields(&obj.Spec.Lifecycle) {
		if *field.value == "" {
			*field.value = defaultDuration(field.path, o.Defaults)
		}
	}
	if obj.Spec.Workspace.Path == "" {
		obj.Spec.Workspace.Path = DefaultWorkspacePath
	}
	if obj.Spec.Workspace.Source == "" {
		obj.Spec.Workspace.Source = v1.WorkspaceSourceEmpty
	}
	if obj.Spec.Workdir == "" {
		obj.Spec.Workdir = obj.Spec.Workspace.Path
	}
	inferEgressMode(&obj.Spec.Network, len(obj.Spec.Secrets) > 0)
	if err = validateSpec(obj.Spec); err != nil {
		return nil, errors.New("manifest: this server's defaults are invalid: " + err.Error())
	}
	return env, nil
}

func defaultQuantity(path string, d Defaults) v1.Quantity {
	switch path {
	case "spec.resources.cpu":
		return d.CPU
	case "spec.resources.memory":
		return d.Memory
	default:
		return d.Disk
	}
}

func defaultDuration(path string, d Defaults) v1.Duration {
	switch path {
	case "spec.lifecycle.autoStop":
		return d.AutoStop
	case "spec.lifecycle.ttl":
		return d.TTL
	default:
		return d.AutoDelete
	}
}

// admit runs the operator's admission step over the defaulted object and
// validates what it returns, so a webhook cannot write a manifest the schema
// refuses.
func admit(ctx context.Context, obj *v1.Sandbox, env *v1.Environment, o Options) ([]string, error) {
	if o.Admit == nil {
		return nil, nil
	}
	action := "create"
	if o.Existing != nil {
		action = "update"
	}
	in := clone(obj)
	out, warnings, err := o.Admit(ctx, &in, AdmitRequest{Actor: o.Actor, Action: action, Existing: o.Existing, Environment: env})
	if err != nil {
		return nil, fail("admission_refused", upperFirst(err.Error()))
	}
	if out == nil {
		return nil, fail("admission_refused", "The admission step returned no manifest.")
	}
	next := clone(out)
	next.Status = v1.SandboxStatus{}
	if next.APIVersion != obj.APIVersion || next.Kind != obj.Kind {
		return nil, fail("admission_refused", "The admission step changed the apiVersion or the kind.")
	}
	if o.Existing != nil && next.Metadata.Name != obj.Metadata.Name {
		return nil, fail("admission_refused", "The admission step changed the name of a sandbox that exists.")
	}
	if err = validate(&next); err != nil {
		return nil, err
	}
	*obj = next
	return slices.Clone(warnings), nil
}

// semantic runs the rules that need the defaulted object: the operator's
// ceilings, the authorizer's limits, immutability against the stored object,
// and the lifecycle rule that relates two fields.
func semantic(obj *v1.Sandbox, o Options) error {
	if err := ceilings(obj, o); err != nil {
		return err
	}
	if o.Existing != nil {
		if err := immutable(o.Existing, obj); err != nil {
			return err
		}
		// The narrow fields are the boundary. Any caller may move one
		// inward; only a caller that is not the sandbox itself may move
		// one outward, which is invariant 9 of the architecture.
		if o.Actor.Workload {
			if err := narrowing(o.Existing, obj); err != nil {
				return err
			}
		}
	}
	return autoStopWithinTTL(obj.Spec.Lifecycle)
}

func ceilings(obj *v1.Sandbox, o Options) error {
	for _, c := range []struct {
		path           string
		value, ceiling v1.Quantity
	}{
		{"spec.resources.cpu", obj.Spec.Resources.CPU, o.Ceilings.CPU},
		{"spec.resources.memory", obj.Spec.Resources.Memory, o.Ceilings.Memory},
		{"spec.resources.disk", obj.Spec.Resources.Disk, o.Ceilings.Disk},
	} {
		if c.value == "" || c.ceiling == "" {
			continue
		}
		limit, err := ParseQuantity(c.ceiling)
		if err != nil {
			return errors.New("manifest: this server's ceiling for " + c.path + " is invalid: " + err.Error())
		}
		value, err := ParseQuantity(c.value)
		if err != nil {
			return failAt("invalid_field", c.path, upperFirst(err.Error())+".")
		}
		if value > limit {
			return failAt("ceiling_exceeded", c.path, fmt.Sprintf("The request of %s is above this server's ceiling of %s.", c.value, c.ceiling))
		}
	}
	if err := ttlCeiling(obj.Spec.Lifecycle.TTL, o.Ceilings.TTL); err != nil {
		return err
	}
	// scheduling.priority arrives with the scheduler, so the only priority a
	// manifest expresses today is zero. A negative ceiling is one it cannot
	// meet, and a limit that cannot be met is refused rather than ignored.
	if o.Limits.MaxPriority < 0 {
		return failAt("ceiling_exceeded", "spec.scheduling.priority", "The priority of 0 is above the limit this caller was granted.")
	}
	return nil
}

func ttlCeiling(ttl, ceiling v1.Duration) error {
	if ttl == "" || ceiling == "" {
		return nil
	}
	limit, unbounded, err := ParseDuration(ceiling)
	if err != nil {
		return errors.New("manifest: this server's ttl ceiling is invalid: " + err.Error())
	}
	if unbounded {
		return nil
	}
	value, never, err := ParseDuration(ttl)
	if err != nil {
		return failAt("invalid_field", "spec.lifecycle.ttl", upperFirst(err.Error())+".")
	}
	if never || value > limit {
		return failAt("ceiling_exceeded", "spec.lifecycle.ttl", fmt.Sprintf("The time to live of %s is above this server's ceiling of %s.", ttl, ceiling))
	}
	return nil
}

// immutable names every field the contract marks unchangeable that an update
// changed, in one error, in the order of the field table.
func immutable(existing, obj *v1.Sandbox) error {
	var paths []string
	for _, f := range []struct {
		path    string
		changed bool
	}{
		{"metadata.name", existing.Metadata.Name != obj.Metadata.Name},
		{"spec.environment", existing.Spec.Environment != obj.Spec.Environment},
		{"spec.image", existing.Spec.Image != obj.Spec.Image},
		{"spec.command", !slices.Equal(existing.Spec.Command, obj.Spec.Command)},
		{"spec.args", !slices.Equal(existing.Spec.Args, obj.Spec.Args)},
		{"spec.workdir", existing.Spec.Workdir != obj.Spec.Workdir},
		{"spec.user", existing.Spec.User != obj.Spec.User},
		{"spec.workspace.path", existing.Spec.Workspace.Path != obj.Spec.Workspace.Path},
		{"spec.workspace.source", existing.Spec.Workspace.Source != obj.Spec.Workspace.Source},
		{"spec.env", !maps.Equal(existing.Spec.Env, obj.Spec.Env)},
	} {
		if f.changed {
			paths = append(paths, f.path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return failPaths("immutable_field", "These fields cannot be changed after the sandbox is created: "+strings.Join(paths, ", ")+".", paths)
}

// autoStopWithinTTL holds an idle stop inside the sandbox's life. The rule
// compares two durations: autoStop never under a duration ttl is accepted,
// because the ttl already ends the sandbox and an idle stop before it is what
// the caller declined.
func autoStopWithinTTL(l v1.Lifecycle) error {
	if l.AutoStop == "" || l.TTL == "" {
		return nil
	}
	stop, stopNever, err := ParseDuration(l.AutoStop)
	if err != nil {
		return failAt("invalid_field", "spec.lifecycle.autoStop", upperFirst(err.Error())+".")
	}
	ttl, ttlNever, err := ParseDuration(l.TTL)
	if err != nil {
		return failAt("invalid_field", "spec.lifecycle.ttl", upperFirst(err.Error())+".")
	}
	if stopNever || ttlNever || stop <= ttl {
		return nil
	}
	return failAt("invalid_field", "spec.lifecycle.autoStop", fmt.Sprintf("The idle stop of %s is later than the time to live of %s.", l.AutoStop, l.TTL))
}

// capabilities holds the manifest to what the environment can serve, and
// returns a warning where the environment records a field instead of enforcing
// it.
func capabilities(obj *v1.Sandbox, env *v1.Environment, o Options) ([]string, error) {
	if obj.Spec.Workspace.Source != v1.WorkspaceSourceEmpty {
		return nil, failAt("capability_unsupported", "spec.workspace.source", "This server fills a workspace from "+v1.WorkspaceSourceEmpty+" only.")
	}
	if o.Existing != nil && obj.Spec.Resources != o.Existing.Spec.Resources && !env.Status.Capabilities.Resize {
		return nil, failAt("capability_unsupported", "spec.resources", "This environment cannot change the resources of a sandbox that exists.")
	}
	warnings, err := egressCapability(obj, env)
	if err != nil {
		return nil, err
	}
	// An environment that confines nothing runs the workload as the server's
	// own process: it has no cgroup to size and no user to switch to. The
	// fields are recorded so one manifest stays valid across environments.
	if env.Status.Isolation != v1.IsolationNone {
		return warnings, nil
	}
	if obj.Spec.Resources != (v1.Resources{}) {
		warnings = append(warnings, WarningResourcesNotEnforced)
	}
	if obj.Spec.User != "" {
		warnings = append(warnings, WarningUserNotApplied)
	}
	return warnings, nil
}

func lookupEnvironment(ctx context.Context, lookup Lookup, name string) (*v1.Environment, error) {
	env, err := lookup.Environment(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, failAt("not_found", "spec.environment", "There is no such environment.")
	case err != nil:
		var known *Error
		if errors.As(err, &known) {
			return nil, known
		}
		return nil, failAt("authorizer_unavailable", "spec.environment", "The permission service is unavailable; retry shortly.")
	case env == nil:
		return nil, failAt("not_found", "spec.environment", "There is no such environment.")
	}
	return env, nil
}

// FixedEnvironment answers one environment, by its name and by the empty name,
// which asks for the default environment. A server that serves the Environment
// kind answers from its store instead. It knows no secret; WithSecrets gives
// it the half that reads them.
func FixedEnvironment(env v1.Environment) Lookup { return fixedEnvironment{env: env} }

// WithSecrets is the lookup l with its secret half answered by f. A server
// composes its own per-request lookup this way: the environment is fixed and
// the secrets come from the store through the authorizer's mount decision.
func WithSecrets(l Lookup, f SecretFunc) Lookup { return secretLookup{Lookup: l, secrets: f} }

type secretLookup struct {
	Lookup
	secrets SecretFunc
}

func (s secretLookup) Secret(ctx context.Context, nameOrID string) (*v1.Secret, error) {
	if s.secrets == nil {
		return nil, ErrNotFound
	}
	return s.secrets(ctx, nameOrID)
}

type fixedEnvironment struct{ env v1.Environment }

func (f fixedEnvironment) Environment(_ context.Context, name string) (*v1.Environment, error) {
	if name != "" && name != f.env.Metadata.Name {
		return nil, ErrNotFound
	}
	env := f.env
	return &env, nil
}

// Secret answers nothing: a fixed environment is the whole of what this
// lookup knows, so a manifest that mounts a secret against it is not_found.
func (f fixedEnvironment) Secret(context.Context, string) (*v1.Secret, error) {
	return nil, ErrNotFound
}

// NativeEnvironment describes the in-process environment the native driver
// serves. It confines nothing, so its isolation class is none and it declares
// no capability of its own.
func NativeEnvironment(name string) v1.Environment {
	return v1.Environment{
		APIVersion: v1.APIVersion,
		Kind:       "Environment",
		Metadata:   v1.Metadata{Name: name},
		Spec:       v1.EnvironmentSpec{Isolation: v1.IsolationNone},
		Status:     v1.EnvironmentStatus{Driver: "native", Isolation: v1.IsolationNone},
	}
}

// ResolveNative resolves a manifest against the one native environment, with
// no operator defaults and no ceilings, and adds the refusals that environment
// owns: it runs no image and owns the workspace directory.
func ResolveNative(ctx context.Context, obj v1.Sandbox, environment string) (v1.Sandbox, error) {
	out, _, err := ResolveNativeWith(ctx, obj, NativeOptions(environment, nil))
	return out, err
}

// NativeOptions are ResolveNative's options, with the secret half of the
// lookup supplied by the caller. A server passes its own, so a mount reaches
// the store through the authorizer's decision; a caller with no secrets
// passes nil and every mount is not_found.
func NativeOptions(environment string, secrets SecretFunc) Options {
	return Options{Lookup: WithSecrets(FixedEnvironment(NativeEnvironment(environment)), secrets)}
}

// ResolveNativeWith is ResolveNative over caller-supplied options. It returns
// the secrets the manifest mounts beside the resolved manifest, so the
// controller binds to the objects the resolver already decided on.
func ResolveNativeWith(ctx context.Context, obj v1.Sandbox, o Options) (v1.Sandbox, []v1.Secret, error) {
	resolved, err := Resolve(ctx, &obj, o)
	if err != nil {
		return obj, nil, err
	}
	out := resolved.Sandbox
	if out.Spec.Image != "" {
		return obj, nil, failAt("capability_unsupported", "spec.image", "Native environments do not run images.")
	}
	if len(out.Spec.Command) == 0 && len(out.Spec.Args) > 0 {
		return obj, nil, failAt("invalid_field", "spec.args", "Arguments need a command on a native environment.")
	}
	if len(out.Spec.Command) > 0 {
		if err = ValidateExec(append(slices.Clone(out.Spec.Command), out.Spec.Args...), nil, ""); err != nil {
			return obj, nil, err
		}
	}
	if out.Spec.Workspace.Path != DefaultWorkspacePath || out.Spec.Workdir != DefaultWorkspacePath {
		return obj, nil, failAt("capability_unsupported", "spec.workspace.path", "Native environments keep the workspace at "+DefaultWorkspacePath+" and start there.")
	}
	return out, resolved.Secrets, nil
}

func clone(obj *v1.Sandbox) v1.Sandbox {
	out := *obj
	out.Metadata.Labels = maps.Clone(obj.Metadata.Labels)
	out.Metadata.Annotations = maps.Clone(obj.Metadata.Annotations)
	out.Spec.Command = slices.Clone(obj.Spec.Command)
	out.Spec.Args = slices.Clone(obj.Spec.Args)
	out.Spec.Env = maps.Clone(obj.Spec.Env)
	out.Spec.Network.Egress.AllowedHosts = slices.Clone(obj.Spec.Network.Egress.AllowedHosts)
	out.Spec.Network.Egress.DeniedHosts = slices.Clone(obj.Spec.Network.Egress.DeniedHosts)
	out.Spec.Secrets = slices.Clone(obj.Spec.Secrets)
	out.Status.Conditions = slices.Clone(obj.Status.Conditions)
	out.Status.Secrets.Mounted = slices.Clone(obj.Status.Secrets.Mounted)
	out.Status.Secrets.NotInjectable = slices.Clone(obj.Status.Secrets.NotInjectable)
	out.Status.Warnings = slices.Clone(obj.Status.Warnings)
	if obj.Status.EgressState != nil {
		state := *obj.Status.EgressState
		state.Secrets = slices.Clone(obj.Status.EgressState.Secrets)
		out.Status.EgressState = &state
	}
	if obj.Status.ExitCode != nil {
		code := *obj.Status.ExitCode
		out.Status.ExitCode = &code
	}
	return out
}
