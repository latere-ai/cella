// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// reads is the sandbox as a client reads it: the stored object with the
// cluster's state read over it.
func (c *cluster) reads() v1.Sandbox {
	c.t.Helper()
	obj, err := c.control.Refresh(c.t.Context(), c.stored())
	if err != nil {
		c.t.Fatal(err)
	}
	return obj
}

// claim is the UID of the claim that holds the sandbox's workspace.
func (c *cluster) claim() string {
	c.t.Helper()
	pvc, err := c.driver.cs.CoreV1().PersistentVolumeClaims(c.driver.opts.Namespace).Get(c.t.Context(), objectName(c.id), metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	return string(pvc.UID)
}

// awaitRead reads the sandbox until it is in a phase.
func (c *cluster) awaitRead(phase string) v1.Sandbox {
	c.t.Helper()
	deadline := time.Now().Add(clusterWait)
	for {
		obj := c.reads()
		if obj.Status.Phase == phase {
			return obj
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the sandbox stayed %s %q, want %s", obj.Status.Phase, obj.Status.Reason, phase)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestClusterAFailedSandboxIsStartedWithItsWorkspace is what a client does
// with a sandbox whose container ended, on a real cluster and through the
// controller as the API calls it. A file is written to the workspace and one
// outside it; the container's main process ends, by its own non-zero exit
// in one case and by the kernel's kill for memory in the other, with no
// memory ceiling to grow under; the sandbox reads Failed with the cluster's
// reason and nothing brings it back; a start makes a new Pod on the same
// claim; and the file in the workspace is read back, while the one outside
// it is gone, as after any start.
func TestClusterAFailedSandboxIsStartedWithItsWorkspace(t *testing.T) {
	recovered := func(t *testing.T, c *cluster, reason string) {
		t.Helper()
		failed := c.awaitRead(phaseFailed)
		if failed.Status.Reason != reason {
			t.Fatalf("the failed sandbox reads reason %q, want %q", failed.Status.Reason, reason)
		}
		claim, ended := c.claim(), c.pod().UID
		// Nothing starts it on its own: the readings pass and the Pod that
		// ended is still the sandbox's.
		time.Sleep(3 * clusterReading)
		if got := c.reads(); got.Status.Phase != phaseFailed || c.pod().UID != ended {
			t.Fatalf("left alone, the failed sandbox reads %s and its Pod was replaced: %v", got.Status.Phase, c.pod().UID != ended)
		}
		before := time.Now()
		started, err := c.control.Start(t.Context(), c.id, 0)
		if err != nil {
			t.Fatalf("a start of the failed sandbox: %v", err)
		}
		if started.Status.Phase != driver.Running || started.Status.ExitCode != nil {
			t.Fatalf("the started sandbox answers %s with exit %v, want Running", started.Status.Phase, started.Status.ExitCode)
		}
		t.Logf("failed with %s; running again %s after the start was asked for", reason, time.Since(before).Round(time.Millisecond))
		if c.claim() != claim {
			t.Fatal("the start replaced the workspace claim")
		}
		if c.pod().UID == ended {
			t.Fatal("the Pod after the start is the one that ended")
		}
		if out, code, err := c.run("cat /workspace/marker"); err != nil || code != 0 || out != "kept" {
			t.Fatalf("the workspace after the start holds %q (%d, %v), want the file written before the failure", out, code, err)
		}
		if out, _, err := c.run("cat /tmp/scratch 2>/dev/null || echo gone"); err != nil || out != "gone" {
			t.Errorf("the file outside the workspace reads %q, %v after the start, want it gone", out, err)
		}
	}
	t.Run("a main process that exits non-zero", func(t *testing.T) {
		c := onCluster(t, "restart-exit", "128Mi", v1.ResourceMax{},
			[]string{"sh", "-c", "while [ ! -f /workspace/fail ]; do sleep 0.2; done; rm /workspace/fail; exit 3"})
		c.awaitRead(driver.Running)
		if out, code, err := c.run("echo kept > /workspace/marker && echo scratch > /tmp/scratch && : > /workspace/fail"); err != nil || code != 0 {
			t.Fatalf("preparing the sandbox: %q, %d, %v", out, code, err)
		}
		recovered(t, c, "Error")
	})
	t.Run("a container killed for memory with no ceiling", func(t *testing.T) {
		c := onCluster(t, "restart-oom", "128Mi", v1.ResourceMax{}, nil)
		c.awaitRead(driver.Running)
		if out, code, err := c.run("echo kept > /workspace/marker && echo scratch > /tmp/scratch"); err != nil || code != 0 {
			t.Fatalf("preparing the sandbox: %q, %d, %v", out, code, err)
		}
		group, _, err := c.run("cat /sys/fs/cgroup/memory.oom.group 2>/dev/null || echo 0")
		if err != nil {
			t.Fatal(err)
		}
		if group != "1" {
			t.Skipf("this node kills one process for memory (memory.oom.group=%s), so a command's kill leaves the sandbox running and nothing fails", group)
		}
		if _, code, err := c.run("dd if=/dev/zero of=/dev/null bs=200M count=1"); err == nil && code == 0 {
			t.Fatal("a command that allocates past the limit succeeded; the limit is not enforced on this node")
		}
		recovered(t, c, "OOMKilled")
	})
}
