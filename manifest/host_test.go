// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// hostEnvironment is a worker environment whose live workers report hosts:
// a process sandbox that binds a host folder and declares no egress mode of
// its own.
func hostEnvironment(hosts ...v1.HostInfo) v1.Environment {
	env := NativeEnvironment("laptop")
	env.Spec.Isolation = v1.IsolationContainer
	env.Status.Isolation = v1.IsolationContainer
	env.Status.Capabilities = v1.Capabilities{HostWorkspace: true, Files: true}
	for i, h := range hosts {
		env.Status.Hosts = append(env.Status.Hosts, v1.HostStatus{Worker: "wrk_" + string(rune('a'+i)), Host: h})
	}
	return env
}

func resolveOn(t *testing.T, env v1.Environment, sb v1.Sandbox) (*Resolved, error) {
	t.Helper()
	return Resolve(context.Background(), &sb, Options{
		Lookup: FixedEnvironment(env), Defaults: Defaults{Image: "registry.example/base:1"},
	})
}

func hostSandbox(egress v1.Egress, folder string) v1.Sandbox {
	return v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: "on-host"},
		Spec: v1.SandboxSpec{Environment: "laptop", Workspace: v1.Workspace{Host: folder}, Network: v1.Network{Egress: egress}}}
}

// TestHostWorkspaceResolves: a folder every live host grants resolves with
// the workspace path and the workdir at the folder; one a host does not
// grant, or any on an environment with no live host, is workspace_not_granted;
// and an environment that binds no host folder refuses the field.
func TestHostWorkspaceResolves(t *testing.T) {
	grants := v1.HostInfo{Grants: []v1.HostGrant{{Path: "/home/dev/site", Write: true}}}
	both := v1.HostInfo{Grants: []v1.HostGrant{{Path: "/home/dev", Write: true}}}
	out, err := resolveOn(t, hostEnvironment(grants, both), hostSandbox(v1.Egress{}, "/home/dev/site/app"))
	if err != nil {
		t.Fatalf("a granted folder was refused: %v", err)
	}
	if ws := out.Sandbox.Spec.Workspace; ws.Path != "/home/dev/site/app" || ws.Source != "" || out.Sandbox.Spec.Workdir != "/home/dev/site/app" {
		t.Errorf("the workspace resolved to %+v, workdir %q", ws, out.Sandbox.Spec.Workdir)
	}
	for name, env := range map[string]v1.Environment{
		"a host that does not grant it": hostEnvironment(grants, v1.HostInfo{Grants: []v1.HostGrant{{Path: "/srv"}}}),
		"no live host":                  hostEnvironment(),
	} {
		if _, err := resolveOn(t, env, hostSandbox(v1.Egress{}, "/home/dev/site")); codeOf(t, err) != "workspace_not_granted" {
			t.Errorf("%s: answered %v", name, err)
		}
	}
	plain := hostEnvironment(grants)
	plain.Status.Capabilities.HostWorkspace = false
	if _, err := resolveOn(t, plain, hostSandbox(v1.Egress{}, "/home/dev/site")); codeOf(t, err) != "capability_unsupported" {
		t.Errorf("an environment that binds no folder answered %v", err)
	}
	for folder, code := range map[string]string{"relative/site": "invalid_field", "/": "invalid_field"} {
		if _, err := resolveOn(t, hostEnvironment(grants), hostSandbox(v1.Egress{}, folder)); codeOf(t, err) != code {
			t.Errorf("%q answered %v, want %s", folder, err, code)
		}
	}
	other := hostSandbox(v1.Egress{}, "/home/dev/site")
	other.Spec.Workspace.Path = "/workspace"
	if _, err := resolveOn(t, hostEnvironment(grants), other); codeOf(t, err) != "exclusive_fields" {
		t.Errorf("a workspace path beside a host folder answered %v", err)
	}
}

// TestEgressNarrowsToTheHost: on an environment whose hosts report a network
// a mode the environment does not declare is narrowed, not refused, and the
// resolved manifest says what the sandbox got; a manifest the host leaves as
// it asked carries no warning.
func TestEgressNarrowsToTheHost(t *testing.T) {
	host := v1.HostInfo{Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com", "pypi.org"}}}
	out, err := resolveOn(t, hostEnvironment(host), hostSandbox(v1.Egress{Mode: v1.EgressOpen}, ""))
	if err != nil {
		t.Fatalf("open on an allow-list host was refused: %v", err)
	}
	if e := out.Sandbox.Spec.Network.Egress; e.Mode != v1.EgressAllowlist || strings.Join(e.AllowedHosts, ",") != "github.com,pypi.org" {
		t.Errorf("open narrowed to %+v", e)
	}
	if len(out.Warnings) != 1 || !strings.HasPrefix(out.Warnings[0], WarningEgressNarrowed) || !strings.Contains(out.Warnings[0], "github.com, pypi.org") {
		t.Errorf("the warnings are %v", out.Warnings)
	}
	kept, err := resolveOn(t, hostEnvironment(host), hostSandbox(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"github.com"}}, ""))
	if err != nil || len(kept.Warnings) != 0 || kept.Sandbox.Spec.Network.Egress.Mode != v1.EgressAllowlist {
		t.Errorf("an allow list inside the host's resolved to %+v %v, %v", kept.Sandbox.Spec.Network.Egress, kept.Warnings, err)
	}
	anyHost := hostEnvironment(v1.HostInfo{Network: v1.HostNetwork{Mode: "open", Blocked: []string{"pastebin.com"}}})
	blocked, err := resolveOn(t, anyHost, hostSandbox(v1.Egress{Mode: v1.EgressOpen}, ""))
	if e := blocked.Sandbox.Spec.Network.Egress; err != nil || e.Mode != v1.EgressOpen || !slices.Equal(e.DeniedHosts, []string{"pastebin.com"}) ||
		len(blocked.Warnings) != 1 || !strings.HasSuffix(blocked.Warnings[0], " open except pastebin.com.") {
		t.Errorf("open on an open host that blocks a host resolved to %+v %v, %v", e, blocked.Warnings, err)
	}
	unblocked, err := resolveOn(t, hostEnvironment(v1.HostInfo{Network: v1.HostNetwork{Mode: "open"}}), hostSandbox(v1.Egress{Mode: v1.EgressOpen}, ""))
	if err != nil || len(unblocked.Warnings) != 0 || unblocked.Sandbox.Spec.Network.Egress.Mode != v1.EgressOpen {
		t.Errorf("open on an open host that blocks nothing resolved to %+v %v, %v", unblocked.Sandbox.Spec.Network.Egress, unblocked.Warnings, err)
	}
	closed, err := resolveOn(t, hostEnvironment(v1.HostInfo{Network: v1.HostNetwork{Mode: "none"}}), hostSandbox(v1.Egress{Mode: v1.EgressOpen}, ""))
	if err != nil || closed.Sandbox.Spec.Network.Egress.Mode != v1.EgressNone || !strings.HasSuffix(closed.Warnings[0], " none.") {
		t.Errorf("open on a host that allows none resolved to %+v %v, %v", closed.Sandbox.Spec.Network.Egress, closed.Warnings, err)
	}
}
