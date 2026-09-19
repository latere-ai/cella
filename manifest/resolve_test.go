// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

func sandbox() v1.Sandbox {
	return v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "work"}}
}

// container is an environment that confines a workload and can resize it;
// native is the in-process one, which confines nothing.
func container(name string) v1.Environment {
	env := NativeEnvironment(name)
	env.Spec.Isolation = v1.IsolationContainer
	env.Status = v1.EnvironmentStatus{Driver: "k8s", Isolation: v1.IsolationContainer, Capabilities: v1.Capabilities{Resize: true}}
	return env
}

func containerOptions() Options { return Options{Lookup: FixedEnvironment(container("default"))} }
func nativeOptions() Options {
	return Options{Lookup: FixedEnvironment(NativeEnvironment("default"))}
}

func resolve(t *testing.T, obj v1.Sandbox, o Options) *Resolved {
	t.Helper()
	got, err := Resolve(t.Context(), &obj, o)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

// refusal resolves a manifest that must fail and returns the contract error.
func refusal(t *testing.T, obj v1.Sandbox, o Options) *Error {
	t.Helper()
	_, err := Resolve(t.Context(), &obj, o)
	var known *Error
	if !errors.As(err, &known) {
		t.Fatalf("Resolve: got %v, want a manifest error", err)
	}
	return known
}

func TestDefaultsFillOnlyAbsentFields(t *testing.T) {
	o := containerOptions()
	o.Defaults = Defaults{CPU: "1", Memory: "2Gi", Disk: "10Gi", AutoStop: "15m", TTL: "24h", AutoDelete: "72h"}
	obj := sandbox()
	obj.Spec.Resources.Memory = "8Gi"
	obj.Spec.Lifecycle.TTL = "1h"
	got := resolve(t, obj, o).Sandbox
	want := v1.SandboxSpec{
		Environment: "default",
		Workdir:     DefaultWorkspacePath,
		Resources:   v1.Resources{CPU: "1", Memory: "8Gi", Disk: "10Gi"},
		Workspace:   v1.Workspace{Path: DefaultWorkspacePath, Source: v1.WorkspaceSourceEmpty},
		Lifecycle:   v1.Lifecycle{AutoStop: "15m", TTL: "1h", AutoDelete: "72h"},
	}
	if got.Spec.Environment != want.Environment || got.Spec.Workdir != want.Workdir || got.Spec.Resources != want.Resources || got.Spec.Workspace != want.Workspace || got.Spec.Lifecycle != want.Lifecycle {
		t.Fatalf("spec = %+v, want %+v", got.Spec, want)
	}
	// A caller's workspace path moves the default working directory with it.
	obj = sandbox()
	obj.Spec.Workspace.Path = "/srv/work"
	if got = resolve(t, obj, containerOptions()).Sandbox; got.Spec.Workdir != "/srv/work" {
		t.Fatalf("workdir = %q, want the workspace path", got.Spec.Workdir)
	}
	// An operator default that the schema refuses is the server's fault, not
	// the caller's, so it is not a contract error.
	o.Defaults.CPU = "twelve"
	if _, err := Resolve(t.Context(), &v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox"}, o); err == nil {
		t.Fatal("an invalid default was accepted")
	} else if known := (*Error)(nil); errors.As(err, &known) {
		t.Fatalf("an invalid default surfaced as a contract error: %v", err)
	}
}

func TestMetadataRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   v1.Metadata
		code string
	}{
		{"reserved label", v1.Metadata{Labels: map[string]string{"cella.latere.ai/owner": "evil"}}, "reserved_prefix"},
		{"reserved label subdomain", v1.Metadata{Labels: map[string]string{"pool.cella.latere.ai/owner": "evil"}}, "reserved_prefix"},
		{"reserved annotation", v1.Metadata{Annotations: map[string]string{"cella.latere.ai/note": "x"}}, "reserved_prefix"},
		{"key syntax", v1.Metadata{Labels: map[string]string{"bad key": "v"}}, "invalid_field"},
		{"key with two slashes", v1.Metadata{Labels: map[string]string{"a/b/c": "v"}}, "invalid_field"},
		{"key domain syntax", v1.Metadata{Labels: map[string]string{"BAD.org/key": "v"}}, "invalid_field"},
		{"key domain too long", v1.Metadata{Labels: map[string]string{strings.Repeat("a", 254) + "/key": "v"}}, "invalid_field"},
		{"key label too long", v1.Metadata{Labels: map[string]string{strings.Repeat("a", 64) + ".org/key": "v"}}, "invalid_field"},
		{"label value syntax", v1.Metadata{Labels: map[string]string{"team": "bad value"}}, "invalid_field"},
		{"label value too long", v1.Metadata{Labels: map[string]string{"team": strings.Repeat("a", 64)}}, "invalid_field"},
		{"annotation value too long", v1.Metadata{Annotations: map[string]string{"example.org/note": strings.Repeat("v", 4097)}}, "invalid_field"},
		{"annotations too large", v1.Metadata{Annotations: map[string]string{"a": strings.Repeat("v", 4096), "b": strings.Repeat("v", 4096), "c": strings.Repeat("v", 4096), "d": strings.Repeat("v", 4096), "e": strings.Repeat("v", 4096), "f": strings.Repeat("v", 4096), "g": strings.Repeat("v", 4096), "h": strings.Repeat("v", 4096), "i": strings.Repeat("v", 4096), "j": strings.Repeat("v", 4096), "k": strings.Repeat("v", 4096), "l": strings.Repeat("v", 4096), "m": strings.Repeat("v", 4096), "n": strings.Repeat("v", 4096), "o": strings.Repeat("v", 4096), "p": strings.Repeat("v", 4096), "q": strings.Repeat("v", 4096)}}, "invalid_field"},
		{"name syntax", v1.Metadata{Name: "INVALID"}, "invalid_field"},
		{"name too long", v1.Metadata{Name: strings.Repeat("a", 64)}, "invalid_field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			if tc.md.Name != "" {
				obj.Metadata.Name = tc.md.Name
			}
			obj.Metadata.Labels, obj.Metadata.Annotations = tc.md.Labels, tc.md.Annotations
			if got := refusal(t, obj, containerOptions()); got.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", got.Code, tc.code, got)
			}
		})
	}
	obj := sandbox()
	obj.Metadata.Labels = map[string]string{"example.org/team": "research", "empty": ""}
	obj.Metadata.Annotations = map[string]string{"example.org/note": "any value", "cella.latere.ai.example.org/note": "not the reserved domain"}
	resolve(t, obj, containerOptions())
}

