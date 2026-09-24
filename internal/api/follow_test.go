// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// followed is one open following feed: the response and a reader of its
// lines. The context bounds the whole feed, so a feed that stopped writing
// fails the test at the deadline rather than hanging it; every read below
// ends on a line arriving.
type followed struct {
	t      *testing.T
	res    *http.Response
	lines  *bufio.Reader
	cancel context.CancelFunc
}

// follow opens GET /v1/events with the query, asking for newline-delimited
// JSON the way a client of the feed does.
func (f *fixture) follow(query, token string) *followed {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/v1/events?"+query, nil)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", ndjson)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	s := &followed{t: f.t, res: res, lines: bufio.NewReader(res.Body), cancel: cancel}
	f.t.Cleanup(s.close)
	return s
}

func (s *followed) close() {
	s.cancel()
	_ = s.res.Body.Close()
}

// line reads one line without its newline, or the error that ended the feed.
func (s *followed) line() (string, error) {
	s.t.Helper()
	line, err := s.lines.ReadString('\n')
	if err != nil {
		return line, err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// record reads the next record, passing over heartbeats, and fails on an error
// line or the end of the feed.
func (s *followed) record() events.Record {
	s.t.Helper()
	for {
		line, err := s.line()
		if err != nil {
			s.t.Fatalf("the feed ended before a record: %v", err)
		}
		if line == "" {
			continue
		}
		var record events.Record
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.ID == "" {
			s.t.Fatalf("the feed wrote %q where a record was due", line)
		}
		return record
	}
}

// until reads records until one of the type arrives and returns every record
// read, which follow one another in sequence where the feed is one object's.
func (s *followed) until(kind events.Type) []events.Record {
	s.t.Helper()
	var out []events.Record
	for {
		record := s.record()
		out = append(out, record)
		if record.Type == kind {
			return out
		}
	}
}

// ended reads the feed to its end and returns the error line it closed with,
// or the empty string where it ended without one.
func (s *followed) ended() string {
	s.t.Helper()
	last := ""
	for {
		line, err := s.line()
		if errors.Is(err, io.EOF) {
			return last
		}
		if err != nil {
			s.t.Fatalf("the feed failed rather than ending: %v", err)
		}
		if strings.Contains(line, `"error"`) {
			last = line
		}
	}
}

// following is how many following feeds the handler holds open.
func (f *fixture) following() int64 { return f.h.(*handler).following.Load() }

// settle waits until the handler holds n following feeds, which is how a test
// learns that a feed it left has returned on the server.
func (f *fixture) settle(n int64) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for f.following() != n {
		if time.Now().After(deadline) {
			f.t.Fatalf("the handler holds %d following feeds, want %d", f.following(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// contiguous fails unless the records follow one another in sequence.
func contiguous(t *testing.T, records []events.Record) {
	t.Helper()
	for i := 1; i < len(records); i++ {
		if records[i].Seq != records[i-1].Seq+1 {
			t.Fatalf("the feed sent seq %d after %d", records[i].Seq, records[i-1].Seq)
		}
	}
}

// TestFollowedFeed: follow=1 with an object answers newline-delimited JSON
// with the headers a proxy reads, whatever Accept named; it replays from the
// cursor and carries a record committed after it opened while the response is
// still open; and the page beside it still negotiates its syntax.
func TestFollowedFeed(t *testing.T) {
	f := setupRecorded(t)
	obj := f.sandbox("followed")
	page := f.feed("object="+obj.Status.ID, 200, f.alice)
	newest := page.Items[0].Seq
	s := f.follow("follow=1&object="+obj.Status.ID+"&cursor="+strconv.FormatInt(newest-1, 10), f.alice)
	if s.res.StatusCode != http.StatusOK {
		t.Fatalf("the follow answered %d", s.res.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type": ndjson, "Cache-Control": "no-cache", "X-Accel-Buffering": "no",
	} {
		if got := s.res.Header.Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
	replayed := s.record()
	if replayed.Seq != newest || replayed.Object.ID != obj.Status.ID {
		t.Fatalf("the replay opened with %+v, want seq %d of %s", replayed, newest, obj.Status.ID)
	}
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 200)
	live := s.until(events.TypeExec)
	contiguous(t, append([]events.Record{replayed}, live...))
	for _, r := range live {
		if r.Object.ID != obj.Status.ID {
			t.Errorf("the feed of %s carries a record of %s", obj.Status.ID, r.Object.ID)
		}
	}

	// A page is negotiated as before: it answers YAML where asked,
	// newline-delimited JSON is not one of its syntaxes, and follow=0 is a
	// page.
	for accept, want := range map[string]int{"application/yaml": http.StatusOK, ndjson: http.StatusNotAcceptable} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+"/v1/events?object="+obj.Status.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		req.Header.Set("Accept", accept)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Errorf("a page asked for as %s answered %d", accept, res.StatusCode)
		}
		if want != http.StatusOK {
			continue
		}
		var page struct {
			Items []map[string]any `yaml:"items"`
		}
		if err := yaml.Unmarshal(body, &page); err != nil || len(page.Items) == 0 || !strings.Contains(res.Header.Get("Content-Type"), "yaml") {
			t.Errorf("a page asked for as YAML answered %s %q (%v)", res.Header.Get("Content-Type"), body, err)
		}
	}
	if got := f.feed("follow=0&object="+obj.Status.ID, 200, f.alice); len(got.Items) == 0 {
		t.Error("follow=0 answered no page")
	}
}

// TestFollowedFeedRefusals: every refusal is an envelope before the stream
// begins, with the status and sentence of design 008's table.
func TestFollowedFeedRefusals(t *testing.T) {
	f := setupRecordedWith(t, recordedOptions{journalCap: 2})
	obj := f.sandbox("refused")
	for range 3 {
		f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 200)
	}
	newest := f.feed("object="+obj.Status.ID, 200, f.alice).Items[0].Seq
	id := "follow=1&object=" + obj.Status.ID
	for _, tc := range []struct {
		name, query string
		status      int
		code, want  string
	}{
		{"a follow that is neither 0 nor 1", "follow=2&object=" + obj.Status.ID, 400, "invalid_field", "follow"},
		{"a cursor that is not a number", id + "&cursor=newest", 400, "invalid_field", "cursor"},
		{"a negative cursor", id + "&cursor=-1", 400, "invalid_field", "cursor"},
		{"a cursor past the newest record", id + "&cursor=" + strconv.FormatInt(newest+1, 10), 400, "invalid_field", "cursor"},
		{"a cursor the ring no longer holds", id + "&cursor=0", 410, "cursor_expired",
			"The feed no longer holds the records after that position; read it again from the newest."},
		{"a cursor without an object", "follow=1&cursor=3", 400, "invalid_field", "cursor"},
		{"an object that is not there", "follow=1&object=sbx_01j0000000000000000000000", 404, "not_found", "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := string(f.request("GET", "/v1/events?"+tc.query, f.alice, "", tc.status))
			if !strings.Contains(body, `"code":"`+tc.code+`"`) || !strings.Contains(body, tc.want) {
				t.Fatalf("the refusal is %s, want %s naming %s", body, tc.code, tc.want)
			}
		})
	}

	// A control plane that journals nothing has nothing to follow.
	plain := setup(t, nil)
	sandbox := plain.sandbox("plain")
	for _, query := range []string{"follow=1&object=" + sandbox.Status.ID, "follow=1"} {
		body := string(plain.request("GET", "/v1/events?"+query, plain.alice, "", 422))
		if !strings.Contains(body, "capability_unsupported") {
			t.Errorf("%s over no journal answered %s", query, body)
		}
	}

	// The cap: one feed open is the limit, the next is refused with 429,
	// and the slot is free again once the first caller leaves.
	capped := setupRecordedWith(t, recordedOptions{configure: func(o *Options) { o.FollowLimit = 1 }})
	first := capped.follow("follow=1", capped.alice)
	if first.res.StatusCode != http.StatusOK {
		t.Fatalf("the first feed answered %d", first.res.StatusCode)
	}
	body := string(capped.request("GET", "/v1/events?follow=1", capped.alice, "", 429))
	if !strings.Contains(body, `"code":"rate_limited"`) || !strings.Contains(body, "Too many requests; wait and retry.") {
		t.Fatalf("the feed past the cap answered %s", body)
	}
	first.close()
	capped.settle(0)
	if again := capped.follow("follow=1", capped.alice); again.res.StatusCode != http.StatusOK {
		t.Fatalf("a feed after the first left answered %d", again.res.StatusCode)
	}
}

// TestFollowedFeedHeartbeat: a feed with nothing to send writes an empty line
// each heartbeat, so a proxy in front of it sees bytes on an idle stream.
func TestFollowedFeedHeartbeat(t *testing.T) {
	f := setupRecordedWith(t, recordedOptions{configure: func(o *Options) { o.FollowHeartbeat = 10 * time.Millisecond }})
	obj := f.sandbox("idle")
	s := f.follow("follow=1&object="+obj.Status.ID, f.alice)
	for range 2 {
		line, err := s.line()
		if err != nil || line != "" {
			t.Fatalf("an idle feed wrote %q, %v; want an empty line", line, err)
		}
	}
}

// TestFollowedFeedEnds: a feed ends when the server stops taking requests,
// when its caller leaves, at its bearer's expiry with the error line that
// says so, and after the record of its object's delete.
func TestFollowedFeedEnds(t *testing.T) {
	t.Run("drain", func(t *testing.T) {
		draining := make(chan struct{})
		f := setupRecordedWith(t, recordedOptions{configure: func(o *Options) { o.Draining = draining }})
		obj := f.sandbox("drained")
		one, every := f.follow("follow=1&object="+obj.Status.ID, f.alice), f.follow("follow=1", f.alice)
		f.settle(2)
		close(draining)
		for _, s := range []*followed{one, every} {
			if last := s.ended(); last != "" {
				t.Errorf("a drained feed ended with %s", last)
			}
		}
		f.settle(0)
	})
	t.Run("disconnect", func(t *testing.T) {
		f := setupRecorded(t)
		obj := f.sandbox("left")
		s := f.follow("follow=1&object="+obj.Status.ID, f.alice)
		f.settle(1)
		s.close()
		f.settle(0)
	})
	t.Run("expiry", func(t *testing.T) {
		f := setupRecorded(t)
		obj := f.sandbox("expiring")
		soon := f.issuer.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(2 * time.Second).Unix()})
		s := f.follow("follow=1&object="+obj.Status.ID, soon)
		if s.res.StatusCode != http.StatusOK {
			t.Fatalf("the follow answered %d", s.res.StatusCode)
		}
		last := s.ended()
		if !strings.Contains(last, `"code":"unauthenticated"`) || !strings.Contains(last, "fresh token") {
			t.Fatalf("a feed whose bearer expired ended with %q", last)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		f := setupRecorded(t)
		obj := f.sandbox("ended")
		s := f.follow("follow=1&object="+obj.Status.ID, f.alice)
		f.request("DELETE", "/v1/sandboxes/"+obj.Status.ID, f.alice, "", 202)
		records := s.until(events.TypeDeleted)
		contiguous(t, records)
		if last := s.ended(); last != "" {
			t.Fatalf("the feed of a deleted sandbox ended with %s", last)
		}
	})
}

// TestFollowedFeedOfEveryObject: a feed of every object carries each record
// of the caller's own objects, of every kind it may list, and none of another
// owner's; and a credential that may not list sandboxes is refused one.
func TestFollowedFeedOfEveryObject(t *testing.T) {
	f := setupRecorded(t)
	mine := f.sandbox("mine")
	var theirs v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.bob, createBody, 201), &theirs); err != nil {
		t.Fatal(err)
	}
	alice, bob := f.follow("follow=1", f.alice), f.follow("follow=1", f.bob)
	exec := func(id, token string) {
		f.request("POST", "/v1/sandboxes/"+id+"/exec?wait=1", token, `{"command":["true"]}`, 200)
	}
	exec(theirs.Status.ID, f.bob)
	exec(mine.Status.ID, f.alice)
	secret := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret","metadata":{"name":"token"},` +
		`"spec":{"scope":{"hosts":["api.example.com"]},"value":"s3cret"}}`
	f.request("PUT", "/v1/secrets/token", f.alice, secret, 201)
	exec(theirs.Status.ID, f.bob)

	seen := alice.until(events.TypeExec)
	seen = append(seen, alice.until(events.TypeSecretCreated)...)
	for _, r := range seen {
		if r.Object.Owner != mine.Status.Owner {
			t.Errorf("alice's feed carries %s of %s, owned by %s", r.Type, r.Object.ID, r.Object.Owner)
		}
	}
	if last := seen[len(seen)-1]; last.Object.Kind != events.KindSecret {
		t.Errorf("alice's feed carries no record of her secret: %+v", last)
	}
	var execs int
	for range 2 {
		for _, r := range bob.until(events.TypeExec) {
			if r.Object.ID != theirs.Status.ID {
				t.Errorf("bob's feed carries %s of %s", r.Type, r.Object.ID)
			}
			if r.Type == events.TypeExec {
				execs++
			}
		}
	}
	if execs != 2 {
		t.Errorf("bob's feed carries %d of his two execs", execs)
	}

	// A personal access token granted one sandbox's read may follow that
	// sandbox and may not follow every object, which is a list.
	pat := f.issuer.Mint(issuertest.Claims{Sub: "alice", Extra: map[string]any{
		"token_use": "pat",
		"authorization_details": []any{map[string]any{
			"type": "latere-authz", "actions": []any{"cella:" + authorizer.ActionSandboxRead},
			"datatypes": []any{authorizer.KindSandbox}, "locations": []any{"https://api.example.com"},
			"identifier": mine.Status.ID,
		}},
	}})
	body := string(f.request("GET", "/v1/events?follow=1", pat, "", 403))
	if !strings.Contains(body, "forbidden") {
		t.Fatalf("every object's feed under a token without sandbox.list answered %s", body)
	}
	if s := f.follow("follow=1&object="+mine.Status.ID, pat); s.res.StatusCode != http.StatusOK {
		t.Fatalf("the granted sandbox's feed answered %d", s.res.StatusCode)
	}
}

