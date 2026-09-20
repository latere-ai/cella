// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// desk is the desktop the display cases create sandboxes with.
var desk = driver.Geometry{Width: 1280, Height: 800}

func ptr(v int) *int { return &v }

// commands records every command the driver ran inside a sandbox and answers
// each from one table, so a case sees exactly what reached the engine.
type commands struct {
	mu   sync.Mutex
	ran  [][]string
	out  map[string]string
	code map[string]int
}

func (c *commands) answer(cmd []string, _ []string, _ string) fakeExecResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = append(c.ran, cmd)
	joined := strings.Join(cmd, " ")
	result := fakeExecResult{}
	for match, code := range c.code {
		if strings.Contains(joined, match) {
			result.code = code
		}
	}
	for match, out := range c.out {
		if strings.Contains(joined, match) {
			result.stdout = out
		}
	}
	return result
}

func (c *commands) list() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ran)
}

// ranWith reports whether any command carried the text.
func (c *commands) ranWith(text string) bool {
	for _, cmd := range c.list() {
		if strings.Contains(strings.Join(cmd, " "), text) {
			return true
		}
	}
	return false
}

// countWith is how many commands carried the text.
func (c *commands) countWith(text string) int {
	n := 0
	for _, cmd := range c.list() {
		if strings.Contains(strings.Join(cmd, " "), text) {
			n++
		}
	}
	return n
}

// upDriver is an engine whose desktop answers every probe, with a sandbox of
// the given shape already created.
func upDriver(t *testing.T, spec driver.CreateSpec) (*Driver, *commands, *fake) {
	t.Helper()
	f := newFake(t)
	c := &commands{out: map[string]string{}, code: map[string]int{}}
	f.run = c.answer
	d := f.driver(t)
	create(t, d, spec)
	return d, c, f
}

// TestCreateStartsTheDesktop holds the lifecycle rule of spec 023: the desktop
// is built at create and again at every start, and the supervisor is
// idempotent so a repeated call converges on one.
func TestCreateStartsTheDesktop(t *testing.T) {
	d, c, f := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	if c.countWith("cella-display/start") != 1 {
		t.Fatalf("the create ran %d supervisor commands: %v", c.countWith("cella-display/start"), c.list())
	}
	// The command is the shared install script, geometry and all: the podman
	// driver and the display image bring up the same desktop.
	if !c.ranWith(display.InstallScript(desk)) {
		t.Fatalf("the create ran something other than the install script: %v", c.list())
	}
	if !c.ranWith("1280x800x24") {
		t.Fatalf("the geometry did not reach the supervisor: %v", c.list())
	}
	// The workload beside the desktop is told which screen to draw on.
	if got := f.containerOf(t, "sbx_a").env[display.DisplayEnv]; got != display.DisplayValue {
		t.Fatalf("the container's DISPLAY is %q", got)
	}
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if got := c.countWith("cella-display/start"); got != 2 {
		t.Fatalf("the start ran %d supervisor commands in total, want 2", got)
	}
}

