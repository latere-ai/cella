// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"cmp"
	"context"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"latere.ai/x/cella/autosize"
	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The cluster tests of spec 078 run the controller's sizing over this driver
// against a real kubelet: the resize subresource applied by a node, a
// container the kernel kills, and the kernel's own count of kills. They are
// skipped where no cluster is configured, like the conformance run, and where
// the cluster serves no resize subresource or no metrics API, which is a
// cluster below Kubernetes 1.33 or one with no metrics-server.

// clusterReading is how often the controller reads a sandbox in these tests.
// The metrics API's own sample period bounds how soon two readings differ,
// so a shorter interval reads the same sample twice and counts it once.
const clusterReading = 2 * time.Second

// clusterWait bounds one wait for the cluster: an image pull, a metrics
// sample, a resize the kubelet applies, a Pod that starts again.
const clusterWait = 4 * time.Minute

// cluster is one sandbox under a ceiling on a real cluster, with the
// controller's reading loop running over it.
type cluster struct {
	t       *testing.T
	driver  *Driver
	control *controller.Controller
	events  *records
	id      string
}

// onCluster creates one sandbox of the given start and ceiling on the
// configured cluster and starts the reading loop. command is the sandbox's
// main command, nil for the driver's own.
func onCluster(t *testing.T, name, memory string, max v1.ResourceMax, command []string) *cluster {
	t.Helper()
	kubeconfig := cmp.Or(os.Getenv(envKubeconfig), os.Getenv("KUBECONFIG"))
	if kubeconfig == "" {
		t.Skipf("no cluster: set %s (or KUBECONFIG) to a kubeconfig of a cluster that serves pods/resize and the metrics API", envKubeconfig)
	}
	namespace := cmp.Or(os.Getenv(envNamespace), DefaultNamespace)
	d, err := New(Options{
		Namespace: namespace, Kubeconfig: kubeconfig,
		ReadyTimeout: 3 * time.Minute, GracePeriod: time.Second,
	})
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	ctx := t.Context()
	if _, err := d.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		Name: namespace,
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("namespace %s: %v", namespace, err)
	}
	if err := d.Preflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !d.Capabilities().Autosize.Memory {
		t.Skip("the cluster serves no pods/resize subresource or no metrics API, so the driver declares no memory autosize")
	}
	c := &cluster{t: t, driver: d, events: &records{}}
	c.control, err = controller.Open(ctx, controller.Options{
		Driver: d, Environment: "default", DataDir: t.TempDir(),
		Events: c.events, AutosizeInterval: clusterReading,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() { defer close(done); c.control.RunAutosize(loop) }()
	t.Cleanup(func() {
		stop()
		<-done
		if c.id != "" {
			if _, err := c.control.Act(context.WithoutCancel(ctx), c.id, "delete"); err != nil {
				t.Logf("deleting %s: %v", c.id, err)
			}
		}
		_ = c.control.Close()
	})
	obj := v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: name},
		Spec: v1.SandboxSpec{
			Environment: "default", Image: cmp.Or(os.Getenv(envImage), "alpine:3"), Command: command,
			Workdir:   "/workspace",
			Resources: v1.Resources{CPU: "100m", Memory: v1.Quantity(memory), Disk: "1Gi", Max: max},
			Workspace: v1.Workspace{Path: "/workspace", Source: v1.WorkspaceSourceEmpty},
			Network:   v1.Network{Egress: v1.Egress{Mode: v1.EgressOpen}},
		},
	}
	created, err := c.control.Create(ctx, obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	c.id = created.Status.ID
	if err := c.control.Schedule(ctx); err != nil {
		t.Fatalf("creating the sandbox: %v", err)
	}
	return c
}

func (c *cluster) stored() v1.Sandbox {
	c.t.Helper()
	obj, err := c.control.Get(c.t.Context(), c.id, "alice")
	if err != nil {
		c.t.Fatal(err)
	}
	return obj
}

// await polls the stored sandbox until it is what a case waits for.
func (c *cluster) await(what string, done func(v1.Sandbox) bool) v1.Sandbox {
	c.t.Helper()
	deadline := time.Now().Add(clusterWait)
	for {
		obj := c.stored()
		if done(obj) {
			return obj
		}
		if time.Now().After(deadline) {
			state, err := c.driver.Inspect(c.t.Context(), c.id)
			c.t.Fatalf("timed out waiting for %s; stored: %s at %+v, %d moves, usage %+v; driver: %+v, %v",
				what, obj.Status.Phase, obj.Status.Resources, obj.Status.Moves, obj.Status.Usage, state, err)
		}
		time.Sleep(time.Second)
	}
}

