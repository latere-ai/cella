// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// The desktop and the ports live in the workspace volume's labels, beside the
// rest of the identity: both are fixed at create by spec 003, and the record
// volume holds only what changes.
const (
	labelDisplay = prefix + "display"
	labelPorts   = prefix + "ports"
)

// On podman the desktop is a second process in the sandbox's own container,
// so the sandbox's image carries the X server, the window manager and the two
// tools; the cella-display image is that image for a desktop sandbox. The
// driver writes the supervisor in and starts it, which is what keeps the
// container's own command the workload's.
var (
	_ driver.DisplayDriver = (*Driver)(nil)
	_ driver.InputDriver   = (*Driver)(nil)
)

// geometryLabel and geometryOf carry the desktop through the engine's labels.
func geometryLabel(g *driver.Geometry) string {
	if g == nil {
		return ""
	}
	return strconv.Itoa(g.Width) + "x" + strconv.Itoa(g.Height)
}

func geometryOf(label string) *driver.Geometry {
	width, height, ok := strings.Cut(label, "x")
	if !ok {
		return nil
	}
	w, err := strconv.Atoi(width)
	if err != nil {
		return nil
	}
	h, err := strconv.Atoi(height)
	if err != nil {
		return nil
	}
	return &driver.Geometry{Width: w, Height: h}
}

// portsLabel and portsOf carry the declared ports through the engine's labels
// as "name:port:expose", one per comma. A list is written once and read back;
// a label is a string, and a shape the reader can misparse is a shape the
// writer should not have used, so every field is required.
func portsLabel(ports []driver.Port) string {
	if len(ports) == 0 {
		return ""
	}
	out := make([]string, len(ports))
	for i, p := range ports {
		expose := p.Expose
		if expose == "" {
			expose = driver.ExposeNone
		}
		out[i] = p.Name + ":" + strconv.Itoa(p.Port) + ":" + expose
	}
	return strings.Join(out, ",")
}

func portsOf(label string) []driver.Port {
	if label == "" {
		return nil
	}
	var out []driver.Port
	for entry := range strings.SplitSeq(label, ",") {
		fields := strings.Split(entry, ":")
		if len(fields) != 3 {
			continue
		}
		port, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		out = append(out, driver.Port{Name: fields[0], Port: port, Expose: fields[2]})
	}
	return out
}

// desktop is the geometry one sandbox was created with, read from the engine.
func (d *Driver) desktop(ctx context.Context, id string) (*driver.Geometry, error) {
	vi, err := d.inspectVolume(ctx, workspaceVolume(id))
	if err != nil {
		return nil, err
	}
	return geometryOf(vi.Labels[labelDisplay]), nil
}

// runner is the way into one sandbox the desktop's commands take: one exec
// session per command, its output collected, a nonzero exit an error.
func (d *Driver) runner(id string) display.Runner {
	return func(ctx context.Context, argv []string, stdout io.Writer) error {
		e, err := d.Exec(ctx, id, driver.ExecRequest{Command: argv})
		if err != nil {
			return err
		}
		defer func() { _ = e.Close() }()
		var stderr strings.Builder
		var group [2]error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, group[1] = io.Copy(&stderr, e.Stderr())
		}()
		_, group[0] = io.Copy(stdout, e.Stdout())
		<-done
		if group[0] != nil {
			return group[0]
		}
		code, err := e.Wait(ctx)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("%s exited %d: %s", argv[0], code, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

// startDesktop brings the desktop up inside a sandbox that asked for one. It
// runs at create and at every start, because the supervisor lives in the
// process tree a stop ends, and it is idempotent, so a repeated call converges
// on one desktop. A failure is logged and not returned: a sandbox whose
// desktop did not come up is a running sandbox with DisplayReady false, which
// is what the condition is for.
func (d *Driver) startDesktop(ctx context.Context, id string, g *driver.Geometry) {
	if g == nil {
		return
	}
	if err := display.Install(ctx, d.runner(id), *g); err != nil {
		slog.WarnContext(ctx, "the sandbox's desktop did not start", "sandbox", id, "error", err)
	}
}

// Display is the geometry the sandbox was created with. A sandbox that asked
// for no desktop is ErrNotFound, so a caller tells an environment with no
// screen to give from a sandbox that wanted none.
func (d *Driver) Display(ctx context.Context, id string) (driver.Geometry, error) {
	g, err := d.desktop(ctx, id)
	if err != nil {
		return driver.Geometry{}, err
	}
	if g == nil {
		return driver.Geometry{}, driver.ErrNotFound
	}
	return *g, nil
}

// Screenshot is one frame of the desktop. The desktop has to be ready: a
// capture against an X server that is not up would answer an empty frame, and
// an empty frame is not a screenshot.
func (d *Driver) Screenshot(ctx context.Context, id string, req driver.ScreenshotRequest) (io.ReadCloser, error) {
	run, err := d.readyRunner(ctx, id)
	if err != nil {
		return nil, err
	}
	frame, err := display.Capture(ctx, run, req)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(frame))), nil
}

