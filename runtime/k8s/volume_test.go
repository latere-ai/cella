// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

// The images a volume fixture names: the fill image, which carries cellad,
// and a pinned source image. Both are example registries.
const (
	fillImage   = "registry.example.com/cellad:1"
	sourceImage = "registry.example.com/grader@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// volumeHarness is a driver that fills volumes from images, provisions them
// from their own class, and places every Pod it writes on a pool.
func volumeHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(o *Options) {
		o.FillImage = fillImage
		o.StorageClass = "workspace"
		o.VolumeClass = "shared"
		o.NodeSelector = map[string]string{"pool": "sandboxes"}
		o.Tolerations = []map[string]string{{"key": "dedicated", "value": "sandboxes", "effect": "NoSchedule"}}
		o.ImagePullSecrets = []string{"pull"}
	})
}

func imageVolume(id string) driver.VolumeSpec {
	return driver.VolumeSpec{
		ID: id, Name: "grader", Owner: "alice@example.com", Size: "2Gi", Access: driver.AccessSharedRead,
		Source: driver.VolumeSource{Kind: driver.SourceImage, Image: sourceImage},
	}
}

func emptyVolume(id string) driver.VolumeSpec {
	return driver.VolumeSpec{
		ID: id, Name: "results", Owner: "alice@example.com", Size: "1Gi", Access: driver.AccessSingle,
		Source: driver.VolumeSource{Kind: driver.SourceEmpty},
	}
}

func (h *harness) volumeClaim(t *testing.T, id string) *corev1.PersistentVolumeClaim {
	t.Helper()
	claim, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(t.Context(), volumeClaimName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("volume claim %s: %v", id, err)
	}
	return claim
}

// fillPodOf reads one volume's fill Pod, nil when there is none.
func (h *harness) fillPodOf(t *testing.T, id string) *corev1.Pod {
	t.Helper()
	pod, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), fillPodName(id), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pod
}