// TestASandboxWithoutADesktopStartsNone is the other half: nothing runs inside
// a sandbox that asked for no screen.
func TestASandboxWithoutADesktopStartsNone(t *testing.T) {
	d, c, f := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img"})
	if len(c.list()) != 0 {
		t.Fatalf("a sandbox with no desktop ran %v", c.list())
	}
	if _, ok := f.containerOf(t, "sbx_a").env[display.DisplayEnv]; ok {
		t.Fatal("a sandbox with no desktop is told to draw on one")
	}
	if _, err := d.Display(t.Context(), "sbx_a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Display on a sandbox with no desktop: %v", err)
	}
	if err := d.Input(t.Context(), "sbx_a", []driver.InputEvent{{Type: "click", Button: "left"}}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Input on a sandbox with no desktop: %v", err)
	}
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil || len(state.Conditions) != 0 || len(state.Ports) != 0 {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

func TestDisplayGeometry(t *testing.T) {
	d, _, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	got, err := d.Display(t.Context(), "sbx_a")
	if err != nil || got != desk {
		t.Fatalf("Display = %+v, %v", got, err)
	}
	if _, err = d.Display(t.Context(), "sbx_absent"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Display on an unknown sandbox: %v", err)
	}
}

// TestDisplayReadyFollowsTheProbe is the condition: the driver's own reading of
// the desktop at every Inspect, and never at List.
func TestDisplayReadyFollowsTheProbe(t *testing.T) {
	d, c, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Conditions) != 1 || state.Conditions[0].Type != v1.ConditionDisplayReady ||
		state.Conditions[0].Status != v1.ConditionTrue {
		t.Fatalf("the conditions are %+v, want DisplayReady true", state.Conditions)
	}
	c.code["xdpyinfo"] = 1
	if state, err = d.Inspect(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if state.Conditions[0].Status != v1.ConditionFalse || state.Conditions[0].Message == "" {
		t.Fatalf("a desktop that is not up reads %+v", state.Conditions[0])
	}
	// A sweep of every sandbox runs no command: the reaper's path must not
	// cost one probe per sandbox per tick.
	before := len(c.list())
	if _, err = d.List(t.Context(), driver.Filter{}); err != nil {
		t.Fatal(err)
	}
	if len(c.list()) != before {
		t.Fatalf("List ran %d commands", len(c.list())-before)
	}
}

// TestPortsProbe reads the kernel's own socket table inside the sandbox.
func TestPortsProbe(t *testing.T) {
	ports := []driver.Port{{Name: "web", Port: 8080}, {Name: "api", Port: 9090}}
	d, c, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Ports: ports})
	c.out["/proc/net/tcp"] = "  sl  local_address rem_address   st\n" +
		"   0: 00000000:1F90 00000000:0000 0A 0 0 0\n"
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	want := []driver.PortState{
		{Name: "web", Port: 8080, State: driver.PortListening},
		{Name: "api", Port: 9090, State: driver.PortClosed},
	}
	if !slices.Equal(state.Ports, want) {
		t.Fatalf("the ports are %+v, want %+v", state.Ports, want)
	}
	// A probe that fails reports every port closed rather than none at all: a
	// caller reading no ports would read a sandbox that declared none.
	c.code["/proc/net/tcp"] = 1
	if state, err = d.Inspect(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if len(state.Ports) != 2 || state.Ports[0].State != driver.PortClosed {
		t.Fatalf("a failed probe reports %+v", state.Ports)
	}
	// A stopped sandbox is probed for nothing: there is no process to ask.
	if err = d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if state, err = d.Inspect(t.Context(), "sbx_a"); err != nil || state.Ports != nil {
		t.Fatalf("a stopped sandbox reports %+v, %v", state.Ports, err)
	}
}

func TestScreenshot(t *testing.T) {
	d, c, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	c.out["xwd"] = "\x89PNG frame"
	frame, err := d.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(frame)
	if string(body) != "\x89PNG frame" {
		t.Fatalf("the frame is %q", body)
	}
	if err = frame.Close(); err != nil {
		t.Fatal(err)
	}
	if !c.ranWith("png:-") {
		t.Fatalf("the capture did not name the encoding: %v", c.list())
	}
	// The request's format and scale reach the capture.
	if _, err = d.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{Format: display.FormatJPEG, Scale: 0.5}); err != nil {
		t.Fatal(err)
	}
	if !c.ranWith("-resize 50%") || !c.ranWith("jpg:-") {
		t.Fatalf("the scale and the format did not reach the capture: %v", c.list())
	}
	// A request outside the contract never reaches the sandbox.
	if _, err = d.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{Scale: 9}); err == nil {
		t.Fatal("a scale outside the contract was captured")
	}
	// A desktop that is not up is the condition, not an empty frame.
	c.code["xdpyinfo"] = 1
	if _, err = d.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{}); !errors.Is(err, driver.ErrDisplayNotReady) {
		t.Fatalf("a screenshot of a desktop that is not up: %v", err)
	}
}

