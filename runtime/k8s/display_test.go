// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/display"
)

// displayImage is what an operator configures the desktop container with. It
// is an example registry, so no deployment's image reaches this package.
const displayImage = "registry.example.com/cella-display:1"

// desk is the desktop every display case asks for.
var desk = driver.Geometry{Width: 1280, Height: 800}

// withDisplay configures a driver an operator gave a display image.
func withDisplay(o *Options) { o.DisplayImage = displayImage }

// desktopHarness is a driver with a display image whose Pods come up with both
// containers ready, so the desktop reads as up.
func desktopHarness(t *testing.T, objects ...kruntime.Object) *harness {
	t.Helper()
	h := newHarnessWith(t, withDisplay, objects...)
	h.status = bothReady
	return h
}

// bothReady is the kubelet's part for a Pod carrying a desktop.
func bothReady(pod *corev1.Pod) {
	running(pod)
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses,
			corev1.ContainerStatus{Name: c.Name, Ready: true})
	}
}

// TestCapabilitiesFollowTheDisplayImage: an operator who configured no image
// has no desktop to give, and a manifest that asks for one is then refused at
// resolve, where the refusal names the field.
func TestCapabilitiesFollowTheDisplayImage(t *testing.T) {
	plain := newHarness(t)
	if c := plain.Capabilities(); c.Display || c.Input {
		t.Fatalf("a driver with no display image declares %+v", c)
	}
	desktop := newHarnessWith(t, withDisplay)
	if c := desktop.Capabilities(); !c.Display || !c.Input {
		t.Fatalf("a driver with a display image declares %+v", c)
	}
	// A create that asks for a desktop the driver cannot build is refused
	// rather than answered with a Pod that has no screen in it.
	_, err := plain.Create(t.Context(), driver.CreateSpec{ID: "sbx_a", Image: image, Display: &desk})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("a desktop with no image: %v", err)
	}
}

// TestPodWithDisplay is the container spec the cluster receives: the desktop
// beside the workload, the shared /tmp, the geometry, the ready probe and the
// same hardened context the workload carries.
func TestPodWithDisplay(t *testing.T) {
	h := desktopHarness(t)
	pod, err := h.pod(driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Display: &desk}, h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("the Pod has %d containers, want the workload and the desktop", len(pod.Spec.Containers))
	}
	desktop := pod.Spec.Containers[1]
	if desktop.Name != DisplayContainer || desktop.Image != displayImage {
		t.Fatalf("the desktop container is %s from %s", desktop.Name, desktop.Image)
	}
	if !slices.Equal(desktop.Command, display.Shell(display.StartScript)) {
		t.Fatalf("the desktop runs %v", desktop.Command)
	}
	env := map[string]string{}
	for _, e := range desktop.Env {
		env[e.Name] = e.Value
	}
	if !maps.Equal(env, display.Env(desk)) {
		t.Fatalf("the desktop's environment is %v, want %v", env, display.Env(desk))
	}
	// One emptyDir carries the X socket to both containers, which is the
	// writable mount the baseline already grants.
	mounts := map[string]string{}
	for _, m := range desktop.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if !maps.Equal(mounts, map[string]string{"tmp": "/tmp"}) {
		t.Fatalf("the desktop mounts %v", mounts)
	}
	if !slices.ContainsFunc(pod.Spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "tmp" && m.MountPath == "/tmp"
	}) {
		t.Fatalf("the workload does not share the desktop's /tmp: %v", pod.Spec.Containers[0].VolumeMounts)
	}
	if pod.Spec.HostIPC || (pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace) {
		t.Fatal("the desktop was given the host's IPC or a shared process namespace")
	}
	// The desktop is held to the same baseline as the workload.
	if !reflectEqualContext(desktop.SecurityContext, hardened()) {
		t.Fatalf("the desktop's security context is %+v", desktop.SecurityContext)
	}
	probe := desktop.ReadinessProbe
	if probe == nil || probe.Exec == nil || !slices.Equal(probe.Exec.Command, display.Shell(display.ReadyScript())) {
		t.Fatalf("the desktop's readiness probe is %+v", probe)
	}
	// The workload is told which screen to draw on and nothing else of the
	// desktop's own environment.
	workload := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		workload[e.Name] = e.Value
	}
	if !maps.Equal(workload, map[string]string{display.DisplayEnv: display.DisplayValue}) {
		t.Fatalf("the workload's environment is %v", workload)
	}

	// A sandbox that asked for no desktop gets one container and no variable.
	plain, err := h.pod(driver.CreateSpec{ID: "sbx_b", Name: "b", Owner: "alice", Image: image}, h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Spec.Containers) != 1 || len(plain.Spec.Containers[0].Env) != 0 {
		t.Fatalf("a sandbox with no desktop renders %d containers and %v", len(plain.Spec.Containers), plain.Spec.Containers[0].Env)
	}
}

