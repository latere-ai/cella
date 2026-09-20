// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	v1 "latere.ai/x/cella/manifest/v1"
)

const (
	issuerURL = "https://login.example.com"
	alice     = issuerURL + "|alice"
	bob       = issuerURL + "|bob"
)

// caller is one verified person, as the verifier hands it on.
func caller(subject, sub string, claims map[string]any) auth.Caller {
	return auth.Caller{Subject: subject, Issuer: issuerURL, Sub: sub, Claims: claims}
}

// info is what one request tells the authorizer about itself.
var info = authz.Caller{ID: "req_01J9", IP: "203.0.113.4", UserAgent: "cella/0.1"}

// asking builds the client against an endpoint, wrapped as cellad wraps
// it. now drives the cache's clock.
func asking(t *testing.T, url, token string, now func() time.Time) *auth.Authorizer {
	t.Helper()
	c, err := auth.NewClient(auth.ClientOptions{URL: url, Token: token, HTTP: &http.Client{}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewAuthorizer(c)
}

// TestAuthorizerFailsClosed is spec 006's row: every failure mode is
// authorizer_unavailable and never an allow.
func TestAuthorizerFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*stub.Server)
	}{
		{"a status other than 200", func(s *stub.Server) { s.Fail(http.StatusInternalServerError) }},
		{"a 404, which is a misconfigured endpoint", func(s *stub.Server) { s.Fail(http.StatusNotFound) }},
		{"a 200 whose body is not JSON", func(s *stub.Server) { s.FailBody(stub.BodyMalformed) }},
		{"a 200 whose body carries no allow", func(s *stub.Server) { s.FailBody(stub.BodyNoAllow) }},
		{"no answer at all, within the timeout", func(s *stub.Server) { s.Hang() }},
		{"a connection refused", func(s *stub.Server) { s.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
			a := askingWithTimeout(t, s.URL(), s.Token(), 200*time.Millisecond)
			tc.fail(s)
			t.Cleanup(s.Resume)
			_, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
				authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource())
			if err == nil {
				t.Fatal("the call produced an allow out of an endpoint that answered nothing")
			}
			if code := auth.CodeOf(err); code != auth.CodeAuthorizerUnavailable {
				t.Fatalf("the refusal is %q, want %q", code, auth.CodeAuthorizerUnavailable)
			}
		})
	}
}

// TestAuthorizerFailsClosedOnLimitsItCannotRead: a ceiling the control
// plane cannot read is not a ceiling it can hold, so an allow carrying
// one is no decision rather than an allow with no ceiling.
func TestAuthorizerFailsClosedOnLimitsItCannotRead(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.SetRules(stub.Rule{Allow: true, Limits: map[string]any{"max_sandboxes": -3}})
	a := asking(t, s.URL(), s.Token(), nil)
	_, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
		authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource())
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Fatalf("err = %v, want %q", err, auth.CodeAuthorizerUnavailable)
	}
}

// TestAuthorizerRetriesOnlyBeforeAResponseLine is spec 006's other half
// of that row: a connection that failed before a response line arrived
// is retried once, and nothing else is.
func TestAuthorizerRetriesOnlyBeforeAResponseLine(t *testing.T) {
	t.Run("a connection closed before a response line is retried once", func(t *testing.T) {
		var calls atomic.Int64
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				hijack(t, w)
				return
			}
			s.Handler().ServeHTTP(w, r)
		}))
		defer drop.Close()
		a := asking(t, drop.URL, s.Token(), nil)
		if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
			authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource()); err != nil {
			t.Fatalf("the dropped connection was not retried: %v", err)
		}
		if n := calls.Load(); n != 2 {
			t.Fatalf("the endpoint saw %d call(s); one drop is one retry", n)
		}
	})

	t.Run("a non-200 is not retried", func(t *testing.T) {
		var calls atomic.Int64
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer endpoint.Close()
		a := asking(t, endpoint.URL, "token", nil)
		if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
			authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9"}.Resource()); auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
			t.Fatalf("err = %v", err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("the endpoint saw %d call(s); a status is an answer and is not retried", n)
		}
	})

	t.Run("a body that does not parse is not retried", func(t *testing.T) {
		var calls atomic.Int64
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte("{"))
		}))
		defer endpoint.Close()
		a := asking(t, endpoint.URL, "token", nil)
		if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
			authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9"}.Resource()); auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
			t.Fatalf("err = %v", err)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("the endpoint saw %d call(s); a body that arrived is not retried", n)
		}
	})
}

