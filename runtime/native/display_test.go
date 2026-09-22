// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestNativeHasNoDisplay states what this driver is: host processes under the
// control plane's own user, with no display server and no window manager.
// There is nothing to attach a screen to, so the driver declares Display and
// Input false and implements neither interface. The conformance suite checks
// the same in both directions; this case states it where a reader of the
// package looks for it.
func TestNativeHasNoDisplay(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := d.Capabilities()
	if c.Display || c.Input {
		t.Fatalf("the native driver declares %+v", c)
	}
	if _, ok := any(d).(driver.DisplayDriver); ok {
		t.Error("the native driver implements runtime.DisplayDriver")
	}
	if _, ok := any(d).(driver.InputDriver); ok {
		t.Error("the native driver implements runtime.InputDriver")
	}
	if _, err = d.Create(t.Context(), driver.CreateSpec{ID: "sbx_display", Name: "n", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	state, err := d.Inspect(t.Context(), "sbx_display")
	if err != nil {
		t.Fatal(err)
	}
	if state.Conditions != nil {
		t.Fatalf("the native driver reports %+v", state.Conditions)
	}
}
