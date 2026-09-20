// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// tokenSpec is a create that carries an identity.
func tokenSpec(id string, token string) driver.CreateSpec {
	s := spec(id)
	s.Token = []byte(token)
	return s
}

// secretOf reads the sandbox's token Secret from the cluster double.
func (h *harness) secretOf(t *testing.T, id string) *corev1.Secret {
	t.Helper()
	secret, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token secret %s: %v", id, err)
	}
	return secret
}

// TestTokenIsASecretProjectedReadOnly is spec 006's identity as a cluster
// holds it: one Secret per sandbox, projected on the reserved directory,
// created before the Pod that mounts it.
func TestTokenIsASecretProjectedReadOnly(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_token"
	h.created(t, tokenSpec(id, "first"))

	secret := h.secretOf(t, id)
	if got := string(secret.Data[tokenKey]); got != "first" {
		t.Errorf("the secret holds %q, want %q", got, "first")
	}
	if secret.Labels[labelID] != id || secret.Labels[labelManagedBy] != managedValue {
		t.Errorf("the secret is stamped %v", secret.Labels)
	}

	pod := h.podOf(t, id)
	volume := volumeNamed(t, pod, tokenVolume)
	if volume.Projected == nil || len(volume.Projected.Sources) != 1 {
		t.Fatalf("the token volume is %+v, want one projected source", volume)
	}
	source := volume.Projected.Sources[0].Secret
	if source == nil || source.Name != tokenSecretName(id) {
		t.Fatalf("the projected source is %+v, want the sandbox's secret", source)
	}
	if source.Optional == nil || !*source.Optional {
		t.Error("the projected secret is required, so a Pod outlives its own identity badly")
	}
	if len(source.Items) != 1 || source.Items[0].Key != tokenKey || source.Items[0].Path != path.Base(driver.TokenPath) {
		t.Errorf("the projected items are %v, want the token at %s", source.Items, driver.TokenPath)
	}
	if volume.Projected.DefaultMode == nil || *volume.Projected.DefaultMode != tokenMode {
		t.Errorf("the projected mode is %v, want %o", volume.Projected.DefaultMode, tokenMode)
	}

	mount := mountNamed(t, pod, tokenVolume)
	if mount.MountPath != tokenMount || !mount.ReadOnly {
		t.Errorf("the token mount is %+v, want %s read-only", mount, tokenMount)
	}
	if got := envOf(pod)[driver.TokenFileEnv]; got != driver.TokenPath {
		t.Errorf("%s = %q, want %q", driver.TokenFileEnv, got, driver.TokenPath)
	}
}

// TestTokenIsNotInTheClaimsRecord holds the one rule a projected credential
// has on a cluster: the claim keeps the shape of the sandbox, and an
// annotation is readable by everyone who can read the claim.
func TestTokenIsNotInTheClaimsRecord(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_record"
	h.created(t, tokenSpec(id, "a-secret-token"))
	pvc := h.claimOf(t, id)
	for key, value := range pvc.Annotations {
		if strings.Contains(value, "a-secret-token") {
			t.Errorf("the claim annotation %s carries the token", key)
		}
	}
	if pvc.Annotations[annToken] != "true" {
		t.Errorf("the claim does not record that a token was projected: %v", pvc.Annotations)
	}
	var s driver.CreateSpec
	if err := json.Unmarshal([]byte(pvc.Annotations[annSpec]), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Token) != 0 {
		t.Errorf("the recorded spec carries a token of %d bytes", len(s.Token))
	}
}

// TestStartKeepsTheProjection rebuilds the Pod from the claim's record, which
// does not hold the token, and still mounts the identity the sandbox has.
func TestStartKeepsTheProjection(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_restart"
	h.created(t, tokenSpec(id, "first"))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pod := h.podOf(t, id)
	mountNamed(t, pod, tokenVolume)
	if got := envOf(pod)[driver.TokenFileEnv]; got != driver.TokenPath {
		t.Errorf("%s = %q after a start, want %q", driver.TokenFileEnv, got, driver.TokenPath)
	}
	if got := string(h.secretOf(t, id).Data[tokenKey]); got != "first" {
		t.Errorf("the secret holds %q after a start, want %q", got, "first")
	}
}

