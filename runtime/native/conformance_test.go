// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"net"
	"os"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// TestNativeConformance runs the shared driver contract over a per-case root.
// Down removes the root so Ready fails; Up recreates it.
func TestNativeConformance(t *testing.T) {
	var root string
	open := func(t *testing.T) driver.Driver {
		root = t.TempDir()
		d, err := New(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	runtimetest.Run(t, open, runtimetest.Options{
		Down: func() { _ = os.RemoveAll(root) },
		Up:   func() { _ = os.MkdirAll(root, 0700) },
		// The test binary serves the port, so the cases need no tool of the
		// host's.
		Listen: echoCommand,
		Echo:   echoCommand,
	})
}

// TestConformanceWhileAnotherRunHoldsPorts: native sandboxes share the host's
// network, so the port cases meet every other test the machine runs at once.
// With the ports another run holds taken, and the three ports the suite once
// named held here, the port and dial cases still pass, because each picks
// free ports when it runs.
func TestConformanceWhileAnotherRunHoldsPorts(t *testing.T) {
	for _, port := range []string{"18080", "18081", "18090"} {
		// A port already held by something else is the same condition.
		if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); err == nil {
			t.Cleanup(func() { _ = l.Close() })
		}
	}
	TestNativeConformance(t)
}
