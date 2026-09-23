// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// The cluster the conformance run drives, and what it runs there. Every value
// is the caller's: this package names no cluster, no namespace of an
// installation and no image of one.
const (
	envKubeconfig   = "CELLA_TEST_KUBECONFIG"
	envNamespace    = "CELLA_TEST_NAMESPACE"
	envImage        = "CELLA_TEST_IMAGE"
	envDisplayImage = "CELLA_TEST_DISPLAY_IMAGE"
)

// TestClusterConformance runs the whole contract of spec 004 against a real
// cluster. It is skipped where none is configured, so the unit suite stays
// hermetic; a kind cluster is enough, and CELLA_TEST_KUBECONFIG or KUBECONFIG
// names it.
func TestClusterConformance(t *testing.T) {
	kubeconfig := cmp.Or(os.Getenv(envKubeconfig), os.Getenv("KUBECONFIG"))
	if kubeconfig == "" {
		t.Skipf("no cluster: set %s (or KUBECONFIG) to a kubeconfig, %s to the namespace, and %s to an image with a shell and tar",
			envKubeconfig, envNamespace, envImage)
	}
	namespace := cmp.Or(os.Getenv(envNamespace), DefaultNamespace)
	image := cmp.Or(os.Getenv(envImage), "alpine:3")

	options := Options{
		Namespace:  namespace,
		Kubeconfig: kubeconfig,
		// The desktop's image is the operator's: with none named this driver
		// declares no Display and the suite skips every case about a screen.
		DisplayImage: os.Getenv(envDisplayImage),
		// A first pull and a volume that binds on first use both happen
		// inside Create, so the budget is the cluster's, not the suite's.
		ReadyTimeout: 3 * time.Minute,
		GracePeriod:  time.Second,
	}
	first, err := New(options)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	ctx := t.Context()
	if _, err := first.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		Name: namespace,
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("namespace %s: %v", namespace, err)
	}
	if err := first.Preflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	t.Cleanup(func() { sweep(t, first) })

	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		d, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}, runtimetest.Options{
		Image: image, Shell: []string{"sh", "-c"},
		// The desktop the display cases drive is the display container's, so
		// the sandbox's own image is the same one every other case uses.
		DisplayImage: cmp.Or(os.Getenv(envDisplayImage), ""),
		Listen:       listenCommand,
		Echo:         echoCommand,
	})
}

// listenCommand binds one port inside a sandbox. What binds a port belongs to
// the image, not to the contract, and this is what the suite's own image has.
func listenCommand(port int) []string {
	return []string{"sh", "-c", fmt.Sprintf("nc -l -p %d || sleep 600", port)}
}

// echoCommand serves one port inside a sandbox and writes back every byte
// each connection sends, two connections at once: busybox's netcat runs its
// own cat per connection, which is what the suite's image has.
func echoCommand(port int) []string {
	return []string{"nc", "-lk", "-p", strconv.Itoa(port), "-e", "cat"}
}

// sweep removes every object this contract owns in the namespace, so a run
// that failed halfway leaves the cluster as it found it.
func sweep(t *testing.T, d *Driver) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	states, err := d.List(ctx, driver.Filter{})
	if err != nil {
		t.Logf("listing what to clean up: %v", err)
		return
	}
	for _, s := range states {
		if err := d.Delete(ctx, s.ID); err != nil {
			t.Logf("cleaning up %s: %v", s.ID, err)
		}
	}
	pods, err := d.cs.CoreV1().Pods(d.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return
	}
	for _, pod := range pods.Items {
		if err := d.deletePodNamed(ctx, pod.Name); err != nil {
			t.Logf("cleaning up the pod %s: %v", pod.Name, err)
		}
	}
	secrets, err := d.cs.CoreV1().Secrets(d.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return
	}
	for _, secret := range secrets.Items {
		if err := d.cs.CoreV1().Secrets(d.opts.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("cleaning up the secret %s: %v", secret.Name, err)
		}
	}
}