// TestUpdateReprojectsTheToken is the rotation the reaper drives: the Secret
// is replaced in place, so the kubelet re-syncs the file and the workload is
// never restarted.
func TestUpdateReprojectsTheToken(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_rotate"
	h.created(t, tokenSpec(id, "first"))
	h.cs.ClearActions()
	if err := h.Update(t.Context(), id, driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	if got := string(h.secretOf(t, id).Data[tokenKey]); got != "second" {
		t.Errorf("the secret holds %q, want %q", got, "second")
	}
	for _, action := range h.cs.Actions() {
		if action.GetResource().Resource == "pods" && action.GetVerb() != "get" {
			t.Errorf("the rotation %sd the Pod; a re-projection keeps the workload running", action.GetVerb())
		}
	}
}

// TestUpdateOfAMissingSandboxProjectsNothing keeps a rotation against a
// sandbox this cluster does not have from creating the Secret of one.
func TestUpdateOfAMissingSandboxProjectsNothing(t *testing.T) {
	h := newHarness(t)
	if err := h.Update(t.Context(), "sbx_gone", driver.Change{Token: []byte("first")}); err == nil {
		t.Fatal("a re-projection into a sandbox that does not exist was reported as a success")
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName("sbx_gone"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the refused re-projection left a secret behind: %v", err)
	}
}

// TestUpdateAddsTheProjectionToASandboxWithout gives a sandbox created before
// this control plane minted anything an identity at its next start.
func TestUpdateAddsTheProjectionToASandboxWithout(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_late"
	h.created(t, spec(id))
	if err := h.Update(t.Context(), id, driver.Change{Token: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if h.claimOf(t, id).Annotations[annToken] != "true" {
		t.Fatal("the claim does not record the projection the update made")
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	mountNamed(t, h.podOf(t, id), tokenVolume)
}

// TestDeleteRemovesTheSecret leaves no identity behind: the sandbox is gone
// and so is the material that spoke for it.
func TestDeleteRemovesTheSecret(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_gone_token"
	h.created(t, tokenSpec(id, "first"))
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName(id), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the token secret outlived the sandbox: %v", err)
	}
	// A second delete of the same sandbox is the state this asks for.
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
}

// TestNoTokenProjectsNothing is the hosted regression restated on a cluster.
func TestNoTokenProjectsNothing(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_plain"
	h.created(t, spec(id))
	pod := h.podOf(t, id)
	if slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == tokenVolume }) {
		t.Error("a token volume was rendered for a sandbox that carries none")
	}
	if _, ok := envOf(pod)[driver.TokenFileEnv]; ok {
		t.Errorf("%s is set on a sandbox with no token", driver.TokenFileEnv)
	}
	if h.claimOf(t, id).Annotations[annToken] != "" {
		t.Error("the claim records a projection that did not happen")
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName(id), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a secret was created for a sandbox with no token: %v", err)
	}
}

// TestFileHelperCarriesNoToken keeps the transfer Pod out of the identity: it
// reaches the claim's files and nothing the sandbox holds.
func TestFileHelperCarriesNoToken(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_helper"
	h.created(t, tokenSpec(id, "first"))
	helper, err := h.helperPod(id, tokenSpec(id, "first"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(helper.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == tokenVolume }) {
		t.Error("the transfer helper mounts the sandbox's identity")
	}
}

func volumeNamed(t *testing.T, pod *corev1.Pod, name string) corev1.Volume {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("the Pod has no volume %q; it has %v", name, pod.Spec.Volumes)
	return corev1.Volume{}
}

func mountNamed(t *testing.T, pod *corev1.Pod, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("the container has no mount %q; it has %v", name, pod.Spec.Containers[0].VolumeMounts)
	return corev1.VolumeMount{}
}

func envOf(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

// TestUpdateCarriesTheTokenBesideTheOtherFields keeps one call that changes
// the labels and the identity from dropping either half.
func TestUpdateCarriesTheTokenBesideTheOtherFields(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_both"
	h.created(t, spec(id))
	labels := map[string]string{"team": "a"}
	if err := h.Update(t.Context(), id, driver.Change{Labels: &labels, Token: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	pvc := h.claimOf(t, id)
	if pvc.Annotations[annToken] != "true" {
		t.Errorf("the claim does not record the projection: %v", pvc.Annotations)
	}
	s, err := specOf(pvc)
	if err != nil {
		t.Fatal(err)
	}
	if s.Labels["team"] != "a" {
		t.Errorf("the labels the same call carried were dropped: %v", s.Labels)
	}
	if got := string(h.secretOf(t, id).Data[tokenKey]); got != "first" {
		t.Errorf("the secret holds %q, want %q", got, "first")
	}
}
