// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

var (
	// ErrVolumeBusy is an attach or a delete that a volume's access or its
	// attachments exclude. Design 008 answers it with 409 volume_busy.
	ErrVolumeBusy = errors.New("the volume is attached in a way that excludes this")
	// ErrNoVolumeStore is a volume act on a control plane whose store keeps
	// no volumes. Design 008 answers it capability_unsupported.
	ErrNoVolumeStore = errors.New("this control plane's store keeps no volumes")
	// ErrVolumeQuota is a create past the owner's volume ceiling. Design 008
	// answers it with 422 volume_quota_exceeded.
	ErrVolumeQuota = errors.New("volume count limit reached")
)

// The mutations the controller appends for the Volume kind: design 009's
// rows for it, and the status write that is no event, which is how a fill
// that completed, a delete in flight and a volume the driver lost reach the
// store.
const (
	MutationVolumeCreated  = "volume.created"
	MutationVolumeUpdated  = "volume.updated"
	MutationVolumeAttached = "volume.attached"
	MutationVolumeDetached = "volume.detached"
	MutationVolumeFailed   = "volume.failed"
	MutationVolumeDeleted  = "volume.deleted"
	MutationVolumeStatus   = "volume.status"
)

// The two modes an attach record names, which is how a sandbox holds one
// volume.
const (
	VolumeModeReadOnly  = "read-only"
	VolumeModeReadWrite = "read-write"
)

// ReasonVolumesUnavailable is the VolumesAttached reason of a create that
// could not attach a volume it was bound to: the volume failed, was lost, or
// was deleted while the sandbox waited for it.
const ReasonVolumesUnavailable = "VolumeUnavailable"

// DefaultVolumeInterval is how often the volume loop passes over the volumes
// being filled and deleted when nothing wakes it sooner.
const DefaultVolumeInterval = 5 * time.Second

// VolumesLease is the lease the volume loop runs under, so one replica fills
// and deletes the volumes of every environment.
const VolumesLease = "volumes"

// MetricLeaseVolumes is the volume loop's lease by kind, for LeaseHeld.
const MetricLeaseVolumes = "volumes"

// VolumeAct is one thing the controller did to one Volume: design 009's
// type, the object as it stands after the act, and for an attach or a detach
// the sandbox and the mode it holds the volume in.
type VolumeAct struct {
	Type    string
	Object  v1.Volume
	Sandbox *v1.Sandbox
	Mode    string
}

// Volumes is the Volume kind's store as the controller reads it. A write
// records the act's mutation with the object, in one transaction where the
// store has them.
type Volumes interface {
	// LoadVolumes reads every live Volume.
	LoadVolumes() (map[string]v1.Volume, error)
	// WriteVolume stores the act's object and records the act.
	WriteVolume(ctx context.Context, act VolumeAct) error
	// RemoveVolume deletes the act's object and records the act.
	RemoveVolume(ctx context.Context, act VolumeAct) error
}

// VolumesEnabled reports whether this control plane keeps volumes.
func (c *Controller) VolumesEnabled() bool { return c.volumes != nil }

// volumeDriver is the driver of one environment as a keeper of volumes. An
// environment whose driver declares no Volumes capability keeps none, so a
// driver that implements the interface and does not declare it is not
// asked.
func (c *Controller) volumeDriver(environment string) (driver.VolumeDriver, error) {
	d, err := c.driverFor(environment)
	if err != nil {
		return nil, err
	}
	vd, ok := d.(driver.VolumeDriver)
	if !ok || !d.Capabilities().Volumes {
		return nil, fmt.Errorf("%w: the %s driver keeps no volumes", driver.ErrUnsupported, d.Name())
	}
	return vd, nil
}

