// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"latere.ai/x/cella/manifest"
	driver "latere.ai/x/cella/runtime"
)

func TestCreateInspectDelete(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_create"
	ref, err := h.Create(t.Context(), spec(id))
	if err != nil || ref.ID != id {
		t.Fatalf("Create = %+v %v", ref, err)
	}
	if got := h.claimOf(t, id).Labels[labelID]; got != id {
		t.Fatalf("claim id label %q", got)
	}
	if got := h.podOf(t, id).Spec.Containers[0].Image; got != image {
		t.Fatalf("pod image %q", got)
	}
	// A second create of one id is refused, and the sandbox that exists is
	// left exactly as it was.
	other := spec(id)
	other.Name = "overwritten"
	if _, err := h.Create(t.Context(), other); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
	state, err := h.Inspect(t.Context(), id)
	if err != nil || state.Name != "fixture" {
		t.Fatalf("the refused create changed the sandbox: %+v %v", state, err)
	}
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inspect(t.Context(), id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Inspect after Delete = %v", err)
	}
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
	if err := h.Delete(t.Context(), "not a legal id"); err != nil {
		t.Fatalf("Delete of an illegal id = %v, want nil", err)
	}
}

func TestCreateRollsBack(t *testing.T) {
	h := newHarness(t)
	h.cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("admission denied the pod")
	})
	const id = "sbx_rollback"
	if _, err := h.Create(t.Context(), spec(id)); err == nil {
		t.Fatal("Create with a refused pod returned no error")
	}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the claim of a failed create survived: %v", err)
	}
}

func TestCreateRollsBackWhenThePodNeverStarts(t *testing.T) {
	h := newHarness(t)
	h.status = pending("ImagePullBackOff", "Back-off pulling image")
	h.opts.ReadyTimeout = 300 * time.Millisecond
	const id = "sbx_neverstarts"
	_, err := h.Create(t.Context(), spec(id))
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("Create = %v, want the reason the pod gave", err)
	}
	for _, get := range []func() error{
		func() error {
			_, e := h.cs.CoreV1().Pods(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{})
			return e
		},
		func() error {
			_, e := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{})
			return e
		},
	} {
		if err := get(); !apierrors.IsNotFound(err) {
			t.Fatalf("a failed create left an object behind: %v", err)
		}
	}
}

// A create whose caller hangs up still cleans up: a rollback that inherited
// the cancelled context would delete nothing.
func TestCreateRollsBackAfterCancellation(t *testing.T) {
	h := newHarness(t)
	h.status = func(*corev1.Pod) {}
	h.opts.ReadyTimeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	const id = "sbx_cancelled"
	// The caller hangs up at the first read of the Pod, which is inside the
	// wait for readiness, so the create fails where the rollback has both
	// objects to clean up.
	var once sync.Once
	h.cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		once.Do(cancel)
		return false, nil, nil
	})
	if _, err := h.Create(ctx, spec(id)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create = %v, want the caller's cancellation", err)
	}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the cancelled create leaked its claim: %v", err)
	}
}

func TestStopStartKeepsTheClaim(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_cycle"
	first := h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Phase != driver.Stopped || stopped.StoppedAt.IsZero() {
		t.Fatalf("after Stop: %+v", stopped)
	}
	if stopped.StoppedAt.Before(first.StartedAt) {
		t.Fatalf("stoppedAt %v before startedAt %v", stopped.StoppedAt, first.StartedAt)
	}
	// The claim is the sandbox and survives the Pod.
	h.claimOf(t, id)
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	again, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !again.StoppedAt.Equal(stopped.StoppedAt) {
		t.Fatalf("a second Stop restamped: %v then %v", stopped.StoppedAt, again.StoppedAt)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	started, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != driver.Running || !started.StoppedAt.IsZero() {
		t.Fatalf("after Start: %+v", started)
	}
	if started.StartedAt.Before(stopped.StoppedAt) {
		t.Fatalf("startedAt %v did not advance past stoppedAt %v", started.StartedAt, stopped.StoppedAt)
	}
	if !started.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("createdAt moved from %v to %v", first.CreatedAt, started.CreatedAt)
	}
	// A second start of a running sandbox changes nothing.
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	twice, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !twice.StartedAt.Equal(started.StartedAt) {
		t.Fatalf("a second Start restamped: %v then %v", started.StartedAt, twice.StartedAt)
	}
	// The Pod of the new leg carries the spec the claim kept.
	if got := h.podOf(t, id).Labels[labelID]; got != id {
		t.Fatalf("restarted pod id label %q", got)
	}
}

