// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	driver "latere.ai/x/cella/runtime"
)

// The two subresources of the cluster that sizing a sandbox inside a ceiling
// reads and writes (spec 078), named once so the access review, the
// discovery read and the calls agree.
const (
	// resizeSubresource is the Pod subresource a running container's
	// requests and limits are changed through.
	resizeSubresource = "resize"
	// metricsGroup and metricsVersion are the metrics API, which serves the
	// working set of each container of a Pod.
	metricsGroup   = "metrics.k8s.io"
	metricsVersion = "v1beta1"
)

// sizingVerbs are the accesses sizing uses beyond the verbs table. They are
// not required: a service account without them starts as before, and the
// driver declares no memory autosize and changes a size at a start alone.
var sizingVerbs = []struct{ group, resource, subresource, verb string }{
	{"", "pods", resizeSubresource, "patch"},
	{metricsGroup, "pods", "", "get"},
}

// killReadTimeout bounds the one command that reads the kill count, so a
// sandbox that cannot run it holds a reading up for this long and no longer.
const killReadTimeout = 10 * time.Second

// killCountCommand prints the kernel's memory events of the workload's own
// cgroup: memory.events where the node runs cgroup v2, memory.oom_control
// where it runs v1. Both are the kernel's files, and the one the node does
// not have is an error cat reports and the reader ignores.
var killCountCommand = []string{"cat", "/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/memory/memory.oom_control"}

// meter reads the metrics API's sample of one container. It is an interface
// for the reason the exec stream is: the API is served by a live cluster, and
// everything around the read is exercised against a client double.
type meter interface {
	workingSet(ctx context.Context, pod, container string) (bytes int64, at time.Time, err error)
}

// restMeter reads the sample over the cluster's own REST client.
type restMeter struct {
	client    rest.Interface
	namespace string
}

// podMetrics is the part of a metrics.k8s.io PodMetrics object this driver
// reads: when the sample was taken, and each container's working set.
type podMetrics struct {
	Timestamp  time.Time `json:"timestamp"`
	Containers []struct {
		Name  string `json:"name"`
		Usage struct {
			Memory string `json:"memory"`
		} `json:"usage"`
	} `json:"containers"`
}

func (m *restMeter) workingSet(ctx context.Context, pod, container string) (int64, time.Time, error) {
	raw, err := m.client.Get().
		AbsPath("/apis", metricsGroup, metricsVersion, "namespaces", m.namespace, "pods", pod).
		DoRaw(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	return readWorkingSet(raw, container)
}

// readWorkingSet picks one container's working set out of a PodMetrics body.
// The workload's container alone is read: a Pod's sum would count the
// desktop beside it against a limit the desktop does not run under.
func readWorkingSet(raw []byte, container string) (int64, time.Time, error) {
	var sample podMetrics
	if err := json.Unmarshal(raw, &sample); err != nil {
		return 0, time.Time{}, fmt.Errorf("pod metrics: %w", err)
	}
	for _, c := range sample.Containers {
		if c.Name != container {
			continue
		}
		q, err := resource.ParseQuantity(c.Usage.Memory)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("pod metrics: memory %q: %w", c.Usage.Memory, err)
		}
		return q.Value(), sample.Timestamp.UTC(), nil
	}
	return 0, time.Time{}, fmt.Errorf("%w: the metrics API holds no sample of the container %s yet", driver.ErrNotRunning, container)
}

var _ driver.MemoryReader = (*Driver)(nil)

// detectSizing reads what the cluster serves and what the service account
// may do with it, and records what the driver can therefore declare. It runs
// at Preflight and declares only what holds: the resize subresource for a
// change in place, and that with the metrics API for memory autosize.
func (d *Driver) detectSizing(ctx context.Context) {
	inPlace, whyNot := d.usable(ctx, "v1", "pods/"+resizeSubresource, sizingVerbs[0])
	metrics, whyNoMetrics := d.usable(ctx, metricsGroup+"/"+metricsVersion, "pods", sizingVerbs[1])
	d.inPlace.Store(inPlace)
	d.memory.Store(inPlace && metrics)
	if inPlace && metrics {
		return
	}
	slog.InfoContext(ctx, "memory autosize is not declared",
		"namespace", d.opts.Namespace, "reasons", strings.Join(slices.DeleteFunc([]string{whyNot, whyNoMetrics}, func(s string) bool { return s == "" }), "; "))
}

