// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package display_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime/display"
)

// screen is the desktop every case validates against.
var screen = display.Geometry{Width: 1280, Height: 800}

func at(v int) *int { return &v }

func ok(e display.InputEvent) display.InputEvent { return e }

// TestValidate is the whole rule table of spec 023, one row per refusal and
// one per shape that passes. Every refusal names the field it is about, so a
// caller learns which event of a batch it may not send.
func TestValidate(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", display.MaxText+1)
	for _, c := range []struct {
		name   string
		events []display.InputEvent
		path   string // empty means the batch is legal
	}{
		{"move", []display.InputEvent{{Type: display.TypeMove, X: at(10), Y: at(20)}}, ""},
		{"move at the origin", []display.InputEvent{{Type: display.TypeMove, X: at(0), Y: at(0)}}, ""},
		{"move needs x", []display.InputEvent{{Type: display.TypeMove, Y: at(20)}}, "events[0].x"},
		{"move needs y", []display.InputEvent{{Type: display.TypeMove, X: at(20)}}, "events[0].y"},
		{"x past the width", []display.InputEvent{{Type: display.TypeMove, X: at(1280), Y: at(0)}}, "events[0].x"},
		{"y past the height", []display.InputEvent{{Type: display.TypeMove, X: at(0), Y: at(800)}}, "events[0].y"},
		{"negative x", []display.InputEvent{{Type: display.TypeMove, X: at(-1), Y: at(0)}}, "events[0].x"},
		{"click without a point", []display.InputEvent{{Type: display.TypeClick, Button: "left"}}, ""},
		{"click with a point", []display.InputEvent{{Type: display.TypeClick, Button: "right", X: at(1), Y: at(2)}}, ""},
		{"click with half a point", []display.InputEvent{{Type: display.TypeClick, Button: "left", X: at(1)}}, "events[0].x"},
		{"click off the desktop", []display.InputEvent{{Type: display.TypeClick, Button: "left", X: at(5000), Y: at(2)}}, "events[0].x"},
		{"click needs a button", []display.InputEvent{{Type: display.TypeClick}}, "events[0].button"},
		{"a numeric button is not a button", []display.InputEvent{{Type: display.TypeClick, Button: "1"}}, "events[0].button"},
		{"double click", []display.InputEvent{{Type: display.TypeDoubleClick, Button: "left"}}, ""},
		{"triple click", []display.InputEvent{{Type: display.TypeTripleClick, Button: "middle"}}, ""},
		{"press", []display.InputEvent{{Type: display.TypeMouseDown, Button: "left"}}, ""},
		{"release", []display.InputEvent{{Type: display.TypeMouseUp, Button: "left"}}, ""},
		{"drag", []display.InputEvent{{Type: display.TypeDrag, Button: "left", X: at(1), Y: at(2), ToX: at(3), ToY: at(4)}}, ""},
		{"drag needs a destination", []display.InputEvent{{Type: display.TypeDrag, Button: "left", X: at(1), Y: at(2)}}, "events[0].toX"},
		{"drag destination off the desktop", []display.InputEvent{{Type: display.TypeDrag, Button: "left", X: at(1), Y: at(2), ToX: at(3), ToY: at(900)}}, "events[0].toY"},
		{"scroll", []display.InputEvent{{Type: display.TypeScroll, Direction: "down", Amount: 3}}, ""},
		{"scroll needs a direction", []display.InputEvent{{Type: display.TypeScroll, Amount: 3}}, "events[0].direction"},
		{"scroll direction is an enum", []display.InputEvent{{Type: display.TypeScroll, Direction: "sideways", Amount: 3}}, "events[0].direction"},
		{"scroll needs an amount", []display.InputEvent{{Type: display.TypeScroll, Direction: "up"}}, "events[0].amount"},
		{"scroll amount is bounded", []display.InputEvent{{Type: display.TypeScroll, Direction: "up", Amount: 51}}, "events[0].amount"},
		{"key", []display.InputEvent{{Type: display.TypeKey, Key: "Return"}}, ""},
		{"key with modifiers", []display.InputEvent{{Type: display.TypeKey, Key: "a", Modifiers: []string{"ctrl"}}}, ""},
		{"key needs a keysym", []display.InputEvent{{Type: display.TypeKey}}, "events[0].key"},
		{"a chord is not a keysym", []display.InputEvent{{Type: display.TypeKey, Key: "ctrl+a"}}, "events[0].key"},
		{"a flag is not a keysym", []display.InputEvent{{Type: display.TypeKey, Key: "-window"}}, "events[0].key"},
		{"a space is not a keysym", []display.InputEvent{{Type: display.TypeKey, Key: "a b"}}, "events[0].key"},
		{"a shell byte is not a keysym", []display.InputEvent{{Type: display.TypeKey, Key: "a;rm"}}, "events[0].key"},
		{"unknown modifier", []display.InputEvent{{Type: display.TypeKey, Key: "a", Modifiers: []string{"hyper"}}}, "events[0].modifiers"},
		{"type", []display.InputEvent{{Type: display.TypeType, Text: "hello"}}, ""},
		{"type needs text", []display.InputEvent{{Type: display.TypeType}}, "events[0].text"},
		{"text is bounded", []display.InputEvent{{Type: display.TypeType, Text: long}}, "events[0].text"},
		{"text at the bound", []display.InputEvent{{Type: display.TypeType, Text: long[:display.MaxText]}}, ""},
		{"wait", []display.InputEvent{{Type: display.TypeWait, Ms: 100}}, ""},
		{"wait needs a duration", []display.InputEvent{{Type: display.TypeWait}}, "events[0].ms"},
		{"one wait is bounded", []display.InputEvent{{Type: display.TypeWait, Ms: display.MaxWaitMS + 1}}, "events[0].ms"},
		{"unknown type", []display.InputEvent{{Type: display.TypeMove, X: at(0), Y: at(0)}, {Type: "teleport"}}, "events[1].type"},
		{"no type", []display.InputEvent{{}}, "events[0].type"},
		{"empty batch", nil, "events"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := display.Validate(c.events, screen)
			if c.path == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			var invalid *display.Invalid
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate: %v, want a *display.Invalid naming %s", err, c.path)
			}
			if !errors.Is(err, display.ErrInvalid) {
				t.Errorf("Validate: %v does not unwrap to ErrInvalid", err)
			}
			if !slices.Contains(invalid.Paths, c.path) {
				t.Errorf("Validate paths %v, want one of them %s", invalid.Paths, c.path)
			}
			if invalid.Detail == "" {
				t.Error("the refusal carries no sentence")
			}
		})
	}
}

