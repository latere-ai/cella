// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// LoadSecrets reads every live Secret and remembers each row's version, so
// the first write of each object is conditional on the row it was read from.
// The objects come back without their values: a value is read by one call and
// this is not it.
func (c *Controlled) LoadSecrets() (map[string]v1.Secret, error) {
	ctx := context.Background()
	secrets := map[string]v1.Secret{}
	versions := map[string]int64{}
	cursor := ""
	for {
		var (
			rows []Object
			next string
		)
		err := c.store.Tx(ctx, func(tx Tx) error {
			var err error
			rows, next, err = tx.Desired().List(ctx, KindSecret, Filter{}, Page{Cursor: cursor})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			obj, err := decodeSecret(row)
			if err != nil {
				return nil, err
			}
			secrets[row.ID] = obj
			versions[row.ID] = row.Version
		}
		if next == "" {
			break
		}
		cursor = next
	}
	c.mu.Lock()
	for id, version := range versions {
		c.versions[id] = version
	}
	c.mu.Unlock()
	return secrets, nil
}

// WriteSecret stores one Secret at the version this process last saw, seals
// the plaintext where one was given, and appends the mutation to the journal,
// all in one transaction. It returns the version the value now holds, which
// is what status.version reports.
//
// A nil plaintext is an update that changed something other than the value:
// the row's ciphertext and its version are left exactly as they were.
func (c *Controlled) WriteSecret(ctx context.Context, obj v1.Secret, plaintext []byte, mutation string) (int, error) {
	c.mu.Lock()
	version := c.versions[obj.Status.ID]
	c.mu.Unlock()
	var (
		written int64
		value   = obj.Status.Version
	)
	err := c.store.Tx(ctx, func(tx Tx) error {
		if len(plaintext) > 0 {
			next, err := tx.Values().Put(ctx, obj.Status.ID, plaintext)
			if err != nil {
				return err
			}
			value = next
		}
		stored := obj
		stored.Status.Version = value
		row, err := encodeSecret(stored)
		if err != nil {
			return err
		}
		event, err := c.secretRecord(ctx, mutation, stored)
		if err != nil {
			return err
		}
		if written, err = tx.Desired().Put(ctx, row, version); err != nil {
			return err
		}
		_, err = tx.Journal().Append(ctx, event)
		return err
	})
	if err != nil {
		// One sentinel reaches the API whichever store is underneath: a
		// value written to a control plane with no key is the same refusal
		// on the snapshot store and on this one.
		if errors.Is(err, ErrNoSecretKey) {
			return 0, errors.Join(controller.ErrNoSecretKey, err)
		}
		return 0, err
	}
	c.mu.Lock()
	c.versions[obj.Status.ID] = written
	c.mu.Unlock()
	return value, nil
}

// RemoveSecret deletes one Secret, its value and its version, and appends the
// mutation, in one transaction. A row another replica already deleted is not
// an error: the object is gone either way, and the journal still records that
// this replica ended it.
func (c *Controlled) RemoveSecret(ctx context.Context, id, mutation string) error {
	err := c.store.Tx(ctx, func(tx Tx) error {
		obj := v1.Secret{Status: v1.SecretStatus{ID: id}}
		if row, err := tx.Desired().Get(ctx, KindSecret, id); err == nil {
			if decoded, err := decodeSecret(row); err == nil {
				obj = decoded
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		event, err := c.secretRecord(ctx, mutation, obj)
		if err != nil {
			return err
		}
		if err := tx.Desired().Delete(ctx, KindSecret, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := tx.Values().Delete(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = tx.Journal().Append(ctx, event)
		return err
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.versions, id)
	c.mu.Unlock()
	return nil
}

// OpenValue is the one call in this repository that returns a secret's
// plaintext, and it has one caller: the controller's view builder, which
// hands what it reads straight to egress.Compile and to nothing else.
// TestValuesAreConfined holds both edges.
func (c *Controlled) OpenValue(ctx context.Context, secretID string) ([]byte, int, error) {
	var (
		plaintext []byte
		version   int
	)
	err := c.store.Tx(ctx, func(tx Tx) error {
		var err error
		plaintext, version, err = tx.Values().Open(ctx, secretID)
		return err
	})
	return plaintext, version, err
}

// Rewrap moves every wrapped data key from one key to the next and reports
// how many rows moved. It is the key rotation of design 010: no value's
// ciphertext is read, and the store reads under the new key when it returns.
func (c *Controlled) Rewrap(ctx context.Context, oldKEK, newKEK []byte) (int, error) {
	var n int
	err := c.store.Tx(ctx, func(tx Tx) error {
		var err error
		n, err = tx.Values().Rewrap(ctx, oldKEK, newKEK)
		return err
	})
	return n, err
}

// secretRecord is the journal row for one act on a Secret: design 009's
// record, built where the row and the state it explains commit together.
func (c *Controlled) secretRecord(ctx context.Context, mutation string, obj v1.Secret) (Event, error) {
	rec, err := events.SecretMutation(events.Type(mutation), obj, events.ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		return Event{}, err
	}
	return journalRow(rec, c.delivery)
}

// encodeSecret turns one Secret into a row. The value is blanked first: what
// the objects table holds is the object a read returns, and the one column
// that holds a value is the sealed one in secret_values.
func encodeSecret(obj v1.Secret) (Object, error) {
	status := obj.Status
	obj.Status = v1.SecretStatus{}
	obj.Spec.Value = ""
	data, err := json.Marshal(obj)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the secret: %w", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the status: %w", err)
	}
	return Object{
		Kind: KindSecret, ID: status.ID, Owner: status.Owner, Name: obj.Metadata.Name,
		Labels: obj.Metadata.Labels, Data: data, Status: encoded,
	}, nil
}

// decodeSecret reverses encodeSecret.
func decodeSecret(row Object) (v1.Secret, error) {
	var obj v1.Secret
	if err := json.Unmarshal(row.Data, &obj); err != nil {
		return v1.Secret{}, fmt.Errorf("store: decoding the secret %s: %w", row.ID, err)
	}
	if len(row.Status) > 0 {
		if err := json.Unmarshal(row.Status, &obj.Status); err != nil {
			return v1.Secret{}, fmt.Errorf("store: decoding the status of %s: %w", row.ID, err)
		}
	}
	return obj, nil
}
