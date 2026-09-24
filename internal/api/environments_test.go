// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
)

// keyed is a control plane that signs its own tokens: the signer of spec 006,
// the revocation list of spec 010, and the two key routes over them.
type keyed struct {
	*fixture
	keys   *auth.EnvironmentKeys
	hub    *remote.Hub
	signer *auth.Signer
}

// setupKeyed stands a control plane up with a signer, so the key routes have
// something to mint with and the verifier has something to check against.
func setupKeyed(t *testing.T, policy authz.Authorizer) *keyed {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner(auth.SignerOptions{
		Issuer: "https://control.example.test", Audience: "cella", Keys: []*rsa.PrivateKey{key},
	})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	revocations := store.NewRevocations(journal)
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer.URL()}, Audience: "cella",
		LocalIssuer: signer.Issuer(), LocalKeys: signer.PublicKeys(), Revocations: revocations,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	c, err := controller.Open(t.Context(), controller.Options{
		DataDir: t.TempDir(), Driver: d, Environment: "default",
		Registrations: WorkerRegistrations(hub),
		NewDriver: func(obj v1.Environment) (runtime.Driver, error) {
			return remote.New(remote.Options{Environment: obj.Status.ID, Transport: hub.Transport(obj.Status.ID)})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	keys, err := auth.NewEnvironmentKeys(signer, revocations, store.NewKeyRegistry(journal), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy = &auth.OwnerPolicy{DefaultEnvironment: "default", Admins: []string{issuer.URL() + "|admin"}}
	}
	h, err := New(Options{
		Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy),
		Keys: keys, Workers: hub,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	f := &fixture{
		t: t, url: server.URL, issuerURL: issuer.URL(),
		alice: issuer.Mint(issuertest.Claims{Sub: "admin"}),
		bob:   issuer.Mint(issuertest.Claims{Sub: "bob"}),
		h:     h, c: c,
	}
	return &keyed{fixture: f, keys: keys, hub: hub, signer: signer}
}

type mintedKey struct {
	Token string `json:"token"`
	JTI   string `json:"jti"`
	Exp   string `json:"exp"`
}

// TestEnvironmentKeyRoutes is the mint and the revoke of spec 021 over HTTP:
// the route a data plane is installed from, and the one that ends a key.
func TestEnvironmentKeyRoutes(t *testing.T) {
	p := setupKeyed(t, nil)

	var first mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &first); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	switch {
	case first.Token == "":
		t.Fatalf("the mint returned no key")
	case first.JTI == "":
		t.Errorf("the mint returned no jti, and the jti is what revokes the key")
	case first.Exp == "":
		t.Errorf("the mint returned no expiry")
	}
	if _, err := time.Parse(time.RFC3339, first.Exp); err != nil {
		t.Errorf("the expiry %q is not an instant: %v", first.Exp, err)
	}

	// The key names its environment and nothing else. It reaches the worker
	// routes of that environment and no route that decides on a subject.
	caller, err := p.verify(t, first.Token)
	if err != nil {
		t.Fatalf("the minted key does not verify: %v", err)
	}
	environment, isEnvironment := caller.Environment()
	if !isEnvironment || environment != "default" {
		t.Errorf("the key names %q as an environment (%t), want default", environment, isEnvironment)
	}
	p.request(http.MethodGet, "/v1/sandboxes", first.Token, "", http.StatusForbidden)

	// An environment holds several keys, so a worker and a gateway each
	// carry their own and one is revoked without ending the others.
	var second mintedKey
	if err = json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &second); err != nil {
		t.Fatalf("the second mint's answer did not decode: %v", err)
	}
	if second.JTI == first.JTI || second.Token == first.Token {
		t.Errorf("two mints returned one key")
	}

	// The revocation ends the first key on its next request and leaves the
	// second one working.
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+first.JTI, p.alice, "", http.StatusNoContent)
	if _, err = p.verify(t, first.Token); err == nil {
		t.Errorf("the revoked key still verifies")
	}
	if _, err = p.verify(t, second.Token); err != nil {
		t.Errorf("revoking one key ended another: %v", err)
	}
	// A revocation is idempotent: a recovery that retried revokes a jti it
	// already revoked.
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+first.JTI, p.alice, "", http.StatusNoContent)
}

