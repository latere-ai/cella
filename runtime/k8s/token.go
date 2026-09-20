// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"path"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// The projection of spec 006's identity on a cluster: one Secret per sandbox,
// mounted read-only on the directory the control plane reserves, with the
// token as one key in it.
const (
	// tokenVolume is the name of the projected volume inside the Pod.
	tokenVolume = "cella-token"
	// tokenKey is the Secret key and the file name inside the mount, so the
	// file lands exactly at runtime.TokenPath.
	tokenKey = "token"
	// tokenMode is what the file is created with. Group read is added by
	// the kubelet where the Pod carries an fsGroup, which the baseline sets
	// to the sandbox's own group, so the sandbox reads its identity and
	// nothing outside the Pod can.
	tokenMode int32 = 0o400
)

// tokenMount is the directory the Secret is projected on, which is the parent
// of the reserved path. It is a directory and not a subPath mount of one key,
// because a subPath does not follow a Secret that changes and a rotation
// would then need a restart.
var tokenMount = path.Dir(driver.TokenPath)

// tokenSecretName is the Secret of one sandbox. It is derived from the object
// name, so a start rebuilds the reference from the id alone.
func tokenSecretName(id string) string { return "cella-token-" + objectName(id) }

// tokenProjection is the volume and the mount every Pod of a sandbox with an
// identity carries. The source is optional, so a Pod whose Secret was removed
// starts with an empty directory rather than staying Pending forever.
func tokenProjection(id string) (corev1.Volume, corev1.VolumeMount) {
	mode := tokenMode
	optional := true
	volume := corev1.Volume{
		Name: tokenVolume,
		Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: &mode,
			Sources: []corev1.VolumeProjection{{
				Secret: &corev1.SecretProjection{
					Name:     tokenSecretName(id),
					Items:    []corev1.KeyToPath{{Key: tokenKey, Path: path.Base(driver.TokenPath)}},
					Optional: &optional,
				},
			}},
		},
	}
	return volume, corev1.VolumeMount{Name: tokenVolume, MountPath: tokenMount, ReadOnly: true}
}

// putToken creates or replaces the sandbox's Secret. A replace is what a
// rotation is: the kubelet re-syncs the projected file within its sync window
// and the workload keeps running.
func (d *Driver) putToken(ctx context.Context, id string, token []byte) error {
	secrets := d.cs.CoreV1().Secrets(d.opts.Namespace)
	secret := &corev1.Secret{
		Name: tokenSecretName(id), Namespace: d.opts.Namespace,
		Labels: map[string]string{labelManagedBy: managedValue, labelID: id},
		Type:   corev1.SecretTypeOpaque,
		Data:   map[string][]byte{tokenKey: token},
	}
	_, err := secrets.Create(ctx, secret, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return mapErr(err, "token secret create")
	}
	current, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return mapErr(err, "token secret get")
	}
	// Only this key is replaced: spec 018 projects the gateway's authority
	// through the same mount, and a rotation of the identity is not a
	// withdrawal of the boundary.
	if current.Data == nil {
		current.Data = map[string][]byte{}
	}
	current.Data[tokenKey] = token
	_, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
	return mapErr(err, "token secret update")
}

// deleteToken removes the sandbox's Secret. A Secret that is already gone is
// the state this asks for.
func (d *Driver) deleteToken(ctx context.Context, id string) error {
	err := d.cs.CoreV1().Secrets(d.opts.Namespace).Delete(ctx, tokenSecretName(id), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return mapErr(err, "token secret delete")
}
