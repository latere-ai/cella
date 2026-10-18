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

// The condition types a Sandbox's status carries. The driver owns Ready,
// WorkspaceReady, EgressEnforced, VolumesAttached and DisplayReady, and the
// controller owns Scheduled, MemoryAtCeiling and MemoryUnplaced; each says
// what the environment actually does, never what the manifest asked for.
// Observed is the list's, written on one answer and never stored.
const (
	ConditionReady           = "Ready"
	ConditionWorkspaceReady  = "WorkspaceReady"
	ConditionEgressEnforced  = "EgressEnforced"
	ConditionVolumesAttached = "VolumesAttached"
	ConditionScheduled       = "Scheduled"
	ConditionDisplayReady    = "DisplayReady"
	// ConditionMemoryAtCeiling True says the sandbox's memory is at its
	// spec.resources.max.memory and its workload is using nearly all of it,
	// or lost a process to the limit, so there is no room left to give. It
	// is the controller's, written when a reading crosses the line either
	// way, and absent for a sandbox with no memory ceiling (spec 078).
	ConditionMemoryAtCeiling = "MemoryAtCeiling"
	// ConditionMemoryUnplaced True says the sandbox needed more memory,
	// inside its ceiling, and no node could hold the larger size, which its
	// message names. The sandbox goes on at the size it holds. It is the
	// controller's, written when a move to the larger size is undone and
	// set False when a later raise is given, and absent for a sandbox no
	// raise ever failed for (spec 078).
	ConditionMemoryUnplaced = "MemoryUnplaced"
	// ConditionObserved False on a row of a list page says the environment's
	// driver did not answer the read that page made of this sandbox, so the
	// phase, the conditions and the instants are the ones the control plane
	// last wrote and not a read of the runtime. The list answers the row
	// rather than failing the page, and no other answer carries the
	// condition: a read of one sandbox that cannot reach its driver is
	// refused with driver_unavailable.
	ConditionObserved = "Observed"
	// ConditionHostApproval is the host's answer to a create on a worker
	// whose embedder asks before it runs one (spec 082). It is Unknown with
	// ReasonHostPending while the host has not answered, True once it
	// allowed the create, and False with the reason the sandbox failed for
	// when it refused or did not answer in time. A sandbox on a worker that
	// answers at once carries none.
	ConditionHostApproval = "HostApproval"
)

// The reasons the HostApproval condition carries.
const (
	ReasonHostPending  = "Pending"
	ReasonHostApproved = "Approved"
	ReasonHostRefused  = "HostRefused"
	ReasonHostTimeout  = "HostTimeout"
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
	// ReasonDriverUnavailable is Observed false because the driver of the
	// sandbox's environment answered the read with an error.
	ReasonDriverUnavailable = "DriverUnavailable"
	// ReasonEnvironmentNotHeld is Observed false because this control plane
	// holds no environment of the name the sandbox is placed on, so there
	// is no driver to ask.
	ReasonEnvironmentNotHeld = "EnvironmentNotHeld"
	// ReasonUsage and ReasonOOM are MemoryAtCeiling true: the working set
	// passed the warning line, or the kernel killed a process for memory.
	// ReasonBelowCeiling is the condition false again.
	ReasonUsage        = "Usage"
	ReasonOOM          = "OOM"
	ReasonBelowCeiling = "BelowCeiling"
	// ReasonMemoryRaised is status.reason of a sandbox that reads Starting
	// because the control plane is starting it again at a larger memory
	// size: its workload was killed for memory with room left under its
	// ceiling, or the node could not hold a raise and it is being moved.
	// Its workspace is kept and it reads Running again without any act of a
	// client, so a client that finds it waits (spec 078).
	ReasonMemoryRaised = "MemoryRaised"
	// ReasonMemoryNotRaised is status.reason of a sandbox that reads
	// Starting because the control plane is starting it again at the size
	// it holds: its workload was killed for memory and no node had room for
	// a larger size a moment ago. Its workspace is kept and it reads
	// Running again without any act of a client (spec 078).
	ReasonMemoryNotRaised = "MemoryNotRaised"
	// ReasonNoRoom and ReasonRestartsExhausted are MemoryUnplaced true. No
	// node had room for the size asked, and the sandbox is kept running at
	// the size it holds; or it was then killed for memory as often as it
	// is started again there, and is left Failed until a client starts or
	// deletes it. MemoryUnplaced false carries ReasonMemoryRaised: a later
	// raise was given.
	ReasonNoRoom            = "NoRoom"
	ReasonRestartsExhausted = "RestartsExhausted"
)
