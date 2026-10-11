// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"latere.ai/x/cella/autosize"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// fill leaves the cluster's one node with about left bytes of memory nobody
// has requested, by a Pod that requests the rest and uses none of it, so a
// sandbox that asks for more than that finds no room. It is removed with the
// test.
func (c *cluster) fill(left int64) {
	c.t.Helper()
	ctx := c.t.Context()
	nodes, err := c.driver.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) != 1 {
		c.t.Skipf("filling a node needs a cluster of one node: %d nodes, %v", len(nodes.Items), err)
	}
	free := nodes.Items[0].Status.Allocatable.Memory().Value()
	pods, err := c.driver.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, container := range pod.Spec.Containers {
			free -= container.Resources.Requests.Memory().Value()
		}
	}
	if free <= left {
		c.t.Skipf("the node has %d bytes of memory unrequested, which is no more than the %d this case leaves", free, left)
	}
	namespace := c.driver.opts.Namespace
	filler := &corev1.Pod{
		GenerateName: "filler-", Namespace: namespace,
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{{
				Name: "filler", Image: os.Getenv(envImage), Command: []string{"sleep", "3600"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceMemory: *resource.NewQuantity(free-left, resource.BinarySI),
				}},
			}},
		},
	}
	if filler.Spec.Containers[0].Image == "" {
		filler.Spec.Containers[0].Image = "alpine:3"
	}
	made, err := c.driver.cs.CoreV1().Pods(namespace).Create(ctx, filler, metav1.CreateOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() {
		_ = c.driver.cs.CoreV1().Pods(namespace).Delete(context.WithoutCancel(ctx), made.Name, metav1.DeleteOptions{})
	})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, clusterWait, true, func(ctx context.Context) (bool, error) {
		pod, err := c.driver.cs.CoreV1().Pods(namespace).Get(ctx, made.Name, metav1.GetOptions{})
		return err == nil && pod.Status.Phase == corev1.PodRunning, nil
	}); err != nil {
		c.t.Fatalf("the Pod that fills the node never ran: %v", err)
	}
}