// Screen opens one paced sequence of frames over a single exec session. The
// session's own file inside the sandbox is what ends it: the engine has no way
// to end an exec session it started, so the loop watches for the file and the
// driver removes it when the stream is done.
func (d *Driver) Screen(ctx context.Context, id string, fps int, format string) (<-chan driver.Frame, error) {
	run, err := d.readyRunner(ctx, id)
	if err != nil {
		return nil, err
	}
	session := display.NewSession()
	e, err := d.Exec(ctx, id, driver.ExecRequest{Command: display.Shell(display.StreamScript(fps, format, session))})
	if err != nil {
		return nil, err
	}
	stream := &execStream{exec: e}
	end := func() {
		// The clean-up runs after the caller's context may already be done,
		// so it carries the context's values and none of its cancellation.
		clean := context.WithoutCancel(ctx)
		if err := run(clean, display.Shell(display.EndStreamScript(session)), io.Discard); err != nil {
			slog.WarnContext(clean, "the screen session was not ended inside the sandbox",
				"sandbox", id, "error", err)
		}
	}
	return display.Frames(ctx, stream, format, end), nil
}

// execStream is one exec session read as a stream: the command's standard
// output, and Close ending the session.
type execStream struct{ exec driver.Exec }

func (s *execStream) Read(p []byte) (int, error) { return s.exec.Stdout().Read(p) }
func (s *execStream) Close() error               { return s.exec.Close() }

// Input runs one batch against the desktop. The batch is validated against
// this sandbox's own geometry before its first event runs, so a coordinate off
// this screen is refused rather than clamped by the tool.
func (d *Driver) Input(ctx context.Context, id string, events []driver.InputEvent) error {
	g, err := d.desktop(ctx, id)
	if err != nil {
		return err
	}
	if g == nil {
		return driver.ErrNotFound
	}
	if err = display.Validate(events, *g); err != nil {
		return fmt.Errorf("%w: %w", driver.ErrInvalid, err)
	}
	run, err := d.readyRunner(ctx, id)
	if err != nil {
		return err
	}
	return display.Perform(ctx, run, events)
}

// readyRunner is the way into a sandbox whose desktop is up, or the reason it
// is not. Every operation on the screen takes it, so none of them acts on a
// desktop that is still coming up.
func (d *Driver) readyRunner(ctx context.Context, id string) (display.Runner, error) {
	run := d.runner(id)
	if err := display.Ready(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// observe is the driver's own reading of a running sandbox: whether its
// desktop is up and which of its declared ports something is listening on.
// Both are commands inside the sandbox, so both run at Inspect and neither at
// List: a sweep of every sandbox is the reaper's path, and a command per
// sandbox per tick is a cost it must not carry.
func (d *Driver) observe(ctx context.Context, id string, g *driver.Geometry, ports []driver.Port, running bool) ([]v1.Condition, []driver.PortState) {
	if !running || (g == nil && len(ports) == 0) {
		return nil, nil
	}
	run := d.runner(id)
	var conditions []v1.Condition
	if g != nil {
		conditions = append(conditions, displayCondition(display.Ready(ctx, run)))
	}
	return conditions, probePorts(ctx, run, ports)
}

// displayCondition turns the ready probe into the contract's condition.
func displayCondition(err error) v1.Condition {
	if err == nil {
		return v1.Condition{Type: v1.ConditionDisplayReady, Status: v1.ConditionTrue, Reason: "DesktopUp"}
	}
	return v1.Condition{
		Type: v1.ConditionDisplayReady, Status: v1.ConditionFalse, Reason: "DesktopStarting",
		Message: "The X server or the window manager has not registered yet.",
	}
}

// probePorts reports each declared port against the kernel's socket table
// inside the sandbox. A probe that fails leaves every port closed rather than
// absent: a caller reading no ports at all would read it as a sandbox that
// declared none.
func probePorts(ctx context.Context, run display.Runner, ports []driver.Port) []driver.PortState {
	if len(ports) == 0 {
		return nil
	}
	listening, err := display.PortsListening(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "the port probe did not answer", "error", err)
	}
	out := make([]driver.PortState, len(ports))
	for i, p := range ports {
		state := driver.PortClosed
		if listening[p.Port] {
			state = driver.PortListening
		}
		out[i] = driver.PortState{Name: p.Name, Port: p.Port, State: state}
	}
	return out
}

// displayEnv adds what a workload beside the desktop needs to address the
// screen: the display the X server listens on, whose socket is under the
// container's own /tmp. A sandbox that asked for no desktop gets nothing.
func displayEnv(s driver.CreateSpec, env map[string]string) map[string]string {
	if s.Display == nil {
		return env
	}
	out := make(map[string]string, len(env)+1)
	maps.Copy(out, env)
	out[display.DisplayEnv] = display.DisplayValue
	return out
}