func TestStartReplacesAPodThatEnded(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_restart"
	h.created(t, spec(id))
	pod := h.podOf(t, id)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: metav1.Now()}},
	}}
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	failed, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Phase != phaseFailed || failed.ExitCode == nil || *failed.ExitCode != 1 {
		t.Fatalf("state after the process ended: %+v", failed)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	restarted, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Phase != driver.Running {
		t.Fatalf("phase after Start: %q", restarted.Phase)
	}
}

func TestStartRollsBackOnTimeout(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_startfail"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	h.status = pending("", "")
	h.opts.ReadyTimeout = 300 * time.Millisecond
	err := h.Start(t.Context(), id)
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("Start = %v, want a readiness failure", err)
	}
	if _, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), objectName(id), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the pod of a failed start survived: %v", err)
	}
	// The record still says stopped: a start that failed replaced nothing.
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != driver.Stopped || state.StoppedAt.IsZero() {
		t.Fatalf("after the failed start: %+v", state)
	}
}

func TestWaitReadyReports(t *testing.T) {
	h := newHarness(t)
	h.opts.ReadyTimeout = 300 * time.Millisecond
	const id = "sbx_wait"
	// No Pod at all: the wait says so rather than naming a state it never saw.
	err := h.waitReady(t.Context(), id)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("waitReady with no pod = %v", err)
	}
	h.status = func(*corev1.Pod) {}
	unschedulable := podWith(objectName(id), corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes are available: insufficient memory",
	}}})
	if _, err := h.cs.CoreV1().Pods(namespace).Create(t.Context(), unschedulable, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err = h.waitReady(t.Context(), id)
	if err == nil || !strings.Contains(err.Error(), "0/3 nodes are available") {
		t.Fatalf("waitReady = %v, want the scheduler's answer", err)
	}
	// A caller that hung up keeps its own error: a cancellation is not a
	// verdict on the Pod.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.waitReady(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady after cancellation = %v", err)
	}
}

func TestLifecycleNotFound(t *testing.T) {
	h := newHarness(t)
	const absent = "sbx_absent"
	labels := map[string]string{"a": "1"}
	for name, call := range map[string]func() error{
		"Start":  func() error { return h.Start(t.Context(), absent) },
		"Stop":   func() error { return h.Stop(t.Context(), absent) },
		"Update": func() error { return h.Update(t.Context(), absent, driver.Change{Labels: &labels}) },
		"Touch":  func() error { return h.Touch(t.Context(), absent) },
		"Inspect": func() error {
			_, err := h.Inspect(t.Context(), absent)
			return err
		},
		"Exec": func() error {
			_, err := h.Exec(t.Context(), absent, driver.ExecRequest{Command: []string{"true"}})
			return err
		},
		"Logs": func() error {
			_, err := h.Logs(t.Context(), absent, driver.LogsRequest{})
			return err
		},
		"ExportTar": func() error { return h.ExportTar(t.Context(), absent, nil, nil) },
		"ImportTar": func() error { return h.ImportTar(t.Context(), absent, driver.DefaultWorkdir, strings.NewReader("")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("%s = %v, want ErrNotFound", name, err)
			}
		})
	}
}