// usable reports whether the cluster serves one resource and the service
// account holds one verb on it, with the sentence that says which is
// missing.
func (d *Driver) usable(ctx context.Context, groupVersion, name string, v struct{ group, resource, subresource, verb string }) (bool, string) {
	list, err := d.cs.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return false, fmt.Sprintf("the cluster does not serve %s: %v", groupVersion, err)
	}
	if !slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == name }) {
		return false, fmt.Sprintf("the cluster does not serve %s in %s", name, groupVersion)
	}
	allowed, err := d.may(ctx, v)
	if err != nil {
		return false, err.Error()
	}
	if !allowed {
		return false, fmt.Sprintf("the service account may not %s %s in %s", v.verb, resourceName(v.resource, v.subresource), cmpGroup(v.group))
	}
	return true, ""
}

// cmpGroup names an API group in a sentence, the core group by its name.
func cmpGroup(group string) string {
	if group == "" {
		return "the core group"
	}
	return group
}

// may asks the cluster whether the service account holds one verb.
func (d *Driver) may(ctx context.Context, v struct{ group, resource, subresource, verb string }) (bool, error) {
	review, err := d.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
			Namespace: d.opts.Namespace, Group: v.group, Resource: v.resource,
			Subresource: v.subresource, Verb: v.verb,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("access review for %s %s: %w", v.verb, resourceName(v.resource, v.subresource), err)
	}
	return review.Status.Allowed, nil
}

// checkSize refuses a change of size the cluster could not render, and one
// that names a workspace other than the sandbox's own: a claim is not
// resized through a change of size.
func (d *Driver) checkSize(ctx context.Context, id string, r *driver.Resources) error {
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return err
	}
	spec, err := specOf(pvc)
	if err != nil {
		return err
	}
	if r.Disk != "" {
		want, err := d.diskQuantity(r.Disk)
		if err != nil {
			return err
		}
		have, err := d.diskQuantity(spec.Resources.Disk)
		if err != nil {
			return err
		}
		if want.Cmp(have) != 0 {
			return fmt.Errorf("%w: a change of size does not resize the workspace, which is %s", driver.ErrUnsupported, have.String())
		}
	}
	_, err = d.resources(*r)
	return err
}

// resizeInPlace asks the cluster to give the running Pod the size the record
// holds, through the resize subresource. The kubelet applies it without
// restarting the container where the node has the room, because memory's
// resize policy is NotRequired, the cluster's default. A sandbox with no
// running Pod has nothing to change in place and starts at the record; a
// cluster that does not serve the subresource changes a size at a start
// alone.
func (d *Driver) resizeInPlace(ctx context.Context, id string) error {
	if !d.inPlace.Load() {
		return nil
	}
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return err
	}
	spec, err := specOf(pvc)
	if err != nil {
		return err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return err
	}
	if !live(pod) {
		return nil
	}
	want, err := d.resources(spec.Resources)
	if err != nil {
		return err
	}
	if sameRequirements(workloadOf(pod).Resources, want) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": []map[string]any{
		{"name": Container, "resources": want},
	}}})
	if err != nil {
		return err
	}
	_, err = d.cs.CoreV1().Pods(d.opts.Namespace).
		Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, resizeSubresource)
	if apierrors.IsNotFound(err) {
		// The Pod went between the read and the patch. The record holds
		// the size, and the next start takes it.
		return nil
	}
	if err != nil {
		return fmt.Errorf("pod resize: %w", err)
	}
	return nil
}

