// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package native runs host processes with no isolation. Use it only for trusted
// local development and tests; file API containment is not a process sandbox.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	driver "latere.ai/x/cella/runtime"
)

type record struct {
	State         driver.State
	Env           map[string]string
	Workdir       string
	Command, Args []string
	PID           int
}

// Driver owns directory-backed environments and all executions it starts.
// One Driver instance must exclusively own its root directory.
type Driver struct {
	root   string
	mu     sync.Mutex
	active map[string]map[*execution]struct{}
	mains  map[string]*mainProcess
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.Attacher = (*Driver)(nil)
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func New(root string) (*Driver, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: root is required", driver.ErrInvalid)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	d := &Driver{root: abs, active: make(map[string]map[*execution]struct{}), mains: make(map[string]*mainProcess)}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID.MatchString(entry.Name()) {
			continue
		}
		r, err := d.load(entry.Name())
		if err != nil {
			return nil, err
		}
		if len(r.Command) > 0 && r.State.Phase == driver.Running {
			r.State.Phase = "Lost"
			r.State.Reason = "ProcessUnrecoverable"
			if err := d.save(entry.Name(), r); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}
func (d *Driver) Name() string      { return "native" }
func (d *Driver) Isolation() string { return driver.IsolationNone }
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Files: true, Attach: ptySupported}
}
func (d *Driver) Preflight(ctx context.Context) error { return d.Ready(ctx) }
func (d *Driver) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.mains {
		if err := d.mainError(id); err != nil {
			return err
		}
	}
	_, err := os.Stat(d.root)
	return err
}
func (d *Driver) dir(id string) string { return filepath.Join(d.root, id) }
func (d *Driver) load(id string) (record, error) {
	if !validID.MatchString(id) {
		return record{}, driver.ErrInvalid
	}
	b, err := os.ReadFile(filepath.Join(d.dir(id), "record.json"))
	if errors.Is(err, os.ErrNotExist) {
		return record{}, driver.ErrNotFound
	}
	if err != nil {
		return record{}, err
	}
	var r record
	err = json.Unmarshal(b, &r)
	return r, err
}
func (d *Driver) save(id string, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := filepath.Join(d.dir(id), "record.json")
	if err = os.WriteFile(p+".tmp", b, 0600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}
func (d *Driver) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if err := ctx.Err(); err != nil {
		return driver.Ref{}, err
	}
	if !validID.MatchString(s.ID) || s.Lifecycle.TTL < 0 || s.Lifecycle.AutoStop < 0 || s.Lifecycle.AutoDelete < 0 {
		return driver.Ref{}, driver.ErrInvalid
	}
	if s.Image != "" {
		return driver.Ref{}, driver.ErrUnsupported
	}
	if len(s.Args) > 0 && len(s.Command) == 0 {
		return driver.Ref{}, driver.ErrInvalid
	}
	if s.Workdir == "" {
		s.Workdir = driver.DefaultWorkdir
	}
	if _, err := workspacePath(s.Workdir); err != nil {
		return driver.Ref{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Mkdir(d.dir(s.ID), 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return driver.Ref{}, driver.ErrAlreadyExists
		}
		return driver.Ref{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(d.dir(s.ID))
		}
	}()
	if err := os.Mkdir(filepath.Join(d.dir(s.ID), "workspace"), 0700); err != nil {
		return driver.Ref{}, err
	}
	now := time.Now().UTC()
	r := record{State: driver.State{ID: s.ID, Name: s.Name, Owner: s.Owner, Phase: driver.Running, Isolation: driver.IsolationNone, Labels: maps.Clone(s.Labels), CreatedAt: now, StartedAt: now, LastActivityAt: now, AutoStop: s.Lifecycle.AutoStop, AutoDelete: s.Lifecycle.AutoDelete}, Env: maps.Clone(s.Env), Workdir: s.Workdir, Command: slices.Clone(s.Command), Args: slices.Clone(s.Args)}
	if s.Lifecycle.TTL > 0 {
		r.State.ExpiresAt = now.Add(s.Lifecycle.TTL)
	}
	if err := d.save(s.ID, r); err != nil {
		return driver.Ref{}, err
	}
	if len(r.Command) > 0 {
		if err := d.startMainLocked(ctx, s.ID, &r); err != nil {
			return driver.Ref{}, err
		}
	}
	ok = true
	return driver.Ref{ID: s.ID}, nil
}
func (d *Driver) Inspect(ctx context.Context, id string) (driver.State, error) {
	if err := ctx.Err(); err != nil {
		return driver.State{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err == nil {
		err = d.mainError(id)
	}
	return r.State, err
}
func (d *Driver) List(ctx context.Context, f driver.Filter) ([]driver.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	out := []driver.State{}
	for _, e := range entries {
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		r, err := d.load(e.Name())
		if err != nil {
			return nil, err
		}
		if err := d.mainError(e.Name()); err != nil {
			return nil, err
		}
		s := r.State
		if f.Owner != "" && s.Owner != f.Owner || f.Phase != "" && s.Phase != f.Phase || len(f.IDs) > 0 && !slices.Contains(f.IDs, s.ID) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
func (d *Driver) edit(ctx context.Context, id string, fn func(*record)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	fn(&r)
	return d.save(id, r)
}
func (d *Driver) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if err := d.mainError(id); err != nil {
		return err
	}
	if r.State.Phase == driver.Running {
		return nil
	}
	if r.State.Phase == "Lost" {
		return driver.ErrUnsupported
	}
	if len(r.Command) > 0 {
		return d.startMainLocked(ctx, id, &r)
	}
	r.State.Phase = driver.Running
	r.State.StartedAt = time.Now().UTC()
	r.State.LastActivityAt = r.State.StartedAt
	r.State.StoppedAt = time.Time{}
	return d.save(id, r)
}
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if r.State.Phase == "Lost" {
		return driver.ErrUnsupported
	}
	if main := d.mains[id]; main != nil {
		main.stopping = true
	}
	d.cancel(id)
	if r.State.Phase != driver.Stopped {
		r.State.Phase = driver.Stopped
		r.State.StoppedAt = time.Now().UTC()
	}
	if err := d.save(id, r); err != nil {
		return err
	}
	if main := d.mains[id]; main != nil && mainFinished(main) {
		delete(d.mains, id)
	}
	return nil
}
func (d *Driver) cancel(id string) {
	for e := range d.active[id] {
		_ = e.Close()
	}
	// Reapers signal completion before taking mu, so this is safe while the
	// lifecycle operation holds mu and prevents any new exec from starting.
	for e := range d.active[id] {
		<-e.done
	}
}
func (d *Driver) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.load(id); err != nil {
		return err
	}
	d.cancel(id)
	if err := os.RemoveAll(d.dir(id)); err != nil {
		return err
	}
	delete(d.mains, id)
	return nil
}
func (d *Driver) Touch(ctx context.Context, id string) error {
	return d.edit(ctx, id, func(r *record) { r.State.LastActivityAt = time.Now().UTC() })
}
func (d *Driver) Update(ctx context.Context, id string, c driver.Change) error {
	if c.Lifecycle != nil && (c.Lifecycle.TTL < 0 || c.Lifecycle.AutoStop < 0 || c.Lifecycle.AutoDelete < 0) {
		return driver.ErrInvalid
	}
	return d.edit(ctx, id, func(r *record) {
		if c.Labels != nil {
			r.State.Labels = maps.Clone(*c.Labels)
		}
		if c.Env != nil {
			r.Env = maps.Clone(*c.Env)
		}
		if c.Lifecycle != nil {
			r.State.AutoStop = c.Lifecycle.AutoStop
			r.State.AutoDelete = c.Lifecycle.AutoDelete
			r.State.ExpiresAt = time.Time{}
			if c.Lifecycle.TTL > 0 {
				r.State.ExpiresAt = r.State.CreatedAt.Add(c.Lifecycle.TTL)
			}
		}
	})
}

// Close cancels all executions and waits for main-process state to be saved.
func (d *Driver) Close() error {
	d.mu.Lock()
	for id := range d.active {
		if main := d.mains[id]; main != nil {
			main.stopping = true
		}
		d.cancel(id)
	}
	mains := make([]*mainProcess, 0, len(d.mains))
	for _, main := range d.mains {
		mains = append(mains, main)
	}
	d.mu.Unlock()
	var result error
	for _, main := range mains {
		<-main.done
		result = errors.Join(result, main.err)
	}
	return result
}
