// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/k8s/fill"
)

// DefaultFillTimeout bounds one fill of a volume from an image: the pull of
// the image and the copy of its files together. A fill past it is Failed, so
// an image that can never be pulled ends a volume rather than holding it
// Pending forever.
const DefaultFillTimeout = 30 * time.Minute

// The labels and annotations of a volume's claim and of its fill Pod. A
// volume's objects carry no managed-by label, so no list of sandboxes ever
// reads one as a sandbox.
const (
	labelKind   = prefix + "kind"
	labelVolume = prefix + "volume"
	kindVolume  = "volume"
	kindFill    = "volume-fill"

	annVolumeName = prefix + "name"
	annSize       = prefix + "size"
	annAccess     = prefix + "access"
	annSource     = prefix + "source"
	annPhase      = prefix + "phase"
	annReason     = prefix + "reason"
	annMessage    = prefix + "message"
)

// The paths and names inside a fill Pod: where the init container leaves the
// program, where the volume's claim is mounted, and the program's own path.
const (
	fillDir       = "/cella-fill"
	fillVolumeDir = "/cella-volume"
	fillProgram   = fillDir + "/cellad"
	fillStage     = "stage"
	fillCopy      = "fill"
	fillHostname  = "cella-fill"
)

// maxFillMessage bounds the sentence a failed fill leaves on the claim, which
// is an annotation and is read back into the volume's status.
const maxFillMessage = 1024

// volumeClaimName is the claim of one volume. A sandbox's claim is named by
// objectName, which never holds a dot, so the dot here keeps a volume's claim
// apart from every sandbox's whatever the two ids are.
func volumeClaimName(id string) string { return "volume." + objectName(id) }

// fillPodName is the Pod that fills one volume, apart from every sandbox's
// Pod for the same reason.
func fillPodName(id string) string { return "fill." + objectName(id) }

// CreateVolume provisions one volume's claim and, for an image source, the
// Pod that fills it. A claim that exists is ErrAlreadyExists, after the fill
// Pod of a volume still Pending has been made again where a previous call
// made the claim and not the Pod, so the call is safe on every pass of the
// controller's volume loop.
//
// Every access mode is a ReadWriteOnce claim: the fill writes it, and a
// shared-read volume is mounted read-only by each sandbox. Whether such a
// volume is shared by sandboxes on several nodes is the storage class's
// property: a class whose volumes need no attaching to one node, NFS or
// CephFS, serves an RWO claim on many nodes, and one that attaches a volume
// to one node, block storage, serves the sandboxes of that node alone.
func (d *Driver) CreateVolume(ctx context.Context, spec driver.VolumeSpec) (driver.Ref, error) {
	if err := spec.Check(); err != nil {
		return driver.Ref{}, err
	}
	if !validID.MatchString(spec.ID) {
		return driver.Ref{}, fmt.Errorf("%w: volume id %q", driver.ErrInvalid, spec.ID)
	}
	image := spec.Source.Kind == driver.SourceImage
	if image && d.opts.FillImage == "" {
		return driver.Ref{}, fmt.Errorf("%w: no fill image is configured, so this driver fills no volume from an image", driver.ErrUnsupported)
	}
	size, err := resource.ParseQuantity(spec.Size)
	if err != nil || size.Sign() <= 0 {
		return driver.Ref{}, fmt.Errorf("%w: volume size %q", driver.ErrInvalid, spec.Size)
	}
	claims := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace)
	created, err := claims.Create(ctx, d.volumeClaim(spec, size), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, err := d.getVolumeClaim(ctx, spec.ID)
		if errors.Is(err, driver.ErrNotFound) {
			// The claim of this name is being deleted, or is no
			// volume's; either way the name is taken until it goes.
			return driver.Ref{}, driver.ErrAlreadyExists
		}
		if err != nil {
			return driver.Ref{}, err
		}
		if image && existing.Annotations[annPhase] == driver.VolumePending {
			if err := d.putFillPod(ctx, spec, size, existing); err != nil {
				return driver.Ref{}, err
			}
		}
		return driver.Ref{}, driver.ErrAlreadyExists
	}
	if err != nil {
		return driver.Ref{}, mapErr(err, "volume claim create")
	}
	// A fill Pod that cannot be made now is made by the next call, which
	// finds the claim Pending and no Pod, so the claim is kept.
	if image {
		if err := d.putFillPod(ctx, spec, size, created); err != nil {
			return driver.Ref{}, err
		}
	}
	return driver.Ref{ID: spec.ID}, nil
}

