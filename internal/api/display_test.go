// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// deskDriver is a driver with a screen, wrapped around the native one so every
// other route behaves as it does elsewhere. It records the geometry each
// sandbox was created with, renders a frame of that size, and runs a batch
// against a rule a case arms.
type deskDriver struct {
	runtime.Driver
	mu sync.Mutex
	// screens is the geometry per sandbox, absent for one that asked for none.
	screens map[string]runtime.Geometry
	// noInput declares a screen with no way to act on it.
	noInput bool
	// failAt is the index of the event the desktop refuses, and -1 accepts
	// every batch.
	failAt int
	// batches counts what reached the driver.
	batches int
	// frames is what a screen session produces before it ends.
	frames int
	// hold blocks a screen session open until it is closed.
	hold chan struct{}
}

func newDeskDriver(d runtime.Driver) *deskDriver {
	return &deskDriver{Driver: d, screens: map[string]runtime.Geometry{}, failAt: -1, frames: 2}
}

func (d *deskDriver) Capabilities() runtime.Capabilities {
	c := d.Driver.Capabilities()
	c.Display = true
	c.Input = !d.noInput
	return c
}

func (d *deskDriver) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	if s.Display != nil {
		d.mu.Lock()
		d.screens[s.ID] = *s.Display
		d.mu.Unlock()
	}
	return d.Driver.Create(ctx, s)
}

func (d *deskDriver) Inspect(ctx context.Context, id string) (runtime.State, error) {
	state, err := d.Driver.Inspect(ctx, id)
	if err != nil {
		return state, err
	}
	d.mu.Lock()
	_, hasScreen := d.screens[id]
	d.mu.Unlock()
	if hasScreen {
		state.Conditions = append(state.Conditions,
			v1.Condition{Type: v1.ConditionDisplayReady, Status: v1.ConditionTrue, Reason: "DesktopUp"})
	}
	state.Ports = []runtime.PortState{{Name: "web", Port: 8080, State: runtime.PortListening}}
	return state, nil
}

func (d *deskDriver) geometry(id string) (runtime.Geometry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	g, ok := d.screens[id]
	if !ok {
		return runtime.Geometry{}, runtime.ErrNotFound
	}
	return g, nil
}

func (d *deskDriver) Display(_ context.Context, id string) (runtime.Geometry, error) {
	return d.geometry(id)
}

func (d *deskDriver) Screenshot(_ context.Context, id string, req runtime.ScreenshotRequest) (io.ReadCloser, error) {
	g, err := d.geometry(id)
	if err != nil {
		return nil, err
	}
	req, err = req.Normalize()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(pngOf(g, req.Scale))), nil
}

func pngOf(g runtime.Geometry, scale float64) []byte {
	img := image.NewRGBA(image.Rect(0, 0, int(float64(g.Width)*scale), int(float64(g.Height)*scale)))
	img.Set(0, 0, color.White)
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}

func (d *deskDriver) Screen(ctx context.Context, id string, _ int, format string) (<-chan runtime.Frame, error) {
	g, err := d.geometry(id)
	if err != nil {
		return nil, err
	}
	out := make(chan runtime.Frame, 1)
	go func() {
		defer close(out)
		for i := range d.frames {
			select {
			case out <- runtime.Frame{At: time.Now(), Format: format, Data: append([]byte(fmt.Sprint(i)), pngOf(g, 1)...)}:
			case <-ctx.Done():
				return
			}
		}
		if d.hold != nil {
			select {
			case <-d.hold:
			case <-ctx.Done():
			}
		}
	}()
	return out, nil
}

func (d *deskDriver) Input(_ context.Context, id string, events []runtime.InputEvent) error {
	if _, err := d.geometry(id); err != nil {
		return err
	}
	d.mu.Lock()
	d.batches++
	d.mu.Unlock()
	if d.failAt >= 0 && d.failAt < len(events) {
		return &runtime.InputFailure{Index: d.failAt, Executed: d.failAt, Err: errors.New("xdotool exited 1")}
	}
	return nil
}