// CreateVolume stores a resolved Volume under a fresh id, Pending, and wakes
// the volume loop, which asks the driver to fill it. The name is unique among
// the owner's live volumes, as it is for every other kind. max is the owner's
// volume ceiling, 0 for none: a create that would hold more than max volumes
// is ErrVolumeQuota. The count and the write run under one hold of the
// controller's lock, so two creates racing for the last slot yield one volume
// and one refusal.
func (c *Controller) CreateVolume(ctx context.Context, obj v1.Volume, owner string, max int) (v1.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		return obj, ErrNoVolumeStore
	}
	if owner == "" {
		return obj, errors.New("volume owner is required")
	}
	for _, held := range c.volumeObjects {
		if held.Status.Owner == owner && held.Metadata.Name == obj.Metadata.Name {
			return obj, ErrNameTaken
		}
	}
	if count := c.countVolumesLocked(owner); max > 0 && count >= max {
		return obj, fmt.Errorf("%w: the owner holds %d volumes of a ceiling of %d", ErrVolumeQuota, count, max)
	}
	if err := c.admits(obj.Spec.Environment); err != nil {
		return obj, err
	}
	if _, err := c.volumeDriver(obj.Spec.Environment); err != nil {
		return obj, err
	}
	id, err := newVolumeID()
	if err != nil {
		return obj, err
	}
	now := time.Now().UTC()
	obj.Status = v1.VolumeStatus{ID: id, Owner: owner, Phase: v1.VolumePending, CreatedAt: now, UpdatedAt: now}
	if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeCreated, Object: obj}); err != nil {
		return obj, err
	}
	c.wakeVolumes()
	return c.volumeRead(id), nil
}

// countVolumesLocked is how many volumes the owner holds against the volume
// ceiling: every one of its volumes but those being deleted. A Failed or Lost
// volume keeps its slot, since its row and its name stay until the owner
// deletes it, and a Deleting one gives its slot up at once, since nothing can
// mount it again and the volume loop finishes its delete. The caller holds
// the controller's lock.
func (c *Controller) countVolumesLocked(owner string) int {
	count := 0
	for _, held := range c.volumeObjects {
		if held.Status.Owner == owner && held.Status.Phase != v1.VolumeDeleting {
			count++
		}
	}
	return count
}

// UpdateVolume replaces one Volume's metadata and the fields of its spec an
// update may change, which the resolver has already held it to.
func (c *Controller) UpdateVolume(ctx context.Context, obj v1.Volume, id string) (v1.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		return obj, ErrNoVolumeStore
	}
	held, ok := c.volumeObjects[id]
	if !ok {
		return obj, ErrNotFound
	}
	obj.Status = held.Status
	obj.Status.UpdatedAt = time.Now().UTC()
	if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeUpdated, Object: obj}); err != nil {
		return obj, err
	}
	return c.volumeRead(id), nil
}

// DeleteVolume ends one volume that no sandbox holds. The intent is written
// before the driver is asked, so a delete the driver does not finish is
// finished by the volume loop rather than forgotten. A volume any sandbox's
// desired state still mounts, Stopped ones included, is ErrVolumeBusy: its
// holder detaches first.
func (c *Controller) DeleteVolume(ctx context.Context, id string) (v1.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		return v1.Volume{}, ErrNoVolumeStore
	}
	held, ok := c.volumeObjects[id]
	if !ok {
		return v1.Volume{}, ErrNotFound
	}
	if holders := c.attachedToLocked(id); len(holders) > 0 {
		return c.volumeRead(id), fmt.Errorf("%w: it is mounted by %s", ErrVolumeBusy, strings.Join(holders, ", "))
	}
	if held.Status.Phase != v1.VolumeDeleting {
		held.Status.Phase = v1.VolumeDeleting
		held.Status.UpdatedAt = time.Now().UTC()
		if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeStatus, Object: held}); err != nil {
			return v1.Volume{}, err
		}
	}
	// A fill in flight settles against the row and removes what it made
	// once it sees the delete, so the driver is not asked twice at once.
	if _, filling := c.filling[id]; filling {
		return c.volumeRead(id), nil
	}
	if err := c.finishVolumeDelete(ctx, held); err != nil {
		c.log.WarnContext(ctx, "the volume's delete is left to the volume loop", "volume", id, "err", err)
		c.wakeVolumes()
	}
	// The answer is the volume as the delete found it, which no sandbox
	// holds, in the shape every other read has.
	out := manifest.CloneVolume(held)
	out.Status.AttachedTo = []string{}
	return out, nil
}