// setFillStatus writes the kubelet's part on one volume's fill Pod.
func (h *harness) setFillStatus(t *testing.T, id string, mutate func(*corev1.Pod)) {
	t.Helper()
	pod := h.fillPodOf(t, id)
	if pod == nil {
		t.Fatalf("volume %s has no fill pod", id)
	}
	pod.Status = corev1.PodStatus{}
	mutate(pod)
	if _, err := h.cs.CoreV1().Pods(namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestVolumeClaimAndFillPodRender: an image volume is one claim of its own
// class and kind, and one fill Pod that stages the program from the fill
// image and runs it as the source image's command, on the sandboxes' pool
// and under their baseline, with no token, no service link and no sandbox
// label.
func TestVolumeClaimAndFillPodRender(t *testing.T) {
	h := volumeHarness(t)
	const id = "vol_render"
	ref, err := h.CreateVolume(t.Context(), imageVolume(id))
	if err != nil || ref.ID != id {
		t.Fatalf("CreateVolume = %+v %v", ref, err)
	}
	claim := h.volumeClaim(t, id)
	if claim.Labels[labelKind] != kindVolume || claim.Labels[labelVolume] != id || claim.Labels[labelManagedBy] != "" {
		t.Fatalf("claim labels %v: a volume is its own kind and no sandbox", claim.Labels)
	}
	for key, want := range map[string]string{
		annOwner: "alice@example.com", annVolumeName: "grader", annSize: "2Gi", annAccess: driver.AccessSharedRead,
		annSource: driver.SourceImage, annImage: sourceImage, annPhase: driver.VolumePending,
	} {
		if got := claim.Annotations[key]; got != want {
			t.Errorf("claim annotation %s = %q, want %q", key, got, want)
		}
	}
	if !slices.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
		t.Errorf("access modes %v, want ReadWriteOnce", claim.Spec.AccessModes)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "shared" {
		t.Errorf("storage class %v, want the volume class", claim.Spec.StorageClassName)
	}
	if size := claim.Spec.Resources.Requests[corev1.ResourceStorage]; size.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("size %v, want 2Gi", size.String())
	}

	pod := h.fillPodOf(t, id)
	if pod == nil {
		t.Fatal("no fill pod")
	}
	if pod.Labels[labelKind] != kindFill || pod.Labels[labelVolume] != id || pod.Labels[labelSandbox] != "" || pod.Labels[labelManagedBy] != "" {
		t.Errorf("fill pod labels %v: no sandbox rule and no sandbox list may select it", pod.Labels)
	}
	spec := pod.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever || spec.ActiveDeadlineSeconds == nil ||
		*spec.ActiveDeadlineSeconds != int64(DefaultFillTimeout/time.Second) || spec.Hostname != fillHostname {
		t.Errorf("restart %v deadline %v hostname %q", spec.RestartPolicy, spec.ActiveDeadlineSeconds, spec.Hostname)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("the fill pod carries a service account token or service links")
	}
	if !reflect.DeepEqual(spec.NodeSelector, map[string]string{"pool": "sandboxes"}) || len(spec.Tolerations) != 1 ||
		spec.Tolerations[0].Key != "dedicated" || len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "pull" {
		t.Errorf("placement %v %v %v, want the sandboxes' pool and pull secret", spec.NodeSelector, spec.Tolerations, spec.ImagePullSecrets)
	}
	sc := spec.SecurityContext
	if sc == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != DefaultRunAsUser || *sc.RunAsGroup != DefaultRunAsGroup ||
		*sc.FSGroup != DefaultRunAsGroup || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context %+v", sc)
	}
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("containers %d init and %d main, want one each", len(spec.InitContainers), len(spec.Containers))
	}
	stage, copier := spec.InitContainers[0], spec.Containers[0]
	if stage.Image != fillImage || len(stage.Command) != 0 || !slices.Equal(stage.Args, []string{"fill", "stage", "/cella-fill/cellad"}) {
		t.Errorf("stage container %q %v %v, want the fill image's entrypoint staging the program", stage.Image, stage.Command, stage.Args)
	}
	wantCommand := []string{"/cella-fill/cellad", "fill", "copy", "--max-bytes", strconv.FormatInt(2<<30, 10), "/", "/cella-volume"}
	if copier.Image != sourceImage || !slices.Equal(copier.Command, wantCommand) || len(copier.Args) != 0 {
		t.Errorf("copy container %q %v %v, want the source image running %v", copier.Image, copier.Command, copier.Args, wantCommand)
	}
	if copier.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("termination message policy %q", copier.TerminationMessagePolicy)
	}
	for _, c := range []corev1.Container{stage, copier} {
		if !reflect.DeepEqual(c.SecurityContext, hardened()) {
			t.Errorf("container %s runs without the baseline: %+v", c.Name, c.SecurityContext)
		}
		if c.Resources.Limits == nil {
			t.Errorf("container %s has no limits", c.Name)
		}
	}
	if !reflect.DeepEqual(stage.VolumeMounts, []corev1.VolumeMount{{Name: fillStage, MountPath: "/cella-fill"}}) {
		t.Errorf("stage mounts %+v", stage.VolumeMounts)
	}
	if !reflect.DeepEqual(copier.VolumeMounts, []corev1.VolumeMount{
		{Name: fillStage, MountPath: "/cella-fill", ReadOnly: true},
		{Name: kindVolume, MountPath: "/cella-volume"},
	}) {
		t.Errorf("copy mounts %+v", copier.VolumeMounts)
	}
	if len(spec.Volumes) != 2 || spec.Volumes[0].EmptyDir == nil || spec.Volumes[1].PersistentVolumeClaim == nil ||
		spec.Volumes[1].PersistentVolumeClaim.ClaimName != volumeClaimName(id) {
		t.Errorf("pod volumes %+v", spec.Volumes)
	}
	// A volume is no sandbox: the list of sandboxes does not read its
	// claim or its fill Pod.
	states, err := h.List(t.Context(), driver.Filter{})
	if err != nil || len(states) != 0 {
		t.Fatalf("List = %+v %v, want no sandbox", states, err)
	}
}

