// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"latere.ai/x/pkg/requestid"
)

// RequestIDHeader is the header design 008 carries the request id in,
// inbound from a client and outbound on every answer.
const RequestIDHeader = requestid.Header

// requestID is the id one response carries: the client's own where it is
// within design 008's rule (one to 128 visible ASCII characters, a space not
// among them, so nothing a caller sends can grow or break an envelope, a log
// line or a span attribute), and a minted `req_` id otherwise. The id is the
// envelope's request_id, the authorizer's request.id, the admission step's,
// the event's and the trace's correlation, so it is decided once, at the
// door, and every one of those reads the header this sets.
func requestID(r *http.Request) string { return requestid.FromRequest(r) }