// InspectVolume reads one volume. A volume filled from an image is Pending
// until its fill Pod ends, and the verdict is then written on the claim and
// the Pod removed, so the claim alone answers every later read.
func (d *Driver) InspectVolume(ctx context.Context, id string) (driver.VolumeState, error) {
	claim, err := d.getVolumeClaim(ctx, id)
	if err != nil {
		return driver.VolumeState{}, err
	}
	state := volumeStateOf(claim)
	if state.Phase != driver.VolumePending {
		return state, nil
	}
	pod, err := d.cs.CoreV1().Pods(d.opts.Namespace).Get(ctx, fillPodName(id), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// The claim was made and its Pod was not; the next CreateVolume
		// makes it.
		return state, nil
	case err != nil:
		return driver.VolumeState{}, fmt.Errorf("fill pod get: %w", err)
	}
	verdict := d.fillVerdict(pod)
	if verdict.Phase == driver.VolumePending {
		return state, nil
	}
	// The verdict is written before the Pod goes, so a delete that fails
	// leaves a claim that already answers and a Pod the next read removes.
	if err := d.settleVolume(ctx, claim.Name, verdict); err != nil {
		return driver.VolumeState{}, err
	}
	if err := d.cs.CoreV1().Pods(d.opts.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return driver.VolumeState{}, fmt.Errorf("fill pod delete: %w", err)
	}
	state.Phase, state.Reason, state.Message = verdict.Phase, verdict.Reason, verdict.Message
	return state, nil
}

