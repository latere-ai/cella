// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

func TestDeclarations(t *testing.T) {
	h := newHarness(t)
	if h.Name() != "k8s" {
		t.Fatalf("Name = %q", h.Name())
	}
	if h.Isolation() != "container" {
		t.Fatalf("Isolation = %q", h.Isolation())
	}
	got := h.Capabilities()
	if !reflect.DeepEqual(got, driver.Capabilities{Files: true, Pool: true}) {
		t.Fatalf("Capabilities = %+v, want Files and Pool", got)
	}
	// Every capability with an optional interface behind it is undeclared,
	// because none of them is implemented here.
	if got.Attach || got.Dial || got.Display || got.Input || got.Mesh || got.Volumes || got.Snapshots || got.Resize || got.Ingress || len(got.Egress) > 0 {
		t.Fatalf("a capability is declared without its behaviour: %+v", got)
	}
}

func TestOptionDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	for _, row := range []struct {
		field string
		got   any
		want  any
	}{
		{"Namespace", o.Namespace, DefaultNamespace},
		{"RunAsUser", o.RunAsUser, DefaultRunAsUser},
		{"RunAsGroup", o.RunAsGroup, DefaultRunAsGroup},
		{"CPURequestRatio", o.CPURequestRatio, DefaultCPURequestRatio},
		{"MemoryRequestRatio", o.MemoryRequestRatio, DefaultMemoryRequestRatio},
		{"DefaultCPU", o.DefaultCPU, DefaultCPU},
		{"DefaultMemory", o.DefaultMemory, DefaultMemory},
		{"DefaultDisk", o.DefaultDisk, DefaultDisk},
		{"ReadyTimeout", o.ReadyTimeout, DefaultReadyTimeout},
		{"GracePeriod", o.GracePeriod, DefaultGracePeriod},
	} {
		if !reflect.DeepEqual(row.got, row.want) {
			t.Errorf("%s = %v, want %v", row.field, row.got, row.want)
		}
	}
	if o.Now == nil {
		t.Error("the clock is nil")
	}
	// No default names a deployment: the cluster-shaped values stay empty
	// until an operator fills them.
	if o.StorageClass != "" || o.NodeSelector != nil || o.Tolerations != nil || o.ImagePullSecrets != nil || o.Kubeconfig != "" {
		t.Errorf("a cluster-shaped default is set: %+v", o)
	}
}

func TestNewWithoutAKubeconfig(t *testing.T) {
	// An empty path means the in-cluster configuration and only that: the
	// loading rules of a workstation are never consulted, so a control plane
	// with no configuration reaches no cluster at all.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	_, err := New(Options{Namespace: namespace})
	if err == nil || !strings.Contains(err.Error(), "not running in a cluster") {
		t.Fatalf("New = %v, want the in-cluster failure", err)
	}
	if _, err := New(Options{Namespace: namespace, Kubeconfig: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("New accepted a kubeconfig that is not there")
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
clusters:
- cluster: {server: https://127.0.0.1:6443}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: c
current-context: c
users:
- name: u
  user: {token: t}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{Namespace: namespace, Kubeconfig: path})
	if err != nil {
		t.Fatal(err)
	}
	if d.stream == nil {
		t.Fatal("a driver built from a kubeconfig cannot run a command")
	}
}

func TestReady(t *testing.T) {
	h := newHarness(t)
	if err := h.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if err := h.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), "cluster") {
		t.Fatalf("Ready = %v, want the cluster named", err)
	}
	down := newHarness(t)
	down.cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("namespaces"), namespace)
	})
	if err := down.Ready(t.Context()); err == nil || !strings.Contains(err.Error(), namespace) {
		t.Fatalf("Ready = %v, want the namespace named", err)
	}
}

