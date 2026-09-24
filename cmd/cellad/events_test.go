// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
)

// The sink's own rules, written out rather than imported. The sink lives in
// another repository and this one depends on none of it, so the wire is held
// by a second implementation: a change to the header or the body that this
// stub would refuse fails here.
const (
	signatureHeader = "Cella-Signature"
	freshness       = 5 * time.Minute
)

// record is the sink's view of one delivery. Only the fields a sink reads are
// named, so a field this test does not mention is one the wire does not owe.
type record struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Time   time.Time
	Object struct {
		Kind   string            `json:"kind"`
		ID     string            `json:"id"`
		Name   string            `json:"name"`
		Owner  string            `json:"owner"`
		Labels map[string]string `json:"labels"`
	} `json:"object"`
	Sandbox *struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
	} `json:"sandbox"`
	Subject   string          `json:"subject"`
	RequestID string          `json:"requestId"`
	Reason    string          `json:"reason"`
	Data      json.RawMessage `json:"data"`
	Raw       []byte          `json:"-"`
}

// stubSink is the operator's endpoint: it verifies every signature, refuses
// an unsigned or stale delivery, and answers 200 once the record is stored.
type stubSink struct {
	server  *httptest.Server
	secrets []string

	mu    sync.Mutex
	got   []record
	fails int
}

func newStubSink(t *testing.T, secrets ...string) *stubSink {
	t.Helper()
	s := &stubSink{secrets: secrets}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubSink) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		http.Error(w, "the delivery is "+got, http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "the delivery carried a bearer", http.StatusBadRequest)
		return
	}
	if err := verifySignature(s.secrets, r.Header.Get(signatureHeader), body, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var rec record
	if err := json.Unmarshal(body, &rec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec.Raw = body
	s.mu.Lock()
	defer s.mu.Unlock()
	// The first deliveries fail, so the run proves the retry as well as the
	// happy path: every record still arrives, once, in order.
	if s.fails > 0 {
		s.fails--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.got = append(s.got, rec)
	w.WriteHeader(http.StatusOK)
}

func (s *stubSink) records() []record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]record(nil), s.got...)
}

