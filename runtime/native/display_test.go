// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestNativeHasNoDisplay states what this driver is: host processes under the
// control plane's own user, with no display server, no window manager and no
// network namespace of their own. There is nothing to attach a screen to and
// nothing to confine a port inside, so the driver declares Display, Input and
// Dial false and implements none of the three interfaces. The conformance
// suite checks the same in both directions; this case states it where a reader
// of the package looks for it.
func TestNativeHasNoDisplay(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := d.Capabilities()
	if c.Display || c.Input || c.Dial {
		t.Fatalf("the native driver declares %+v", c)
	}
	if _, ok := any(d).(driver.DisplayDriver); ok {
		t.Error("the native driver implements runtime.DisplayDriver")
	}
	if _, ok := any(d).(driver.InputDriver); ok {
		t.Error("the native driver implements runtime.InputDriver")
	}
	if _, ok := any(d).(driver.Dialer); ok {
		t.Error("the native driver implements runtime.Dialer")
	}
	// A sandbox the native driver runs reports no port state either: nothing
	// probes a process that shares the host's network.
	if _, err = d.Create(t.Context(), driver.CreateSpec{ID: "sbx_display", Name: "n", Owner: "alice",
		Ports: []driver.Port{{Name: "web", Port: 8080}}}); err != nil {
		t.Fatal(err)
	}
	state, err := d.Inspect(t.Context(), "sbx_display")
	if err != nil {
		t.Fatal(err)
	}
	if state.Ports != nil || state.Conditions != nil {
		t.Fatalf("the native driver reports %+v and %+v", state.Ports, state.Conditions)
	}
}