func TestFieldSyntax(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*v1.Sandbox)
		code string
		path string
	}{
		{"version", func(o *v1.Sandbox) { o.APIVersion = "cella.latere.ai/v1" }, "unsupported_version", "apiVersion"},
		{"kind", func(o *v1.Sandbox) { o.Kind = "Secret" }, "unsupported_kind", "kind"},
		{"environment name", func(o *v1.Sandbox) { o.Spec.Environment = "Not A Name" }, "invalid_field", "spec.environment"},
		{"command NUL", func(o *v1.Sandbox) { o.Spec.Command = []string{"sh", "\x00"} }, "invalid_field", "spec.command[1]"},
		{"args NUL", func(o *v1.Sandbox) { o.Spec.Command, o.Spec.Args = []string{"sh"}, []string{"\x00"} }, "invalid_field", "spec.args[0]"},
		{"empty command", func(o *v1.Sandbox) { o.Spec.Command = []string{""} }, "invalid_field", "spec.command[0]"},
		{"workdir relative", func(o *v1.Sandbox) { o.Spec.Workdir = "work" }, "invalid_field", "spec.workdir"},
		{"workdir unclean", func(o *v1.Sandbox) { o.Spec.Workdir = "/workspace/../etc" }, "invalid_field", "spec.workdir"},
		{"user name", func(o *v1.Sandbox) { o.Spec.User = "Not A User" }, "invalid_field", "spec.user"},
		{"user gid", func(o *v1.Sandbox) { o.Spec.User = "1000:root" }, "invalid_field", "spec.user"},
		{"user uid range", func(o *v1.Sandbox) { o.Spec.User = "99999999999" }, "invalid_field", "spec.user"},
		{"user too long", func(o *v1.Sandbox) { o.Spec.User = strings.Repeat("u", 33) }, "invalid_field", "spec.user"},
		{"cpu quantity", func(o *v1.Sandbox) { o.Spec.Resources.CPU = "one" }, "invalid_field", "spec.resources.cpu"},
		{"memory quantity", func(o *v1.Sandbox) { o.Spec.Resources.Memory = "2GB" }, "invalid_field", "spec.resources.memory"},
		{"disk not positive", func(o *v1.Sandbox) { o.Spec.Resources.Disk = "0" }, "invalid_field", "spec.resources.disk"},
		{"negative cpu", func(o *v1.Sandbox) { o.Spec.Resources.CPU = "-1" }, "invalid_field", "spec.resources.cpu"},
		{"workspace path relative", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "workspace" }, "invalid_field", "spec.workspace.path"},
		{"workspace path root", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/" }, "invalid_field", "spec.workspace.path"},
		{"workspace path reserved", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/run/cella/token" }, "invalid_field", "spec.workspace.path"},
		{"workspace path is the reserved mount", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/run/cella" }, "invalid_field", "spec.workspace.path"},
		{"workspace source", func(o *v1.Sandbox) { o.Spec.Workspace.Source = "ftp" }, "invalid_field", "spec.workspace.source"},
		{"workspace source git", func(o *v1.Sandbox) { o.Spec.Workspace.Source = "git" }, "capability_unsupported", "spec.workspace.source"},
		{"workspace source volume", func(o *v1.Sandbox) { o.Spec.Workspace.Source = "volume" }, "capability_unsupported", "spec.workspace.source"},
		{"autoStop syntax", func(o *v1.Sandbox) { o.Spec.Lifecycle.AutoStop = "later" }, "invalid_field", "spec.lifecycle.autoStop"},
		{"ttl syntax", func(o *v1.Sandbox) { o.Spec.Lifecycle.TTL = "soon" }, "invalid_field", "spec.lifecycle.ttl"},
		{"autoDelete zero", func(o *v1.Sandbox) { o.Spec.Lifecycle.AutoDelete = "0s" }, "invalid_field", "spec.lifecycle.autoDelete"},
		{"env key", func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"bad-key": "v"} }, "invalid_field", "spec.env.bad-key"},
		{"env NUL", func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"A": "\x00"} }, "invalid_field", "spec.env.A"},
		{"env reserved", func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"CELLA_OWNER": "v"} }, "reserved_prefix", "spec.env.CELLA_OWNER"},
		{"env too large", func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"A": strings.Repeat("a", 32769)} }, "invalid_field", "spec.env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			tc.mut(&obj)
			got := refusal(t, obj, containerOptions())
			if got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("got %q at %q, want %q at %q", got.Code, got.Path, tc.code, tc.path)
			}
		})
	}
	// The forms the table admits.
	for _, mut := range []func(*v1.Sandbox){
		func(o *v1.Sandbox) { o.Spec.User = "1000" },
		func(o *v1.Sandbox) { o.Spec.User = "1000:1000" },
		func(o *v1.Sandbox) { o.Spec.User = "app_runner" },
		func(o *v1.Sandbox) { o.Spec.Resources = v1.Resources{CPU: "500m", Memory: "2Gi", Disk: "10Gi"} },
		func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/srv/workspace" },
		func(o *v1.Sandbox) { o.Spec.Workspace.Source = v1.WorkspaceSourceEmpty },
		func(o *v1.Sandbox) {
			o.Spec.Lifecycle = v1.Lifecycle{AutoStop: "15m", TTL: "24h", AutoDelete: "72h"}
		},
		func(o *v1.Sandbox) {
			o.Spec.Lifecycle = v1.Lifecycle{AutoStop: v1.DurationNever, TTL: v1.DurationNever, AutoDelete: v1.DurationNever}
		},
	} {
		obj := sandbox()
		mut(&obj)
		resolve(t, obj, containerOptions())
	}
}

