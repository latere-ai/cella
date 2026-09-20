// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	driver "latere.ai/x/cella/runtime"
)

// pollInterval is how often a wait re-reads an object it is waiting on.
const pollInterval = 250 * time.Millisecond

// patchAttempts bounds how often a lost compare-and-swap is rebuilt.
const patchAttempts = 5

// Create provisions the claim, then the Pod, and waits for the Pod to start.
// An existing claim is ErrAlreadyExists and nothing is touched; a failure
// after this call created the claim removes what this call created, so a
// half-built sandbox never survives.
func (d *Driver) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if err := d.validate(s); err != nil {
		return driver.Ref{}, err
	}
	if err := s.CheckPrewarm(); err != nil {
		return driver.Ref{}, err
	}
	now := d.opts.Now().UTC()
	pvc, err := d.claim(s, now)
	if err != nil {
		return driver.Ref{}, err
	}
	// A prewarmed entry carries the projection with no Secret behind it.
	// The kubelet cannot add a volume to a running Pod, so an entry that
	// was rendered without the mount could never be handed the identity an
	// adoption mints; the source is optional, so the Pod starts either way.
	token := len(s.Token) > 0 || s.Prewarm
	pod, err := d.pod(s, now, token)
	if err != nil {
		return driver.Ref{}, err
	}
	if token {
		pvc.Annotations[annToken] = "true"
	}
	// The mesh's policy and Service are in the cluster before the Pod that
	// belongs to them, so a member is reachable by its peers and by nothing
	// else from the moment it starts (spec 022).
	if err := d.joinMesh(ctx, s.Mesh.ID); err != nil {
		return driver.Ref{}, err
	}
	if _, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return driver.Ref{}, mapErr(err, "claim create")
	}
	// Every rollback runs after the call has already failed, often because
	// the caller hung up, so it drops the cancellation it inherited: a
	// cancelled context makes each cleanup a no-op and leaks both objects.
	rollback := func() { _ = d.remove(context.WithoutCancel(ctx), s.ID) }
	// The identity is in the cluster before the Pod that mounts it, so the
	// workload's first read finds the token rather than an empty directory
	// the kubelet fills a moment later (spec 006).
	if token {
		if err := d.putToken(ctx, s.ID, s.Token); err != nil {
			rollback()
			return driver.Ref{}, err
		}
	}
	if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		rollback()
		return driver.Ref{}, mapErr(err, "pod create")
	}
	if err := d.waitReady(ctx, s.ID); err != nil {
		rollback()
		return driver.Ref{}, err
	}
	return driver.Ref{ID: s.ID}, nil
}

// Start attaches a Pod to an existing claim. A live Pod is a no-op; a Pod that
// has ended or is ending is removed first, so a start always faces a clean
// claim.
func (d *Driver) Start(ctx context.Context, id string) error {
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return err
	}
	if pod != nil {
		if _, _, ended := terminated(pod); !ended && pod.DeletionTimestamp == nil &&
			pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			return d.waitReady(ctx, id)
		}
		if err := d.deletePod(ctx, id); err != nil {
			return err
		}
	}
	spec, err := specOf(pvc)
	if err != nil {
		return err
	}
	started := d.opts.Now().UTC()
	fresh, err := d.pod(spec, started, pvc.Annotations[annToken] == "true")
	if err != nil {
		return err
	}
	if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Create(ctx, fresh, metav1.CreateOptions{}); err != nil {
		return mapErr(err, "pod create")
	}
	if err := d.waitReady(ctx, id); err != nil {
		_ = d.deletePod(context.WithoutCancel(ctx), id)
		return err
	}
	// The leg's start is stamped once the Pod runs, so a start that failed
	// leaves the record of the stop it did not replace.
	return d.apply(ctx, id, func(*corev1.PersistentVolumeClaim) (change, error) {
		return change{set: map[string]string{annStartedAt: started.Format(stamp), annActivityAt: started.Format(stamp)}, remove: []string{annStoppedAt}}, nil
	})
}

// Stop deletes the Pod and keeps the claim. The stop is stamped once: a second
// stop of a stopped sandbox reads the same instant back.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if _, err := d.getClaim(ctx, id); err != nil {
		return err
	}
	if err := d.deletePod(ctx, id); err != nil {
		return err
	}
	return d.apply(ctx, id, func(pvc *corev1.PersistentVolumeClaim) (change, error) {
		if pvc.Annotations[annStoppedAt] != "" {
			return change{}, nil
		}
		return change{set: map[string]string{annStoppedAt: d.opts.Now().UTC().Format(stamp)}}, nil
	})
}

// Delete removes both objects and waits for them to be gone, so the id is free
// for a caller that creates it again.
func (d *Driver) Delete(ctx context.Context, id string) error {
	if !validID.MatchString(id) {
		return nil
	}
	return d.remove(ctx, id)
}

