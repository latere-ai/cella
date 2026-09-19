// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
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
	})
}
