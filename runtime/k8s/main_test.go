// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"io"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

// namespace is the one every test drives against. It is this repository's own
// default, not an installation's.
const namespace = DefaultNamespace

// image is the container image the fixtures name. It is an example registry,
// so no deployment's image reaches this package.
const image = "registry.example.com/base:1"

// clock advances by a millisecond per read, so two stamps taken in one test
// are two instants and an advancing timestamp is observable without sleeping.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(time.Millisecond)
	return c.at
}

// harness is one driver over a client double, with the seams the unit suite
// drives: the clock, the exec stream, and the reactors on the fake.
type harness struct {
	*Driver
	cs    *fake.Clientset
	clock *clock
	exec  *recorder
	// status is the kubelet's part, applied to every Pod the fake accepts. A
	// test that wants a Pod that never starts replaces it.
	status func(*corev1.Pod)
}

// newHarness builds a driver whose Pods come up ready, whose access reviews
// pass, and whose exec stream is recorded rather than dialed.
func newHarness(t *testing.T, objects ...kruntime.Object) *harness {
	t.Helper()
	return newHarnessWith(t, nil, objects...)
}

// newHarnessWith is newHarness with the options a case needs changed, which is
// how a case drives a driver an operator configured differently.
func newHarnessWith(t *testing.T, configure func(*Options), objects ...kruntime.Object) *harness {
	t.Helper()
	cs := fake.NewClientset(objects...)
	accessAllowed(cs)
	h := &harness{
		cs:     cs,
		clock:  &clock{at: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)},
		exec:   &recorder{},
		status: running,
	}
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		h.status(pod)
		return false, pod, nil
	})
	opts := Options{Namespace: namespace, Client: cs, Now: h.clock.now, ReadyTimeout: time.Second}
	if configure != nil {
		configure(&opts)
	}
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	d.stream = h.exec
	h.Driver = d
	return h
}

// running is the kubelet's part: a Pod the API server accepted comes up ready.
// The fake tracker runs no controllers, so without this every wait for
// readiness would spend its whole budget.
func running(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

// pending is a Pod that never starts, with the reason a cluster gives.
func pending(reason, message string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}}
	}
}

// accessAllowed answers every self subject access review with yes. The fake
// has no authorizer, and an unanswered review is neither allowed nor denied.
func accessAllowed(cs *fake.Clientset) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
}

// versioned stamps a resource version on every created object and bumps it on
// every patch, which is what a real API server does and what the guard of the
// compare-and-swap reads.
func versioned(cs *fake.Clientset) {
	var mu sync.Mutex
	next := 0
	bump := func(obj kruntime.Object) {
		mu.Lock()
		defer mu.Unlock()
		next++
		if m, err := apimeta.Accessor(obj); err == nil {
			m.SetResourceVersion(strconv.Itoa(next))
		}
	}
	cs.PrependReactor("create", "*", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		bump(action.(k8stesting.CreateAction).GetObject())
		return false, nil, nil
	})
}

// recorder stands in for the exec stream. Each call is recorded, and a handler
// may write output, read stdin, or fail.
type recorder struct {
	mu     sync.Mutex
	calls  []execCall
	handle func(ctx context.Context, c execCall, stdin io.Reader, stdout, stderr io.Writer) error
}

type execCall struct {
	pod       string
	argv      []string
	container string
}

func (r *recorder) stream(ctx context.Context, pod string, o execOpts, stdin io.Reader, stdout, stderr io.Writer) error {
	c := execCall{pod: pod, argv: o.argv, container: o.name()}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	handle := r.handle
	r.mu.Unlock()
	if handle == nil {
		return nil
	}
	return handle(ctx, c, stdin, stdout, stderr)
}

func (r *recorder) last() execCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return execCall{}
	}
	return r.calls[len(r.calls)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ran is every command recorded so far.
func (r *recorder) ran() []execCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// spec is the fixture every lifecycle test starts from.
func spec(id string) driver.CreateSpec {
	return driver.CreateSpec{ID: id, Name: "fixture", Owner: "alice@example.com", Image: image}
}

// created makes one sandbox and fails the test if it does not come up.
func (h *harness) created(t *testing.T, s driver.CreateSpec) driver.State {
	t.Helper()
	if _, err := h.Create(t.Context(), s); err != nil {
		t.Fatalf("Create %s: %v", s.ID, err)
	}
	state, err := h.Inspect(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("Inspect %s: %v", s.ID, err)
	}
	return state
}

func (h *harness) podOf(t *testing.T, id string) *corev1.Pod {
	t.Helper()
	pod, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod %s: %v", id, err)
	}
	return pod
}

func (h *harness) claimOf(t *testing.T, id string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	return pvc
}
