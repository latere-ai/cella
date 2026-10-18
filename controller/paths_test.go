// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestObserveCarriesTheScratchPaths: the scratch directories a driver reports
// reach status.paths as a copy of their own, and a driver that reports none
// leaves the field empty.
func TestObserveCarriesTheScratchPaths(t *testing.T) {
	reported := &v1.SandboxPaths{Home: "/data/sandboxes/sbx_1/home", Tmp: "/data/sandboxes/sbx_1/tmp"}
	obj := observe(v1.Sandbox{}, driver.State{Phase: driver.Running, Paths: reported})
	if obj.Status.Paths == nil || *obj.Status.Paths != *reported {
		t.Fatalf("status.paths reads %+v, want %+v", obj.Status.Paths, reported)
	}
	reported.Home = "/elsewhere"
	if obj.Status.Paths.Home == "/elsewhere" {
		t.Error("status.paths shares the driver's value")
	}
	if obj = observe(v1.Sandbox{}, driver.State{Phase: driver.Running}); obj.Status.Paths != nil {
		t.Errorf("a driver that reports no paths left %+v", obj.Status.Paths)
	}
}