// TestDecisionCache is spec 006's row: a second identical decision is
// answered without a call, an allow expires at the answer's ttl and at
// the cap, a deny at five seconds, unavailability is never cached, and
// a create and a list are keyed without a resource id.
func TestDecisionCache(t *testing.T) {
	newClock := func() (func() time.Time, func(time.Duration)) {
		at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
		return func() time.Time { return at }, func(d time.Duration) { at = at.Add(d) }
	}
	read := auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource()

	t.Run("a second identical decision costs no call", func(t *testing.T) {
		now, _ := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		a := asking(t, s.URL(), s.Token(), now)
		for range 3 {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead, read); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(s.Requests()); n != 1 {
			t.Fatalf("the endpoint saw %d call(s), want one", n)
		}
	})

	t.Run("an allow expires at the answer's ttl", func(t *testing.T) {
		now, advance := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		s.SetRules(stub.Rule{Allow: true, TTL: 30})
		a := asking(t, s.URL(), s.Token(), now)
		ask := func() {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead, read); err != nil {
				t.Fatal(err)
			}
		}
		ask()
		advance(29 * time.Second)
		ask()
		if n := len(s.Requests()); n != 1 {
			t.Fatalf("the endpoint saw %d call(s) inside the ttl", n)
		}
		advance(2 * time.Second)
		ask()
		if n := len(s.Requests()); n != 2 {
			t.Fatalf("the endpoint saw %d call(s) past the ttl", n)
		}
	})

	t.Run("an allow is capped at ten minutes however long the ttl", func(t *testing.T) {
		now, advance := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		s.SetRules(stub.Rule{Allow: true, TTL: 86400})
		a := asking(t, s.URL(), s.Token(), now)
		ask := func() {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead, read); err != nil {
				t.Fatal(err)
			}
		}
		ask()
		advance(authz.MaxTTL + time.Second)
		ask()
		if n := len(s.Requests()); n != 2 {
			t.Fatalf("the endpoint saw %d call(s); a ttl past the cap is capped", n)
		}
	})

	t.Run("a deny is held five seconds", func(t *testing.T) {
		now, advance := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		s.SetRules(stub.Rule{Allow: false, Reason: "not_owner"})
		a := asking(t, s.URL(), s.Token(), now)
		ask := func() {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead, read); auth.CodeOf(err) != auth.CodeForbidden {
				t.Fatalf("err = %v", err)
			}
		}
		ask()
		advance(authz.DenyTTL - time.Second)
		ask()
		if n := len(s.Requests()); n != 1 {
			t.Fatalf("the endpoint saw %d call(s) inside the deny's window", n)
		}
		advance(2 * time.Second)
		ask()
		if n := len(s.Requests()); n != 2 {
			t.Fatalf("the endpoint saw %d call(s) past the deny's window", n)
		}
	})

	t.Run("unavailability is never cached", func(t *testing.T) {
		now, _ := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		a := asking(t, s.URL(), s.Token(), now)
		s.Fail(http.StatusBadGateway)
		for range 3 {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead, read); err == nil {
				t.Fatal("an endpoint that answered nothing produced an allow")
			}
		}
		s.Resume()
		if n := len(s.Requests()); n != 3 {
			t.Fatalf("the endpoint saw %d call(s); an outage is asked again every time", n)
		}
	})

	t.Run("a create and a list are asked every time", func(t *testing.T) {
		now, _ := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		a := asking(t, s.URL(), s.Token(), now)
		for range 3 {
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
				authorizer.ActionSandboxCreate, auth.Sandbox{Name: "dev"}.Resource()); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
				authorizer.ActionSandboxList, auth.List(authorizer.ActionSandboxList)); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(s.Requests()); n != 6 {
			t.Fatalf("the endpoint saw %d call(s); an answer about no object names no key to remember it by", n)
		}
	})

	t.Run("two subjects are two keys", func(t *testing.T) {
		now, _ := newClock()
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		a := asking(t, s.URL(), s.Token(), now)
		for _, subject := range []string{alice, bob, alice, bob} {
			if _, err := a.Decide(t.Context(), caller(subject, "who", nil), info, authorizer.ActionSandboxRead, read); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(s.Requests()); n != 2 {
			t.Fatalf("the endpoint saw %d call(s); two subjects are two keys and each is remembered", n)
		}
	})
}