// TestExpiryReadsEveryNumberForm: exp arrives as a JSON number, which decodes
// as a float, and an absent or unreadable one sets no deadline.
func TestExpiryReadsEveryNumberForm(t *testing.T) {
	at := time.Unix(1790000000, 0)
	for name, claim := range map[string]any{
		"float": float64(at.Unix()), "integer": at.Unix(), "number": json.Number(strconv.FormatInt(at.Unix(), 10)),
	} {
		if got, ok := expiry(callerWith(claim)); !ok || !got.Equal(at) {
			t.Errorf("the %s form read as %s, %v", name, got, ok)
		}
	}
	for name, claim := range map[string]any{"absent": nil, "text": "soon", "bad number": json.Number("x")} {
		if _, ok := expiry(callerWith(claim)); ok {
			t.Errorf("the %s form set a deadline", name)
		}
	}
}

// callerWith is a caller whose token carries the exp claim given, or none.
func callerWith(exp any) auth.Caller {
	claims := map[string]any{}
	if exp != nil {
		claims["exp"] = exp
	}
	return auth.Caller{Subject: "alice", Claims: claims}
}

// scripted answers each action as a test names it: an error, a deny, or an
// allow with a filter on one label.
type scripted struct {
	fail, deny string
	asked      map[string]int
}