// TestEnvironmentKeyList is spec 021's listing of an environment's keys over
// HTTP: every key minted for the environment, oldest first and a page at a
// time, with its jti, when it was minted, its exp, whether it is revoked and
// who minted it, and never the token. It is an administrator's act, as the
// mint and the revocation are.
func TestEnvironmentKeyList(t *testing.T) {
	p := setupKeyed(t, nil)
	var minted []mintedKey
	for range 3 {
		var key mintedKey
		if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &key); err != nil {
			t.Fatalf("the mint's answer did not decode: %v", err)
		}
		minted = append(minted, key)
	}
	revoked := minted[1].JTI
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+revoked, p.alice, "", http.StatusNoContent)
	slices.SortFunc(minted, func(a, b mintedKey) int { return strings.Compare(a.JTI, b.JTI) })

	type page struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"next"`
	}
	read := func(query string) (page, string) {
		t.Helper()
		body := p.request(http.MethodGet, "/v1/environments/default/keys"+query, p.alice, "", http.StatusOK)
		var got page
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("the list did not decode: %v", err)
		}
		return got, string(body)
	}
	all, body := read("")
	if len(all.Items) != len(minted) || all.Next != "" {
		t.Fatalf("the list carries %d keys and the cursor %q, want all %d on one page", len(all.Items), all.Next, len(minted))
	}
	for i, item := range all.Items {
		want := minted[i]
		if item["jti"] != want.JTI || item["exp"] != want.Exp {
			t.Errorf("key %d reads %v, want the jti %s and the exp %s its mint returned", i, item, want.JTI, want.Exp)
		}
		if at, _ := item["mintedAt"].(string); at == "" {
			t.Errorf("key %d carries no mint time: %v", i, item)
		} else if _, err := time.Parse(time.RFC3339, at); err != nil {
			t.Errorf("key %d was minted at %q, which is not an instant: %v", i, at, err)
		}
		if item["mintedBy"] != p.issuerURL+"|admin" {
			t.Errorf("key %d was minted by %v, want the administrator who asked", i, item["mintedBy"])
		}
		isRevoked := want.JTI == revoked
		if item["revoked"] != isRevoked {
			t.Errorf("key %d reads revoked %v, want %v", i, item["revoked"], isRevoked)
		}
		if _, marked := item["revokedAt"]; marked != isRevoked {
			t.Errorf("key %d carries revokedAt %v while revoked is %v", i, item["revokedAt"], isRevoked)
		}
		if _, leaked := item["token"]; leaked {
			t.Errorf("key %d carries a token member", i)
		}
	}
	for _, key := range minted {
		if strings.Contains(body, key.Token) {
			t.Fatalf("the list carries the token of %s", key.JTI)
		}
	}

	first, _ := read("?limit=2")
	if len(first.Items) != 2 || first.Next != minted[1].JTI {
		t.Fatalf("the first page carries %d keys and the cursor %q, want two and %s", len(first.Items), first.Next, minted[1].JTI)
	}
	rest, _ := read("?limit=2&cursor=" + first.Next)
	if len(rest.Items) != 1 || rest.Items[0]["jti"] != minted[2].JTI || rest.Next != "" {
		t.Errorf("the second page is %v with the cursor %q, want the newest key and no cursor", rest.Items, rest.Next)
	}

	p.request(http.MethodGet, "/v1/environments/default/keys", p.bob, "", http.StatusForbidden)
	p.request(http.MethodGet, "/v1/environments/eu-gpu/keys", p.alice, "", http.StatusNotFound)
	p.request(http.MethodGet, "/v1/environments/default/keys?limit=0", p.alice, "", http.StatusBadRequest)
	p.request(http.MethodGet, "/v1/environments/default/keys?limit=201", p.alice, "", http.StatusBadRequest)
	p.request(http.MethodGet, "/v1/environments/default/keys", "", "", http.StatusUnauthorized)
}

// TestEnvironmentKeyRefusals holds who may mint and what may be minted for.
func TestEnvironmentKeyRefusals(t *testing.T) {
	p := setupKeyed(t, nil)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		token  func() string
		status int
	}{
		{"a subject the owner policy does not make an admin", http.MethodPost,
			"/v1/environments/default/keys", func() string { return p.bob }, http.StatusForbidden},
		{"an environment this control plane does not hold", http.MethodPost,
			"/v1/environments/eu-gpu/keys", func() string { return p.alice }, http.StatusNotFound},
		{"a revocation on an environment this control plane does not hold", http.MethodDelete,
			"/v1/environments/eu-gpu/keys/01JABC", func() string { return p.alice }, http.StatusNotFound},
		{"no bearer at all", http.MethodPost,
			"/v1/environments/default/keys", func() string { return "" }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.request(tc.method, tc.path, tc.token(), "", tc.status)
		})
	}
}

// TestEnvironmentKeyRoutesWithoutASigner holds what a control plane that
// signs nothing answers: the route is refused as a capability rather than
// failing somewhere below.
func TestEnvironmentKeyRoutesWithoutASigner(t *testing.T) {
	f := setup(t, nil)
	f.request(http.MethodPost, "/v1/environments/default/keys", f.alice, "", http.StatusUnprocessableEntity)
	f.request(http.MethodDelete, "/v1/environments/default/keys/01JABC", f.alice, "", http.StatusUnprocessableEntity)
	f.request(http.MethodGet, "/v1/environments/default/keys", f.alice, "", http.StatusUnprocessableEntity)
}

// TestTheDefaultEnvironmentReads holds the read and the list of the
// environment this control plane drives itself.
func TestTheDefaultEnvironmentReads(t *testing.T) {
	p := setupKeyed(t, nil)
	var obj v1.Environment
	if err := json.Unmarshal(p.request(http.MethodGet, "/v1/environments/default", p.alice, "", http.StatusOK), &obj); err != nil {
		t.Fatalf("the environment did not decode: %v", err)
	}
	switch {
	case obj.Kind != v1.KindEnvironment:
		t.Errorf("the object is a %q", obj.Kind)
	case obj.Metadata.Name != "default":
		t.Errorf("the environment is named %q", obj.Metadata.Name)
	case obj.Spec.Mode != v1.EnvironmentInprocess:
		t.Errorf("the control plane's own environment is %q, want %q", obj.Spec.Mode, v1.EnvironmentInprocess)
	case obj.Status.Driver != "native":
		t.Errorf("the environment reports the driver %q, want native", obj.Status.Driver)
	case obj.Status.Phase != v1.EnvironmentReady:
		t.Errorf("the environment reports the phase %q", obj.Status.Phase)
	}

	var list struct {
		Items []v1.Environment `json:"items"`
	}
	if err := json.Unmarshal(p.request(http.MethodGet, "/v1/environments", p.alice, "", http.StatusOK), &list); err != nil {
		t.Fatalf("the list did not decode: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != "default" {
		t.Errorf("the list is %v, want the one environment this control plane drives", list.Items)
	}
	p.request(http.MethodGet, "/v1/environments/eu-gpu", p.alice, "", http.StatusNotFound)
}

// TestWorkerRegistrationRoute is spec 021's registration: a worker with a
// valid key registers and receives a wrk_ id, and one whose driver or
// isolation class differs from the environment's is refused.
func TestWorkerRegistrationRoute(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)

	body := `{"driver":"podman","isolation":"container","capabilities":{"egress":null,"files":true}}`
	var registered remote.Registered
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/self/workers", key, body, http.StatusCreated), &registered); err != nil {
		t.Fatalf("the registration answer did not decode: %v", err)
	}
	if !strings.HasPrefix(registered.Worker, "wrk_") {
		t.Errorf("the worker id is %q, want a wrk_ id", registered.Worker)
	}
	if registered.Environment != "default" {
		t.Errorf("the registration names the environment %q", registered.Environment)
	}
	if registered.HeartbeatInterval == "" || registered.Lease == "" {
		t.Errorf("the registration names no heartbeat interval or lease: %+v", registered)
	}

	// One environment is one data plane: a second worker reporting another
	// driver is refused rather than admitted beside the first.
	mismatch := `{"driver":"k8s","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key, mismatch, http.StatusUnprocessableEntity)
	isolation := `{"driver":"podman","isolation":"vm"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key, isolation, http.StatusUnprocessableEntity)
	// A registration naming no driver is a defect on the worker's side.
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{}`, http.StatusBadRequest)
	// A body the registration does not have is refused rather than ignored.
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{"driver":"podman","isolation":"container","pool":2}`, http.StatusBadRequest)
}

// TestEnvironmentKeyReachesItsOwnEnvironmentOnly holds the rule the gateway
// stream already applies: a key names one environment, and a path naming
// another is refused.
func TestEnvironmentKeyReachesItsOwnEnvironmentOnly(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	body := `{"driver":"podman","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/eu-gpu/workers", key, body, http.StatusForbidden)
	// A key reaches no route that decides on a subject, whichever it is.
	p.request(http.MethodGet, "/v1/environments", key, "", http.StatusForbidden)
	p.request(http.MethodPost, "/v1/environments/default/keys", key, "", http.StatusForbidden)
}