// TestFillPodIsOwnedByItsClaim: a claim the API server gave a uid owns its
// fill Pod, so removing the claim by hand takes the Pod with it.
func TestFillPodIsOwnedByItsClaim(t *testing.T) {
	h := volumeHarness(t)
	claim := &corev1.PersistentVolumeClaim{Name: volumeClaimName("vol_owned"), UID: "uid-1"}
	pod, err := h.fillPod(imageVolume("vol_owned"), resource.MustParse("1Gi"), claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != "uid-1" || pod.OwnerReferences[0].Kind != "PersistentVolumeClaim" {
		t.Fatalf("owner references %+v", pod.OwnerReferences)
	}
}

// TestEmptyVolumeIsAvailableAtOnce: an empty volume is Available once its
// claim exists, before the class binds it, makes no fill Pod, and reports
// the capacity the cluster granted once it is bound.
func TestEmptyVolumeIsAvailableAtOnce(t *testing.T) {
	h := volumeHarness(t)
	const id = "vol_empty"
	if _, err := h.CreateVolume(t.Context(), emptyVolume(id)); err != nil {
		t.Fatal(err)
	}
	if h.fillPodOf(t, id) != nil {
		t.Fatal("an empty volume has a fill pod")
	}
	state, err := h.InspectVolume(t.Context(), id)
	if err != nil || state.Phase != driver.VolumeAvailable || state.Capacity != "1Gi" || state.ID != id {
		t.Fatalf("InspectVolume = %+v %v, want Available at 1Gi", state, err)
	}
	claim := h.volumeClaim(t, id)
	claim.Status.Phase = corev1.ClaimBound
	claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if state, err := h.InspectVolume(t.Context(), id); err != nil || state.Capacity != "2Gi" {
		t.Fatalf("InspectVolume of a bound claim = %+v %v, want the granted 2Gi", state, err)
	}
}

// TestFillVerdicts: a fill Pod's state is read into the volume's phase. A
// Pod that runs is Pending; one that succeeded is Available; one that failed
// is Failed with the reason the program named, the reason of a container
// that could not start, or SourceUnreachable; a pull that fails and a fill
// past its deadline are Failed SourceUnreachable. A verdict is written on
// the claim and the Pod removed, so the next read answers from the claim.
func TestFillVerdicts(t *testing.T) {
	terminated := func(name, reason, message string, code int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, Message: message,
		}}}
	}
	waiting := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: reason, Message: "manifest unknown",
		}}}
	}
	for _, tc := range []struct {
		name    string
		status  func(*corev1.Pod)
		phase   string
		reason  string
		message string
	}{
		{"running", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }, driver.VolumePending, "", ""},
		{"succeeded", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }, driver.VolumeAvailable, "", ""},
		{"too large", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{terminated(fillCopy, "Error", "SourceTooLarge: /big takes the content past 10 bytes", 1)}
		}, driver.VolumeFailed, driver.ReasonSourceTooLarge, "/big takes the content past 10 bytes"},
		{"cannot start", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{terminated(fillCopy, "StartError", "exec format error", 128)}
		}, driver.VolumeFailed, driver.ReasonSourceUnsupported, "exec format error"},
		{"stage failed", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated(fillStage, "Error", "", 2)}
		}, driver.VolumeFailed, driver.ReasonSourceUnreachable, "the stage container exited 2 (Error)"},
		{"failed with no status", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }, driver.VolumeFailed,
			driver.ReasonSourceUnreachable, "the fill Pod failed"},
		{"deadline", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.Reason = "DeadlineExceeded"
		}, driver.VolumeFailed, driver.ReasonSourceUnreachable, "the fill did not finish within 30m0s"},
		{"source pull", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting(fillCopy, "ImagePullBackOff")}
		}, driver.VolumeFailed, driver.ReasonSourceUnreachable, "the image of the fill container could not be pulled: manifest unknown"},
		{"fill image pull", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting(fillStage, "ErrImagePull")}
		}, driver.VolumeFailed, driver.ReasonSourceUnreachable, "the image of the stage container could not be pulled: manifest unknown"},
		{"overdue", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC))
		}, driver.VolumeFailed, driver.ReasonSourceUnreachable, "the fill did not finish within 30m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := volumeHarness(t)
			const id = "vol_verdict"
			if _, err := h.CreateVolume(t.Context(), imageVolume(id)); err != nil {
				t.Fatal(err)
			}
			h.setFillStatus(t, id, tc.status)
			state, err := h.InspectVolume(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state.Phase != tc.phase || state.Reason != tc.reason || state.Message != tc.message {
				t.Fatalf("InspectVolume = %+v, want %s %q %q", state, tc.phase, tc.reason, tc.message)
			}
			if tc.phase == driver.VolumePending {
				if h.fillPodOf(t, id) == nil {
					t.Fatal("a running fill's pod was removed")
				}
				return
			}
			if h.fillPodOf(t, id) != nil {
				t.Fatal("the fill pod outlived its verdict")
			}
			claim := h.volumeClaim(t, id)
			if claim.Annotations[annPhase] != tc.phase || claim.Annotations[annReason] != tc.reason || claim.Annotations[annMessage] != tc.message {
				t.Fatalf("the claim records %v", claim.Annotations)
			}
			again, err := h.InspectVolume(t.Context(), id)
			if err != nil || again.Phase != tc.phase || again.Reason != tc.reason {
				t.Fatalf("a second read = %+v %v, want the recorded verdict", again, err)
			}
		})
	}
}