func TestAutoStopAgainstTTL(t *testing.T) {
	obj := sandbox()
	obj.Spec.Lifecycle = v1.Lifecycle{AutoStop: "2h", TTL: "1h"}
	got := refusal(t, obj, containerOptions())
	if got.Code != "invalid_field" || got.Path != "spec.lifecycle.autoStop" {
		t.Fatalf("got %q at %q", got.Code, got.Path)
	}
	// never declines the idle stop; the ttl still ends the sandbox.
	obj.Spec.Lifecycle = v1.Lifecycle{AutoStop: v1.DurationNever, TTL: "1h"}
	resolve(t, obj, containerOptions())
	obj.Spec.Lifecycle = v1.Lifecycle{AutoStop: "1h", TTL: v1.DurationNever}
	resolve(t, obj, containerOptions())
	obj.Spec.Lifecycle = v1.Lifecycle{AutoStop: "1h", TTL: "1h"}
	resolve(t, obj, containerOptions())
	// Validation runs after defaulting, so an operator's idle stop above a
	// caller's shorter life is refused rather than silently applied.
	o := containerOptions()
	o.Defaults.AutoStop = "15m"
	obj = sandbox()
	obj.Spec.Lifecycle.TTL = "5m"
	if got = refusal(t, obj, o); got.Code != "invalid_field" {
		t.Fatalf("code = %q, want invalid_field", got.Code)
	}
}