// TestValidateBatchBounds covers the two rules about a batch as a whole: how
// many events it may carry and how long its waits may add up to.
func TestValidateBatchBounds(t *testing.T) {
	t.Parallel()
	typing := make([]display.InputEvent, display.MaxEvents)
	for i := range typing {
		typing[i] = display.InputEvent{Type: display.TypeType, Text: strings.Repeat("x", display.MaxText)}
	}
	if err := display.Validate(typing, screen); err != nil {
		t.Fatalf("%d events of %d bytes: %v, want nil", display.MaxEvents, display.MaxText, err)
	}
	if err := display.Validate(append(slices.Clone(typing), typing[0]), screen); err == nil {
		t.Fatalf("%d events: nil, want a refusal", display.MaxEvents+1)
	}
	waits := make([]display.InputEvent, 7)
	for i := range waits {
		waits[i] = display.InputEvent{Type: display.TypeWait, Ms: display.MaxWaitMS}
	}
	err := display.Validate(waits[:6], screen)
	if err != nil {
		t.Fatalf("60 seconds of waiting: %v, want nil", err)
	}
	var invalid *display.Invalid
	if err = display.Validate(waits, screen); !errors.As(err, &invalid) || !slices.Contains(invalid.Paths, "events") {
		t.Fatalf("70 seconds of waiting: %v, want a refusal naming events", err)
	}
}

