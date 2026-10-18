// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
)

// HostPolicy is the host's own say over what the control plane sends. The
// worker asks it after the built-in checks pass, so a policy can narrow what
// the host grants and never widen it.
type HostPolicy interface {
	// Admit is asked before a create runs. It blocks until the host
	// decides or ctx ends; a person may be asked meanwhile. A create not
	// decided within a second is shown to the control plane's caller as
	// waiting for the host.
	Admit(ctx context.Context, c Create) Verdict
	// Check is asked before every other operation on a sandbox.
	Check(ctx context.Context, op Operation) Verdict
}

// Create is one create as the host decides it.
type Create struct {
	// Sandbox is the manifest as the control plane sent it: the sandbox's
	// id and owner in its status, its name and labels in its metadata, and
	// the image, command, workdir, workspace, egress and resources it asks
	// for in its spec.
	Sandbox v1.Sandbox
	// Labels are the sandbox's labels, which name the caller's session when
	// a caller sets them.
	Labels map[string]string
}

// Operation is one operation on a sandbox as the host checks it.
type Operation struct {
	// Type is the operation, one of remote's Op names: remote.OpExec,
	// remote.OpWrite, remote.OpRemove and the rest.
	Type    string
	Sandbox string
	// Command is the command of an exec.
	Command []string
	// Path is the path of a file operation, and To a move's destination.
	Path string
	To   string
}

// Verdict is the host's answer.
type Verdict struct {
	Allow bool
	// Reason is shown to the caller on a refusal. The worker keeps at most
	// MaxReasonBytes of it.
	Reason string
}

// MaxReasonBytes is the most of a Verdict's reason a refusal carries.
const MaxReasonBytes = 200

// The outcomes a Record carries.
const (
	OutcomeOK      = "ok"
	OutcomeRefused = "refused"
	OutcomeFailed  = "failed"
)

// Record is one operation the worker answered, for the host's own log. It
// carries no file content, no stdin and no output.
type Record struct {
	Time    time.Time
	Type    string
	Sandbox string
	Labels  map[string]string
	// Command is an exec's command, Path a file operation's path and To a
	// move's destination.
	Command []string
	Path    string
	To      string
	// Outcome is OutcomeOK, OutcomeRefused (the host or its built-in checks
	// said no) or OutcomeFailed (the driver did), and Reason why.
	Outcome  string
	Reason   string
	ExitCode *int
}

// heldAfter is how long a create waits on the policy before the worker tells
// the control plane it is held. A test shortens it.
var heldAfter = time.Second

// gate is the worker's half between the stream and the driver: the built-in
// checks, the host's policy, and the record of what was answered.
type gate struct {
	driver  runtime.Driver
	policy  HostPolicy
	record  func(Record)
	timeout time.Duration
	log     *slog.Logger
	now     func() time.Time

	mu sync.Mutex
	// labels is every sandbox's labels as a create or a read of the driver
	// last gave them, so a record names the caller's session.
	labels map[string]map[string]string
}

func newGate(o Options, log *slog.Logger) *gate {
	return &gate{
		driver: o.Driver, policy: o.Policy, record: o.Record, timeout: o.AdmitTimeout, log: log,
		now: time.Now, labels: map[string]map[string]string{},
	}
}

// onSandbox reports whether an operation is about one sandbox, and so is
// refused for a sandbox the driver does not hold and asked of Check.
func onSandbox(opType string) bool {
	switch opType {
	case remote.OpCreate, remote.OpList, remote.OpWatch:
		return false
	}
	return true
}

// writes reports whether a file operation changes the host folder it names.
func writes(opType string) bool {
	switch opType {
	case remote.OpWrite, remote.OpMkdir, remote.OpRemove, remote.OpMove, remote.OpImportTar:
		return true
	}
	return false
}

