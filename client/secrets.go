// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"

	v1 "latere.ai/x/cella/manifest/v1"
)

// CreateSecret creates a Secret from a manifest that names it. No answer
// carries its value.
func (c *Client) CreateSecret(ctx context.Context, m Manifest) (v1.Secret, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindSecret.Path(), nil, m.Body, m.mediaType())
	return decodeInto[v1.Secret](raw, err)
}

// ApplySecret applies a Secret manifest by name: a create when the name is
// free and an update when the caller holds it.
func (c *Client) ApplySecret(ctx context.Context, name string, m Manifest) (v1.Secret, []byte, error) {
	raw, err := c.send(ctx, http.MethodPut, KindSecret.item(name), nil, m.Body, m.mediaType())
	return decodeInto[v1.Secret](raw, err)
}

// GetSecret reads one Secret by id or by name. No answer carries its value.
func (c *Client) GetSecret(ctx context.Context, ref string) (v1.Secret, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindSecret.item(ref), nil, nil, "")
	return decodeInto[v1.Secret](raw, err)
}

// ListSecrets follows the cursor to the end, or to Limit objects, and returns
// the decoded objects with their own bytes, in order. Owner and Labels
// narrow the list; a secret has no phase and no environment, so the server
// ignores those two selectors.
func (c *Client) ListSecrets(ctx context.Context, o ListOptions) ([]v1.Secret, []json.RawMessage, error) {
	return listAll[v1.Secret](ctx, c, KindSecret, o)
}