// TestArgv is the other half of the vocabulary: what one event becomes inside
// the sandbox.
func TestArgv(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		e    display.InputEvent
		want [][]string
	}{
		{"move", ok(display.InputEvent{Type: display.TypeMove, X: at(3), Y: at(4)}),
			[][]string{{"xdotool", "mousemove", "--", "3", "4"}}},
		{"click at the pointer", ok(display.InputEvent{Type: display.TypeClick, Button: "left"}),
			[][]string{{"xdotool", "click", "--clearmodifiers", "1"}}},
		{"click at a point", ok(display.InputEvent{Type: display.TypeClick, Button: "right", X: at(1), Y: at(2)}),
			[][]string{{"xdotool", "mousemove", "--", "1", "2"}, {"xdotool", "click", "--clearmodifiers", "3"}}},
		{"double click", ok(display.InputEvent{Type: display.TypeDoubleClick, Button: "middle"}),
			[][]string{{"xdotool", "click", "--clearmodifiers", "--repeat", "2", "2"}}},
		{"triple click", ok(display.InputEvent{Type: display.TypeTripleClick, Button: "left"}),
			[][]string{{"xdotool", "click", "--clearmodifiers", "--repeat", "3", "1"}}},
		{"press", ok(display.InputEvent{Type: display.TypeMouseDown, Button: "left"}),
			[][]string{{"xdotool", "mousedown", "1"}}},
		{"release", ok(display.InputEvent{Type: display.TypeMouseUp, Button: "left"}),
			[][]string{{"xdotool", "mouseup", "1"}}},
		{"drag", ok(display.InputEvent{Type: display.TypeDrag, Button: "left", X: at(1), Y: at(2), ToX: at(30), ToY: at(40)}),
			[][]string{
				{"xdotool", "mousemove", "--", "1", "2"},
				{"xdotool", "mousedown", "1"},
				{"xdotool", "mousemove", "--", "30", "40"},
				{"xdotool", "mouseup", "1"},
			}},
		{"scroll", ok(display.InputEvent{Type: display.TypeScroll, Direction: "down", Amount: 3}),
			[][]string{{"xdotool", "click", "--repeat", "3", "5"}}},
		{"scroll at a point", ok(display.InputEvent{Type: display.TypeScroll, Direction: "up", Amount: 1, X: at(7), Y: at(8)}),
			[][]string{{"xdotool", "mousemove", "--", "7", "8"}, {"xdotool", "click", "--repeat", "1", "4"}}},
		{"scroll left and right", ok(display.InputEvent{Type: display.TypeScroll, Direction: "left", Amount: 2}),
			[][]string{{"xdotool", "click", "--repeat", "2", "6"}}},
		{"key", ok(display.InputEvent{Type: display.TypeKey, Key: "Return"}),
			[][]string{{"xdotool", "key", "--clearmodifiers", "Return"}}},
		{"key with a chord", ok(display.InputEvent{Type: display.TypeKey, Key: "a", Modifiers: []string{"ctrl", "shift"}}),
			[][]string{{"xdotool", "key", "--clearmodifiers", "ctrl+shift+a"}}},
		{"type", ok(display.InputEvent{Type: display.TypeType, Text: "-n hello"}),
			[][]string{{"xdotool", "type", "--clearmodifiers", "--", "-n hello"}}},
		{"wait runs no command", ok(display.InputEvent{Type: display.TypeWait, Ms: 5}), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := display.Argv(c.e)
			if !slices.EqualFunc(got, c.want, slices.Equal) {
				t.Fatalf("Argv:\n got %v\nwant %v", got, c.want)
			}
		})
	}
}