func TestPreflightNamesWhatIsMissing(t *testing.T) {
	h := newHarness(t)
	if err := h.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	// One rule missing is named, with the verb and the resource an operator
	// puts in the role.
	denied := newHarness(t)
	denied.cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		review.Status.Allowed = review.Spec.ResourceAttributes.Subresource != "exec"
		return true, review, nil
	})
	err := denied.Preflight(t.Context())
	if err == nil || !strings.Contains(err.Error(), "create pods/exec") {
		t.Fatalf("Preflight = %v, want the missing rule named", err)
	}
	// A cluster that refuses the review itself is a different failure.
	broken := newHarness(t)
	broken.cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the authorization API is unavailable")
	})
	if err := broken.Preflight(t.Context()); err == nil || !strings.Contains(err.Error(), "access review") {
		t.Fatalf("Preflight = %v, want the review failure", err)
	}
}

func TestPreflightChecksTheStorageClass(t *testing.T) {
	h := newHarness(t)
	h.opts.StorageClass = "fast"
	err := h.Preflight(t.Context())
	if err == nil || !strings.Contains(err.Error(), "fast") {
		t.Fatalf("Preflight = %v, want the missing storage class named", err)
	}
	if _, err := h.cs.StorageV1().StorageClasses().Create(t.Context(), &storagev1.StorageClass{
		Name: "fast",
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Preflight(t.Context()); err != nil {
		t.Fatalf("Preflight with the class present = %v", err)
	}
}

func TestPreflightNeedsTheCluster(t *testing.T) {
	h := newHarness(t)
	h.cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("no route to host")
	})
	if err := h.Preflight(t.Context()); err == nil {
		t.Fatal("Preflight passed with the cluster unreachable")
	}
}

func TestErrorMapping(t *testing.T) {
	if got := mapErr(nil, "x"); got != nil {
		t.Fatalf("mapErr(nil) = %v", got)
	}
	if got := mapErr(apierrors.NewNotFound(corev1.Resource("pods"), "p"), "x"); !errors.Is(got, driver.ErrNotFound) {
		t.Fatalf("mapErr = %v", got)
	}
	if got := mapErr(apierrors.NewAlreadyExists(corev1.Resource("pods"), "p"), "x"); !errors.Is(got, driver.ErrAlreadyExists) {
		t.Fatalf("mapErr = %v", got)
	}
	plain := errors.New("something else")
	got := mapErr(plain, "claim get")
	if !errors.Is(got, plain) || !strings.Contains(got.Error(), "claim get") {
		t.Fatalf("mapErr = %v", got)
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing failed", nil, false},
		{"a lost race", apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), "p", errors.New("busy")), true},
		{"a refused test operation", errors.New("testing value /metadata/resourceVersion failed"), true},
		{"gone", apierrors.NewNotFound(corev1.Resource("persistentvolumeclaims"), "p"), false},
		{"no rule", apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "p", errors.New("no")), false},
		{"no credentials", apierrors.NewUnauthorized("no token"), false},
		{"the caller hung up", context.Canceled, false},
		{"the budget ran out", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Fatalf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDriverTakesAClientAndAClock(t *testing.T) {
	cs := fake.NewClientset()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d, err := New(Options{Namespace: namespace, Client: cs, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	if d.stream != nil {
		t.Fatal("a driver with no cluster configuration carries a command stream")
	}
	if !d.opts.Now().Equal(at) {
		t.Fatal("the clock is not the one that was given")
	}
	// A rest configuration alone is enough to build both.
	withRest, err := New(Options{Namespace: namespace, Client: cs, REST: &rest.Config{Host: "https://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if withRest.stream == nil {
		t.Fatal("a driver with a rest configuration cannot run a command")
	}
}

func TestResourceNaming(t *testing.T) {
	if got := resourceName("pods", ""); got != "pods" {
		t.Fatalf("resourceName = %q", got)
	}
	if got := resourceName("pods", "log"); got != "pods/log" {
		t.Fatalf("resourceName = %q", got)
	}
}