// TestClusterAKillWithNoRoomForMoreIsStartedAgainAtItsSize is a cluster with
// no room for the next size, which on a small cluster is the usual case. The
// node is filled until a sandbox of 128Mi fits and one of 768Mi does not. A
// command that allocates past the limit takes the container: the move to
// 768Mi is never scheduled and is undone, the sandbox runs at 128Mi again
// and its MemoryUnplaced condition names the size that found no room. The
// same command once more, inside the backoff, is answered at once by a Pod
// at 128Mi on the same claim, and readers of the sandbox throughout see
// Running and Starting only, never Failed. Killed again at every start it is
// started SameSizeRestarts times and then reads Failed with OOMKilled, and a
// client's start brings it back with the file in its workspace.
func TestClusterAKillWithNoRoomForMoreIsStartedAgainAtItsSize(t *testing.T) {
	c := onCluster(t, "no-room", "128Mi", v1.ResourceMax{Memory: "1Gi"}, nil)
	c.awaitRead(driver.Running)
	group, _, err := c.run("cat /sys/fs/cgroup/memory.oom.group 2>/dev/null || echo 0")
	if err != nil {
		t.Fatal(err)
	}
	if group != "1" {
		t.Skipf("this node kills one process for memory (memory.oom.group=%s), so a command's kill leaves the sandbox running and nothing is restarted", group)
	}
	if out, code, err := c.run("echo kept > /workspace/marker"); err != nil || code != 0 {
		t.Fatalf("preparing the sandbox: %q, %d, %v", out, code, err)
	}
	c.fill(300 << 20)
	// A Pod nobody schedules is waited for this long before the move is
	// undone, which is the wait this case pays once.
	c.driver.opts.ReadyTimeout = 20 * time.Second
	claim := c.claim()
	kill := func() {
		t.Helper()
		if _, code, err := c.run("dd if=/dev/zero of=/dev/null bs=200M count=1"); err == nil && code == 0 {
			t.Fatal("a command that allocates past the limit succeeded; the limit is not enforced on this node")
		}
	}
	unplaced := func(obj v1.Sandbox) v1.Condition {
		for _, cond := range obj.Status.Conditions {
			if cond.Type == v1.ConditionMemoryUnplaced {
				return cond
			}
		}
		return v1.Condition{}
	}
	limit := func() string {
		t.Helper()
		return c.pod().Spec.Containers[0].Resources.Limits.Memory().String()
	}
	// replaced waits until the sandbox runs again in another Pod than the
	// one named, which is gone for a moment in between.
	replaced := func(ended string) {
		t.Helper()
		deadline := time.Now().Add(clusterWait)
		for {
			pod, err := c.driver.cs.CoreV1().Pods(c.driver.opts.Namespace).Get(t.Context(), objectName(c.id), metav1.GetOptions{})
			if err == nil && string(pod.UID) != ended && c.reads().Status.Phase == driver.Running {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the sandbox was not started again at the size it holds: it reads %s", c.reads().Status.Phase)
			}
			time.Sleep(50 * time.Millisecond)
		}
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
			if strings.HasPrefix(fresh.Status.Reason, "Memory") {
				what += " " + fresh.Status.Reason
			}
			seen.saw(what)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	stopped := false
	halt := func() {
		if !stopped {
			stopped = true
			close(stop)
			<-done
		}
	}
	defer halt()

	// The first kill: a move to 768Mi that no node takes, undone.
	seen.more(t, 20)
	started := time.Now()
	kill()
	obj := c.await("the move that finds no room to be undone", func(obj v1.Sandbox) bool {
		return obj.Status.Phase == driver.Running && unplaced(obj).Status == v1.ConditionTrue
	})
	undone := time.Since(started)
	if cond := unplaced(obj); cond.Reason != v1.ReasonNoRoom || !strings.Contains(cond.Message, "No node had room for 768Mi of memory") {
		t.Fatalf("after the undone move the condition is %+v, want MemoryUnplaced True NoRoom naming 768Mi", cond)
	}
	if obj.Status.Resources.Memory != "128Mi" || obj.Status.Moves != 0 || limit() != "128Mi" {
		t.Fatalf("after the undone move the sandbox holds %s with %d moves and a Pod limited to %s, want 128Mi and none", obj.Status.Resources.Memory, obj.Status.Moves, limit())
	}
	c.awaitRead(driver.Running)

	// The second kill, inside the backoff: started again at 128Mi at once.
	ended := string(c.pod().UID)
	started = time.Now()
	kill()
	replaced(ended)
	back := time.Since(started)
	seen.more(t, 20)
	halt()
	seen.mu.Lock()
	saw := map[string]int{}
	maps.Copy(saw, seen.seen)
	seen.mu.Unlock()
	t.Logf("the move that found no room was undone %s after the first kill; after the second the sandbox ran again at 128Mi in %s; readers saw %v",
		undone.Round(time.Millisecond), back.Round(time.Millisecond), saw)
	raised, held := "Starting "+v1.ReasonMemoryRaised, "Starting "+v1.ReasonMemoryNotRaised
	for what, n := range saw {
		if what != driver.Running && what != raised && what != held {
			t.Errorf("readers saw %q %d times; a sandbox the control plane is starting again reads Running or Starting only", what, n)
		}
	}
	if saw[held] == 0 {
		t.Errorf("readers saw %v, want Starting with reason %s for the start at the size held", saw, v1.ReasonMemoryNotRaised)
	}
	obj = c.stored()
	if obj.Status.Moves != 0 || limit() != "128Mi" || c.claim() != claim {
		t.Fatalf("after the start at the size held: %d moves, a Pod limited to %s, the same claim %v", obj.Status.Moves, limit(), c.claim() == claim)
	}
	if out, code, err := c.run("cat /workspace/marker"); err != nil || code != 0 || out != "kept" {
		t.Fatalf("the workspace holds %q (%d, %v), want the file written before the kills", out, code, err)
	}

	// Killed at every start: the bound, then Failed until a client acts.
	for i := 2; i <= autosize.SameSizeRestarts; i++ {
		time.Sleep(autosize.SameSizeRestartBackoff)
		ended = string(c.pod().UID)
		kill()
		replaced(ended)
	}
	time.Sleep(autosize.SameSizeRestartBackoff)
	kill()
	failed := c.awaitRead(phaseFailed)
	obj = c.await("the control plane to say it gave up", func(obj v1.Sandbox) bool {
		return unplaced(obj).Reason == v1.ReasonRestartsExhausted
	})
	ended = string(c.pod().UID)
	time.Sleep(3 * clusterReading)
	if failed.Status.Reason != "OOMKilled" || string(c.pod().UID) != ended || c.reads().Status.Phase != phaseFailed {
		t.Fatalf("after %d starts at the same size the sandbox reads %s %q and its Pod was replaced: %v, want it left Failed OOMKilled",
			autosize.SameSizeRestarts, c.reads().Status.Phase, failed.Status.Reason, string(c.pod().UID) != ended)
	}
	if _, err := c.control.Start(t.Context(), c.id, 0); err != nil {
		t.Fatalf("a client's start of the sandbox: %v", err)
	}
	c.awaitRead(driver.Running)
	if out, code, err := c.run("cat /workspace/marker"); err != nil || code != 0 || out != "kept" || c.claim() != claim {
		t.Fatalf("after the client's start the workspace holds %q (%d, %v) on the same claim %v", out, code, err, c.claim() == claim)
	}
}