// live reports whether a Pod is one whose containers can still be changed:
// present, not being deleted, and not ended.
func live(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	if _, _, ended := terminated(pod); ended {
		return false
	}
	return pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

// workloadOf is the workload's container as the Pod's spec holds it.
func workloadOf(pod *corev1.Pod) corev1.Container {
	for _, c := range pod.Spec.Containers {
		if c.Name == Container {
			return c
		}
	}
	return corev1.Container{}
}

// sameRequirements compares the cpu and the memory of two renderings by
// value, so 1Gi and 1024Mi are one size.
func sameRequirements(a, b corev1.ResourceRequirements) bool {
	return sameList(a.Requests, b.Requests) && sameList(a.Limits, b.Limits)
}

func sameList(a, b corev1.ResourceList) bool {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		x, y := a[name], b[name]
		if x.Cmp(y) != 0 {
			return false
		}
	}
	return true
}

// size writes the size half of one State: what the sandbox holds now, and
// what it was asked to hold where that is not yet the same.
//
// What it holds is what the kubelet reports it gave the container, where the
// kubelet reports that, and what the Pod asks for otherwise; a stopped
// sandbox holds what its next Pod will be rendered at. The CPU is the amount
// the sandbox was granted, which the request is a fraction of, and the CPU
// limit what it may use.
func (d *Driver) size(s *driver.State, pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) {
	spec, err := specOf(pvc)
	if err != nil {
		return
	}
	asked, err := d.resources(spec.Resources)
	if err != nil {
		return
	}
	cpu, err := quantity(spec.Resources.CPU, d.opts.DefaultCPU, "cpu")
	if err != nil {
		return
	}
	held := asked.Limits
	if pod != nil {
		held = heldLimits(pod)
	}
	disk := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	s.Resources = driver.Resources{
		CPU: cpu.String(), CPULimit: quantityString(held, corev1.ResourceCPU),
		Memory: quantityString(held, corev1.ResourceMemory), Disk: disk.String(),
	}
	if pod == nil || pod.DeletionTimestamp != nil {
		return
	}
	reason, message := outstanding(pod, asked, held)
	if reason == "" {
		return
	}
	s.Resize = &driver.Resize{
		Resources: driver.Resources{
			CPU: cpu.String(), CPULimit: quantityString(asked.Limits, corev1.ResourceCPU),
			Memory: quantityString(asked.Limits, corev1.ResourceMemory),
		},
		Reason: reason, Message: message,
	}
}

func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	q := list[name]
	return q.String()
}

// heldLimits is the cpu and the memory limit the workload's container runs
// under: the kubelet's own report of what it applied, and the Pod's spec for
// a resource, or a whole container, the kubelet has not reported.
func heldLimits(pod *corev1.Pod) corev1.ResourceList {
	out := workloadOf(pod).Resources.Limits.DeepCopy()
	if out == nil {
		out = corev1.ResourceList{}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != Container || cs.Resources == nil {
			continue
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if q, ok := cs.Resources.Limits[name]; ok {
				out[name] = q
			}
		}
	}
	return out
}

// outstanding says why the size asked for is not the size held, or nothing
// where it is. The kubelet's verdict is the PodResizePending condition, and
// the status field it replaced on a cluster that still writes that one. A
// Pod whose spec does not ask for the recorded size was never asked in
// place, so only a restart gives it that size.
func outstanding(pod *corev1.Pod, asked corev1.ResourceRequirements, held corev1.ResourceList) (reason, message string) {
	if !sameRequirements(workloadOf(pod).Resources, asked) {
		return driver.ResizeAtRestart, "the running Pod was not given the size in place"
	}
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodResizePending || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case corev1.PodReasonDeferred:
			return driver.ResizeDeferred, c.Message
		case corev1.PodReasonInfeasible:
			return driver.ResizeInfeasible, c.Message
		}
	}
	switch pod.Status.Resize {
	case corev1.PodResizeStatusDeferred:
		return driver.ResizeDeferred, ""
	case corev1.PodResizeStatusInfeasible:
		return driver.ResizeInfeasible, ""
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodResizeInProgress && c.Status == corev1.ConditionTrue {
			return driver.ResizeInProgress, c.Message
		}
	}
	if !sameList(held, asked.Limits) {
		return driver.ResizeInProgress, ""
	}
	return "", ""
}

