// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	v1 "latere.ai/x/cella/manifest/v1"
)

// Events is design 009's emission seam for a store that has no journal of
// its own. The local file snapshot is the one such store: it holds desired
// state and nothing else, so the record has no transaction to commit with
// and the emitter journals it separately.
//
// Where the store is design 010's, this seam is not set. There the record is
// written inside the transaction that writes the state, by the bridge that
// owns both, so a record and the change it explains commit together or not
// at all.
type Events interface {
	// Emit records one act. A record is a note about work and not the work,
	// so nothing is reported back: an emitter logs what it could not
	// journal and the act stands.
	Emit(ctx context.Context, a Act)
}

// Act is one thing the controller did to one sandbox: design 009's type, and
// the object as it stands after the act. The reason is read from the
// object's status, which is where every act writes it.
type Act struct {
	Type   string
	Object v1.Sandbox
}

// emit hands one act to the emitter, where there is one.
func (c *Controller) emit(ctx context.Context, mutation string, obj v1.Sandbox) {
	if c.events == nil {
		return
	}
	c.events.Emit(ctx, Act{Type: mutation, Object: obj})
}
