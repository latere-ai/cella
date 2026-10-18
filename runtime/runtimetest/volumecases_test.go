// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime"
)

// volumeCaseNames are the volume cases of the registry.
var volumeCaseNames = []string{"VolumeLifecycle", "AttachFailureIsClean"}

// volumeCase returns a volume case by name, so a rename is an immediate test
// failure rather than a silently skipped assertion.
func volumeCase(t *testing.T, name string) caseDef {
	t.Helper()
	return caseNamed(t, name)
}

// TestVolumeCasesPassOnAKeeper: a driver that keeps volumes passes both
// cases, the half that needs an image skipped where the suite runs none.
func TestVolumeCasesPassOnAKeeper(t *testing.T) {
	for _, name := range volumeCaseNames {
		t.Run(name, func(t *testing.T) {
			r := drive(volumeCase(t, name), opener(NewVolumes(openNative(t))), Options{})
			if r.failed() || r.skipped() != "" {
				t.Fatalf("a driver that keeps volumes did not pass:\n%s", r.report())
			}
		})
	}
}

// TestVolumeCasesSkipWithoutTheCapability: a driver that declares no Volumes
// is skipped by name, and one that declares it without the interface fails.
func TestVolumeCasesSkipWithoutTheCapability(t *testing.T) {
	for _, name := range volumeCaseNames {
		c := volumeCase(t, name)
		r := drive(c, opener(openNative(t)), Options{})
		if !strings.Contains(r.skipped(), "declares no Volumes") || r.failed() {
			t.Errorf("%s over a driver without volumes: %s", c.name, r.report())
		}
		r = drive(c, opener(volumesLiar{openNative(t)}), Options{})
		if !r.failed() {
			t.Errorf("%s passed a driver that declares Volumes and keeps none", c.name)
		}
	}
}

// volumesLiar declares Volumes and implements none of it.
type volumesLiar struct{ runtime.Driver }

func (volumesLiar) Capabilities() runtime.Capabilities { return runtime.Capabilities{Volumes: true} }

// forgetfulKeeper answers a second create of a volume it holds as if it made
// it again, which the contract's idempotence forbids.
type forgetfulKeeper struct{ *Volumes }

func (k forgetfulKeeper) CreateVolume(ctx context.Context, spec runtime.VolumeSpec) (runtime.Ref, error) {
	ref, err := k.Volumes.CreateVolume(ctx, spec)
	if errors.Is(err, runtime.ErrAlreadyExists) {
		return runtime.Ref{ID: spec.ID}, nil
	}
	return ref, err
}

// leakyAttacher makes the sandbox and then refuses the mount, leaving behind
// the object a refused attach must not.
type leakyAttacher struct{ *Volumes }

func (k leakyAttacher) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	s.Volumes = nil
	if _, err := k.Driver.Create(ctx, s); err != nil {
		return runtime.Ref{}, err
	}
	return runtime.Ref{}, runtime.ErrInvalid
}

// TestVolumeCasesCatchWhatTheyHoldTo: a second create that is not
// ErrAlreadyExists fails VolumeLifecycle, and a refused attach that leaves a
// sandbox fails AttachFailureIsClean.
func TestVolumeCasesCatchWhatTheyHoldTo(t *testing.T) {
	r := drive(volumeCase(t, "VolumeLifecycle"), opener(forgetfulKeeper{NewVolumes(openNative(t))}), Options{})
	if !strings.Contains(r.report(), "a second CreateVolume") {
		t.Errorf("a create that is not idempotent passed:\n%s", r.report())
	}
	r = drive(volumeCase(t, "AttachFailureIsClean"), opener(leakyAttacher{NewVolumes(openNative(t))}), Options{})
	if !strings.Contains(r.report(), "after a refused attach") {
		t.Errorf("a refused attach that left a sandbox passed:\n%s", r.report())
	}
}