// finishVolumeDelete asks the driver to remove the volume and then removes
// the row. The caller's mutex is held.
func (c *Controller) finishVolumeDelete(ctx context.Context, held v1.Volume) error {
	vd, err := c.volumeDriver(held.Spec.Environment)
	if err != nil {
		return err
	}
	if err := vd.DeleteVolume(ctx, held.Status.ID); err != nil && !errors.Is(err, driver.ErrNotFound) {
		return err
	}
	return c.removeVolume(ctx, VolumeAct{Type: MutationVolumeDeleted, Object: held})
}

// GetVolume reads one Volume by its id, or by the name its owner gave it.
func (c *Controller) GetVolume(_ context.Context, key, owner string) (v1.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.volumeIDLocked(key, owner)
	if id == "" {
		return v1.Volume{}, ErrNotFound
	}
	return c.volumeRead(id), nil
}

// ListVolumes is every Volume this control plane holds, ordered by id. The
// caller's own filter is the API's: the authorizer decides which of these
// rows that caller may see.
func (c *Controller) ListVolumes() []v1.Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]v1.Volume, 0, len(c.volumeObjects))
	for id := range c.volumeObjects {
		out = append(out, c.volumeRead(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}

// VolumeFor is the manifest resolver's Lookup half over this controller's
// own collection: a mount names a volume by vol_ id or by a name among the
// caller's own.
func (c *Controller) VolumeFor(_ context.Context, nameOrID, owner string) (*v1.Volume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.volumeIDLocked(nameOrID, owner)
	if id == "" {
		return nil, manifest.ErrNotFound
	}
	out := c.volumeRead(id)
	return &out, nil
}

// volumeIDLocked resolves a key to an id: a vol_ id names one volume whoever
// holds it, and any other string names one among the owner's own, which is
// the contract's mount rule as well.
func (c *Controller) volumeIDLocked(key, owner string) string {
	if strings.HasPrefix(key, v1.VolumeIDPrefix) {
		if _, ok := c.volumeObjects[key]; ok {
			return key
		}
		return ""
	}
	for id, held := range c.volumeObjects {
		if held.Status.Owner == owner && held.Metadata.Name == key {
			return id
		}
	}
	return ""
}

// volumeRead is one Volume as a caller reads it: the stored object with
// attachedTo derived from the sandboxes whose desired state mounts it. The
// caller's mutex is held.
func (c *Controller) volumeRead(id string) v1.Volume {
	out := manifest.CloneVolume(c.volumeObjects[id])
	out.Status.AttachedTo = c.attachedToLocked(id)
	return out
}

// attachedToLocked is every sandbox bound to one volume that has not begun
// to go, ordered by id. A Stopped sandbox holds its attachments, and so does
// one whose create failed, until it is deleted.
func (c *Controller) attachedToLocked(id string) []string {
	out := []string{}
	for _, sb := range c.objects {
		if sb.Status.Phase == PhaseDeleting {
			continue
		}
		if slices.ContainsFunc(sb.Status.Volumes, func(a v1.VolumeAttachment) bool { return a.Volume == id }) {
			out = append(out, sb.Status.ID)
		}
	}
	slices.Sort(out)
	return out
}

// bindVolumesLocked binds every mount of a sandbox being created to the
// volume it names, which is the authoritative attach of spec 019: under the
// controller's lock and before the sandbox's row is written, so two creates
// racing for a single volume yield one sandbox and one volume_busy, and a
// refusal leaves nothing behind. A volume still being filled is bound, and
// the sandbox waits for it before its driver is asked.
func (c *Controller) bindVolumesLocked(obj *v1.Sandbox) error {
	if len(obj.Spec.Volumes) == 0 {
		return nil
	}
	if c.volumes == nil {
		return ErrNoVolumeStore
	}
	bound := make([]v1.VolumeAttachment, 0, len(obj.Spec.Volumes))
	for _, m := range obj.Spec.Volumes {
		id := c.volumeIDLocked(m.Volume, obj.Status.Owner)
		if id == "" {
			return fmt.Errorf("%w: the volume %s", ErrNotFound, m.Volume)
		}
		vol := c.volumeObjects[id]
		if vol.Spec.Environment != obj.Status.Environment {
			return &manifest.Error{Code: "invalid_field", Path: "spec.volumes",
				Detail: "the volume " + m.Volume + " lives in the environment " + vol.Spec.Environment}
		}
		if vol.Status.Phase != v1.VolumeAvailable && vol.Status.Phase != v1.VolumePending {
			return &manifest.Error{Code: "invalid_field", Path: "spec.volumes",
				Detail: "the volume " + m.Volume + " is " + vol.Status.Phase}
		}
		if err := c.attachableLocked(vol, m.ReadOnly); err != nil {
			return err
		}
		bound = append(bound, v1.VolumeAttachment{Name: m.Name, Volume: id})
	}
	obj.Status.Volumes = bound
	return nil
}

// attachableLocked is spec 019's access table for one more attach: a
// shared-read volume takes read-only attaches alone, and a single one takes
// one read-write attach or any number of read-only ones, never both.
func (c *Controller) attachableLocked(vol v1.Volume, readOnly bool) error {
	if vol.Spec.Access == v1.VolumeAccessSharedRead {
		if !readOnly {
			return fmt.Errorf("%w: %s is shared read-only", ErrVolumeBusy, vol.Metadata.Name)
		}
		return nil
	}
	for _, sb := range c.objects {
		if sb.Status.Phase == PhaseDeleting {
			continue
		}
		for _, a := range sb.Status.Volumes {
			if a.Volume != vol.Status.ID {
				continue
			}
			if !readOnly || !mountReadOnly(sb, a.Name) {
				return fmt.Errorf("%w: %s is attached to %s", ErrVolumeBusy, vol.Metadata.Name, sb.Status.ID)
			}
		}
	}
	return nil
}

// mountReadOnly reports whether a sandbox holds the mount of this name
// read-only.
func mountReadOnly(sb v1.Sandbox, name string) bool {
	for _, m := range sb.Spec.Volumes {
		if m.Name == name {
			return m.ReadOnly
		}
	}
	return false
}

// volumesReadyLocked reports whether every volume a placed sandbox is bound
// to is Available, so its driver may be asked. It reports false with no
// error while one is still being filled, and an error where one failed, was
// lost, or is gone, which the sandbox cannot wait out.
func (c *Controller) volumesReadyLocked(obj v1.Sandbox) (bool, error) {
	ready := true
	for _, a := range obj.Status.Volumes {
		vol, held := c.volumeObjects[a.Volume]
		switch {
		case !held:
			return false, fmt.Errorf("the volume %s of the mount %s no longer exists", a.Volume, a.Name)
		case vol.Status.Phase == v1.VolumePending:
			ready = false
		case vol.Status.Phase != v1.VolumeAvailable:
			return false, fmt.Errorf("the volume %s of the mount %s is %s", a.Volume, a.Name, vol.Status.Phase)
		}
	}
	return ready, nil
}

// volumesUnavailable is the condition a sandbox carries when it could not
// attach a volume it was bound to.
func volumesUnavailable(err error, now time.Time) v1.Condition {
	return v1.Condition{
		Type: v1.ConditionVolumesAttached, Status: v1.ConditionFalse, Reason: ReasonVolumesUnavailable,
		Message: "A volume could not be attached: " + err.Error() + ".", Since: now,
	}
}

// mountsOf is the driver's half of a sandbox's mounts: each mount of the
// manifest joined to the volume it was bound to at create.
func mountsOf(obj v1.Sandbox) []driver.Mount {
	if len(obj.Spec.Volumes) == 0 {
		return nil
	}
	ids := map[string]string{}
	for _, a := range obj.Status.Volumes {
		ids[a.Name] = a.Volume
	}
	out := make([]driver.Mount, 0, len(obj.Spec.Volumes))
	for _, m := range obj.Spec.Volumes {
		out = append(out, driver.Mount{Name: m.Name, VolumeID: ids[m.Name], Path: m.Path, ReadOnly: m.ReadOnly})
	}
	return out
}

// markAttached records that the driver created a sandbox with every volume
// it is bound to.
func markAttached(obj *v1.Sandbox, now time.Time) {
	if len(obj.Status.Volumes) == 0 {
		return
	}
	for i := range obj.Status.Volumes {
		obj.Status.Volumes[i].Attached = true
	}
	obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
		Type: v1.ConditionVolumesAttached, Status: v1.ConditionTrue, Since: now,
	})
}

