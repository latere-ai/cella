// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
)

const (
	issuer = "https://login.example.com"
	alice  = issuer + "|alice"
	bob    = issuer + "|bob"
)

// TestStubSpeaksTheVocabulary: the stub authorizer of the shared
// contract, told Cella's table, passes the conformance suite driven from
// that same table. A row the package declares and the stub cannot answer,
// or an action outside the table the stub answers with a verdict rather
// than a refusal, fails here.
func TestStubSpeaksTheVocabulary(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	conformance.Run(t, s.URL(), s.Token(),
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))
}

// TestScaffoldSpeaksTheVocabulary: an endpoint a self-hoster writes on
// latere.ai/x/pkg/authz/server, with the owner policy as its whole
// decider, passes the same suite. This is the proof that Cella's
// vocabulary is all a Go authorizer for Cella has to supply.
func TestScaffoldSpeaksTheVocabulary(t *testing.T) {
	endpoint := newEndpoint(t)
	conformance.Run(t, endpoint.URL, bearer,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))
}

// TestScaffoldAnswersEachRow: every action of the table reaches the
// decider and answers a verdict, an allow for the object's owner and a
// not_owner deny for another subject, and an allow carries the ceilings
// the endpoint set as the figures cellad decodes.
func TestScaffoldAnswersEachRow(t *testing.T) {
	endpoint := newEndpoint(t)
	for _, a := range authorizer.Vocabulary().Actions {
		t.Run(a.Name, func(t *testing.T) {
			owned := request(a, "sbx_01J9OWNED0000000000000000", alice)
			d := ask(t, endpoint.URL, owned)
			if !d.Allow {
				t.Fatalf("%s on alice's own object was denied %q", a.Name, d.Reason)
			}
			limits, err := authorizer.DecodeLimits(d)
			if err != nil {
				t.Fatalf("the ceilings of an allow: %v", err)
			}
			if want := (authorizer.Limits{RequestsPerMinute: 1200, MaxSandboxes: 10, MaxPriority: 5}); limits != want {
				t.Errorf("the allow granted %+v, want spec 006's %+v", limits, want)
			}
			if authz.IsList(a.Name) {
				if d.Filter == nil || len(d.Filter.Owners) != 1 || d.Filter.Owners[0] != alice {
					t.Errorf("%s answered the filter %+v; a list narrows to the caller's own objects", a.Name, d.Filter)
				}
				return
			}
			other := request(a, "sbx_01J9OTHERS000000000000000", bob)
			if d := ask(t, endpoint.URL, other); d.Allow || d.Reason != authz.ReasonNotOwner {
				t.Errorf("%s on another subject's object answered %+v, want a %s deny", a.Name, d, authz.ReasonNotOwner)
			}
		})
	}
}

// TestClientRefusesAnActionOutsideTheTable: a core whose client carries
// the vocabulary spends no round trip on a typo, and the mistake is
// named as the core's rather than as an outage at the endpoint.
func TestClientRefusesAnActionOutsideTheTable(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	c, err := authz.NewClient(authz.Options{
		URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}, Vocabulary: authorizer.Vocabulary(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := authz.Request{Subject: alice, Issuer: issuer, Sub: "alice", Action: "sandbox.explode",
		Resource: authz.NewResource(authorizer.KindSandbox, "sbx_01J9OWNED0000000000000000", nil)}
	var unknown *authz.UnknownAction
	if _, err := c.Authorize(context.Background(), req); !errors.As(err, &unknown) {
		t.Fatalf("Authorize(sandbox.explode) = %v, want an UnknownAction", err)
	}
	if unknown.Core != authorizer.Core {
		t.Errorf("the refusal names %q, want %q", unknown.Core, authorizer.Core)
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("the client sent %d requests for an action outside the table; it sends none", n)
	}
}

// The endpoint's bearer, fixed here because the suite checks a wrong one.
const bearer = "conformance-bearer"

// newEndpoint serves the scaffold with the owner policy behind it, which
// is the whole of what a self-hoster writes.
func newEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	h := server.New(server.Options{
		Bearer:     bearer,
		Vocabulary: authorizer.Vocabulary(),
		Decider:    ownerPolicy{},
	})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// ownerPolicy is the whole decider: the owner policy's frame from the
// shared contract, over the owner the resource carries, with the create
// action of the request's own kind. Cella names no page action, so the
// five list actions reach it like every other row and answer a decision
// whose filter narrows the page to the caller's own objects.
type ownerPolicy struct{}

func (ownerPolicy) Decide(_ context.Context, req authz.Request) (authz.Decision, error) {
	kind, _, _ := strings.Cut(req.Action, ".")
	p := authz.Policy{Create: kind + ".create"}
	obj := authz.Object{}
	if owner := req.Resource.String("owner"); owner != "" {
		obj = authz.Object{Exists: true, Owner: owner}
	}
	d := p.Decide(req, obj)
	if authz.IsList(req.Action) {
		d = authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{req.Subject}}}
	}
	if d.Allow {
		d.Limits = grant(1200, 10, 5)
	}
	return d, nil
}

// grant renders the three ceilings of spec 006 the way an endpoint does.
func grant(rate, sandboxes, priority int) json.RawMessage {
	raw, err := json.Marshal(authorizer.WireLimits{
		RequestsPerMinute: &rate, MaxSandboxes: &sandboxes, MaxPriority: &priority,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// request builds one envelope for an action on an object with an owner.
func request(a authz.Action, id, owner string) authz.Request {
	return authz.Request{
		Subject: alice, Issuer: issuer, Sub: "alice", Action: a.Name,
		Resource: authz.NewResource(a.Kind, id, map[string]any{"owner": owner}),
		Request:  authz.Caller{ID: "req_01J9", IP: "203.0.113.4", UserAgent: "cella/0.1"},
		Claims:   map[string]any{},
	}
}

// ask posts one envelope to the endpoint and reads the decision the way
// a core's client does.
func ask(t *testing.T, url string, req authz.Request) authz.Decision {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+bearer)
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %d; a decision is a 200", req.Action, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	d, err := authz.ParseDecision(raw)
	if err != nil {
		t.Fatalf("%s: the answer is no decision: %v", req.Action, err)
	}
	return d
}
