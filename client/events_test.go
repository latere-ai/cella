// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/cella/client"
)

// record is one line of the feed as the server writes it.
func record(seq, kind, reason string) string {
	return `{"id":"evt_` + seq + `","seq":` + seq + `,"type":"` + kind + `","time":"2026-09-24T10:00:0` + seq + `Z",` +
		`"object":{"kind":"Sandbox","id":"sbx_1","name":"dev","owner":"alice","labels":{"team":"core"}},` +
		`"sandbox":{"kind":"Sandbox","id":"sbx_1","name":"dev","owner":"alice"},"subject":"alice",` +
		`"workload":{"id":"sbx_0"},"requestId":"req_` + seq + `","reason":"` + reason + `","data":{"command":"sh"}}`
}

// TestAnEventPageIsOneObjectsHistory: a page names its object, carries the
// cursor and the page size the caller asked for, and decodes every record
// with its own bytes.
func TestAnEventPageIsOneObjectsHistory(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + record("2", "sandbox.stopped", "AutoStop") + `,` + record("1", "sandbox.created", "") + `],"next":"1"}`))
	})
	c := f.client(client.Config{})
	page, raw, err := c.Events(t.Context(), "sbx_1", client.EventOptions{Cursor: "3", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last().Query
	if f.last().Path != "/v1/events" || q.Get("object") != "sbx_1" || q.Get("cursor") != "3" || q.Get("limit") != "200" || q.Has("follow") {
		t.Fatalf("the page called %s?%s", f.last().Path, q.Encode())
	}
	if page.Next != "1" || len(page.Items) != 2 || !strings.Contains(string(raw), `"next":"1"`) {
		t.Fatalf("the page is %+v", page)
	}
	first := page.Items[0]
	if first.Seq != 2 || first.Type != "sandbox.stopped" || first.Reason != "AutoStop" || first.Object.Labels["team"] != "core" ||
		first.Sandbox == nil || first.Sandbox.ID != "sbx_1" || first.Workload == nil || first.Workload.ID != "sbx_0" ||
		first.RequestID != "req_2" || string(first.Data) != `{"command":"sh"}` || first.Time.Second() != 2 {
		t.Fatalf("the first record decoded as %+v", first)
	}
	if string(first.Raw) != record("2", "sandbox.stopped", "AutoStop") {
		t.Fatalf("the record's bytes are %s", first.Raw)
	}

	if _, _, err = c.Events(t.Context(), "sbx_1", client.EventOptions{}); err != nil {
		t.Fatal(err)
	}
	if q := f.last().Query; q.Has("cursor") || q.Has("limit") {
		t.Fatalf("a page with no options sent %s", q.Encode())
	}
	for name, body := range map[string]string{
		"an answer that is no page": `[]`,
		"a page holding no record":  `{"items":[1],"next":""}`,
	} {
		bad := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		if _, _, err = bad.client(client.Config{}).Events(t.Context(), "sbx_1", client.EventOptions{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	missing := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Nothing by that name exists.", nil)
	})
	if _, _, err = missing.client(client.Config{}).Events(t.Context(), "gone", client.EventOptions{}); client.CodeOf(err) != "not_found" {
		t.Fatalf("a page of an object that is not there read as %v", err)
	}
}

// TestAFollowedFeedReadsRecordsAsTheyArrive: the feed hands over each record
// as the server writes it, not once the response ends; it passes over the
// heartbeats, keeps each record's bytes, and ends with io.EOF where the
// server closed it.
func TestAFollowedFeedReadsRecordsAsTheyArrive(t *testing.T) {
	next := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, record("8", "sandbox.exec", "")+"\n\n")
		flusher.Flush()
		<-next
		_, _ = io.WriteString(w, "\n"+record("9", "sandbox.deleted", "Requested")+"\n")
		flusher.Flush()
	})
	c := f.client(client.Config{})
	feed, err := c.FollowEvents(t.Context(), client.FollowOptions{Object: "sbx_1", Cursor: client.After(7)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = feed.Close() }()
	q := f.last().Query
	if q.Get("follow") != "1" || q.Get("object") != "sbx_1" || q.Get("cursor") != "7" {
		t.Fatalf("the feed was opened with %s", q.Encode())
	}
	first, err := feed.Next()
	if err != nil || first.Seq != 8 || first.Type != "sandbox.exec" || string(first.Raw) != record("8", "sandbox.exec", "") {
		t.Fatalf("the first record is %+v, %v", first, err)
	}
	// The second record is written only now, so the first arrived before
	// the response ended.
	close(next)
	second, err := feed.Next()
	if err != nil || second.Seq != 9 || second.Reason != "Requested" {
		t.Fatalf("the second record is %+v, %v", second, err)
	}
	if _, err = feed.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("the feed's end read as %v", err)
	}

	// Every object's feed carries neither an object nor a cursor.
	all := newFixture(t, func(http.ResponseWriter, *http.Request) {})
	everything, err := all.client(client.Config{}).FollowEvents(t.Context(), client.FollowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = everything.Close()
	if q := all.last().Query; q.Get("follow") != "1" || q.Has("object") || q.Has("cursor") {
		t.Fatalf("every object's feed was opened with %s", q.Encode())
	}
}

// TestAFeedThatEndsOnAFailureIsTheError: a failure after the first byte is
// the feed's last line, an error envelope, and it reaches the caller as the
// same Error a refusal is. A refusal before the feed opens is an Error with
// its status, and a line that is no record, a line cut short, and a line
// past the bound end the feed with a failure of their own.
func TestAFeedThatEndsOnAFailureIsTheError(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, record("1", "sandbox.created", "")+"\n")
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"Sign in and send a valid token.","details":{"request_id":"req_feed"}}}`+"\n")
	})
	feed, err := f.client(client.Config{}).FollowEvents(t.Context(), client.FollowOptions{Object: "sbx_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = feed.Close() }()
	if _, err = feed.Next(); err != nil {
		t.Fatal(err)
	}
	_, err = feed.Next()
	var refusal *client.Error
	if !errors.As(err, &refusal) || refusal.Code != "unauthenticated" || refusal.Status != http.StatusInternalServerError || refusal.RequestID != "req_feed" {
		t.Fatalf("the failure line read as %T: %v", err, err)
	}

	refused := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests.", nil)
	})
	if _, err = refused.client(client.Config{}).FollowEvents(t.Context(), client.FollowOptions{}); client.CodeOf(err) != "rate_limited" {
		t.Fatalf("a refused feed opened with %v", err)
	}

	for name, tc := range map[string]struct {
		body  string
		check func(error) bool
	}{
		"a line that is no record": {"not json\n", func(err error) bool { return err != nil && !errors.Is(err, io.EOF) }},
		"a line cut short":         {`{"seq":1`, func(err error) bool { return errors.Is(err, io.ErrUnexpectedEOF) }},
		"a line past the bound": {strings.Repeat("x", 2<<20) + "\n", func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "bound")
		}},
	} {
		bad := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) })
		feed, err := bad.client(client.Config{}).FollowEvents(t.Context(), client.FollowOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = feed.Next(); !tc.check(err) {
			t.Errorf("%s read as %v", name, err)
		}
		_ = feed.Close()
	}
}
