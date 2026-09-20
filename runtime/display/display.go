// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package display is the desktop vocabulary of spec 023: the geometry a
// sandbox asks for, the frames its screen produces, the pointer and keyboard
// events it accepts, the rules that hold a batch to the contract before
// anything runs, and the commands one of those events becomes inside the
// sandbox.
//
// Every driver that declares Display and Input validates through this package
// and runs the commands it builds, so one implementation answers what a
// gesture means and one rule set decides what a caller may ask for. Package
// runtime aliases the four types, so a caller names one type and a driver
// imports one package. This package imports the standard library alone.
package display

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Geometry is a desktop's size in pixels. The depth is always 24: a screen a
// caller reads as PNG has no use for a palette, and one depth keeps the
// capture path a single command.
type Geometry struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// The geometry bounds of spec 003. They bound the X server's frame buffer,
// which is width times height times four bytes of resident memory.
const (
	MinWidth  = 320
	MaxWidth  = 7680
	MinHeight = 240
	MaxHeight = 4320
)

// Depth is the colour depth every desktop runs at.
const Depth = 24

// Valid reports whether the geometry is inside the contract's bounds.
func (g Geometry) Valid() bool {
	return g.Width >= MinWidth && g.Width <= MaxWidth && g.Height >= MinHeight && g.Height <= MaxHeight
}

// String is the X server's own spelling, which is what the supervisor reads.
func (g Geometry) String() string {
	return strconv.Itoa(g.Width) + "x" + strconv.Itoa(g.Height) + "x" + strconv.Itoa(Depth)
}

// The encodings a frame is answered in.
const (
	FormatPNG  = "png"
	FormatJPEG = "jpeg"
)

// ScreenshotRequest is one frame's encoding. A zero Format is FormatPNG and a
// zero Scale is 1.0, so the zero value is one full-size PNG.
type ScreenshotRequest struct {
	Format string  `json:"format,omitempty"`
	Scale  float64 `json:"scale,omitempty"`
}

// Scale bounds. Below the minimum a frame carries no readable text, and above
// 1 the capture tool would upscale pixels the screen never had.
const (
	MinScale = 0.1
	MaxScale = 1.0
)

// Normalize fills the defaults and reports whether what remains is inside the
// contract. It is the one place the two optional fields are read.
func (r ScreenshotRequest) Normalize() (ScreenshotRequest, error) {
	if r.Format == "" {
		r.Format = FormatPNG
	}
	if r.Format != FormatPNG && r.Format != FormatJPEG {
		return r, &Invalid{Paths: []string{"format"}, Detail: "A frame is encoded as png or jpeg."}
	}
	if r.Scale == 0 {
		r.Scale = MaxScale
	}
	if r.Scale < MinScale || r.Scale > MaxScale {
		return r, &Invalid{Paths: []string{"scale"}, Detail: "The scale is between 0.1 and 1.0."}
	}
	return r, nil
}

// MediaType is what a frame of this encoding is served as.
func MediaType(format string) string {
	if format == FormatJPEG {
		return "image/jpeg"
	}
	return "image/png"
}

// Frame is one capture of the whole desktop.
type Frame struct {
	At     time.Time `json:"at"`
	Format string    `json:"format"`
	Data   []byte    `json:"data"`
}

