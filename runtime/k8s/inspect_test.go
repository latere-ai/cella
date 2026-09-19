// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// claimFor builds the durable half of a sandbox as the driver stamps it.
func claimFor(t *testing.T, id string, mutate ...func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
	t.Helper()
	h := newHarness(t)
	pvc, err := h.claim(spec(id), time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mutate {
		m(pvc)
	}
	return pvc
}

func podWith(name string, status corev1.PodStatus, mutate ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{Name: name, Namespace: namespace, Status: status}
	for _, m := range mutate {
		m(pod)
	}
	return pod
}

func TestPhaseTable(t *testing.T) {
	const id = "sbx_phase"
	now := metav1.Now()
	deleting := func(o *corev1.Pod) { o.DeletionTimestamp = &now }
	exit := func(code int32) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, FinishedAt: now, Reason: "Error"}},
		}}}
	}
	for _, tc := range []struct {
		name  string
		claim func(*corev1.PersistentVolumeClaim)
		pod   *corev1.Pod
		phase string
		code  *int
	}{
		{"claim only", nil, nil, driver.Stopped, nil},
		{"claim deleting", func(p *corev1.PersistentVolumeClaim) { p.DeletionTimestamp = &now }, nil, phaseDeleting, nil},
		{"claim lost", func(p *corev1.PersistentVolumeClaim) { p.Status.Phase = corev1.ClaimLost }, nil, phaseLost, nil},
		{"pod deleting", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodRunning}, deleting), phaseStopping, nil},
		{"pod pending", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodPending}), driver.Pending, nil},
		{"pod running, not ready", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodRunning}), phaseStarting, nil},
		{"pod ready", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}), driver.Running, nil},
		{"pod failed", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}), phaseFailed, nil},
		{"container exited non-zero", nil, podWith(objectName(id), exit(4)), phaseFailed, ptr(4)},
		{"container exited zero", nil, podWith(objectName(id), exit(0)), driver.Stopped, ptr(0)},
		{"pod succeeded", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodSucceeded}), driver.Stopped, ptr(0)},
		{"pod unknown", nil, podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodUnknown}), driver.Pending, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutate []func(*corev1.PersistentVolumeClaim)
			if tc.claim != nil {
				mutate = append(mutate, tc.claim)
			}
			got := state(claimFor(t, id, mutate...), tc.pod)
			if got.Phase != tc.phase {
				t.Fatalf("phase %q, want %q", got.Phase, tc.phase)
			}
			switch {
			case tc.code == nil && got.ExitCode != nil:
				t.Fatalf("exit code %d, want none", *got.ExitCode)
			case tc.code != nil && (got.ExitCode == nil || *got.ExitCode != *tc.code):
				t.Fatalf("exit code %v, want %d", got.ExitCode, *tc.code)
			}
		})
	}
}

// An orphaned claim is a stopped sandbox: the reaper's delete rule reads the
// stop instant off it, and nothing treats an intact workspace as lost.
func TestOrphanClaimIsStoppedNotLost(t *testing.T) {
	stopped := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
	pvc := claimFor(t, "sbx_orphan", func(p *corev1.PersistentVolumeClaim) {
		p.Annotations[annStoppedAt] = stopped.Format(stamp)
	})
	got := state(pvc, nil)
	if got.Phase != driver.Stopped {
		t.Fatalf("phase %q", got.Phase)
	}
	if !got.StoppedAt.Equal(stopped) {
		t.Fatalf("stoppedAt %v, want %v", got.StoppedAt, stopped)
	}
}

func TestIdentityReadsBack(t *testing.T) {
	h := newHarness(t)
	s := driver.CreateSpec{
		ID: "sbx_identity", Name: "identity", Owner: "alice@example.com", Image: image,
		Labels:    map[string]string{"team/owner": "platform", "purpose": "test"},
		Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute, AutoDelete: 2 * time.Minute},
	}
	created := h.created(t, s)
	// A second driver over the same cluster and no store of its own reads the
	// same sandbox, which is what recovery after a restart depends on.
	fresh, err := New(Options{Namespace: namespace, Client: h.cs})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.Inspect(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID || got.Name != s.Name || got.Owner != s.Owner {
		t.Fatalf("identity %+v", got)
	}
	if !maps.Equal(got.Labels, s.Labels) {
		t.Fatalf("labels %v, want %v", got.Labels, s.Labels)
	}
	if got.Isolation != "container" || got.Phase != driver.Running {
		t.Fatalf("isolation %q phase %q", got.Isolation, got.Phase)
	}
	if got.AutoStop != time.Minute || got.AutoDelete != 2*time.Minute {
		t.Fatalf("lifecycle %v %v", got.AutoStop, got.AutoDelete)
	}
	if !got.ExpiresAt.Equal(got.CreatedAt.Add(time.Hour)) {
		t.Fatalf("expiresAt %v, want createdAt+1h %v", got.ExpiresAt, got.CreatedAt.Add(time.Hour))
	}
	if got.CreatedAt.IsZero() || got.StartedAt.Before(got.CreatedAt) || got.LastActivityAt.IsZero() {
		t.Fatalf("instants %+v", got)
	}
	if !got.StoppedAt.IsZero() {
		t.Fatalf("stoppedAt %v while running", got.StoppedAt)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("two reads of one sandbox disagree: %v and %v", created.CreatedAt, got.CreatedAt)
	}
}