func TestCeilings(t *testing.T) {
	o := containerOptions()
	o.Ceilings = Ceilings{CPU: "4", Memory: "8Gi", Disk: "20Gi", TTL: "24h"}
	for _, tc := range []struct {
		name string
		mut  func(*v1.Sandbox)
		path string
	}{
		{"cpu", func(s *v1.Sandbox) { s.Spec.Resources.CPU = "8" }, "spec.resources.cpu"},
		{"memory", func(s *v1.Sandbox) { s.Spec.Resources.Memory = "16Gi" }, "spec.resources.memory"},
		{"disk", func(s *v1.Sandbox) { s.Spec.Resources.Disk = "21Gi" }, "spec.resources.disk"},
		{"ttl", func(s *v1.Sandbox) { s.Spec.Lifecycle.TTL = "48h" }, "spec.lifecycle.ttl"},
		{"ttl never", func(s *v1.Sandbox) { s.Spec.Lifecycle.TTL = v1.DurationNever }, "spec.lifecycle.ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			tc.mut(&obj)
			got := refusal(t, obj, o)
			if got.Code != "ceiling_exceeded" || got.Path != tc.path {
				t.Fatalf("got %q at %q, want ceiling_exceeded at %q", got.Code, got.Path, tc.path)
			}
		})
	}
	// At the ceiling is inside it, and an absent ceiling bounds nothing.
	obj := sandbox()
	obj.Spec.Resources = v1.Resources{CPU: "4", Memory: "8Gi", Disk: "20Gi"}
	obj.Spec.Lifecycle.TTL = "24h"
	resolve(t, obj, o)
	obj.Spec.Resources.CPU = "64"
	obj.Spec.Lifecycle.TTL = v1.DurationNever
	resolve(t, obj, containerOptions())
	// A ttl ceiling of never is no ceiling.
	unbounded := containerOptions()
	unbounded.Ceilings.TTL = v1.DurationNever
	resolve(t, obj, unbounded)
	// A limit no manifest can meet is refused, never ignored.
	limited := containerOptions()
	limited.Limits = Limits{MaxPriority: -1}
	if got := refusal(t, sandbox(), limited); got.Code != "ceiling_exceeded" {
		t.Fatalf("code = %q, want ceiling_exceeded", got.Code)
	}
	// A ceiling this server cannot parse is the server's fault.
	broken := containerOptions()
	broken.Ceilings.CPU = "four"
	obj = sandbox()
	obj.Spec.Resources.CPU = "1"
	if _, err := Resolve(t.Context(), &obj, broken); err == nil {
		t.Fatal("an invalid ceiling was accepted")
	} else if known := (*Error)(nil); errors.As(err, &known) {
		t.Fatalf("an invalid ceiling surfaced as a contract error: %v", err)
	}
	broken = containerOptions()
	broken.Ceilings.TTL = "day"
	obj = sandbox()
	obj.Spec.Lifecycle.TTL = "1h"
	if _, err := Resolve(t.Context(), &obj, broken); err == nil {
		t.Fatal("an invalid ttl ceiling was accepted")
	}
}

