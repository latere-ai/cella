// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// ErrNoSecretKey is a value written to a control plane that was given no key
// to seal it with. Design 008 answers it capability_unsupported: the server
// can serve everything else and cannot serve this.
var ErrNoSecretKey = errors.New("this control plane holds no secret key, so a secret value cannot be stored")

// The mutations the controller appends for the Secret kind, which are the
// three rows design 009's table gives it.
const (
	MutationSecretCreated = "secret.created"
	MutationSecretUpdated = "secret.updated"
	MutationSecretDeleted = "secret.deleted"
)

// Secrets is the Secret kind's store as the controller reads it: the objects
// beside the sandboxes, and the sealed values behind one decrypting call.
//
// The controller holds no value of its own. What its own collection carries
// is the object a read returns; the plaintext is read per compile and handed
// straight to the map the gateway receives.
type Secrets interface {
	// LoadSecrets reads every live Secret, without its value.
	LoadSecrets() (map[string]v1.Secret, error)
	// WriteSecret stores one Secret, seals plaintext where one is given, and
	// records the mutation. It returns the version the value now holds. A
	// nil plaintext leaves the stored value and its version untouched.
	WriteSecret(ctx context.Context, obj v1.Secret, plaintext []byte, mutation string) (version int, err error)
	// RemoveSecret deletes one Secret and its value and records the mutation.
	RemoveSecret(ctx context.Context, id, mutation string) error
	// OpenValue returns one value's plaintext. It has one caller.
	OpenValue(ctx context.Context, secretID string) (plaintext []byte, version int, err error)
}

// SecretUses is the half of a Secrets store that records when a gateway last
// substituted a secret. It is optional: a store without it never stamps
// status.lastUsedAt, and serves every other act the same.
type SecretUses interface {
	// WriteSecretUse stores one Secret's object as given, at the version
	// this process last saw, and leaves its value and its value's version
	// untouched. It appends no journal record: a use is an observation of
	// the data plane, not an act on the Secret.
	WriteSecretUse(ctx context.Context, obj v1.Secret) error
}

// SecretsEnabled reports whether this control plane stores secret values.
func (c *Controller) SecretsEnabled() bool { return c.secrets != nil }

// CreateSecret stores a resolved Secret under a fresh id. The name is unique
// among the owner's live secrets, as it is for every other kind.
func (c *Controller) CreateSecret(ctx context.Context, obj v1.Secret, owner string) (v1.Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		return obj, ErrNoSecretKey
	}
	if owner == "" {
		return obj, errors.New("secret owner is required")
	}
	for _, held := range c.secretObjects {
		if held.Status.Owner == owner && held.Metadata.Name == obj.Metadata.Name {
			return obj, ErrNameTaken
		}
	}
	id, err := newSecretID()
	if err != nil {
		return obj, err
	}
	now := time.Now().UTC()
	plaintext := []byte(obj.Spec.Value)
	obj.Spec.Value = ""
	obj.Status = v1.SecretStatus{ID: id, Owner: owner, CreatedAt: now, UpdatedAt: now}
	if err = c.persistSecret(ctx, obj, plaintext, MutationSecretCreated); err != nil {
		return obj, err
	}
	return c.secretRead(id), nil
}

// UpdateSecret replaces one Secret's spec. A value in the new spec is written
// and bumps status.version; an absent one leaves the stored value as it was.
// Either way every sandbox mounting this secret is handed its map again, so a
// rotation reaches a running workload's next request without a restart.
func (c *Controller) UpdateSecret(ctx context.Context, obj v1.Secret, id string) (v1.Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		return obj, ErrNoSecretKey
	}
	held, ok := c.secretObjects[id]
	if !ok {
		return obj, ErrNotFound
	}
	plaintext := []byte(obj.Spec.Value)
	obj.Spec.Value = ""
	obj.Status = held.Status
	obj.Status.UpdatedAt = time.Now().UTC()
	if err := c.persistSecret(ctx, obj, plaintext, MutationSecretUpdated); err != nil {
		return obj, err
	}
	c.repushMounts(ctx, id)
	return c.secretRead(id), nil
}

