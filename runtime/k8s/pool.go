// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	driver "latere.ai/x/cella/runtime"
)

// adopt turns one prewarmed entry into the caller's sandbox, which is the pool
// of spec 020 as this driver provides it.
//
// The claim's pool label is the cluster's mutex. One guarded patch tests the
// label, the resource version and the spec the mutation was built from, and
// removes the label in the same act, so of two adopters that both read the
// entry exactly one patch applies and the other is told the entry is gone.
// Nothing else needs a guard: once the label is off, no second adopter can
// reach this sandbox.
//
// The claim is written before the identity is projected. A loser must never
// have written its caller's token into a Secret the winner's Pod mounts, and a
// projection that fails after the claim deletes the sandbox rather than
// leaving half of one caller's identity in the pool.
func (d *Driver) adopt(ctx context.Context, id string, a driver.Adoption) error {
	if a.Lifecycle.TTL < 0 || a.Lifecycle.AutoStop < 0 || a.Lifecycle.AutoDelete < 0 {
		return fmt.Errorf("%w: a negative lifecycle duration", driver.ErrInvalid)
	}
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return err
	}
	if pvc.Labels[labelPool] != "true" {
		return driver.ErrNotFound
	}
	held, err := specOf(pvc)
	if err != nil {
		return err
	}
	if a.Workspace.Path != "" && a.Workspace.Path != workspacePath(held) {
		return fmt.Errorf("%w: the entry's workspace is at %s, not %s", driver.ErrInvalid, workspacePath(held), a.Workspace.Path)
	}
	adopted := adoptedSpec(held, a)
	patch, err := adoptionPatch(pvc, adopted, d.opts.Now().UTC())
	if err != nil {
		return err
	}
	if _, err = d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).
		Patch(ctx, pvc.Name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return d.classifyClaim(ctx, id, err)
	}
	if err := d.projectAdopted(ctx, id, adopted, a); err != nil {
		return errors.Join(err, d.discard(ctx, id))
	}
	return nil
}

// discard removes an entry this driver claimed and could not finish adopting.
// The Pod and the Secret are removed without reporting: the caller already
// holds the error that matters, and what decides whether a caller is left with
// nothing is the claim, which is the sandbox. The context's cancellation is
// dropped, because the projection most often failed with the call that carried
// it.
func (d *Driver) discard(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
	_ = d.deletePod(ctx, id)
	_ = d.deleteToken(ctx, id)
	err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Delete(ctx, objectName(id), *d.deleteOptions())
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("discarding the entry %s: %w", id, err)
	}
	return nil
}

// adoptedSpec is the entry's stored spec with the caller's half written over
// it. It is what a later Start renders the Pod from, so the entry's shape
// survives and nothing of the pool does: an adopted sandbox that is stopped
// and started again comes back as the caller's, not as an entry.
func adoptedSpec(held driver.CreateSpec, a driver.Adoption) driver.CreateSpec {
	out := held
	out.Prewarm = false
	out.Owner, out.Name = a.Owner, a.Name
	out.Labels = maps.Clone(a.Labels)
	out.Env = maps.Clone(a.Env)
	out.Lifecycle = a.Lifecycle
	out.Egress = a.Egress
	if a.Workspace.Path != "" {
		out.Workspace = a.Workspace
	}
	return out
}

