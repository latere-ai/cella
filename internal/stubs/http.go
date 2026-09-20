// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxBody is what a stub reads of a request. A manifest and a record are
// both small, and a stub is not the place to discover how a peer behaves
// against a body limit.
const maxBody = 1 << 20

// writeJSON answers one JSON body with a status.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is written, so the answer is already on the wire and
		// the only thing left is not to hide the failure.
		_, _ = fmt.Fprintf(w, "\n%v\n", err)
	}
}

// bearerOK reports whether a request carries the bearer an endpoint
// requires. An endpoint told no token requires none, which is what a
// stub reached over loopback by one process is usually started as.
func bearerOK(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(raw)), []byte(token)) == 1
}

// outage is one of the failure modes an endpoint of specs 007 and 009 can
// be put into: no answer at all, a 200 that is not JSON, a 200 with no
// verdict, or a status of its own.
type outage struct {
	mode   string
	status int
	// closed is shut when the role is closing, so a request parked by the
	// timeout mode is released with the process and never with a leak.
	closed chan struct{}
}

// parseOutage reads a fail-mode flag.
func parseOutage(mode string, closed chan struct{}) (*outage, error) {
	o := &outage{mode: strings.TrimSpace(mode), closed: closed}
	switch {
	case o.mode == "" || o.mode == FailTimeout || o.mode == FailMalformed || o.mode == FailNoAllow:
	case strings.HasPrefix(o.mode, FailStatusPrefix):
		code, err := statusOf(o.mode)
		if err != nil {
			return nil, err
		}
		o.mode, o.status = FailStatusPrefix, code
	default:
		return nil, fmt.Errorf("the failure mode is %q; %s, %s, %s or %s<code>", mode, FailTimeout, FailMalformed, FailNoAllow, FailStatusPrefix)
	}
	return o, nil
}

// answer writes the mode's answer and reports whether it did, so a
// handler asks it once before it does any work of its own.
func (o *outage) answer(w http.ResponseWriter, r *http.Request) bool {
	switch o.mode {
	case FailTimeout:
		select {
		case <-r.Context().Done():
		case <-o.closed:
		}
		return true
	case FailMalformed:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
		return true
	case FailNoAllow:
		writeJSON(w, http.StatusOK, map[string]any{"reason": "an answer with no verdict"})
		return true
	case FailStatusPrefix:
		http.Error(w, "the stub is told to fail", o.status)
		return true
	}
	return false
}