// TestRevokedKeyIsRefusedOnTheWorkerRoutes holds spec 021's rule that a key
// revoked through the route stops working at once, on the data plane routes
// as everywhere else.
func TestRevokedKeyIsRefusedOnTheWorkerRoutes(t *testing.T) {
	p := setupKeyed(t, nil)
	var key mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &key); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	body := `{"driver":"podman","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key.Token, body, http.StatusCreated)
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+key.JTI, p.alice, "", http.StatusNoContent)
	p.request(http.MethodPost, "/v1/environments/self/workers", key.Token, body, http.StatusUnauthorized)
	p.request(http.MethodGet, "/v1/environments/self/operations", key.Token, "", http.StatusUnauthorized)
}

// TestWorkerRoutesWithoutAHub holds what a control plane that serves no
// worker environment answers on the two routes an environment key reaches.
func TestWorkerRoutesWithoutAHub(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	p.h.(*handler).Workers = nil
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{"driver":"native","isolation":"none"}`, http.StatusUnprocessableEntity)
}

// mintKey takes one environment key through the route, which is how a data
// plane is keyed in a deployment.
func (p *keyed) mintKey(t *testing.T) string {
	t.Helper()
	var key mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &key); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	return key.Token
}

// verify runs one bearer through the handler's own verifier, which is what
// every route reads and what the revocation list answers for.
func (p *keyed) verify(t *testing.T, token string) (auth.Caller, error) {
	t.Helper()
	return p.h.(*handler).Verifier.VerifyContext(t.Context(), token)
}

