// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// fakeDesktop is a driver with a screen: it renders a frame of the geometry it
// was created with, validates a batch against it, and probes a port it was
// told is bound. It is what the display cases are driven against, so the suite
// proves it passes a conforming driver and fails each way one can fail.
type fakeDesktop struct {
	Nop
	mu sync.Mutex
	// boxes is every sandbox this driver holds, by id.
	boxes map[string]runtime.CreateSpec
	// bound is the port the fake reports as held, and 0 reports none.
	bound int
	// wrongSize renders a frame of a size nobody asked for.
	wrongSize bool
	// acceptsAnything skips validation, which is what a driver with a
	// vocabulary of its own would do.
	acceptsAnything bool
	// notReady leaves DisplayReady off the state.
	notReady bool
	// batches is every batch the driver was given.
	batches [][]runtime.InputEvent
}

func newFakeDesktop() *fakeDesktop {
	return &fakeDesktop{boxes: map[string]runtime.CreateSpec{}}
}

func (d *fakeDesktop) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Display: true, Input: true}
}

func (d *fakeDesktop) Create(_ context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.boxes[s.ID] = s
	return runtime.Ref{ID: s.ID}, nil
}

func (d *fakeDesktop) Delete(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.boxes, id)
	return nil
}

func (d *fakeDesktop) spec(id string) (runtime.CreateSpec, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.boxes[id]
	return s, ok
}

func (d *fakeDesktop) Inspect(_ context.Context, id string) (runtime.State, error) {
	s, ok := d.spec(id)
	if !ok {
		return runtime.State{}, runtime.ErrNotFound
	}
	state := runtime.State{ID: id, Name: s.Name, Owner: s.Owner, Phase: runtime.Running, Isolation: runtime.IsolationNone}
	if s.Display != nil && !d.notReady {
		state.Conditions = []v1.Condition{{Type: v1.ConditionDisplayReady, Status: v1.ConditionTrue}}
	}
	for _, p := range s.Ports {
		port := runtime.PortState{Name: p.Name, Port: p.Port, State: runtime.PortClosed}
		if p.Port == d.bound {
			port.State = runtime.PortListening
		}
		state.Ports = append(state.Ports, port)
	}
	return state, nil
}

func (d *fakeDesktop) Display(_ context.Context, id string) (runtime.Geometry, error) {
	s, ok := d.spec(id)
	if !ok || s.Display == nil {
		return runtime.Geometry{}, runtime.ErrNotFound
	}
	return *s.Display, nil
}

func (d *fakeDesktop) Screenshot(_ context.Context, id string, req runtime.ScreenshotRequest) (io.ReadCloser, error) {
	s, ok := d.spec(id)
	if !ok || s.Display == nil {
		return nil, runtime.ErrNotFound
	}
	req, err := req.Normalize()
	if err != nil {
		return nil, err
	}
	g := *s.Display
	if d.wrongSize {
		g.Width, g.Height = 7, 7
	}
	return io.NopCloser(bytes.NewReader(frameOf(g, req))), nil
}

// frameOf renders one frame of the geometry, scaled and encoded as the request
// asks, which is what a real capture tool produces.
func frameOf(g runtime.Geometry, req runtime.ScreenshotRequest) []byte {
	width, height := int(float64(g.Width)*req.Scale), int(float64(g.Height)*req.Scale)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.White)
	var out bytes.Buffer
	if req.Format == display.FormatJPEG {
		_ = jpeg.Encode(&out, img, nil)
		return out.Bytes()
	}
	_ = png.Encode(&out, img)
	return out.Bytes()
}

