// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// RevocationLog is the whole of the list: the question the verifier asks,
// the write the controller makes when it replaces or ends a token, and the
// sweep that drops a row no live token could present.
type RevocationLog interface {
	Revocations
	Revoke(ctx context.Context, jti string, exp time.Time) error
	Forget(ctx context.Context, before time.Time) (int, error)
}

// WorkloadTokens is the identity half of the controller's create order and
// of its reaper: the signer of spec 006 and the revocation list of spec 010,
// behind the two methods the controller declares.
type WorkloadTokens struct {
	signer *Signer
	log    RevocationLog
}

// NewWorkloadTokens builds the controller's Tokens. The list is optional:
// without one a token cannot be ended before its exp, which is the bound a
// deployment with no store has anyway.
func NewWorkloadTokens(signer *Signer, log RevocationLog) (*WorkloadTokens, error) {
	if signer == nil {
		return nil, errors.New("a workload token is signed by CELLA_TOKEN_KEY, and no signer was given")
	}
	return &WorkloadTokens{signer: signer, log: log}, nil
}

// Mint signs one sandbox's identity, capped by the sandbox's own expiry, and
// reports the jti a revocation is keyed by.
func (w *WorkloadTokens) Mint(_ context.Context, obj v1.Sandbox) (token, jti string, exp time.Time, err error) {
	minted, err := w.signer.MintWorkload(Workload{
		Sandbox:     obj.Status.ID,
		Environment: obj.Status.Environment,
		ExpiresAt:   obj.Status.ExpiresAt,
		// The spawn budget rides on the claim from desired state, which
		// spec 022 fills; until it does, a workload token carries no
		// budget rather than a budget of zero.
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	return minted.Value, minted.JTI, minted.ExpiresAt, nil
}

// Revoke ends one token before its exp. Without a list there is nothing to
// write it to and the token lives to its exp, which the caller cannot fix and
// is not an error of its act.
func (w *WorkloadTokens) Revoke(ctx context.Context, jti string, exp time.Time) error {
	if w.log == nil {
		return nil
	}
	return w.log.Revoke(ctx, jti, exp)
}

// Forget is the sweep the reaper's tick runs: a revocation outlives the token
// it ends and nothing more.
func (w *WorkloadTokens) Forget(ctx context.Context, before time.Time) (int, error) {
	if w.log == nil {
		return 0, nil
	}
	return w.log.Forget(ctx, before)
}