// TestWorkerStreamRoute opens the stream a worker holds: the upgrade, the
// hello that binds it to its registration, and the environment reporting it
// as connected. The control plane dials nothing; everything here travels on
// the connection the worker opened.
func TestWorkerStreamRoute(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	var registered remote.Registered
	body := `{"driver":"native","isolation":"none","capabilities":{"files":true}}`
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/self/workers", key, body, http.StatusCreated), &registered); err != nil {
		t.Fatalf("the registration answer did not decode: %v", err)
	}

	dialer := &websocket.Dialer{Subprotocols: []string{remote.Protocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(p.url, "http")+"/v1/environments/self/operations",
		http.Header{"Authorization": []string{"Bearer " + key}})
	if err != nil {
		if res != nil {
			_ = res.Body.Close()
		}
		t.Fatalf("the worker's stream did not open: %v", err)
	}
	if res != nil {
		_ = res.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	if conn.Subprotocol() != remote.Protocol {
		t.Fatalf("the server negotiated %q, want %q", conn.Subprotocol(), remote.Protocol)
	}
	hello, err := remote.EncodeMessage(remote.NoOperation,
		remote.Message{Type: remote.MessageHello, Worker: registered.Worker})
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, hello); err != nil {
		t.Fatalf("the hello was not sent: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connected := 0
		for _, w := range p.hub.Workers("default") {
			if w.Connected {
				connected++
			}
		}
		if connected == 1 {
			// The environment reports it once the phase loop has run, which
			// is the one writer of an environment's observed half.
			if err = p.c.Phases(t.Context()); err != nil {
				t.Fatalf("the phase loop did not run: %v", err)
			}
			var obj v1.Environment
			if err = json.Unmarshal(p.request(http.MethodGet, "/v1/environments/default", p.alice, "", http.StatusOK), &obj); err != nil {
				t.Fatalf("the environment did not decode: %v", err)
			}
			if obj.Status.Workers != 1 {
				t.Errorf("the environment reports %d workers", obj.Status.Workers)
			}
			if obj.Status.LastHeartbeat.IsZero() {
				t.Errorf("the environment reports a worker and no heartbeat")
			}
			// One operation down proves the other direction: the hub writes
			// on the stream the worker opened, and the worker reads it.
			go func() {
				_, _ = p.hub.Transport("default").Open(t.Context(), remote.OpInspect, remote.Request{ID: "sbx_1"})
			}()
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				t.Fatalf("the operation never reached the worker: %v", readErr)
			}
			_, stream, payload, decodeErr := remote.DecodeFrame(frame)
			if decodeErr != nil {
				t.Fatalf("the frame did not decode: %v", decodeErr)
			}
			if stream != remote.StreamControl {
				t.Fatalf("the operation rode on sub-stream %d", stream)
			}
			message, decodeErr := remote.DecodeMessage(payload)
			if decodeErr != nil {
				t.Fatalf("the message did not decode: %v", decodeErr)
			}
			if message.Type != remote.MessageOperation || remote.OperationType(message) != remote.OpInspect {
				t.Errorf("the worker was handed %q/%q", message.Type, remote.OperationType(message))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the worker's hello never bound its stream")
}

// TestWorkerStreamRefusesAnotherProtocol holds the subprotocol rule: a client
// that does not ask for this vocabulary is refused rather than answered in
// something it cannot read.
func TestWorkerStreamRefusesAnotherProtocol(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	p.request(http.MethodPost, "/v1/environments/self/workers", key,
		`{"driver":"native","isolation":"none"}`, http.StatusCreated)

	dialer := &websocket.Dialer{Subprotocols: []string{"cella.worker.v0"}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(p.url, "http")+"/v1/environments/self/operations",
		http.Header{"Authorization": []string{"Bearer " + key}})
	if res != nil {
		_ = res.Body.Close()
	}
	if err == nil {
		// Some clients complete the handshake and learn on the first read.
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, readErr := conn.ReadMessage(); readErr == nil {
			t.Errorf("a client asking for another vocabulary was served")
		}
	}
}

// TestEnvironmentKeyEventsAreJournaled holds design 009's two key records: a
// mint and a revocation each name the jti and carry no key.
func TestEnvironmentKeyEventsAreJournaled(t *testing.T) {
	p := setupKeyed(t, nil)
	journal, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	p.h.(*handler).Events = events.NewEmitter(store.EventJournal(journal, store.Journaled), slog.New(slog.DiscardHandler))

	var minted mintedKey
	if err = json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &minted); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+minted.JTI, p.alice, "", http.StatusNoContent)

	records := environmentRecords(t, journal)
	if len(records) != 2 {
		t.Fatalf("the two key acts wrote %d records", len(records))
	}
	if records[0].Type != events.TypeEnvironmentKeyed || records[1].Type != events.TypeEnvironmentKeyRevoked {
		t.Errorf("the records are %s and %s", records[0].Type, records[1].Type)
	}
	for _, r := range records {
		if r.Object.Kind != events.KindEnvironment || r.Object.Name != "default" {
			t.Errorf("the record is about %+v, want the default environment", r.Object)
		}
		if !strings.Contains(string(r.Data), minted.JTI) {
			t.Errorf("the record does not name the jti: %s", r.Data)
		}
		if strings.Contains(string(r.Data), minted.Token) {
			t.Errorf("the record carries the key itself")
		}
	}
}

// environmentRecords reads the journal back, oldest first, which is the
// order the acts happened in.
func environmentRecords(t *testing.T, s *memory.Store) []events.Record {
	t.Helper()
	var rows []store.Event
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		var err error
		rows, _, err = tx.Journal().ByObject(t.Context(), "default", store.Page{Limit: 50})
		return err
	}); err != nil {
		t.Fatalf("the journal did not answer: %v", err)
	}
	out := make([]events.Record, 0, len(rows))
	for _, row := range slices.Backward(rows) {
		record, err := events.Rebuild(row.Payload, row.ID, row.Seq, row.Type, row.At)
		if err != nil {
			t.Fatalf("the record %s did not rebuild: %v", row.ID, err)
		}
		out = append(out, record)
	}
	return out
}

