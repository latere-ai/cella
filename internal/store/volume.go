// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// LoadVolumes reads every live Volume and remembers each row's version, so
// the first write of each object is conditional on the row it was read from.
func (c *Controlled) LoadVolumes() (map[string]v1.Volume, error) {
	ctx := context.Background()
	volumes := map[string]v1.Volume{}
	versions := map[string]int64{}
	cursor := ""
	for {
		var (
			rows []Object
			next string
		)
		err := c.store.Tx(ctx, func(tx Tx) error {
			var err error
			rows, next, err = tx.Desired().List(ctx, KindVolume, Filter{}, Page{Cursor: cursor})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			obj, err := decodeVolume(row)
			if err != nil {
				return nil, err
			}
			volumes[row.ID] = obj
			versions[row.ID] = row.Version
		}
		if next == "" {
			break
		}
		cursor = next
	}
	c.mu.Lock()
	maps.Copy(c.versions, versions)
	c.mu.Unlock()
	return volumes, nil
}

// WriteVolume stores one Volume at the version this process last saw and
// appends the act to the journal, in one transaction. An attach or a detach
// writes the row as it stands, so its record commits with the row it is
// about.
func (c *Controlled) WriteVolume(ctx context.Context, act controller.VolumeAct) error {
	id := act.Object.Status.ID
	c.mu.Lock()
	version := c.versions[id]
	c.mu.Unlock()
	row, err := encodeVolume(act.Object)
	if err != nil {
		return err
	}
	event, err := c.volumeRecord(ctx, act)
	if err != nil {
		return err
	}
	var written int64
	if err := c.write(ctx, func(tx Tx) error {
		var err error
		if written, err = tx.Desired().Put(ctx, row, version); err != nil {
			return err
		}
		_, err = tx.Journal().Append(ctx, event)
		return err
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.versions[id] = written
	c.mu.Unlock()
	return nil
}

// RemoveVolume deletes one Volume and appends the act, in one transaction. A
// row another replica already deleted is not an error: the object is gone
// either way, and the journal still records that this replica ended it.
func (c *Controlled) RemoveVolume(ctx context.Context, act controller.VolumeAct) error {
	id := act.Object.Status.ID
	event, err := c.volumeRecord(ctx, act)
	if err != nil {
		return err
	}
	if err := c.write(ctx, func(tx Tx) error {
		if err := tx.Desired().Delete(ctx, KindVolume, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err := tx.Journal().Append(ctx, event)
		return err
	}); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.versions, id)
	c.mu.Unlock()
	return nil
}

// volumeRecord is the journal row for one act on a Volume: design 009's
// record, built where the row and the state it explains commit together.
func (c *Controlled) volumeRecord(ctx context.Context, act controller.VolumeAct) (Event, error) {
	rec, err := events.VolumeMutation(events.Type(act.Type), act.Object, act.Sandbox, act.Mode,
		events.ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		return Event{}, err
	}
	return journalRow(rec, c.delivery)
}

// encodeVolume turns one Volume into a row. What is derived at read,
// attachedTo, is not stored: it is a function of the sandboxes.
func encodeVolume(obj v1.Volume) (Object, error) {
	status := obj.Status
	status.AttachedTo = nil
	obj.Status = v1.VolumeStatus{}
	data, err := json.Marshal(obj)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the volume: %w", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the volume status: %w", err)
	}
	return Object{
		Kind: KindVolume, ID: status.ID, Owner: status.Owner, Name: obj.Metadata.Name,
		Environment: obj.Spec.Environment, Phase: status.Phase, Labels: obj.Metadata.Labels,
		Data: data, Status: encoded,
	}, nil
}

// decodeVolume reverses encodeVolume.
func decodeVolume(row Object) (v1.Volume, error) {
	var obj v1.Volume
	if err := json.Unmarshal(row.Data, &obj); err != nil {
		return v1.Volume{}, fmt.Errorf("store: decoding the volume %s: %w", row.ID, err)
	}
	if len(row.Status) > 0 {
		if err := json.Unmarshal(row.Status, &obj.Status); err != nil {
			return v1.Volume{}, fmt.Errorf("store: decoding the status of %s: %w", row.ID, err)
		}
	}
	return obj, nil
}

// The controller's seam this type satisfies.
var _ controller.Volumes = (*Controlled)(nil)