// remove is Delete without the id check: the Pod, then the claim, each waited
// out. The order matters, since a claim in use by a Pod stays terminating
// until the Pod is gone.
func (d *Driver) remove(ctx context.Context, id string) error {
	// The membership is read before the claim that carries it goes, because
	// the last-member rule counts what the cluster still holds.
	mesh := ""
	if state, err := d.Inspect(ctx, id); err == nil {
		mesh = state.MeshID
	}
	if err := d.deletePod(ctx, id); err != nil {
		return err
	}
	if err := d.deleteToken(ctx, id); err != nil {
		return err
	}
	claims := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace)
	if err := claims.Delete(ctx, objectName(id), *d.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("claim delete: %w", err)
	}
	if err := d.waitGone(ctx, "claim", func(ctx context.Context) error {
		_, err := claims.Get(ctx, objectName(id), metav1.GetOptions{})
		return err
	}); err != nil {
		return err
	}
	return d.leaveMesh(ctx, id, mesh)
}

// deletePod removes the Pod of one sandbox and waits until the cluster has
// forgotten it, so the next phase read is not the previous Pod's.
func (d *Driver) deletePod(ctx context.Context, id string) error {
	return d.deletePodNamed(ctx, objectName(id))
}

func (d *Driver) deletePodNamed(ctx context.Context, name string) error {
	pods := d.cs.CoreV1().Pods(d.opts.Namespace)
	if err := pods.Delete(ctx, name, *d.deleteOptions()); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pod delete: %w", err)
	}
	return d.waitGone(ctx, "pod", func(ctx context.Context) error {
		_, err := pods.Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

func (d *Driver) deleteOptions() *metav1.DeleteOptions {
	seconds := int64(d.opts.GracePeriod / time.Second)
	policy := metav1.DeletePropagationForeground
	return &metav1.DeleteOptions{GracePeriodSeconds: &seconds, PropagationPolicy: &policy}
}

// Update rewrites the mutable half of the record: the user's labels, the
// environment the next Exec carries, and the deadlines the reaper reads.
func (d *Driver) Update(ctx context.Context, id string, c driver.Change) error {
	adoption, err := c.Adoption()
	if err != nil {
		return err
	}
	if adoption != nil {
		return d.adopt(ctx, id, *adoption)
	}
	if c.Lifecycle != nil && (c.Lifecycle.TTL < 0 || c.Lifecycle.AutoStop < 0 || c.Lifecycle.AutoDelete < 0) {
		return fmt.Errorf("%w: a negative lifecycle duration", driver.ErrInvalid)
	}
	// The token is written into the Secret the Pod already projects, not
	// into the claim's record: the kubelet re-syncs the file and the
	// workload keeps running. A sandbox that carried none takes one here
	// and mounts it at its next start.
	if len(c.Token) > 0 {
		if _, err := d.getClaim(ctx, id); err != nil {
			return err
		}
		if err := d.putToken(ctx, id, c.Token); err != nil {
			return err
		}
	}
	return d.apply(ctx, id, func(pvc *corev1.PersistentVolumeClaim) (change, error) {
		spec, err := specOf(pvc)
		if err != nil {
			return change{}, err
		}
		if c.Labels != nil {
			spec.Labels = maps.Clone(*c.Labels)
		}
		if c.Env != nil {
			spec.Env = maps.Clone(*c.Env)
		}
		out := change{spec: &spec}
		// The claim records that a token is projected, so a start renders
		// the mount the create rendered. A sandbox that carried none takes
		// the record here with the token.
		if len(c.Token) > 0 && pvc.Annotations[annToken] != "true" {
			out.set = map[string]string{annToken: "true"}
		}
		if c.Lifecycle != nil {
			spec.Lifecycle = *c.Lifecycle
			// The deadline is measured from the sandbox's creation, not from
			// the update, so extending a ttl twice is not a moving window.
			created := parseStamp(pvc.Annotations, annCreatedAt)
			if out.set == nil {
				out.set = map[string]string{}
			}
			maps.Copy(out.set, lifecycleAnnotations(*c.Lifecycle, created))
			for _, key := range []string{annExpiresAt, annAutoStop, annAutoDelete} {
				if _, ok := out.set[key]; !ok {
					out.remove = append(out.remove, key)
				}
			}
		}
		return out, nil
	})
}

// Touch stamps the activity the reaper's idle rule reads. It is not a
// compare-and-swap: activity is a high-water mark, and a stamp that loses a
// race to a later stamp has lost nothing.
func (d *Driver) Touch(ctx context.Context, id string) error {
	if _, err := d.getClaim(ctx, id); err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{annActivityAt: d.opts.Now().UTC().Format(stamp)},
	}})
	if err != nil {
		return err
	}
	name := objectName(id)
	if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pod activity patch: %w", err)
	}
	_, err = d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return mapErr(err, "claim activity patch")
}

