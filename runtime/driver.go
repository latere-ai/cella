// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package runtime defines substrate operations independently of hosted services.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

var (
	ErrNotFound      = errors.New("environment not found")
	ErrAlreadyExists = errors.New("environment already exists")
	ErrNotRunning    = errors.New("environment is not running")
	ErrUnsupported   = errors.New("operation is not supported")
	ErrInvalid       = errors.New("invalid runtime request")
)

const (
	Pending        = "Pending"
	Running        = "Running"
	Stopped        = "Stopped"
	IsolationNone  = "none"
	DefaultWorkdir = "/workspace"
)

// TokenPath is where a driver projects the sandbox's workload token, under
// the control plane's own prefix, read-only and readable by the sandbox's
// user alone (specs 004 and 006). A driver that has no mount namespace to
// put it in projects the file somewhere it owns and names that path in
// TokenFileEnv instead.
const TokenPath = "/run/cella/token"

// TokenFileEnv is the variable every driver sets to where it put the token,
// which is the file the agent client of spec 011 reads per request. It is
// set only where a token was projected: a sandbox on a control plane that
// mints none carries neither the file nor the variable.
const TokenFileEnv = "CELLA_TOKEN_FILE"

type Capabilities = v1.Capabilities
type Isolation = v1.Isolation

type Lifecycle struct {
	TTL        time.Duration `json:"ttl,omitempty"`
	AutoStop   time.Duration `json:"autoStop,omitempty"`
	AutoDelete time.Duration `json:"autoDelete,omitempty"`
}

// Resources is the compute a sandbox is granted, in Kubernetes quantity
// syntax as the manifest wrote it. A driver that enforces no limit records the
// request and the resolved manifest carries the warning.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// Workspace is where the sandbox's own files live inside it.
type Workspace struct {
	Path string `json:"path,omitempty"`
}

// Egress is the sandbox's network boundary as the driver receives it: the
// rule the driver enforces itself, and what it points the workload at so
// every connection the boundary admits leaves through the gateway.
//
// Mode, AllowedHosts and DeniedHosts are the resolved manifest's, already
// joined with every mounted secret's scope. A driver that declares no egress
// capability records them and confines nothing, and the sandbox's
// EgressEnforced condition says so.
type Egress struct {
	Mode         string   `json:"mode,omitempty"`
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	DeniedHosts  []string `json:"deniedHosts,omitempty"`
	// ProxyAddr and ReverseAddr are the gateway's two doors as a sandbox of
	// this environment reaches them, host or host:port. Empty means the
	// installation runs no such door and the driver sets nothing.
	ProxyAddr   string `json:"proxyAddr,omitempty"`
	ReverseAddr string `json:"reverseAddr,omitempty"`
	// Credential is this sandbox's own, what both doors authenticate. It
	// lives as long as the sandbox.
	Credential string `json:"credential,omitempty"`
	// CAPEM is the authority the gateway signs its leaves with. The driver
	// projects it read-only inside the sandbox and names it in the trust
	// variables, so the workload trusts that door and nothing else.
	CAPEM string `json:"caPem,omitempty"`
}
type CreateSpec struct {
	ID, Name, Owner, Image, Workdir string
	Command, Args                   []string
	Env, Labels                     map[string]string
	Lifecycle                       Lifecycle
	User                            string
	Resources                       Resources
	Workspace                       Workspace
	Egress                          Egress
	// Display is the virtual desktop the sandbox asks for, nil for a sandbox
	// with no screen. The geometry is fixed at create: the X server sizes its
	// frame buffer once and the manifest field is immutable (spec 003).
	Display *Geometry `json:"display,omitempty"`
	// Ports are what the manifest declares runs inside the sandbox. The
	// driver records them, probes them at Inspect, and reaches nothing a
	// caller named that is not in this list.
	Ports []Port `json:"ports,omitempty"`
	// Token is the workload token the driver projects at TokenPath, empty
	// for a control plane that mints none. It is never serialized: a driver
	// that keeps the create spec beside the sandbox keeps the shape of the
	// sandbox and not the credential it was started with.
	Token []byte `json:"-"`
}
type Ref struct {
	ID string `json:"id"`
}
type State struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Owner     string            `json:"owner"`
	Phase     string            `json:"phase"`
	Reason    string            `json:"reason,omitempty"`
	ExitCode  *int              `json:"exitCode,omitempty"`
	Isolation string            `json:"isolation"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Ports is one entry per declared port with the driver's own probe at
	// this Inspect, and Conditions is what the driver says about the
	// environment it built: the contract's Ready, WorkspaceReady,
	// EgressEnforced, VolumesAttached and DisplayReady. A condition the
	// driver does not write is absent rather than unknown.
	Ports          []PortState    `json:"ports,omitempty"`
	Conditions     []v1.Condition `json:"conditions,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	StartedAt      time.Time      `json:"startedAt"`
	StoppedAt      time.Time      `json:"stoppedAt,omitzero"`
	LastActivityAt time.Time      `json:"lastActivityAt"`
	ExpiresAt      time.Time      `json:"expiresAt,omitzero"`
	AutoStop       time.Duration  `json:"autoStop,omitempty"`
	AutoDelete     time.Duration  `json:"autoDelete,omitempty"`
}
type Filter struct {
	Owner, Phase string
	IDs          []string
}
type Change struct {
	Labels    *map[string]string
	Env       *map[string]string
	Lifecycle *Lifecycle
	// Token re-projects the workload token before the one in the sandbox
	// expires, without restarting the workload. Empty leaves the projection
	// as it is.
	Token []byte
}
type ExecRequest struct {
	Command []string
	Env     map[string]string
	Workdir string
	Stdin   io.Reader
	TTY     bool
	Timeout time.Duration
}

// Exec streams both outputs; callers drain both before waiting, or close to cancel.
type Exec interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait(context.Context) (int, error)
	Close() error
}
type LogsRequest struct {
	Follow    bool
	Since     time.Time
	TailLines int
}

// Driver reports its actual isolation and capabilities; callers must check them.
type Driver interface {
	Name() string
	Isolation() string
	Capabilities() Capabilities
	Preflight(context.Context) error
	Ready(context.Context) error
	Create(context.Context, CreateSpec) (Ref, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Update(context.Context, string, Change) error
	Inspect(context.Context, string) (State, error)
	List(context.Context, Filter) ([]State, error)
	Exec(context.Context, string, ExecRequest) (Exec, error)
	Logs(context.Context, string, LogsRequest) (io.ReadCloser, error)
	ExportTar(context.Context, string, []string, io.Writer) error
	ImportTar(context.Context, string, string, io.Reader) error
	Touch(context.Context, string) error
}
