// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"latere.ai/x/cella/runtime"
)

// Volumes wraps a driver with volumes kept in memory, for the tests of a
// control plane over a driver that keeps none. It declares Volumes and
// VolumeImages, records the mounts each sandbox was created with, refuses a
// create that mounts a volume it does not hold Available, and hands the
// wrapped driver the spec without its mounts.
//
// A create fills at once unless Fill is set, which decides the state a create
// leaves, or the error it returns, so a test drives a fill that is still
// running, one that failed, and one that failed before it returned.
type Volumes struct {
	runtime.Driver
	// Fill is what CreateVolume leaves for one spec. Nil leaves the volume
	// Available at its size.
	Fill func(runtime.VolumeSpec) (runtime.VolumeState, error)

	mu      sync.Mutex
	volumes map[string]runtime.VolumeState
	specs   map[string]runtime.VolumeSpec
	mounts  map[string][]runtime.Mount
	creates map[string]int
}

var _ runtime.VolumeDriver = (*Volumes)(nil)

// NewVolumes wraps d.
func NewVolumes(d runtime.Driver) *Volumes {
	return &Volumes{
		Driver: d, volumes: map[string]runtime.VolumeState{}, specs: map[string]runtime.VolumeSpec{},
		mounts: map[string][]runtime.Mount{}, creates: map[string]int{},
	}
}

// Capabilities are the wrapped driver's with volumes kept and filled from an
// image.
func (v *Volumes) Capabilities() runtime.Capabilities {
	caps := v.Driver.Capabilities()
	caps.Volumes, caps.VolumeImages = true, true
	return caps
}

// Create records the mounts and creates the sandbox without them. A mount of
// a volume that is not Available is ErrInvalid, which is what a driver that
// cannot find the volume answers.
func (v *Volumes) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	v.mu.Lock()
	for _, m := range s.Volumes {
		if state, held := v.volumes[m.VolumeID]; !held || state.Phase != runtime.VolumeAvailable {
			v.mu.Unlock()
			return runtime.Ref{}, fmt.Errorf("%w: the volume %s is not available", runtime.ErrInvalid, m.VolumeID)
		}
	}
	if len(s.Volumes) > 0 {
		v.mounts[s.ID] = slices.Clone(s.Volumes)
	}
	v.mu.Unlock()
	s.Volumes = nil
	return v.Driver.Create(ctx, s)
}

// CreateVolume makes the volume and applies Fill, or fills it at once. A
// volume it already holds is ErrAlreadyExists.
func (v *Volumes) CreateVolume(_ context.Context, spec runtime.VolumeSpec) (runtime.Ref, error) {
	if err := spec.Check(); err != nil {
		return runtime.Ref{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.creates[spec.ID]++
	if _, held := v.volumes[spec.ID]; held {
		return runtime.Ref{}, runtime.ErrAlreadyExists
	}
	state := runtime.VolumeState{ID: spec.ID, Phase: runtime.VolumeAvailable, Capacity: spec.Size}
	if v.Fill != nil {
		next, err := v.Fill(spec)
		if err != nil {
			return runtime.Ref{}, err
		}
		state = next
		state.ID = spec.ID
	}
	v.volumes[spec.ID] = state
	v.specs[spec.ID] = spec
	return runtime.Ref{ID: spec.ID}, nil
}

// InspectVolume reports one volume.
func (v *Volumes) InspectVolume(_ context.Context, id string) (runtime.VolumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	state, held := v.volumes[id]
	if !held {
		return runtime.VolumeState{}, runtime.ErrNotFound
	}
	return state, nil
}

// DeleteVolume forgets one volume. One already gone is not an error.
func (v *Volumes) DeleteVolume(_ context.Context, id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.volumes, id)
	delete(v.specs, id)
	return nil
}

// Set moves a volume to a state, which is a fill that settled while a test
// watched.
func (v *Volumes) Set(state runtime.VolumeState) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.volumes[state.ID] = state
}

// Lose forgets a volume as a data plane that lost it does.
func (v *Volumes) Lose(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.volumes, id)
}

// Held reports the volumes this driver holds, by id.
func (v *Volumes) Held() map[string]runtime.VolumeState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.volumes)
}

// Spec is the spec one volume was created from.
func (v *Volumes) Spec(id string) (runtime.VolumeSpec, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	spec, held := v.specs[id]
	return spec, held
}

// Mounts are the mounts one sandbox was created with.
func (v *Volumes) Mounts(id string) []runtime.Mount {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.mounts[id])
}

// Creates is how many times CreateVolume was asked for one volume.
func (v *Volumes) Creates(id string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.creates[id]
}
