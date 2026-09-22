// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// screenSubprotocol is the one design 008 names for the screen socket. The
// frames carry their own shape, so a client that offers nothing still
// connects.
const screenSubprotocol = "cella.screen.v1"

// maxInputBytes bounds one input batch. It is the route's own cap and not the
// manifest's: design 023 requires 256 events of a kibibyte of text each to
// fit, and the manifest cap is two orders smaller.
const maxInputBytes = 1 << 20

// defaultFPS is the pace a screen session takes when the request names none.
const defaultFPS = 5

var screenUpgrader = websocket.Upgrader{
	Subprotocols: []string{screenSubprotocol},
	// The origin is not the boundary here: every frame travels under a bearer
	// the handshake already verified.
	CheckOrigin:      func(*http.Request) bool { return true },
	HandshakeTimeout: 10 * time.Second,
}

// displayStatus is what GET .../display answers: the geometry the sandbox was
// created with and whether its desktop has come up.
type displayStatus struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	Ready  bool `json:"ready"`
}

// portList is what GET .../ports answers, in the shape every list route takes.
type portList struct {
	Items []v1.PortStatus `json:"items"`
	Next  string          `json:"next"`
}

// inputBatch is the body of POST .../input.
type inputBatch struct {
	Events []runtime.InputEvent `json:"events"`
}

// inputResult is the answer: how many events ran, and where the batch stopped
// when one of them was refused by the desktop.
type inputResult struct {
	Executed int          `json:"executed"`
	Failed   *inputFailed `json:"failed,omitempty"`
}
type inputFailed struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// desktop reads, authorizes and gates in one place: the object, the action its
// row names, and the capability the route needs before any driver call. A
// route that needs the pointer as well names it, because a screen with no way
// to act on it serves a screenshot and refuses a gesture.
func (h *handler) desktop(w http.ResponseWriter, r *http.Request, action string, pointer bool) (v1.Sandbox, bool) {
	obj, err := h.authorizedObject(r, action)
	if err != nil {
		respondError(w, err)
		return obj, false
	}
	caps := h.Controller.CapabilitiesOf(obj.Status.Environment)
	if !caps.Display {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "the environment provides no desktop"})
		return obj, false
	}
	if pointer && !caps.Input {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "the environment accepts no input"})
		return obj, false
	}
	return obj, true
}

// display serves GET /v1/sandboxes/{id}/display: the geometry and whether the
// desktop is up. A sandbox that asked for no screen is not found.
func (h *handler) display(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.desktop(w, r, authorizer.ActionSandboxRead, false)
	if !ok {
		return
	}
	geometry, err := h.Controller.Display(r.Context(), obj.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	// The condition is the driver's, read through the same refresh every read
	// of the sandbox takes.
	fresh, err := h.Controller.Refresh(r.Context(), obj)
	if err != nil {
		respondError(w, err)
		return
	}
	respond(w, http.StatusOK, displayStatus{
		Width: geometry.Width, Height: geometry.Height, Ready: conditionHolds(fresh, v1.ConditionDisplayReady),
	})
}

// conditionHolds reports whether one of the sandbox's conditions is true.
func conditionHolds(obj v1.Sandbox, kind string) bool {
	for _, condition := range obj.Status.Conditions {
		if condition.Type == kind {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

// ports serves GET /v1/sandboxes/{id}/ports: every declared port with the
// driver's probe of it.
func (h *handler) ports(w http.ResponseWriter, r *http.Request) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxRead)
	if err != nil {
		respondError(w, err)
		return
	}
	fresh, err := h.Controller.Refresh(r.Context(), obj)
	if err != nil {
		respondError(w, err)
		return
	}
	items := fresh.Status.Ports
	if items == nil {
		items = []v1.PortStatus{}
	}
	respond(w, http.StatusOK, portList{Items: items})
}

// screenshot serves GET /v1/sandboxes/{id}/screenshot: one frame, as the
// image it is.
func (h *handler) screenshot(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.desktop(w, r, authorizer.ActionSandboxExec, false)
	if !ok {
		return
	}
	req, err := screenshotRequest(r)
	if err != nil {
		respondError(w, err)
		return
	}
	geometry, err := h.Controller.Display(r.Context(), obj.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	frame, err := h.Controller.Screenshot(r.Context(), obj.Status.ID, req)
	if err != nil {
		respondError(w, err)
		return
	}
	defer func() { _ = frame.Close() }()
	h.touch(r, obj)
	stream := newStream(w, display.MediaType(req.Format))
	if _, err = io.Copy(stream, frame); err != nil {
		stream.fail(err)
		return
	}
	// The record names the frame's size and its encoding, and no pixel of it.
	h.emit(r, obj, events.TypeScreenshot, events.Screenshot{
		Width: int(float64(geometry.Width) * req.Scale), Height: int(float64(geometry.Height) * req.Scale),
		Format: req.Format,
	})
}

// screenshotRequest reads the two selectors design 023 names, and refuses a
// value outside their range rather than clamping it.
func screenshotRequest(r *http.Request) (runtime.ScreenshotRequest, error) {
	q := r.URL.Query()
	req := runtime.ScreenshotRequest{Format: q.Get("format")}
	if raw := q.Get("scale"); raw != "" {
		scale, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return req, &manifest.Error{Code: "invalid_field", Path: "scale", Detail: "scale is a number between 0.1 and 1.0"}
		}
		req.Scale = scale
	}
	normalized, err := req.Normalize()
	if err != nil {
		return req, invalidField(err)
	}
	return normalized, nil
}

// invalidField turns a refusal of the desktop's own rules into the contract's
// error, carrying the paths it named so a caller reads which field it is.
func invalidField(err error) error {
	var refusal *display.Invalid
	if !errors.As(err, &refusal) {
		return err
	}
	out := &manifest.Error{Code: "invalid_field", Detail: refusal.Detail, Paths: refusal.Paths}
	if len(refusal.Paths) > 0 {
		out.Path = refusal.Paths[0]
	}
	return out
}

// input serves POST /v1/sandboxes/{id}/input: one batch, validated whole, then
// executed in order. A batch that stops part way is still a 200: how much of
// the gesture landed is the answer, not a failure that says nothing.
func (h *handler) input(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.desktop(w, r, authorizer.ActionSandboxExec, true)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInputBytes))
	if err != nil {
		respondError(w, err)
		return
	}
	var batch inputBatch
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&batch); err != nil {
		respondError(w, &manifest.Error{Code: "bad_request", Detail: "the body is one JSON object carrying events"})
		return
	}
	geometry, err := h.Controller.Display(r.Context(), obj.Status.ID)
	if err != nil {
		respondError(w, err)
		return
	}
	if err = display.Validate(batch.Events, geometry); err != nil {
		respondError(w, invalidField(err))
		return
	}
	h.touch(r, obj)
	result := inputResult{Executed: len(batch.Events)}
	err = h.Controller.Input(r.Context(), obj.Status.ID, batch.Events)
	var failure *runtime.InputFailure
	switch {
	case errors.As(err, &failure):
		result.Executed = failure.Executed
		result.Failed = &inputFailed{Index: failure.Index, Reason: reasonOf(failure)}
	case err != nil:
		respondError(w, err)
		return
	}
	// The record names how many events the batch carried, and no key, button
	// or character of what was typed.
	h.emit(r, obj, events.TypeInput, events.Input{Events: len(batch.Events)})
	respond(w, http.StatusOK, result)
}

