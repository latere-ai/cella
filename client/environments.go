// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// CreateEnvironment creates an Environment from a manifest that names it.
func (c *Client) CreateEnvironment(ctx context.Context, m Manifest) (v1.Environment, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindEnvironment.Path(), nil, m.Body, m.mediaType())
	return decodeInto[v1.Environment](raw, err)
}

// ApplyEnvironment applies an Environment manifest under a name: a create when
// the name is free and an update of the one that holds it.
func (c *Client) ApplyEnvironment(ctx context.Context, name string, m Manifest) (v1.Environment, []byte, error) {
	raw, err := c.send(ctx, http.MethodPut, KindEnvironment.item(name), nil, m.Body, m.mediaType())
	return decodeInto[v1.Environment](raw, err)
}

// GetEnvironment reads one Environment by name. The empty name is the
// environment a manifest that names none is placed on.
func (c *Client) GetEnvironment(ctx context.Context, ref string) (v1.Environment, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindEnvironment.item(ref), nil, nil, "")
	return decodeInto[v1.Environment](raw, err)
}

// ListEnvironments follows the cursor to the end, or to Limit objects, and
// returns the decoded objects with their own bytes, in order. The selectors
// are sent as the sandbox list sends them.
func (c *Client) ListEnvironments(ctx context.Context, o ListOptions) ([]v1.Environment, []json.RawMessage, error) {
	return listAll[v1.Environment](ctx, c, KindEnvironment, o)
}

// EnvironmentKey is one key a worker or a gateway of an environment connects
// with. The token is answered once, at the mint, and never again.
type EnvironmentKey struct {
	// Token is the key itself, which a worker or a gateway presents.
	Token string `json:"token"`
	// JTI names the key, which is what revokes it.
	JTI string `json:"jti"`
	// Expires is when the key stops working on its own.
	Expires time.Time `json:"exp"`
}

// MintEnvironmentKey mints a key for one environment. A control plane that
// signs no keys refuses with capability_unsupported.
func (c *Client) MintEnvironmentKey(ctx context.Context, ref string) (EnvironmentKey, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindEnvironment.item(ref)+"/keys", nil, nil, "")
	return decodeInto[EnvironmentKey](raw, err)
}

// RevokeEnvironmentKey ends one key by the jti its mint answered. Every stream
// and every route reads the revocation, so the key stops working on its next
// frame and its next request.
func (c *Client) RevokeEnvironmentKey(ctx context.Context, ref, jti string) error {
	return c.write(ctx, http.MethodDelete, KindEnvironment.item(ref)+"/keys/"+url.PathEscape(jti), nil, nil, "")
}

// KeyRecord is one environment key as the key list answers it. The token is
// not part of it: the control plane keeps no copy, so a list is not a way to
// recover a key, only to see and revoke one.
type KeyRecord struct {
	// JTI names the key, which is what revokes it.
	JTI string `json:"jti"`
	// MintedAt is when the key was minted.
	MintedAt time.Time `json:"mintedAt"`
	// Expires is when the key stops working on its own.
	Expires time.Time `json:"exp"`
	// Revoked reports whether the key was ended by a revocation.
	Revoked bool `json:"revoked"`
	// RevokedAt is when, for a revoked key.
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	// MintedBy is the subject that minted it, where the server recorded one.
	MintedBy string `json:"mintedBy,omitempty"`
}

// ListEnvironmentKeys follows the cursor to the end and returns the keys the
// environment holds a record of, oldest first. It is an administrator's call,
// as the mint and the revocation are. A key minted before the server kept
// records is not listed and is still revoked by its jti.
func (c *Client) ListEnvironmentKeys(ctx context.Context, ref string) ([]KeyRecord, error) {
	var keys []KeyRecord
	cursor := ""
	for {
		q := url.Values{"limit": {strconv.Itoa(maxPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, err := c.send(ctx, http.MethodGet, KindEnvironment.item(ref)+"/keys", q, nil, "")
		if err != nil {
			return nil, err
		}
		var page struct {
			Items []KeyRecord `json:"items"`
			Next  string      `json:"next"`
		}
		if err = json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("the key list answer is no page: %w", err)
		}
		keys = append(keys, page.Items...)
		if page.Next == "" || len(page.Items) == 0 {
			return keys, nil
		}
		cursor = page.Next
	}
}
