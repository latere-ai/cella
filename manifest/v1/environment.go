// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Environment names where sandboxes run and what that place can enforce. A
// manifest resolves against the environment it names: the isolation class is
// what the driver actually provides, and the capabilities are what its
// operations honour. Registration and capacity are the worker contract's and
// are not declared here yet.
type Environment struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Spec       EnvironmentSpec   `json:"spec"`
	Status     EnvironmentStatus `json:"status,omitzero"`
}

// EnvironmentSpec is what the operator declared.
type EnvironmentSpec struct {
	Isolation Isolation `json:"isolation"`
	// Scheduling and Pool are the environment's own decision about when a
	// sandbox runs and what is kept ready for it. A manifest chooses
	// neither: a pool is an acceleration the caller cannot ask for and
	// cannot refuse (spec 020).
	Scheduling SchedulingSpec `json:"scheduling,omitzero"`
	Pool       PoolSpec       `json:"pool,omitzero"`
}

// SchedulingSpec is when a sandbox of this environment runs.
type SchedulingSpec struct {
	Mode string `json:"mode,omitempty"`
}

// The scheduling modes. Direct starts a sandbox now or fails it; Queued admits
// against the environment's capacity by priority and fair share and is spec
// 020's later work, refused as capability_unsupported until the queue lands.
const (
	SchedulingDirect = "direct"
	SchedulingQueued = "queued"
)

// The reasons the Scheduled condition carries. Placement is the whole of what
// they say: where the sandbox came from, never whether it is healthy.
const (
	// ReasonPlaced is a sandbox the driver created for it.
	ReasonPlaced = "Placed"
	// ReasonFromPool is a sandbox adopted from an entry the environment had
	// already prewarmed. It is the only place a caller sees that its create
	// was accelerated.
	ReasonFromPool = "FromPool"
)

// PoolSpec is what the environment keeps prewarmed: entries of one shape, made
// for nobody, each turned into a caller's sandbox by one adoption. Size zero
// is no pool, which is every environment until an operator asks for one.
type PoolSpec struct {
	Size      int       `json:"size,omitempty"`
	Image     string    `json:"image,omitempty"`
	Resources Resources `json:"resources,omitzero"`
	Display   *Display  `json:"display,omitempty"`
}

// Display is a virtual desktop's geometry, as a sandbox and a pool entry each
// declare it. A pool entry and a create match on it because the desktop is
// already up at the entry's resolution and a running one cannot be resized
// into another (spec 023).
type Display struct {
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// EnvironmentStatus is what the control plane observed of the driver serving
// the environment.
type EnvironmentStatus struct {
	ID           string       `json:"id,omitempty"`
	Driver       string       `json:"driver,omitempty"`
	Isolation    Isolation    `json:"isolation,omitempty"`
	Capabilities Capabilities `json:"capabilities,omitzero"`
}
