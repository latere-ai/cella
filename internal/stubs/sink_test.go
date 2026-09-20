// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/stubs"
)

// record is one delivery body of spec 009, reduced to the members the
// sink reads: the sequence it orders by and the object it files under.
func record(seq int64, object string) []byte {
	body, err := json.Marshal(map[string]any{
		"id": "evt_" + object, "seq": seq, "type": "sandbox.created",
		"time": time.Now().UTC().Format(time.RFC3339Nano),
		"object": map[string]any{
			"kind": "Sandbox", "id": object, "name": object, "owner": "https://issuer.example|alice",
		},
		"subject": "https://issuer.example|alice",
	})
	if err != nil {
		panic(err)
	}
	return body
}

// deliver posts one record the way the deliverer of spec 009 does, with
// the header the real signer writes.
func deliver(t *testing.T, url string, body []byte, at time.Time, secrets ...string) (int, []byte) {
	t.Helper()
	return post(t, url, body, map[string]string{
		events.SignatureHeader: events.Header(at, body, secrets...),
	})
}

// TestTheSinkVerifiesWhatTheDelivererSigns is the interoperation the
// signature exists for: the header internal/events writes is the one this
// sink verifies, and the two implementations are independent.
func TestTheSinkVerifiesWhatTheDelivererSigns(t *testing.T) {
	const first, second = "the-first-half", "the-second-half"
	s := start(t, stubs.Options{Sink: stubs.SinkOptions{Secrets: []string{first, second}}})
	url := s.URL(stubs.RoleSink)

	for name, secret := range map[string]string{"the first half": first, "the second half": second} {
		t.Run(name, func(t *testing.T) {
			if code, body := deliver(t, url, record(1, "sbx_"+name), time.Now(), secret); code != http.StatusNoContent {
				t.Fatalf("a record signed under %s answered %d: %s", name, code, body)
			}
		})
	}
	// A rotation sends both halves at once, which either end accepts.
	if code, body := deliver(t, url, record(2, "sbx_rotating"), time.Now(), first, second); code != http.StatusNoContent {
		t.Fatalf("a record signed under both halves answered %d: %s", code, body)
	}
}

// TestTheSinkRefusesWhatItCannotVerify: a wrong secret, a body changed
// after it was signed, a stale clock and a header of the wrong shape are
// each 401, and none of them is stored.
func TestTheSinkRefusesWhatItCannotVerify(t *testing.T) {
	now := time.Now()
	s := start(t, stubs.Options{Sink: stubs.SinkOptions{
		Secrets: []string{"the-secret"},
		Now:     func() time.Time { return now },
	}})
	url := s.URL(stubs.RoleSink)
	body := record(1, "sbx_1")

	if code, _ := deliver(t, url, body, now, "another-secret"); code != http.StatusUnauthorized {
		t.Errorf("a record signed under a secret this sink does not hold answered %d, want 401", code)
	}
	if code, _ := post(t, url, body, map[string]string{
		events.SignatureHeader: events.Header(now, record(9, "sbx_other"), "the-secret"),
	}); code != http.StatusUnauthorized {
		t.Errorf("a body changed after it was signed answered %d, want 401", code)
	}
	for name, at := range map[string]time.Time{
		"six minutes old":         now.Add(-6 * time.Minute),
		"six minutes in the lead": now.Add(6 * time.Minute),
	} {
		if code, _ := deliver(t, url, body, at, "the-secret"); code != http.StatusUnauthorized {
			t.Errorf("a delivery %s answered %d, want 401", name, code)
		}
	}
	for name, header := range map[string]string{
		"no header":  "",
		"no t":       "v1=abcdef",
		"no v1":      "t=1700000000",
		"a bad t":    "t=recently,v1=abcdef",
		"nonsense":   "hello",
		"a wrong v1": "t=" + time.Now().Format("20060102") + ",v1=00",
	} {
		if code, _ := post(t, url, body, map[string]string{events.SignatureHeader: header}); code != http.StatusUnauthorized {
			t.Errorf("a delivery with %s answered %d, want 401", name, code)
		}
	}
	if _, read := get(t, url+"/events"); string(read) != "[]\n" {
		t.Errorf("a record nobody could verify was stored: %s", read)
	}
	// The window is what the refusals above are measured against, so the
	// delivery that sits inside it is accepted and stored.
	if code, _ := deliver(t, url, body, now.Add(-4*time.Minute), "the-secret"); code != http.StatusNoContent {
		t.Errorf("a delivery inside the window answered %d", code)
	}
	if got := seqs(t, url+"/events"); len(got) != 1 {
		t.Errorf("the sink holds %v records, and one delivery verified", got)
	}
}

