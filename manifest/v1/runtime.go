// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Isolation names the boundary a driver actually provides. It is an alias so
// phase and status transport fields can carry it without conversion.
type Isolation = string

const (
	IsolationContainer Isolation = "container"
	IsolationVM        Isolation = "vm"
	IsolationProcess   Isolation = "process"
	IsolationNone      Isolation = "none"
)

type EgressMode string

const (
	EgressNone      EgressMode = "none"
	EgressAllowlist EgressMode = "allowlist"
	EgressOpen      EgressMode = "open"
)

// Capabilities declare enforcement and optional operations, never aspirations.
// An empty Egress list means no egress enforcement is provided.
type Capabilities struct {
	Egress    []EgressMode `json:"egress"`
	Mesh      bool         `json:"mesh"`
	Ingress   bool         `json:"ingress"`
	Volumes   bool         `json:"volumes"`
	Snapshots bool         `json:"snapshots"`
	Attach    bool         `json:"attach"`
	Dial      bool         `json:"dial"`
	Display   bool         `json:"display"`
	Input     bool         `json:"input"`
	Resize    bool         `json:"resize"`
	Pool      bool         `json:"pool"`
	Files     bool         `json:"files"`
	Detach    bool         `json:"detach"`
	// Autosize says which resources the driver sizes inside a manifest's
	// spec.resources.max. It is always on the wire, each flag false where
	// the driver holds that resource fixed.
	Autosize Autosize `json:"autosize"`
}

// Autosize is one flag per resource a driver can size inside a ceiling
// (spec 078). CPU is a limit at the ceiling with the request at the floor;
// Memory is a limit raised as the workload needs it; Disk is a workspace
// that grows before it fills.
type Autosize struct {
	CPU    bool `json:"cpu"`
	Memory bool `json:"memory"`
	Disk   bool `json:"disk"`
}

// And is what two declarations both provide, flag by flag.
func (a Autosize) And(other Autosize) Autosize {
	return Autosize{CPU: a.CPU && other.CPU, Memory: a.Memory && other.Memory, Disk: a.Disk && other.Disk}
}

// Equal reports whether two declarations say the same thing. The egress modes
// are a list in a fixed order, so they are compared elementwise and the flags
// one by one.
func (c Capabilities) Equal(other Capabilities) bool {
	if len(c.Egress) != len(other.Egress) {
		return false
	}
	for i, mode := range c.Egress {
		if other.Egress[i] != mode {
			return false
		}
	}
	return c.Mesh == other.Mesh && c.Ingress == other.Ingress && c.Volumes == other.Volumes &&
		c.Snapshots == other.Snapshots && c.Attach == other.Attach && c.Dial == other.Dial &&
		c.Display == other.Display && c.Input == other.Input && c.Resize == other.Resize &&
		c.Pool == other.Pool && c.Files == other.Files && c.Detach == other.Detach &&
		c.Autosize == other.Autosize
}