// TestFillMessageIsBounded: a failure's sentence is cut to what an
// annotation is given.
func TestFillMessageIsBounded(t *testing.T) {
	state := fillFailed(driver.ReasonSourceUnreachable, strings.Repeat("x", 3*maxFillMessage))
	if len(state.Message) != maxFillMessage {
		t.Fatalf("a message of %d bytes was kept", len(state.Message))
	}
}

// TestCreateVolumeIsIdempotent: a second create of one volume is
// ErrAlreadyExists and changes nothing; one whose fill Pod is missing while
// it is Pending makes the Pod again; one that is settled makes none.
func TestCreateVolumeIsIdempotent(t *testing.T) {
	h := volumeHarness(t)
	const id = "vol_again"
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("second CreateVolume = %v, want ErrAlreadyExists", err)
	}
	if err := h.cs.CoreV1().Pods(namespace).Delete(t.Context(), fillPodName(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if state, err := h.InspectVolume(t.Context(), id); err != nil || state.Phase != driver.VolumePending {
		t.Fatalf("a Pending volume with no fill pod reads %+v %v, want Pending", state, err)
	}
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("third CreateVolume = %v, want ErrAlreadyExists", err)
	}
	if h.fillPodOf(t, id) == nil {
		t.Fatal("the missing fill pod was not made again")
	}
	h.setFillStatus(t, id, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	if state, err := h.InspectVolume(t.Context(), id); err != nil || state.Phase != driver.VolumeAvailable {
		t.Fatalf("InspectVolume = %+v %v", state, err)
	}
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("CreateVolume of a filled volume = %v, want ErrAlreadyExists", err)
	}
	if h.fillPodOf(t, id) != nil {
		t.Fatal("a filled volume was filled again")
	}
}

