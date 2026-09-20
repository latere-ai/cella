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
	Secrets     []SecretMount     `json:"secrets,omitempty"`
	Network     Network           `json:"network,omitzero"`
	Mesh        Mesh              `json:"mesh,omitzero"`
	Lifecycle   Lifecycle         `json:"lifecycle,omitzero"`
	// Display asks for a virtual desktop of this size. Absent, the sandbox
	// has no screen and none of the computer-use operations answer for it.
	Display *Display `json:"display,omitempty"`
}

// Display is a virtual desktop's geometry, as a sandbox declares it in
// spec.display and as an environment declares the shape it prewarms an entry
// at. Both fields are set or neither is, and the size is fixed for the
// sandbox's life: the X server sizes its frame buffer once. A pool entry and
// a create match on it for the same reason: the desktop is already up at the
// entry's resolution and a running one cannot be resized into another
// (spec 023).
type Display struct {
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Mesh is what the sandbox composes with: the peers it reaches and the
// sandboxes it may create. The two are apart because spawn rights do not need
// a mesh and a mesh does not grant them.
type Mesh struct {
	// Enabled joins a mesh. It is a root's declaration: a spawned child
	// inherits its parent's mesh and may not set this field.
	Enabled bool `json:"enabled,omitempty"`
	// Spawn is what this sandbox may create through its own workload token.
	Spawn Spawn `json:"spawn,omitzero"`
}

// Spawn is the two-axis right to create sandboxes: how many in total, and how
// many generations below this one. A child's values are at most the parent's
// remainder minus one on each axis, so the tree is bounded by what the root
// declared.
type Spawn struct {
	Budget int `json:"budget,omitempty"`
	Depth  int `json:"depth,omitempty"`
}

// SpawnStatus is the sandbox's position on both axes: the budget it was
// resolved with, how much of it the ledger has recorded as used, and the
// generations still open below it.
type SpawnStatus struct {
	Budget int `json:"budget"`
	Used   int `json:"used"`
	Depth  int `json:"depth"`
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
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Environment string `json:"environment"`
	Driver      string `json:"driver"`
	Isolation   string `json:"isolation"`
	Phase       string `json:"phase"`
	// Parent is the spawning sandbox's id and Root the id of the sandbox at
	// the top of the tree, which is the sandbox's own when a subject applied
	// it. Both are written at create and never change.
	Parent string `json:"parent,omitempty"`
	Root   string `json:"root,omitempty"`
	// Mesh is the msh_ id this sandbox is a member of, minted at the create
	// of a root that enabled one and inherited by every descendant. Empty is
	// a sandbox that reaches no peer.
	Mesh string `json:"mesh,omitempty"`
	// Spawn is the budget as the ledger and the resolved manifest report it
	// together.
	Spawn      SpawnStatus `json:"spawn,omitzero"`
	Conditions []Condition `json:"conditions,omitempty"`
	// Ports is one entry per declared port with the driver's own probe of it
	// at the last read.
	Ports []PortStatus `json:"ports,omitempty"`
	// Secrets is which placeholders are in the sandbox's environment and
	// which of them the gateway will not substitute.
	Secrets        SecretsStatus `json:"secrets,omitzero"`
	CreatedAt      time.Time     `json:"createdAt"`
	StartedAt      time.Time     `json:"startedAt,omitzero"`
	StoppedAt      time.Time     `json:"stoppedAt,omitzero"`
	LastActivityAt time.Time     `json:"lastActivityAt,omitzero"`
	ExpiresAt      time.Time     `json:"expiresAt,omitzero"`
	ExitCode       *int          `json:"exitCode,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"`
	// EgressState is the control plane's own record of the sandbox's
	// boundary: the credential both gateway doors authenticate and the
	// generation of the map that carries it. It is desired state, not
	// something a caller reads, so the API strips it from every response
	// and it lives here only because it must survive a restart with the
	// object it belongs to.
	EgressState *EgressState `json:"egressState,omitempty"`
	// TokenState is the control plane's own record of the workload token
	// the sandbox holds: what a revocation is keyed by, when it was minted
	// and when it stops verifying. The token itself is never here; it is
	// inside the sandbox and nowhere else. Like EgressState it is desired
	// state rather than something a caller reads, so the API strips it from
	// every response, and it lives here so that a control plane that
	// restarted can still end the token a running sandbox holds.
	TokenState *TokenState `json:"tokenState,omitempty"`
}

// TokenState is one sandbox's identity as the control plane tracks it. The
// two instants are what the re-mint of spec 005 is measured against: a token
// is replaced once two thirds of the span between them has passed.
type TokenState struct {
	// JTI is the key of the revocation that ends this token.
	JTI string `json:"jti"`
	// IssuedAt is the mint and ExpiresAt is when the token stops verifying
	// anywhere, which is the sandbox's own expiry where that is sooner.
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// EgressState is what the control plane keeps per sandbox so that a gateway
// connecting with nothing, after its own restart or the control plane's, is
// handed the same map the running sandbox was started against. The credential
// is minted once at create and never rotates while the sandbox lives, because
// the sandbox holds it in its own environment and nothing re-reads it.
type EgressState struct {
	Credential string `json:"credential,omitempty"`
	Version    int64  `json:"version,omitempty"`
	// Secrets is one record per mounted secret: which Secret this sandbox
	// was bound to, under which environment key, and the placeholder minted
	// for it. It lives here because a placeholder the workload already holds
	// must not change under it, and because binding by id is what keeps a
	// recreated secret of the same name from taking a running sandbox's
	// place.
	Secrets []MountedSecret `json:"secrets,omitempty"`
}

// PortStatus is one declared port and what the driver's probe found:
// listening when something inside the sandbox holds it, closed otherwise. URL
// is an endpoint a public port was given, and empty for every other port.
type PortStatus struct {
	Name  string `json:"name"`
	Port  int    `json:"port"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}

// The two states a declared port is in.
const (
	PortListening = "listening"
	PortClosed    = "closed"
)

// Condition is one statement about the environment, written by the server:
// what it is, whether it holds, why, and since when.
type Condition struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}
