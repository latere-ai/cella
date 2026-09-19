// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package controller coordinates durable desired state with one direct driver.
package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"math/big"
	"slices"
	"sort"
	"sync"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

var (
	ErrNotFound  = errors.New("sandbox not found")
	ErrNameTaken = errors.New("sandbox name is already in use")
	ErrQuota     = errors.New("sandbox count limit reached")
	ErrPhase     = errors.New("sandbox phase does not allow this operation")
)

type Options struct {
	DataDir     string
	Store       Store
	Driver      driver.Driver
	Environment string
}
type Controller struct {
	mu          sync.Mutex
	store       Store
	driver      driver.Driver
	environment string
	objects     map[string]v1.Sandbox
}

// Open restores desired state from an operator-supplied store, or the provisional
// single-process local file store when DataDir is set. Store is the seam for
// the transactional memory and Postgres adapters specified by design 010.
func Open(o Options) (*Controller, error) {
	if o.Driver == nil || o.Environment == "" || (o.Store == nil && o.DataDir == "") {
		return nil, errors.New("controller requires a driver, store or data directory, and environment")
	}
	store := o.Store
	var err error
	if store == nil {
		store, err = OpenFileStore(o.DataDir)
		if err != nil {
			return nil, err
		}
	}
	objects, err := store.Load()
	if err == nil {
		for id, obj := range objects {
			if id == "" || obj.Status.ID != id || obj.Status.Owner == "" || obj.Spec.Environment != o.Environment {
				err = errors.New("invalid controller snapshot object")
				break
			}
		}
	}
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if objects == nil {
		objects = map[string]v1.Sandbox{}
	}
	return &Controller{store: store, driver: o.Driver, environment: o.Environment, objects: objects}, nil
}
func (c *Controller) Close() error        { c.mu.Lock(); defer c.mu.Unlock(); return c.store.Close() }
func (c *Controller) Environment() string { return c.environment }
func (c *Controller) Isolation() string   { return c.driver.Isolation() }
func (c *Controller) save() error         { return c.store.Save(c.objects) }

// Create atomically reserves the owner's name and quota before calling runtime.
func (c *Controller) Create(ctx context.Context, obj v1.Sandbox, owner string, max int) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner == "" {
		return obj, errors.New("sandbox owner is required")
	}
	// The resolver's warnings say what this environment could not honour. They
	// are the caller's answer and outlive the status the controller writes.
	warnings := slices.Clone(obj.Status.Warnings)
	lifecycle, err := lifecycleOf(obj.Spec.Lifecycle)
	if err != nil {
		return obj, err
	}
	id, err := newID()
	if err != nil {
		return obj, err
	}
	if obj.Metadata.Name == "" {
		obj.Metadata.Name = "sandbox-" + id[len(id)-10:]
	}
	count := 0
	for _, v := range c.objects {
		if v.Status.Owner == owner {
			if v.Status.Phase != "Deleting" {
				count++
			}
			if v.Metadata.Name == obj.Metadata.Name {
				return obj, ErrNameTaken
			}
		}
	}
	if max > 0 && count >= max {
		return obj, ErrQuota
	}
	obj.Status = v1.SandboxStatus{ID: id, Owner: owner, Environment: c.environment, Driver: c.driver.Name(), Isolation: c.driver.Isolation(), Phase: driver.Pending, CreatedAt: time.Now().UTC(), Warnings: warnings}
	c.objects[id] = clone(obj)
	if err = c.save(); err != nil {
		delete(c.objects, id)
		return obj, err
	}
	_, err = c.driver.Create(ctx, driver.CreateSpec{
		ID: id, Name: obj.Metadata.Name, Owner: owner, Image: obj.Spec.Image,
		Command: obj.Spec.Command, Args: obj.Spec.Args, Env: obj.Spec.Env,
		Workdir: obj.Spec.Workdir, Labels: obj.Metadata.Labels, User: obj.Spec.User,
		Resources: driver.Resources{CPU: string(obj.Spec.Resources.CPU), Memory: string(obj.Spec.Resources.Memory), Disk: string(obj.Spec.Resources.Disk)},
		Workspace: driver.Workspace{Path: obj.Spec.Workspace.Path},
		Lifecycle: lifecycle,
	})
	if err != nil {
		obj.Status.Phase = "Failed"
		obj.Status.Reason = "RuntimeCreateFailed"
		c.objects[id] = clone(obj)
		return obj, errors.Join(err, c.save())
	}
	obj, err = c.refresh(ctx, obj)
	c.objects[id] = clone(obj)
	return clone(obj), errors.Join(err, c.save())
}
func (c *Controller) Get(ctx context.Context, key, owner string) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[key]
	if !ok {
		for _, v := range c.objects {
			if v.Status.Owner == owner && v.Metadata.Name == key {
				obj = v
				ok = true
				break
			}
		}
	}
	if !ok {
		return v1.Sandbox{}, ErrNotFound
	}
	return clone(obj), nil
}

