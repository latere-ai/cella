// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"latere.ai/x/pkg/httpjson"
)

// Error is one refusal of the API, decoded from the envelope of design 008:
// the code a caller decides on, the fixed user sentence, the request id the
// server stamped, and the paths a field error names. The status is kept
// beside the code because the exit scheme maps an unknown code by its class.
type Error struct {
	Status int
	Code   string
	// Message is the API's sentence for the code, the one line a refusal
	// prints.
	Message string
	// RequestID is the response's own id, which is what identifies the call
	// in the server's journal. It is the server's and not the one the
	// request carried.
	RequestID string
	// Paths are the fields a code that names fields named.
	Paths []string
	// Detail is the developer sentence, printed only under -v.
	Detail string
	// RetryAfter is the header a 429 carries.
	RetryAfter string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d", e.Status)
}

// Unreachable is a request that reached no status: the dial, the TLS
// handshake, or the wait for the first response byte failed. It is a
// separate type because the exit scheme separates a server that refused
// from a server that was not there.
type Unreachable struct {
	Op  string
	Err error
}

func (e *Unreachable) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *Unreachable) Unwrap() error { return e.Err }

// CodeOf is the API's code for an error, or the empty string for anything
// that is not a refusal.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// errorFrom reads a refusal. The body is the envelope where the handler
// wrote one; a body that is not an envelope, which is what a proxy or a
// listener that is not this API answers, still becomes an Error carrying
// the status, so every failure has a code path and an exit.
func errorFrom(resp *http.Response, body []byte) *Error {
	e := &Error{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	var envelope httpjson.ErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
		e.RequestID, _ = envelope.Error.Details["request_id"].(string)
		e.Detail, _ = envelope.Error.Details["detail"].(string)
		e.Paths = append(e.Paths, list(envelope.Error.Details["paths"])...)
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if e.RequestID == "" {
		e.RequestID = resp.Header.Get("X-Request-ID")
	}
	return e
}

// list reads the envelope's paths, which JSON decodes as a list of any.
func list(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