// verifySignature is the formula of design 009 on the receiving side.
func verifySignature(secrets []string, header string, body []byte, now time.Time) error {
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
	if delta := now.Sub(at); delta > freshness || delta < -freshness {
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

// The canaries of design 009: a command, a file body, and a value shaped like
// a secret. None of the three may appear in any record of the whole run.
const (
	canaryCommand = "a-command-nobody-should-see"
	canaryFile    = "a-file-body-nobody-should-see"
	canarySecret  = "sk-ant-avaluenobodyshouldsee0000"
)

// TestEventsEndToEnd runs cellad on the native driver with a stub sink and
// drives one sandbox through create, files, exec, stop and delete. Every
// record arrives once, in sequence order for the sandbox, signed and with the
// labels the sink files it under; the first two deliveries fail, so the run
// proves the retry on the way.
func TestEventsEndToEnd(t *testing.T) {
	sink := newStubSink(t, "first-secret", "second-secret")
	sink.fails = 2
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":        issuer.URL(),
		"CELLA_OIDC_AUDIENCE":       "cella,platform.example",
		"CELLA_EVENTS_URL":          sink.server.URL,
		"CELLA_EVENTS_SECRET":       "first-secret,second-secret",
		"CELLA_EVENTS_RETRY_WINDOW": "1h",
	})
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	call := func(method, path, body, media string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", media)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}
	created := call("POST", "/v1/sandboxes?wait=1", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",`+
		`"metadata":{"name":"events","labels":{"tenant":"acme"}},`+
		`"spec":{"env":{"API_TOKEN":"`+canarySecret+`"}}}`, "application/json", 201)
	var obj struct {
		Status struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
		} `json:"status"`
	}
	if err := json.Unmarshal(created, &obj); err != nil {
		t.Fatal(err)
	}
	path := "/v1/sandboxes/" + obj.Status.ID

	var upload bytes.Buffer
	tw := tar.NewWriter(&upload)
	if err := tw.WriteHeader(&tar.Header{Name: "canary.txt", Mode: 0600, Size: int64(len(canaryFile))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(canaryFile)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	call("PUT", path+"/files?dest=/workspace", upload.String(), "application/x-tar", 204)
	call("POST", path+"/exec?wait=1", `{"command":["sh","-c","printf `+canaryCommand+`; exit 5"]}`, "application/json", 200)
	call("GET", path+"/files?path=/workspace/canary.txt", "", "application/json", 200)
	call("POST", path+"/stop", "", "application/json", 200)
	call("DELETE", path, "", "application/json", 202)

	want := []string{
		"sandbox.created", "sandbox.started", "sandbox.files", "sandbox.exec",
		"sandbox.files", "sandbox.stopped", "sandbox.deleted",
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(sink.records()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := sink.records()
	types := make([]string, 0, len(got))
	for _, r := range got {
		types = append(types, r.Type)
	}
	if len(got) != len(want) {
		t.Fatalf("the sink took %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("the sink took %v, want %v", types, want)
		}
	}
	seen := map[string]bool{}
	for i, r := range got {
		if seen[r.ID] {
			t.Errorf("%s arrived twice", r.ID)
		}
		seen[r.ID] = true
		if r.Seq != int64(i+1) {
			t.Errorf("%s took sequence %d at position %d", r.Type, r.Seq, i)
		}
		if r.Object.Kind != "Sandbox" || r.Object.ID != obj.Status.ID || r.Object.Name != "events" {
			t.Errorf("%s names the object %+v", r.Type, r.Object)
		}
		if r.Object.Labels["tenant"] != "acme" {
			t.Errorf("%s arrived without the labels the sink files it under: %+v", r.Type, r.Object.Labels)
		}
		if r.Object.Owner != obj.Status.Owner || r.Subject != obj.Status.Owner {
			t.Errorf("%s names owner %q and subject %q", r.Type, r.Object.Owner, r.Subject)
		}
		if r.RequestID == "" {
			t.Errorf("%s carries no request id", r.Type)
		}
		if !strings.HasPrefix(r.ID, "evt_") {
			t.Errorf("the id is %q", r.ID)
		}
	}
	// An operation names the sandbox in context; a mutation does not need to.
	for _, r := range got {
		operation := r.Type == "sandbox.exec" || r.Type == "sandbox.files"
		if operation && (r.Sandbox == nil || r.Sandbox.Labels["tenant"] != "acme") {
			t.Errorf("%s names no sandbox in context: %+v", r.Type, r.Sandbox)
		}
		if !operation && r.Sandbox != nil {
			t.Errorf("%s names a sandbox in context it did not need: %+v", r.Type, r.Sandbox)
		}
	}
	// The terminal transitions a person asked for carry Request. The sink's
	// usage fold opens its interval on started and closes it on stopped, so
	// a sandbox that runs from creation has to say it started.
	for _, r := range got {
		if r.Type == "sandbox.started" || r.Type == "sandbox.stopped" || r.Type == "sandbox.deleted" {
			if r.Reason != "Request" {
				t.Errorf("%s carries reason %q", r.Type, r.Reason)
			}
		}
	}
	if code := stop(); code != 0 {
		t.Fatalf("shutdown: %d", code)
	}
}

// TestNoContentInEvents: the canary command, the canary file body and the
// value shaped like a secret reach no record of the run above. It reads the
// bytes the sink received, so it holds the wire and not an internal type.
func TestNoContentInEvents(t *testing.T) {
	sink := newStubSink(t, "only-secret")
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
		"CELLA_EVENTS_URL":    sink.server.URL,
		"CELLA_EVENTS_SECRET": "only-secret",
	})
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	call := func(method, path, body, media string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", media)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}
	created := call("POST", "/v1/sandboxes?wait=1", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",`+
		`"metadata":{"name":"canary"},"spec":{"env":{"API_TOKEN":"`+canarySecret+`"},`+
		`"command":["sh","-c","sleep 60"]}}`, "application/json", 201)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(created, &obj); err != nil {
		t.Fatal(err)
	}
	path := "/v1/sandboxes/" + obj.Status.ID
	var upload bytes.Buffer
	tw := tar.NewWriter(&upload)
	if err := tw.WriteHeader(&tar.Header{Name: "canary.txt", Mode: 0600, Size: int64(len(canaryFile))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(canaryFile)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	call("PUT", path+"/files?dest=/workspace", upload.String(), "application/x-tar", 204)
	call("POST", path+"/exec?wait=1", `{"command":["sh","-c","printf `+canaryCommand+`"]}`, "application/json", 200)
	call("DELETE", path, "", "application/json", 202)

	deadline := time.Now().Add(30 * time.Second)
	for len(sink.records()) < 5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := sink.records()
	if len(got) < 5 {
		t.Fatalf("the sink took %d record(s), want the create, the start, the transfer, the exec and the delete", len(got))
	}
	for _, r := range got {
		for _, canary := range []string{canaryCommand, canaryFile, canarySecret} {
			if bytes.Contains(r.Raw, []byte(canary)) {
				t.Errorf("%s carries a canary: %s", r.Type, r.Raw)
			}
		}
	}
	if code := stop(); code != 0 {
		t.Fatalf("shutdown: %d", code)
	}
}