// TestArgvHoldsModifiersAroundAClick pins the defect the port fixes: the
// hosted source pressed the modifiers and then cleared them on the click, so
// a chorded click never reached the desktop chorded.
func TestArgvHoldsModifiersAroundAClick(t *testing.T) {
	t.Parallel()
	got := display.Argv(display.InputEvent{Type: display.TypeClick, Button: "left", Modifiers: []string{"ctrl"}})
	want := [][]string{
		{"xdotool", "keydown", "ctrl"},
		{"xdotool", "click", "1"},
		{"xdotool", "keyup", "ctrl"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("Argv:\n got %v\nwant %v", got, want)
	}
	for _, argv := range got {
		if slices.Contains(argv, "--clearmodifiers") {
			t.Fatalf("the click clears the modifiers it is held under: %v", argv)
		}
	}
}

func TestGeometry(t *testing.T) {
	t.Parallel()
	if g := (display.Geometry{Width: 1280, Height: 800}); !g.Valid() || g.String() != "1280x800x24" {
		t.Fatalf("Geometry %v: valid %v, string %q", g, g.Valid(), g.String())
	}
	for _, g := range []display.Geometry{
		{Width: 319, Height: 800}, {Width: 7681, Height: 800},
		{Width: 1280, Height: 239}, {Width: 1280, Height: 4321}, {},
	} {
		if g.Valid() {
			t.Errorf("Geometry %v is valid, want refused", g)
		}
	}
}

func TestScreenshotRequestNormalize(t *testing.T) {
	t.Parallel()
	got, err := display.ScreenshotRequest{}.Normalize()
	if err != nil || got.Format != display.FormatPNG || got.Scale != display.MaxScale {
		t.Fatalf("the zero request normalizes to %+v, %v", got, err)
	}
	if _, err = (display.ScreenshotRequest{Format: "gif"}).Normalize(); err == nil {
		t.Error("gif: nil, want a refusal")
	}
	for _, scale := range []float64{0.05, 1.5, -1} {
		if _, err = (display.ScreenshotRequest{Scale: scale}).Normalize(); err == nil {
			t.Errorf("scale %v: nil, want a refusal", scale)
		}
	}
	if display.MediaType(display.FormatJPEG) != "image/jpeg" || display.MediaType("") != "image/png" {
		t.Error("the media types do not follow the formats")
	}
}

// TestScripts holds the commands to what the drivers depend on: the geometry
// reaches the supervisor, the capture names the format and the scale, the
// stream paces itself, and the install is idempotent.
func TestScripts(t *testing.T) {
	t.Parallel()
	install := display.InstallScript(screen)
	for _, want := range []string{"kill -0", display.PIDPath, "base64 -d", "1280x800x24", "setsid"} {
		if !strings.Contains(install, want) {
			t.Errorf("the install script does not carry %q", want)
		}
	}
	if !strings.Contains(display.StartScript, "Xvfb") {
		t.Error("the supervisor does not start an X server")
	}
	ready := display.ReadyScript()
	if !strings.Contains(ready, "xdpyinfo") || !strings.Contains(ready, "_NET_SUPPORTING_WM_CHECK") {
		t.Errorf("the ready script checks one half only: %s", ready)
	}
	full := display.CaptureScript(display.ScreenshotRequest{Format: display.FormatPNG, Scale: 1})
	if strings.Contains(full, "-resize") || !strings.HasSuffix(full, "png:-") {
		t.Errorf("a full-size PNG capture is %q", full)
	}
	half := display.CaptureScript(display.ScreenshotRequest{Format: display.FormatJPEG, Scale: 0.5})
	if !strings.Contains(half, "-resize 50%") || !strings.HasSuffix(half, "jpg:-") {
		t.Errorf("a half-size JPEG capture is %q", half)
	}
	for fps, want := range map[int]string{1: "1.000", 4: "0.250", 10: "0.100", 100: "0.100", 0: "1.000"} {
		if stream := display.StreamScript(fps, display.FormatPNG); !strings.Contains(stream, "sleep "+want) {
			t.Errorf("a stream at %d frames a second sleeps something other than %s: %s", fps, want, stream)
		}
	}
	if env := display.Env(screen); env[display.DisplayEnv] != ":0" || env[display.GeometryEnv] != "1280x800x24" {
		t.Errorf("the desktop environment is %v", env)
	}
	if !strings.Contains(display.StopScript(), display.PIDPath) {
		t.Error("the stop script does not read the pid file")
	}
}

func TestReadFrame(t *testing.T) {
	t.Parallel()
	stream := bytes.NewReader([]byte("00000003" + "abc" + "00000000" + "00000001" + "z"))
	frame, err := display.ReadFrame(stream)
	if err != nil || string(frame) != "abc" {
		t.Fatalf("the first frame is %q, %v", frame, err)
	}
	if frame, err = display.ReadFrame(stream); err != nil || len(frame) != 0 {
		t.Fatalf("an empty frame is %q, %v", frame, err)
	}
	if frame, err = display.ReadFrame(stream); err != nil || string(frame) != "z" {
		t.Fatalf("the third frame is %q, %v", frame, err)
	}
	if _, err = display.ReadFrame(stream); !errors.Is(err, io.EOF) {
		t.Fatalf("the end of the stream is %v, want io.EOF", err)
	}
	if _, err = display.ReadFrame(bytes.NewReader([]byte("00000009short"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a truncated frame is %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err = display.ReadFrame(bytes.NewReader([]byte("zzzzzzzz1"))); !errors.Is(err, display.ErrInvalid) {
		t.Fatalf("a header that is not a length is %v", err)
	}
	if _, err = display.ReadFrame(bytes.NewReader([]byte("7fffffff1"))); !errors.Is(err, display.ErrFrameTooLarge) {
		t.Fatalf("an oversized header is %v", err)
	}
}

// TestListening reads a real kernel table: one listener on 8080, one
// established connection that is not a listener, and one IPv6 listener.
func TestListening(t *testing.T) {
	t.Parallel()
	const table = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 22661 1 0000 100 0 0 10 0
   1: 0100007F:B3A6 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 22999 1 0000 20 0 0 10 0
   2: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 23111 1 0000 100 0 0 10 0
malformed
`
	got := display.Listening(table)
	if !got[8080] || !got[80] {
		t.Fatalf("the listening ports are %v, want 8080 and 80", got)
	}
	if got[45990] {
		t.Errorf("an established connection reads as a listener: %v", got)
	}
	if len(display.Listening("")) != 0 {
		t.Error("an empty table names ports")
	}
}
