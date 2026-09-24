// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"time"
)

// KeyRecord is what the control plane keeps of one environment key it minted,
// and never the token: the jti a revocation names, the environment, the
// subject that minted it and when, when it expires, and when it was revoked,
// zero while it is live.
type KeyRecord struct {
	JTI         string
	Environment string
	Subject     string
	MintedAt    time.Time
	ExpiresAt   time.Time
	RevokedAt   time.Time
}

// KeyLog is the registry of the keys each environment holds (spec 021). It is
// kept in the store the revocation list is in, so a revocation marks the key's
// record and refuses its jti in one transaction, and a listing never shows a
// key live that the verifier refuses.
type KeyLog interface {
	// Record keeps one minted key.
	Record(ctx context.Context, k KeyRecord) error
	// Revoke marks the key revoked at the instant given and refuses its jti
	// until exp passes. A jti with no record is refused all the same.
	Revoke(ctx context.Context, jti string, at, exp time.Time) error
	// List is one page of an environment's keys in the order they were
	// minted, and the cursor of the next page.
	List(ctx context.Context, environment, cursor string, limit int) ([]KeyRecord, string, error)
}

// EnvironmentKeys is the mint, the revocation and the listing of the
// credential a data plane carries: the worker of spec 021 and the gateway of
// spec 018 each hold one, and the API's three key routes are these three
// methods.
//
// An environment holds several keys, so one is revoked without ending the
// others, and each is shown once: the control plane signs it and keeps a
// record of it but no copy, which is why a lost key is replaced rather than
// retrieved.
type EnvironmentKeys struct {
	signer *Signer
	log    RevocationLog
	keys   KeyLog
	ttl    time.Duration
}

// NewEnvironmentKeys builds the key half over the signer of spec 006, the
// revocation list of spec 010, and the key registry. The list and the
// registry are optional: without the list a key cannot be ended before its
// exp, which is the bound a deployment with no store has anyway, and without
// the registry nothing is listed. Where the registry is given, it is the one
// that writes a revocation, in the same transaction as its own mark.
func NewEnvironmentKeys(signer *Signer, log RevocationLog, keys KeyLog, ttl time.Duration) (*EnvironmentKeys, error) {
	if signer == nil {
		return nil, errors.New("an environment key is signed by CELLA_TOKEN_KEY, and no signer was given")
	}
	if ttl <= 0 {
		return nil, errors.New("CELLA_ENVIRONMENT_KEY_TTL is the lifetime of every environment key, and it is positive")
	}
	return &EnvironmentKeys{signer: signer, log: log, keys: keys, ttl: ttl}, nil
}

// Mint signs one key for one environment on behalf of the subject that asked,
// records it, and reports the jti a revocation is keyed by. The value is
// returned once and never stored. A key the registry could not record is not
// handed out, so every key a caller holds is one a listing shows.
func (e *EnvironmentKeys) Mint(ctx context.Context, environment, subject string) (Token, error) {
	token, err := e.signer.MintEnvironmentKey(environment, e.ttl)
	if err != nil {
		return Token{}, err
	}
	if e.keys == nil {
		return token, nil
	}
	err = e.keys.Record(ctx, KeyRecord{
		JTI: token.JTI, Environment: environment, Subject: subject,
		MintedAt: token.IssuedAt, ExpiresAt: token.ExpiresAt,
	})
	if err != nil {
		return Token{}, err
	}
	return token, nil
}

// Revoke refuses one key from now until its exp passes. The exp written is
// this instant plus the key lifetime, because a key minted before its record
// was kept has no exp the control plane knows; a row that outlives the key it
// ends is a row the sweep drops a little late and never one that lets a
// revoked key back in.
func (e *EnvironmentKeys) Revoke(ctx context.Context, jti string) error {
	if jti == "" {
		return errors.New("a revocation names the jti the mint returned")
	}
	now := time.Now()
	if e.keys != nil {
		return e.keys.Revoke(ctx, jti, now, now.Add(e.ttl))
	}
	if e.log == nil {
		return nil
	}
	return e.log.Revoke(ctx, jti, now.Add(e.ttl))
}

// List is one page of an environment's keys, oldest first. A control plane
// with no registry recorded none and lists none.
func (e *EnvironmentKeys) List(ctx context.Context, environment, cursor string, limit int) ([]KeyRecord, string, error) {
	if e.keys == nil {
		return nil, "", nil
	}
	return e.keys.List(ctx, environment, cursor, limit)
}

// TTL is how long a key this mint signs lives, which a caller reports beside
// the value it was given.
func (e *EnvironmentKeys) TTL() time.Duration { return e.ttl }
