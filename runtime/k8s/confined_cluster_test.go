// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"cmp"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// TestClusterPodIsConfined is spec 013's control plane model, row 1, on a
// real kubelet: the baseline the render tests prove on the object is read
// back from inside the running container with id, /proc/1/status and a
// mount attempt, so the fields are not only sent but honored.
func TestClusterPodIsConfined(t *testing.T) {
	kubeconfig := cmp.Or(os.Getenv(envKubeconfig), os.Getenv("KUBECONFIG"))
	if kubeconfig == "" {
		t.Skipf("no cluster: set %s (or KUBECONFIG) to a kubeconfig and %s to an alpine image", envKubeconfig, envImage)
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
	id := "sbx_confined_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := d.Create(ctx, driver.CreateSpec{
		ID: id, Name: "confined", Owner: "alice", Image: cmp.Or(os.Getenv(envImage), "alpine:3"),
		Workspace: driver.Workspace{Path: "/workspace"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Delete(context.WithoutCancel(ctx), id); err != nil {
			t.Logf("deleting %s: %v", id, err)
		}
	})

	// Not root, and the shell the Pod renders as the kept-alive command is
	// its own PID 1: shareProcessNamespace is false, so a container with
	// no other container to share a namespace with starts its own.
	out := inHome(t, d, id, `id -u; head -n2 /proc/1/status`)
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	if len(lines) < 1 || lines[0] == "0" {
		t.Fatalf("id -u answered %q, want a non-root uid", out)
	}
	if !strings.Contains(out, "sh") {
		t.Fatalf("/proc/1/status is %q, want the kept-alive shell's own PID 1", out)
	}

	// No new privilege, every capability dropped, and the default seccomp
	// profile, read back from the container's own kernel view rather than
	// from the object the driver sent.
	caps := inHome(t, d, id, `grep -E '^(CapEff|NoNewPrivs|Seccomp):' /proc/1/status`)
	if !strings.Contains(caps, "CapEff:\t0000000000000000") {
		t.Fatalf("CapEff is %q, want every capability dropped", caps)
	}
	if !strings.Contains(caps, "NoNewPrivs:\t1") {
		t.Fatalf("NoNewPrivs is %q, want privilege escalation refused", caps)
	}
	if !strings.Contains(caps, "Seccomp:\t2") {
		t.Fatalf("Seccomp is %q, want the filtered mode RuntimeDefault sets", caps)
	}

	// The root file system is read only: a mount attempt inside it is
	// refused by the kernel, with no capability left to make it root's
	// decision instead.
	mount := inHome(t, d, id, `mount -t tmpfs tmpfs /mnt >/dev/null 2>&1; echo "rc=$?"`)
	if strings.Contains(mount, "rc=0") {
		t.Fatalf("a mount inside the Pod answered %q, want it refused", mount)
	}
	write := inHome(t, d, id, `touch /confined-write >/dev/null 2>&1; echo "rc=$?"`)
	if strings.Contains(write, "rc=0") {
		t.Fatalf("writing the root file system answered %q, want it refused as read only", write)
	}
}
