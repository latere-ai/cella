// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// On k8s the desktop is a second container in the Pod, from the display image
// of spec 014, and /tmp is one emptyDir both containers mount. The X socket
// under /tmp/.X11-unix is therefore shared without host IPC and without a
// shared process namespace, and the workload's image needs none of the
// desktop's tools.
var (
	_ driver.DisplayDriver = (*Driver)(nil)
	_ driver.InputDriver   = (*Driver)(nil)
)

// The readiness probe's timings. The desktop takes a second or two to come up,
// so the probe starts early and repeats often; its failure is DisplayReady
// false and never the Pod restarting, because the container has no restart
// policy of its own and the sandbox's readiness is the workload's.
const (
	displayProbePeriod  = 2
	displayProbeTimeout = 3
)

// displayContainer is the desktop beside the workload. It carries the same
// hardened context as the workload, because an X server, a window manager and
// the two tools all run with a read-only root once their home and their
// runtime directory are under the shared /tmp.
func (d *Driver) displayContainer(g driver.Geometry) (corev1.Container, error) {
	if d.opts.DisplayImage == "" {
		return corev1.Container{}, fmt.Errorf("%w: this driver has no display image configured", driver.ErrUnsupported)
	}
	limits, err := d.resources(d.displayResources())
	if err != nil {
		return corev1.Container{}, err
	}
	return corev1.Container{
		Name:    DisplayContainer,
		Image:   d.opts.DisplayImage,
		Command: display.Shell(display.StartScript),
		Env:     env(display.Env(g)),
		// The desktop's own working directory is the home it writes into,
		// which is under the shared /tmp and never the workspace: nothing the
		// desktop writes belongs to the sandbox's files.
		WorkingDir: display.SharedPath,
		Resources:  limits,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "tmp", MountPath: display.SharedPath},
		},
		ReadinessProbe: &corev1.Probe{
			Exec:                &corev1.ExecAction{Command: display.Shell(display.ReadyScript())},
			PeriodSeconds:       displayProbePeriod,
			TimeoutSeconds:      displayProbeTimeout,
			InitialDelaySeconds: 1,
			FailureThreshold:    1,
		},
		SecurityContext: hardened(),
	}, nil
}

// displayResources is the compute the desktop container runs under: the
// operator's, or the driver's defaults where the operator named none.
func (d *Driver) displayResources() driver.Resources {
	r := d.opts.DisplayResources
	if r.CPU == "" {
		r.CPU = d.opts.DefaultCPU
	}
	if r.Memory == "" {
		r.Memory = d.opts.DefaultMemory
	}
	// The desktop writes into the shared /tmp and claims no disk of its own.
	r.Disk = ""
	return r
}

// displayReady reads the condition off the Pod. The probe runs in the cluster,
// so the condition costs the control plane nothing and answers for a List as
// well as for an Inspect.
func displayReady(pod *corev1.Pod) (v1.Condition, bool) {
	for _, container := range pod.Spec.Containers {
		if container.Name != DisplayContainer {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == DisplayContainer && status.Ready {
				return v1.Condition{Type: v1.ConditionDisplayReady, Status: v1.ConditionTrue, Reason: "DesktopUp"}, true
			}
		}
		return v1.Condition{
			Type: v1.ConditionDisplayReady, Status: v1.ConditionFalse, Reason: "DesktopStarting",
			Message: "The X server or the window manager has not registered yet.",
		}, true
	}
	return v1.Condition{}, false
}

// runner is the way into one sandbox the desktop's commands take: one exec
// into the display container, its output collected, a nonzero exit an error.
func (d *Driver) runner(pod, container string) display.Runner {
	return func(ctx context.Context, argv []string, stdout io.Writer) error {
		e, err := d.start(ctx, pod, execOpts{argv: argv, container: container}, 0)
		if err != nil {
			return err
		}
		defer func() { _ = e.Close() }()
		var stderr strings.Builder
		var read [2]error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, read[1] = io.Copy(&stderr, e.Stderr())
		}()
		_, read[0] = io.Copy(stdout, e.Stdout())
		<-done
		if read[0] != nil {
			return read[0]
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

// desktop is the geometry one running sandbox was created with.
func (d *Driver) desktop(ctx context.Context, id string) (driver.Geometry, error) {
	spec, err := d.runningSpec(ctx, id)
	if err != nil {
		return driver.Geometry{}, err
	}
	if spec.Display == nil {
		return driver.Geometry{}, driver.ErrNotFound
	}
	return *spec.Display, nil
}

// Display is the geometry the sandbox was created with. A sandbox that asked
// for no desktop is ErrNotFound.
func (d *Driver) Display(ctx context.Context, id string) (driver.Geometry, error) {
	return d.desktop(ctx, id)
}

// Screenshot is one frame of the desktop, captured in the display container.
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

// Screen opens one paced sequence of frames over a single exec. Ending the
// context ends the exec, which the cluster turns into the command being
// signalled; the session's own file is removed as well, so a loop that
// outlives its stream ends at its next turn.
func (d *Driver) Screen(ctx context.Context, id string, fps int, format string) (<-chan driver.Frame, error) {
	run, err := d.readyRunner(ctx, id)
	if err != nil {
		return nil, err
	}
	session := display.NewSession()
	e, err := d.start(ctx, objectName(id), execOpts{
		argv:      display.Shell(display.StreamScript(fps, format, session)),
		container: DisplayContainer,
	}, 0)
	if err != nil {
		return nil, err
	}
	end := func() {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := run(clean, display.Shell(display.EndStreamScript(session)), io.Discard); err != nil {
			slog.WarnContext(clean, "the screen session was not ended inside the sandbox",
				"sandbox", id, "error", err)
		}
	}
	return display.Frames(ctx, &execStream{exec: e}, format, end), nil
}

// execStream is one exec read as a stream: the command's standard output, and
// Close ending it.
type execStream struct{ exec driver.Exec }

func (s *execStream) Read(p []byte) (int, error) { return s.exec.Stdout().Read(p) }
func (s *execStream) Close() error               { return s.exec.Close() }

// Input runs one batch against the desktop, validated whole against this
// sandbox's own geometry before its first event runs.
func (d *Driver) Input(ctx context.Context, id string, events []driver.InputEvent) error {
	g, err := d.desktop(ctx, id)
	if err != nil {
		return err
	}
	if err = display.Validate(events, g); err != nil {
		return fmt.Errorf("%w: %w", driver.ErrInvalid, err)
	}
	run, err := d.readyRunner(ctx, id)
	if err != nil {
		return err
	}
	return display.Perform(ctx, run, events)
}

// readyRunner is the way into a sandbox whose desktop is up, or the reason it
// is not.
func (d *Driver) readyRunner(ctx context.Context, id string) (display.Runner, error) {
	run := d.runner(objectName(id), DisplayContainer)
	if err := display.Ready(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// probePorts reports each declared port against the kernel's socket table
// inside the sandbox. The workload's container is the one asked, because the
// two containers share no network namespace boundary inside the Pod and the
// workload is what binds. A probe that fails leaves every port closed rather
// than absent: a caller reading no ports would read a sandbox that declared
// none.
func (d *Driver) probePorts(ctx context.Context, id string, ports []driver.Port) []driver.PortState {
	if len(ports) == 0 {
		return nil
	}
	listening, err := display.PortsListening(ctx, d.runner(objectName(id), Container))
	if err != nil {
		slog.WarnContext(ctx, "the port probe did not answer", "sandbox", id, "error", err)
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
