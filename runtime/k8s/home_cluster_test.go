// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"cmp"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// TestClusterTheImagesHomeIsWritable runs the home of Options.Home on a real
// kubelet. The image's home, here the image's /etc/apk, is filled on the
// claim before the workload starts; the workload, under the read-only root
// and as the non-root user, reads the image's files there and writes its
// own; and what it wrote is read back after a stop and a start, which fill
// nothing over it.
func TestClusterTheImagesHomeIsWritable(t *testing.T) {
	kubeconfig := cmp.Or(os.Getenv(envKubeconfig), os.Getenv("KUBECONFIG"))
	if kubeconfig == "" {
		t.Skipf("no cluster: set %s (or KUBECONFIG) to a kubeconfig and %s to an alpine image", envKubeconfig, envImage)
	}
	namespace := cmp.Or(os.Getenv(envNamespace), DefaultNamespace)
	d, err := New(Options{
		Namespace: namespace, Kubeconfig: kubeconfig, Home: "/etc/apk",
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
	id := "sbx_home_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := d.Create(ctx, driver.CreateSpec{
		ID: id, Name: "home", Owner: "alice", Image: cmp.Or(os.Getenv(envImage), "alpine:3"),
		Workspace: driver.Workspace{Path: "/workspace"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Delete(context.WithoutCancel(ctx), id); err != nil {
			t.Logf("deleting %s: %v", id, err)
		}
	})

	out := inHome(t, d, id, `echo "$HOME"; test -s "$HOME/arch" && echo filled; echo kept > "$HOME/mine" && echo written`)
	if want := "/etc/apk\nfilled\nwritten\n"; out != want {
		t.Fatalf("the first start answered %q, want %q", out, want)
	}
	if err := d.Stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if out := inHome(t, d, id, `cat "$HOME/mine"`); out != "kept\n" {
		t.Fatalf("after a stop and a start the home holds %q, want what the workload wrote", out)
	}
}

// inHome runs one script in the sandbox and returns its output, failing the
// test on an error or a non-zero exit.
func inHome(t *testing.T, d *Driver, id, script string) string {
	t.Helper()
	e, err := d.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer func() { _ = e.Close() }()
	var out, errOut []byte
	var wg sync.WaitGroup
	wg.Go(func() { out, _ = io.ReadAll(e.Stdout()) })
	wg.Go(func() { errOut, _ = io.ReadAll(e.Stderr()) })
	wg.Wait()
	wait, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	code, err := e.Wait(wait)
	if err != nil || code != 0 {
		t.Fatalf("%q exited %d: %v: %s", script, code, err, strings.TrimSpace(string(errOut)))
	}
	return string(out)
}
