// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SignatureHeader carries the attempt's clock and one signature per
// configured secret. There is no Authorization header on a delivery: the
// signature is the credential, and a second one would be a second secret to
// rotate.
const SignatureHeader = "Cella-Signature"

// Signature is the v1 value for a body at an instant: the hex HMAC-SHA256 of
// "<t>.<body>" under one secret.
func Signature(secret string, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	// A hash's Write never fails, which is what hash.Hash documents.
	_, _ = fmt.Fprintf(mac, "%d.", t.Unix())
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Header is the whole Cella-Signature value: t, then one v1 per secret in
// the order CELLA_EVENTS_SECRET lists them. Two are sent while a secret is
// being rotated, so either end may move first and no record is refused in
// between.
//
// t is the instant of this attempt and not the record's time, and it is
// recomputed on every attempt, so a record deferred for a day still arrives
// inside the sink's freshness window.
func Header(t time.Time, body []byte, secrets ...string) string {
	parts := make([]string, 0, len(secrets)+1)
	parts = append(parts, fmt.Sprintf("t=%d", t.Unix()))
	for _, secret := range secrets {
		parts = append(parts, "v1="+Signature(secret, t, body))
	}
	return strings.Join(parts, ",")
}
