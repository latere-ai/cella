// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"flag"
	"slices"
	"testing"

	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/device"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/")

// TestFormalModels holds formal/, which the lean and tla gates check, to
// every lifecycle table of the module.
func TestFormalModels(t *testing.T) {
	statemachinetest.Models(t, "../../formal", *update,
		controller.SandboxLifecycle.Graph(),
		controller.VolumeLifecycle.Graph(),
		controller.EnvironmentLifecycle.Graph(),
		store.OperationTable.Graph(),
		store.DeliveryTable.Graph(),
		store.KeyTable.Graph(),
		store.RowTable.Graph(),
		device.EngineTable.Graph(),
	)
}

// TestEveryPhaseIsInItsLifecycle keeps the phases of a kind to the phases of
// its table, so a phase added to one is added to the other. Degraded is a
// phase of the contract that nothing writes.
func TestEveryPhaseIsInItsLifecycle(t *testing.T) {
	sandbox := []v1.SandboxPhase{v1.SandboxPending, v1.SandboxQueued, v1.SandboxStarting, v1.SandboxRunning,
		v1.SandboxStopping, v1.SandboxStopped, v1.SandboxFailed, v1.SandboxLost, v1.SandboxRecovering, v1.SandboxDeleting}
	volume := []v1.VolumePhase{v1.VolumePending, v1.VolumeAvailable, v1.VolumeFailed, v1.VolumeDeleting, v1.VolumeLost}
	environment := []v1.EnvironmentPhase{v1.EnvironmentPending, v1.EnvironmentReady, v1.EnvironmentOffline}
	if got := controller.SandboxLifecycle.States(); !sameSet(got, sandbox) {
		t.Errorf("SandboxLifecycle has %v, want %v", got, sandbox)
	}
	if got := controller.VolumeLifecycle.States(); !sameSet(got, volume) {
		t.Errorf("VolumeLifecycle has %v, want %v", got, volume)
	}
	if got := controller.EnvironmentLifecycle.States(); !sameSet(got, environment) {
		t.Errorf("EnvironmentLifecycle has %v, want %v", got, environment)
	}
}

func sameSet[S ~string](a, b []S) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