// TestEnvironmentRoutesUnderADenyingAuthorizer holds that every environment
// route asks the authorizer first: a subject the operator's policy refuses
// reads nothing and mints nothing.
func TestEnvironmentRoutesUnderADenyingAuthorizer(t *testing.T) {
	p := setupKeyed(t, denyEverything{})
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"the read", http.MethodGet, "/v1/environments/default"},
		{"the list", http.MethodGet, "/v1/environments"},
		{"the mint", http.MethodPost, "/v1/environments/default/keys"},
		{"the revocation", http.MethodDelete, "/v1/environments/default/keys/01JABC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.request(tc.method, tc.path, p.alice, "", http.StatusForbidden)
		})
	}
}

// TestRevocationNamesAJTI holds that a revocation without one is refused
// rather than sweeping nothing in silence.
func TestRevocationNamesAJTI(t *testing.T) {
	p := setupKeyed(t, nil)
	p.request(http.MethodDelete, "/v1/environments/default/keys/%20", p.alice, "", http.StatusBadRequest)
}

// denyEverything is an operator policy that refuses every action, which is
// what an authorizer of a locked-down installation answers a caller with no
// grant at all.
type denyEverything struct{}

func (denyEverything) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Allow: false, Reason: "this installation grants nothing"}, nil
}