// adoptionPatch is the guarded patch: three test operations, the record the
// adoption writes, and the removal of the pool label. The stamps are written
// after the caller's own annotations for the same reason the create's are: the
// control plane's record of when a sandbox began is not a caller's to set.
func adoptionPatch(pvc *corev1.PersistentVolumeClaim, adopted driver.CreateSpec, now time.Time) ([]byte, error) {
	encoded, err := json.Marshal(adopted)
	if err != nil {
		return nil, err
	}
	ops := []patchOp{{Op: "test", Path: labelPath(labelPool), Value: "true"}}
	if pvc.ResourceVersion != "" {
		ops = append(ops, patchOp{Op: "test", Path: "/metadata/resourceVersion", Value: pvc.ResourceVersion})
	}
	if current, ok := pvc.Annotations[annSpec]; ok {
		ops = append(ops, patchOp{Op: "test", Path: annPath(annSpec), Value: current})
	}
	set := map[string]string{
		annSpec:      string(encoded),
		annOwner:     adopted.Owner,
		annToken:     "true",
		annCreatedAt: now.Format(stamp),
		// The two instants the deadline rules count from are the adoption's,
		// not the prewarm's: an entry that sat warm for hours is not expired
		// or idle on the first tick after a caller took it.
		annActivityAt: now.Format(stamp),
		annStartedAt:  now.Format(stamp),
	}
	maps.Copy(set, lifecycleAnnotations(adopted.Lifecycle, now))
	for _, key := range slices.Sorted(maps.Keys(set)) {
		ops = append(ops, patchOp{Op: "add", Path: annPath(key), Value: set[key]})
	}
	for _, key := range []string{annExpiresAt, annAutoStop, annAutoDelete} {
		if _, written := set[key]; written {
			continue
		}
		if _, ok := pvc.Annotations[key]; ok {
			ops = append(ops, patchOp{Op: "remove", Path: annPath(key)})
		}
	}
	ops = append(ops, adoptionLabelOps(pvc.Labels, adopted.Name)...)
	return json.Marshal(ops)
}

// adoptionLabelOps names the sandbox and takes it out of the pool. The name is
// a label because a cluster selects on it; a name no label may hold is left
// off, exactly as a create leaves it off.
func adoptionLabelOps(held map[string]string, name string) []patchOp {
	ops := []patchOp{}
	switch {
	case labelValue.MatchString(name):
		ops = append(ops, patchOp{Op: "add", Path: labelPath(labelName), Value: name})
	case held[labelName] != "":
		ops = append(ops, patchOp{Op: "remove", Path: labelPath(labelName)})
	}
	return append(ops, patchOp{Op: "remove", Path: labelPath(labelPool)})
}

// labelPath is the JSON pointer of one label key, escaped as the pointer
// grammar asks.
func labelPath(key string) string { return "/metadata/labels/" + jsonPointer(key) }

// projectAdopted writes what the adopted sandbox holds inside itself and
// stamps the Pod. The Secret is mounted by a projection the entry was rendered
// with, so the kubelet syncs the file into the running container; the Pod's
// labels follow the claim's so a cluster selecting on them sees one sandbox
// and not an entry beside it.
func (d *Driver) projectAdopted(ctx context.Context, id string, adopted driver.CreateSpec, a driver.Adoption) error {
	if len(a.Token) > 0 {
		if err := d.putToken(ctx, id, a.Token); err != nil {
			return fmt.Errorf("adopting %s: %w", id, err)
		}
	}
	labels := map[string]any{labelPool: nil}
	if labelValue.MatchString(adopted.Name) {
		labels[labelName] = adopted.Name
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	// No guard on the Pod: the claim's label is the mutex, so once this call
	// is reached no other adopter is patching this Pod.
	_, err = d.cs.CoreV1().Pods(d.opts.Namespace).
		Patch(ctx, objectName(id), types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("adopting %s: pod patch: %w", id, err)
	}
	return nil
}

// classifyClaim reads the claim again to say what a refused patch means. A
// lost test operation reaches this driver as an invalid request from a cluster
// and as a plain patch error from a client double, so the answer is taken from
// the object rather than from the error: an entry that is no longer one was
// taken by another adopter, and anything else is the failure it says it is.
func (d *Driver) classifyClaim(ctx context.Context, id string, err error) error {
	pvc, readErr := d.getClaim(ctx, id)
	if errors.Is(readErr, driver.ErrNotFound) || (readErr == nil && pvc.Labels[labelPool] != "true") {
		return driver.ErrNotFound
	}
	return mapErr(err, "claim adoption patch")
}