// run is one client command in the sandbox, through the controller as the
// API runs it, with its output and its exit code.
func (c *cluster) run(script string) (string, int, error) {
	c.t.Helper()
	e, err := c.control.Exec(c.t.Context(), c.id, driver.ExecRequest{Command: []string{"sh", "-c", script}, Timeout: 2 * time.Minute})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = e.Close() }()
	var out strings.Builder
	drained := make(chan struct{})
	go func() { defer close(drained); _, _ = io.Copy(io.Discard, e.Stderr()) }()
	_, _ = io.Copy(&out, e.Stdout())
	<-drained
	code, err := e.Wait(c.t.Context())
	return strings.TrimSpace(out.String()), code, err
}

func (c *cluster) pod() *corev1.Pod {
	c.t.Helper()
	pod, err := c.driver.cs.CoreV1().Pods(c.driver.opts.Namespace).Get(c.t.Context(), objectName(c.id), metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	return pod
}

// TestClusterMemoryGrowsInPlace: a process that holds most of the sandbox's
// memory is found by two samples of the metrics API, the kubelet raises the
// container's limit without restarting it, and the process is still there
// with its memory when the new size is recorded.
func TestClusterMemoryGrowsInPlace(t *testing.T) {
	c := onCluster(t, "grows-in-place", "128Mi", v1.ResourceMax{CPU: "1", Memory: "1Gi"}, nil)
	first := c.pod()
	if got := first.Spec.Containers[0].Resources; got.Limits.Cpu().String() != "1" || got.Requests.Cpu().MilliValue() != 10 {
		t.Fatalf("the sandbox runs with %v, want the cpu ceiling as its limit over a tenth of the 100m it was granted", got)
	}
	// A process that takes 112Mi and keeps it: dd fills one buffer and then
	// blocks writing it into a pipe nobody reads.
	out, code, err := c.run(`(dd if=/dev/zero bs=112M count=1 2>/dev/null | sleep 3600) >/dev/null 2>&1 &
sleep 2; pidof dd`)
	if err != nil || code != 0 || out == "" {
		t.Fatalf("starting the process that holds memory: %q, %d, %v", out, code, err)
	}
	holder := strings.Fields(out)[0]
	obj := c.await("memory raised", func(obj v1.Sandbox) bool { return obj.Status.Resources.Memory == "768Mi" })
	if obj.Status.Moves != 0 {
		t.Fatalf("the raise took %d moves, want it in place", obj.Status.Moves)
	}
	if obj.Status.Usage == nil || !autosize.MemoryPressed(obj.Status.Usage.Memory, 128<<20) {
		t.Fatalf("status.usage = %+v, want a working set at the grow line of the first size", obj.Status.Usage)
	}
	want := []controller.ResizedData{{Resource: autosize.ResourceMemory, From: "128Mi", To: "768Mi", Reason: autosize.ReasonUsage}}
	if got := c.events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	after := c.pod()
	if after.UID != first.UID {
		t.Fatal("the Pod was replaced")
	}
	for _, cs := range after.Status.ContainerStatuses {
		if cs.Name == Container && cs.RestartCount != 0 {
			t.Fatalf("the container was restarted %d times", cs.RestartCount)
		}
	}
	// The same process is still running, under the limit the kernel now
	// enforces for the container.
	out, code, err = c.run("kill -0 " + holder + " && cat /sys/fs/cgroup/memory.max")
	if err != nil || code != 0 {
		t.Fatalf("the process that held memory is gone after the raise: %q, %d, %v", out, code, err)
	}
	if out != "805306368" {
		t.Errorf("the container's memory limit in its cgroup is %s, want 768Mi", out)
	}
	t.Logf("raised in place to %s with usage %+v", obj.Status.Resources.Memory, obj.Status.Usage)
}

// TestClusterOOMKillGrowsMemory: a sandbox whose main process needs more than
// its memory is killed by the kernel, the Pod ends Failed with OOMKilled, and
// the controller starts it again at the next size, where it runs, with the
// file it wrote before the kill still in its workspace.
func TestClusterOOMKillGrowsMemory(t *testing.T) {
	// The main process writes a marker and then becomes one that needs
	// 200Mi, which the 128Mi it starts with cannot hold.
	command := []string{"sh", "-c", "echo kept >> /workspace/marker; exec dd if=/dev/zero of=/dev/null bs=200M"}
	c := onCluster(t, "oom-moves", "128Mi", v1.ResourceMax{Memory: "1Gi"}, command)
	obj := c.await("the move after the kill", func(obj v1.Sandbox) bool {
		return obj.Status.Moves == 1 && obj.Status.Phase == driver.Running
	})
	if obj.Status.Resources.Memory != "768Mi" {
		t.Fatalf("after the kill the sandbox holds %s, want 768Mi", obj.Status.Resources.Memory)
	}
	want := []controller.ResizedData{{Resource: autosize.ResourceMemory, From: "128Mi", To: "768Mi", Reason: autosize.ReasonOOM, Moved: true}}
	if got := c.events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	if got := c.pod().Spec.Containers[0].Resources.Limits.Memory().String(); got != "768Mi" {
		t.Fatalf("the new Pod's memory limit is %s", got)
	}
	// The workspace is the claim the first Pod wrote to: the marker has one
	// line from each start.
	out, code, err := c.run("cat /workspace/marker")
	if err != nil || code != 0 || out != "kept\nkept" {
		t.Fatalf("the workspace after the move holds %q (%d, %v), want the line of each start", out, code, err)
	}
	// It runs at the larger size and is not moved again.
	time.Sleep(5 * clusterReading)
	if after := c.stored(); after.Status.Moves != 1 {
		t.Fatalf("a sandbox that runs at its new size was moved again: %d moves", after.Status.Moves)
	}
	if state, err := c.driver.Inspect(t.Context(), c.id); err != nil || state.Phase != driver.Running {
		t.Fatalf("the moved sandbox is %+v, %v", state, err)
	}
}

// TestClusterAChildKilledForMemoryGrowsTheSandbox: a client's command
// allocates past the limit in a sandbox whose main process is a shell. On a
// node that kills one process at a time the command dies with 137 and the
// shell lives on; the kernel's count rises, the limit is raised in place
// before the exit is reported, and the same command then succeeds. On a node
// that kills the whole container the kill takes the sandbox, which is the
// case TestClusterOOMKillGrowsMemory proves, and the sandbox is started again
// at the larger size where the command succeeds.
func TestClusterAChildKilledForMemoryGrowsTheSandbox(t *testing.T) {
	c := onCluster(t, "child-killed", "128Mi", v1.ResourceMax{Memory: "1Gi"}, nil)
	first := c.pod()
	group, _, err := c.run("cat /sys/fs/cgroup/memory.oom.group 2>/dev/null || echo 0")
	if err != nil {
		t.Fatal(err)
	}
	// The baseline of the kill count is the reading the start asked for and
	// the loop's own; a few intervals is both.
	time.Sleep(3 * clusterReading)
	const child = "dd if=/dev/zero of=/dev/null bs=200M count=1"
	started := time.Now()
	out, code, err := c.run(child)
	reported := time.Since(started)
	t.Logf("memory.oom.group=%s; the command that allocates 200Mi under 128Mi ended %d, %v after %s: %q", group, code, err, reported.Round(time.Millisecond), out)
	if err == nil && code == 0 {
		t.Fatal("a command that allocates past the limit succeeded; the limit is not enforced on this node")
	}
	obj := c.await("memory raised after the kill", func(obj v1.Sandbox) bool {
		return obj.Status.Resources.Memory == "768Mi" && obj.Status.Phase == driver.Running
	})
	t.Logf("the size was recorded %s after the command started", time.Since(started).Round(time.Millisecond))
	records := c.events.resized()
	if len(records) != 1 || records[0].Reason != autosize.ReasonOOM || records[0].From != "128Mi" || records[0].To != "768Mi" {
		t.Fatalf("records = %+v, want one raise for oom", records)
	}
	after := c.pod()
	if group == "1" {
		// The node kills the container's whole cgroup: the shell did not
		// survive, the Pod ended OOMKilled, and the sandbox was moved.
		if obj.Status.Moves != 1 || !records[0].Moved || after.UID == first.UID {
			t.Fatalf("on a node that kills the whole container: %d moves, record %+v", obj.Status.Moves, records[0])
		}
		t.Log("this node kills the whole container for a child's memory (memory.oom.group=1): the sandbox was restarted at the larger size")
	} else {
		if code != 137 {
			t.Errorf("the killed command ended %d, want 137", code)
		}
		if obj.Status.Moves != 0 || records[0].Moved || after.UID != first.UID {
			t.Fatalf("the container was restarted for a child's kill: %d moves, record %+v", obj.Status.Moves, records[0])
		}
		for _, cs := range after.Status.ContainerStatuses {
			if cs.Name == Container && cs.RestartCount != 0 {
				t.Fatalf("the container was restarted %d times", cs.RestartCount)
			}
		}
	}
	// The same command succeeds under the raised limit.
	deadline := time.Now().Add(clusterWait)
	for {
		out, code, err = c.run(child)
		if err == nil && code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the same command still fails after the raise: %q, %d, %v", out, code, err)
		}
		time.Sleep(time.Second)
	}
	if got := c.stored().Status.Resources.Memory; got != "768Mi" {
		t.Fatalf("a command that fits raised memory again, to %s", got)
	}
}

