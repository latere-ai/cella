// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// The phases this driver reports. Pending, Running and Stopped are the
// contract's constants; the rest are the names spec 004 fixes for every
// driver.
const (
	phaseStarting = "Starting"
	phaseStopping = "Stopping"
	phaseDeleting = "Deleting"
	phaseFailed   = "Failed"
	phaseLost     = "Lost"
)

// Inspect reads one sandbox's two objects and assembles its state.
func (d *Driver) Inspect(ctx context.Context, id string) (driver.State, error) {
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return driver.State{}, err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return driver.State{}, err
	}
	s := state(pvc, pod)
	// The port probe is a command inside the sandbox, so it runs at Inspect
	// and never at List: a sweep of every sandbox is the reaper's path, and a
	// command per sandbox per tick is a cost it must not carry.
	if s.Phase == driver.Running {
		if spec, err := specOf(pvc); err == nil {
			s.Ports = d.probePorts(ctx, id, spec.Ports)
		}
	}
	return s, nil
}

// getClaim reads the claim of one id. The object name is derived, so the id
// label is what decides the answer: a name that belongs to another sandbox is
// not this one.
func (d *Driver) getClaim(ctx context.Context, id string) (*corev1.PersistentVolumeClaim, error) {
	if !validID.MatchString(id) {
		return nil, driver.ErrNotFound
	}
	pvc, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Get(ctx, objectName(id), metav1.GetOptions{})
	if err != nil {
		return nil, mapErr(err, "claim get")
	}
	if pvc.Labels[labelID] != id {
		return nil, driver.ErrNotFound
	}
	return pvc, nil
}

// getPod reads the Pod of one id, or nil when the sandbox is stopped.
func (d *Driver) getPod(ctx context.Context, id string) (*corev1.Pod, error) {
	pod, err := d.cs.CoreV1().Pods(d.opts.Namespace).Get(ctx, objectName(id), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("pod get: %w", err)
	case pod.Labels[labelID] != id:
		return nil, nil
	}
	return pod, nil
}

// List answers from the two label-selected collections. Owner and phase are
// not labels: the owner is an annotation because an address is no legal label
// value, and the phase is derived rather than stored, so both narrow the
// assembled states rather than the query.
func (d *Driver) List(ctx context.Context, f driver.Filter) ([]driver.State, error) {
	claims, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("claim list: %w", err)
	}
	pods, err := d.cs.CoreV1().Pods(d.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return nil, fmt.Errorf("pod list: %w", err)
	}
	byID := map[string]*corev1.Pod{}
	for i := range pods.Items {
		byID[pods.Items[i].Labels[labelID]] = &pods.Items[i]
	}
	out := []driver.State{}
	for i := range claims.Items {
		pvc := &claims.Items[i]
		id := pvc.Labels[labelID]
		if id == "" {
			continue
		}
		if s := state(pvc, byID[id]); f.Selects(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// state is the whole read path: the two objects in, one State out, no other
// input. The claim carries the identity and every instant; the Pod carries the
// phase and, once it has ended, the exit code.
func state(pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) driver.State {
	s := driver.State{
		ID:             pvc.Labels[labelID],
		Name:           pvc.Labels[labelName],
		Owner:          pvc.Annotations[annOwner],
		Isolation:      "container",
		CreatedAt:      parseStamp(pvc.Annotations, annCreatedAt),
		StartedAt:      parseStamp(pvc.Annotations, annStartedAt),
		LastActivityAt: parseStamp(pvc.Annotations, annActivityAt),
		ExpiresAt:      parseStamp(pvc.Annotations, annExpiresAt),
		AutoStop:       parseDuration(pvc.Annotations, annAutoStop),
		AutoDelete:     parseDuration(pvc.Annotations, annAutoDelete),
		Pool:           pvc.Labels[labelPool] == "true",
	}
	if spec, err := specOf(pvc); err == nil {
		s.Name = spec.Name
		s.Labels = maps.Clone(spec.Labels)
	}
	if pod != nil {
		if touched := parseStamp(pod.Annotations, annActivityAt); touched.After(s.LastActivityAt) {
			s.LastActivityAt = touched
		}
	}
	s.Phase, s.Reason, s.ExitCode, s.StoppedAt = phase(pvc, pod)
	if pod != nil {
		if condition, ok := displayReady(pod); ok {
			s.Conditions = append(s.Conditions, condition)
		}
	}
	return s
}

// phase applies the package's phase table.
func phase(pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) (string, string, *int, time.Time) {
	switch {
	case pvc.DeletionTimestamp != nil:
		return phaseDeleting, "", nil, parseStamp(pvc.Annotations, annStoppedAt)
	case pvc.Status.Phase == corev1.ClaimLost:
		return phaseLost, "ClaimLost", nil, parseStamp(pvc.Annotations, annStoppedAt)
	case pod == nil:
		// A claim with no Pod is a stopped sandbox, never a lost one: the
		// workspace is intact and a Start brings it back.
		return driver.Stopped, "", nil, parseStamp(pvc.Annotations, annStoppedAt)
	case pod.DeletionTimestamp != nil:
		return phaseStopping, "", nil, time.Time{}
	}
	code, finished, ended := terminated(pod)
	switch {
	case ended && code != 0:
		return phaseFailed, terminationReason(pod), &code, finished
	case ended:
		return driver.Stopped, "", &code, finished
	case pod.Status.Phase == corev1.PodFailed:
		return phaseFailed, pod.Status.Reason, nil, time.Time{}
	case pod.Status.Phase == corev1.PodRunning && ready(pod):
		return driver.Running, "", nil, time.Time{}
	case pod.Status.Phase == corev1.PodRunning:
		return phaseStarting, waiting(pod), nil, time.Time{}
	default:
		return driver.Pending, waiting(pod), nil, time.Time{}
	}
}

// terminated reports the workload container's exit, which is the sandbox's own
// result. A sidecar's exit is not the sandbox's.
func terminated(pod *corev1.Pod) (code int, finished time.Time, ok bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != Container || cs.State.Terminated == nil {
			continue
		}
		return int(cs.State.Terminated.ExitCode), cs.State.Terminated.FinishedAt.UTC(), true
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return 0, time.Time{}, true
	}
	return 0, time.Time{}, false
}

func terminationReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == Container && cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	return pod.Status.Reason
}

// ready reports whether the workload is ready, which is the sandbox's own
// readiness. The Pod's condition is not it: a Pod carrying the desktop of spec
// 023 is not Ready until the desktop is, and a sandbox whose desktop has not
// come up is a running sandbox with DisplayReady false, never one stuck at
// Starting. A Pod whose container statuses are not written yet falls back to
// the Pod condition, which is what a cluster answers before the kubelet
// reports.
func ready(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == Container {
			return cs.Ready
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waiting says what the Pod is waiting for, so a sandbox that never starts
// carries the cluster's own answer rather than a timeout. An image still
// pulling and an unschedulable Pod are the two an operator meets.
func waiting(pod *corev1.Pod) string {
	statuses := slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			if w.Message != "" {
				return fmt.Sprintf("container %s is %s: %s", cs.Name, w.Reason, w.Message)
			}
			return fmt.Sprintf("container %s is %s", cs.Name, w.Reason)
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue {
			if c.Message != "" {
				return "pod not scheduled: " + c.Message
			}
			return "pod not scheduled"
		}
	}
	return "pod phase " + string(pod.Status.Phase)
}