// TestAuthorizerRequestShapes is spec 006's row: every action of the
// table reaches the authorizer with the resource shape its row names,
// workload set for a sandbox caller, issuer and sub apart, and every
// claim of the token in claims verbatim.
func TestAuthorizerRequestShapes(t *testing.T) {
	claims := map[string]any{"email": "alice@example.com", "groups": []any{"research"}, "plan": "team"}
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	a := asking(t, s.URL(), s.Token(), nil)

	shapes := map[string]struct {
		res   authz.Resource
		want  []string
		unset []string
	}{
		authorizer.KindSandbox: {
			res:  auth.Sandbox{ID: "sbx_01J9", Name: "dev", Owner: alice, Environment: "env_01J9", Root: "sbx_01J9", Labels: map[string]string{"team": "research"}}.Resource(),
			want: []string{"name", "owner", "environment", "root", "labels"},
		},
		authorizer.KindSecret: {
			res:   auth.Secret{ID: "sec_01J9", Name: "token", Owner: alice}.Resource(),
			want:  []string{"name", "owner"},
			unset: []string{"environment"},
		},
		authorizer.KindVolume: {
			res:  auth.Volume{ID: "vol_01J9", Name: "data", Owner: alice, Environment: "env_01J9"}.Resource(),
			want: []string{"name", "owner", "environment"},
		},
		authorizer.KindSandboxSet: {
			res:  auth.Set{ID: "set_01J9", Name: "fleet", Owner: alice, Environment: "env_01J9"}.Resource(),
			want: []string{"name", "owner", "environment"},
		},
		authorizer.KindEnvironment: {
			res:  auth.Environment{ID: "env_01J9", Name: "default", Owner: alice, Isolation: "vm"}.Resource(),
			want: []string{"name", "owner", "isolation"},
		},
	}

	for _, action := range authorizer.Vocabulary().Actions {
		t.Run(action.Name, func(t *testing.T) {
			s.ClearRequests()
			shape := shapes[action.Kind]
			res := shape.res
			if authz.IsList(action.Name) {
				res = auth.List(action.Name)
			}
			if _, err := a.Decide(t.Context(), caller(alice, "alice", claims), info, action.Name, res); err != nil {
				t.Fatal(err)
			}
			seen := s.Requests()
			if len(seen) != 1 {
				t.Fatalf("the endpoint saw %d call(s)", len(seen))
			}
			req := seen[0]
			if req.Action != action.Name {
				t.Errorf("action = %q", req.Action)
			}
			if req.Resource.Kind != action.Kind {
				t.Errorf("resource.kind = %q, want %q", req.Resource.Kind, action.Kind)
			}
			if req.Subject != alice || req.Issuer != issuerURL || req.Sub != "alice" {
				t.Errorf("the subject travelled as %q with %q and %q apart", req.Subject, req.Issuer, req.Sub)
			}
			if req.Request != info {
				t.Errorf("the request member is %+v, want %+v", req.Request, info)
			}
			for name, want := range claims {
				if got, ok := req.Claims[name]; !ok {
					t.Errorf("the claim %q did not reach the authorizer", name)
				} else if name == "plan" && got != want {
					t.Errorf("the claim %q arrived as %v, want %v", name, got, want)
				}
			}
			if authz.IsList(action.Name) {
				if req.Resource.ID != "" || len(req.Resource.Fields) != 0 {
					t.Errorf("a list named the object %q with %v; a list names the kind alone", req.Resource.ID, req.Resource.Fields)
				}
				return
			}
			for _, name := range shape.want {
				if _, ok := req.Resource.Fields[name]; !ok {
					t.Errorf("the resource carries no %q; spec 006's row for %s names it", name, action.Kind)
				}
			}
			for _, name := range shape.unset {
				if _, ok := req.Resource.Fields[name]; ok {
					t.Errorf("the resource carries %q, which spec 006's row for %s does not name", name, action.Kind)
				}
			}
		})
	}
}

