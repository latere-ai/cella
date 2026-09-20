// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"net"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/pkg/hostmatch"

	v1 "latere.ai/x/cella/manifest/v1"
)

// WarningEgressNotEnforced is what an environment whose driver enforces no
// egress rule returns in status.warnings. The boundary is still recorded and
// the gateway still substitutes toward a secret's scope; nothing in the
// environment stops a workload from opening a connection around it.
const WarningEgressNotEnforced = "This environment enforces no egress rule; the boundary is recorded and status.conditions reports EgressEnforced false."

// The JSON paths of the egress fields, named once so a refusal, a narrowing
// violation and a capability refusal all point at the same string.
const (
	pathEgressMode   = "spec.network.egress.mode"
	pathAllowedHosts = "spec.network.egress.allowedHosts"
	pathDeniedHosts  = "spec.network.egress.deniedHosts"
)

// validateNetwork is the structural half of the egress rule: the mode is one
// of three words, exactly one host list belongs to the mode that reads it,
// and every pattern passes the host rule. The mode is still empty here on a
// manifest that did not set one, so a rule that needs the inferred mode is
// checked again after defaulting rather than guessed at now.
func validateNetwork(n v1.Network) error {
	e := n.Egress
	if e.Mode != "" && v1.EgressModeRank(e.Mode) < 0 {
		return failAt("invalid_field", pathEgressMode, "The egress mode is none, allowlist, or open.")
	}
	switch e.Mode {
	case v1.EgressAllowlist:
		if len(e.DeniedHosts) > 0 {
			return failPaths("exclusive_fields", "Denied hosts belong to the open mode; an allow list already denies everything it does not name.", []string{pathEgressMode, pathDeniedHosts})
		}
	case v1.EgressOpen:
		if len(e.AllowedHosts) > 0 {
			return failPaths("exclusive_fields", "Allowed hosts belong to the allowlist mode; the open mode admits every host its denied list does not name.", []string{pathEgressMode, pathAllowedHosts})
		}
	case v1.EgressNone:
		if len(e.AllowedHosts) > 0 || len(e.DeniedHosts) > 0 {
			return failPaths("exclusive_fields", "The none mode blocks every host, so neither list means anything beside it.", []string{pathEgressMode, pathAllowedHosts, pathDeniedHosts})
		}
	default:
		// No mode was set, so it is about to be inferred. Two lists name two
		// different modes and no inference can serve both.
		if len(e.AllowedHosts) > 0 && len(e.DeniedHosts) > 0 {
			return failPaths("exclusive_fields", "Allowed hosts ask for the allowlist mode and denied hosts for the open mode; set one list, or set the mode.", []string{pathAllowedHosts, pathDeniedHosts})
		}
	}
	for _, list := range []struct {
		path     string
		patterns []string
	}{{pathAllowedHosts, e.AllowedHosts}, {pathDeniedHosts, e.DeniedHosts}} {
		for i, pattern := range list.patterns {
			if err := ValidateHostPattern(list.path+"["+strconv.Itoa(i)+"]", pattern); err != nil {
				return err
			}
		}
	}
	return validatePorts(n.Ports)
}

// ValidateHostPattern holds one host pattern to the contract's host rule: an
// exact fully qualified name or one leading "*." wildcard, under the grammar
// latere.ai/x/pkg/hostmatch enforces, lowercased with a trailing dot trimmed,
// carrying no port, and naming no address a sandbox could use to reach the
// machine it runs on. It is exported because a Secret's scope is held to the
// same rule.
func ValidateHostPattern(path, pattern string) error {
	raw := strings.TrimSpace(pattern)
	if raw == "" {
		return failAt("invalid_field", path, "A host pattern cannot be empty.")
	}
	if strings.ContainsRune(raw, ':') {
		return failAt("invalid_field", path, "A host pattern names a host and never a port.")
	}
	if strings.ContainsRune(raw, '/') {
		return failAt("invalid_field", path, "A host pattern names a host and never a path or a scheme.")
	}
	normalized := v1.NormalizeHost(raw)
	bare, _ := strings.CutPrefix(normalized, "*.")
	if ip := net.ParseIP(bare); ip != nil {
		return failAt("invalid_field", path, "A host pattern is a name, not an address; an address names one machine and outlives no deployment.")
	}
	if !hostmatch.ValidPattern(normalized) {
		return failAt("invalid_field", path, "A host pattern is a fully qualified name such as api.example.com, or one leading wildcard such as *.example.com.")
	}
	if isLocalName(bare) {
		return failAt("invalid_field", path, "A host pattern cannot name the machine the sandbox runs on.")
	}
	return nil
}

// isLocalName reports whether a name resolves by convention to the host the
// sandbox runs on. A single label is already refused by the pattern grammar;
// this catches the reserved names that carry the same meaning with a dot in
// them.
func isLocalName(name string) bool {
	return name == "localhost" || strings.HasSuffix(name, ".localhost") || strings.HasSuffix(name, ".local")
}

// inferEgressMode is stage 2 for the boundary: a manifest that named no mode
// takes the one its own fields ask for. Allowed hosts ask for allowlist;
// denied hosts ask for open; a mounted secret with no host list asks for
// allowlist, because a sandbox that reaches everything has no boundary for
// the secret's scope to sit inside. An empty manifest asks for open. Stage 1
// has already refused the one combination no single mode serves.
func inferEgressMode(n *v1.Network, mountsASecret bool) {
	switch {
	case n.Egress.Mode != "":
	case len(n.Egress.AllowedHosts) > 0:
		n.Egress.Mode = v1.EgressAllowlist
	case len(n.Egress.DeniedHosts) > 0:
		n.Egress.Mode = v1.EgressOpen
	case mountsASecret:
		n.Egress.Mode = v1.EgressAllowlist
	default:
		n.Egress.Mode = v1.EgressOpen
	}
}

