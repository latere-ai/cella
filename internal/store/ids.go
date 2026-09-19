// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/rand"
	"os"
	"strings"
)

// EventPrefix is the kind prefix design 001 gives a journal event.
const EventPrefix = "evt_"

// EventID is the id Append assigns an event that carries none: the prefix and
// 26 random characters. Order within an object is the sequence, not the id.
func EventID() string {
	return EventPrefix + strings.ToLower(rand.Text())
}

// Holder is one lease identity for this process: the hostname and eight
// random characters, so two replicas on one host are two holders and a
// restarted replica never inherits the lease its predecessor held.
func Holder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strings.ToLower(rand.Text())[:8]
}