// runningPod is the Pod of a sandbox whose workload is running, which is
// what a reading is taken of.
func (d *Driver) runningPod(ctx context.Context, id string) (*corev1.Pod, error) {
	if _, err := d.getClaim(ctx, id); err != nil {
		return nil, err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return nil, err
	}
	if !live(pod) {
		return nil, driver.ErrNotRunning
	}
	if !started(pod) {
		return nil, startingErr(pod)
	}
	return pod, nil
}

// MemoryUsage reads the workload container's working set from the metrics
// API. The sample is the cluster's, taken on its own schedule, so two reads
// inside one of its windows return one sample with one instant.
func (d *Driver) MemoryUsage(ctx context.Context, id string) (driver.MemoryUsage, error) {
	if d.meter == nil {
		return driver.MemoryUsage{}, fmt.Errorf("%w: this driver was built without a cluster connection", driver.ErrUnsupported)
	}
	pod, err := d.runningPod(ctx, id)
	if err != nil {
		return driver.MemoryUsage{}, err
	}
	bytes, at, err := d.meter.workingSet(ctx, pod.Name, Container)
	if apierrors.IsNotFound(err) {
		return driver.MemoryUsage{}, fmt.Errorf("%w: the metrics API holds no sample of this sandbox yet", driver.ErrNotRunning)
	}
	if err != nil {
		return driver.MemoryUsage{}, fmt.Errorf("pod metrics: %w", err)
	}
	return driver.MemoryUsage{WorkingSet: bytes, ReadAt: at}, nil
}

// MemoryKills reads how many processes the kernel has killed for memory in
// the workload's cgroup, by one command in the workload's own container, as
// its own user, over the path a client's command takes. The Pod status says
// nothing of a kill the container survived and the metrics API serves usage
// alone, so the kernel's own counter is the one source inside the driver's
// namespace. The instance is the Pod: a start makes a new one, with a
// cgroup whose count begins again.
func (d *Driver) MemoryKills(ctx context.Context, id string) (driver.MemoryKills, error) {
	pod, err := d.runningPod(ctx, id)
	if err != nil {
		return driver.MemoryKills{}, err
	}
	count, found, err := d.readKillCount(ctx, pod.Name)
	if err != nil {
		// A container that ended while it was being read is a sandbox that
		// is no longer running, which is what its next read will say, and
		// not a reading that failed.
		if now, readErr := d.getPod(ctx, id); readErr == nil && (!live(now) || !started(now) || now.UID != pod.UID) {
			return driver.MemoryKills{}, driver.ErrNotRunning
		}
		return driver.MemoryKills{}, fmt.Errorf("kill count: %w", err)
	}
	if !found {
		return driver.MemoryKills{}, fmt.Errorf("%w: the sandbox's cgroup reports no count of kills for memory", driver.ErrUnsupported)
	}
	return driver.MemoryKills{Count: count, Instance: string(pod.UID)}, nil
}

// readKillCount runs the one command that prints the kernel's counters in
// the workload's container and sums what it printed.
func (d *Driver) readKillCount(ctx context.Context, pod string) (count int64, found bool, err error) {
	e, err := d.start(ctx, pod, execOpts{argv: killCountCommand}, killReadTimeout)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = e.Close() }()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// The error cat prints for the file this node does not have is
		// read and dropped, so the command never blocks on it.
		_, _ = io.Copy(io.Discard, e.Stderr())
	}()
	count, found, readErr := readKills(e.Stdout())
	<-drained
	if readErr != nil {
		return 0, false, readErr
	}
	// The exit code is not read: cat exits non-zero for the one file of the
	// two the node does not have, and what it printed is the answer.
	if _, err := e.Wait(ctx); err != nil {
		return 0, false, err
	}
	return count, found, nil
}

// readKills sums the kill counters of the kernel's memory events: oom_kill,
// the processes killed, and oom_group_kill, the times a whole group was. A
// rise in either is a process of the workload that ran out, and a line that
// merely begins with one of the names, oom_kill_disable among them, is not
// one of them.
func readKills(r io.Reader) (count int64, found bool, err error) {
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) != 2 || (fields[0] != "oom_kill" && fields[0] != "oom_group_kill") {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		count += n
		found = true
	}
	return count, found, lines.Err()
}