func TestImmutableFields(t *testing.T) {
	existing := resolve(t, sandbox(), containerOptions()).Sandbox
	existing.Spec.Env = map[string]string{"A": "b"}
	o := containerOptions()
	o.Existing = &existing
	next := existing
	next.Metadata.Name = "renamed"
	next.Spec.Image = "ghcr.io/example/sandbox:1.4"
	next.Spec.Command = []string{"sh"}
	next.Spec.Args = []string{"-c", "true"}
	next.Spec.Workdir = "/srv"
	next.Spec.User = "1000"
	next.Spec.Workspace = v1.Workspace{Path: "/srv", Source: v1.WorkspaceSourceEmpty}
	next.Spec.Env = map[string]string{"A": "changed"}
	got := refusal(t, next, o)
	want := []string{"metadata.name", "spec.image", "spec.command", "spec.args", "spec.workdir", "spec.user", "spec.workspace.path", "spec.env"}
	if got.Code != "immutable_field" || !slices.Equal(got.Paths, want) {
		t.Fatalf("got %q with %v, want immutable_field with %v", got.Code, got.Paths, want)
	}
	if got.Path != want[0] {
		t.Fatalf("path = %q, want the first changed path", got.Path)
	}
	// The mutable fields of the table are accepted on the same update.
	next = existing
	next.Metadata.Labels = map[string]string{"team": "b"}
	next.Spec.Lifecycle = v1.Lifecycle{TTL: "1h"}
	next.Spec.Resources = v1.Resources{CPU: "2"}
	resolve(t, next, o)
	// An environment that cannot resize refuses the same change.
	native := nativeOptions()
	nativeExisting := resolve(t, sandbox(), native).Sandbox
	native.Existing = &nativeExisting
	next = nativeExisting
	next.Spec.Resources = v1.Resources{CPU: "2"}
	if got = refusal(t, next, native); got.Code != "capability_unsupported" || got.Path != "spec.resources" {
		t.Fatalf("got %q at %q, want capability_unsupported at spec.resources", got.Code, got.Path)
	}
	// An environment change is immutable even where the environment exists.
	next = existing
	next.Spec.Environment = "other"
	if got = refusal(t, next, o); got.Code != "not_found" {
		t.Fatalf("code = %q, want not_found for an environment this lookup does not serve", got.Code)
	}
}