// recordAttachments journals one volume.attached or volume.detached per
// volume a sandbox is bound to. A record is a note about work and not the
// work, so one that is not written is logged and the act stands. The
// caller's mutex is held.
func (c *Controller) recordAttachments(ctx context.Context, obj v1.Sandbox, mutation string) {
	for _, a := range obj.Status.Volumes {
		vol, held := c.volumeObjects[a.Volume]
		if !held {
			continue
		}
		mode := VolumeModeReadWrite
		if mountReadOnly(obj, a.Name) {
			mode = VolumeModeReadOnly
		}
		sandbox := export(obj)
		if err := c.persistVolume(ctx, VolumeAct{Type: mutation, Object: vol, Sandbox: &sandbox, Mode: mode}); err != nil {
			c.log.WarnContext(ctx, "the volume's record was not written", "volume", a.Volume, "sandbox", obj.Status.ID,
				"type", mutation, "err", err)
		}
	}
}

// releaseVolumesLocked is the volume half of a sandbox's delete: one detach
// record per volume, and a volume that does not retain and that no other
// sandbox holds marked for deletion, which the volume loop finishes. The
// sandbox's row is already gone, so attachedTo no longer counts it.
func (c *Controller) releaseVolumesLocked(ctx context.Context, obj v1.Sandbox) {
	if len(obj.Status.Volumes) == 0 {
		return
	}
	c.recordAttachments(ctx, obj, MutationVolumeDetached)
	woken := false
	for _, a := range obj.Status.Volumes {
		vol, held := c.volumeObjects[a.Volume]
		if !held || vol.Spec.Retains() || vol.Status.Phase == v1.VolumeDeleting || len(c.attachedToLocked(a.Volume)) > 0 {
			continue
		}
		vol.Status.Phase = v1.VolumeDeleting
		vol.Status.UpdatedAt = time.Now().UTC()
		if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeStatus, Object: vol}); err != nil {
			c.log.WarnContext(ctx, "the volume was not marked for deletion with its last sandbox",
				"volume", a.Volume, "err", err)
			continue
		}
		woken = true
	}
	if woken {
		c.wakeVolumes()
	}
}

