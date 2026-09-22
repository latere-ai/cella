// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"latere.ai/x/cella/internal/events"
)

const (
	// RequestIDHeader is the header design 008 carries the request id in,
	// inbound from a client and outbound on every answer.
	RequestIDHeader = "X-Request-Id"
	// RequestIDPrefix is the kind prefix design 001 gives a request id.
	RequestIDPrefix = "req_"
	// maxRequestIDBytes is the longest id design 008 takes from a client.
	// Beyond it the id is replaced, so nothing a caller sends can grow an
	// envelope, a log line or a span attribute without bound.
	maxRequestIDBytes = 128
)

// requestID is the id one response carries: the client's own where it is
// within design 008's rule, and a minted `req_` and ULID otherwise. The id is
// the envelope's request_id, the authorizer's request.id, the admission
// step's, the event's and the trace's correlation, so it is decided once, at
// the door, and every one of those reads the header this sets.
func requestID(r *http.Request) string {
	if given := r.Header.Get(RequestIDHeader); acceptableRequestID(given) {
		return given
	}
	return RequestIDPrefix + events.ULID()
}

// acceptableRequestID is design 008's rule for a client's own id: at most 128
// printable ASCII characters, and at least one. A byte outside that range
// would reach a log line, a trace attribute and an error envelope unescaped.
func acceptableRequestID(given string) bool {
	if given == "" || len(given) > maxRequestIDBytes {
		return false
	}
	for i := range len(given) {
		if given[i] < 0x20 || given[i] > 0x7e {
			return false
		}
	}
	return true
}
