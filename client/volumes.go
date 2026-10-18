// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"

	v1 "latere.ai/x/cella/manifest/v1"
)

// ApplyVolume applies a Volume manifest by name: a create when the name is
// free, answered Pending while the volume is filled, and an update when the
// caller holds it. An image source comes back as the digest-pinned reference
// the server admitted, which is what a caller compares to verify the
// volume's content.
func (c *Client) ApplyVolume(ctx context.Context, name string, m Manifest) (v1.Volume, []byte, error) {
	raw, err := c.send(ctx, http.MethodPut, KindVolume.item(name), nil, m.Body, m.mediaType())
	return decodeInto[v1.Volume](raw, err)
}

// GetVolume reads one Volume by id or by name. Its phase is Available once
// it is filled, and status.attachedTo names every sandbox that mounts it.
func (c *Client) GetVolume(ctx context.Context, ref string) (v1.Volume, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindVolume.item(ref), nil, nil, "")
	return decodeInto[v1.Volume](raw, err)
}

// ListVolumes follows the cursor to the end, or to Limit objects, and returns
// the decoded objects with their own bytes, in order. Owner and Labels
// narrow the list.
func (c *Client) ListVolumes(ctx context.Context, o ListOptions) ([]v1.Volume, []json.RawMessage, error) {
	return listAll[v1.Volume](ctx, c, KindVolume, o)
}