// RunVolumes ticks every volume interval until ctx ends, and at once when a
// volume is created or marked for deletion. Each pass runs only while the
// volumes lease is held.
func (c *Controller) RunVolumes(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.volumeInterval)
	defer stop()
	defer c.fillWork.Wait()
	c.volumeTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-c.volumeWake:
		}
		c.volumeTick(ctx)
	}
}

func (c *Controller) volumeTick(ctx context.Context) {
	if c.volumes == nil {
		return
	}
	held, err := c.lease.Acquire(ctx, VolumesLease, LeaseTTL)
	c.metrics.LeaseHeld(MetricLeaseVolumes, err == nil && held)
	if err != nil {
		c.log.WarnContext(ctx, "volumes lease unavailable", "lease", VolumesLease, "err", err)
		return
	}
	if held {
		c.FillVolumes(ctx)
	}
}

// FillVolumes is one pass of the volume loop: every volume that is Pending or
// Deleting, and is not already in the driver's hands, is handed to it in a
// goroutine of its own, so one slow fill holds up no other.
func (c *Controller) FillVolumes(ctx context.Context) {
	c.mu.Lock()
	var due []v1.Volume
	for id, obj := range c.volumeObjects {
		if _, busy := c.filling[id]; busy {
			continue
		}
		if obj.Status.Phase == v1.VolumePending || obj.Status.Phase == v1.VolumeDeleting {
			c.filling[id] = struct{}{}
			due = append(due, manifest.CloneVolume(obj))
		}
	}
	c.mu.Unlock()
	for _, obj := range due {
		c.fillWork.Go(func() { c.fill(ctx, obj) })
	}
}

