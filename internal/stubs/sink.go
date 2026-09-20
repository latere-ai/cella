// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SignatureHeader, Freshness and the v1 formula are spec 009's, written
// out here rather than imported from internal/events. The sink is the
// other end of that delivery, and a verifier built from the signer agrees
// with it whatever either one does.
const (
	SignatureHeader = "Cella-Signature"
	// Freshness is how far the attempt's clock may be from this sink's
	// before the delivery is refused, in either direction.
	Freshness = 5 * time.Minute
	// DefaultFailStatus is what -sink-fail-first answers: a status the
	// deliverer retries.
	DefaultFailStatus = http.StatusServiceUnavailable
)

// SinkOptions configures the sink role.
type SinkOptions struct {
	// Addr is the listen address, empty to turn the role off.
	Addr string
	// Secrets are the halves of CELLA_EVENTS_SECRET this sink verifies
	// against. A delivery signed under any one of them is accepted, which
	// is what makes a rotation two deploys and no lost record.
	Secrets []string
	// FailFirst answers FailStatus to the first n deliveries whose
	// signature verified, so the retry of spec 009 is exercised.
	FailFirst  int
	FailStatus int
	// Status answers the next delivery with that status and then returns
	// to normal: 400 is spec 009's permanent drop.
	Status int
	// Now is the sink's clock, for a test that drives the freshness
	// window without waiting five minutes.
	Now func() time.Time
}

// sink is the role's state: the records it holds and the answers it owes.
type sink struct {
	o       SinkOptions
	mu      sync.Mutex
	records []json.RawMessage
	seqs    []int64
	failed  int
	status  int
}

// newSink builds the sink's handler.
func newSink(o SinkOptions) (http.Handler, func(), error) {
	secrets := make([]string, 0, len(o.Secrets))
	for _, s := range o.Secrets {
		if s = strings.TrimSpace(s); s != "" {
			secrets = append(secrets, s)
		}
	}
	if len(secrets) == 0 {
		return nil, nil, errors.New("the sink verifies each delivery's signature and was given no secret to verify it against")
	}
	o.Secrets = secrets
	if o.FailStatus == 0 {
		o.FailStatus = DefaultFailStatus
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &sink{o: o, status: o.Status}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{$}", s.deliver)
	mux.HandleFunc("GET /events", s.list)
	mux.HandleFunc("DELETE /events", s.clear)
	return mux, nil, nil
}

// deliver reads one delivery: the signature first, because a body nobody
// signed is a body from anywhere and is never stored.
func (s *sink) deliver(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.verify(r.Header.Get(SignatureHeader), body); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if code, ok := s.owed(); ok {
		http.Error(w, "the stub is told to answer "+strconv.Itoa(code), code)
		return
	}
	var record struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(body, &record); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.records = append(s.records, json.RawMessage(slices.Clone(body)))
	s.seqs = append(s.seqs, record.Seq)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// verify checks the header spec 009 states: t is the attempt's clock, and
// each v1 is the hex HMAC-SHA256 of "<t>.<body>" under one secret. A
// delivery is accepted when any v1 matches any configured secret, so
// either end may move first in a rotation.
func (s *sink) verify(header string, body []byte) error {
	var seconds int64
	var offered []string
	for part := range strings.SplitSeq(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch name {
		case "t":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("the signature's t is %q and not a unix time", value)
			}
			seconds = n
		case "v1":
			offered = append(offered, value)
		}
	}
	if seconds == 0 || len(offered) == 0 {
		return fmt.Errorf("the %s header is %q; the form is t=<unix>,v1=<hex>", SignatureHeader, header)
	}
	if skew := s.o.Now().Sub(time.Unix(seconds, 0)); skew > Freshness || skew < -Freshness {
		return fmt.Errorf("the delivery's clock is %s from this sink's, and the window is %s", skew.Round(time.Second), Freshness)
	}
	for _, secret := range s.o.Secrets {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = fmt.Fprintf(mac, "%d.", seconds)
		_, _ = mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		for _, got := range offered {
			if hmac.Equal([]byte(got), []byte(want)) {
				return nil
			}
		}
	}
	return errors.New("no signature verifies against a secret this sink holds")
}

// owed reports the status a delivery is answered with instead of the
// acknowledgement: the one-shot status first, then the opening run of
// failures.
func (s *sink) owed() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != 0 {
		code := s.status
		s.status = 0
		return code, true
	}
	if s.failed < s.o.FailFirst {
		s.failed++
		return s.o.FailStatus, true
	}
	return 0, false
}

// list serves what the sink holds, in sequence order. A record carries
// its own object, so one order over the whole list is one order per
// object, and a caller that wants one object's feed names it.
func (s *sink) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order := make([]int, len(s.records))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return int(s.seqs[a] - s.seqs[b]) })
	out := []json.RawMessage{}
	object := strings.TrimSpace(r.URL.Query().Get("object"))
	for _, i := range order {
		if object != "" && objectOf(s.records[i]) != object {
			continue
		}
		out = append(out, s.records[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// clear forgets every record and the answers owed, so one process serves
// one tier after another.
func (s *sink) clear(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.records, s.seqs, s.failed, s.status = nil, nil, 0, s.o.Status
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// objectOf is the id of the object a record is about.
func objectOf(record json.RawMessage) string {
	var body struct {
		Object struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.Unmarshal(record, &body); err != nil {
		return ""
	}
	return body.Object.ID
}

// DefaultSinkSecret is what the sink verifies against when no secret is
// given, the way the shared authorizer stub has a default bearer: a
// process started with no arguments serves every role.
const DefaultSinkSecret = "stub-events-secret"
