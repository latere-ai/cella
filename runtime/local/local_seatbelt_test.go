// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package local

import (
	"context"
	"testing"

	"latere.ai/x/pkg/hostsandbox"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// TestLocalConformanceUnderSeatbelt is the same suite under macOS's own
// Seatbelt through hostsandbox's native driver, which needs nothing
// installed: every command of every case runs inside a profile the driver
// renders. The file builds on macOS alone, so a Linux run, which fails on
// any skipped local test, never sees it.
func TestLocalConformanceUnderSeatbelt(t *testing.T) {
	withIsolation(t, hostsandbox.IsolationSeatbelt)
	probe, err := New(Options{DataDir: tempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Preflight(context.Background()); err != nil {
		t.Fatalf("Seatbelt is not ready on this Mac: %v", err)
	}
	data := tempDir(t)
	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		d, err := New(Options{DataDir: data})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}, runtimetest.Options{})
}