// DeleteVolume removes one volume's fill Pod and its claim. Neither waits:
// the cluster keeps a claim a Pod still mounts until the Pod is gone, and a
// volume that is already gone is not an error.
func (d *Driver) DeleteVolume(ctx context.Context, id string) error {
	if !validID.MatchString(id) {
		return nil
	}
	if err := d.cs.CoreV1().Pods(d.opts.Namespace).Delete(ctx, fillPodName(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("fill pod delete: %w", err)
	}
	if err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Delete(ctx, volumeClaimName(id), *d.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("volume claim delete: %w", err)
	}
	return nil
}

// getVolumeClaim reads the claim of one volume, and ErrNotFound for an id
// that names none.
func (d *Driver) getVolumeClaim(ctx context.Context, id string) (*corev1.PersistentVolumeClaim, error) {
	if !validID.MatchString(id) {
		return nil, driver.ErrNotFound
	}
	claim, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Get(ctx, volumeClaimName(id), metav1.GetOptions{})
	if err != nil {
		return nil, mapErr(err, "volume claim get")
	}
	// A claim being deleted is a volume already gone: a read right after
	// DeleteVolume answers as the delete meant, while the cluster still
	// waits on the claim's protection to lift.
	if claim.Labels[labelKind] != kindVolume || claim.Labels[labelVolume] != id || claim.DeletionTimestamp != nil {
		return nil, driver.ErrNotFound
	}
	return claim, nil
}

// checkAttachable refuses a sandbox that mounts a volume this driver does not
// hold Available, before any of the sandbox's objects exist: a Pod naming a
// claim that is missing or still being filled would wait out the ready
// timeout and leave a rollback behind, where this answers at once and leaves
// nothing.
func (d *Driver) checkAttachable(ctx context.Context, mounts []driver.Mount) error {
	for _, m := range mounts {
		claim, err := d.getVolumeClaim(ctx, m.VolumeID)
		if errors.Is(err, driver.ErrNotFound) {
			return fmt.Errorf("%w: the mount %q names the volume %s, which this driver does not hold", driver.ErrInvalid, m.Name, m.VolumeID)
		}
		if err != nil {
			return err
		}
		if phase := volumeStateOf(claim).Phase; phase != driver.VolumeAvailable {
			return fmt.Errorf("%w: the mount %q names the volume %s, which is %s", driver.ErrInvalid, m.Name, m.VolumeID, phase)
		}
	}
	return nil
}

// volumeClaim renders one volume's claim. An empty volume is Available from
// the moment its claim exists: a class that binds on first use leaves the
// claim Pending until a Pod mounts it, and that is the first sandbox's.
func (d *Driver) volumeClaim(spec driver.VolumeSpec, size resource.Quantity) *corev1.PersistentVolumeClaim {
	phase := driver.VolumeAvailable
	if spec.Source.Kind == driver.SourceImage {
		phase = driver.VolumePending
	}
	claim := &corev1.PersistentVolumeClaim{
		Name: volumeClaimName(spec.ID), Namespace: d.opts.Namespace,
		Labels: map[string]string{labelKind: kindVolume, labelVolume: spec.ID},
		Annotations: map[string]string{
			annOwner: spec.Owner, annVolumeName: spec.Name, annSize: spec.Size,
			annAccess: spec.Access, annSource: spec.Source.Kind, annImage: spec.Source.Image,
			annCreatedAt: d.opts.Now().UTC().Format(stamp), annPhase: phase,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if class := d.opts.VolumeClass; class != "" {
		claim.Spec.StorageClassName = &class
	}
	return claim
}

// putFillPod makes one volume's fill Pod. One that exists is left as it is:
// it is this volume's fill, already running or already ended, and the next
// read takes its verdict.
func (d *Driver) putFillPod(ctx context.Context, spec driver.VolumeSpec, size resource.Quantity, claim *corev1.PersistentVolumeClaim) error {
	pod, err := d.fillPod(spec, size, claim)
	if err != nil {
		return err
	}
	if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("fill pod create: %w", err)
	}
	return nil
}

// fillPod renders the Pod that fills one volume from its image. The init
// container runs the fill image, whose entrypoint is cellad, and leaves the
// program in a directory the two containers share; the main container runs
// the source image with that program as its command, so the image's own
// entrypoint never runs and an image with no shell is filled as well as one
// with one. The Pod carries the sandboxes' baseline, their node placement
// and pull secrets, no service account token and no sandbox label: no
// sandbox's network rule selects it, and it needs no network, since the
// kubelet pulls both images.
func (d *Driver) fillPod(spec driver.VolumeSpec, size resource.Quantity, claim *corev1.PersistentVolumeClaim) (*corev1.Pod, error) {
	limits, err := d.resources(driver.Resources{})
	if err != nil {
		return nil, err
	}
	deadline := int64(d.opts.FillTimeout / time.Second)
	pod := &corev1.Pod{
		Name: fillPodName(spec.ID), Namespace: d.opts.Namespace,
		Labels:      map[string]string{labelKind: kindFill, labelVolume: spec.ID},
		Annotations: map[string]string{annOwner: spec.Owner, annImage: spec.Source.Image},
		Spec: corev1.PodSpec{
			Hostname:                      fillHostname,
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			AutomountServiceAccountToken:  ptr(false),
			EnableServiceLinks:            ptr(false),
			ShareProcessNamespace:         ptr(false),
			TerminationGracePeriodSeconds: ptr(int64(d.opts.GracePeriod / time.Second)),
			NodeSelector:                  maps.Clone(d.opts.NodeSelector),
			Tolerations:                   tolerations(d.opts.Tolerations),
			ImagePullSecrets:              pullSecrets(d.opts.ImagePullSecrets),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr(true),
				RunAsUser:    ptr(d.opts.RunAsUser),
				RunAsGroup:   ptr(d.opts.RunAsGroup),
				// The group owns the claim, so the copy, which runs as
				// the baseline's user, writes it.
				FSGroup:        ptr(d.opts.RunAsGroup),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{{
				Name:            fillStage,
				Image:           d.opts.FillImage,
				Args:            []string{"fill", fill.ModeStage, fillProgram},
				Resources:       limits,
				VolumeMounts:    []corev1.VolumeMount{{Name: fillStage, MountPath: fillDir}},
				SecurityContext: hardened(),
			}},
			Containers: []corev1.Container{{
				Name:  fillCopy,
				Image: spec.Source.Image,
				Command: []string{fillProgram, "fill", fill.ModeCopy,
					"--" + fill.FlagMaxBytes, strconv.FormatInt(size.Value(), 10), "/", fillVolumeDir},
				Resources: limits,
				VolumeMounts: []corev1.VolumeMount{
					{Name: fillStage, MountPath: fillDir, ReadOnly: true},
					{Name: kindVolume, MountPath: fillVolumeDir},
				},
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				SecurityContext:          hardened(),
			}},
			Volumes: []corev1.Volume{
				{Name: fillStage, EmptyDir: &corev1.EmptyDirVolumeSource{}},
				{Name: kindVolume,
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}},
			},
		},
	}
	// The claim owns its fill, so a claim removed by hand takes its Pod
	// with it. A claim the API server has not given a uid yet, which only
	// a test double returns, owns nothing.
	if claim.UID != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: claim.Name, UID: claim.UID,
		}}
	}
	return pod, nil
}

