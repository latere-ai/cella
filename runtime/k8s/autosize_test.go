// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"latere.ai/x/cella/autosize"
	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The tests of this file run the controller's sizing over this driver and a
// client double: the policy and the Pod it acts on in one process, with the
// kubelet's part played by the test. The same behavior against a real
// kubelet is the cluster tests of autosize_cluster_test.go.

// records is every record the controller emitted for a change of size.
type records struct {
	mu   sync.Mutex
	acts []controller.Act
}

func (r *records) Emit(_ context.Context, a controller.Act) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, a)
}
func (r *records) EmitSecret(context.Context, controller.SecretAct)           {}
func (r *records) EmitEnvironment(context.Context, controller.EnvironmentAct) {}

func (r *records) resized() []controller.ResizedData {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []controller.ResizedData
	for _, a := range r.acts {
		if a.Type == controller.MutationResized {
			out = append(out, a.Data.(controller.ResizedData))
		}
	}
	return out
}

// sized is one sandbox under a ceiling, created through the controller on a
// cluster that serves the resize subresource and the metrics API.
type sized struct {
	*harness
	t       *testing.T
	control *controller.Controller
	events  *records
	meter   *fakeMeter
	id      string

	mu sync.Mutex
	// kills is the kernel's count the workload's cgroup reports, and exit
	// the status a client's command ends with.
	kills int64
	exit  int
}

func sizedSandbox(t *testing.T, max v1.ResourceMax) *sized {
	t.Helper()
	h := sizing(t)
	s := &sized{harness: h, t: t, events: &records{}, meter: &fakeMeter{at: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}}
	h.meter = s.meter
	// The exec stream answers the driver's reading of the kill count with
	// the kernel's file, and ends a client's command with the status the
	// test set.
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if slices.Equal(c.argv, killCountCommand) {
			_, err := io.WriteString(stdout, "low 0\nhigh 0\nmax 0\noom 0\noom_kill "+itoa(s.kills)+"\noom_group_kill 0\n")
			return err
		}
		if s.exit != 0 {
			return exited(s.exit)
		}
		return nil
	}
	control, err := controller.Open(t.Context(), controller.Options{
		Driver: h.Driver, Environment: "default", DataDir: t.TempDir(), Events: s.events,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	s.control = control
	obj := v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "work"},
		Spec: v1.SandboxSpec{
			Environment: "default", Image: image, Workdir: "/workspace",
			Resources: v1.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi", Max: max},
			Workspace: v1.Workspace{Path: "/workspace", Source: v1.WorkspaceSourceEmpty},
			Network:   v1.Network{Egress: v1.Egress{Mode: v1.EgressOpen}},
		},
	}
	created, err := control.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.id = created.Status.ID
	if err := control.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.await("the sandbox to run", func(obj v1.Sandbox) bool { return obj.Status.Phase == driver.Running })
	return s
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// read is one pass of the controller's reading loop.
func (s *sized) read() {
	s.t.Helper()
	if _, err := s.control.Autosize(s.t.Context()); err != nil {
		s.t.Fatalf("Autosize: %v", err)
	}
}

// sample sets the working set the metrics API reports next, as a new sample.
func (s *sized) sample(bytes int64) {
	s.meter.bytes = bytes
	s.meter.at = s.meter.at.Add(15 * time.Second)
}

func (s *sized) stored() v1.Sandbox {
	s.t.Helper()
	obj, err := s.control.Get(s.t.Context(), s.id, "alice")
	if err != nil {
		s.t.Fatal(err)
	}
	return obj
}