// desktopBody is a manifest that asks for a screen.
const desktopBody = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
	`"metadata":{"name":"work"},"spec":{"display":{"width":1280,"height":800},` +
	`"network":{"ports":[{"name":"web","port":8080}]}}}`

// desk sets a fixture up over a driver with a screen and creates one sandbox
// with a desktop, answering its id.
func desk(t *testing.T, configure func(*deskDriver)) (*fixture, *deskDriver, string) {
	t.Helper()
	var d *deskDriver
	f := setupDriver(t, nil, func(base runtime.Driver) runtime.Driver {
		d = newDeskDriver(base)
		if configure != nil {
			configure(d)
		}
		return d
	})
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, desktopBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	return f, d, obj.Status.ID
}

// TestDisplayRoutes drives the five routes over real HTTP under a signed
// bearer: the geometry, the frame, the batch, the probe.
func TestDisplayRoutes(t *testing.T) {
	f, _, id := desk(t, nil)
	base := "/v1/sandboxes/" + id

	var status displayStatus
	if err := json.Unmarshal(f.request("GET", base+"/display", f.alice, "", 200), &status); err != nil {
		t.Fatal(err)
	}
	if status != (displayStatus{Width: 1280, Height: 800, Ready: true}) {
		t.Fatalf("the display is %+v", status)
	}

	frame := f.get(base+"/screenshot", f.alice, 200, "image/png")
	config, format, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("decoding the frame: %v", err)
	}
	if format != "png" || config.Width != 1280 || config.Height != 800 {
		t.Fatalf("the frame is %s %dx%d", format, config.Width, config.Height)
	}
	half := f.get(base+"/screenshot?scale=0.5", f.alice, 200, "image/png")
	if config, _, err = image.DecodeConfig(bytes.NewReader(half)); err != nil || config.Width != 640 {
		t.Fatalf("the half-scale frame is %d wide: %v", config.Width, err)
	}

	var result inputResult
	body := `{"events":[{"type":"move","x":10,"y":20},{"type":"click","button":"left"},{"type":"type","text":"hello"}]}`
	if err = json.Unmarshal(f.request("POST", base+"/input", f.alice, body, 200), &result); err != nil {
		t.Fatal(err)
	}
	if result.Executed != 3 || result.Failed != nil {
		t.Fatalf("the batch answered %+v", result)
	}

	var ports portList
	if err = json.Unmarshal(f.request("GET", base+"/ports", f.alice, "", 200), &ports); err != nil {
		t.Fatal(err)
	}
	want := []v1.PortStatus{{Name: "web", Port: 8080, State: v1.PortListening}}
	if len(ports.Items) != 1 || ports.Items[0] != want[0] {
		t.Fatalf("the ports are %+v, want %+v", ports.Items, want)
	}

	// A sandbox that asked for no screen is not found on the display routes,
	// and its ports list is empty rather than absent.
	plain := f.sandbox("plain")
	f.request("GET", "/v1/sandboxes/"+plain.Status.ID+"/display", f.alice, "", 404)
	f.request("GET", "/v1/sandboxes/"+plain.Status.ID+"/screenshot", f.alice, "", 404)
	f.request("POST", "/v1/sandboxes/"+plain.Status.ID+"/input", f.alice,
		`{"events":[{"type":"click","button":"left"}]}`, 404)
}

// TestDisplayRoutesAuthorize: every route reads, authorizes and only then
// acts, so another caller's sandbox is not found and never acted on.
func TestDisplayRoutesAuthorize(t *testing.T) {
	f, d, id := desk(t, nil)
	base := "/v1/sandboxes/" + id
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/display", ""},
		{"GET", "/screenshot", ""},
		{"GET", "/ports", ""},
		{"POST", "/input", `{"events":[{"type":"click","button":"left"}]}`},
	} {
		f.request(route.method, base+route.path, f.bob, route.body, 403)
		f.request(route.method, base+route.path, "", route.body, 401)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.batches != 0 {
		t.Fatalf("a refused request reached the driver %d times", d.batches)
	}
}

