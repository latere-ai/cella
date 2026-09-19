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
}
