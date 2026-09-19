// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
)

// Freshness is the window the sink holds t to. It is stated here because the
// verifier below is stated here.
const Freshness = 5 * time.Minute

// verify is the sink's own check, written out rather than imported. The sink
// lives in another repository and this repository depends on none of it, so
// the contract is held by a second implementation of the formula: if the two
// ever disagree, this test fails and the wire is what changed.
//
// It mirrors the receiving side: parse t and every v1 out of the header,
// hold t to the freshness window, and accept when some v1 equals the HMAC of
// "<t>.<body>" under some configured secret, compared in constant time.
func verify(secrets []string, header string, body []byte, now time.Time) error {
	if len(secrets) == 0 {
		return errors.New("no secret is configured")
	}
	var unix string
	var macs []string
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			unix = v
		case "v1":
			macs = append(macs, v)
		}
	}
	if unix == "" || len(macs) == 0 {
		return errors.New("the header carries no t and v1")
	}
	seconds, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return fmt.Errorf("t is %q, not unix seconds", unix)
	}
	at := time.Unix(seconds, 0)
	if delta := now.Sub(at); delta > Freshness || delta < -Freshness {
		return fmt.Errorf("t is %s away from this clock", delta)
	}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		if _, err := fmt.Fprintf(mac, "%d.", at.Unix()); err != nil {
			return err
		}
		if _, err := mac.Write(body); err != nil {
			return err
		}
		want := mac.Sum(nil)
		for _, candidate := range macs {
			got, err := hex.DecodeString(candidate)
			if err != nil {
				continue
			}
			if hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return errors.New("no v1 verifies over these bytes")
}

var body = []byte(`{"id":"evt_a","seq":1,"type":"sandbox.created"}`)

// TestSignatureVerifies: the header a deployment with one or two secrets
// sends is accepted by a sink holding either half, which is what makes a
// rotation an operation with no outage.
func TestSignatureVerifies(t *testing.T) {
	now := time.Now()
	both := []string{"first-secret", "second-secret"}
	header := events.Header(now, body, both...)
	if strings.Count(header, "v1=") != 2 {
		t.Fatalf("two secrets produced %q", header)
	}
	for _, sink := range [][]string{{"first-secret"}, {"second-secret"}, both} {
		if err := verify(sink, header, body, now); err != nil {
			t.Errorf("a sink holding %v refused the header: %v", sink, err)
		}
	}
	one := events.Header(now, body, "first-secret")
	if err := verify(both, one, body, now); err != nil {
		t.Errorf("a sink holding both refused a core signing with the first: %v", err)
	}
}

// TestSignatureRejects: a changed byte, a foreign secret, and a t outside the
// window are each refused.
func TestSignatureRejects(t *testing.T) {
	now := time.Now()
	header := events.Header(now, body, "first-secret")
	changed := append([]byte(nil), body...)
	changed[len(changed)-2] = 'X'
	for _, tc := range []struct {
		name    string
		secrets []string
		header  string
		body    []byte
		now     time.Time
	}{
		{"one byte of the body changed", []string{"first-secret"}, header, changed, now},
		{"a secret the core does not hold", []string{"other-secret"}, header, body, now},
		{"no secret configured", nil, header, body, now},
		{"t is older than the window", []string{"first-secret"}, header, body, now.Add(2 * Freshness)},
		{"t is ahead of the window", []string{"first-secret"}, header, body, now.Add(-2 * Freshness)},
		{"no t in the header", []string{"first-secret"}, "v1=deadbeef", body, now},
		{"t is not a number", []string{"first-secret"}, "t=noon,v1=deadbeef", body, now},
		{"no v1 in the header", []string{"first-secret"}, fmt.Sprintf("t=%d", now.Unix()), body, now},
	} {
		if err := verify(tc.secrets, tc.header, tc.body, tc.now); err == nil {
			t.Errorf("%s verified", tc.name)
		}
	}
}

// TestSignatureIsFreshOnEveryAttempt: a record held for a day is signed with
// the clock of the attempt that delivers it, so it lands inside the window.
func TestSignatureIsFreshOnEveryAttempt(t *testing.T) {
	made := time.Now().Add(-24 * time.Hour)
	stale := events.Header(made, body, "first-secret")
	if err := verify([]string{"first-secret"}, stale, body, time.Now()); err == nil {
		t.Fatal("a day-old signature passed the freshness window")
	}
	retry := time.Now()
	if err := verify([]string{"first-secret"}, events.Header(retry, body, "first-secret"), body, retry); err != nil {
		t.Fatalf("the re-signed attempt was refused: %v", err)
	}
}

// TestSignatureFormula pins the wire: the value is the hex HMAC-SHA256 of
// "<t>.<body>", and nothing else.
func TestSignatureFormula(t *testing.T) {
	at := time.Unix(1_758_283_200, 0)
	mac := hmac.New(sha256.New, []byte("secret"))
	if _, err := mac.Write([]byte("1758283200." + string(body))); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(mac.Sum(nil))
	if got := events.Signature("secret", at, body); got != want {
		t.Errorf("Signature = %s, want %s", got, want)
	}
	if got := events.Header(at, body, "secret"); got != "t=1758283200,v1="+want {
		t.Errorf("Header = %s", got)
	}
}