// fillVerdict reads what a fill Pod says about its volume: Pending while it
// runs, Available once it succeeded, and Failed with the reason the program
// wrote where it failed, where an image could not be pulled, and where the
// fill outlived its deadline.
func (d *Driver) fillVerdict(pod *corev1.Pod) driver.VolumeState {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return driver.VolumeState{Phase: driver.VolumeAvailable}
	case corev1.PodFailed:
		if pod.Status.Reason == "DeadlineExceeded" {
			return fillFailed(driver.ReasonSourceUnreachable, d.overdue())
		}
		return fillFailure(pod)
	}
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if waiting := status.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
				return fillFailed(driver.ReasonSourceUnreachable, "the image of the "+status.Name+" container could not be pulled: "+waiting.Message)
			}
		}
	}
	if !pod.CreationTimestamp.IsZero() && d.opts.Now().Sub(pod.CreationTimestamp.Time) > d.opts.FillTimeout {
		return fillFailed(driver.ReasonSourceUnreachable, d.overdue())
	}
	return driver.VolumeState{Phase: driver.VolumePending}
}

// overdue is the sentence of a fill that outlived its deadline.
func (d *Driver) overdue() string {
	return "the fill did not finish within " + d.opts.FillTimeout.String()
}

// fillFailure is the verdict of a fill Pod that failed: the reason the
// program named at the head of its termination message, the reason a
// container that could not be started has, and SourceUnreachable for any
// other end, which is a source this fill could not take in.
func fillFailure(pod *corev1.Pod) driver.VolumeState {
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		ended := status.State.Terminated
		if ended == nil || ended.ExitCode == 0 {
			continue
		}
		message := strings.TrimSpace(ended.Message)
		for _, reason := range []string{driver.ReasonSourceTooLarge, driver.ReasonSourceUnsupported, driver.ReasonSourceUnreachable} {
			if rest, ok := strings.CutPrefix(message, reason+":"); ok {
				return fillFailed(reason, strings.TrimSpace(rest))
			}
		}
		if message == "" {
			message = fmt.Sprintf("the %s container exited %d (%s)", status.Name, ended.ExitCode, ended.Reason)
		}
		switch ended.Reason {
		case "StartError", "ContainerCannotRun":
			return fillFailed(driver.ReasonSourceUnsupported, message)
		}
		return fillFailed(driver.ReasonSourceUnreachable, message)
	}
	message := strings.TrimSpace(pod.Status.Message)
	if message == "" {
		message = "the fill Pod failed"
	}
	return fillFailed(driver.ReasonSourceUnreachable, message)
}

