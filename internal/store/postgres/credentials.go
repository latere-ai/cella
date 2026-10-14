// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/cella/internal/store"
)

// A sandbox row carries two values that are credentials: the values of the
// sandbox's environment, at spec.env of its data, and the credential both
// gateway doors authenticate, at egressState.credential of its status. With a
// key, a write takes both out of the row and seals them together in the row's
// sealed columns, under the envelope secret values use, and a read puts them
// back, so a caller of Desired reads the object it wrote and a reader of the
// table reads neither. Without a key a row keeps them where they are, as every
// release before the columns did.
//
// A row whose values are sealed holds sealedEnv at spec.env. It is a string
// where every earlier release decodes an object, so such a release refuses the
// row, and its controller fails to open, rather than serving the sandbox with
// no environment and no credential. It is also what says the sealed columns
// are the row's: a row without it is whole as it stands, whatever the columns
// hold, because it was written by a process that does not seal.
const sealedEnv = "sealed"

// The paths of the two values, as the object and the member under it.
const (
	envObject, envMember               = "spec", "env"
	credentialObject, credentialMember = "egressState", "credential"
)

// held is what the sealed columns of one row carry: each value as the JSON
// the row held it as.
type held struct {
	Env        json.RawMessage `json:"env,omitempty"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

func (h held) empty() bool { return h.Env == nil && h.Credential == nil }

// inTheClear selects the live sandbox rows that hold either value in the
// clear: an environment that is an object with members rather than the
// marker, or a credential that is not empty.
const inTheClear = `(jsonb_typeof(data #> '{spec,env}') = 'object' and data #> '{spec,env}' <> '{}'::jsonb)
	or coalesce(status #>> '{egressState,credential}', '') <> ''`

// stripCredentials is the set clause that leaves a row holding neither value,
// in the clear or sealed. A deleted sandbox is never read again, so its row
// keeps neither.
const stripCredentials = `data = case when jsonb_typeof(data) = 'object' then data #- '{spec,env}' else data end,
	status = case when jsonb_typeof(status) = 'object' then status #- '{egressState,credential}' else status end,
	sealed_key = null, sealed = null`

// seal is the row a write stores: for a sandbox, on a store with a key, its
// data and status without the two values and the two sealed; for anything
// else, the object as it was given.
func seal(env store.Envelope, obj store.Object) (data, status, wrapped, sealed []byte, err error) {
	if obj.Kind != store.KindSandbox || !env.Ready() {
		return obj.Data, obj.Status, nil, nil, nil
	}
	data, status, h, err := split(obj.Data, obj.Status)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: separating the credentials of %s: %w", obj.ID, err)
	}
	if h.empty() {
		return data, status, nil, nil, nil
	}
	plaintext, err := json.Marshal(h)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: encoding the credentials of %s: %w", obj.ID, err)
	}
	wrapped, sealed, err = env.Seal(plaintext)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("store: sealing the credentials of %s: %w", obj.ID, err)
	}
	return data, status, wrapped, sealed, nil
}

// unseal puts the sealed values back into a sandbox row read from the table,
// so obj is the object that was written. A row without the marker is left as
// it is. A row with it is opened, and one that cannot be is an error: a
// process without the key, or with another key, cannot serve the sandbox.
func unseal(env store.Envelope, obj *store.Object, wrapped, sealed []byte) error {
	if obj.Kind != store.KindSandbox || !marked(obj.Data) {
		return nil
	}
	if len(sealed) == 0 {
		return fmt.Errorf("store: the sandbox %s is marked sealed and holds no sealed credentials", obj.ID)
	}
	plaintext, err := env.Open(wrapped, sealed)
	if errors.Is(err, store.ErrNoSecretKey) {
		return fmt.Errorf("store: the sandbox %s holds sealed credentials and this store has no CELLA_SECRET_KEY to open them: %w", obj.ID, err)
	}
	if err != nil {
		return fmt.Errorf("store: opening the credentials of %s: %w", obj.ID, err)
	}
	var h held
	if err := json.Unmarshal(plaintext, &h); err != nil {
		return fmt.Errorf("store: decoding the credentials of %s: %w", obj.ID, err)
	}
	data, err := put(obj.Data, envObject, envMember, h.Env)
	if err != nil {
		return fmt.Errorf("store: restoring the environment of %s: %w", obj.ID, err)
	}
	status := obj.Status
	// A credential the status holds in the clear was written after the
	// sealed one, and stands.
	_, inline, err := take(status, credentialObject, credentialMember)
	if err != nil {
		return fmt.Errorf("store: reading the gateway credential of %s: %w", obj.ID, err)
	}
	if inline == nil && h.Credential != nil {
		if status, err = put(status, credentialObject, credentialMember, h.Credential); err != nil {
			return fmt.Errorf("store: restoring the gateway credential of %s: %w", obj.ID, err)
		}
	}
	obj.Data, obj.Status = data, status
	return nil
}

// split takes both values out of a sandbox's data and status and marks the
// data, where either is there to take. Its input is an object as a caller
// wrote it, or as unseal put it back, so data that is already marked is
// refused rather than sealed as an environment.
func split(data, status []byte) ([]byte, []byte, held, error) {
	if marked(data) {
		return nil, nil, held{}, errors.New("the data is already marked sealed")
	}
	data, env, err := take(data, envObject, envMember)
	if err != nil {
		return nil, nil, held{}, err
	}
	status, credential, err := take(status, credentialObject, credentialMember)
	if err != nil {
		return nil, nil, held{}, err
	}
	h := held{Env: env, Credential: credential}
	if h.empty() {
		return data, status, h, nil
	}
	marker, err := json.Marshal(sealedEnv)
	if err != nil {
		return nil, nil, held{}, err
	}
	if data, err = put(data, envObject, envMember, marker); err != nil {
		return nil, nil, held{}, err
	}
	return data, status, h, nil
}

// marked reports whether a sandbox's data names its values as sealed.
func marked(data []byte) bool {
	top, ok := members(data)
	if !ok {
		return false
	}
	spec, ok := members(top[envObject])
	if !ok {
		return false
	}
	var s string
	return json.Unmarshal(spec[envMember], &s) == nil && s == sealedEnv
}

// take removes document.outer.inner where it holds a value and returns the
// document without it and the value. A member that is absent, null, the empty
// string or the empty object is no value, and the document comes back as it
// was.
func take(document []byte, outer, inner string) ([]byte, json.RawMessage, error) {
	top, ok := members(document)
	if !ok {
		return document, nil, nil
	}
	nested, ok := members(top[outer])
	if !ok {
		return document, nil, nil
	}
	value, ok := nested[inner]
	if !ok || blank(value) {
		return document, nil, nil
	}
	delete(nested, inner)
	out, err := replace(top, outer, nested)
	if err != nil {
		return nil, nil, err
	}
	return out, value, nil
}

// put sets document.outer.inner to value, making the document and the object
// where either is missing, and removes the member where value is nil.
func put(document []byte, outer, inner string, value json.RawMessage) ([]byte, error) {
	top, ok := members(document)
	if !ok {
		if !null(document) {
			return nil, errors.New("the document is not an object")
		}
		top = map[string]json.RawMessage{}
	}
	nested, ok := members(top[outer])
	if !ok {
		if !null(top[outer]) {
			return nil, fmt.Errorf("%s is not an object", outer)
		}
		nested = map[string]json.RawMessage{}
	}
	if value == nil {
		if _, present := nested[inner]; !present {
			return document, nil
		}
		delete(nested, inner)
	} else {
		nested[inner] = value
	}
	return replace(top, outer, nested)
}

// replace writes nested back as top.outer and encodes top.
func replace(top map[string]json.RawMessage, outer string, nested map[string]json.RawMessage) ([]byte, error) {
	encoded, err := json.Marshal(nested)
	if err != nil {
		return nil, err
	}
	top[outer] = encoded
	return json.Marshal(top)
}

// members reads a JSON object as its members, and reports false for any other
// document, which holds no member.
func members(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if null(raw) {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// null reports an absent document or the JSON literal null.
func null(raw json.RawMessage) bool {
	var v any
	return len(raw) == 0 || (json.Unmarshal(raw, &v) == nil && v == nil)
}

// blank reports a member that holds no value to seal.
func blank(raw json.RawMessage) bool {
	if null(raw) {
		return true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v := v.(type) {
	case string:
		return v == ""
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// SealCredentials rewrites every sandbox row that holds an environment value
// or a gateway credential in the clear, which every row an earlier release
// wrote does: a live row is sealed, and a deleted one keeps neither value. It
// reports how many rows it rewrote and every row it could not.
//
// It is what a process runs once it is the writer and before its controller
// loads, so the rows are sealed when the release rolls out rather than at
// each sandbox's next write. A rewrite changes no value a reader of the store
// sees and leaves the row's version alone, so the controller's next
// conditional write still matches; it is conditional on the version it read,
// so a row written in between is left to that write, which seals it itself.
// A live row is left as it stands on a store without a key, and the count says
// so by leaving it out; a deleted row needs none.
func (s *Store) SealCredentials(ctx context.Context) (int, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	tag, err := s.pool.Exec(ctx, `update objects set `+stripCredentials+`
		where kind = $1 and deleted_at is not null
			and (data #> '{spec,env}' is not null or status #> '{egressState,credential}' is not null or sealed is not null)`,
		store.KindSandbox)
	if err != nil {
		return 0, fmt.Errorf("store: clearing the credentials of deleted sandboxes: %w", err)
	}
	rewritten := int(tag.RowsAffected())
	if !s.env.Ready() {
		return rewritten, nil
	}
	var failed error
	cursor := ""
	for {
		batch, err := s.unsealed(ctx, cursor)
		if err != nil {
			return rewritten, errors.Join(failed, err)
		}
		for _, row := range batch {
			done, err := s.sealRow(ctx, row)
			if err != nil {
				failed = errors.Join(failed, fmt.Errorf("sandbox %s: %w", row.obj.ID, err))
				continue
			}
			if done {
				rewritten++
			}
		}
		if len(batch) < store.DefaultPageLimit {
			return rewritten, failed
		}
		cursor = batch[len(batch)-1].obj.ID
	}
}

// unsealedRow is one live sandbox row SealCredentials rewrites, as it was read.
type unsealedRow struct {
	obj             store.Object
	wrapped, sealed []byte
}

// unsealed reads one page of the live sandbox rows that hold a value in the
// clear, after cursor in id order.
func (s *Store) unsealed(ctx context.Context, cursor string) ([]unsealedRow, error) {
	rows, err := s.pool.Query(ctx, `select id, version, data, status, sealed_key, sealed from objects
		where kind = $1 and deleted_at is null and id > $2 and (`+inTheClear+`)
		order by id limit $3`, store.KindSandbox, cursor, store.DefaultPageLimit)
	if err != nil {
		return nil, fmt.Errorf("store: listing the sandboxes with credentials in the clear: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (unsealedRow, error) {
		row := unsealedRow{obj: store.Object{Kind: store.KindSandbox}}
		err := r.Scan(&row.obj.ID, &row.obj.Version, &row.obj.Data, &row.obj.Status, &row.wrapped, &row.sealed)
		return row, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: reading the sandboxes with credentials in the clear: %w", err)
	}
	return batch, nil
}

// sealRow rewrites one row read by unsealed, and reports false where the row
// moved since, which leaves it to the write that moved it.
func (s *Store) sealRow(ctx context.Context, row unsealedRow) (bool, error) {
	obj := row.obj
	if err := unseal(s.env, &obj, row.wrapped, row.sealed); err != nil {
		return false, err
	}
	data, status, wrapped, sealed, err := seal(s.env, obj)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `update objects set data = $3, status = $4, sealed_key = $5, sealed = $6
		where id = $1 and version = $2 and deleted_at is null`,
		obj.ID, obj.Version, jsonText(data), jsonText(status), wrapped, sealed)
	if err != nil {
		return false, fmt.Errorf("store: sealing the credentials: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
