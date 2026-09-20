// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Network is the boundary a sandbox lives inside: what it may reach on its
// way out. Ports, which are what may be reached inside it, join with the
// operations contract.
type Network struct {
	Egress Egress `json:"egress,omitzero"`
	Ports  []Port `json:"ports,omitempty"`
}

// Port is one port the sandbox declares something runs on inside it, and how
// far that port may be reached from.
type Port struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Expose Expose `json:"expose,omitempty"`
}

// Expose is how far a declared port reaches.
type Expose string

// The three reaches of spec 023. None is the default: the port is reachable
// through the control plane's own routes and from nowhere else.
const (
	ExposeNone   Expose = "none"
	ExposeMesh   Expose = "mesh"
	ExposePublic Expose = "public"
)

// Exposes is every reach a port may declare, in the order spec 003 lists them.
var Exposes = []Expose{ExposeNone, ExposeMesh, ExposePublic}

// Egress declares the sandbox's reach. The mode is the rule and the two lists
// are its argument: allowedHosts belongs to allowlist and deniedHosts to open,
// so exactly one list is ever set. DNS and the control plane are admitted by
// the driver's own rule in every mode and appear in neither list.
type Egress struct {
	Mode         EgressMode `json:"mode,omitempty"`
	AllowedHosts []string   `json:"allowedHosts,omitempty"`
	DeniedHosts  []string   `json:"deniedHosts,omitempty"`
}

// EgressModeOrder ranks the modes from widest to strictest. A caller narrows
// by moving later in this order; moving earlier is widening the boundary.
var EgressModeOrder = []EgressMode{EgressOpen, EgressAllowlist, EgressNone}

// EgressModeRank is the position of a mode in EgressModeOrder, and -1 for a
// value that is not a mode.
func EgressModeRank(m EgressMode) int {
	for i, known := range EgressModeOrder {
		if known == m {
			return i
		}
	}
	return -1
}

// The condition types a Sandbox's status carries. The driver owns the first
// five and the controller owns Scheduled; each says what the environment
// actually does, never what the manifest asked for.
const (
	ConditionReady           = "Ready"
	ConditionWorkspaceReady  = "WorkspaceReady"
	ConditionEgressEnforced  = "EgressEnforced"
	ConditionVolumesAttached = "VolumesAttached"
	ConditionScheduled       = "Scheduled"
	ConditionDisplayReady    = "DisplayReady"
)

// The values a condition's Status takes, spelled as Kubernetes spells them so
// one object reads the same in both vocabularies.
const (
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"
)

// The reasons this contract's conditions carry.
const (
	// ReasonEnforced is EgressEnforced true: the driver enforces the
	// sandbox's mode and a gateway holds its map.
	ReasonEnforced = "Enforced"
	// ReasonNotEnforcedByDriver is EgressEnforced false because the
	// environment's driver declares no enforcement of the mode. The boundary
	// is recorded and the gateway still substitutes; nothing stops a workload
	// from dialing around it.
	ReasonNotEnforcedByDriver = "NotEnforcedByDriver"
	// ReasonNoGateway is EgressEnforced false because no gateway of the
	// environment holds the sandbox's map.
	ReasonNoGateway = "NoGateway"
)