// DeleteSecret drops one Secret and its value, and hands every sandbox that
// mounts it a map without it: the placeholder stays in the workload's
// environment as the inert string it always was, status.secrets.notInjectable
// names it, and the next request leaves unauthenticated rather than with a
// value whose owner withdrew it.
func (c *Controller) DeleteSecret(ctx context.Context, id string) (v1.Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		return v1.Secret{}, ErrNoSecretKey
	}
	held, ok := c.secretObjects[id]
	if !ok {
		return v1.Secret{}, ErrNotFound
	}
	previous := c.secretObjects
	delete(c.secretObjects, id)
	if err := c.secrets.RemoveSecret(ctx, id, MutationSecretDeleted); err != nil {
		c.secretObjects = previous
		return v1.Secret{}, err
	}
	if c.durable == nil {
		c.emitSecret(ctx, MutationSecretDeleted, held)
	}
	c.repushMounts(ctx, id)
	// Every answer this type gives goes through the same strip, so the rule
	// that a value is never read back is one line and not a habit.
	return manifest.StripSecretValue(held), nil
}

// ErrSecretNotMounted is a reported use of a secret the sandbox does not bind.
// A gateway holds only the maps of its environment's sandboxes, so a report
// like this is a race with an apply that removed the mount, or a gateway that
// is not telling the truth; either way there is nothing to stamp.
var ErrSecretNotMounted = errors.New("the sandbox does not mount this secret")

// SecretUsed records that a gateway substituted one secret's value into a
// request of one sandbox, as the secret's status.lastUsedAt.
//
// The stamp moves only to a use at least v1.SecretLastUsedResolution after the
// one it holds, which is what bounds the writes to one per secret per
// interval however many gateways and sandboxes report it; a use older than
// the stamp never moves it back. The time is the gateway's, clamped to this
// process's clock so a gateway whose clock runs ahead cannot stamp the
// future, and cut to the second.
//
// It writes nothing for a secret that no longer exists or a sandbox that does
// not bind it, and nothing where the store has no seam for it. A write that
// fails is returned and not retried: the gateway's next report writes it.
func (c *Controller) SecretUsed(ctx context.Context, sandboxID, secretID string, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secretUses == nil {
		return nil
	}
	held, ok := c.secretObjects[secretID]
	if !ok {
		return ErrNotFound
	}
	sb, ok := c.objects[sandboxID]
	if !ok {
		return ErrNotFound
	}
	if !bindsSecret(sb, secretID) {
		return ErrSecretNotMounted
	}
	if now := c.clock.Now(); at.IsZero() || at.After(now) {
		at = now
	}
	at = at.UTC().Truncate(time.Second)
	if last := held.Status.LastUsedAt; !last.IsZero() && at.Sub(last) < v1.SecretLastUsedResolution {
		return nil
	}
	obj := held
	obj.Status.LastUsedAt = at
	if err := c.secretUses.WriteSecretUse(ctx, obj); err != nil {
		return err
	}
	c.secretObjects[secretID] = obj
	return nil
}

// bindsSecret reports whether a sandbox's boundary carries this secret.
func bindsSecret(sb v1.Sandbox, secretID string) bool {
	if sb.Status.EgressState == nil {
		return false
	}
	return slices.ContainsFunc(sb.Status.EgressState.Secrets, func(m v1.MountedSecret) bool { return m.ID == secretID })
}

// GetSecret reads one Secret by its id, or by the name its owner gave it.
func (c *Controller) GetSecret(_ context.Context, key, owner string) (v1.Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.secretIDLocked(key, owner)
	if id == "" {
		return v1.Secret{}, ErrNotFound
	}
	return c.secretRead(id), nil
}