func (s *scripted) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	s.asked[req.Action]++
	switch req.Action {
	case s.fail:
		return authz.Decision{}, errors.New("the authorizer is down")
	case s.deny:
		return authz.Decision{Reason: "denied by the test"}, nil
	}
	if authz.IsList(req.Action) {
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{"alice"}, Labels: map[string]string{"team": "a"}}}, nil
	}
	return authz.Decision{Allow: true}, nil
}

// TestRecordGateDecidesEachRecord: the gate of every object's feed applies a
// kind's list filter on owner and labels to the record's own object, asks
// each read once per object, skips a kind the caller may not list and a kind
// it does not know, and ends the feed on an authorizer that fails.
func TestRecordGateDecidesEachRecord(t *testing.T) {
	policy := &scripted{fail: authorizer.ActionEnvironmentList, deny: authorizer.ActionSecretList, asked: map[string]int{}}
	h := &handler{Authorizer: auth.NewAuthorizer(policy)}
	r := httptest.NewRequest(http.MethodGet, "/v1/events?follow=1", nil)
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, auth.Caller{Subject: "alice"}))
	gate := &recordGate{h: h, r: r, lists: map[string]listVerdict{}, reads: map[string]bool{}}
	object := func(kind, id, owner, team string) events.Record {
		return events.Record{Object: events.Object{Kind: kind, ID: id, Owner: owner, Labels: map[string]string{"team": team}}}
	}
	for _, tc := range []struct {
		name    string
		record  events.Record
		allowed bool
	}{
		{"a sandbox the filter passes", object(events.KindSandbox, "sbx_a", "alice", "a"), true},
		{"the same sandbox again", object(events.KindSandbox, "sbx_a", "alice", "a"), true},
		{"a label the filter does not name", object(events.KindSandbox, "sbx_b", "alice", "b"), false},
		{"another owner", object(events.KindSandbox, "sbx_c", "bob", "a"), false},
		{"a kind the caller may not list", object(events.KindSecret, "sec_a", "alice", "a"), false},
		{"a kind the feed does not serve", object("Volume", "vol_a", "alice", "a"), false},
	} {
		allowed, err := gate.allows(tc.record)
		if err != nil || allowed != tc.allowed {
			t.Errorf("%s: allowed %v, %v; want %v", tc.name, allowed, err, tc.allowed)
		}
	}
	if n := policy.asked[authorizer.ActionSandboxRead]; n != 1 {
		t.Errorf("the gate asked sandbox.read %d times for one object", n)
	}
	if _, err := gate.allows(object(events.KindEnvironment, "default", "", "a")); err == nil {
		t.Error("an authorizer that failed let the feed go on")
	}

	// A record about the default environment passes the filter as the
	// default does in the environment list, and its read decides it; every
	// other environment's record is held to the filter.
	policy = &scripted{asked: map[string]int{}}
	h = &handler{Authorizer: auth.NewAuthorizer(policy)}
	gate = &recordGate{h: h, r: r, defaultEnvironment: "default", lists: map[string]listVerdict{}, reads: map[string]bool{}}
	environment := func(name string) events.Record {
		return events.Record{Object: events.Object{Kind: events.KindEnvironment, ID: name, Name: name, Owner: "controller"}}
	}
	if allowed, err := gate.allows(environment("default")); err != nil || !allowed {
		t.Errorf("a record of the default environment: allowed %v, %v; its read allows it", allowed, err)
	}
	if allowed, err := gate.allows(environment("eu-gpu")); err != nil || allowed {
		t.Errorf("a record of an environment outside the filter: allowed %v, %v", allowed, err)
	}
	if n := policy.asked[authorizer.ActionEnvironmentRead]; n != 1 {
		t.Errorf("the gate asked environment.read %d times; once, for the default", n)
	}
}
