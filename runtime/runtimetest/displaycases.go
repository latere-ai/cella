// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/jpeg" // the second encoding a screenshot may be asked for
	_ "image/png"  // the encoding a screenshot is asked for by default
	"slices"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// desktopTimeout bounds how long a desktop has to come up. It is longer than
// the suite's other waits because an X server and a window manager are two
// processes starting inside a container that may still be warming.
var desktopTimeout = 90 * time.Second

// conformanceGeometry is the desktop every display case asks for. It is not
// the contract's default: a case that asserts the frame is the declared size
// proves nothing if the size is whatever the driver would have chosen.
var conformanceGeometry = runtime.Geometry{Width: 800, Height: 600}

// desktop creates a sandbox with a screen and waits for the driver to report
// it ready. It skips the case where the suite was given no image carrying the
// desktop's tools, naming that as the reason.
func desktop(t tb, d runtime.Driver, opts Options, id string) runtime.State {
	t.Helper()
	if opts.DisplayImage == "" {
		t.Skipf("Options.DisplayImage is empty: this suite has no image carrying a desktop")
	}
	spec := runtime.CreateSpec{ID: id, Name: "desktop", Owner: "alice", Display: &conformanceGeometry,
		Image: opts.DisplayImage}
	ref, err := d.Create(context.Background(), spec)
	must(t, err, "Create "+id)
	t.Cleanup(func() { _ = d.Delete(context.Background(), id) })
	need(t, ref.ID == id, "Create returned id %q, want %q", ref.ID, id)
	waitPhase(t, d, id, runtime.Running)
	return waitDisplayReady(t, d, id)
}

// waitDisplayReady polls until the driver writes DisplayReady true.
func waitDisplayReady(t tb, d runtime.Driver, id string) runtime.State {
	t.Helper()
	deadline := time.Now().Add(desktopTimeout)
	for {
		state, err := d.Inspect(context.Background(), id)
		must(t, err, "Inspect "+id)
		for _, condition := range state.Conditions {
			if condition.Type == v1.ConditionDisplayReady && condition.Status == v1.ConditionTrue {
				return state
			}
		}
		need(t, time.Now().Before(deadline), "%s carries no DisplayReady after %s: %+v", id, desktopTimeout, state.Conditions)
		time.Sleep(200 * time.Millisecond)
	}
}

// displayScreenshot is the Display capability's contract: a sandbox that asked
// for a screen reports that geometry, reaches DisplayReady, and answers a
// frame of exactly that size in the encoding the request named.
func displayScreenshot(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Display {
		t.Skipf("the driver declares no Display")
	}
	screen, ok := d.(runtime.DisplayDriver)
	need(t, ok, "the driver declares Display and does not implement runtime.DisplayDriver")
	ctx := context.Background()
	const id = "sbx_cnf_display"
	desktop(t, d, opts, id)

	geometry, err := screen.Display(ctx, id)
	must(t, err, "Display")
	expect(t, geometry == conformanceGeometry, "Display = %+v, want %+v", geometry, conformanceGeometry)

	frame, err := screen.Screenshot(ctx, id, runtime.ScreenshotRequest{})
	must(t, err, "Screenshot")
	defer func() { _ = frame.Close() }()
	config, format, err := image.DecodeConfig(frame)
	must(t, err, "decoding the frame")
	expect(t, format == "png", "the default encoding is %q, want png", format)
	expect(t, config.Width == conformanceGeometry.Width && config.Height == conformanceGeometry.Height,
		"the frame is %dx%d, want the declared %dx%d", config.Width, config.Height,
		conformanceGeometry.Width, conformanceGeometry.Height)

	// The request's own encoding and scale reach the capture.
	half, err := screen.Screenshot(ctx, id, runtime.ScreenshotRequest{Format: display.FormatJPEG, Scale: 0.5})
	must(t, err, "Screenshot at half scale")
	defer func() { _ = half.Close() }()
	config, format, err = image.DecodeConfig(half)
	must(t, err, "decoding the half-scale frame")
	expect(t, format == "jpeg", "the requested encoding is %q, want jpeg", format)
	expect(t, config.Width == conformanceGeometry.Width/2,
		"the half-scale frame is %d wide, want %d", config.Width, conformanceGeometry.Width/2)

	// A sandbox that asked for no screen is not a sandbox with one.
	const plain = "sbx_cnf_noscreen"
	create(t, d, opts, runtime.CreateSpec{ID: plain, Name: "plain", Owner: "alice"})
	_, err = screen.Display(ctx, plain)
	wantErr(t, err, runtime.ErrNotFound, "Display on a sandbox with no screen")
}