func TestAdmissionOutputIsValidated(t *testing.T) {
	// A nil admission step is the identity.
	plain := resolve(t, sandbox(), containerOptions()).Sandbox
	o := containerOptions()
	o.Actor = Actor{Subject: "https://login.example.com|alice"}
	var seen AdmitRequest
	o.Admit = func(_ context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = req
		out := *in
		out.Spec.Resources.CPU = "2"
		return &out, []string{"The policy set the cpu request."}, nil
	}
	got := resolve(t, sandbox(), o)
	if got.Sandbox.Spec.Resources.CPU != "2" || !slices.Equal(got.Warnings, []string{"The policy set the cpu request."}) {
		t.Fatalf("admission output = %+v, warnings %v", got.Sandbox.Spec, got.Warnings)
	}
	if got.Sandbox.Status.Warnings == nil || seen.Action != "create" || seen.Actor != o.Actor || seen.Environment == nil || seen.Environment.Metadata.Name != "default" {
		t.Fatalf("request = %+v, status warnings %v", seen, got.Sandbox.Status.Warnings)
	}
	if plain.Spec.Resources.CPU != "" {
		t.Fatal("the identity step changed the object")
	}
	// Its output goes through structural validation again.
	o.Admit = func(_ context.Context, in *v1.Sandbox, _ AdmitRequest) (*v1.Sandbox, []string, error) {
		out := *in
		out.Spec.Resources.CPU = "two"
		return &out, nil, nil
	}
	if bad := refusal(t, sandbox(), o); bad.Code != "invalid_field" || bad.Path != "spec.resources.cpu" {
		t.Fatalf("got %q at %q", bad.Code, bad.Path)
	}
	// It may not change the kind, return nothing, or rename an existing object.
	for _, tc := range []struct {
		name  string
		admit AdmitFunc
	}{
		{"kind", func(_ context.Context, in *v1.Sandbox, _ AdmitRequest) (*v1.Sandbox, []string, error) {
			out := *in
			out.Kind = "Secret"
			return &out, nil, nil
		}},
		{"version", func(_ context.Context, in *v1.Sandbox, _ AdmitRequest) (*v1.Sandbox, []string, error) {
			out := *in
			out.APIVersion = "cella.latere.ai/v2"
			return &out, nil, nil
		}},
		{"nothing", func(context.Context, *v1.Sandbox, AdmitRequest) (*v1.Sandbox, []string, error) {
			return nil, nil, nil
		}},
		{"refusal", func(context.Context, *v1.Sandbox, AdmitRequest) (*v1.Sandbox, []string, error) {
			return nil, nil, errors.New("this image is not in the catalogue")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := containerOptions()
			refused.Admit = tc.admit
			if got := refusal(t, sandbox(), refused); got.Code != "admission_refused" {
				t.Fatalf("code = %q, want admission_refused", got.Code)
			}
		})
	}
	update := containerOptions()
	existing := plain
	update.Existing = &existing
	update.Admit = func(_ context.Context, in *v1.Sandbox, req AdmitRequest) (*v1.Sandbox, []string, error) {
		seen = req
		out := *in
		out.Metadata.Name = "renamed"
		return &out, nil, nil
	}
	if got := refusal(t, plain, update); got.Code != "admission_refused" {
		t.Fatalf("code = %q, want admission_refused", got.Code)
	}
	if seen.Action != "update" || seen.Existing != &existing {
		t.Fatalf("request = %+v, want the update action and the stored object", seen)
	}
}

func TestNameGeneration(t *testing.T) {
	o := containerOptions()
	o.NewName = func() string { return "brave-otter-1a2b" }
	obj := sandbox()
	obj.Metadata.Name = ""
	if got := resolve(t, obj, o).Sandbox; got.Metadata.Name != "brave-otter-1a2b" {
		t.Fatalf("name = %q", got.Metadata.Name)
	}
	// A name the caller set is never replaced.
	if got := resolve(t, sandbox(), o).Sandbox; got.Metadata.Name != "work" {
		t.Fatalf("name = %q, want the caller's", got.Metadata.Name)
	}
	// A generated name is held to the same rule as a written one.
	o.NewName = func() string { return "Not A Name" }
	if got := refusal(t, obj, o); got.Code != "invalid_field" || got.Path != "metadata.name" {
		t.Fatalf("got %q at %q", got.Code, got.Path)
	}
	// Without a generator the name stays absent for the controller to fill.
	if got := resolve(t, obj, containerOptions()).Sandbox; got.Metadata.Name != "" {
		t.Fatalf("name = %q, want it left absent", got.Metadata.Name)
	}
}