// TestACreateCarriesNoID: the id does not exist until the create is
// allowed, so a create names the manifest's fields and no id.
func TestACreateCarriesNoID(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	a := asking(t, s.URL(), s.Token(), nil)
	res := auth.Sandbox{Name: "dev", Environment: "env_01J9", Parent: "sbx_PARENT"}.Resource()
	if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxCreate, res); err != nil {
		t.Fatal(err)
	}
	req := s.Requests()[0]
	if req.Resource.ID != "" {
		t.Errorf("a create named the id %q", req.Resource.ID)
	}
	for _, name := range []string{"name", "environment", "parent"} {
		if _, ok := req.Resource.Fields[name]; !ok {
			t.Errorf("a create carries no %q", name)
		}
	}
	if _, ok := req.Resource.Fields["owner"]; ok {
		t.Error("a create named an owner; there is no object yet to own")
	}
}

// TestAWorkloadCallerCarriesItsSandbox: the workload member is set when
// the caller is a sandbox and absent for a person.
func TestAWorkloadCallerCarriesItsSandbox(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	a := asking(t, s.URL(), s.Token(), nil)
	sandbox := auth.Caller{Subject: "sandbox:sbx_01J9", Issuer: publicURL, Sub: "sandbox:sbx_01J9", Minted: true}
	if _, err := a.Decide(t.Context(), sandbox, info, authorizer.ActionSandboxExec,
		auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource()); err != nil {
		t.Fatal(err)
	}
	req := s.Requests()[0]
	if req.Workload == nil || req.Workload["id"] != "sbx_01J9" {
		t.Fatalf("workload = %v, want the caller's own sandbox", req.Workload)
	}
	if req.Subject != "sandbox:sbx_01J9" {
		t.Errorf("subject = %q; a token cellad minted renders as its bare sub", req.Subject)
	}

	s.ClearRequests()
	if _, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSandboxRead,
		auth.Sandbox{ID: "sbx_OTHER", Owner: alice}.Resource()); err != nil {
		t.Fatal(err)
	}
	if req := s.Requests()[0]; req.Workload != nil {
		t.Errorf("a person's request carried workload %v", req.Workload)
	}
}

// TestDenyMapping is spec 006's row: a deny on an own action is
// forbidden, and a deny through Lookup is not_found and identical to a
// missing object.
func TestDenyMapping(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.SetRules(stub.Rule{Allow: false, Reason: "not_owner"})
	a := asking(t, s.URL(), s.Token(), nil)
	res := auth.Secret{ID: "sec_01J9", Owner: bob}.Resource()

	_, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSecretRead, res)
	if code := auth.CodeOf(err); code != auth.CodeForbidden {
		t.Fatalf("a deny on the request's own action is %q, want %q", code, auth.CodeForbidden)
	}
	if !strings.Contains(err.Error(), "not_owner") {
		t.Errorf("the endpoint's reason is not in the developer detail: %v", err)
	}

	_, err = a.Lookup(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSecretMount, res)
	if code := auth.CodeOf(err); code != auth.CodeNotFound {
		t.Fatalf("a deny through a lookup is %q, want %q", code, auth.CodeNotFound)
	}

	// A secret that does not exist is the same answer, so a manifest
	// cannot be written to enumerate what somebody else owns.
	missing := auth.Secret{ID: "sec_NOTHING"}.Resource()
	_, err = a.Lookup(t.Context(), caller(alice, "alice", nil), info, authorizer.ActionSecretMount, missing)
	if code := auth.CodeOf(err); code != auth.CodeNotFound {
		t.Fatalf("a missing object is %q, want %q", code, auth.CodeNotFound)
	}
}

// TestAnUnknownActionCostsNoRoundTrip: the client carries the
// vocabulary, so a typo is the core's mistake and not an outage at the
// endpoint.
func TestAnUnknownActionCostsNoRoundTrip(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	a := asking(t, s.URL(), s.Token(), nil)
	_, err := a.Decide(t.Context(), caller(alice, "alice", nil), info, "sandbox.explode",
		auth.Sandbox{ID: "sbx_01J9"}.Resource())
	var unknown *authz.UnknownAction
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want an UnknownAction", err)
	}
	if auth.CodeOf(err) == auth.CodeAuthorizerUnavailable {
		t.Error("a mistake in the core was reported as an outage at the endpoint")
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("the endpoint saw %d call(s) for an action outside the table", n)
	}
}

