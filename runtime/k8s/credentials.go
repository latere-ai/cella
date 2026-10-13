// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// The credentials of one sandbox, the values of its environment and its
// gateway credential, are kept in the sandbox's Secret and never in the record
// the claim carries. The record is object metadata, which every account that
// may read the namespace's claims reads; a Secret is read only by an account
// granted Secrets. Neither key is listed in the projection, so neither reaches
// the sandbox's file system through the mount.
const (
	// envKey holds the environment as one JSON object. One key and not one
	// per variable: a write merges keys into the Secret, so a variable an
	// update drops would outlive it, and a variable named like another key
	// would overwrite that key.
	envKey = "env.json"
	// credentialKey holds the credential both gateway doors authenticate.
	credentialKey = "egress-credential"
)

// credentials is the half of a spec the claim's record leaves out.
type credentials struct {
	env        map[string]string
	credential string
}

func (c credentials) empty() bool { return len(c.env) == 0 && c.credential == "" }

// data renders the credentials as the Secret keys a write carries. Both keys
// are written, an empty environment as an empty object, so a Secret that
// carries the environment key is the whole answer and the record is not read
// for one.
func (c credentials) data() (map[string][]byte, error) {
	env, err := encodeEnv(c.env)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{envKey: env, credentialKey: []byte(c.credential)}, nil
}

func encodeEnv(env map[string]string) ([]byte, error) {
	if env == nil {
		env = map[string]string{}
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encoding the environment: %w", err)
	}
	return encoded, nil
}

// strip splits a spec into the record the claim carries and the credentials
// the Secret carries.
func strip(s driver.CreateSpec) (driver.CreateSpec, credentials) {
	held := credentials{env: s.Env, credential: s.Egress.Credential}
	s.Env = nil
	s.Egress.Credential = ""
	return s, held
}

// encodeRecord is the value of the spec annotation: the spec without its
// credentials. Every write of the annotation goes through it.
func encodeRecord(s driver.CreateSpec) (string, error) {
	stripped, _ := strip(s)
	encoded, err := json.Marshal(stripped)
	if err != nil {
		return "", fmt.Errorf("encoding the record: %w", err)
	}
	return string(encoded), nil
}

// inline reports whether a claim's record still carries credentials, which
// only a record written before they moved to the Secret does. An unreadable
// record carries none this driver can move.
func inline(pvc *corev1.PersistentVolumeClaim) bool {
	s, err := specOf(pvc)
	if err != nil {
		return false
	}
	_, held := strip(s)
	return !held.empty()
}

// fullSpec is the claim's record with its credentials put back, which is what
// a Pod is rendered from and what a command runs with. Values the record
// still carries win over the Secret's: only a driver from before the move
// writes them there, so they are the newest, and they are the same values
// while a rewrite is between its two writes. A Secret that is gone, or that
// carries no environment key, is a sandbox with no credentials to add.
func (d *Driver) fullSpec(ctx context.Context, id string, pvc *corev1.PersistentVolumeClaim) (driver.CreateSpec, error) {
	s, err := specOf(pvc)
	if err != nil {
		return driver.CreateSpec{}, err
	}
	if _, held := strip(s); !held.empty() {
		return s, nil
	}
	secret, err := d.cs.CoreV1().Secrets(d.opts.Namespace).Get(ctx, tokenSecretName(id), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return s, nil
	case err != nil:
		return driver.CreateSpec{}, fmt.Errorf("credentials secret get: %w", err)
	}
	raw, ok := secret.Data[envKey]
	if !ok {
		return s, nil
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil {
		return driver.CreateSpec{}, fmt.Errorf("%w: secret %s key %s: %w", driver.ErrInvalid, secret.Name, envKey, err)
	}
	if len(env) > 0 {
		s.Env = env
	}
	s.Egress.Credential = string(secret.Data[credentialKey])
	return s, nil
}

// secretWrite is what one mutation of the claim writes into the Secret before
// the claim is patched: the credentials a record from before the move still
// carries, with the record rewritten in the same patch, and the environment
// the mutation sets. The Secret is written first, so a patch that loses its
// race loses no value, and the rebuild from the claim as it then stands
// writes the same keys again.
func secretWrite(pvc *corev1.PersistentVolumeClaim, mutation *change) (map[string][]byte, error) {
	data := map[string][]byte{}
	// An unreadable record holds nothing to move, and refusing it is the
	// build's: every build that writes the record reads it first.
	if s, err := specOf(pvc); err == nil {
		if _, held := strip(s); !held.empty() {
			if data, err = held.data(); err != nil {
				return nil, err
			}
			if mutation.spec == nil {
				mutation.spec = &s
			}
		}
	}
	if mutation.env != nil {
		env, err := encodeEnv(*mutation.env)
		if err != nil {
			return nil, err
		}
		data[envKey] = env
	}
	return data, nil
}

// MoveCredentials rewrites every sandbox record in the namespace that an
// earlier version of this driver stamped with the sandbox's credentials: the
// values go into the sandbox's Secret and the record keeps the rest. It is
// what a control plane runs once at start, so the values leave the claims
// when the new version is rolled out rather than at each sandbox's next
// change. It reports how many records it rewrote and every failure; a record
// it could not rewrite is still read as it stands and is rewritten by its
// sandbox's next change. Two processes that run it at once rewrite each
// record once, and Preflight does not run it, because Preflight writes
// nothing.
func (d *Driver) MoveCredentials(ctx context.Context) (int, error) {
	claims, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return 0, fmt.Errorf("claim list: %w", err)
	}
	rewritten := 0
	var failed error
	for i := range claims.Items {
		pvc := &claims.Items[i]
		id := pvc.Labels[labelID]
		if !inline(pvc) || id == "" {
			continue
		}
		// An empty mutation: apply moves the credentials and rewrites the
		// record, and does nothing to a record another process rewrote
		// first.
		err := d.apply(ctx, id, func(*corev1.PersistentVolumeClaim) (change, error) { return change{}, nil })
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("sandbox %s: %w", id, err))
			continue
		}
		rewritten++
	}
	return rewritten, failed
}
