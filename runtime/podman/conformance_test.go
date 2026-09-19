// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"os"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// conformanceImage is the base image the suite's sandboxes run. It is a
// published image name, not a coordinate of any installation, and an operator
// running the suite elsewhere overrides it.
const conformanceImage = "docker.io/library/alpine:latest"

// TestPodmanConformance runs the shared driver contract against a real engine.
// It skips, naming every socket it tried, where none answers, so a machine
// with no podman still runs the suite.
func TestPodmanConformance(t *testing.T) {
	d, err := New(Options{Socket: os.Getenv("CELLA_PODMAN_SOCKET")})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(t.Context()); err != nil {
		t.Skipf("no podman engine for the conformance suite: %v", err)
	}
	open := func(t *testing.T) driver.Driver {
		d, err := New(Options{Socket: d.Socket()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	runtimetest.Run(t, open, runtimetest.Options{Image: conformanceImage})
}
