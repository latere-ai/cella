// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"time"
)

// EnvironmentKeys is the mint and the revocation of the credential a data
// plane carries: the worker of spec 021 and the gateway of spec 018 each hold
// one, and the API's two key routes are these two methods.
//
// An environment holds several keys, so one is revoked without ending the
// others, and each is shown once: the control plane signs it and keeps no
// copy, which is why a lost key is replaced rather than retrieved.
type EnvironmentKeys struct {
	signer *Signer
	log    RevocationLog
	ttl    time.Duration
}

// NewEnvironmentKeys builds the key half over the signer of spec 006 and the
// revocation list of spec 010. The list is optional: without one a key cannot
// be ended before its exp, which is the bound a deployment with no store has
// anyway.
func NewEnvironmentKeys(signer *Signer, log RevocationLog, ttl time.Duration) (*EnvironmentKeys, error) {
	if signer == nil {
		return nil, errors.New("an environment key is signed by CELLA_TOKEN_KEY, and no signer was given")
	}
	if ttl <= 0 {
		return nil, errors.New("CELLA_ENVIRONMENT_KEY_TTL is the lifetime of every environment key, and it is positive")
	}
	return &EnvironmentKeys{signer: signer, log: log, ttl: ttl}, nil
}

// Mint signs one key for one environment and reports the jti a revocation is
// keyed by. The value is returned once and never stored.
func (e *EnvironmentKeys) Mint(_ context.Context, environment string) (Token, error) {
	return e.signer.MintEnvironmentKey(environment, e.ttl)
}

// Revoke refuses one key from now until its exp passes. The exp written is
// this instant plus the key lifetime, because the control plane kept no copy
// of the key and so does not know when that one expires; a row that outlives
// the key it ends is a row the sweep drops a little late and never one that
// lets a revoked key back in.
func (e *EnvironmentKeys) Revoke(ctx context.Context, jti string) error {
	if jti == "" {
		return errors.New("a revocation names the jti the mint returned")
	}
	if e.log == nil {
		return nil
	}
	return e.log.Revoke(ctx, jti, time.Now().Add(e.ttl))
}

// TTL is how long a key this mint signs lives, which a caller reports beside
// the value it was given.
func (e *EnvironmentKeys) TTL() time.Duration { return e.ttl }