func (d *fakeDesktop) Screen(ctx context.Context, id string, _ int, format string) (<-chan runtime.Frame, error) {
	s, ok := d.spec(id)
	if !ok || s.Display == nil {
		return nil, runtime.ErrNotFound
	}
	frames := make(chan runtime.Frame, 1)
	go func() {
		defer close(frames)
		frame := runtime.Frame{Format: format, Data: frameOf(*s.Display, runtime.ScreenshotRequest{Format: format, Scale: 1})}
		select {
		case frames <- frame:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return frames, nil
}

func (d *fakeDesktop) Input(_ context.Context, id string, events []runtime.InputEvent) error {
	s, ok := d.spec(id)
	if !ok || s.Display == nil {
		return runtime.ErrNotFound
	}
	if !d.acceptsAnything {
		if err := display.Validate(events, *s.Display); err != nil {
			return fmt.Errorf("%w: %w", runtime.ErrInvalid, err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, events)
	return nil
}

// desktopOptions is what a suite over an image carrying a desktop is given.
func desktopOptions() Options {
	return Options{
		Image:        "registry.example.com/base:1",
		DisplayImage: "registry.example.com/cella-display:1",
		Listen:       func(port int) []string { return []string{"listen", fmt.Sprint(port)} },
	}
}

// runCase drives one registered case and fails the test with everything the
// case recorded.
func runCase(t *testing.T, name string, d runtime.Driver, opts Options) *recorder {
	t.Helper()
	return drive(caseNamed(t, name), opener(d), opts)
}

func TestDisplayCasesPassAConformingDriver(t *testing.T) {
	d := newFakeDesktop()
	d.bound = 18080
	for _, name := range []string{"DisplayScreenshot", "ScreenStream", "InputAcceptsAndRefuses", "PortsReportListening"} {
		t.Run(name, func(t *testing.T) {
			if r := runCase(t, name, d, desktopOptions()); r.failed() {
				t.Fatalf("%s failed a conforming driver:\n%s", name, r.report())
			}
		})
	}
	// The batch the case sent reached the driver whole, in order.
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.batches) == 0 || len(d.batches[0]) != 4 {
		t.Fatalf("the driver saw %d batches", len(d.batches))
	}
}

// TestDisplayCasesSkipWhatTheSuiteCannotAsk: a capability the driver does not
// declare, an image with no desktop in it, and a suite with no command that
// binds a port are each named as the reason rather than passing silently.
func TestDisplayCasesSkipWhatTheSuiteCannotAsk(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		driver     runtime.Driver
		opts       Options
	}{
		{"DisplayScreenshot", "declares no Display", Nop{}, desktopOptions()},
		{"InputAcceptsAndRefuses", "declares no Input", Nop{}, desktopOptions()},
		{"ScreenStream", "declares no Display", Nop{}, desktopOptions()},
		{"DisplayScreenshot", "no image carrying a desktop", newFakeDesktop(), Options{}},
		{"InputAcceptsAndRefuses", "no image carrying a desktop", newFakeDesktop(), Options{}},
		{"PortsReportListening", "no command that binds a port", newFakeDesktop(), Options{}},
	} {
		t.Run(tc.name+"/"+tc.want, func(t *testing.T) {
			r := runCase(t, tc.name, tc.driver, tc.opts)
			if !strings.Contains(r.skipped(), tc.want) {
				t.Fatalf("the case was not skipped for the right reason: %q", r.report())
			}
		})
	}
}

// TestDisplayCasesFailEachDefect is the other half: a suite that passes
// everything catches nothing, so each way a driver can break the contract is
// driven here and must fail.
func TestDisplayCasesFailEachDefect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() *fakeDesktop
		case_ string
	}{
		{"a frame of the wrong size", func() *fakeDesktop {
			d := newFakeDesktop()
			d.wrongSize = true
			return d
		}, "DisplayScreenshot"},
		{"a desktop that never becomes ready", func() *fakeDesktop {
			d := newFakeDesktop()
			d.notReady = true
			return d
		}, "DisplayScreenshot"},
		{"a driver that accepts a click off the screen", func() *fakeDesktop {
			d := newFakeDesktop()
			d.acceptsAnything = true
			return d
		}, "InputAcceptsAndRefuses"},
		{"a port nothing holds reported as listening", func() *fakeDesktop {
			d := newFakeDesktop()
			d.bound = 18081
			return d
		}, "PortsReportListening"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A case that waits is not made to wait its whole budget for a
			// driver that will never answer.
			desk, poll := desktopTimeout, pollTimeout
			desktopTimeout, pollTimeout = 200*time.Millisecond, 200*time.Millisecond
			defer func() { desktopTimeout, pollTimeout = desk, poll }()
			if r := runCase(t, tc.case_, tc.build(), desktopOptions()); !r.failed() {
				t.Fatalf("%s passed %s", tc.case_, tc.name)
			}
		})
	}
}

// TestCapabilityAndInterfaceAgreeOnTheDesktop holds the declaration to the
// implementation in both directions, which is what lets the controller and the
// API branch on the declaration alone.
func TestCapabilityAndInterfaceAgreeOnTheDesktop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver runtime.Driver
		want   string
	}{
		{"a declaration with no screen behind it", displayLiar{Nop{}}, "does not implement runtime.DisplayDriver"},
		{"a declaration with no pointer behind it", inputLiar{Nop{}}, "does not implement runtime.InputDriver"},
		{"a declaration with no dialer behind it", dialLiar{Nop{}}, "does not implement runtime.Dialer"},
		{"a screen nothing declares", undeclaredDesktop{newFakeDesktop()}, "does not declare Display"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCase(t, "NameIsolationCapabilities", tc.driver, Options{})
			if !strings.Contains(r.report(), tc.want) {
				t.Fatalf("the suite reported %q, want it to name %q", r.report(), tc.want)
			}
		})
	}
}

// The three declarations with nothing behind them.
type displayLiar struct{ runtime.Driver }

func (displayLiar) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Display: true}
}

type inputLiar struct{ runtime.Driver }

func (inputLiar) Capabilities() runtime.Capabilities { return runtime.Capabilities{Input: true} }

type dialLiar struct{ runtime.Driver }

func (dialLiar) Capabilities() runtime.Capabilities { return runtime.Capabilities{Dial: true} }

// undeclaredDesktop implements the screen and declares none of it, which the
// suite catches as well: a capability the API does not see is one no caller
// can reach, and a driver that grew one silently is a driver whose contract
// drifted.
type undeclaredDesktop struct{ *fakeDesktop }

func (undeclaredDesktop) Capabilities() runtime.Capabilities { return runtime.Capabilities{} }