func TestListAndFilter(t *testing.T) {
	h := newHarness(t)
	a, b, c := "sbx_list_a", "sbx_list_b", "sbx_list_c"
	for _, s := range []driver.CreateSpec{
		{ID: a, Name: "list-a", Owner: "alice", Image: image, Labels: map[string]string{"k": "1"}},
		{ID: b, Name: "list-b", Owner: "alice", Image: image},
		{ID: c, Name: "list-c", Owner: "bob", Image: image},
	} {
		h.created(t, s)
	}
	if err := h.Stop(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	ids := func(f driver.Filter) []string {
		t.Helper()
		states, err := h.List(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range states {
			out = append(out, s.ID)
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		name string
		f    driver.Filter
		want []string
	}{
		{"all", driver.Filter{}, []string{a, b, c}},
		{"owner", driver.Filter{Owner: "alice"}, []string{a, b}},
		{"phase", driver.Filter{Phase: driver.Stopped}, []string{b}},
		{"ids", driver.Filter{IDs: []string{a, c}}, []string{a, c}},
		{"all three", driver.Filter{Owner: "alice", Phase: driver.Running, IDs: []string{a, b, c}}, []string{a}},
		{"none", driver.Filter{Owner: "nobody"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(tc.f); !slices.Equal(got, tc.want) {
				t.Fatalf("List %+v = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
	// A claim of another owner in the same namespace is not this contract's.
	foreign := &corev1.PersistentVolumeClaim{Name: "foreign", Namespace: namespace}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Create(t.Context(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := ids(driver.Filter{}); !slices.Equal(got, []string{a, b, c}) {
		t.Fatalf("List picked up an unmanaged claim: %v", got)
	}
}

func TestInspectRefusesAnotherSandboxesObject(t *testing.T) {
	h := newHarness(t)
	h.created(t, spec("sbx_mine"))
	// An object whose derived name matches but whose id label does not is a
	// different sandbox, and a read of it is a miss rather than a wrong answer.
	pvc := h.claimOf(t, "sbx_mine")
	pvc.Labels[labelID] = "sbx_theirs"
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Update(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inspect(t.Context(), "sbx_mine"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Inspect = %v, want ErrNotFound", err)
	}
	if _, err := h.Inspect(t.Context(), "not a legal id"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Inspect of an illegal id = %v, want ErrNotFound", err)
	}
}

func TestPodOfAnotherSandboxIsNotRead(t *testing.T) {
	h := newHarness(t)
	h.created(t, spec("sbx_podlabel"))
	pod := h.podOf(t, "sbx_podlabel")
	pod.Labels[labelID] = "sbx_other"
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := h.Inspect(t.Context(), "sbx_podlabel")
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != driver.Stopped {
		t.Fatalf("phase %q, want the claim read alone", got.Phase)
	}
}

func TestWaitingNamesWhatThePodIsDoing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"pull", corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "registry.example.com/base:1"`}},
		}}}, `container main is ImagePullBackOff: Back-off pulling image "registry.example.com/base:1"`},
		{"pull without a message", corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}}, "container main is ContainerCreating"},
		{"unschedulable", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: insufficient memory",
		}}}, "pod not scheduled: 0/3 nodes are available: insufficient memory"},
		{"unschedulable without a message", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		}}}, "pod not scheduled"},
		{"nothing to say", corev1.PodStatus{Phase: corev1.PodPending}, "pod phase Pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waiting(podWith("p", tc.status)); got != tc.want {
				t.Fatalf("waiting = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateOfAClaimWithoutASpecAnnotation(t *testing.T) {
	// A claim this contract owns whose spec annotation was removed by hand is
	// still read: the identity half of the record stands on its own.
	pvc := claimFor(t, "sbx_bare", func(p *corev1.PersistentVolumeClaim) { delete(p.Annotations, annSpec) })
	got := state(pvc, nil)
	if got.ID != "sbx_bare" || got.Owner != "alice@example.com" {
		t.Fatalf("state %+v", got)
	}
	if got.Labels != nil {
		t.Fatalf("labels %v, want none without the spec", got.Labels)
	}
	if _, err := specOf(pvc); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("specOf = %v, want ErrInvalid", err)
	}
	pvc.Annotations[annSpec] = "{"
	if _, err := specOf(pvc); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("specOf of a broken annotation = %v, want ErrInvalid", err)
	}
}

func TestParsersTolerateNonsense(t *testing.T) {
	annotations := map[string]string{annCreatedAt: "not a time", annAutoStop: "not a duration", annAutoDelete: "-5s"}
	if got := parseStamp(annotations, annCreatedAt); !got.IsZero() {
		t.Fatalf("parseStamp = %v, want the zero time", got)
	}
	if got := parseDuration(annotations, annAutoStop); got != 0 {
		t.Fatalf("parseDuration = %v", got)
	}
	if got := parseDuration(annotations, annAutoDelete); got != 0 {
		t.Fatalf("a negative duration read back as %v", got)
	}
}