type lookupFunc func(context.Context, string) (*v1.Environment, error)

func (f lookupFunc) Environment(ctx context.Context, name string) (*v1.Environment, error) {
	return f(ctx, name)
}

func TestLookupErrors(t *testing.T) {
	obj := sandbox()
	obj.Spec.Environment = "eu-gpu"
	if got := refusal(t, obj, containerOptions()); got.Code != "not_found" || got.Path != "spec.environment" {
		t.Fatalf("got %q at %q, want not_found", got.Code, got.Path)
	}
	// A lookup that cannot decide fails closed, and a decided refusal keeps
	// its own code.
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"unavailable", errors.New("the authorizer timed out"), "authorizer_unavailable"},
		{"not found", ErrNotFound, "not_found"},
		{"decided", failAt("forbidden", "spec.environment", "This caller may not use that environment."), "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Lookup: lookupFunc(func(context.Context, string) (*v1.Environment, error) { return nil, tc.err })}
			if got := refusal(t, sandbox(), o); got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
		})
	}
	// A lookup that answers nothing at all is a missing environment.
	empty := Options{Lookup: lookupFunc(func(context.Context, string) (*v1.Environment, error) { return nil, nil })}
	if got := refusal(t, sandbox(), empty); got.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", got.Code)
	}
	// The default environment answers the empty name and its name is written back.
	if got := resolve(t, sandbox(), containerOptions()).Sandbox; got.Spec.Environment != "default" {
		t.Fatalf("environment = %q", got.Spec.Environment)
	}
	// Resolve needs a manifest and a lookup.
	if _, err := Resolve(t.Context(), nil, containerOptions()); err == nil {
		t.Fatal("a nil manifest was accepted")
	}
	if _, err := Resolve(t.Context(), &obj, Options{}); err == nil {
		t.Fatal("a missing lookup was accepted")
	}
}

func TestStatusIsIgnoredOnApply(t *testing.T) {
	obj := sandbox()
	obj.Status = v1.SandboxStatus{ID: "sbx_forged", Owner: "mallory", Phase: "Running", Warnings: []string{"forged"}}
	got := resolve(t, obj, nativeOptions())
	if got.Sandbox.Status.ID != "" || got.Sandbox.Status.Owner != "" || got.Sandbox.Status.Phase != "" {
		t.Fatalf("status survived apply: %+v", got.Sandbox.Status)
	}
	if got.Sandbox.Status.Warnings != nil || got.Warnings != nil {
		t.Fatalf("warnings = %v, want none for a manifest with nothing to warn about", got.Sandbox.Status.Warnings)
	}
	// The resolver returns its own warnings in the status it clears.
	obj = sandbox()
	obj.Spec.User = "1000"
	got = resolve(t, obj, nativeOptions())
	if !slices.Equal(got.Sandbox.Status.Warnings, []string{WarningUserNotApplied}) {
		t.Fatalf("status warnings = %v", got.Sandbox.Status.Warnings)
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	o := containerOptions()
	o.Defaults = Defaults{CPU: "1", Memory: "2Gi", Disk: "10Gi", AutoStop: "15m", TTL: "24h", AutoDelete: "72h"}
	o.NewName = func() string { return "brave-otter-1a2b" }
	in := sandbox()
	in.Metadata.Name = ""
	in.Metadata.Labels = map[string]string{"team": "research", "tier": "gold"}
	in.Metadata.Annotations = map[string]string{"example.org/ticket": "1234"}
	in.Spec.Command, in.Spec.Args = []string{"/bin/bash"}, []string{"-l"}
	in.Spec.Env = map[string]string{"LOG_LEVEL": "debug", "REGION": "eu"}
	in.Spec.User = "1000:1000"
	before, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(resolve(t, in, o).Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		next, err := json.Marshal(resolve(t, in, o).Sandbox)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(first, next) {
			t.Fatalf("resolve is not deterministic:\n%s\n%s", first, next)
		}
	}
	after, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("resolve mutated its input:\n%s\n%s", before, after)
	}
	// The returned object owns its maps and slices too.
	got := resolve(t, in, o)
	got.Sandbox.Metadata.Labels["team"] = "mutated"
	got.Sandbox.Spec.Command[0] = "/bin/sh"
	got.Warnings = append(got.Warnings, "mutated")
	again, err := json.Marshal(resolve(t, in, o).Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, again) {
		t.Fatal("a caller mutated the resolver's own state")
	}
}

