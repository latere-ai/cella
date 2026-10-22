// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"slices"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// WarningEgressNarrowed opens the warning a manifest resolved against a host
// network carries when the host narrowed what it asked for; the effective
// mode and hosts follow it.
const WarningEgressNarrowed = "This environment's host narrows the egress boundary; the sandbox gets"

// hostsOf is what every live worker of an environment reports of its host.
func hostsOf(env *v1.Environment) []v1.HostInfo {
	out := make([]v1.HostInfo, 0, len(env.Status.Hosts))
	for _, h := range env.Status.Hosts {
		out = append(out, h.Host)
	}
	return out
}

// hostWorkspaceCapability holds spec.workspace.host to what the environment
// can bind: a driver that declares HostWorkspace, and a folder inside a
// grant of every host the environment's live workers report (spec 081). A
// folder one host grants and another does not is refused, because a sandbox
// may be placed on either. The worker and its driver check again against
// their own grants, so a stale view here never widens what a host allows.
func hostWorkspaceCapability(obj *v1.Sandbox, env *v1.Environment) error {
	folder := obj.Spec.Workspace.Host
	if folder == "" {
		return nil
	}
	if !env.Status.Capabilities.HostWorkspace {
		return failAt("capability_unsupported", pathWorkspaceHost, "This environment binds no host folder as a workspace.")
	}
	hosts := hostsOf(env)
	if len(hosts) == 0 {
		return failAt("workspace_not_granted", pathWorkspaceHost, "No live worker of this environment grants "+folder+".")
	}
	for _, h := range hosts {
		if _, granted := h.Grant(folder); !granted {
			return failAt("workspace_not_granted", pathWorkspaceHost, "A host serving this environment does not grant "+folder+".")
		}
	}
	return nil
}

// egressOnHost is the boundary's row of stage 7 for an environment whose
// workers report a host network: what the manifest asks for is narrowed to
// what every host allows, by spec 081's table, rather than refused for a
// mode the environment does not declare, and the manifest carries a warning
// naming what the sandbox gets. An environment that reports no host network
// is held to the modes it declares, as every other is.
func egressOnHost(obj *v1.Sandbox, env *v1.Environment) ([]string, error) {
	network, reported := v1.HostNetworkOf(hostsOf(env))
	if !reported {
		return egressCapability(obj, env)
	}
	asked := obj.Spec.Network.Egress
	got := v1.NarrowEgress(asked, network)
	if got.Mode == asked.Mode && slices.Equal(got.AllowedHosts, asked.AllowedHosts) && slices.Equal(got.DeniedHosts, asked.DeniedHosts) {
		return nil, nil
	}
	obj.Spec.Network.Egress = got
	warning := WarningEgressNarrowed + " " + string(got.Mode)
	if len(got.AllowedHosts) > 0 {
		warning += " to " + strings.Join(got.AllowedHosts, ", ")
	}
	if len(got.DeniedHosts) > 0 {
		warning += " except " + strings.Join(got.DeniedHosts, ", ")
	}
	return []string{warning + "."}, nil
}