// reflectEqualContext compares two security contexts by their rendered form,
// which is enough for a case that asks whether the baseline was applied.
func reflectEqualContext(got, want *corev1.SecurityContext) bool {
	return got != nil && want != nil &&
		*got.ReadOnlyRootFilesystem == *want.ReadOnlyRootFilesystem &&
		*got.AllowPrivilegeEscalation == *want.AllowPrivilegeEscalation &&
		*got.RunAsNonRoot == *want.RunAsNonRoot &&
		*got.Privileged == *want.Privileged &&
		slices.Equal(got.Capabilities.Drop, want.Capabilities.Drop)
}

// TestDisplayReadyIsTheContainersOwn: the probe runs in the cluster, so the
// condition is read off the Pod and the control plane runs no command for it.
// A desktop that has not come up leaves the sandbox running.
func TestDisplayReadyIsTheContainersOwn(t *testing.T) {
	h := desktopHarness(t)
	state := h.created(t, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Display: &desk})
	if len(state.Conditions) != 1 || state.Conditions[0].Status != v1.ConditionTrue {
		t.Fatalf("the conditions are %+v, want DisplayReady true", state.Conditions)
	}
	if h.exec.count() != 0 {
		t.Fatalf("reading the condition ran %v", h.exec.ran())
	}

	// A Pod whose desktop has not registered is a running sandbox with the
	// condition false, never one stuck at Starting.
	half := newHarnessWith(t, withDisplay)
	half.status = func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: Container, Ready: true},
			{Name: DisplayContainer, Ready: false},
		}
	}
	state = half.created(t, driver.CreateSpec{ID: "sbx_b", Name: "b", Owner: "alice", Image: image, Display: &desk})
	if state.Phase != driver.Running {
		t.Fatalf("a sandbox whose desktop is coming up is %q, want Running", state.Phase)
	}
	if state.Conditions[0].Status != v1.ConditionFalse || state.Conditions[0].Message == "" {
		t.Fatalf("the condition is %+v", state.Conditions[0])
	}

	// A sandbox with no desktop carries no condition at all.
	plain := newHarnessWith(t, withDisplay)
	state = plain.created(t, spec("sbx_c"))
	if len(state.Conditions) != 0 {
		t.Fatalf("a sandbox with no desktop carries %+v", state.Conditions)
	}
}

// TestDisplayGeometryAndOperations drives the four operations against the
// display container.
func TestDisplayGeometryAndOperations(t *testing.T) {
	h := desktopHarness(t)
	h.created(t, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Display: &desk})
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		if strings.Contains(strings.Join(c.argv, " "), "xwd") {
			_, _ = io.WriteString(stdout, "\x89PNG")
		}
		return nil
	}
	got, err := h.Display(t.Context(), "sbx_a")
	if err != nil || got != desk {
		t.Fatalf("Display = %+v, %v", got, err)
	}
	frame, err := h.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(frame)
	if string(body) != "\x89PNG" {
		t.Fatalf("the frame is %q", body)
	}
	_ = frame.Close()
	// Every command on the screen runs in the desktop's container: the tools
	// live in its image and not in the sandbox's.
	for _, call := range h.exec.ran() {
		if call.container != DisplayContainer {
			t.Fatalf("a desktop command ran in %s: %v", call.container, call.argv)
		}
	}

	if err = h.Input(t.Context(), "sbx_a", []driver.InputEvent{{Type: display.TypeClick, Button: "left", X: ptr[int](4), Y: ptr[int](5)}}); err != nil {
		t.Fatal(err)
	}
	last := h.exec.last()
	if !slices.Equal(last.argv, []string{"xdotool", "click", "--clearmodifiers", "1"}) {
		t.Fatalf("the click ran %v", last.argv)
	}
	// A coordinate off this sandbox's own screen never reaches the tool.
	before := h.exec.count()
	err = h.Input(t.Context(), "sbx_a", []driver.InputEvent{{Type: display.TypeClick, Button: "left", X: ptr[int](5000), Y: ptr[int](5)}})
	if !errors.Is(err, driver.ErrInvalid) || h.exec.count() != before {
		t.Fatalf("a click off the screen: %v, and %d commands ran", err, h.exec.count()-before)
	}

	// A sandbox with no desktop answers neither operation.
	h.created(t, spec("sbx_b"))
	if _, err = h.Display(t.Context(), "sbx_b"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Display on a sandbox with no desktop: %v", err)
	}
	if err = h.Input(t.Context(), "sbx_b", []driver.InputEvent{{Type: display.TypeClick, Button: "left"}}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Input on a sandbox with no desktop: %v", err)
	}
}