// TestLimitsAndFilterReachTheCaller: the three ceilings of spec 006 and
// the filter of a list reach cellad as the figures its specs name.
func TestLimitsAndFilterReachTheCaller(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.SetRules(stub.Rule{
		Allow:  true,
		Limits: map[string]any{"requests_per_minute": 1200, "max_sandboxes": 10, "max_priority": 5},
		Filter: &authz.Filter{Owners: []string{alice}, Labels: map[string]string{"team": "research"}},
	})
	a := asking(t, s.URL(), s.Token(), nil)
	d, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
		authorizer.ActionSandboxList, auth.List(authorizer.ActionSandboxList))
	if err != nil {
		t.Fatal(err)
	}
	if want := (authorizer.Limits{RequestsPerMinute: 1200, MaxSandboxes: 10, MaxPriority: 5}); d.Limits != want {
		t.Errorf("the ceilings read as %+v, want %+v", d.Limits, want)
	}
	if d.Filter == nil || len(d.Filter.Owners) != 1 || d.Filter.Owners[0] != alice {
		t.Fatalf("the filter is %+v; a list narrows to the owners the answer names", d.Filter)
	}
	if d.Filter.Labels["team"] != "research" {
		t.Errorf("the filter's labels are %v", d.Filter.Labels)
	}
}

// TestCheckReadsAnAllowOnTheProbeAsAMisconfiguration: the probe is
// denied by every authorizer, and an endpoint that allows it is one that
// does not read the request.
func TestCheckReadsAnAllowOnTheProbeAsAMisconfiguration(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	if err := asking(t, s.URL(), s.Token(), nil).Check(t.Context()); err != nil {
		t.Fatalf("the stub denies the probe and the check failed: %v", err)
	}

	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allow":true}`))
	}))
	defer always.Close()
	err := asking(t, always.URL, "token", nil).Check(t.Context())
	if !errors.Is(err, authz.ErrProbeAllowed) {
		t.Fatalf("err = %v, want the probe's own refusal", err)
	}
}