// TestCreateVolumeRefuses: an image source with no fill image is
// ErrUnsupported, and a spec no driver can act on is ErrInvalid or
// ErrUnsupported with nothing made.
func TestCreateVolumeRefuses(t *testing.T) {
	plain := newHarness(t)
	if _, err := plain.CreateVolume(t.Context(), imageVolume("vol_noimage")); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("an image volume without a fill image = %v, want ErrUnsupported", err)
	}
	if plain.Capabilities().VolumeImages {
		t.Fatal("a driver with no fill image declares VolumeImages")
	}
	if !volumeHarness(t).Capabilities().VolumeImages {
		t.Fatal("a driver with a fill image does not declare VolumeImages")
	}
	h := volumeHarness(t)
	for name, tc := range map[string]struct {
		mutate func(*driver.VolumeSpec)
		want   error
	}{
		"bad id":       {func(s *driver.VolumeSpec) { s.ID = "not a legal id" }, driver.ErrInvalid},
		"bad size":     {func(s *driver.VolumeSpec) { s.Size = "lots" }, driver.ErrInvalid},
		"zero size":    {func(s *driver.VolumeSpec) { s.Size = "0" }, driver.ErrInvalid},
		"unpinned":     {func(s *driver.VolumeSpec) { s.Source.Image = "registry.example.com/grader:1" }, driver.ErrInvalid},
		"bad access":   {func(s *driver.VolumeSpec) { s.Access = "shared-write" }, driver.ErrInvalid},
		"snapshot":     {func(s *driver.VolumeSpec) { s.Source = driver.VolumeSource{Kind: "snapshot"} }, driver.ErrUnsupported},
		"missing size": {func(s *driver.VolumeSpec) { s.Size = "" }, driver.ErrInvalid},
	} {
		spec := imageVolume("vol_refused")
		tc.mutate(&spec)
		if _, err := h.CreateVolume(t.Context(), spec); !errors.Is(err, tc.want) {
			t.Errorf("%s: CreateVolume = %v, want %v", name, err, tc.want)
		}
	}
	if claims, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).List(t.Context(), metav1.ListOptions{}); err != nil || len(claims.Items) != 0 {
		t.Fatalf("a refused create left %d claims (%v)", len(claims.Items), err)
	}
}

// TestDeleteVolume: a delete removes the fill Pod and the claim, a second is
// nil, an illegal id is nil, and a claim of the volume's name that is not a
// volume's is never read as one.
func TestDeleteVolume(t *testing.T) {
	h := volumeHarness(t)
	const id = "vol_gone"
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteVolume(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if h.fillPodOf(t, id) != nil {
		t.Fatal("the fill pod survived the delete")
	}
	if _, err := h.InspectVolume(t.Context(), id); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("InspectVolume after DeleteVolume = %v, want ErrNotFound", err)
	}
	if err := h.DeleteVolume(t.Context(), id); err != nil {
		t.Fatalf("second DeleteVolume = %v", err)
	}
	if err := h.DeleteVolume(t.Context(), "not a legal id"); err != nil {
		t.Fatalf("DeleteVolume of an illegal id = %v", err)
	}
	if _, err := h.InspectVolume(t.Context(), "not a legal id"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("InspectVolume of an illegal id = %v", err)
	}
	stranger := &corev1.PersistentVolumeClaim{Name: volumeClaimName("vol_stranger"), Namespace: namespace}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Create(t.Context(), stranger, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.InspectVolume(t.Context(), "vol_stranger"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a claim no volume made reads %v, want ErrNotFound", err)
	}
}

// TestVolumeAndSandboxNamesNeverMeet: a sandbox's objects are named by
// objectName, which never holds a dot, and a volume's always do, so no two
// ids name one object.
func TestVolumeAndSandboxNamesNeverMeet(t *testing.T) {
	for _, id := range []string{"sbx_01J9", "vol_01J9", "volume.vol-01j9", "fill.vol-01j9", "a.b", "A_B.c"} {
		if strings.Contains(objectName(id), ".") {
			t.Errorf("objectName(%q) = %q holds a dot", id, objectName(id))
		}
		if !strings.Contains(volumeClaimName(id), ".") || !strings.Contains(fillPodName(id), ".") {
			t.Errorf("a volume object of %q is named without a dot", id)
		}
	}
}

// TestPreflightChecksTheVolumeClass: a volume class the cluster does not
// have is named at start.
func TestPreflightChecksTheVolumeClass(t *testing.T) {
	h := newHarnessWith(t, func(o *Options) { o.VolumeClass = "shared" })
	if err := h.Preflight(t.Context()); err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("Preflight = %v, want the missing volume class named", err)
	}
	if got := (Options{StorageClass: "workspace"}).withDefaults().VolumeClass; got != "workspace" {
		t.Fatalf("the volume class defaults to %q, want the workspace class", got)
	}
}