func TestUpdateEveryMutableField(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_update"
	s := spec(id)
	s.Labels = map[string]string{"a": "1"}
	s.Env = map[string]string{"GREETING": "hello"}
	s.Lifecycle = driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute}
	created := h.created(t, s)

	labels := map[string]string{"b": "2"}
	if err := h.Update(t.Context(), id, driver.Change{Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	after, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(after.Labels, labels) {
		t.Fatalf("labels %v", after.Labels)
	}
	environment := map[string]string{"GREETING": "changed"}
	if err := h.Update(t.Context(), id, driver.Change{Env: &environment}); err != nil {
		t.Fatal(err)
	}
	// The environment reaches the next command without a restart, because the
	// wrapper reads it from the record rather than from the Pod.
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"printenv"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.exec.last().argv, " "); !strings.Contains(got, "GREETING=changed") {
		t.Fatalf("argv %q", got)
	}
	if err := h.Update(t.Context(), id, driver.Change{Lifecycle: &driver.Lifecycle{TTL: 2 * time.Hour, AutoStop: 2 * time.Minute, AutoDelete: 3 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	after, err = h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.AutoStop != 2*time.Minute || after.AutoDelete != 3*time.Minute {
		t.Fatalf("lifecycle %v %v", after.AutoStop, after.AutoDelete)
	}
	if !after.ExpiresAt.Equal(created.CreatedAt.Add(2 * time.Hour)) {
		t.Fatalf("expiresAt %v, want createdAt+2h %v", after.ExpiresAt, created.CreatedAt.Add(2*time.Hour))
	}
	// A zero lifecycle is no deadline at all, so the keys leave the record.
	if err := h.Update(t.Context(), id, driver.Change{Lifecycle: &driver.Lifecycle{}}); err != nil {
		t.Fatal(err)
	}
	after, err = h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ExpiresAt.IsZero() || after.AutoStop != 0 || after.AutoDelete != 0 {
		t.Fatalf("a zero lifecycle left %+v", after)
	}
	for _, key := range []string{annExpiresAt, annAutoStop, annAutoDelete} {
		if _, ok := h.claimOf(t, id).Annotations[key]; ok {
			t.Errorf("%s is still stamped", key)
		}
	}
	if err := h.Update(t.Context(), id, driver.Change{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Update(t.Context(), id, driver.Change{Lifecycle: &driver.Lifecycle{TTL: -time.Second}}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a negative ttl was accepted: %v", err)
	}
}

func TestUpdateRetriesTheConflict(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_conflict"
	h.created(t, spec(id))
	// One writer lands between the read and the patch, so the guard fails and
	// the mutation is rebuilt from the record as it now stands.
	var once sync.Once
	var raced int
	claims := corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")
	tracker := h.cs.Tracker()
	h.cs.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		// The tracker is written through directly: the fake holds its own
		// lock across the whole reaction chain, so a client call from inside
		// a reactor would wait on the call that entered it.
		once.Do(func() {
			raced++
			obj, err := tracker.Get(claims, namespace, objectName(id))
			if err != nil {
				return
			}
			pvc := obj.(*corev1.PersistentVolumeClaim)
			s, err := specOf(pvc)
			if err != nil {
				return
			}
			s.Owner = "the other writer"
			encoded, _ := json.Marshal(s)
			pvc.Annotations[annSpec] = string(encoded)
			if err := tracker.Update(claims, pvc, namespace); err != nil {
				t.Errorf("the racing write failed: %v", err)
			}
		})
		return false, nil, nil
	})
	labels := map[string]string{"after": "the race"}
	if err := h.Update(t.Context(), id, driver.Change{Labels: &labels}); err != nil {
		t.Fatalf("Update = %v, want the retry to land", err)
	}
	if raced != 1 {
		t.Fatalf("the race never happened")
	}
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(state.Labels, labels) {
		t.Fatalf("labels %v, want the retried mutation", state.Labels)
	}
	// The other writer's field survived, so the retry rebuilt the record
	// rather than writing back the spec it first read.
	kept, err := specOf(h.claimOf(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if kept.Owner != "the other writer" {
		t.Fatalf("the spec annotation reads %q, want the other writer's value kept", kept.Owner)
	}
}

func TestUpdateGivesUpAndSaysSo(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_persistent"
	h.created(t, spec(id))
	attempts := 0
	h.cs.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		attempts++
		return true, nil, apierrors.NewConflict(corev1.Resource("persistentvolumeclaims"), objectName(id), errors.New("busy"))
	})
	labels := map[string]string{"x": "1"}
	err := h.Update(t.Context(), id, driver.Change{Labels: &labels})
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("Update = %v, want a message about the lost attempts", err)
	}
	if attempts != patchAttempts {
		t.Fatalf("%d attempts, want %d", attempts, patchAttempts)
	}
}

func TestUpdateStopsAtASettledRefusal(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_forbidden"
	h.created(t, spec(id))
	attempts := 0
	h.cs.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		attempts++
		return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), objectName(id), errors.New("no rule"))
	})
	labels := map[string]string{"x": "1"}
	if err := h.Update(t.Context(), id, driver.Change{Labels: &labels}); err == nil {
		t.Fatal("a forbidden patch was reported as success")
	}
	if attempts != 1 {
		t.Fatalf("%d attempts, want one: a refusal is not a race", attempts)
	}
}