// inputAcceptsAndRefuses is the Input capability's contract: a gesture inside
// the desktop lands, and one outside it is refused whole, before anything runs.
func inputAcceptsAndRefuses(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Input {
		t.Skipf("the driver declares no Input")
	}
	pointer, ok := d.(runtime.InputDriver)
	need(t, ok, "the driver declares Input and does not implement runtime.InputDriver")
	ctx := context.Background()
	const id = "sbx_cnf_input"
	desktop(t, d, opts, id)

	inside := 10
	must(t, pointer.Input(ctx, id, []runtime.InputEvent{
		{Type: display.TypeMove, X: &inside, Y: &inside},
		{Type: display.TypeClick, Button: "left"},
		{Type: display.TypeKey, Key: "Escape"},
		{Type: display.TypeType, Text: "conformance"},
	}), "a batch inside the desktop")

	outside := conformanceGeometry.Width + 1
	err := pointer.Input(ctx, id, []runtime.InputEvent{{Type: display.TypeClick, Button: "left", X: &outside, Y: &inside}})
	wantErr(t, err, runtime.ErrInvalid, "a click outside the desktop")
	var invalid *display.Invalid
	if errors.As(err, &invalid) {
		expect(t, slices.Contains(invalid.Paths, "events[0].x"),
			"the refusal names %v, want events[0].x", invalid.Paths)
	} else {
		t.Errorf("the refusal is %v, want one that names the field", err)
	}
	// An event the contract does not have is refused as well, so a driver
	// cannot quietly accept a vocabulary of its own.
	wantErr(t, pointer.Input(ctx, id, []runtime.InputEvent{{Type: "teleport"}}), runtime.ErrInvalid, "an event outside the vocabulary")
}

// portsReportListening is the probe of spec 023: a declared port something
// inside the sandbox holds reads listening, and one nothing holds reads closed.
func portsReportListening(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if opts.Listen == nil {
		t.Skipf("Options.Listen is nil: this suite has no command that binds a port")
	}
	const id = "sbx_cnf_ports"
	const bound, idle = 18080, 18081
	spec := runtime.CreateSpec{
		ID: id, Name: "ports", Owner: "alice",
		Command: opts.Listen(bound),
		Ports:   []runtime.Port{{Name: "bound", Port: bound}, {Name: "idle", Port: idle}},
	}
	create(t, d, opts, spec)

	deadline := time.Now().Add(pollTimeout)
	for {
		state, err := d.Inspect(context.Background(), id)
		must(t, err, "Inspect "+id)
		need(t, len(state.Ports) == 2, "the state reports %d ports, want the two declared: %+v", len(state.Ports), state.Ports)
		expect(t, state.Ports[0].Name == "bound" && state.Ports[1].Name == "idle",
			"the ports are reported as %+v, want them in the order declared", state.Ports)
		expect(t, state.Ports[1].State == runtime.PortClosed,
			"a port nothing holds reads %q", state.Ports[1].State)
		if state.Ports[0].State == runtime.PortListening {
			return
		}
		need(t, time.Now().Before(deadline), "the bound port stayed %q after %s", state.Ports[0].State, pollTimeout)
		time.Sleep(100 * time.Millisecond)
	}
}

// screenStream is the paced sequence: a frame arrives, and cancelling the
// caller's context ends the session.
func screenStream(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Display {
		t.Skipf("the driver declares no Display")
	}
	screen := d.(runtime.DisplayDriver)
	const id = "sbx_cnf_screen"
	desktop(t, d, opts, id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := screen.Screen(ctx, id, 4, display.FormatPNG)
	must(t, err, "Screen")
	select {
	case frame, ok := <-frames:
		need(t, ok, "the session closed before its first frame")
		config, format, err := image.DecodeConfig(bytes.NewReader(frame.Data))
		must(t, err, "decoding a streamed frame")
		expect(t, format == "png", "a streamed frame is %q", format)
		expect(t, config.Width == conformanceGeometry.Width, "a streamed frame is %d wide", config.Width)
	case <-time.After(desktopTimeout):
		t.Fatalf("no frame arrived within %s", desktopTimeout)
	}
	cancel()
	deadline := time.After(desktopTimeout)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("the session did not end within %s of its context", desktopTimeout)
		}
	}
}