// TestClusterAKilledSandboxNeverReadsFailed: a client's command allocates
// past the limit of a sandbox with room under its ceiling while other
// clients read the sandbox continuously, the stored object and the driver's
// state over it as the API's read does. Whatever the node does with the
// kill, no read answers Failed, Stopped or not found. Where the node kills
// the whole container, the readers see Running, then Starting with reason
// MemoryRaised, then Running, the workspace keeps its file, and what a
// restart always costs is gone: the process the client left in the
// background and the file it wrote outside the workspace. Where the node
// kills one process, the sandbox reads Running throughout and loses nothing.
func TestClusterAKilledSandboxNeverReadsFailed(t *testing.T) {
	c := onCluster(t, "never-failed", "128Mi", v1.ResourceMax{Memory: "1Gi"}, nil)
	if out, code, err := c.run("echo kept > /workspace/marker && echo scratch > /tmp/scratch && (tail -f /dev/null >/dev/null 2>&1 &) && sleep 1 && pidof tail"); err != nil || code != 0 || out == "" {
		t.Fatalf("preparing the sandbox: %q, %d, %v", out, code, err)
	}
	group, _, err := c.run("cat /sys/fs/cgroup/memory.oom.group 2>/dev/null || echo 0")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * clusterReading)

	seen := &watched{seen: map[string]int{}}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			obj, err := c.control.Get(context.Background(), c.id, "alice")
			if err != nil {
				seen.saw("not found")
				continue
			}
			fresh, err := c.control.Refresh(context.Background(), obj)
			if err != nil {
				seen.saw("unread: " + err.Error())
				continue
			}
			what := fresh.Status.Phase
			if fresh.Status.Reason == v1.ReasonMemoryRaised {
				what += " " + fresh.Status.Reason
			}
			seen.saw(what)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	seen.more(t, 20)
	started := time.Now()
	_, code, runErr := c.run("dd if=/dev/zero of=/dev/null bs=200M count=1")
	if runErr == nil && code == 0 {
		t.Fatal("a command that allocates past the limit succeeded; the limit is not enforced on this node")
	}
	obj := c.await("memory raised after the kill", func(obj v1.Sandbox) bool {
		return obj.Status.Resources.Memory == "768Mi" && obj.Status.Phase == driver.Running
	})
	back := time.Since(started)
	seen.more(t, 20)
	close(stop)
	<-done
	seen.mu.Lock()
	saw := map[string]int{}
	maps.Copy(saw, seen.seen)
	seen.mu.Unlock()
	t.Logf("memory.oom.group=%s; the command ended %d, %v; running again at 768Mi %s after it started; readers saw %v",
		group, code, runErr, back.Round(time.Millisecond), saw)
	starting := "Starting " + v1.ReasonMemoryRaised
	for what, n := range saw {
		if what != driver.Running && what != starting {
			t.Errorf("readers saw %q %d times; a sandbox being given more memory reads Running or Starting only", what, n)
		}
	}
	// The workspace is kept either way.
	if out, code, err := c.run("cat /workspace/marker"); err != nil || code != 0 || out != "kept" {
		t.Fatalf("the workspace after the kill holds %q (%d, %v), want the file written before it", out, code, err)
	}
	scratch, _, err := c.run("cat /tmp/scratch 2>/dev/null || echo gone")
	if err != nil {
		t.Fatal(err)
	}
	background, _, err := c.run("pidof tail || echo gone")
	if err != nil {
		t.Fatal(err)
	}
	if group == "1" {
		if obj.Status.Moves != 1 || saw[starting] == 0 {
			t.Fatalf("on a node that kills the whole container: %d moves, readers saw %v, want one restart read as Starting", obj.Status.Moves, saw)
		}
		if scratch != "gone" || background != "gone" {
			t.Errorf("after the restart the file outside the workspace is %q and the background process %q, want both gone", scratch, background)
		}
		return
	}
	if obj.Status.Moves != 0 || saw[starting] != 0 {
		t.Fatalf("on a node that kills one process: %d moves, readers saw %v, want the sandbox Running throughout", obj.Status.Moves, saw)
	}
	if scratch != "scratch" || background == "gone" {
		t.Errorf("after a raise in place the file outside the workspace is %q and the background process %q, want both still there", scratch, background)
	}
}