// The guard is real: a patch built from one reading of the record does not
// land on a record that has moved on.
func TestPatchGuardsAgainstAStaleRead(t *testing.T) {
	h := newHarness(t)
	versioned(h.cs)
	const id = "sbx_guard"
	h.created(t, spec(id))
	pvc := h.claimOf(t, id)
	if pvc.ResourceVersion == "" {
		t.Fatal("the fixture carries no resource version to guard with")
	}
	first, err := patchFor(pvc, change{set: map[string]string{annStoppedAt: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"/metadata/resourceVersion"`) {
		t.Fatalf("the patch carries no version guard: %s", first)
	}
	stale := pvc.DeepCopy()
	stale.ResourceVersion = "999"
	second, err := patchFor(stale, change{set: map[string]string{annStoppedAt: "two"}})
	if err != nil {
		t.Fatal(err)
	}
	claims := h.cs.CoreV1().PersistentVolumeClaims(namespace)
	if _, err := claims.Patch(t.Context(), pvc.Name, types.JSONPatchType, first, metav1.PatchOptions{}); err != nil {
		t.Fatalf("the guarded patch of a current read was refused: %v", err)
	}
	if _, err := claims.Patch(t.Context(), pvc.Name, types.JSONPatchType, second, metav1.PatchOptions{}); err == nil {
		t.Fatal("a patch built from a stale read landed")
	}
	if got := h.claimOf(t, id).Annotations[annStoppedAt]; got != "one" {
		t.Fatalf("stopped-at %q", got)
	}
}

// A key holding a slash is escaped, so the patch names the key it means.
func TestPatchEscapesTheKey(t *testing.T) {
	// The domain's slash is the escape the pointer needs; it is read from the
	// contract rather than written out, so this file is not itself a
	// coordinate.
	if got, want := annPath(annSpec), "/metadata/annotations/"+manifest.ReservedKeyDomain+"~1spec"; got != want {
		t.Fatalf("annPath = %q, want %q", got, want)
	}
	if got := annPath("a~b/c"); got != "/metadata/annotations/a~0b~1c" {
		t.Fatalf("annPath = %q", got)
	}
	pvc := &corev1.PersistentVolumeClaim{Name: "bare"}
	patch, err := patchFor(pvc, change{set: map[string]string{annStoppedAt: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), `{"op":"add","path":"/metadata/annotations","value":{}}`) {
		t.Fatalf("a claim with no annotations is not given the map: %s", patch)
	}
}

func TestTouchStampsActivity(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_touch"
	before := h.created(t, spec(id))
	if err := h.Touch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	after, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastActivityAt.After(before.LastActivityAt) {
		t.Fatalf("lastActivityAt %v did not advance past %v", after.LastActivityAt, before.LastActivityAt)
	}
	// Both objects carry the stamp, so a reader of either sees the activity.
	if h.podOf(t, id).Annotations[annActivityAt] == "" {
		t.Fatal("the pod carries no activity stamp")
	}
	// A stopped sandbox still takes a touch: the claim is the record.
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Touch(t.Context(), id); err != nil {
		t.Fatalf("Touch while stopped = %v", err)
	}
	stopped, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.LastActivityAt.After(after.LastActivityAt) {
		t.Fatalf("a touch while stopped did not land: %v", stopped.LastActivityAt)
	}
}

func TestTouchReportsAFailedPatch(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_touchfail"
	h.created(t, spec(id))
	h.cs.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("patch denied")
	})
	if err := h.Touch(t.Context(), id); err == nil {
		t.Fatal("a failed pod patch was reported as success")
	}
}

func TestDeleteReportsAFailedDelete(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_deletefail"
	h.created(t, spec(id))
	h.cs.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("delete denied")
	})
	if err := h.Delete(t.Context(), id); err == nil {
		t.Fatal("a failed claim delete was reported as success")
	}
}

func TestWaitGoneGivesUp(t *testing.T) {
	h := newHarness(t)
	h.opts.ReadyTimeout = 200 * time.Millisecond
	const id = "sbx_stuck"
	h.created(t, spec(id))
	// A finalizer holds the Pod, which is what a terminating Pod looks like.
	h.cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, nil
	})
	err := h.Stop(t.Context(), id)
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("Stop = %v, want the stuck pod named", err)
	}
}