// change is one mutation of the claim's stamped record.
type change struct {
	set    map[string]string
	remove []string
	spec   *driver.CreateSpec
}

func (c change) empty() bool { return len(c.set) == 0 && len(c.remove) == 0 && c.spec == nil }

// apply reads the claim, builds the mutation from what it read, and patches
// under a compare-and-swap on the resource version and on the spec annotation
// it built from. A lost race is rebuilt from the claim as it now stands, so
// two writers never interleave into a record neither of them wrote.
func (d *Driver) apply(ctx context.Context, id string, build func(*corev1.PersistentVolumeClaim) (change, error)) error {
	var last error
	for attempt := range patchAttempts {
		pvc, err := d.getClaim(ctx, id)
		if err != nil {
			return err
		}
		mutation, err := build(pvc)
		if err != nil {
			return err
		}
		if mutation.empty() {
			return nil
		}
		patch, err := patchFor(pvc, mutation)
		if err != nil {
			return err
		}
		_, err = d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).
			Patch(ctx, pvc.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return mapErr(err, "claim patch")
		}
		last = err
		if attempt < patchAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval / 5):
			}
		}
	}
	return fmt.Errorf("claim patch lost %d attempts: %w", patchAttempts, last)
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// patchFor builds the guarded JSON patch. The leading test operations are the
// guard: the resource version the claim was read at, when the cluster supplies
// one, and the spec annotation the mutation was built from.
func patchFor(pvc *corev1.PersistentVolumeClaim, c change) ([]byte, error) {
	ops := []patchOp{}
	if pvc.ResourceVersion != "" {
		ops = append(ops, patchOp{Op: "test", Path: "/metadata/resourceVersion", Value: pvc.ResourceVersion})
	}
	if current, ok := pvc.Annotations[annSpec]; ok {
		ops = append(ops, patchOp{Op: "test", Path: annPath(annSpec), Value: current})
	}
	if pvc.Annotations == nil {
		ops = append(ops, patchOp{Op: "add", Path: "/metadata/annotations", Value: map[string]string{}})
	}
	if c.spec != nil {
		encoded, err := json.Marshal(*c.spec)
		if err != nil {
			return nil, err
		}
		ops = append(ops, patchOp{Op: "add", Path: annPath(annSpec), Value: string(encoded)})
	}
	for _, key := range slices.Sorted(maps.Keys(c.set)) {
		ops = append(ops, patchOp{Op: "add", Path: annPath(key), Value: c.set[key]})
	}
	for _, key := range c.remove {
		// A remove of an absent key fails the whole patch, so only what is
		// there is removed.
		if _, ok := pvc.Annotations[key]; ok {
			ops = append(ops, patchOp{Op: "remove", Path: annPath(key)})
		}
	}
	return json.Marshal(ops)
}

// annPath is the JSON pointer of one annotation key.
func annPath(key string) string { return "/metadata/annotations/" + jsonPointer(key) }

// jsonPointer escapes one key for a JSON pointer, with the grammar's own two
// escapes, so a key holding a slash patches the key it names.
func jsonPointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// waitReady waits for the Pod to leave Pending. A Pod that has already run and
// exited has started, so a short main command is not a failed create; only a
// Pod that never starts spends the budget, and the error says what it was
// waiting for.
func (d *Driver) waitReady(ctx context.Context, id string) error {
	return d.waitPodReady(ctx, objectName(id))
}

func (d *Driver) waitPodReady(ctx context.Context, name string) error {
	var last *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, pollInterval, d.opts.ReadyTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := d.cs.CoreV1().Pods(d.opts.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		last = pod
		if _, _, ended := terminated(pod); ended {
			return true, nil
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return ready(pod), nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return true, nil
		default:
			return false, nil
		}
	})
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		// The caller hung up. Its own cancellation is the answer, not a
		// readiness verdict this driver never reached.
		return ctx.Err()
	case last == nil:
		return fmt.Errorf("pod %s does not exist after %s", name, d.opts.ReadyTimeout)
	default:
		return fmt.Errorf("pod %s not ready within %s: %s", name, d.opts.ReadyTimeout, waiting(last))
	}
}

// waitGone waits until a get answers NotFound.
func (d *Driver) waitGone(ctx context.Context, what string, get func(context.Context) error) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, d.opts.ReadyTimeout, true, func(ctx context.Context) (bool, error) {
		err := get(ctx)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s still present after %s: %w", what, d.opts.ReadyTimeout, err)
	}
	return nil
}