// await polls the stored sandbox until it is what a case waits for. What a
// reading leaves running behind it, the wait for a verdict and a move, ends
// on its own schedule.
func (s *sized) await(what string, done func(v1.Sandbox) bool) v1.Sandbox {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj := s.stored()
		if done(obj) {
			return obj
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s; the sandbox is %s at %+v with %d moves", what, obj.Status.Phase, obj.Status.Resources, obj.Status.Moves)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// podsMade is how many Pods the driver created for the sandbox.
func (s *sized) podsMade() int {
	n := 0
	for _, action := range s.cs.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

// defer the kubelet's verdict on every resize from now on: the API server
// takes the patch into the Pod's spec, the container keeps the size it had,
// and the Pod carries the condition that says why.
func (s *sized) kubeletDefers(reason, message string) {
	s.cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		patch, ok := action.(k8stesting.PatchAction)
		if !ok || action.GetSubresource() != resizeSubresource || patch.GetPatchType() != types.StrategicMergePatchType {
			return false, nil, nil
		}
		var body struct {
			Spec struct {
				Containers []struct {
					Name      string                      `json:"name"`
					Resources corev1.ResourceRequirements `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		pod, err := s.cs.Tracker().Get(action.GetResource(), action.GetNamespace(), patch.GetName())
		if err != nil {
			return true, nil, err
		}
		current := pod.(*corev1.Pod).DeepCopy()
		before := current.Spec.Containers[0].Resources.Limits.DeepCopy()
		current.Spec.Containers[0].Resources = body.Spec.Containers[0].Resources
		current.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: Container, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			Resources: &corev1.ResourceRequirements{Limits: before},
		}}
		current.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: reason, Message: message},
		}
		if err := s.cs.Tracker().Update(action.GetResource(), current, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, current, nil
	})
}

// TestMemoryGrowsInPlace: a working set past the grow line at two readings
// raises the running Pod's memory through the resize subresource, limit and
// request together, with no Pod deleted or made, and the size and its record
// say it was raised in place.
func TestMemoryGrowsInPlace(t *testing.T) {
	s := sizedSandbox(t, v1.ResourceMax{CPU: "2", Memory: "1Gi"})
	pod := s.podOf(t, s.id)
	if got := pod.Spec.Containers[0].Resources; got.Limits.Cpu().String() != "2" || got.Requests.Cpu().MilliValue() != 50 || got.Limits.Memory().String() != "256Mi" {
		t.Fatalf("the sandbox was created with %v, want the cpu ceiling as its limit over a 50m request, and 256Mi", got)
	}
	s.sample(240 << 20)
	s.read()
	if n := len(s.resizePatches()); n != 0 {
		t.Fatalf("one reading at the line patched the Pod %d times", n)
	}
	s.sample(245 << 20)
	s.read()
	obj := s.await("the raise to be recorded", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "768Mi" })
	if obj.Status.Moves != 0 || obj.Status.Usage == nil || obj.Status.Usage.Memory != 245<<20 {
		t.Fatalf("status = %d moves, usage %+v", obj.Status.Moves, obj.Status.Usage)
	}
	after := s.podOf(t, s.id)
	if after.UID != pod.UID || s.podsMade() != 1 {
		t.Fatalf("the Pod was replaced: %d Pods made", s.podsMade())
	}
	if got := after.Spec.Containers[0].Resources; got.Limits.Memory().String() != "768Mi" || got.Requests.Memory().String() != "768Mi" || got.Limits.Cpu().String() != "2" {
		t.Fatalf("the Pod asks for %v after the raise", got)
	}
	want := []controller.ResizedData{{Resource: autosize.ResourceMemory, From: "256Mi", To: "768Mi", Reason: autosize.ReasonUsage}}
	if got := s.events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	// Two more readings at the new line reach the ceiling, and no reading
	// after that changes anything.
	for range 2 {
		s.sample(700 << 20)
		s.read()
	}
	s.await("the ceiling", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "1Gi" })
	patches := len(s.resizePatches())
	for range 3 {
		s.sample(1000 << 20)
		s.read()
	}
	if n := len(s.resizePatches()); n != patches || s.podsMade() != 1 {
		t.Fatalf("a sandbox at its ceiling was patched %d more times and has %d Pods", n-patches, s.podsMade())
	}
}

// TestOOMKillGrowsMemory: a kill that takes the container ends the Pod Failed
// with OOMKilled, and the sandbox is started again at once at the next size,
// with a session open or not, on the same claim. At its ceiling the same
// kill leaves it Failed, and nothing restarts it however often it is read.
func TestOOMKillGrowsMemory(t *testing.T) {
	s := sizedSandbox(t, v1.ResourceMax{Memory: "768Mi"})
	claim := s.claimOf(t, s.id).UID
	killed := func() {
		pod := s.podOf(t, s.id)
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.Now(),
			}},
		}}
		if _, err := s.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A terminal is open, which a move for a deferred raise would wait
	// out: after a kill there is nothing left of it to protect.
	terminal, err := s.control.Attach(t.Context(), s.id, driver.AttachRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = terminal.Close() }()
	killed()
	if state, err := s.Inspect(t.Context(), s.id); err != nil || state.Phase != phaseFailed || state.Reason != "OOMKilled" {
		t.Fatalf("the killed sandbox is %+v, %v", state, err)
	}
	s.read()
	obj := s.await("the move", func(obj v1.Sandbox) bool { return obj.Status.Moves == 1 })
	if obj.Status.Phase != driver.Running || obj.Status.Resources.Memory != "768Mi" {
		t.Fatalf("after the kill the sandbox is %s at %+v", obj.Status.Phase, obj.Status.Resources)
	}
	if got := s.podOf(t, s.id).Spec.Containers[0].Resources.Limits.Memory().String(); got != "768Mi" {
		t.Fatalf("the new Pod's memory limit is %s", got)
	}
	if s.claimOf(t, s.id).UID != claim {
		t.Fatal("the move replaced the workspace claim")
	}
	want := []controller.ResizedData{{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonOOM, Moved: true}}
	if got := s.events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	// Killed again at the ceiling: it stays Failed, as a sandbox with no
	// ceiling does, and no reading starts it.
	made := s.podsMade()
	killed()
	for range 4 {
		s.read()
	}
	time.Sleep(50 * time.Millisecond)
	if s.podsMade() != made || len(s.resizePatches()) != 0 {
		t.Fatalf("a sandbox killed at its ceiling got %d more Pods and %d resize patches", s.podsMade()-made, len(s.resizePatches()))
	}
	if state, err := s.Inspect(t.Context(), s.id); err != nil || state.Phase != phaseFailed {
		t.Fatalf("at its ceiling the killed sandbox is %+v, %v, want it left Failed", state, err)
	}
	if got := s.events.resized(); len(got) != 1 {
		t.Fatalf("records = %+v, want no resize at the ceiling", got)
	}
}

// TestDeferredResizeMoves: where the kubelet defers a raise, or says the node
// can never hold it, the sandbox keeps running at its size while a client's
// command is open in it, and is then deleted and made again at the larger
// size on the same claim, recorded with moved true and counted in
// status.moves.
func TestDeferredResizeMoves(t *testing.T) {
	for _, reason := range []string{corev1.PodReasonDeferred, corev1.PodReasonInfeasible} {
		t.Run(reason, func(t *testing.T) {
			s := sizedSandbox(t, v1.ResourceMax{Memory: "4Gi"})
			s.kubeletDefers(reason, "Node didn't have enough capacity: memory")
			claim, first := s.claimOf(t, s.id).UID, s.podOf(t, s.id).UID
			// A client's command is running.
			command, err := s.control.Exec(t.Context(), s.id, driver.ExecRequest{Command: []string{"make"}})
			if err != nil {
				t.Fatal(err)
			}
			s.sample(240 << 20)
			s.read()
			s.sample(245 << 20)
			s.read()
			if n := len(s.resizePatches()); n != 1 {
				t.Fatalf("the Pod was patched %d times, want the one raise", n)
			}
			state, err := s.Inspect(t.Context(), s.id)
			if err != nil {
				t.Fatal(err)
			}
			if state.Resources.Memory != "256Mi" || state.Resize == nil || state.Resize.Reason != reason || state.Resize.Resources.Memory != "768Mi" {
				t.Fatalf("the driver reports %+v with %+v outstanding", state.Resources, state.Resize)
			}
			// Read again and again with the command open: nothing moves.
			for range 3 {
				s.read()
			}
			time.Sleep(50 * time.Millisecond)
			if s.podsMade() != 1 || s.podOf(t, s.id).UID != first {
				t.Fatal("the sandbox was moved while a client's command was open in it")
			}
			if obj := s.stored(); obj.Status.Resources.Memory != "256Mi" || len(s.events.resized()) != 0 {
				t.Fatalf("a raise not applied is reported as %+v with %+v", obj.Status.Resources, s.events.resized())
			}
			// The command ends, and the next reading moves the sandbox.
			if _, err := command.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.read()
			obj := s.await("the move", func(obj v1.Sandbox) bool { return obj.Status.Moves == 1 })
			if obj.Status.Resources.Memory != "768Mi" || obj.Status.Phase != driver.Running {
				t.Fatalf("after the move the sandbox is %s at %+v", obj.Status.Phase, obj.Status.Resources)
			}
			moved := s.podOf(t, s.id)
			if s.podsMade() != 2 {
				t.Fatalf("%d Pods were made, want the first and the moved one", s.podsMade())
			}
			if got := moved.Spec.Containers[0].Resources; got.Limits.Memory().String() != "768Mi" || got.Requests.Memory().String() != "768Mi" {
				t.Fatalf("the moved Pod asks for %v", got)
			}
			if got := moved.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != objectName(s.id) || s.claimOf(t, s.id).UID != claim {
				t.Fatalf("the moved Pod mounts the claim %s, want the sandbox's own workspace kept", got)
			}
			want := []controller.ResizedData{{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonUsage, Moved: true}}
			if got := s.events.resized(); !slices.Equal(got, want) {
				t.Fatalf("records = %+v, want %+v", got, want)
			}
			// One move: the readings after it start nothing.
			for range 2 {
				s.read()
			}
			time.Sleep(50 * time.Millisecond)
			if s.podsMade() != 2 {
				t.Fatalf("the moved sandbox was restarted again: %d Pods", s.podsMade())
			}
		})
	}
}

// TestAChildKilledForMemoryGrowsTheSandbox: a child of the workload is killed
// for memory and the container lives on, so the Pod never terminates and no
// two readings are near the limit. The kernel's count of kills, read from the
// workload's own cgroup over the exec path, is the signal: the first count is
// the baseline and raises nothing, and the client's command that exits 137
// has the sandbox read before its exit is reported, so the raise is already
// asked of the cluster, in place, when the client reads the exit. One rise
// is one step, and a restarted sandbox begins a new baseline.
func TestAChildKilledForMemoryGrowsTheSandbox(t *testing.T) {
	s := sizedSandbox(t, v1.ResourceMax{Memory: "4Gi"})
	first := s.podOf(t, s.id).UID
	// The cgroup already counts kills when the control plane first reads
	// it: that count is where it starts from.
	s.mu.Lock()
	s.kills = 2
	s.mu.Unlock()
	s.sample(40 << 20)
	s.read()
	if n := len(s.resizePatches()); n != 0 {
		t.Fatalf("a count that was already there patched the Pod %d times", n)
	}
	// A client's install allocates past the limit: the kernel kills the
	// child, the shell reports the signal, and the container keeps running.
	s.mu.Lock()
	s.kills, s.exit = 3, 137
	s.mu.Unlock()
	command, err := s.control.Exec(t.Context(), s.id, driver.ExecRequest{Command: []string{"sh", "-c", "pip install big"}})
	if err != nil {
		t.Fatal(err)
	}
	code, err := command.Wait(t.Context())
	if err != nil || code != 137 {
		t.Fatalf("the killed command ended %d, %v", code, err)
	}
	// By the time the client reads the exit the raise has been asked for.
	patches := s.resizePatches()
	if len(patches) != 1 {
		t.Fatalf("when the killed command's exit was reported the Pod had been patched %d times, want once", len(patches))
	}
	if got := s.podOf(t, s.id).Spec.Containers[0].Resources.Limits.Memory().String(); got != "768Mi" {
		t.Fatalf("the Pod asks for %s of memory after the kill", got)
	}
	obj := s.await("the raise to be recorded", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "768Mi" })
	if obj.Status.Moves != 0 || s.podOf(t, s.id).UID != first || s.podsMade() != 1 {
		t.Fatalf("the container was restarted for a child's kill: %d moves, %d Pods", obj.Status.Moves, s.podsMade())
	}
	want := []controller.ResizedData{{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonOOM}}
	if got := s.events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	// The kill count was read by the driver's own command in the workload's
	// container, and that reading stamped no activity on the sandbox.
	var read bool
	for _, call := range s.exec.ran() {
		if slices.Equal(call.argv, killCountCommand) {
			read = true
			if call.container != Container || call.stdin || call.tty {
				t.Fatalf("the kill count was read by %+v", call)
			}
		}
	}
	if !read {
		t.Fatal("the kill count was never read")
	}
	// Several kills in one interval are one step, and the same count again
	// is none.
	s.mu.Lock()
	s.kills, s.exit = 9, 0
	s.mu.Unlock()
	s.read()
	s.await("the second step", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "1280Mi" })
	s.read()
	time.Sleep(50 * time.Millisecond)
	if n := len(s.resizePatches()); n != 2 {
		t.Fatalf("six kills in one reading and then none patched the Pod %d times in all, want 2", n)
	}
	// A stop and a start is a new workload instance, whose first count,
	// whatever it is, raises nothing. The start takes the size reached.
	if _, err := s.control.Act(t.Context(), s.id, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.control.Start(t.Context(), s.id, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.podOf(t, s.id).Spec.Containers[0].Resources.Limits.Memory().String(); got != "1280Mi" {
		t.Fatalf("the start gave the sandbox %s, want the size it had reached", got)
	}
	pod := s.podOf(t, s.id)
	pod.UID = "restarted"
	if _, err := s.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.kills = 20
	s.mu.Unlock()
	s.read()
	time.Sleep(50 * time.Millisecond)
	if n := len(s.resizePatches()); n != 2 {
		t.Fatalf("the first count of a restarted sandbox patched the Pod: %d patches", n)
	}
}

// TestAStartAndARecoveryTakeTheSizeHeld is the controller's half of the last
// size: the sandbox is started at what it was raised to, and the status that
// a recreation reads carries it.
func TestAStartAndARecoveryTakeTheSizeHeld(t *testing.T) {
	s := sizedSandbox(t, v1.ResourceMax{Memory: "4Gi"})
	s.sample(40 << 20)
	s.read()
	s.mu.Lock()
	s.kills = 1
	s.mu.Unlock()
	s.read()
	s.await("the raise", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "768Mi" })
	if _, err := s.control.Act(t.Context(), s.id, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.control.Start(t.Context(), s.id, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.podOf(t, s.id).Spec.Containers[0].Resources.Limits.Memory().String(); got != "768Mi" {
		t.Fatalf("a start after a stop gave the sandbox %s, want the 768Mi it held", got)
	}
	if got := s.stored().Status.Resources.Memory; got != "768Mi" {
		t.Fatalf("status.resources.memory after the start is %s", got)
	}
}
