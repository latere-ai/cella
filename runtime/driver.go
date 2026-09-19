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

type Capabilities = v1.Capabilities
type Isolation = v1.Isolation

type Lifecycle struct {
	TTL        time.Duration `json:"ttl,omitempty"`
	AutoStop   time.Duration `json:"autoStop,omitempty"`
	AutoDelete time.Duration `json:"autoDelete,omitempty"`
}
type CreateSpec struct {
	ID, Name, Owner, Image, Workdir string
	Command, Args                   []string
	Env, Labels                     map[string]string
	Lifecycle                       Lifecycle
}
type Ref struct {
	ID string `json:"id"`
}
type State struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Owner          string            `json:"owner"`
	Phase          string            `json:"phase"`
	Isolation      string            `json:"isolation"`
	Labels         map[string]string `json:"labels,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
	StartedAt      time.Time         `json:"startedAt"`
	StoppedAt      time.Time         `json:"stoppedAt,omitzero"`
	LastActivityAt time.Time         `json:"lastActivityAt"`
	ExpiresAt      time.Time         `json:"expiresAt,omitzero"`
	AutoStop       time.Duration     `json:"autoStop,omitempty"`
	AutoDelete     time.Duration     `json:"autoDelete,omitempty"`
}
type Filter struct {
	Owner, Phase string
	IDs          []string
}
type Change struct {
	Labels    *map[string]string
	Env       *map[string]string
	Lifecycle *Lifecycle
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