func TestNativeWarnsInsteadOfRefusing(t *testing.T) {
	obj := sandbox()
	obj.Spec.Resources = v1.Resources{CPU: "2", Memory: "4Gi"}
	obj.Spec.User = "1000"
	got := resolve(t, obj, nativeOptions())
	want := []string{WarningResourcesNotEnforced, WarningUserNotApplied}
	if !slices.Equal(got.Warnings, want) {
		t.Fatalf("warnings = %v, want %v", got.Warnings, want)
	}
	if got.Sandbox.Spec.Resources.CPU != "2" || got.Sandbox.Spec.User != "1000" {
		t.Fatal("the recorded request was dropped")
	}
	// An environment that confines the workload enforces both and warns about
	// neither.
	if warnings := resolve(t, obj, containerOptions()).Warnings; warnings != nil {
		t.Fatalf("warnings = %v, want none from a container environment", warnings)
	}
}

func TestResolveNative(t *testing.T) {
	got, err := ResolveNative(sandbox(), "default")
	if err != nil || got.Spec.Environment != "default" || got.Spec.Workdir != DefaultWorkspacePath {
		t.Fatal(got, err)
	}
	if got.Spec.Workspace != (v1.Workspace{Path: DefaultWorkspacePath, Source: v1.WorkspaceSourceEmpty}) {
		t.Fatalf("workspace = %+v", got.Spec.Workspace)
	}
	for _, tc := range []struct {
		name string
		mut  func(*v1.Sandbox)
		code string
	}{
		{"image", func(o *v1.Sandbox) { o.Spec.Image = "ghcr.io/example/sandbox:1.4" }, "capability_unsupported"},
		{"args without command", func(o *v1.Sandbox) { o.Spec.Args = []string{"-l"} }, "invalid_field"},
		{"command NUL", func(o *v1.Sandbox) { o.Spec.Command = []string{"sh", "\x00"} }, "invalid_field"},
		{"workdir", func(o *v1.Sandbox) { o.Spec.Workdir = "/etc" }, "capability_unsupported"},
		{"workspace path", func(o *v1.Sandbox) { o.Spec.Workspace.Path = "/srv/work" }, "capability_unsupported"},
		{"environment", func(o *v1.Sandbox) { o.Spec.Environment = "other" }, "not_found"},
		{"version", func(o *v1.Sandbox) { o.APIVersion = "cella.latere.ai/v1" }, "unsupported_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			tc.mut(&obj)
			_, err := ResolveNative(obj, "default")
			var known *Error
			if !errors.As(err, &known) || known.Code != tc.code {
				t.Fatalf("got %v, want %q", err, tc.code)
			}
		})
	}
	obj := sandbox()
	obj.Spec.Command, obj.Spec.Args = []string{"sh"}, []string{"-c", "true"}
	if _, err = ResolveNative(obj, "default"); err != nil {
		t.Fatal(err)
	}
}