// TestDisplayCapabilityGate answers 422 before any driver call on an
// environment that has no screen, and on one that has a screen and no pointer.
func TestDisplayCapabilityGate(t *testing.T) {
	// An environment with no screen refuses the manifest field at resolve,
	// where the refusal names it, and every route at the gate.
	plain := setup(t, nil)
	refused := plain.request("POST", "/v1/sandboxes", plain.alice, desktopBody, 422)
	if !strings.Contains(string(refused), "capability_unsupported") {
		t.Fatalf("a desktop on an environment with none: %s", refused)
	}
	obj := plain.sandbox("plain")
	base := "/v1/sandboxes/" + obj.Status.ID
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/display", ""},
		{"GET", "/screenshot", ""},
		{"POST", "/input", `{"events":[{"type":"click","button":"left"}]}`},
	} {
		body := plain.request(route.method, base+route.path, plain.alice, route.body, 422)
		if !strings.Contains(string(body), "capability_unsupported") {
			t.Fatalf("%s %s answered %s", route.method, route.path, body)
		}
	}

	// A screen with no pointer serves a frame and refuses a gesture. The
	// sandbox is created against an environment that has both, because the
	// manifest field needs both; the pointer then goes, which is the state an
	// environment reconfigured under a live sandbox is in.
	f, d, id := desk(t, nil)
	d.mu.Lock()
	d.noInput = true
	d.mu.Unlock()
	base = "/v1/sandboxes/" + id
	f.get(base+"/screenshot", f.alice, 200, "image/png")
	body := f.request("POST", base+"/input", f.alice, `{"events":[{"type":"click","button":"left"}]}`, 422)
	if !strings.Contains(string(body), "capability_unsupported") {
		t.Fatalf("input on an environment with no pointer: %s", body)
	}
}