// availableVolumes makes the two volumes mountedSpec mounts, an empty one
// and one filled from an image, and settles the second's fill.
func (h *harness) availableVolumes(t *testing.T) {
	t.Helper()
	if _, err := h.CreateVolume(t.Context(), emptyVolume("vol_state")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateVolume(t.Context(), imageVolume("vol_grader")); err != nil {
		t.Fatal(err)
	}
	h.setFillStatus(t, "vol_grader", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	if state, err := h.InspectVolume(t.Context(), "vol_grader"); err != nil || state.Phase != driver.VolumeAvailable {
		t.Fatalf("the image volume reads %+v %v", state, err)
	}
}

// mountedSpec is a sandbox that mounts one volume read-write and one
// read-only.
func mountedSpec(id string) driver.CreateSpec {
	s := spec(id)
	s.Volumes = []driver.Mount{
		{Name: "state", VolumeID: "vol_state", Path: "/data"},
		{Name: "grader", VolumeID: "vol_grader", Path: "/examen/check-images/abc", ReadOnly: true},
	}
	return s
}

// TestSandboxMountsItsVolumes: each mount is a pod volume naming the
// volume's claim and a mount of the workload container, read-only on both
// where the mount is; a start renders them again from the claim's record;
// and the helper of a file transfer carries none of them.
func TestSandboxMountsItsVolumes(t *testing.T) {
	h := volumeHarness(t)
	const id = "sbx_mounts"
	h.availableVolumes(t)
	h.created(t, mountedSpec(id))
	check := func(pod *corev1.Pod) {
		t.Helper()
		claims := map[string]corev1.PersistentVolumeClaimVolumeSource{}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				claims[v.Name] = *v.PersistentVolumeClaim
			}
		}
		mounts := map[string]corev1.VolumeMount{}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			mounts[m.MountPath] = m
		}
		for _, want := range mountedSpec(id).Volumes {
			mount, ok := mounts[want.Path]
			if !ok {
				t.Fatalf("no mount at %s: %+v", want.Path, pod.Spec.Containers[0].VolumeMounts)
			}
			source, ok := claims[mount.Name]
			if !ok || source.ClaimName != volumeClaimName(want.VolumeID) {
				t.Fatalf("the mount at %s names %+v, want the claim of %s", want.Path, source, want.VolumeID)
			}
			if mount.ReadOnly != want.ReadOnly || source.ReadOnly != want.ReadOnly {
				t.Fatalf("the mount at %s is read-only %v on the mount and %v on the source, want %v",
					want.Path, mount.ReadOnly, source.ReadOnly, want.ReadOnly)
			}
		}
	}
	check(h.podOf(t, id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	check(h.podOf(t, id))
	helper, err := h.helperPod(id, mountedSpec(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range helper.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != objectName(id) {
			t.Fatalf("the transfer helper mounts the volume claim %s", v.PersistentVolumeClaim.ClaimName)
		}
	}
}

// TestMountPathsAreChecked: a mount the Pod could not carry is ErrInvalid
// before any object is made.
func TestMountPathsAreChecked(t *testing.T) {
	for name, mounts := range map[string][]driver.Mount{
		"relative":             {{Name: "a", VolumeID: "vol_a", Path: "data"}},
		"root":                 {{Name: "a", VolumeID: "vol_a", Path: "/"}},
		"unclean":              {{Name: "a", VolumeID: "vol_a", Path: "/data/../etc"}},
		"inside the workspace": {{Name: "a", VolumeID: "vol_a", Path: "/workspace/data"}},
		"above the workspace":  {{Name: "a", VolumeID: "vol_a", Path: "/workspace"}},
		"the control plane's":  {{Name: "a", VolumeID: "vol_a", Path: "/run/cella/x"}},
		"scratch":              {{Name: "a", VolumeID: "vol_a", Path: "/tmp"}},
		"nested": {
			{Name: "a", VolumeID: "vol_a", Path: "/data"},
			{Name: "b", VolumeID: "vol_b", Path: "/data/more"},
		},
		"no volume id": {{Name: "a", Path: "/data"}},
		"no name":      {{VolumeID: "vol_a", Path: "/data"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := volumeHarness(t)
			s := spec("sbx_badmount")
			s.Volumes = mounts
			if _, err := h.Create(t.Context(), s); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("Create = %v, want ErrInvalid", err)
			}
			if claims, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).List(t.Context(), metav1.ListOptions{}); err != nil || len(claims.Items) != 0 {
				t.Fatalf("a refused create left %d claims (%v)", len(claims.Items), err)
			}
		})
	}
	// A path beside the workspace that shares its prefix is no nesting.
	h := volumeHarness(t)
	if _, err := h.CreateVolume(t.Context(), emptyVolume("vol_a")); err != nil {
		t.Fatal(err)
	}
	s := spec("sbx_besides")
	s.Volumes = []driver.Mount{{Name: "a", VolumeID: "vol_a", Path: "/workspaces"}}
	if _, err := h.Create(t.Context(), s); err != nil {
		t.Fatalf("a mount beside the workspace = %v", err)
	}
}