// admit is the remote server's Admit: the built-in checks first, then the
// policy. hold sends the held frame for the operation.
func (g *gate) admit(ctx context.Context, opType string, req remote.Request, hold func() error) error {
	if opType == remote.OpCreate {
		if req.Spec == nil {
			return fmt.Errorf("%w: a create carries a spec", runtime.ErrInvalid)
		}
		if err := g.checkCreate(*req.Spec); err != nil {
			return err
		}
		g.remember(req.Spec.ID, req.Spec.Labels)
		if g.policy == nil {
			return nil
		}
		return g.ask(ctx, createOf(*req.Spec), hold)
	}
	if !onSandbox(opType) {
		return nil
	}
	state, err := g.driver.Inspect(ctx, req.ID)
	if errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("%w: %w %s", runtime.ErrNotFound, errUnknownSandbox, req.ID)
	}
	if err != nil {
		return err
	}
	g.remember(req.ID, state.Labels)
	op := operationOf(opType, req)
	if err := g.checkWrite(op); err != nil {
		return err
	}
	if g.policy == nil {
		return nil
	}
	if v := g.policy.Check(ctx, op); !v.Allow {
		return refused(v.Reason)
	}
	return nil
}

// checkCreate is the built-in half of a create: what the host grants, read
// from the driver and not from what the control plane resolved. The network
// is not checked here, because the driver narrows whatever egress arrives to
// the host's own network.
func (g *gate) checkCreate(spec runtime.CreateSpec) error {
	if spec.Isolation != "" && spec.Isolation != g.driver.Isolation() {
		return fmt.Errorf("%w: the sandbox asks for %s isolation and this host provides %s",
			runtime.ErrHostRefused, spec.Isolation, g.driver.Isolation())
	}
	folder := spec.Workspace.Host
	if folder == "" {
		return nil
	}
	describer, ok := g.driver.(runtime.HostDescriber)
	if !ok || !g.driver.Capabilities().HostWorkspace {
		return fmt.Errorf("%w: this host binds no host folder, and the sandbox names %s", runtime.ErrWorkspaceNotGranted, folder)
	}
	if !path.IsAbs(folder) || path.Clean(folder) != folder {
		return fmt.Errorf("%w: %s is not a clean absolute path", runtime.ErrWorkspaceNotGranted, folder)
	}
	if _, granted := describer.Host().Grant(folder); !granted {
		return fmt.Errorf("%w: %s is outside every folder this host grants", runtime.ErrWorkspaceNotGranted, folder)
	}
	return nil
}

// checkWrite refuses a file operation that writes into a folder the host
// grants read-only, for the source and, on a move, the destination.
func (g *gate) checkWrite(op Operation) error {
	if !writes(op.Type) {
		return nil
	}
	describer, ok := g.driver.(runtime.HostDescriber)
	if !ok {
		return nil
	}
	host := describer.Host()
	for _, p := range []string{op.Path, op.To} {
		if p == "" {
			continue
		}
		if grant, held := host.Grant(path.Clean(p)); held && !grant.Write {
			return fmt.Errorf("%w: %s is in a folder this host grants read-only", runtime.ErrWorkspaceNotGranted, p)
		}
	}
	return nil
}

