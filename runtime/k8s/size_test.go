// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// served are the two resources sizing needs of a cluster, as its discovery
// lists them: the resize subresource in the core group, and Pods in the
// metrics API.
func served(resize, metrics bool) []*metav1.APIResourceList {
	core := &metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods"}, {Name: "pods/exec"}}}
	if resize {
		core.APIResources = append(core.APIResources, metav1.APIResource{Name: "pods/" + resizeSubresource})
	}
	out := []*metav1.APIResourceList{core}
	if metrics {
		out = append(out, &metav1.APIResourceList{
			GroupVersion: metricsGroup + "/" + metricsVersion, APIResources: []metav1.APIResource{{Name: "nodes"}, {Name: "pods"}},
		})
	}
	return out
}

// sizing is a harness over a cluster that serves both, with Preflight run, so
// the driver declares memory autosize and changes a size in place.
func sizing(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.cs.Resources = served(true, true)
	if err := h.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.Capabilities().Autosize.Memory {
		t.Fatal("a cluster that serves the resize subresource and the metrics API declares no memory autosize")
	}
	return h
}

// fakeMeter answers the metrics API's sample, or the error it is given.
type fakeMeter struct {
	bytes int64
	at    time.Time
	err   error
	asked []string
}

func (m *fakeMeter) workingSet(_ context.Context, pod, container string) (int64, time.Time, error) {
	m.asked = append(m.asked, pod+"/"+container)
	return m.bytes, m.at, m.err
}

// kubelet plays the node's part on a Pod the fake holds: it writes what the
// workload's container was given into the container's status, with the
// conditions a resize leaves, which no controller of the fake does.
func (h *harness) kubelet(t *testing.T, id string, limits corev1.ResourceList, conditions ...corev1.PodCondition) {
	t.Helper()
	pod := h.podOf(t, id)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: Container, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		Resources: &corev1.ResourceRequirements{Limits: limits},
	}}
	pod.Status.Conditions = append([]corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, conditions...)
	pod.Status.Resize = ""
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func limits(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
}

// resizePatches is every patch of the resize subresource the client saw.
func (h *harness) resizePatches() []k8stesting.PatchAction {
	var out []k8stesting.PatchAction
	for _, action := range h.cs.Actions() {
		if patch, ok := action.(k8stesting.PatchAction); ok && action.GetSubresource() == resizeSubresource {
			out = append(out, patch)
		}
	}
	return out
}

