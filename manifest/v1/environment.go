// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"errors"
	"time"
)

// KindEnvironment is the kind an Environment manifest declares.
const KindEnvironment = "Environment"

// EnvironmentIDPrefix is the kind prefix an Environment's id carries.
const EnvironmentIDPrefix = "env_"

// Environment names where sandboxes run and what that place can enforce. A
// manifest resolves against the environment it names: the isolation class is
// what the driver actually provides, and the capabilities are what its
// operations honour.
//
// One environment is in-process, driven by the driver cellad opened itself.
// Every other is served by workers that connect outbound with an environment
// key, report what their driver provides, and claim the operations the
// control plane enqueues for them (spec 021).
type Environment struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Spec       EnvironmentSpec   `json:"spec"`
	Status     EnvironmentStatus `json:"status,omitzero"`
}

// EnvironmentSpec is what the operator declared.
type EnvironmentSpec struct {
	// Mode is where the driver lives: in this process, or on workers. It is
	// immutable, and the in-process one is the control plane's own object,
	// which no caller applies.
	Mode      string    `json:"mode,omitempty"`
	Isolation Isolation `json:"isolation"`
	// Capacity is the ceiling placement admits against, the lesser of it and
	// what the live workers report.
	Capacity Capacity `json:"capacity,omitzero"`
	// Scheduling and Pool are the environment's own decision about when a
	// sandbox runs and what is kept ready for it. A manifest chooses
	// neither: a pool is an acceleration the caller cannot ask for and
	// cannot refuse (spec 020).
	Scheduling SchedulingSpec `json:"scheduling,omitzero"`
	Pool       PoolSpec       `json:"pool,omitzero"`
	// WorkspaceClass is the storage class of every managed workspace of this
	// environment; empty takes the driver's own default.
	WorkspaceClass string `json:"workspaceClass,omitempty"`
	// Gateway is the host, or host:port, a sandbox of this environment
	// reaches the egress gateway at. It is required where the driver
	// enforces egress and empty where it enforces none.
	Gateway string `json:"gateway,omitempty"`
}

// The two modes of an environment. Inprocess is the driver cellad opened for
// itself, one per control plane, and Worker is a data plane that connects
// outbound.
const (
	EnvironmentInprocess = "inprocess"
	EnvironmentWorker    = "worker"
)

// EnvironmentModes is every accepted mode, in the order spec 021 lists them.
var EnvironmentModes = []string{EnvironmentInprocess, EnvironmentWorker}

// CapacityAuto is the capacity an in-process k8s environment declares: the
// cluster's allocatable less the configured headroom, read rather than
// written. It is valid on no other environment.
const CapacityAuto = "auto"

// Capacity is how much an environment holds: a quantity triple and a count of
// sandboxes. It decodes from the object below or from the word auto, because
// spec 021 writes both in the same field.
type Capacity struct {
	CPU       Quantity `json:"cpu,omitempty"`
	Memory    Quantity `json:"memory,omitempty"`
	Disk      Quantity `json:"disk,omitempty"`
	Sandboxes int      `json:"sandboxes,omitempty"`
	// Auto says the driver reads the ceiling from the cluster it drives
	// rather than from this object. It is not a field of the object form: it
	// is the word auto written in the field's place.
	Auto bool `json:"-"`
}

// IsZero reports a capacity nobody declared, which is what an environment
// applied without the field carries.
func (c Capacity) IsZero() bool {
	return !c.Auto && c.CPU == "" && c.Memory == "" && c.Disk == "" && c.Sandboxes == 0
}

// MarshalJSON writes the word auto where the driver reads the ceiling from
// the cluster, and the object otherwise, so a read returns what an apply
// wrote.
func (c Capacity) MarshalJSON() ([]byte, error) {
	if c.Auto {
		return json.Marshal(CapacityAuto)
	}
	type plain Capacity
	return json.Marshal(plain(c))
}

// UnmarshalJSON accepts the object or the word auto. Any other string is an
// error the manifest package turns into invalid_field at spec.capacity.
func (c *Capacity) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var word string
		if err := json.Unmarshal(data, &word); err != nil {
			return err
		}
		if word != CapacityAuto {
			return errors.New("a capacity is an object or the word " + CapacityAuto)
		}
		*c = Capacity{Auto: true}
		return nil
	}
	type plain Capacity
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*c = Capacity(p)
	return nil
}

// SchedulingSpec is when a sandbox of this environment runs.
type SchedulingSpec struct {
	Mode string `json:"mode,omitempty"`
	// Queues are the named lines a sandbox waits in under the queued mode,
	// and DefaultQueue the one a manifest that names none joins.
	Queues       []string `json:"queues,omitempty"`
	DefaultQueue string   `json:"defaultQueue,omitempty"`
}

// The scheduling modes. Direct starts a sandbox now or fails it; Queued admits
// against the environment's capacity by priority and fair share and is spec
// 020's later work, refused as capability_unsupported until the queue lands.
const (
	SchedulingDirect = "direct"
	SchedulingQueued = "queued"
)

// DefaultQueueName is the queue an environment declares when it names none.
const DefaultQueueName = "default"

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

// The phases an environment passes through. Ready places, reaps and watches;
// Degraded and Offline refuse a create and leave running sandboxes alone; and
// on Offline a sandbox with no observed counterpart is held Lost rather than
// reaped, because a data plane reporting nothing is not a data plane
// reporting an empty world.
const (
	EnvironmentPending  = "Pending"
	EnvironmentReady    = "Ready"
	EnvironmentDegraded = "Degraded"
	EnvironmentOffline  = "Offline"
)

// The reasons a phase below Ready carries. A live driver with no gateway
// where the environment declares one carries ReasonNoGateway, which the
// EgressEnforced condition already names.
const (
	// ReasonNoWorker is a gateway with no worker inside the window.
	ReasonNoWorker = "NoWorker"
	// ReasonHeartbeatLost is a worker environment that has heard nothing for
	// the offline window.
	ReasonHeartbeatLost = "HeartbeatLost"
	// ReasonDriverNotReady is an in-process driver that has failed Ready for
	// the offline window.
	ReasonDriverNotReady = "DriverNotReady"
)

// EnvironmentStatus is what the control plane observed of the drivers serving
// the environment. It is written by the replica holding the environments
// lease and never by a caller.
type EnvironmentStatus struct {
	ID    string `json:"id,omitempty"`
	Owner string `json:"owner,omitempty"`
	// Phase and Reason are the machine of spec 021; Reason is empty on Ready.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Driver is recorded from the first registration, or from the in-process
	// driver, and every later worker must report the same name.
	Driver    string    `json:"driver,omitempty"`
	Isolation Isolation `json:"isolation,omitempty"`
	// Capabilities is the intersection of the live workers' reports, or the
	// in-process driver's own.
	Capabilities Capabilities `json:"capabilities,omitzero"`
	// Workers and Gateways are how many of each hold a stream open, and
	// LastHeartbeat when the most recent worker was last heard from.
	Workers       int       `json:"workers,omitempty"`
	Gateways      int       `json:"gateways,omitempty"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitzero"`
	// Used is what the environment's sandboxes hold of its capacity.
	Used Capacity `json:"used,omitzero"`
	// Version is the store row's version, which design 008's ETag carries
	// and an If-Match is compared against.
	Version   int64     `json:"version,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}
