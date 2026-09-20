// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// Display answers the geometry of one sandbox's desktop. A driver that
// declares no Display has no DisplayDriver, which the API reports as the
// capability the environment lacks.
func (c *Controller) Display(ctx context.Context, id string) (runtime.Geometry, error) {
	d, ok := c.driver.(runtime.DisplayDriver)
	if !ok {
		return runtime.Geometry{}, runtime.ErrUnsupported
	}
	return d.Display(ctx, id)
}

// Screenshot is one frame of a sandbox's desktop.
func (c *Controller) Screenshot(ctx context.Context, id string, req runtime.ScreenshotRequest) (io.ReadCloser, error) {
	d, ok := c.driver.(runtime.DisplayDriver)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return d.Screenshot(ctx, id, req)
}

// Screen opens one paced sequence of frames. Cancelling the context ends the
// session; the channel closes when the sandbox or the session does.
func (c *Controller) Screen(ctx context.Context, id string, fps int, format string) (<-chan runtime.Frame, error) {
	d, ok := c.driver.(runtime.DisplayDriver)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return d.Screen(ctx, id, fps, format)
}

// Input runs one validated batch of pointer and keyboard events.
func (c *Controller) Input(ctx context.Context, id string, events []runtime.InputEvent) error {
	d, ok := c.driver.(runtime.InputDriver)
	if !ok {
		return runtime.ErrUnsupported
	}
	return d.Input(ctx, id, events)
}

// geometryOf carries the manifest's desktop into the driver's own type, and
// nil where the manifest asked for none.
func geometryOf(d *v1.Display) *runtime.Geometry {
	if d == nil {
		return nil
	}
	return &runtime.Geometry{Width: d.Width, Height: d.Height}
}

// portsOf carries the manifest's declared ports into the driver's own type.
func portsOf(ports []v1.Port) []runtime.Port {
	if len(ports) == 0 {
		return nil
	}
	out := make([]runtime.Port, len(ports))
	for i, p := range ports {
		out[i] = runtime.Port{Name: p.Name, Port: p.Port, Expose: string(p.Expose)}
	}
	return out
}

// portStatusOf carries the driver's probe back into the manifest's status.
func portStatusOf(ports []runtime.PortState) []v1.PortStatus {
	if len(ports) == 0 {
		return nil
	}
	out := make([]v1.PortStatus, len(ports))
	for i, p := range ports {
		out[i] = v1.PortStatus{Name: p.Name, Port: p.Port, State: p.State, URL: p.URL}
	}
	return out
}