// askingWithTimeout is asking with the deadline CELLA_AUTHORIZER_TIMEOUT
// carries, for the outage cases that would otherwise wait the default.
func askingWithTimeout(t *testing.T, url, token string, timeout time.Duration) *auth.Authorizer {
	t.Helper()
	c, err := auth.NewClient(auth.ClientOptions{URL: url, Token: token, HTTP: &http.Client{}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewAuthorizer(c)
}

// hijack takes the connection and closes it without a response line,
// which is the one failure the contract retries.
func hijack(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	h, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("the test server does not hijack")
	}
	conn, _, err := h.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

// patClaims is a verified personal access token's claims as the verifier
// hands them on: the credential class, and one grant naming one action on
// one sandbox.
var patClaims = map[string]any{
	"token_use": "pat",
	"authorization_details": []any{map[string]any{
		"type": "latere-authz", "actions": []any{"cella:sandbox.read"},
		"datatypes": []any{"Sandbox"}, "locations": []any{"https://api.latere.ai"},
		"identifier": "sbx_01J9GRANTED",
	}},
}

// TestEnvelopeCarriesTheCredentialsClaims is id-13's A18 at cellad's
// policy enforcement point. The grants are read off the envelope, so a
// request built with empty claims restricts nothing and fails open at the
// one layer the refusal at the door was meant to close. Envelope is the
// only place cellad builds a request, and it forwards the token verbatim:
// what an operator's endpoint reads, and what the owner policy reads, is
// what the issuer signed.
func TestEnvelopeCarriesTheCredentialsClaims(t *testing.T) {
	req := auth.Envelope(caller(alice, "alice", patClaims), info,
		authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9GRANTED", Owner: alice}.Resource())
	if use := req.Claims["token_use"]; use != "pat" {
		t.Errorf("the envelope carries token_use %v; the credential class reaches the decision point", use)
	}
	grants, err := authz.ParseGrants(req.Claims)
	if err != nil {
		t.Fatalf("the envelope's claims are no grants: %v", err)
	}
	if len(grants) != 1 || len(grants[0].Actions) != 1 || grants[0].Actions[0] != "cella:sandbox.read" {
		t.Fatalf("the decision point reads %+v off the envelope; it reads the grant the token carries", grants)
	}
}

// TestAGrantThatCoversNothingRefusesTheCaller: the deny the intersection
// writes is the refusal a caller is answered with, and its reason is
// "grant". A deny on the request's own action is forbidden; one on an
// object reached through a lookup is not_found, so a refused object and a
// missing one stay one answer for a narrowed key too.
func TestAGrantThatCoversNothingRefusesTheCaller(t *testing.T) {
	a := auth.NewAuthorizer(&auth.OwnerPolicy{Admins: []string{alice}})
	granted := auth.Sandbox{ID: "sbx_01J9GRANTED", Owner: alice}.Resource()
	elsewhere := auth.Sandbox{ID: "sbx_01J9ELSEWHERE", Owner: alice}.Resource()

	if _, err := a.Decide(t.Context(), caller(alice, "alice", patClaims), info,
		authorizer.ActionSandboxRead, granted); err != nil {
		t.Fatalf("the action the key's own grant names was refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		ask  func() error
		want auth.Code
	}{{
		name: "a decision on an action the key was not granted",
		ask: func() error {
			_, err := a.Decide(t.Context(), caller(alice, "alice", patClaims), info,
				authorizer.ActionSandboxDelete, granted)
			return err
		},
		want: auth.CodeForbidden,
	}, {
		name: "a lookup of an object no grant names",
		ask: func() error {
			_, err := a.Lookup(t.Context(), caller(alice, "alice", patClaims), info,
				authorizer.ActionSandboxRead, elsewhere)
			return err
		},
		want: auth.CodeNotFound,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ask()
			if code := auth.CodeOf(err); code != tc.want {
				t.Fatalf("the refusal is %q, want %q", code, tc.want)
			}
			if !strings.Contains(err.Error(), authz.ReasonGrant) {
				t.Errorf("the refusal reads %q; a developer reads %q and knows the key was narrowed", err, authz.ReasonGrant)
			}
		})
	}
}

// TestTheWorkloadMemberIsTheStores: the tree position and the budget in the
// envelope come from the sandbox the control plane read, never from the
// claim the token was minted with, so an authorizer decides on what is and
// not on what a token says.
func TestTheWorkloadMemberIsTheStores(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	a := asking(t, s.URL(), s.Token(), nil)
	// The claim says one thing and the store says another. The envelope
	// carries the store's.
	sandbox := auth.Caller{
		Subject: "sandbox:sbx_CHILD", Issuer: publicURL, Sub: "sandbox:sbx_CHILD", Minted: true,
		Claims: map[string]any{"spawn": map[string]any{"budget": float64(99), "depth": float64(9)}},
	}
	sandbox = sandbox.WithSandbox(v1.SandboxStatus{
		ID: "sbx_CHILD", Parent: "sbx_ROOT", Root: "sbx_ROOT", Environment: "env_01J9",
		Mesh:  "msh_01J9",
		Spawn: v1.SpawnStatus{Budget: 2, Used: 1, Depth: 1},
	})
	if _, err := a.Decide(t.Context(), sandbox, info, authorizer.ActionSandboxCreate,
		auth.Sandbox{Name: "grandchild", Parent: "sbx_CHILD", Root: "sbx_ROOT"}.Resource()); err != nil {
		t.Fatal(err)
	}
	req := s.Requests()[0]
	for name, want := range map[string]any{
		"id": "sbx_CHILD", "parent": "sbx_ROOT", "root": "sbx_ROOT",
		"environment": "env_01J9", "mesh": "msh_01J9",
	} {
		if got := req.Workload[name]; got != want {
			t.Errorf("workload.%s = %v, want %v", name, got, want)
		}
	}
	spawn, ok := req.Workload["spawn"].(map[string]any)
	if !ok {
		t.Fatalf("workload.spawn = %v", req.Workload["spawn"])
	}
	for name, want := range map[string]float64{"budget": 2, "used": 1, "depth": 1} {
		if got, _ := spawn[name].(float64); got != want {
			t.Errorf("workload.spawn.%s = %v, want %v; the ledger is the budget, not the claim", name, spawn[name], want)
		}
	}
	// A caller the control plane read nothing for carries the id alone,
	// which is what tells an authorizer "no parent" from "a parent this
	// version does not send".
	s.ClearRequests()
	bare := auth.Caller{Subject: "sandbox:sbx_CHILD", Issuer: publicURL, Sub: "sandbox:sbx_CHILD", Minted: true}
	if _, err := a.Decide(t.Context(), bare, info, authorizer.ActionSandboxExec,
		auth.Sandbox{ID: "sbx_CHILD", Owner: alice}.Resource()); err != nil {
		t.Fatal(err)
	}
	if got := s.Requests()[0].Workload; len(got) != 1 || got["id"] != "sbx_CHILD" {
		t.Fatalf("workload = %v, want the id alone", got)
	}
}
