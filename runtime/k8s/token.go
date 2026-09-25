// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"maps"
	"path"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

// The projection of spec 006's identity and spec 018's trust file on a
// cluster: one Secret per sandbox, mounted read-only on the directory the
// control plane reserves, with the token as one key and the trust file, the
// public roots and the gateway's certificate authority, as another.
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
	// authorityKey is the trust file, the public roots and the gateway's
	// authority, and the file name that lands it at egress.CAPath beside the
	// token.
	authorityKey = "egress-ca.pem"
	// authorityMode is readable by every user of the sandbox: it holds
	// certificates and no secret, and a tool the workload runs as another
	// user verifies through it as well.
	authorityMode int32 = 0o444
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
// identity or a trust file carries. Both keys are listed whichever the Secret
// holds, so a prewarmed entry that is adopted later receives both through the
// kubelet's sync. The source is optional, which covers a missing
// key as well as a missing Secret, so a Pod whose Secret was removed starts
// with an empty directory rather than staying Pending forever.
func tokenProjection(id string) (corev1.Volume, corev1.VolumeMount) {
	mode := tokenMode
	optional := true
	volume := corev1.Volume{
		Name: tokenVolume,
		Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: &mode,
			Sources: []corev1.VolumeProjection{{
				Secret: &corev1.SecretProjection{
					Name: tokenSecretName(id),
					Items: []corev1.KeyToPath{
						{Key: tokenKey, Path: path.Base(driver.TokenPath)},
						{Key: authorityKey, Path: path.Base(egress.CAPath), Mode: ptr(authorityMode)},
					},
					Optional: &optional,
				},
			}},
		},
	}
	return volume, corev1.VolumeMount{Name: tokenVolume, MountPath: tokenMount, ReadOnly: true}
}

// putTrust adds the trust file to the Secret data a write carries: the
// driver's public roots, then the gateway's authority the sandbox was created
// with. A sandbox created with no authority carries no trust file.
func (d *Driver) putTrust(data map[string][]byte, authority string) {
	if bundle := egress.TrustBundle(d.opts.TrustRoots, authority); bundle != nil {
		data[authorityKey] = bundle
	}
}

// putSecret creates the sandbox's Secret, or writes the keys given into the
// one there and leaves every other key as it is. A token rotation is such a
// write, of the token and the trust file: the kubelet re-syncs the projected
// files within its sync window and the workload keeps running.
func (d *Driver) putSecret(ctx context.Context, id string, data map[string][]byte) error {
	secrets := d.cs.CoreV1().Secrets(d.opts.Namespace)
	secret := &corev1.Secret{
		Name: tokenSecretName(id), Namespace: d.opts.Namespace,
		Labels: map[string]string{labelManagedBy: managedValue, labelID: id},
		Type:   corev1.SecretTypeOpaque,
		Data:   maps.Clone(data),
	}
	_, err := secrets.Create(ctx, secret, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return mapErr(err, "token secret create")
	}
	current, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return mapErr(err, "token secret get")
	}
	if current.Data == nil {
		current.Data = map[string][]byte{}
	}
	maps.Copy(current.Data, data)
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
