// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/store"
)

// key is 32 bytes, the one length AES-256 takes.
var key = []byte("0123456789abcdef0123456789abcdef")

// TestParseKey reads the key an operator pastes: 32 bytes in whichever base64
// their secret manager printed, and a clear refusal of anything else.
func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []byte
	}{
		{"standard", base64.StdEncoding.EncodeToString(key), key},
		{"standard without padding", base64.RawStdEncoding.EncodeToString(key), key},
		{"url safe", base64.URLEncoding.EncodeToString(key), key},
		{"url safe without padding", base64.RawURLEncoding.EncodeToString(key), key},
		{"empty", "", nil},
		{"not base64", "not a key at all!", nil},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short")), nil},
		{"too long", base64.StdEncoding.EncodeToString(append(key, key...)), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ParseKey(tc.raw)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("%q was accepted as a key", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was refused: %v", tc.raw, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("the key reads back %q", got)
			}
		})
	}
}

// TestEnvelopeSealsAndOpens: a value goes in and comes back, and what is
// stored is neither the value nor the key that wraps it.
func TestEnvelopeSealsAndOpens(t *testing.T) {
	envelope, err := store.NewEnvelope(key)
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Ready() {
		t.Fatal("an envelope with a key reports that it cannot seal")
	}
	secret := []byte("the value nobody but egress reads")
	wrapped, sealed, err := envelope.Seal(secret)
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Error("the sealed value holds the plaintext")
	}
	if bytes.Contains(wrapped, key) || bytes.Contains(sealed, key) {
		t.Error("a ciphertext holds the key that wrapped it")
	}
	plaintext, err := envelope.Open(wrapped, sealed)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if !bytes.Equal(plaintext, secret) {
		t.Fatalf("the value reads back %q", plaintext)
	}
	// Every seal takes its own data key and its own nonce, so two seals of
	// one value are two different ciphertexts.
	againWrapped, againSealed, err := envelope.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, againSealed) || bytes.Equal(wrapped, againWrapped) {
		t.Error("two seals of one value are the same bytes")
	}
}

// TestEnvelopeRefusesTheWrongKey: a store opened under another key reads no
// value, which is what a rotated or mistyped CELLA_SECRET_KEY looks like.
func TestEnvelopeRefusesTheWrongKey(t *testing.T) {
	envelope, err := store.NewEnvelope(key)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, sealed, err := envelope.Seal([]byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.NewEnvelope([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(wrapped, sealed); err == nil {
		t.Fatal("another key opened the value")
	}
	// A value whose ciphertext was cut or changed does not open either: GCM
	// authenticates what it decrypts.
	for _, tc := range []struct {
		name            string
		wrapped, sealed []byte
	}{
		{"a truncated data key", wrapped[:8], sealed},
		{"a truncated value", wrapped, sealed[:8]},
		{"a changed value", wrapped, append(bytes.Clone(sealed[:len(sealed)-1]), sealed[len(sealed)-1]^0xff)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := envelope.Open(tc.wrapped, tc.sealed); err == nil {
				t.Fatal("it opened")
			}
		})
	}
}

// TestEnvelopeWithoutAKey: a store opened with no key serves everything but
// secret values, and says so with one error rather than a nil panic.
func TestEnvelopeWithoutAKey(t *testing.T) {
	envelope, err := store.NewEnvelope(nil)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Ready() {
		t.Fatal("an envelope with no key reports that it can seal")
	}
	if _, _, err := envelope.Seal([]byte("value")); !errors.Is(err, store.ErrNoSecretKey) {
		t.Fatalf("sealing without a key: %v", err)
	}
	if _, err := envelope.Open(nil, nil); !errors.Is(err, store.ErrNoSecretKey) {
		t.Fatalf("opening without a key: %v", err)
	}
	if _, err := store.NewEnvelope([]byte("too short")); err == nil {
		t.Fatal("a nine byte key was accepted")
	}
}

// TestEventID and TestHolder pin the shapes design 001 gives an event id and a
// lease holder: a kind prefix on one, a hostname and enough randomness on the
// other that two replicas on one host are two holders.
func TestEventID(t *testing.T) {
	first, second := store.EventID(), store.EventID()
	if !strings.HasPrefix(first, store.EventPrefix) || len(first) != len(store.EventPrefix)+26 {
		t.Fatalf("the event id is %q", first)
	}
	if first == second {
		t.Fatal("two event ids are the same")
	}
	if strings.ToLower(first) != first {
		t.Fatalf("the event id is not lowercase: %q", first)
	}
}

func TestHolder(t *testing.T) {
	first, second := store.Holder(), store.Holder()
	if first == "" || first == second {
		t.Fatalf("two holders of one host are %q and %q", first, second)
	}
	if !strings.Contains(first, "-") {
		t.Fatalf("the holder does not name its host: %q", first)
	}
}

// TestPageOf: a list hands out a cursor only where there is another page, so
// a caller that follows cursors stops rather than reading an empty page.
func TestPageOf(t *testing.T) {
	rows := []string{"a", "b", "c", "d"}
	same := func(s string) string { return s }
	for _, tc := range []struct {
		name string
		rows []string
		page store.Page
		want []string
		next string
	}{
		{"everything fits", rows, store.Page{Limit: 10}, rows, ""},
		{"exactly the page", rows[:2], store.Page{Limit: 2}, rows[:2], ""},
		{"one more than the page", rows[:3], store.Page{Limit: 2}, rows[:2], "b"},
		{"no limit", rows, store.Page{}, rows, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, next := store.PageOf(tc.rows, tc.page, same)
			if len(got) != len(tc.want) || next != tc.next {
				t.Fatalf("page = %v, %q, want %v, %q", got, next, tc.want, tc.next)
			}
		})
	}
	for _, tc := range []struct {
		page store.Page
		want int
	}{
		{store.Page{}, store.DefaultPageLimit},
		{store.Page{Limit: -1}, store.DefaultPageLimit},
		{store.Page{Limit: 5}, 5},
		{store.Page{Limit: store.MaxPageLimit + 1}, store.MaxPageLimit},
	} {
		if got := tc.page.Size(); got != tc.want {
			t.Errorf("a page of %d reads %d rows, want %d", tc.page.Limit, got, tc.want)
		}
	}
}