// fill is one volume's turn with its driver, with the controller's lock
// released: a delete that was left to the loop is finished, and a Pending
// volume is created, which is idempotent, and read. What the driver reports
// is settled against the row as it stands afterwards.
func (c *Controller) fill(ctx context.Context, obj v1.Volume) {
	id := obj.Status.ID
	defer func() {
		c.mu.Lock()
		delete(c.filling, id)
		c.mu.Unlock()
	}()
	vd, err := c.volumeDriver(obj.Spec.Environment)
	if err != nil {
		c.log.WarnContext(ctx, "the volume's environment keeps no volumes", "volume", id, "err", err)
		return
	}
	if obj.Status.Phase == v1.VolumeDeleting {
		c.mu.Lock()
		defer c.mu.Unlock()
		if cur, held := c.volumeObjects[id]; held && cur.Status.Phase == v1.VolumeDeleting {
			if err := c.finishVolumeDelete(context.WithoutCancel(ctx), cur); err != nil {
				c.log.WarnContext(ctx, "the volume's delete did not finish", "volume", id, "err", err)
			}
		}
		return
	}
	state, err := c.applySource(ctx, vd, obj)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		c.log.WarnContext(ctx, "the volume's fill was not read", "volume", id, "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settleVolume(context.WithoutCancel(ctx), vd, id, state)
}

// applySource asks the driver to create and fill one volume and reads what
// came of it. A fill the driver failed before it returned, and a source it
// cannot apply, are a Failed state with the reason; a volume the driver
// already holds is read like one it just made.
func (c *Controller) applySource(ctx context.Context, vd driver.VolumeDriver, obj v1.Volume) (driver.VolumeState, error) {
	_, err := vd.CreateVolume(ctx, volumeSpecOf(obj))
	var failed *driver.FillError
	switch {
	case err == nil, errors.Is(err, driver.ErrAlreadyExists):
	case errors.As(err, &failed):
		return driver.VolumeState{ID: obj.Status.ID, Phase: v1.VolumeFailed, Reason: failed.Reason, Message: failed.Message}, nil
	case errors.Is(err, driver.ErrUnsupported):
		return driver.VolumeState{ID: obj.Status.ID, Phase: v1.VolumeFailed, Reason: driver.ReasonSourceUnsupported, Message: err.Error()}, nil
	default:
		return driver.VolumeState{}, err
	}
	state, err := vd.InspectVolume(ctx, obj.Status.ID)
	if errors.Is(err, driver.ErrNotFound) {
		// The driver took the create and does not report the volume yet;
		// the next pass reads it again.
		return driver.VolumeState{ID: obj.Status.ID, Phase: v1.VolumePending}, nil
	}
	return state, err
}

// settleVolume writes what one fill ended with. A volume deleted while its
// fill ran has what the fill made removed now; one that is no longer Pending
// was settled by someone else. The caller's mutex is held.
func (c *Controller) settleVolume(ctx context.Context, vd driver.VolumeDriver, id string, state driver.VolumeState) {
	cur, held := c.volumeObjects[id]
	if !held || cur.Status.Phase == v1.VolumeDeleting {
		if !held {
			if err := vd.DeleteVolume(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
				c.log.WarnContext(ctx, "a deleted volume's fill left an object behind", "volume", id, "err", err)
			}
			return
		}
		if err := c.finishVolumeDelete(ctx, cur); err != nil {
			c.log.WarnContext(ctx, "the volume's delete did not finish", "volume", id, "err", err)
		}
		return
	}
	if cur.Status.Phase != v1.VolumePending {
		return
	}
	now := time.Now().UTC()
	switch state.Phase {
	case v1.VolumeAvailable:
		cur.Status.Phase = v1.VolumeAvailable
		cur.Status.Capacity = v1.Quantity(state.Capacity)
		if cur.Status.Capacity == "" {
			cur.Status.Capacity = cur.Spec.Size
		}
		cur.Status.UpdatedAt = now
		if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeStatus, Object: cur}); err != nil {
			c.log.WarnContext(ctx, "the filled volume was not written", "volume", id, "err", err)
			return
		}
	case v1.VolumeFailed:
		cur.Status.Phase = v1.VolumeFailed
		cur.Status.Reason = state.Reason
		cur.Status.Message = state.Message
		cur.Status.UpdatedAt = now
		if err := c.persistVolume(ctx, VolumeAct{Type: MutationVolumeFailed, Object: cur}); err != nil {
			c.log.WarnContext(ctx, "the failed volume was not written", "volume", id, "err", err)
			return
		}
	default:
		return
	}
	// A sandbox waiting for this volume is placed and waits on the
	// scheduler loop, which takes it again now rather than at its tick.
	c.wakeScheduler()
}

