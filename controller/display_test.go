// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// desktopDriver is a driver that answers the computer-use operations and
// reports a desktop and a port at every read.
type desktopDriver struct {
	driver.Driver
	events []driver.InputEvent
	fail   error
}

func (d *desktopDriver) Capabilities() driver.Capabilities {
	c := d.Driver.Capabilities()
	c.Display, c.Input = true, true
	return c
}

func (d *desktopDriver) Display(context.Context, string) (driver.Geometry, error) {
	return driver.Geometry{Width: 1280, Height: 800}, nil
}

func (d *desktopDriver) Screenshot(context.Context, string, driver.ScreenshotRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("PNG")), d.fail
}

func (d *desktopDriver) Screen(context.Context, string, int, string) (<-chan driver.Frame, error) {
	frames := make(chan driver.Frame, 1)
	frames <- driver.Frame{Format: "png", Data: []byte("PNG")}
	close(frames)
	return frames, d.fail
}

func (d *desktopDriver) Input(_ context.Context, _ string, events []driver.InputEvent) error {
	d.events = events
	return d.fail
}

func (d *desktopDriver) Inspect(ctx context.Context, id string) (driver.State, error) {
	state, err := d.Driver.Inspect(ctx, id)
	if err != nil {
		return state, err
	}
	state.Conditions = []v1.Condition{{Type: v1.ConditionDisplayReady, Status: v1.ConditionTrue, Reason: "DesktopUp"}}
	state.Ports = []driver.PortState{{Name: "web", Port: 8080, State: driver.PortListening}}
	return state, nil
}

// TestDisplayOperationsReachTheDriver holds the four pass-throughs to the one
// rule they share: a driver that declares the capability is reached, and one
// that does not is the unsupported operation the API turns into 422.
func TestDisplayOperationsReachTheDriver(t *testing.T) {
	c, _ := newController(t)
	plain := c.driver
	if _, err := c.Display(t.Context(), "sbx_x"); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Display on a driver with no desktop: %v", err)
	}
	if _, err := c.Screenshot(t.Context(), "sbx_x", driver.ScreenshotRequest{}); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Screenshot on a driver with no desktop: %v", err)
	}
	if _, err := c.Screen(t.Context(), "sbx_x", 5, "png"); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Screen on a driver with no desktop: %v", err)
	}
	if err := c.Input(t.Context(), "sbx_x", nil); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Input on a driver with no desktop: %v", err)
	}

	desk := &desktopDriver{Driver: plain}
	c.driver = desk
	geometry, err := c.Display(t.Context(), "sbx_x")
	if err != nil || geometry != (driver.Geometry{Width: 1280, Height: 800}) {
		t.Fatalf("Display = %+v, %v", geometry, err)
	}
	shot, err := c.Screenshot(t.Context(), "sbx_x", driver.ScreenshotRequest{})
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	body, _ := io.ReadAll(shot)
	if string(body) != "PNG" {
		t.Fatalf("the frame is %q", body)
	}
	frames, err := c.Screen(t.Context(), "sbx_x", 5, "png")
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if frame := <-frames; string(frame.Data) != "PNG" {
		t.Fatalf("the first frame is %q", frame.Data)
	}
	batch := []driver.InputEvent{{Type: "click", Button: "left"}}
	if err = c.Input(t.Context(), "sbx_x", batch); err != nil || len(desk.events) != 1 {
		t.Fatalf("Input: %v, the driver saw %d events", err, len(desk.events))
	}
}

// TestRefreshCarriesDisplayAndPorts holds the read path: the driver owns the
// conditions and the probe, and a read of the sandbox carries both.
func TestRefreshCarriesDisplayAndPorts(t *testing.T) {
	c, _ := newController(t)
	obj := workspace()
	obj.Spec.Display = &v1.Display{Width: 1280, Height: 800}
	obj.Spec.Network.Ports = []v1.Port{{Name: "web", Port: 8080}}
	recorder := &recordingDriver{Driver: &desktopDriver{Driver: c.driver}}
	c.driver = recorder
	created, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest's desktop and ports reach the driver as the contract's own
	// types, so a driver reads one shape whatever the manifest spelled.
	if recorder.spec.Display == nil || *recorder.spec.Display != (driver.Geometry{Width: 1280, Height: 800}) {
		t.Fatalf("the create spec's desktop is %+v", recorder.spec.Display)
	}
	if len(recorder.spec.Ports) != 1 || recorder.spec.Ports[0] != (driver.Port{Name: "web", Port: 8080}) {
		t.Fatalf("the create spec's ports are %+v", recorder.spec.Ports)
	}
	got, err := c.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var ready *v1.Condition
	for i, condition := range got.Status.Conditions {
		if condition.Type == v1.ConditionDisplayReady {
			ready = &got.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != v1.ConditionTrue || ready.Reason != "DesktopUp" {
		t.Fatalf("the conditions are %+v, want DisplayReady true", got.Status.Conditions)
	}
	if len(got.Status.Ports) != 1 || got.Status.Ports[0] != (v1.PortStatus{Name: "web", Port: 8080, State: v1.PortListening}) {
		t.Fatalf("the ports are %+v", got.Status.Ports)
	}
	// A second read replaces the condition rather than appending a copy.
	if got, err = c.Get(t.Context(), created.Status.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, condition := range got.Status.Conditions {
		if condition.Type == v1.ConditionDisplayReady {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("DisplayReady appears %d times", count)
	}
}

// TestSandboxWithoutADesktopCarriesNoCondition holds the other half: a
// manifest with no display reaches the driver with none and reads back with
// neither the condition nor a port.
func TestSandboxWithoutADesktopCarriesNoCondition(t *testing.T) {
	c, _ := newController(t)
	recorder := &recordingDriver{Driver: c.driver}
	c.driver = recorder
	created, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.spec.Display != nil || recorder.spec.Ports != nil {
		t.Fatalf("the create spec carries %+v and %+v", recorder.spec.Display, recorder.spec.Ports)
	}
	got, err := c.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, condition := range got.Status.Conditions {
		if condition.Type == v1.ConditionDisplayReady {
			t.Fatalf("a sandbox with no desktop carries %+v", condition)
		}
	}
	if got.Status.Ports != nil {
		t.Fatalf("a sandbox with no ports carries %+v", got.Status.Ports)
	}
}
