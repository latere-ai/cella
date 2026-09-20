// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
)

// KeySize is the length of the key CELLA_SECRET_KEY carries and of every
// data key: AES-256 takes 32 bytes.
const KeySize = 32

// Envelope seals a secret value under a data key of its own and that data key
// under the store's key, both with AES-256-GCM.
//
// The two ciphertexts are stored in two columns, so rotating the store's key
// rewrites the wrapped data keys and leaves every value's ciphertext byte
// untouched. The zero Envelope has no key and seals nothing.
//
// The key sits behind a pointer every copy of one Envelope shares, so Adopt
// reaches the copies a store handed to its transactions: a rotation that
// rewrote every row must be the key the next Open reads with, or the process
// would serve nothing until it restarted.
type Envelope struct {
	keys *envelopeKeys
}

type envelopeKeys struct {
	mu  sync.RWMutex
	kek cipher.AEAD
}

// ParseKey reads the store's key from configuration: 32 bytes, base64. Both
// the standard and the URL alphabets are accepted, with or without padding,
// because an operator pastes what their secret manager printed.
func ParseKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("the key is empty")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		key, err := enc.DecodeString(raw)
		if err != nil {
			continue
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("the key is %d bytes, not %d", len(key), KeySize)
		}
		return key, nil
	}
	return nil, fmt.Errorf("the key is not base64 of %d bytes", KeySize)
}

// NewEnvelope takes the store's key. A nil key is the zero Envelope, which
// reports ErrNoSecretKey from every operation: a store opened without a key
// serves everything but secret values.
func NewEnvelope(key []byte) (Envelope, error) {
	if len(key) == 0 {
		return Envelope{}, nil
	}
	if len(key) != KeySize {
		return Envelope{}, fmt.Errorf("store: the key is %d bytes, not %d", len(key), KeySize)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{keys: &envelopeKeys{kek: aead}}, nil
}

// Ready reports whether this envelope can seal.
func (e Envelope) Ready() bool { return e.kek() != nil }

// kek is the key this envelope holds now, which Adopt may have replaced.
func (e Envelope) kek() cipher.AEAD {
	if e.keys == nil {
		return nil
	}
	e.keys.mu.RLock()
	defer e.keys.mu.RUnlock()
	return e.keys.kek
}

// Adopt takes another envelope's key as this one's, for every copy that
// shares this envelope's state. It is the last step of a rotation: the rows
// have been rewritten under next, so the store reads under next from here on.
func (e Envelope) Adopt(next Envelope) {
	if e.keys == nil || next.keys == nil {
		return
	}
	adopted := next.kek()
	e.keys.mu.Lock()
	defer e.keys.mu.Unlock()
	e.keys.kek = adopted
}

// Wrap seals one data key under this envelope's key, and Unwrap opens one.
// The pair is what a rotation runs over: the value's own ciphertext is never
// read, so no plaintext exists at any point of one.
func (e Envelope) Wrap(dataKey []byte) ([]byte, error) {
	kek := e.kek()
	if kek == nil {
		return nil, ErrNoSecretKey
	}
	return box(kek, dataKey)
}

func (e Envelope) Unwrap(wrapped []byte) ([]byte, error) {
	kek := e.kek()
	if kek == nil {
		return nil, ErrNoSecretKey
	}
	dataKey, err := unbox(kek, wrapped)
	if err != nil {
		return nil, fmt.Errorf("store: unwrapping the data key: %w", err)
	}
	return dataKey, nil
}

// Seal returns the wrapped data key and the sealed value. Each is its nonce
// followed by the ciphertext, so one column holds one self-contained box.
func (e Envelope) Seal(plaintext []byte) (wrapped, sealed []byte, err error) {
	kek := e.kek()
	if kek == nil {
		return nil, nil, ErrNoSecretKey
	}
	dataKey := make([]byte, KeySize)
	if _, err = rand.Read(dataKey); err != nil {
		return nil, nil, fmt.Errorf("store: reading a data key: %w", err)
	}
	value, err := newAEAD(dataKey)
	if err != nil {
		return nil, nil, err
	}
	sealed, err = box(value, plaintext)
	if err != nil {
		return nil, nil, err
	}
	wrapped, err = box(kek, dataKey)
	if err != nil {
		return nil, nil, err
	}
	return wrapped, sealed, nil
}

// Open unwraps the data key and returns the value. A key that did not seal
// this row fails here, which is what a wrong CELLA_SECRET_KEY looks like.
func (e Envelope) Open(wrapped, sealed []byte) ([]byte, error) {
	dataKey, err := e.Unwrap(wrapped)
	if err != nil {
		return nil, err
	}
	value, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := unbox(value, sealed)
	if err != nil {
		return nil, fmt.Errorf("store: opening the value: %w", err)
	}
	return plaintext, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: the key is not an AES key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: GCM over the key: %w", err)
	}
	return aead, nil
}

// box seals plaintext as nonce || ciphertext.
func box(aead cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: reading a nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// unbox reverses box.
func unbox(aead cipher.AEAD, sealed []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("the sealed value is shorter than its nonce")
	}
	return aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// RewrapKeys validates the two keys a rotation runs between. The old key must
// be the one the rows were sealed under, which is proved by the first unwrap
// rather than by comparing bytes; both are held to the length an AES-256 key
// has before a single row is read.
func RewrapKeys(oldKEK, newKEK []byte) (old, next Envelope, err error) {
	if old, err = NewEnvelope(oldKEK); err != nil {
		return Envelope{}, Envelope{}, err
	}
	if next, err = NewEnvelope(newKEK); err != nil {
		return Envelope{}, Envelope{}, err
	}
	if !old.Ready() || !next.Ready() {
		return Envelope{}, Envelope{}, ErrNoSecretKey
	}
	return old, next, nil
}

// RewrapKey moves one row's wrapped data key from one envelope to the other.
// The value's own ciphertext is not a parameter, because a rotation never
// touches it.
func RewrapKey(old, next Envelope, wrapped []byte) ([]byte, error) {
	dataKey, err := old.Unwrap(wrapped)
	if err != nil {
		return nil, err
	}
	return next.Wrap(dataKey)
}
