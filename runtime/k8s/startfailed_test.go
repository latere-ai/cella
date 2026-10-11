// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestAFailedSandboxIsStartedOnItsClaim is a client's start of a failed
// sandbox on Kubernetes, through the controller and over the driver. A Pod
// that ended, its main process exiting non-zero, killed for memory with no
// ceiling to grow under, or evicted by its node, leaves the claim that holds
// the workspace, and the sandbox reads Failed with the cluster's reason. The
// start removes the ended Pod and makes one from the spec the claim records,
// on the same claim: the same image, command, size and volume as a start
// after a stop makes. It is one Pod per start: a workload that ends again
// leaves the sandbox Failed, and no Pod is made until the next start.
func TestAFailedSandboxIsStartedOnItsClaim(t *testing.T) {
	exited3, killed137 := 3, 137
	for _, tc := range []struct {
		name, reason string
		code         *int
		end          func(pod *corev1.Pod)
	}{
		{
			name: "a main process that exited non-zero", reason: "Error", code: &exited3,
			end: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 3, Reason: "Error", FinishedAt: metav1.Now(),
					}},
				}}
			},
		},
		{
			name: "a kill for memory with no ceiling", reason: "OOMKilled", code: &killed137,
			end: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.Now(),
					}},
				}}
			},
		},
		{
			name: "a Pod its node evicted", reason: "Evicted",
			end: func(pod *corev1.Pod) { pod.Status.Reason, pod.Status.ContainerStatuses = "Evicted", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sizedSandbox(t, v1.ResourceMax{})
			claim := s.claimOf(t, s.id).UID
			first := s.podOf(t, s.id)
			fail := func() {
				t.Helper()
				pod := s.podOf(t, s.id)
				pod.Status.Phase = corev1.PodFailed
				tc.end(pod)
				if _, err := s.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			read := func() v1.Sandbox {
				t.Helper()
				obj, err := s.control.Refresh(t.Context(), s.stored())
				if err != nil {
					t.Fatal(err)
				}
				return obj
			}
			fail()
			failed := read()
			if failed.Status.Phase != phaseFailed || failed.Status.Reason != tc.reason || !reflect.DeepEqual(failed.Status.ExitCode, tc.code) {
				t.Fatalf("the sandbox reads %s %q with exit %v, want Failed %q", failed.Status.Phase, failed.Status.Reason, failed.Status.ExitCode, tc.reason)
			}
			// Nothing brings it back on its own.
			made := s.podsMade()
			s.read()
			s.read()
			if n := s.podsMade() - made; n != 0 {
				t.Fatalf("%d Pods were made for a failed sandbox nobody started", n)
			}
			started, err := s.control.Start(t.Context(), s.id, 0)
			if err != nil {
				t.Fatalf("a start of the failed sandbox: %v", err)
			}
			if started.Status.Phase != driver.Running || started.Status.Reason != "" || started.Status.ExitCode != nil {
				t.Fatalf("the started sandbox answers %s %q with exit %v, want Running with nothing of the failure", started.Status.Phase, started.Status.Reason, started.Status.ExitCode)
			}
			if n := s.podsMade() - made; n != 1 {
				t.Fatalf("the start made %d Pods, want 1", n)
			}
			if s.claimOf(t, s.id).UID != claim {
				t.Fatal("the start replaced the workspace claim")
			}
			second := s.podOf(t, s.id)
			if second.Status.Phase == corev1.PodFailed {
				t.Fatal("the Pod after the start is the one that ended")
			}
			if !reflect.DeepEqual(second.Spec.Containers[0].Resources, first.Spec.Containers[0].Resources) ||
				second.Spec.Containers[0].Image != first.Spec.Containers[0].Image ||
				!reflect.DeepEqual(second.Spec.Containers[0].Command, first.Spec.Containers[0].Command) ||
				!reflect.DeepEqual(second.Spec.Volumes, first.Spec.Volumes) {
				t.Fatalf("the Pod the start made differs from the one the create made:\n%+v\n%+v", second.Spec, first.Spec)
			}
			// One start is one Pod.
			fail()
			s.read()
			if again := read(); again.Status.Phase != phaseFailed || s.podsMade()-made != 1 {
				t.Fatalf("after the workload ended again the sandbox reads %s with %d Pods made since the failure, want Failed and 1",
					again.Status.Phase, s.podsMade()-made)
			}
			if _, err := s.control.Start(t.Context(), s.id, 0); err != nil {
				t.Fatalf("a second start: %v", err)
			}
			if got := read(); got.Status.Phase != driver.Running || s.podsMade()-made != 2 {
				t.Fatalf("after a second start the sandbox reads %s with %d Pods made, want Running and 2", got.Status.Phase, s.podsMade()-made)
			}
		})
	}
}

// TestAStartOfAFailedCreateOnTheClusterIsRefused: a create the cluster could
// not finish is rolled back, claim and all, so the failed sandbox has no
// workspace, and a start of it is the phase conflict.
func TestAStartOfAFailedCreateOnTheClusterIsRefused(t *testing.T) {
	h := newHarness(t)
	h.status = pending("ImagePullBackOff", "Back-off pulling image")
	h.opts.ReadyTimeout = 300 * time.Millisecond
	control, err := controller.Open(t.Context(), controller.Options{Driver: h.Driver, Environment: "default", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	created, err := control.Create(t.Context(), v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "work"},
		Spec: v1.SandboxSpec{
			Environment: "default", Image: image, Workdir: "/workspace",
			Resources: v1.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi"},
			Workspace: v1.Workspace{Path: "/workspace", Source: v1.WorkspaceSourceEmpty},
			Network:   v1.Network{Egress: v1.Egress{Mode: v1.EgressOpen}},
		},
	}, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The create is the loop's, and fails there.
	_ = control.Schedule(t.Context())
	obj, err := control.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != phaseFailed || obj.Status.Reason != controller.ReasonCreateFailed {
		t.Fatalf("the sandbox is %s %q, want Failed CreateFailed", obj.Status.Phase, obj.Status.Reason)
	}
	if _, err := h.Inspect(t.Context(), created.Status.ID); err == nil {
		t.Fatal("the failed create left the sandbox on the cluster")
	}
	if _, err := control.Start(t.Context(), created.Status.ID, 0); !errors.Is(err, controller.ErrPhase) {
		t.Fatalf("a start of a sandbox whose create failed: %v, want the phase conflict", err)
	}
}
