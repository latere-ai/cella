// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package v1 defines the implemented Sandbox API contract.
package v1

import "time"

const APIVersion = "cella.latere.ai/v1beta1"

// Quantity is a compute amount in Kubernetes syntax, decimal ("500m", "2") or
// binary SI ("2Gi"). It is the caller's own spelling: the manifest package
// parses it and never re-renders it, so what a caller reads back is what it
// wrote.
type Quantity string

// Duration is a Go duration ("15m") or the word never, which no time.Duration
// can express. Zero and negative values are refused by the manifest package.
type Duration string

// DurationNever disables the lifecycle rule it is written on.
const DurationNever Duration = "never"

// Sandbox describes a workspace and its server-owned runtime state.
type Sandbox struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       SandboxSpec   `json:"spec"`
	Status     SandboxStatus `json:"status,omitzero"`
}
type Metadata struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SandboxSpec is the environment a caller asks for. Boundaries this contract
// does not implement yet are refused rather than accepted and ignored.
type SandboxSpec struct {
	Environment string            `json:"environment,omitempty"`
	Image       string            `json:"image,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	User        string            `json:"user,omitempty"`
	Resources   Resources         `json:"resources,omitzero"`
	Workspace   Workspace         `json:"workspace,omitzero"`
	Env         map[string]string `json:"env,omitempty"`
	Network     Network           `json:"network,omitzero"`
	Lifecycle   Lifecycle         `json:"lifecycle,omitzero"`
}

// Resources is the compute the sandbox asks for. Disk sizes the workspace.
type Resources struct {
	CPU    Quantity `json:"cpu,omitempty"`
	Memory Quantity `json:"memory,omitempty"`
	Disk   Quantity `json:"disk,omitempty"`
}

// Workspace is the initial state and mount point of the sandbox's own files.
type Workspace struct {
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
}

// WorkspaceSourceEmpty is a workspace the sandbox starts with nothing in.
const WorkspaceSourceEmpty = "empty"

// Lifecycle is when the sandbox stops being run and being kept. AutoStop
// counts idle time, TTL counts from creation, AutoDelete counts from the stop.
type Lifecycle struct {
	AutoStop   Duration `json:"autoStop,omitempty"`
	TTL        Duration `json:"ttl,omitempty"`
	AutoDelete Duration `json:"autoDelete,omitempty"`
}
type SandboxStatus struct {
	ID             string      `json:"id"`
	Owner          string      `json:"owner"`
	Environment    string      `json:"environment"`
	Driver         string      `json:"driver"`
	Isolation      string      `json:"isolation"`
	Phase          string      `json:"phase"`
	Conditions     []Condition `json:"conditions,omitempty"`
	CreatedAt      time.Time   `json:"createdAt"`
	StartedAt      time.Time   `json:"startedAt,omitzero"`
	StoppedAt      time.Time   `json:"stoppedAt,omitzero"`
	LastActivityAt time.Time   `json:"lastActivityAt,omitzero"`
	ExpiresAt      time.Time   `json:"expiresAt,omitzero"`
	ExitCode       *int        `json:"exitCode,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Warnings       []string    `json:"warnings,omitempty"`
	// EgressState is the control plane's own record of the sandbox's
	// boundary: the credential both gateway doors authenticate and the
	// generation of the map that carries it. It is desired state, not
	// something a caller reads, so the API strips it from every response
	// and it lives here only because it must survive a restart with the
	// object it belongs to.
	EgressState *EgressState `json:"egressState,omitempty"`
}

// EgressState is what the control plane keeps per sandbox so that a gateway
// connecting with nothing, after its own restart or the control plane's, is
// handed the same map the running sandbox was started against. The credential
// is minted once at create and never rotates while the sandbox lives, because
// the sandbox holds it in its own environment and nothing re-reads it.
type EgressState struct {
	Credential string `json:"credential,omitempty"`
	Version    int64  `json:"version,omitempty"`
	// Placeholders maps a mounted secret's name to the opaque token this
	// sandbox holds in its place. It is empty until the Secret kind lands.
	Placeholders map[string]string `json:"placeholders,omitempty"`
}

// Condition is one statement about the environment, written by the server:
// what it is, whether it holds, why, and since when.
type Condition struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}