// observeVolumes is the reaper's half of spec 019's Lost rule: an Available
// volume its driver no longer holds is Lost, and never recreated, because
// its content is gone with it.
func (c *Controller) observeVolumes(ctx context.Context) error {
	if c.volumes == nil {
		return nil
	}
	c.mu.Lock()
	var available []v1.Volume
	for _, obj := range c.volumeObjects {
		if obj.Status.Phase == v1.VolumeAvailable {
			available = append(available, obj)
		}
	}
	c.mu.Unlock()
	var failed error
	for _, obj := range available {
		vd, err := c.volumeDriver(obj.Spec.Environment)
		if err != nil {
			continue
		}
		if _, err := vd.InspectVolume(ctx, obj.Status.ID); !errors.Is(err, driver.ErrNotFound) {
			failed = errors.Join(failed, err)
			continue
		}
		c.mu.Lock()
		if cur, held := c.volumeObjects[obj.Status.ID]; held && cur.Status.Phase == v1.VolumeAvailable {
			cur.Status.Phase = v1.VolumeLost
			cur.Status.UpdatedAt = time.Now().UTC()
			failed = errors.Join(failed, c.persistVolume(ctx, VolumeAct{Type: MutationVolumeStatus, Object: cur}))
		}
		c.mu.Unlock()
	}
	return failed
}

// volumeSpecOf is the driver's half of one volume.
func volumeSpecOf(obj v1.Volume) driver.VolumeSpec {
	return driver.VolumeSpec{
		ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner,
		Size: string(obj.Spec.Size), Access: obj.Spec.Access,
		Source: driver.VolumeSource{Kind: obj.Spec.Source.Kind, Image: obj.Spec.Source.Image},
	}
}

// persistVolume records one act on a Volume. The store is written first, so a
// write that fails leaves this process's collection as it was. The caller's
// mutex is held.
func (c *Controller) persistVolume(ctx context.Context, act VolumeAct) error {
	act.Object.Status.AttachedTo = nil
	if err := c.volumes.WriteVolume(ctx, act); err != nil {
		return err
	}
	c.volumeObjects[act.Object.Status.ID] = manifest.CloneVolume(act.Object)
	// A durable store journaled the record inside the write above. A
	// snapshot store has no journal, so the emitter takes the act here.
	if c.durable == nil && c.events != nil {
		act.Object = c.volumeRead(act.Object.Status.ID)
		c.events.EmitVolume(ctx, act)
	}
	return nil
}

// removeVolume drops one Volume and records the act. The caller's mutex is
// held.
func (c *Controller) removeVolume(ctx context.Context, act VolumeAct) error {
	act.Object.Status.AttachedTo = nil
	if err := c.volumes.RemoveVolume(ctx, act); err != nil {
		return err
	}
	delete(c.volumeObjects, act.Object.Status.ID)
	if c.durable == nil && c.events != nil {
		c.events.EmitVolume(ctx, act)
	}
	return nil
}

// wakeVolumes asks the volume loop for a pass now rather than at its tick.
func (c *Controller) wakeVolumes() {
	select {
	case c.volumeWake <- struct{}{}:
	default:
	}
}

// newVolumeID is a Volume's id: the kind's prefix and the same ULID a
// sandbox takes, so every object of this control plane sorts by the instant
// it was made.
func newVolumeID() (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	return v1.VolumeIDPrefix + strings.TrimPrefix(id, "sbx_"), nil
}
