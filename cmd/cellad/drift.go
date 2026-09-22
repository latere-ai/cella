// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// driftDefault is the test seam of spec 015 applied to a resolve: before the
// admission step reads the defaulted object, the field CELLA_TEST_DRIFT_DEFAULT
// names is moved from its literal default of 0 to 1, so the admission step and
// every later stage read the drifted value as the defaulting stage's output. A
// field the manifest already moved off 0 is left as it is. With no field named
// it returns next unchanged, so a deployment's resolve is exactly the one it
// would be without this function.
//
// It lives in the command and not in the manifest package, so the package a
// platform composes carries no test seam.
func driftDefault(field string, next manifest.AdmitFunc) manifest.AdmitFunc {
	if field == "" {
		return next
	}
	return func(ctx context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
		spawn := &in.Spec.Mesh.Spawn
		switch {
		case field == config.DriftSpawnBudget && spawn.Budget == 0:
			spawn.Budget = 1
		case field == config.DriftSpawnDepth && spawn.Depth == 0:
			spawn.Depth = 1
		}
		if next == nil {
			return in, nil, nil
		}
		return next(ctx, in, req)
	}
}