// TestOperationsNeedAReadyDesktop: every operation on the screen is refused
// while the desktop is coming up, and the refusal names the condition.
func TestOperationsNeedAReadyDesktop(t *testing.T) {
	h := desktopHarness(t)
	h.created(t, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Display: &desk})
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, _, _ io.Writer) error {
		if strings.Contains(strings.Join(c.argv, " "), "xdpyinfo") {
			return errors.New("exit status 1")
		}
		return nil
	}
	if _, err := h.Screenshot(t.Context(), "sbx_a", driver.ScreenshotRequest{}); !errors.Is(err, driver.ErrDisplayNotReady) {
		t.Fatalf("a screenshot of a desktop that is not up: %v", err)
	}
	if err := h.Input(t.Context(), "sbx_a", []driver.InputEvent{{Type: display.TypeClick, Button: "left"}}); !errors.Is(err, driver.ErrDisplayNotReady) {
		t.Fatalf("input on a desktop that is not up: %v", err)
	}
	if _, err := h.Screen(t.Context(), "sbx_a", 5, display.FormatPNG); !errors.Is(err, driver.ErrDisplayNotReady) {
		t.Fatalf("a stream of a desktop that is not up: %v", err)
	}
}

// TestScreenFrames is the stream: one command for the whole session, frames
// read off it, and the session ended inside the sandbox when the stream is
// done.
func TestScreenFrames(t *testing.T) {
	h := desktopHarness(t)
	h.created(t, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Display: &desk})
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		if strings.Contains(strings.Join(c.argv, " "), "while test -e") {
			_, _ = io.WriteString(stdout, "00000003one00000003two")
		}
		return nil
	}
	frames, err := h.Screen(t.Context(), "sbx_a", 10, display.FormatPNG)
	if err != nil {
		t.Fatal(err)
	}
	frame, ok := <-frames
	if !ok || string(frame.Data) != "one" {
		t.Fatalf("the first frame is %q (open %v)", frame.Data, ok)
	}
	for range frames { //nolint:revive // draining what the session produced
	}
	var streams, ends int
	for _, call := range h.exec.ran() {
		joined := strings.Join(call.argv, " ")
		switch {
		case strings.Contains(joined, "while test -e"):
			streams++
		case strings.Contains(joined, "rm -f "+display.HomeDir+"/session."):
			ends++
		}
	}
	if streams != 1 || ends != 1 {
		t.Fatalf("the session ran %d stream commands and %d ends", streams, ends)
	}
}

// TestPortsProbe reads the kernel's socket table in the workload's container,
// which is what binds, and only for a running sandbox.
func TestPortsProbe(t *testing.T) {
	h := desktopHarness(t)
	ports := []driver.Port{{Name: "web", Port: 8080}, {Name: "api", Port: 9090}}
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		if strings.Contains(strings.Join(c.argv, " "), "/proc/net/tcp") {
			_, _ = io.WriteString(stdout, "  sl  local_address rem_address   st\n"+
				"   0: 00000000:1F90 00000000:0000 0A 0 0 0\n")
		}
		return nil
	}
	state := h.created(t, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Image: image, Ports: ports})
	want := []driver.PortState{
		{Name: "web", Port: 8080, State: driver.PortListening},
		{Name: "api", Port: 9090, State: driver.PortClosed},
	}
	if !slices.Equal(state.Ports, want) {
		t.Fatalf("the ports are %+v, want %+v", state.Ports, want)
	}
	if h.exec.last().container != Container {
		t.Fatalf("the probe ran in %s, want the workload's container", h.exec.last().container)
	}
	// A stopped sandbox is probed for nothing: there is no process to ask.
	if err := h.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.Inspect(t.Context(), "sbx_a")
	if err != nil || stopped.Ports != nil {
		t.Fatalf("a stopped sandbox reports %+v, %v", stopped.Ports, err)
	}
	// A sandbox that declared no port is probed for nothing either.
	before := h.exec.count()
	h.created(t, spec("sbx_b"))
	if h.exec.count() != before {
		t.Fatalf("a sandbox with no ports was probed: %v", h.exec.ran()[before:])
	}
}