// TestInputValidation is the rule table over HTTP: a batch that breaks one is
// invalid_field with the path of the event it broke, and nothing runs.
func TestInputValidation(t *testing.T) {
	f, d, id := desk(t, nil)
	path := "/v1/sandboxes/" + id + "/input"
	for _, tc := range []struct{ name, body, want string }{
		{"noEvents", `{"events":[]}`, "events"},
		{"unknownType", `{"events":[{"type":"teleport"}]}`, "events[0].type"},
		{"offTheScreen", `{"events":[{"type":"click","button":"left","x":4000,"y":1}]}`, "events[0].x"},
		{"noButton", `{"events":[{"type":"click"}]}`, "events[0].button"},
		{"emptyText", `{"events":[{"type":"type","text":""}]}`, "events[0].text"},
		{"aChordIsNotAKeysym", `{"events":[{"type":"key","key":"ctrl+a"}]}`, "events[0].key"},
		{"aFlagIsNotAKeysym", `{"events":[{"type":"key","key":"-window"}]}`, "events[0].key"},
		{"unknownModifier", `{"events":[{"type":"key","key":"a","modifiers":["hyper"]}]}`, "events[0].modifiers"},
		{"waitTooLong", `{"events":[{"type":"wait","ms":20000}]}`, "events[0].ms"},
		{"scrollTooFar", `{"events":[{"type":"scroll","direction":"up","amount":99}]}`, "events[0].amount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := f.request("POST", path, f.alice, tc.body, 400)
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						Paths []string `json:"paths"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "invalid_field" {
				t.Fatalf("the code is %q: %s", envelope.Error.Code, body)
			}
			if !slices.Contains(envelope.Error.Details.Paths, tc.want) {
				t.Fatalf("the paths are %v, want %s", envelope.Error.Details.Paths, tc.want)
			}
		})
	}
	// A field the schema does not know is a bad request, not a silent drop.
	f.request("POST", path, f.alice, `{"events":[{"type":"click","button":"left"}],"speed":2}`, 400)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.batches != 0 {
		t.Fatalf("a refused batch reached the driver %d times", d.batches)
	}
}

// TestInputBodyCap: the route's own cap is a mebibyte, so a batch of 256
// one-kibibyte events fits and a body past the cap is refused before it is
// read as JSON.
func TestInputBodyCap(t *testing.T) {
	f, _, id := desk(t, nil)
	path := "/v1/sandboxes/" + id + "/input"
	events := make([]string, display.MaxEvents)
	for i := range events {
		events[i] = `{"type":"type","text":"` + strings.Repeat("x", display.MaxText) + `"}`
	}
	full := `{"events":[` + strings.Join(events, ",") + `]}`
	if len(full) <= 256*1024 {
		t.Fatalf("the full batch is %d bytes, which does not exercise the cap", len(full))
	}
	var result inputResult
	if err := json.Unmarshal(f.request("POST", path, f.alice, full, 200), &result); err != nil {
		t.Fatal(err)
	}
	if result.Executed != display.MaxEvents {
		t.Fatalf("the full batch answered %+v", result)
	}
	// One event more is refused by the rule, not by the cap.
	over := `{"events":[` + strings.Join(append(events, events[0]), ",") + `]}`
	f.request("POST", path, f.alice, over, 400)
	// A body past the cap is refused before anything is decoded.
	huge := `{"events":[{"type":"type","text":"` + strings.Repeat("y", maxInputBytes) + `"}]}`
	f.request("POST", path, f.alice, huge, 413)
}

// TestInputPartialFailure: a driver failure at event 5 of 10 answers 200 with
// what ran and where it stopped.
func TestInputPartialFailure(t *testing.T) {
	f, _, id := desk(t, func(d *deskDriver) { d.failAt = 4 })
	events := make([]string, 10)
	for i := range events {
		events[i] = `{"type":"click","button":"left"}`
	}
	body := f.request("POST", "/v1/sandboxes/"+id+"/input", f.alice,
		`{"events":[`+strings.Join(events, ",")+`]}`, 200)
	var result inputResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Executed != 4 || result.Failed == nil || result.Failed.Index != 4 {
		t.Fatalf("the batch answered %+v", result)
	}
	if result.Failed.Reason == "" {
		t.Fatal("the failure carries no reason")
	}
}

// TestScreenStream: binary frames over the socket design 008 names, and a
// close with 1000 when the sandbox's session ends.
func TestScreenStream(t *testing.T) {
	f, _, id := desk(t, nil)
	conn := f.openScreen("/v1/sandboxes/"+id+"/screen?fps=10", f.alice)
	for i := range 2 {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("reading frame %d: %v", i, err)
		}
		if kind != websocket.BinaryMessage {
			t.Fatalf("frame %d is kind %d, want binary", i, kind)
		}
		if _, _, err = image.DecodeConfig(bytes.NewReader(data[1:])); err != nil {
			t.Fatalf("frame %d is not an image: %v", i, err)
		}
	}
	_, _, err := conn.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.CloseNormalClosure {
		t.Fatalf("the session ended with %v, want a normal close", err)
	}
}

// TestScreenRefusesAValueOutsideTheContract: the pace and the encoding are
// refused before the upgrade, which is the only place a status code can say
// what is wrong.
func TestScreenRefusesAValueOutsideTheContract(t *testing.T) {
	f, _, id := desk(t, nil)
	base := "/v1/sandboxes/" + id
	for _, query := range []string{"?fps=0", "?fps=11", "?fps=x", "?format=gif"} {
		f.request("GET", base+"/screen"+query, f.alice, "", 400)
	}
	for _, query := range []string{"?scale=9", "?scale=x", "?format=gif"} {
		f.request("GET", base+"/screenshot"+query, f.alice, "", 400)
	}
}

// TestDisplayOperationsAreRecorded: each operation writes the record design
// 009 names, and no record carries a frame, a key or a character of text.
func TestDisplayOperationsAreRecorded(t *testing.T) {
	var d *deskDriver
	f := setupRecordedDriver(t, func(base runtime.Driver) runtime.Driver {
		d = newDeskDriver(base)
		return d
	})
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, desktopBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	f.get(base+"/screenshot", f.alice, 200, "image/png")
	const canary = "a-password-nobody-should-see"
	f.request("POST", base+"/input", f.alice,
		`{"events":[{"type":"type","text":"`+canary+`"},{"type":"key","key":"Return"}]}`, 200)

	var sawShot, sawInput bool
	for _, record := range f.records(obj.Status.ID) {
		if strings.Contains(string(record.Data), canary) || strings.Contains(string(record.Data), "Return") {
			t.Fatalf("a record carries what was typed: %s", record.Data)
		}
		switch record.Type {
		case events.TypeScreenshot:
			sawShot = true
			var data events.Screenshot
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Width != 1280 || data.Height != 800 || data.Format != "png" {
				t.Fatalf("the screenshot record is %+v", data)
			}
		case events.TypeInput:
			sawInput = true
			var data events.Input
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Events != 2 {
				t.Fatalf("the input record is %+v", data)
			}
		}
	}
	if !sawShot || !sawInput {
		t.Fatal("one of the two operations wrote no record")
	}
}

// get reads one route's body and holds its status and content type, for a
// route whose answer is an image rather than JSON.
func (f *fixture) get(path, token string, status int, media string) []byte {
	f.t.Helper()
	body := f.expect(status, "GET", path, token, "", "")
	if got := f.header.Get("Content-Type"); got != media {
		f.t.Fatalf("GET %s answered %q, want %q", path, got, media)
	}
	return body
}

// openScreen dials the screen socket with the subprotocol design 008 names.
func (f *fixture) openScreen(path, token string) *websocket.Conn {
	f.t.Helper()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	dialer := websocket.Dialer{Subprotocols: []string{screenSubprotocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.Dial("ws"+strings.TrimPrefix(f.url, "http")+path, header)
	if err != nil {
		f.t.Fatalf("dialing %s: %v", path, err)
	}
	if got := res.Header.Get("Sec-WebSocket-Protocol"); got != screenSubprotocol {
		f.t.Fatalf("the handshake agreed on %q, want %q", got, screenSubprotocol)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestScreenSessionEndsWithTheClient: a viewer that goes away ends the session
// and the record it wrote names how long it ran and how much left, and no
// frame.
func TestScreenSessionEndsWithTheClient(t *testing.T) {
	var d *deskDriver
	f := setupRecordedDriver(t, func(base runtime.Driver) runtime.Driver {
		d = newDeskDriver(base)
		d.hold = make(chan struct{})
		return d
	})
	t.Cleanup(func() { close(d.hold) })
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, desktopBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	conn := f.openScreen("/v1/sandboxes/"+obj.Status.ID+"/screen", f.alice)
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var session *events.Screen
		for _, record := range f.records(obj.Status.ID) {
			if record.Type != events.TypeScreen {
				continue
			}
			var data events.Screen
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			session = &data
		}
		if session != nil {
			if session.BytesOut <= 0 || session.DurationMS < 0 {
				t.Fatalf("the session record is %+v", session)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed session wrote no record")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestScreenStampsActivity: each frame is activity, so a sandbox nobody is
// typing in but somebody is watching is not idle.
func TestScreenStampsActivity(t *testing.T) {
	f, d, id := desk(t, nil)
	d.mu.Lock()
	d.hold = make(chan struct{})
	d.mu.Unlock()
	t.Cleanup(func() { close(d.hold) })
	before := f.activityOf(id)
	conn := f.openScreen("/v1/sandboxes/"+id+"/screen", f.alice)
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if f.activityOf(id).After(before) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("watching the screen stamped no activity past %v", before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// activityOf reads one sandbox's last activity through the route that
// refreshes it from the driver.
func (f *fixture) activityOf(id string) time.Time {
	f.t.Helper()
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/"+id, f.alice, "", 200), &obj); err != nil {
		f.t.Fatal(err)
	}
	return obj.Status.LastActivityAt
}

// TestDisplayRefusesADriverThatFails: a driver that cannot answer is the
// environment being unavailable, and the route says so rather than answering
// an empty frame.
func TestDisplayRefusesADriverThatFails(t *testing.T) {
	f, d, id := desk(t, nil)
	base := "/v1/sandboxes/" + id
	d.mu.Lock()
	delete(d.screens, id)
	d.mu.Unlock()
	f.request("GET", base+"/screenshot", f.alice, "", 404)
	f.request("GET", base+"/screen", f.alice, "", 404)
	f.request("POST", base+"/input", f.alice, `{"events":[{"type":"click","button":"left"}]}`, 404)
}