// InputEvent is one pointer or keyboard step. The four coordinates are
// pointers because x and y are optional on a press and on a click, where
// absent means at the pointer and an explicit 0 means the left or top edge of
// the desktop; an int cannot hold that difference. The JSON shape is spec
// 023's: an absent field decodes to nil.
type InputEvent struct {
	Type      string   `json:"type"`
	X         *int     `json:"x,omitempty"`
	Y         *int     `json:"y,omitempty"`
	ToX       *int     `json:"toX,omitempty"`
	ToY       *int     `json:"toY,omitempty"`
	Button    string   `json:"button,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`
	Key       string   `json:"key,omitempty"`
	Text      string   `json:"text,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Amount    int      `json:"amount,omitempty"`
	Ms        int      `json:"ms,omitempty"`
}

// The event types of spec 023's table.
const (
	TypeMove        = "move"
	TypeMouseDown   = "mouse_down"
	TypeMouseUp     = "mouse_up"
	TypeClick       = "click"
	TypeDoubleClick = "double_click"
	TypeTripleClick = "triple_click"
	TypeDrag        = "drag"
	TypeScroll      = "scroll"
	TypeKey         = "key"
	TypeType        = "type"
	TypeWait        = "wait"
)

// The bounds of one batch. A batch of MaxEvents events each carrying MaxText
// bytes of text fits inside the 1 MiB body cap the input route applies, which
// is why the two bounds are stated together.
const (
	MaxEvents    = 256
	MaxText      = 1 << 10
	MaxWaitMS    = 10_000
	MaxBatchWait = 60_000
	MinAmount    = 1
	MaxAmount    = 50
)

// The buttons a caller may name, and the X button each one is.
var buttons = map[string]int{"left": 1, "middle": 2, "right": 3}

// The modifiers a caller may hold, spelled as the input tool spells them.
var modifiers = map[string]bool{"ctrl": true, "alt": true, "shift": true, "super": true}

// The scroll directions and the X wheel button each one is.
var wheels = map[string]int{"up": 4, "down": 5, "left": 6, "right": 7}

// ErrInvalid is what every refusal of this package unwraps to, so a driver
// can answer runtime.ErrInvalid without reading the paths.
var ErrInvalid = errors.New("the request breaks the display contract")

// Invalid is one refusal: what is wrong, and the JSON paths of the fields it
// is wrong in. The API answers it as invalid_field with these paths.
type Invalid struct {
	Paths  []string
	Detail string
}

func (e *Invalid) Error() string {
	return strings.Join(e.Paths, ", ") + ": " + e.Detail
}
func (e *Invalid) Unwrap() error { return ErrInvalid }

// eventPath is the path of one field of one event, in the spelling spec 023
// names: events[3].modifiers.
func eventPath(i int, field string) string {
	return "events[" + strconv.Itoa(i) + "]." + field
}

func invalid(i int, field, detail string) error {
	return &Invalid{Paths: []string{eventPath(i, field)}, Detail: detail}
}

// Validate holds a whole batch to the contract before any of it runs, which
// is what makes a refusal total: a caller either gets the whole gesture or
// gets none of it and one path saying why.
func Validate(events []InputEvent, g Geometry) error {
	if len(events) == 0 {
		return &Invalid{Paths: []string{"events"}, Detail: "A batch carries at least one event."}
	}
	if len(events) > MaxEvents {
		return &Invalid{Paths: []string{"events"}, Detail: fmt.Sprintf("A batch carries at most %d events.", MaxEvents)}
	}
	waited := 0
	for i, e := range events {
		if err := validate(i, e, g); err != nil {
			return err
		}
		if e.Type == TypeWait {
			waited += e.Ms
		}
	}
	if waited > MaxBatchWait {
		return &Invalid{Paths: []string{"events"}, Detail: fmt.Sprintf("The waits in one batch total at most %d ms.", MaxBatchWait)}
	}
	return nil
}

func validate(i int, e InputEvent, g Geometry) error {
	for _, m := range e.Modifiers {
		if !modifiers[m] {
			return invalid(i, "modifiers", "A modifier is ctrl, alt, shift or super.")
		}
	}
	switch e.Type {
	case TypeMove:
		return needPoint(i, e.X, e.Y, "x", "y", g)
	case TypeMouseDown, TypeMouseUp, TypeClick, TypeDoubleClick, TypeTripleClick:
		if err := optionalPoint(i, e.X, e.Y, "x", "y", g); err != nil {
			return err
		}
		return needButton(i, e.Button)
	case TypeDrag:
		if err := needPoint(i, e.X, e.Y, "x", "y", g); err != nil {
			return err
		}
		if err := needPoint(i, e.ToX, e.ToY, "toX", "toY", g); err != nil {
			return err
		}
		return needButton(i, e.Button)
	case TypeScroll:
		if err := optionalPoint(i, e.X, e.Y, "x", "y", g); err != nil {
			return err
		}
		if _, ok := wheels[e.Direction]; !ok {
			return invalid(i, "direction", "A scroll runs up, down, left or right.")
		}
		if e.Amount < MinAmount || e.Amount > MaxAmount {
			return invalid(i, "amount", fmt.Sprintf("A scroll is between %d and %d wheel clicks.", MinAmount, MaxAmount))
		}
		return nil
	case TypeKey:
		if !keysym(e.Key) {
			return invalid(i, "key", "A key is one keysym of letters, digits and underscores; the modifiers held with it are their own field.")
		}
		return nil
	case TypeType:
		if e.Text == "" {
			return invalid(i, "text", "There is no text to type.")
		}
		if len(e.Text) > MaxText {
			return invalid(i, "text", fmt.Sprintf("One event types at most %d bytes.", MaxText))
		}
		return nil
	case TypeWait:
		if e.Ms < 1 || e.Ms > MaxWaitMS {
			return invalid(i, "ms", fmt.Sprintf("A wait is between 1 and %d ms.", MaxWaitMS))
		}
		return nil
	case "":
		return invalid(i, "type", "The event names no type.")
	default:
		return invalid(i, "type", "The event names a type this contract does not have.")
	}
}

// keysym is the rule that keeps a value the input tool would read as a flag of
// its own out of its argument list: letters, digits and underscores, and
// nothing else, so a leading dash, a space and a shell byte are all refused.
func keysym(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

func needButton(i int, b string) error {
	if _, ok := buttons[b]; !ok {
		return invalid(i, "button", "A button is left, middle or right.")
	}
	return nil
}

func needPoint(i int, x, y *int, xf, yf string, g Geometry) error {
	if x == nil {
		return invalid(i, xf, "The event needs a coordinate.")
	}
	if y == nil {
		return invalid(i, yf, "The event needs a coordinate.")
	}
	return optionalPoint(i, x, y, xf, yf, g)
}

// optionalPoint refuses a half-given point and a point off the desktop. A
// coordinate outside the geometry is refused rather than clamped: a gesture
// that lands somewhere else is not the gesture the caller asked for.
func optionalPoint(i int, x, y *int, xf, yf string, g Geometry) error {
	if (x == nil) != (y == nil) {
		return &Invalid{Paths: []string{eventPath(i, xf), eventPath(i, yf)}, Detail: "A point is both coordinates or neither."}
	}
	if x == nil {
		return nil
	}
	if *x < 0 || *x >= g.Width {
		return invalid(i, xf, fmt.Sprintf("The desktop is %d pixels wide.", g.Width))
	}
	if *y < 0 || *y >= g.Height {
		return invalid(i, yf, fmt.Sprintf("The desktop is %d pixels tall.", g.Height))
	}
	return nil
}

// Argv is the argument lists one validated event becomes inside the sandbox,
// in order. A wait becomes none: the driver sleeps rather than running a
// command, so a batch that pauses costs nothing inside the sandbox.
//
// Modifiers held around a click are pressed and released by their own
// commands, because the input tool's click has no modifier flag. The click
// itself then carries no clear-modifiers flag, which would release the keys
// the command before it pressed.
func Argv(e InputEvent) [][]string {
	switch e.Type {
	case TypeMove:
		return [][]string{move(*e.X, *e.Y)}
	case TypeMouseDown:
		return held(e, point(e), []string{"xdotool", "mousedown", button(e)})
	case TypeMouseUp:
		return held(e, point(e), []string{"xdotool", "mouseup", button(e)})
	case TypeClick:
		return held(e, point(e), click(e, 1))
	case TypeDoubleClick:
		return held(e, point(e), click(e, 2))
	case TypeTripleClick:
		return held(e, point(e), click(e, 3))
	case TypeDrag:
		return held(e, nil,
			move(*e.X, *e.Y),
			[]string{"xdotool", "mousedown", button(e)},
			move(*e.ToX, *e.ToY),
			[]string{"xdotool", "mouseup", button(e)})
	case TypeScroll:
		return held(e, point(e),
			[]string{"xdotool", "click", "--repeat", strconv.Itoa(e.Amount), strconv.Itoa(wheels[e.Direction])})
	case TypeKey:
		return [][]string{{"xdotool", "key", "--clearmodifiers", chord(e.Modifiers, e.Key)}}
	case TypeType:
		return [][]string{{"xdotool", "type", "--clearmodifiers", "--", e.Text}}
	default:
		return nil
	}
}

// point is the move that precedes a gesture carrying a coordinate, or nil.
func point(e InputEvent) []string {
	if e.X == nil {
		return nil
	}
	return move(*e.X, *e.Y)
}

func move(x, y int) []string {
	return []string{"xdotool", "mousemove", "--", strconv.Itoa(x), strconv.Itoa(y)}
}

func button(e InputEvent) string { return strconv.Itoa(buttons[e.Button]) }

func click(e InputEvent, repeat int) []string {
	out := []string{"xdotool", "click"}
	if len(e.Modifiers) == 0 {
		out = append(out, "--clearmodifiers")
	}
	if repeat > 1 {
		out = append(out, "--repeat", strconv.Itoa(repeat))
	}
	return append(out, button(e))
}

// held wraps the gesture in the modifier presses it runs under, after the
// optional move that positions it.
func held(e InputEvent, lead []string, gesture ...[]string) [][]string {
	var out [][]string
	if lead != nil {
		out = append(out, lead)
	}
	keys := chordPrefix(e.Modifiers)
	if keys != "" {
		out = append(out, []string{"xdotool", "keydown", keys})
	}
	out = append(out, gesture...)
	if keys != "" {
		out = append(out, []string{"xdotool", "keyup", keys})
	}
	return out
}

func chord(mods []string, key string) string {
	if p := chordPrefix(mods); p != "" {
		return p + "+" + key
	}
	return key
}

func chordPrefix(mods []string) string { return strings.Join(mods, "+") }