// fillFailed is a Failed verdict with its reason and a sentence bounded to
// what an annotation carries.
func fillFailed(reason, message string) driver.VolumeState {
	if len(message) > maxFillMessage {
		message = message[:maxFillMessage]
	}
	return driver.VolumeState{Phase: driver.VolumeFailed, Reason: reason, Message: message}
}

// settleVolume writes a fill's verdict on the volume's claim.
func (d *Driver) settleVolume(ctx context.Context, claim string, verdict driver.VolumeState) error {
	annotations := map[string]any{annPhase: verdict.Phase, annReason: nil, annMessage: nil}
	if verdict.Reason != "" {
		annotations[annReason] = verdict.Reason
	}
	if verdict.Message != "" {
		annotations[annMessage] = verdict.Message
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	_, err = d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Patch(ctx, claim, types.MergePatchType, patch, metav1.PatchOptions{})
	return mapErr(err, "volume claim patch")
}

// volumeStateOf is a volume as its claim records it: the phase, the reason
// and sentence of a failed fill, and the capacity the cluster granted once
// the claim is bound, or the size asked for before.
func volumeStateOf(claim *corev1.PersistentVolumeClaim) driver.VolumeState {
	state := driver.VolumeState{
		ID:       claim.Labels[labelVolume],
		Phase:    claim.Annotations[annPhase],
		Reason:   claim.Annotations[annReason],
		Message:  claim.Annotations[annMessage],
		Capacity: claim.Annotations[annSize],
	}
	if state.Phase == "" {
		state.Phase = driver.VolumePending
	}
	if granted, ok := claim.Status.Capacity[corev1.ResourceStorage]; ok && claim.Status.Phase == corev1.ClaimBound {
		state.Capacity = granted.String()
	}
	return state
}

// mountVolumes adds each volume a sandbox mounts to its Pod: a pod volume
// naming the volume's claim and a mount in the workload container, both
// read-only where the mount is.
func mountVolumes(pod *corev1.Pod, mounts []driver.Mount) {
	container := &pod.Spec.Containers[0]
	for i, m := range mounts {
		name := "volume-" + strconv.Itoa(i)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: volumeClaimName(m.VolumeID), ReadOnly: m.ReadOnly}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: name, MountPath: m.Path, ReadOnly: m.ReadOnly})
	}
}

// checkMounts refuses a mount the Pod could not carry: an id no claim is
// named from, a path that is not clean and absolute, and a path that equals
// or nests with the workspace, another mount, the control plane's own
// directory, or the scratch directory every sandbox mounts.
func checkMounts(mounts []driver.Mount, workspace string) error {
	for i, m := range mounts {
		if m.Name == "" || !validID.MatchString(m.VolumeID) {
			return fmt.Errorf("%w: mount %q of volume %q", driver.ErrInvalid, m.Name, m.VolumeID)
		}
		if err := checkPath(m.Path); err != nil {
			return err
		}
		for _, taken := range []string{workspace, path.Dir(driver.TokenPath)} {
			if under(m.Path, taken) || under(taken, m.Path) {
				return fmt.Errorf("%w: the mount %q at %s nests with %s", driver.ErrInvalid, m.Name, m.Path, taken)
			}
		}
		if m.Path == "/tmp" {
			return fmt.Errorf("%w: the mount %q is at /tmp, which every sandbox mounts", driver.ErrInvalid, m.Name)
		}
		for _, other := range mounts[:i] {
			if under(m.Path, other.Path) || under(other.Path, m.Path) {
				return fmt.Errorf("%w: the mounts %q and %q nest", driver.ErrInvalid, other.Name, m.Name)
			}
		}
	}
	return nil
}

var _ driver.VolumeDriver = (*Driver)(nil)