// ask puts one create to the policy. A create the policy has not answered
// within heldAfter is reported held, so the caller sees the host is asking;
// one not answered within the admit timeout is refused as host_timeout. A
// caller that went away ends the question with its own error.
func (g *gate) ask(ctx context.Context, c Create, hold func() error) error {
	asking, cancel := context.WithCancel(ctx)
	defer cancel()
	verdicts := make(chan Verdict, 1)
	go func() { verdicts <- g.policy.Admit(asking, c) }()
	held := time.NewTimer(heldAfter)
	defer held.Stop()
	deadline := time.NewTimer(g.timeout)
	defer deadline.Stop()
	for {
		select {
		case v := <-verdicts:
			if v.Allow {
				return nil
			}
			return refused(v.Reason)
		case <-held.C:
			if err := hold(); err != nil {
				g.log.WarnContext(ctx, "the worker could not tell the control plane a create is held", "sandbox", c.Sandbox.Status.ID, "err", err)
			}
		case <-deadline.C:
			return fmt.Errorf("%w: no answer within %s", runtime.ErrHostTimeout, g.timeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// refused is the host's refusal with its reason, cut to MaxReasonBytes at a
// character boundary.
func refused(reason string) error {
	if len(reason) > MaxReasonBytes {
		cut := MaxReasonBytes
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	if reason == "" {
		return runtime.ErrHostRefused
	}
	return fmt.Errorf("%s (%w)", reason, runtime.ErrHostRefused)
}

// answered is the remote server's Answered: one Record per operation.
func (g *gate) answered(opType string, req remote.Request, res remote.Response, err error) {
	id := req.ID
	if req.Spec != nil {
		id = req.Spec.ID
	}
	op := operationOf(opType, req)
	rec := Record{
		Time: g.now(), Type: opType, Sandbox: id, Labels: g.labelsOf(id),
		Command: slices.Clone(op.Command), Path: op.Path, To: op.To,
		Outcome: OutcomeOK, ExitCode: res.Exit,
	}
	switch {
	case err == nil:
	case refusal(err):
		rec.Outcome, rec.Reason = OutcomeRefused, err.Error()
	default:
		rec.Outcome, rec.Reason = OutcomeFailed, err.Error()
	}
	if opType == remote.OpDelete && err == nil {
		g.mu.Lock()
		delete(g.labels, id)
		g.mu.Unlock()
	}
	if g.record != nil {
		g.record(rec)
	}
}

// errUnknownSandbox is an operation on a sandbox the driver does not hold.
// It crosses the seam as not found, so a control plane deleting a sandbox
// its worker lost reads it as gone, and it is recorded as a refusal.
var errUnknownSandbox = errors.New("the host holds no sandbox")

// refusal reports whether an error is the host's or its built-in checks'
// rather than the driver's.
func refusal(err error) bool {
	return errors.Is(err, runtime.ErrHostRefused) || errors.Is(err, runtime.ErrHostTimeout) ||
		errors.Is(err, runtime.ErrWorkspaceNotGranted) || errors.Is(err, errUnknownSandbox)
}

func (g *gate) remember(id string, labels map[string]string) {
	if id == "" {
		return
	}
	g.mu.Lock()
	g.labels[id] = maps.Clone(labels)
	g.mu.Unlock()
}

func (g *gate) labelsOf(id string) map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.labels[id])
}

// operationOf is what the policy is told of one operation.
func operationOf(opType string, req remote.Request) Operation {
	op := Operation{Type: opType, Sandbox: req.ID, Path: req.Path}
	switch opType {
	case remote.OpExec:
		if req.Exec != nil {
			op.Command = slices.Clone(req.Exec.Command)
		}
	case remote.OpMove:
		op.To = req.To
	case remote.OpImportTar:
		op.Path = req.Dest
	case remote.OpExportTar:
		if len(req.Paths) > 0 {
			op.Path = req.Paths[0]
		}
	}
	return op
}

// createOf is the manifest the host decides a create on, built from what the
// control plane sent. The workload token is never in it.
func createOf(spec runtime.CreateSpec) Create {
	sb := v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: v1.KindSandbox,
		Metadata: v1.Metadata{Name: spec.Name, Labels: maps.Clone(spec.Labels)},
		Spec: v1.SandboxSpec{
			Image: spec.Image, Command: slices.Clone(spec.Command), Args: slices.Clone(spec.Args),
			Workdir:   spec.Workdir,
			Workspace: v1.Workspace{Path: spec.Workspace.Path, Host: spec.Workspace.Host},
			Network: v1.Network{Egress: v1.Egress{
				Mode:         v1.EgressMode(spec.Egress.Mode),
				AllowedHosts: slices.Clone(spec.Egress.AllowedHosts),
				DeniedHosts:  slices.Clone(spec.Egress.DeniedHosts),
			}},
		},
		Status: v1.SandboxStatus{ID: spec.ID, Owner: spec.Owner, Isolation: spec.Isolation},
	}
	return Create{Sandbox: sb, Labels: maps.Clone(spec.Labels)}
}