// List returns desired records. Runtime refresh is deferred until after API authorization.
func (c *Controller) Refresh(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	return c.refresh(ctx, clone(obj))
}
func (c *Controller) List() []v1.Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]v1.Sandbox, 0, len(c.objects))
	for _, obj := range c.objects {
		out = append(out, clone(obj))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}
func (c *Controller) refresh(ctx context.Context, obj v1.Sandbox) (v1.Sandbox, error) {
	if obj.Status.Phase == "Deleting" {
		return obj, nil
	}
	state, err := c.driver.Inspect(ctx, obj.Status.ID)
	if errors.Is(err, driver.ErrNotFound) {
		if obj.Status.Phase != driver.Pending && obj.Status.Phase != "Failed" {
			obj.Status.Phase = "Lost"
		}
		return obj, nil
	}
	if err != nil {
		return obj, err
	}
	obj.Status.Phase = state.Phase
	obj.Status.Reason = state.Reason
	obj.Status.ExitCode = state.ExitCode
	obj.Status.StartedAt = state.StartedAt
	obj.Status.StoppedAt = state.StoppedAt
	obj.Status.LastActivityAt = state.LastActivityAt
	obj.Status.ExpiresAt = state.ExpiresAt
	return obj, nil
}

// lifecycleOf turns the resolved manifest's durations into the driver's, where
// never and absent are both the zero duration the drivers read as no bound.
func lifecycleOf(l v1.Lifecycle) (driver.Lifecycle, error) {
	var out driver.Lifecycle
	for _, field := range []struct {
		value v1.Duration
		into  *time.Duration
	}{{l.AutoStop, &out.AutoStop}, {l.TTL, &out.TTL}, {l.AutoDelete, &out.AutoDelete}} {
		if field.value == "" {
			continue
		}
		value, never, err := manifest.ParseDuration(field.value)
		if err != nil {
			return out, err
		}
		if !never {
			*field.into = value
		}
	}
	return out, nil
}

// Act serializes lifecycle changes; failed deletes retain the desired record.
func (c *Controller) Act(ctx context.Context, id, verb string) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, ok := c.objects[id]
	if !ok {
		return obj, ErrNotFound
	}
	var err error
	switch verb {
	case "start":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Stopped {
			return obj, ErrPhase
		}
		err = c.driver.Start(ctx, id)
	case "stop":
		obj, err = c.refresh(ctx, obj)
		if err != nil {
			return obj, err
		}
		if obj.Status.Phase != driver.Running {
			return obj, ErrPhase
		}
		err = c.driver.Stop(ctx, id)
	case "delete":
		old := obj
		obj.Status.Phase = "Deleting"
		c.objects[id] = clone(obj)
		if err = c.save(); err != nil {
			c.objects[id] = old
			return old, err
		}
		err = c.driver.Delete(ctx, id)
		if errors.Is(err, driver.ErrNotFound) {
			err = nil
		}
		if err == nil {
			delete(c.objects, id)
			if err = c.save(); err != nil {
				c.objects[id] = obj
			}
			return obj, err
		}
	default:
		return obj, ErrPhase
	}
	if err != nil {
		return obj, err
	}
	obj, err = c.refresh(ctx, obj)
	if err != nil {
		return obj, err
	}
	c.objects[id] = clone(obj)
	return clone(obj), c.save()
}
func (c *Controller) Exec(ctx context.Context, id string, req driver.ExecRequest) (driver.Exec, error) {
	return c.driver.Exec(ctx, id, req)
}
func clone(obj v1.Sandbox) v1.Sandbox {
	obj.Metadata.Labels = maps.Clone(obj.Metadata.Labels)
	obj.Metadata.Annotations = maps.Clone(obj.Metadata.Annotations)
	obj.Spec.Command = slices.Clone(obj.Spec.Command)
	obj.Spec.Args = slices.Clone(obj.Spec.Args)
	obj.Spec.Env = maps.Clone(obj.Spec.Env)
	obj.Status.Warnings = slices.Clone(obj.Status.Warnings)
	if obj.Status.ExitCode != nil {
		code := *obj.Status.ExitCode
		obj.Status.ExitCode = &code
	}
	return obj
}
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	n := new(big.Int).SetBytes(b[:])
	mask := big.NewInt(31)
	var out [26]byte
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return "sbx_" + string(out[:]), nil
}
