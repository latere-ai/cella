// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/events"
)

// feedPage is what GET /v1/events answers: one page of records and the cursor
// the next page starts at.
type feedPage struct {
	Items []events.Record `json:"items"`
	Next  string          `json:"next"`
}

func (f *recorded) feed(query string, status int, token string) feedPage {
	f.t.Helper()
	var page feedPage
	body := f.request("GET", "/v1/events?"+query, token, "", status)
	if status != 200 {
		return page
	}
	if err := json.Unmarshal(body, &page); err != nil {
		f.t.Fatalf("the feed answered %q: %v", body, err)
	}
	return page
}

// TestObjectFeed proves design 009's per-object feed: the records of the
// object the query names, newest first, each carrying the sequence the
// journal assigned, and no record of another object.
func TestObjectFeed(t *testing.T) {
	f := setupRecorded(t)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(createBody, `"name":"work"`, `"name":"other"`, 1)
	var second struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, other, 201), &second); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 200)

	page := f.feed("object="+obj.Status.ID+"&limit=50", 200, f.alice)
	if len(page.Items) < 2 {
		t.Fatalf("a created and exec'd sandbox has %d record(s)", len(page.Items))
	}
	types := map[events.Type]bool{}
	for i, item := range page.Items {
		types[item.Type] = true
		if item.Object.ID != obj.Status.ID {
			t.Errorf("the feed of %s carries a record of %s", obj.Status.ID, item.Object.ID)
		}
		if item.Seq == 0 {
			t.Errorf("record %d carries no sequence", i)
		}
		if i > 0 && page.Items[i-1].Seq < item.Seq {
			t.Errorf("records are not newest first: seq %d before seq %d", page.Items[i-1].Seq, item.Seq)
		}
	}
	for _, want := range []events.Type{events.TypeCreated, events.TypeExec} {
		if !types[want] {
			t.Errorf("the feed carries no %s record", want)
		}
	}

	// One page at a time, with the cursor the previous page returned, reads
	// the same records in the same order and stops.
	seen := []int64{}
	cursor := ""
	for range len(page.Items) + 1 {
		one := f.feed("object="+obj.Status.ID+"&limit=1&cursor="+cursor, 200, f.alice)
		if len(one.Items) == 0 {
			break
		}
		seen = append(seen, one.Items[0].Seq)
		cursor = one.Next
		if cursor == "" {
			break
		}
	}
	if len(seen) != len(page.Items) {
		t.Errorf("paging read %d record(s) of %d", len(seen), len(page.Items))
	}

	// The second sandbox's feed is its own.
	if got := f.feed("object="+second.Status.ID, 200, f.alice); len(got.Items) == 0 {
		t.Error("the second sandbox has an empty feed")
	} else {
		for _, item := range got.Items {
			if item.Object.ID != second.Status.ID {
				t.Errorf("the feed of %s carries a record of %s", second.Status.ID, item.Object.ID)
			}
		}
	}
}

// TestObjectFeedRefusals proves what the route refuses: a page naming no
// object, a limit outside the ceiling, and an object that is not there,
// whether the read is a page or a follow.
func TestObjectFeedRefusals(t *testing.T) {
	f := setupRecorded(t)
	for _, tc := range []struct {
		query  string
		status int
		code   string
	}{
		{"", 400, "invalid_field"},
		{"object=sbx_01j0000000000000000000000&limit=300", 400, "invalid_field"},
		{"object=sbx_01j0000000000000000000000&limit=zero", 400, "invalid_field"},
		{"object=sbx_01j0000000000000000000000&follow=1", 404, "not_found"},
		{"object=sbx_01j0000000000000000000000", 404, "not_found"},
		{"object=sec_01j0000000000000000000000", 404, "not_found"},
		{"object=env_nothing", 404, "not_found"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			body := f.request("GET", "/v1/events?"+tc.query, f.alice, "", tc.status)
			if !strings.Contains(string(body), tc.code) {
				t.Fatalf("the refusal is %q, want %s", body, tc.code)
			}
		})
	}
}

// TestObjectFeedAuthorizesTheObjectsKind proves design 009's rule that the
// feed is read under the kind's own read: a caller who cannot read the object
// cannot read its records either, and each kind reaches its own journal.
func TestObjectFeedAuthorizesTheObjectsKind(t *testing.T) {
	f := setupRecorded(t)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	// The feed is read after the object and under the object's own action,
	// so another subject's read is the refusal that read would get.
	f.request("GET", "/v1/events?object="+obj.Status.ID, f.bob, "", 403)

	// A Secret's feed is the Secret's, read under the secret's own kind.
	secret := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret","metadata":{"name":"token"},` +
		`"spec":{"scope":{"hosts":["api.example.com"]},"value":"s3cret"}}`
	var made struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("PUT", "/v1/secrets/token", f.alice, secret, 201), &made); err != nil {
		t.Fatal(err)
	}
	page := f.feed("object="+made.Status.ID, 200, f.alice)
	if len(page.Items) == 0 {
		t.Fatal("a created secret has an empty feed")
	}
	for _, item := range page.Items {
		if item.Object.Kind != events.KindSecret {
			t.Errorf("a secret's feed carries a %s record", item.Object.Kind)
		}
		if strings.Contains(string(item.Data), "s3cret") {
			t.Error("a record carries the secret's value")
		}
	}
	f.request("GET", "/v1/events?object="+made.Status.ID, f.bob, "", 403)

	// The environment this control plane drives is addressed by its name, and
	// its feed is decided under environment.read, which the owner policy
	// grants an administrator and no one else.
	body := f.request("GET", "/v1/events?object=default", f.alice, "", 403)
	if !strings.Contains(string(body), "environment.read") {
		t.Fatalf("the environment feed was decided under another action: %q", body)
	}
}

// TestFeedWithoutAnEmitterIsEmpty proves an API built over no journal answers
// an empty page rather than a failure: a control plane that records nothing
// has nothing to serve, and that is not an error the caller can act on.
func TestFeedWithoutAnEmitterIsEmpty(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("plain")
	body := f.request("GET", "/v1/events?object="+obj.Status.ID, f.alice, "", 200)
	var page feedPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || page.Next != "" {
		t.Fatalf("an API over no journal answered %q", body)
	}
}