// narrowing holds a workload actor to the boundary its sandbox was created
// with. Every offending path is named in one error, so a caller learns the
// whole of what it may not do in one round. A non-workload actor passes
// through: widening the boundary is the owner's to do.
func narrowing(existing, obj *v1.Sandbox) error {
	was, now := existing.Spec.Network.Egress, obj.Spec.Network.Egress
	var paths []string
	if v1.EgressModeRank(now.Mode) < v1.EgressModeRank(was.Mode) {
		paths = append(paths, pathEgressMode)
	}
	// An allow list widens when it names a destination the old one did not
	// reach. Containment is directional, so replacing a name with a wildcard
	// over it widens even though the old name still matches.
	if was.Mode == v1.EgressAllowlist && now.Mode == v1.EgressAllowlist {
		for _, pattern := range now.AllowedHosts {
			if !v1.HostCovers(was.AllowedHosts, pattern) {
				paths = append(paths, pathAllowedHosts)
				break
			}
		}
	}
	// A deny list widens when a destination the old one refused is no longer
	// refused, which is removing an entry or replacing one with a narrower
	// pattern.
	if was.Mode == v1.EgressOpen && now.Mode == v1.EgressOpen {
		for _, pattern := range was.DeniedHosts {
			if !v1.HostCovers(now.DeniedHosts, pattern) {
				paths = append(paths, pathDeniedHosts)
				break
			}
		}
	}
	// A secret is reach as much as a host is: mounting one the sandbox did
	// not have puts a value in its hands that its owner did not give it.
	if narrowingSecrets(existing, obj) {
		paths = append(paths, pathSecrets)
	}
	if len(paths) == 0 {
		return nil
	}
	return failPaths("boundary_widened", "A sandbox cannot widen its own network boundary: "+strings.Join(paths, ", ")+".", paths)
}

// egressCapability is the boundary's row of stage 7. An environment declares
// the modes its driver enforces; a mode it does not declare is refused rather
// than recorded, and an environment that declares none warns instead, because
// the manifest is still meaningful there: the gateway substitutes toward a
// secret's scope whether or not the environment confines the workload.
//
// The warning is about what the environment could not honour, so it is
// written only for a manifest that asked for something. A boundary of open
// with no denied host asks for nothing to be kept out, and an environment
// that keeps nothing out has honoured it.
func egressCapability(obj *v1.Sandbox, env *v1.Environment) ([]string, error) {
	e := obj.Spec.Network.Egress
	modes := env.Status.Capabilities.Egress
	if len(modes) == 0 {
		if e.Mode == v1.EgressOpen && len(e.DeniedHosts) == 0 {
			return nil, nil
		}
		return []string{WarningEgressNotEnforced}, nil
	}
	if !slices.Contains(modes, e.Mode) {
		return nil, failAt("capability_unsupported", pathEgressMode, "This environment does not enforce the "+string(e.Mode)+" egress mode.")
	}
	return nil, nil
}

// The JSON paths of the port fields.
const pathPorts = "spec.network.ports"

func portPath(i int, field string) string {
	return pathPorts + "[" + strconv.Itoa(i) + "]." + field
}

// validatePorts holds the declared ports to the field table of spec 003: a
// name that is a DNS label and a port that is a port, each unique in the list,
// and a reach the contract has. Uniqueness is on both, because the proxy
// resolves a name to exactly one port and a probe reports one state per port.
func validatePorts(ports []v1.Port) error {
	names := make(map[string]bool, len(ports))
	numbers := make(map[int]bool, len(ports))
	for i, p := range ports {
		switch {
		case p.Name == "":
			return failAt("missing_field", portPath(i, "name"), "A port needs a name.")
		case len(p.Name) > maxNameLength || !namePattern.MatchString(p.Name):
			return failAt("invalid_field", portPath(i, "name"), "The name must be a DNS label of at most 63 characters.")
		case names[p.Name]:
			return failAt("invalid_field", portPath(i, "name"), "Two ports share the name "+p.Name+".")
		case p.Port < 1 || p.Port > 65535:
			return failAt("invalid_field", portPath(i, "port"), "A port is between 1 and 65535.")
		case numbers[p.Port]:
			return failAt("invalid_field", portPath(i, "port"), "Two ports share the number "+strconv.Itoa(p.Port)+".")
		case p.Expose != "" && !slices.Contains(v1.Exposes, p.Expose):
			return failAt("invalid_field", portPath(i, "expose"), "A port is exposed none, mesh or public.")
		}
		names[p.Name] = true
		numbers[p.Port] = true
	}
	return nil
}

// portCapability is the ports' row of stage 7. A reach beyond the control
// plane's own routes is a boundary the driver has to build: mesh is a network
// only a driver that declares Mesh can make, and public is an endpoint only an
// installed exposer can give. An environment without the capability refuses
// the field rather than recording a reach the sandbox will not have.
func portCapability(obj *v1.Sandbox, env *v1.Environment) error {
	caps := env.Status.Capabilities
	for i, p := range obj.Spec.Network.Ports {
		switch {
		case p.Expose == v1.ExposeMesh && !caps.Mesh:
			return failAt("capability_unsupported", portPath(i, "expose"), "This environment has no mesh for a port to be reached on.")
		case p.Expose == v1.ExposePublic && !caps.Ingress:
			return failAt("capability_unsupported", portPath(i, "expose"), "This environment gives a port no public endpoint.")
		}
	}
	return nil
}