// ListSecrets is every Secret this control plane holds, ordered by id. The
// caller's own filter is the API's: the authorizer decides which of these
// rows that caller may see.
func (c *Controller) ListSecrets() []v1.Secret {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]v1.Secret, 0, len(c.secretObjects))
	for id := range c.secretObjects {
		out = append(out, c.secretRead(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}

// secretIDLocked resolves a route's key to an id: a sec_ id names one secret
// whoever holds it, and any other string names one among the owner's own.
// That is the contract's mount rule as well, so a manifest and a route reach
// exactly the same object for the same string.
func (c *Controller) secretIDLocked(key, owner string) string {
	if strings.HasPrefix(key, v1.SecretIDPrefix) {
		if _, ok := c.secretObjects[key]; ok {
			return key
		}
		return ""
	}
	for id, held := range c.secretObjects {
		if held.Status.Owner == owner && held.Metadata.Name == key {
			return id
		}
	}
	return ""
}

// SecretFor is the manifest resolver's Lookup half over this controller's own
// collection: a mount names a secret by sec_ id or by a name among the
// caller's own, and the object comes back without a value because none is
// held here to return.
func (c *Controller) SecretFor(_ context.Context, nameOrID, owner string) (*v1.Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.secretIDLocked(nameOrID, owner)
	if id == "" {
		return nil, manifest.ErrNotFound
	}
	out := c.secretRead(id)
	return &out, nil
}

// secretRead is one Secret as a caller reads it: the stored object with
// mountedBy derived from the sandboxes that name it. The caller's mutex is
// held.
func (c *Controller) secretRead(id string) v1.Secret {
	out := manifest.StripSecretValue(c.secretObjects[id])
	out.Status.MountedBy = c.mountedByLocked(id)
	return out
}

// mountedByLocked counts the sandboxes bound to this secret that have not
// begun to go. Binding is by id, so a secret deleted and recreated under one
// name starts at zero rather than inheriting the mounts of its namesake.
func (c *Controller) mountedByLocked(id string) int {
	n := 0
	for _, sb := range c.objects {
		if sb.Status.Phase == PhaseDeleting || sb.Status.EgressState == nil {
			continue
		}
		for _, mount := range sb.Status.EgressState.Secrets {
			if mount.ID == id {
				n++
				break
			}
		}
	}
	return n
}

// persistSecret records one Secret and the mutation that produced it. The
// store is written first, so a write that fails leaves this process's
// collection as it was. The caller's mutex is held.
func (c *Controller) persistSecret(ctx context.Context, obj v1.Secret, plaintext []byte, mutation string) error {
	version, err := c.secrets.WriteSecret(ctx, obj, plaintext, mutation)
	if err != nil {
		return err
	}
	obj.Status.Version = version
	c.secretObjects[obj.Status.ID] = obj
	// A durable store journaled the record inside the write above. A
	// snapshot store has no journal, so the emitter takes the act here.
	if c.durable == nil {
		c.emitSecret(ctx, mutation, obj)
	}
	return nil
}

// repushMounts hands every sandbox that mounts this secret its map again, at
// a higher version, so a rotation or a withdrawal reaches a running gateway
// without the sandbox restarting.
//
// It never fails the act that caused it: the sandbox already exists, its
// desired state is written, and a gateway that missed the push is made whole
// by its next snapshot. The caller's mutex is held.
func (c *Controller) repushMounts(ctx context.Context, id string) {
	for _, sb := range c.mountingLocked(id) {
		obj := clone(sb)
		boundary, err := c.pushEgress(ctx, &obj)
		if err != nil {
			c.log.WarnContext(ctx, "the sandbox's map was not pushed after a secret changed",
				"sandbox", obj.Status.ID, "secret", id, "err", err)
		}
		obj.Status.Secrets = boundary.Secrets
		if err := c.persist(ctx, obj, MutationStatus); err != nil {
			c.log.WarnContext(ctx, "the sandbox's state was not written after a secret changed",
				"sandbox", obj.Status.ID, "secret", id, "err", err)
		}
	}
}

// mountingLocked is every live sandbox bound to one secret, ordered by id so
// a push sweep is the same sweep on every run.
func (c *Controller) mountingLocked(id string) []v1.Sandbox {
	var out []v1.Sandbox
	for _, sb := range c.objects {
		if sb.Status.Phase == PhaseDeleting || sb.Status.EgressState == nil {
			continue
		}
		for _, mount := range sb.Status.EgressState.Secrets {
			if mount.ID == id {
				out = append(out, sb)
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b v1.Sandbox) int { return strings.Compare(a.Status.ID, b.Status.ID) })
	return out
}

// newSecretID is a Secret's id: the kind's prefix and the same ULID a sandbox
// takes, so every object of this control plane sorts by the instant it was
// made.
func newSecretID() (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	return v1.SecretIDPrefix + strings.TrimPrefix(id, "sbx_"), nil
}