// failing makes every call of one verb on one resource fail, as a cluster
// that refuses or cannot answer does.
func (h *harness) failing(verb, resource string) {
	h.cs.PrependReactor(verb, resource, func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the cluster refused the " + verb)
	})
}

// TestVolumeCallsReportTheCluster: a claim or a fill Pod the cluster does
// not make, a fill Pod it does not read, a verdict it does not record and a
// delete it does not take are each an error naming the step, and a claim it
// does not make leaves nothing.
func TestVolumeCallsReportTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name, verb, resource string
		act                  func(*harness) error
		want                 string
	}{
		{"claim create", "create", "persistentvolumeclaims", func(h *harness) error {
			_, err := h.CreateVolume(t.Context(), imageVolume("vol_fail"))
			return err
		}, "volume claim create"},
		{"fill pod create", "create", "pods", func(h *harness) error {
			_, err := h.CreateVolume(t.Context(), imageVolume("vol_fail"))
			return err
		}, "fill pod create"},
		{"fill pod get", "get", "pods", func(h *harness) error {
			if _, err := h.CreateVolume(t.Context(), imageVolume("vol_fail")); err != nil {
				return err
			}
			h.failing("get", "pods")
			_, err := h.InspectVolume(t.Context(), "vol_fail")
			return err
		}, "fill pod get"},
		{"verdict patch", "patch", "persistentvolumeclaims", func(h *harness) error {
			if _, err := h.CreateVolume(t.Context(), imageVolume("vol_fail")); err != nil {
				return err
			}
			h.setFillStatus(t, "vol_fail", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
			h.failing("patch", "persistentvolumeclaims")
			_, err := h.InspectVolume(t.Context(), "vol_fail")
			return err
		}, "volume claim patch"},
		{"fill pod removal", "delete", "pods", func(h *harness) error {
			if _, err := h.CreateVolume(t.Context(), imageVolume("vol_fail")); err != nil {
				return err
			}
			h.setFillStatus(t, "vol_fail", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
			h.failing("delete", "pods")
			_, err := h.InspectVolume(t.Context(), "vol_fail")
			return err
		}, "fill pod delete"},
		{"delete of the pod", "delete", "pods", func(h *harness) error {
			h.failing("delete", "pods")
			return h.DeleteVolume(t.Context(), "vol_fail")
		}, "fill pod delete"},
		{"delete of the claim", "delete", "persistentvolumeclaims", func(h *harness) error {
			h.failing("delete", "persistentvolumeclaims")
			return h.DeleteVolume(t.Context(), "vol_fail")
		}, "volume claim delete"},
		{"claim get", "get", "persistentvolumeclaims", func(h *harness) error {
			h.failing("get", "persistentvolumeclaims")
			_, err := h.InspectVolume(t.Context(), "vol_fail")
			return err
		}, "volume claim get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := volumeHarness(t)
			if tc.verb == "create" {
				h.failing(tc.verb, tc.resource)
			}
			err := tc.act(h)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	// A claim whose phase was never recorded reads Pending.
	state := volumeStateOf(&corev1.PersistentVolumeClaim{Labels: map[string]string{labelVolume: "vol_x"}})
	if state.Phase != driver.VolumePending || state.ID != "vol_x" {
		t.Fatalf("a claim with no phase reads %+v", state)
	}
}

// TestRecreatedFillReportsTheCluster: a Pending volume whose fill Pod the
// cluster will not make again is an error, not ErrAlreadyExists, so the loop
// tries again.
func TestRecreatedFillReportsTheCluster(t *testing.T) {
	h := volumeHarness(t)
	const id = "vol_refill"
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); err != nil {
		t.Fatal(err)
	}
	if err := h.cs.CoreV1().Pods(namespace).Delete(t.Context(), fillPodName(id), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.failing("create", "pods")
	if _, err := h.CreateVolume(t.Context(), imageVolume(id)); err == nil || errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("CreateVolume = %v, want the refused fill pod", err)
	}
	h2 := volumeHarness(t)
	h2.failing("get", "persistentvolumeclaims")
	if _, err := h2.CreateVolume(t.Context(), imageVolume(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.CreateVolume(t.Context(), imageVolume(id)); err == nil || errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("CreateVolume over a claim that cannot be read = %v, want the read's error", err)
	}
}

// TestAttachNeedsAnAvailableVolume: a sandbox that mounts a volume this
// driver does not hold, or one still being filled, is refused with
// ErrInvalid before any of its objects exist; and a volume whose claim is
// being deleted reads as gone and takes no mount.
func TestAttachNeedsAnAvailableVolume(t *testing.T) {
	h := volumeHarness(t)
	if _, err := h.CreateVolume(t.Context(), imageVolume("vol_filling")); err != nil {
		t.Fatal(err)
	}
	for _, volume := range []string{"vol_absent", "vol_filling"} {
		s := spec("sbx_attach")
		s.Volumes = []driver.Mount{{Name: "data", VolumeID: volume, Path: "/data"}}
		if _, err := h.Create(t.Context(), s); !errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), volume) {
			t.Fatalf("a mount of %s = %v, want ErrInvalid naming it", volume, err)
		}
		if _, err := h.Inspect(t.Context(), "sbx_attach"); !errors.Is(err, driver.ErrNotFound) {
			t.Fatalf("Inspect after a refused attach = %v, want ErrNotFound", err)
		}
		if _, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), objectName("sbx_attach"), metav1.GetOptions{}); err == nil {
			t.Fatal("a refused attach left a pod")
		}
	}
	if _, err := h.CreateVolume(t.Context(), emptyVolume("vol_going")); err != nil {
		t.Fatal(err)
	}
	claim := h.volumeClaim(t, "vol_going")
	now := metav1.NewTime(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	claim.DeletionTimestamp = &now
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Update(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.InspectVolume(t.Context(), "vol_going"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a claim being deleted reads %v, want ErrNotFound", err)
	}
	if _, err := h.CreateVolume(t.Context(), emptyVolume("vol_going")); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("a create over a claim being deleted = %v, want ErrAlreadyExists", err)
	}
	s := spec("sbx_attach")
	s.Volumes = []driver.Mount{{Name: "data", VolumeID: "vol_going", Path: "/data"}}
	if _, err := h.Create(t.Context(), s); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a mount of a volume being deleted = %v, want ErrInvalid", err)
	}
	h.failing("get", "persistentvolumeclaims")
	s.Volumes[0].VolumeID = "vol_filling"
	if _, err := h.Create(t.Context(), s); err == nil || errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a mount whose claim cannot be read = %v, want the read's error", err)
	}
}