// TestTheSinkServesWhatItHeld: a tier reads the records back, in
// sequence, and narrows them to one object.
func TestTheSinkServesWhatItHeld(t *testing.T) {
	s := start(t, stubs.Options{Sink: stubs.SinkOptions{Secrets: []string{"the-secret"}}})
	url := s.URL(stubs.RoleSink)
	for _, r := range []struct {
		seq    int64
		object string
	}{{3, "sbx_a"}, {1, "sbx_b"}, {2, "sbx_a"}} {
		if code, body := deliver(t, url, record(r.seq, r.object), time.Now(), "the-secret"); code != http.StatusNoContent {
			t.Fatalf("the delivery answered %d: %s", code, body)
		}
	}
	if got := seqs(t, url+"/events"); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("the feed is %v, and spec 009 orders by seq", got)
	}
	if got := seqs(t, url+"/events?object=sbx_a"); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("one object's feed is %v", got)
	}
	if code, _ := postNoBody(t, url+"/events", http.MethodDelete); code != http.StatusNoContent {
		t.Fatalf("the feed was not cleared: %d", code)
	}
	if got := seqs(t, url+"/events"); len(got) != 0 {
		t.Errorf("the cleared feed holds %v", got)
	}
}

// TestTheSinkFailsTheFirstDeliveries: the retry of spec 009 is exercised
// against a sink that refuses and then accepts, and the 400 that is a
// permanent drop is one flag of its own.
func TestTheSinkFailsTheFirstDeliveries(t *testing.T) {
	s := start(t, stubs.Options{Sink: stubs.SinkOptions{
		Secrets: []string{"the-secret"}, FailFirst: 2,
	}})
	url := s.URL(stubs.RoleSink)
	body := record(1, "sbx_1")
	for attempt := 1; attempt <= 2; attempt++ {
		if code, _ := deliver(t, url, body, time.Now(), "the-secret"); code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d answered %d, want 503", attempt, code)
		}
	}
	if code, _ := deliver(t, url, body, time.Now(), "the-secret"); code != http.StatusNoContent {
		t.Fatalf("the third attempt answered %d", code)
	}
	if got := seqs(t, url+"/events"); len(got) != 1 {
		t.Errorf("the sink holds %v; a refused attempt is not a record", got)
	}

	one := start(t, stubs.Options{Sink: stubs.SinkOptions{
		Secrets: []string{"the-secret"}, Status: http.StatusBadRequest,
	}})
	url = one.URL(stubs.RoleSink)
	if code, _ := deliver(t, url, body, time.Now(), "the-secret"); code != http.StatusBadRequest {
		t.Errorf("the one-shot status answered %d, want 400", code)
	}
	if code, _ := deliver(t, url, body, time.Now(), "the-secret"); code != http.StatusNoContent {
		t.Errorf("the delivery after the one-shot status answered %d", code)
	}
}

// TestTheSinkRefusesARecordItCannotRead: a signature that verifies over
// something that is not a record is the 400 spec 009 calls a permanent
// drop, and not an acknowledgement.
func TestTheSinkRefusesARecordItCannotRead(t *testing.T) {
	s := start(t, stubs.Options{Sink: stubs.SinkOptions{Secrets: []string{"the-secret"}}})
	if code, _ := deliver(t, s.URL(stubs.RoleSink), []byte("{not json"), time.Now(), "the-secret"); code != http.StatusBadRequest {
		t.Errorf("a signed body that is no record answered %d, want 400", code)
	}
}

// seqs reads the sequence numbers of a feed, in the order it served them.
func seqs(t *testing.T, url string) []int64 {
	t.Helper()
	code, body := get(t, url)
	if code != http.StatusOK {
		t.Fatalf("the feed answered %d", code)
	}
	var records []struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("the feed is %s: %v", body, err)
	}
	out := make([]int64, 0, len(records))
	for _, r := range records {
		out = append(out, r.Seq)
	}
	return out
}