// TestInput holds the batch to its rules and to what it runs: validated whole
// against this sandbox's own geometry, then one command per argument list.
func TestInput(t *testing.T) {
	d, c, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	batch := []driver.InputEvent{
		{Type: display.TypeMove, X: ptr(10), Y: ptr(20)},
		{Type: display.TypeClick, Button: "left"},
		{Type: display.TypeType, Text: "hello"},
	}
	if err := d.Input(t.Context(), "sbx_a", batch); err != nil {
		t.Fatal(err)
	}
	var gestures [][]string
	for _, cmd := range c.list() {
		if cmd[0] == "xdotool" {
			gestures = append(gestures, cmd)
		}
	}
	want := [][]string{
		{"xdotool", "mousemove", "--", "10", "20"},
		{"xdotool", "click", "--clearmodifiers", "1"},
		{"xdotool", "type", "--clearmodifiers", "--", "hello"},
	}
	if !slices.EqualFunc(gestures, want, slices.Equal) {
		t.Fatalf("the batch ran\n %v\nwant %v", gestures, want)
	}

	// A coordinate off this sandbox's own screen is refused before anything
	// runs, and the refusal is the contract's invalid request.
	before := len(c.list())
	err := d.Input(t.Context(), "sbx_a", []driver.InputEvent{{Type: display.TypeClick, Button: "left", X: ptr(4000), Y: ptr(10)}})
	if !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a click off the screen: %v", err)
	}
	var invalid *display.Invalid
	if !errors.As(err, &invalid) || !slices.Contains(invalid.Paths, "events[0].x") {
		t.Fatalf("the refusal does not name the field: %v", err)
	}
	if len(c.list()) != before {
		t.Fatalf("a refused batch ran %v", c.list()[before:])
	}

	// A batch stops at the first event the desktop refuses and says where.
	c.code["xdotool key"] = 1
	err = d.Input(t.Context(), "sbx_a", []driver.InputEvent{
		{Type: display.TypeMove, X: ptr(1), Y: ptr(1)},
		{Type: display.TypeKey, Key: "Return"},
		{Type: display.TypeKey, Key: "Escape"},
	})
	var failure *driver.InputFailure
	if !errors.As(err, &failure) || failure.Index != 1 || failure.Executed != 1 {
		t.Fatalf("a batch that fails at the second event: %v", err)
	}
}

// TestScreen holds the stream to its shape: one command for the whole session,
// length-prefixed frames off it, and the session ended inside the sandbox when
// the stream is done.
func TestScreen(t *testing.T) {
	d, c, _ := upDriver(t, driver.CreateSpec{ID: "sbx_a", Image: "img", Display: &desk})
	c.out["while test -e"] = "00000003one" + "00000003two"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	frames, err := d.Screen(ctx, "sbx_a", 4, display.FormatPNG)
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := <-frames
	if !ok || string(frame.Data) != "one" || frame.Format != display.FormatPNG {
		t.Fatalf("the first frame is %+v (open %v)", frame, ok)
	}
	for range frames { //nolint:revive // draining what the session produced
	}
	// One command served the whole stream, paced by the driver's own sleep.
	if c.countWith("while test -e") != 1 {
		t.Fatalf("the session ran %d stream commands", c.countWith("while test -e"))
	}
	if !c.ranWith("sleep 0.250") {
		t.Fatalf("the stream is not paced at the requested rate: %v", c.list())
	}
	// The session file is removed when the stream ends, which is the only way
	// to end a loop the engine cannot kill.
	if !c.ranWith("rm -f " + display.HomeDir + "/session.") {
		t.Fatalf("the session was not ended inside the sandbox: %v", c.list())
	}
	c.code["xdpyinfo"] = 1
	if _, err = d.Screen(ctx, "sbx_a", 4, display.FormatPNG); !errors.Is(err, driver.ErrDisplayNotReady) {
		t.Fatalf("a stream of a desktop that is not up: %v", err)
	}
}

func TestPortLabelRoundTrip(t *testing.T) {
	ports := []driver.Port{
		{Name: "web", Port: 8080, Expose: driver.ExposeNone},
		{Name: "api", Port: 9090, Expose: driver.ExposeMesh},
	}
	if got := portsOf(portsLabel(ports)); !slices.Equal(got, ports) {
		t.Fatalf("round trip = %+v, want %+v", got, ports)
	}
	// A port with no reach reads back as the default one.
	if got := portsOf(portsLabel([]driver.Port{{Name: "web", Port: 80}})); got[0].Expose != driver.ExposeNone {
		t.Fatalf("a port with no reach reads %+v", got[0])
	}
	if portsLabel(nil) != "" || portsOf("") != nil {
		t.Fatal("an empty list does not round trip to nothing")
	}
	// A label that is not the shape this driver writes names no port, rather
	// than a port whose number is a guess.
	for _, label := range []string{"web", "web:eighty:none", "web:80"} {
		if got := portsOf(label); len(got) != 0 {
			t.Fatalf("the label %q reads as %+v", label, got)
		}
	}
	for _, label := range []string{"", "1280", "axb", "1280x"} {
		if got := geometryOf(label); got != nil {
			t.Fatalf("the label %q reads as %+v", label, got)
		}
	}
	if geometryLabel(nil) != "" {
		t.Fatal("no desktop is written as a geometry")
	}
}