// reasonOf is the developer sentence for the event a batch stopped at. It is
// the driver's own, which names the tool and its exit and never the gesture.
func reasonOf(failure *runtime.InputFailure) string {
	if failure.Err == nil {
		return "the desktop refused the event"
	}
	return failure.Err.Error()
}

// screen serves GET /v1/sandboxes/{id}/screen: a WebSocket of binary frames,
// server-paced. The session closes with 1000 when the sandbox's stream ends,
// which a stop causes, and 1011 when it fails.
func (h *handler) screen(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.desktop(w, r, authorizer.ActionSandboxExec, false)
	if !ok {
		return
	}
	fps, format, err := screenRequest(r)
	if err != nil {
		respondError(w, err)
		return
	}
	frames, err := h.Controller.Screen(r.Context(), obj.Status.ID, fps, format)
	if err != nil {
		respondError(w, err)
		return
	}
	conn, err := screenUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has written the reply, so the caller already has the reason.
		return
	}
	defer func() { _ = conn.Close() }()
	h.touch(r, obj)
	h.pump(r, conn, obj, frames)
}

// screenRequest reads the pace and the encoding, and refuses a value outside
// the contract rather than clamping it.
func screenRequest(r *http.Request) (fps int, format string, err error) {
	q := r.URL.Query()
	fps = defaultFPS
	if raw := q.Get("fps"); raw != "" {
		fps, err = strconv.Atoi(raw)
		if err != nil || fps < 1 || fps > display.MaxFPS {
			return 0, "", &manifest.Error{Code: "invalid_field", Path: "fps", Detail: "fps is between 1 and 10"}
		}
	}
	format = q.Get("format")
	if format == "" {
		format = display.FormatPNG
	}
	if format != display.FormatPNG && format != display.FormatJPEG {
		return 0, "", &manifest.Error{Code: "invalid_field", Path: "format", Detail: "a frame is encoded as png or jpeg"}
	}
	return fps, format, nil
}

// pump writes each frame as it arrives and watches the client for its close.
// Activity is stamped per frame, coalesced by the controller, so a watched
// sandbox is not reaped while somebody is looking at it.
func (h *handler) pump(r *http.Request, conn *websocket.Conn, obj v1.Sandbox, frames <-chan runtime.Frame) {
	writer := &frameWriter{conn: conn}
	started := time.Now()
	var out int64
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		conn.SetReadLimit(1 << 10)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case frame, open := <-frames:
			if !open {
				closeWith(writer, websocket.CloseNormalClosure, "the session ended")
				h.emitScreen(r, obj, started, out)
				return
			}
			if err := writer.write(websocket.BinaryMessage, frame.Data); err != nil {
				h.emitScreen(r, obj, started, out)
				return
			}
			out += int64(len(frame.Data))
			h.touch(r, obj)
		case <-ticker.C:
			if err := writer.write(websocket.PingMessage, nil); err != nil {
				h.emitScreen(r, obj, started, out)
				return
			}
		case <-gone:
			h.emitScreen(r, obj, started, out)
			return
		case <-r.Context().Done():
			closeWith(writer, websocket.CloseGoingAway, "the server is closing the session")
			h.emitScreen(r, obj, started, out)
			return
		}
	}
}

// emitScreen writes the session's record: how long it ran and how much moved
// each way. A screen session carries nothing in, so the count is zero and the
// field is there because design 009's shape is one shape for every session.
//
// The record is written on a context detached from the request's, because the
// usual way a session ends is the caller going away, which cancels it. What
// happened is still a fact the feed carries.
func (h *handler) emitScreen(r *http.Request, obj v1.Sandbox, started time.Time, out int64) {
	h.emit(r.WithContext(context.WithoutCancel(r.Context())), obj, events.TypeScreen, events.Screen{
		DurationMS: time.Since(started).Milliseconds(), BytesOut: out,
	})
}