// TestCPUAtTheCeiling is spec 078's CPU rule on the rendered Pod: with a CPU
// limit named, the limit is that ceiling and the request is the fraction of
// the CPU the sandbox was granted, so nothing more is reserved for the
// burst; and a spec that names no limit renders the Pod it always did.
func TestCPUAtTheCeiling(t *testing.T) {
	h := newHarness(t)
	s := spec("sbx_burst")
	s.Resources = driver.Resources{CPU: "500m", CPULimit: "2", Memory: "256Mi"}
	pod, err := h.pod(s, h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	got := pod.Spec.Containers[0].Resources
	if limit := got.Limits.Cpu().String(); limit != "2" {
		t.Errorf("cpu limit %s, want the ceiling 2", limit)
	}
	if request := got.Requests.Cpu().MilliValue(); request != 50 {
		t.Errorf("cpu request %dm, want %v of the 500m the sandbox was granted", request, DefaultCPURequestRatio)
	}
	if got.Limits.Memory().String() != "256Mi" || got.Requests.Memory().String() != "256Mi" {
		t.Errorf("memory %v, want it untouched by the cpu ceiling", got)
	}
	if len(pod.Spec.Containers[0].ResizePolicy) != 0 {
		t.Errorf("the Pod names a resize policy %v; the cluster's default is the one the driver relies on", pod.Spec.Containers[0].ResizePolicy)
	}

	// The same sandbox with no ceiling is byte for byte the Pod and the
	// claim it was before the field existed.
	fixed := spec("sbx_fixed")
	fixed.Resources = driver.Resources{CPU: "500m", Memory: "256Mi"}
	plain, err := h.pod(fixed, h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Spec.Containers[0].Resources; got.Limits.Cpu().String() != "500m" || got.Requests.Cpu().MilliValue() != 50 {
		t.Errorf("a sandbox with no cpu ceiling renders %v, want its limit at its cpu", got)
	}
	claim, err := h.claim(fixed, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if record := claim.Annotations[annSpec]; strings.Contains(record, "cpuLimit") || !strings.Contains(record, `"Resources":{"cpu":"500m","memory":"256Mi"}`) {
		t.Errorf("the record of a sandbox with no ceiling is %s", record)
	}

	// A limit below the cpu, or one that is no quantity, is refused before
	// any object exists.
	for _, limit := range []string{"250m", "two"} {
		s.Resources.CPULimit = limit
		if _, err := h.pod(s, h.clock.now(), false); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("a cpu limit of %q: got %v, want ErrInvalid", limit, err)
		}
	}
}

// TestAutosizeCapabilities: the driver declares the autosize of CPU always,
// of memory only where the cluster serves the resize subresource and the
// metrics API and the service account may use both, and of the workspace
// never; and a cluster or a Role without them fails no Preflight.
func TestAutosizeCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name            string
		resize, metrics bool
		deny            string
		want            v1.Autosize
		inPlace         bool
	}{
		{"both served and granted", true, true, "", v1.Autosize{CPU: true, Memory: true}, true},
		{"no resize subresource", false, true, "", v1.Autosize{CPU: true}, false},
		{"no metrics API", true, false, "", v1.Autosize{CPU: true}, true},
		{"neither", false, false, "", v1.Autosize{CPU: true}, false},
		{"the resize patch not granted", true, true, resizeSubresource, v1.Autosize{CPU: true}, false},
		{"the metrics read not granted", true, true, metricsGroup, v1.Autosize{CPU: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cs.Resources = served(tc.resize, tc.metrics)
			h.cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, kruntime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
				a := review.Spec.ResourceAttributes
				review.Status.Allowed = tc.deny == "" || (a.Subresource != tc.deny && a.Group != tc.deny)
				return true, review, nil
			})
			if before := h.Capabilities().Autosize; before != (v1.Autosize{CPU: true}) {
				t.Fatalf("before Preflight the driver declares %+v, want cpu alone", before)
			}
			if err := h.Preflight(t.Context()); err != nil {
				t.Fatalf("Preflight = %v; sizing is optional and fails nothing", err)
			}
			if got := h.Capabilities().Autosize; got != tc.want {
				t.Errorf("Autosize = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(h.Capabilities(), h.Capabilities()) {
				t.Error("the declaration differs between two calls")
			}
			if got := h.inPlace.Load(); got != tc.inPlace {
				t.Errorf("a size is changed in place: %v, want %v", got, tc.inPlace)
			}
		})
	}
	// A cluster whose access review fails for the optional verbs declares
	// nothing more and still passes.
	h := newHarness(t)
	h.cs.Resources = served(true, true)
	h.cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		if a := review.Spec.ResourceAttributes; a.Subresource == resizeSubresource || a.Group == metricsGroup {
			return true, nil, errors.New("the authorization API is unavailable")
		}
		review.Status.Allowed = true
		return true, review, nil
	})
	if err := h.Preflight(t.Context()); err != nil {
		t.Fatalf("Preflight = %v", err)
	}
	if h.Capabilities().Autosize.Memory {
		t.Error("memory autosize is declared with its access unreviewed")
	}
}

// TestAChangeOfSizeIsRecordedAndPatchedInPlace: Change.Resources writes the
// size into the claim's record, which a start reads, and asks the running
// Pod for it through the resize subresource with the request following the
// limit, and writes nothing else to the Pod.
func TestAChangeOfSizeIsRecordedAndPatchedInPlace(t *testing.T) {
	h := sizing(t)
	const id = "sbx_grow"
	s := spec(id)
	s.Resources = driver.Resources{CPU: "500m", CPULimit: "2", Memory: "256Mi", Disk: "5Gi"}
	h.created(t, s)
	next := driver.Resources{CPU: "500m", CPULimit: "2", Memory: "768Mi"}
	if err := h.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
		t.Fatal(err)
	}
	recorded, err := specOf(h.claimOf(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if want := (driver.Resources{CPU: "500m", CPULimit: "2", Memory: "768Mi", Disk: "5Gi"}); recorded.Resources != want {
		t.Fatalf("the record holds %+v, want %+v", recorded.Resources, want)
	}
	patches := h.resizePatches()
	if len(patches) != 1 {
		t.Fatalf("the resize subresource was patched %d times, want once", len(patches))
	}
	var body struct {
		Spec struct {
			Containers []struct {
				Name      string                      `json:"name"`
				Resources corev1.ResourceRequirements `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(patches[0].GetPatch(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Spec.Containers) != 1 || body.Spec.Containers[0].Name != Container {
		t.Fatalf("the patch names %+v, want the workload's container alone", body.Spec.Containers)
	}
	got := body.Spec.Containers[0].Resources
	if got.Limits.Memory().String() != "768Mi" || got.Requests.Memory().String() != "768Mi" {
		t.Errorf("the patch asks for memory %v, want the limit and the request at 768Mi", got)
	}
	if got.Limits.Cpu().String() != "2" || got.Requests.Cpu().MilliValue() != 50 {
		t.Errorf("the patch asks for cpu %v, want the limit at 2 and the request at 50m", got)
	}
	if spec := h.podOf(t, id).Spec.Containers[0].Resources; spec.Limits.Memory().String() != "768Mi" {
		t.Errorf("the Pod asks for %v after the patch", spec.Limits)
	}
	// The same size again is no second patch.
	if err := h.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.resizePatches()); n != 1 {
		t.Errorf("a size the Pod already asks for was patched again: %d patches", n)
	}
	// Every action of the change is one the Role grants.
	table := map[string]bool{}
	for _, v := range append(append([]struct{ group, resource, subresource, verb string }{}, verbs...), sizingVerbs...) {
		table[fmt.Sprintf("%s/%s/%s", v.resource, v.subresource, v.verb)] = true
	}
	for _, action := range h.cs.Actions() {
		if r := action.GetResource().Resource; r == "selfsubjectaccessreviews" || r == "resource" {
			continue
		}
		if key := fmt.Sprintf("%s/%s/%s", action.GetResource().Resource, action.GetSubresource(), action.GetVerb()); !table[key] {
			t.Errorf("the driver took %s, which neither verb table reviews", key)
		}
	}
}

// TestAChangeOfSizeRefusals: a size the cluster could not render and a
// workspace other than the sandbox's own are refused with nothing written.
func TestAChangeOfSizeRefusals(t *testing.T) {
	h := sizing(t)
	const id = "sbx_refuse"
	s := spec(id)
	s.Resources = driver.Resources{CPU: "1", Memory: "256Mi", Disk: "5Gi"}
	h.created(t, s)
	before := h.claimOf(t, id).Annotations[annSpec]
	for _, tc := range []struct {
		name string
		r    driver.Resources
		want error
	}{
		{"memory that is no quantity", driver.Resources{CPU: "1", Memory: "lots"}, driver.ErrInvalid},
		{"a cpu limit below the cpu", driver.Resources{CPU: "1", CPULimit: "500m", Memory: "256Mi"}, driver.ErrInvalid},
		{"another workspace size", driver.Resources{CPU: "1", Memory: "256Mi", Disk: "10Gi"}, driver.ErrUnsupported},
		{"a workspace size that is no quantity", driver.Resources{CPU: "1", Memory: "256Mi", Disk: "big"}, driver.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.Update(t.Context(), id, driver.Change{Resources: &tc.r}); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if after := h.claimOf(t, id).Annotations[annSpec]; after != before {
		t.Errorf("a refused change wrote the record: %s", after)
	}
	if n := len(h.resizePatches()); n != 0 {
		t.Errorf("a refused change patched the Pod %d times", n)
	}
	// The sandbox's own workspace size, spelled another way, is no change.
	same := driver.Resources{CPU: "1", Memory: "512Mi", Disk: "5120Mi"}
	if err := h.Update(t.Context(), id, driver.Change{Resources: &same}); err != nil {
		t.Fatalf("the sandbox's own workspace size was refused: %v", err)
	}
	if err := h.Update(t.Context(), "sbx_absent", driver.Change{Resources: &same}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a change of size of no sandbox: got %v, want ErrNotFound", err)
	}
}

// TestStartTakesTheLastSize is spec 078's memory of a size at the driver: a
// size a sandbox was raised to is the size its next Pod starts at, with no
// store behind the driver, and a later change of size replaces it. A
// stopped sandbox takes a change in its record alone, and a cluster that
// does not serve the resize subresource changes a size at a start alone.
func TestStartTakesTheLastSize(t *testing.T) {
	h := sizing(t)
	const id = "sbx_last"
	s := spec(id)
	s.Resources = driver.Resources{CPU: "500m", Memory: "256Mi"}
	h.created(t, s)
	grown := driver.Resources{CPU: "500m", Memory: "4Gi"}
	if err := h.Update(t.Context(), id, driver.Change{Resources: &grown}); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Resources.Memory != "4Gi" || stopped.Resize != nil {
		t.Fatalf("a stopped sandbox reports %+v with %+v outstanding, want the size it will start at", stopped.Resources, stopped.Resize)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := h.podOf(t, id).Spec.Containers[0].Resources; got.Limits.Memory().String() != "4Gi" || got.Requests.Memory().String() != "4Gi" {
		t.Fatalf("the Pod of a start asks for %v, want the 4Gi the sandbox was raised to", got)
	}

	// An explicit size while stopped is recorded and patches nothing.
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	patched := len(h.resizePatches())
	reset := driver.Resources{CPU: "1", Memory: "1Gi"}
	if err := h.Update(t.Context(), id, driver.Change{Resources: &reset}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.resizePatches()); n != patched {
		t.Fatalf("a change of size of a stopped sandbox patched a Pod: %d patches, want %d", n, patched)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	got := h.podOf(t, id).Spec.Containers[0].Resources
	if got.Limits.Memory().String() != "1Gi" || got.Limits.Cpu().String() != "1" {
		t.Fatalf("the Pod after an explicit size asks for %v, want 1 cpu and 1Gi", got.Limits)
	}

	// Without the resize subresource the size waits for a start, and the
	// state says only a restart applies it.
	old := newHarness(t)
	old.created(t, s)
	if err := old.Update(t.Context(), id, driver.Change{Resources: &grown}); err != nil {
		t.Fatal(err)
	}
	if n := len(old.resizePatches()); n != 0 {
		t.Fatalf("a cluster that serves no resize subresource was patched %d times", n)
	}
	waiting, err := old.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Resources.Memory != "256Mi" || waiting.Resize == nil || waiting.Resize.Reason != driver.ResizeAtRestart || waiting.Resize.Resources.Memory != "4Gi" {
		t.Fatalf("state = %+v with %+v outstanding, want 256Mi held and 4Gi at the next restart", waiting.Resources, waiting.Resize)
	}
	if err := old.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := old.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	started, err := old.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if started.Resources.Memory != "4Gi" || started.Resize != nil {
		t.Fatalf("after the start the sandbox holds %+v with %+v outstanding", started.Resources, started.Resize)
	}
}

// TestStateReportsTheSizeHeldAndTheKubeletsVerdict: what a sandbox holds is
// what the kubelet says it gave the container, and what is outstanding
// carries the kubelet's own verdict: in progress, deferred or infeasible,
// from the condition and from the status field that came before it.
func TestStateReportsTheSizeHeldAndTheKubeletsVerdict(t *testing.T) {
	h := sizing(t)
	const id = "sbx_verdict"
	s := spec(id)
	s.Resources = driver.Resources{CPU: "500m", CPULimit: "2", Memory: "256Mi", Disk: "5Gi"}
	h.created(t, s)
	h.kubelet(t, id, limits("2", "256Mi"))
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := (driver.Resources{CPU: "500m", CPULimit: "2", Memory: "256Mi", Disk: "5Gi"}); state.Resources != want || state.Resize != nil {
		t.Fatalf("a sandbox at its size reports %+v with %+v outstanding, want %+v and nothing", state.Resources, state.Resize, want)
	}
	next := driver.Resources{CPU: "500m", CPULimit: "2", Memory: "768Mi"}
	if err := h.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		conditions []corev1.PodCondition
		legacy     string
		held       string
		reason     string
		message    string
	}{
		{name: "asked and not yet reported", held: "256Mi", reason: driver.ResizeInProgress},
		{name: "in progress", held: "256Mi", reason: driver.ResizeInProgress,
			conditions: []corev1.PodCondition{{Type: corev1.PodResizeInProgress, Status: corev1.ConditionTrue}}},
		{name: "deferred", held: "256Mi", reason: driver.ResizeDeferred, message: "the node has no room",
			conditions: []corev1.PodCondition{{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonDeferred, Message: "the node has no room"}}},
		{name: "infeasible", held: "256Mi", reason: driver.ResizeInfeasible, message: "more than the node holds",
			conditions: []corev1.PodCondition{{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible, Message: "more than the node holds"}}},
		{name: "deferred in the older status field", held: "256Mi", reason: driver.ResizeDeferred, legacy: corev1.PodReasonDeferred},
		{name: "infeasible in the older status field", held: "256Mi", reason: driver.ResizeInfeasible, legacy: corev1.PodReasonInfeasible},
		{name: "a pending condition that is not true is no verdict", held: "256Mi", reason: driver.ResizeInProgress,
			conditions: []corev1.PodCondition{{Type: corev1.PodResizePending, Status: corev1.ConditionFalse, Reason: corev1.PodReasonDeferred}}},
		{name: "applied", held: "768Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.kubelet(t, id, limits("2", tc.held), tc.conditions...)
			if tc.legacy != "" {
				pod := h.podOf(t, id)
				// The older field spells its two verdicts as the
				// condition's reasons are spelled.
				switch tc.legacy {
				case corev1.PodReasonDeferred:
					pod.Status.Resize = corev1.PodResizeStatusDeferred
				case corev1.PodReasonInfeasible:
					pod.Status.Resize = corev1.PodResizeStatusInfeasible
				}
				if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			state, err := h.Inspect(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state.Resources.Memory != tc.held {
				t.Errorf("the sandbox holds %s, want %s", state.Resources.Memory, tc.held)
			}
			switch {
			case tc.reason == "" && state.Resize != nil:
				t.Errorf("an applied size is still outstanding: %+v", state.Resize)
			case tc.reason != "" && (state.Resize == nil || state.Resize.Reason != tc.reason || state.Resize.Message != tc.message || state.Resize.Resources.Memory != "768Mi"):
				t.Errorf("outstanding = %+v, want 768Mi %s %q", state.Resize, tc.reason, tc.message)
			}
			// The list carries the same size as the read of one.
			listed, err := h.List(t.Context(), driver.Filter{IDs: []string{id}})
			if err != nil || len(listed) != 1 {
				t.Fatalf("List = %v, %v", listed, err)
			}
			if listed[0].Resources != state.Resources || !reflect.DeepEqual(listed[0].Resize, state.Resize) {
				t.Errorf("the list reports %+v with %+v outstanding, the read %+v with %+v", listed[0].Resources, listed[0].Resize, state.Resources, state.Resize)
			}
		})
	}
	// A Pod on its way out reports its size and nothing outstanding, and a
	// claim whose record cannot be read reports no size at all.
	pod := h.podOf(t, id)
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	var leaving driver.State
	h.size(&leaving, h.claimOf(t, id), pod)
	if leaving.Resources.Memory == "" || leaving.Resize != nil {
		t.Errorf("a Pod being deleted reports %+v with %+v outstanding", leaving.Resources, leaving.Resize)
	}
	broken := h.claimOf(t, id)
	broken.Annotations[annSpec] = "{"
	var unread driver.State
	h.size(&unread, broken, nil)
	if !unread.Resources.IsZero() {
		t.Errorf("a record that cannot be read reports the size %+v", unread.Resources)
	}
	for _, bad := range []driver.Resources{{CPU: "none"}, {CPU: "1", Memory: "none"}} {
		record := spec(id)
		record.Resources = bad
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		broken.Annotations[annSpec] = string(encoded)
		h.size(&unread, broken, nil)
		if !unread.Resources.IsZero() {
			t.Errorf("a record with the size %+v reports %+v", bad, unread.Resources)
		}
	}
}

// TestAResizeTheClusterRefusesIsNamed: a patch the API server refuses is the
// caller's to hear about, with the record already holding the size, so the
// state says only a restart applies it; and a Pod that went between the read
// and the patch is no failure.
func TestAResizeTheClusterRefusesIsNamed(t *testing.T) {
	h := sizing(t)
	const id = "sbx_refused"
	s := spec(id)
	s.Resources = driver.Resources{CPU: "1", Memory: "256Mi"}
	h.created(t, s)
	refusal := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, objectName(id), nil)
	answer := error(refusal)
	h.cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		if action.GetSubresource() != resizeSubresource {
			return false, nil, nil
		}
		return true, nil, answer
	})
	next := driver.Resources{CPU: "1", Memory: "1Gi"}
	err := h.Update(t.Context(), id, driver.Change{Resources: &next})
	if err == nil || !strings.Contains(err.Error(), "pod resize") {
		t.Fatalf("Update = %v, want the refused resize named", err)
	}
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Resize == nil || state.Resize.Reason != driver.ResizeAtRestart || state.Resize.Resources.Memory != "1Gi" {
		t.Fatalf("outstanding = %+v, want 1Gi at the next restart", state.Resize)
	}
	answer = apierrors.NewNotFound(corev1.Resource("pods"), objectName(id))
	next.Memory = "2Gi"
	if err := h.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
		t.Fatalf("a Pod that went before the patch failed the change: %v", err)
	}
	// A Pod that has ended is not patched: the next start takes the record.
	pod := h.podOf(t, id)
	pod.Status.Phase = corev1.PodFailed
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	patched := len(h.resizePatches())
	next.Memory = "3Gi"
	if err := h.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.resizePatches()); n != patched {
		t.Errorf("an ended Pod was patched: %d patches, want %d", n, patched)
	}
}

// TestMemoryUsageReadsTheWorkloadsSample: the working set is the metrics
// API's sample of the workload's container with the sample's own instant,
// and a sandbox that is not running, a sample not taken yet and a driver
// with no connection each say so.
func TestMemoryUsageReadsTheWorkloadsSample(t *testing.T) {
	h := sizing(t)
	const id = "sbx_usage"
	h.created(t, spec(id))
	if _, err := h.MemoryUsage(t.Context(), id); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("a driver with no connection: got %v, want ErrUnsupported", err)
	}
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	meter := &fakeMeter{bytes: 200 << 20, at: at}
	h.meter = meter
	got, err := h.MemoryUsage(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkingSet != 200<<20 || !got.ReadAt.Equal(at) {
		t.Fatalf("usage = %+v, want 200Mi at %s", got, at)
	}
	if want := []string{objectName(id) + "/" + Container}; !reflect.DeepEqual(meter.asked, want) {
		t.Fatalf("the meter was asked for %v, want %v", meter.asked, want)
	}
	meter.err = apierrors.NewNotFound(schema.GroupResource{Group: metricsGroup, Resource: "pods"}, objectName(id))
	if _, err := h.MemoryUsage(t.Context(), id); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("no sample yet: got %v, want ErrNotRunning", err)
	}
	meter.err = errors.New("the metrics API is unavailable")
	if _, err := h.MemoryUsage(t.Context(), id); err == nil || !strings.Contains(err.Error(), "pod metrics") {
		t.Fatalf("a failed read: got %v, want the metrics API named", err)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MemoryUsage(t.Context(), id); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a stopped sandbox: got %v, want ErrNotRunning", err)
	}
	if _, err := h.MemoryUsage(t.Context(), "sbx_absent"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("no sandbox: got %v, want ErrNotFound", err)
	}
}

// TestReadWorkingSet reads the metrics API's body: the workload's container
// and no other, and what is not a sample is an error.
func TestReadWorkingSet(t *testing.T) {
	body := `{"kind":"PodMetrics","timestamp":"2026-10-11T09:30:00Z","window":"15s","containers":[
		{"name":"display","usage":{"cpu":"1m","memory":"900Mi"}},
		{"name":"main","usage":{"cpu":"250m","memory":"204800Ki"}}]}`
	bytes, at, err := readWorkingSet([]byte(body), Container)
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 200<<20 || !at.Equal(time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("read %d at %s, want the workload's 200Mi at the sample's instant", bytes, at)
	}
	if _, _, err := readWorkingSet([]byte(body), "absent"); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("a container with no sample: got %v, want ErrNotRunning", err)
	}
	if _, _, err := readWorkingSet([]byte(`{`), Container); err == nil {
		t.Error("a body that is no JSON was read")
	}
	if _, _, err := readWorkingSet([]byte(`{"containers":[{"name":"main","usage":{"memory":"much"}}]}`), Container); err == nil {
		t.Error("a memory that is no quantity was read")
	}
}

// TestMemoryKillsReadsTheKernelsCount: the count is the kernel's own, read by
// one command in the workload's container that names the kernel's two files
// and nothing of the workload's; the instance is the Pod, so a start begins
// a new one; and a cgroup that reports no count says so.
func TestMemoryKillsReadsTheKernelsCount(t *testing.T) {
	h := sizing(t)
	const id = "sbx_kills"
	h.created(t, spec(id))
	events := "low 0\nhigh 0\nmax 41\noom 3\noom_kill 2\noom_group_kill 0\n"
	exit := error(nil)
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, events)
		_, _ = io.WriteString(stderr, "cat: can't open '/sys/fs/cgroup/memory/memory.oom_control': No such file or directory\n")
		return exit
	}
	got, err := h.MemoryKills(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || got.Instance != string(h.podOf(t, id).UID) {
		t.Fatalf("kills = %+v, want 2 of the Pod's instance", got)
	}
	call := h.exec.last()
	if call.container != Container || call.stdin || call.tty {
		t.Fatalf("the count was read by %+v, want one plain command in the workload's container", call)
	}
	if !reflect.DeepEqual(call.argv, killCountCommand) || call.argv[0] != "cat" {
		t.Fatalf("the count was read by %v", call.argv)
	}
	for _, path := range call.argv[1:] {
		if !strings.HasPrefix(path, "/sys/fs/cgroup/") {
			t.Errorf("the reading opens %s, which is not the kernel's", path)
		}
	}
	// cgroup v1: the count is in memory.oom_control, beside a line that
	// only begins with its name.
	events = "oom_kill_disable 0\nunder_oom 0\noom_kill 5\n"
	if got, err = h.MemoryKills(t.Context(), id); err != nil || got.Count != 5 {
		t.Fatalf("kills = %+v, %v, want 5 from the v1 file", got, err)
	}
	// A whole group killed counts as well.
	events = "oom_kill 4\noom_group_kill 1\n"
	if got, err = h.MemoryKills(t.Context(), id); err != nil || got.Count != 5 {
		t.Fatalf("kills = %+v, %v, want the two counters summed", got, err)
	}
	// A node that reports neither file answers with no count.
	events = "nothing here\noom_kill many\n"
	if _, err = h.MemoryKills(t.Context(), id); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("no count: got %v, want ErrUnsupported", err)
	}
	events = "oom_kill 1\n"
	exit = errors.New("the stream broke")
	if _, err = h.MemoryKills(t.Context(), id); err == nil || !strings.Contains(err.Error(), "kill count") {
		t.Fatalf("a broken stream: got %v, want the reading named", err)
	}
	exit = nil
	// A start is a new instance, whose count begins again.
	first := got.Instance
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MemoryKills(t.Context(), id); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a stopped sandbox: got %v, want ErrNotRunning", err)
	}
	h.cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).UID = "second-instance"
		return false, nil, nil
	})
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got, err = h.MemoryKills(t.Context(), id); err != nil || got.Instance == first || got.Instance != "second-instance" {
		t.Fatalf("kills after a start = %+v, %v, want a new instance", got, err)
	}
	// A Pod whose workload has not started yet has nothing to read.
	pod := h.podOf(t, id)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: Container, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MemoryKills(t.Context(), id); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a starting sandbox: got %v, want ErrNotRunning", err)
	}
	// A driver with no connection cannot run the command.
	h.stream = nil
	if _, err := h.MemoryKills(t.Context(), id); err == nil {
		t.Fatal("a driver with no exec stream read a count")
	}
}

// TestTheMeterReadsTheMetricsAPI drives the read over a served endpoint: a
// driver built with a connection asks the metrics API for the one Pod in its
// own namespace, and a Pod the API has no sample of is the cluster's
// not-found, which MemoryUsage reads as no sample yet.
func TestTheMeterReadsTheMetricsAPI(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/pods/sampled") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				Kind: "Status", APIVersion: "v1", Status: metav1.StatusFailure,
				Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
			})
			return
		}
		_, _ = io.WriteString(w, `{"timestamp":"2026-10-11T09:30:00Z","containers":[{"name":"main","usage":{"memory":"64Mi"}}]}`)
	}))
	t.Cleanup(server.Close)
	d, err := New(Options{Namespace: namespace, REST: &rest.Config{Host: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	bytes, at, err := d.meter.workingSet(t.Context(), "sampled", Container)
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 64<<20 || at.IsZero() {
		t.Fatalf("read %d at %s, want 64Mi at the sample's instant", bytes, at)
	}
	want := "GET /apis/" + metricsGroup + "/" + metricsVersion + "/namespaces/" + namespace + "/pods/sampled"
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("the meter asked %v, want %s", paths, want)
	}
	if _, _, err := d.meter.workingSet(t.Context(), "unsampled", Container); !apierrors.IsNotFound(err) {
		t.Fatalf("a Pod with no sample: got %v, want the cluster's not-found", err)
	}
}

// TestAContainerThatEndedMidReadIsNotRunning: a kill that takes the container
// while its count is being read fails the command, and the reading answers
// that the sandbox is not running, which is what its next read will say,
// rather than as a reading that failed.
func TestAContainerThatEndedMidReadIsNotRunning(t *testing.T) {
	h := sizing(t)
	const id = "sbx_ended"
	h.created(t, spec(id))
	h.exec.handle = func(ctx context.Context, _ execCall, _ io.Reader, _, _ io.Writer) error {
		pods := h.cs.CoreV1().Pods(namespace)
		pod, err := pods.Get(ctx, objectName(id), metav1.GetOptions{})
		if err != nil {
			return err
		}
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: Container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
		}}
		if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return errors.New("container not found")
	}
	if _, err := h.MemoryKills(t.Context(), id); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a container that ended mid-read: got %v, want ErrNotRunning", err)
	}
}
