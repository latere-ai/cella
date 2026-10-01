// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// uses is a report sink and a clock a test moves, installed on a harness's
// store the way the gateway installs its stream and its clock.
type uses struct {
	mu   sync.Mutex
	now  time.Time
	sent []egress.Use
}

func (u *uses) install(s *store) {
	s.report = func(use egress.Use) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.sent = append(u.sent, use)
	}
	s.now = func() time.Time {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.now
	}
}

func (u *uses) advance(d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.now = u.now.Add(d)
}

func (u *uses) taken() []egress.Use {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]egress.Use(nil), u.sent...)
}

// TestTheGatewayReportsASubstitution: a substitution on either door is
// reported by the sandbox's principal and the secret's id, once per pair per
// resolution; a request that carried no placeholder, or carried it toward a
// host out of scope, is not a use; a snapshot forgets every pair and a purge
// forgets the principal's; and an entry from a control plane that sends no
// id is substituted and not reported.
func TestTheGatewayReportsASubstitution(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	github, search, legacy := egress.MintPlaceholder(), egress.MintPlaceholder(), egress.MintPlaceholder()
	first := bearerEntry(github, "api.example.com")
	first.ID = "sec_github"
	second := bearerEntry(search, "search.example.com")
	second.ID, second.Secret = "sec_search", "search"
	unnamed := bearerEntry(legacy, "legacy.example.com")
	unnamed.Secret = "legacy"
	h := substituting(t, hostPort(t, upstream.URL), trust, first, second, unnamed)
	clock := &uses{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	clock.install(h.store)
	principal := "sandbox:sbx_a"

	expect := func(t *testing.T, want ...egress.Use) {
		t.Helper()
		got := clock.taken()
		if len(got) != len(want) {
			t.Fatalf("the gateway reported %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("report %d is %+v, want %+v", i, got[i], want[i])
			}
		}
	}
	use := func(secret string) egress.Use {
		return egress.Use{Principal: principal, Secret: secret, At: clock.now}
	}

	// The reverse door: this role builds the request and substitutes it.
	got := decodeSeen(t, reverseBody(t, h, "/api.example.com/v1/things", "Authorization", "Bearer "+github))
	if got.Authorization != "Bearer ghp_real" {
		t.Fatalf("the upstream saw %q", got.Authorization)
	}
	want := []egress.Use{use("sec_github")}
	expect(t, want...)

	clock.advance(v1.SecretLastUsedResolution - time.Second)
	reverseBody(t, h, "/api.example.com/v1/things", "Authorization", "Bearer "+github)
	expect(t, want...)

	// A request with no placeholder in it is not a use, however long after.
	clock.advance(time.Hour)
	reverseBody(t, h, "/api.example.com/v1/things", "Authorization", "Bearer something-of-its-own")
	expect(t, want...)

	// The proxy door substitutes inside the engine, over the registry's
	// map, which is what pkg/egress.Gateway.forward calls per request.
	engine, _ := h.store.Registry().Get(principal)
	req := outbound(t, "https://search.example.com/q", map[string]string{"Authorization": "Bearer " + search})
	if _, err := pkgegress.SubstituteHTTPRequestContext(t.Context(), "search.example.com", req, engine); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer ghp_real" {
		t.Fatalf("the engine left %q", req.Header.Get("Authorization"))
	}
	want = append(want, use("sec_search"))
	expect(t, want...)
	// Toward a host out of the secret's scope the placeholder leaves as it
	// is, and nothing was used.
	clock.advance(v1.SecretLastUsedResolution)
	req = outbound(t, "https://elsewhere.example.com/q", map[string]string{"Authorization": "Bearer " + search})
	if _, err := pkgegress.SubstituteHTTPRequestContext(t.Context(), "elsewhere.example.com", req, engine); err != nil {
		t.Fatal(err)
	}
	expect(t, want...)

	// At the resolution the pair is reported again.
	reverseBody(t, h, "/api.example.com/v1/things", "Authorization", "Bearer "+github)
	want = append(want, use("sec_github"))
	expect(t, want...)

	// A snapshot is a stream made whole, maybe to another writer: the next
	// use of every pair is reported at once.
	held, _ := h.store.Map(principal)
	h.store.Replace([]egress.Map{held})
	reverseBody(t, h, "/api.example.com/v1/things", "Authorization", "Bearer "+github)
	want = append(want, use("sec_github"))
	expect(t, want...)

	// An entry with no id is substituted and reported by nothing.
	got = decodeSeen(t, reverseBody(t, h, "/legacy.example.com/v1/things", "Authorization", "Bearer "+legacy))
	if got.Authorization != "Bearer ghp_real" {
		t.Fatalf("an entry with no id was not substituted: %q", got.Authorization)
	}
	expect(t, want...)

	// A purge takes the principal's pairs with it.
	h.store.Remove(principal)
	h.store.reportedMu.Lock()
	left := len(h.store.reported)
	h.store.reportedMu.Unlock()
	if left != 0 {
		t.Fatalf("a purged principal left %d reported pairs", left)
	}
}

// TestAFailedMintIsNotAUse: an oauth entry whose token endpoint refuses fails
// the request, as it did before, and reports nothing.
func TestAFailedMintIsNotAUse(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer tokens.Close()
	placeholder := egress.MintPlaceholder()
	h := substituting(t, hostPort(t, upstream.URL), trust, egress.Entry{
		ID: "sec_vendor", Secret: "vendor", Kind: "oauth_client_credentials", Placeholder: placeholder,
		Hosts: []string{"vendor.example.com"}, Ports: []int{443},
		Header: "Authorization", Scheme: egress.SchemeBearer, Value: "client-id:client-secret",
		OAuth: &egress.OAuth{TokenURL: tokens.URL},
	})
	clock := &uses{now: time.Now().UTC()}
	clock.install(h.store)
	resp, _ := reverse(t, h.reverse.URL+"/vendor.example.com/v1/things", "c",
		map[string]string{"Authorization": "Bearer " + placeholder})
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a request whose token could not be minted was sent")
	}
	if got := clock.taken(); len(got) != 0 {
		t.Fatalf("a failed mint was reported as a use: %+v", got)
	}
}

// TestAFullUseBufferDoesNotHoldTheRequest: with nothing draining the stream,
// every request is still answered, and the buffer keeps its newest uses.
func TestAFullUseBufferDoesNotHoldTheRequest(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	placeholder := egress.MintPlaceholder()
	entry := bearerEntry(placeholder, "api.example.com")
	entry.ID = "sec_github"
	h := substituting(t, hostPort(t, upstream.URL), trust, entry)
	client := &syncClient{uses: make(chan egress.Use, 2), log: slog.Default()}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := &uses{now: start}
	clock.install(h.store)
	h.store.report = client.Use
	for range 5 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.reverse.URL+"/api.example.com/v1/things", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(egress.CredentialHeader, "c")
		req.Header.Set("Authorization", "Bearer "+placeholder)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("a request with a full use buffer: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if decodeSeen(t, string(body)).Authorization != "Bearer ghp_real" {
			t.Fatalf("the upstream answered %q", body)
		}
		clock.advance(v1.SecretLastUsedResolution)
	}
	if len(client.uses) != 2 {
		t.Fatalf("the buffer holds %d uses, want its cap of 2", len(client.uses))
	}
	oldest := <-client.uses
	if want := start.Add(3 * v1.SecretLastUsedResolution); !oldest.At.Equal(want) {
		t.Fatalf("the buffer holds the use of %v, want the newest two from %v", oldest.At, want)
	}
}

// TestUsesGoUpTheSameStream: a use is a frame on the stream the gateway
// already holds, as a record is.
func TestUsesGoUpTheSameStream(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{}})
	}
	c := runClient(t, p, newStore(nil), "")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c.Use(egress.Use{Principal: egress.Principal("sbx_a"), Secret: "sec_github", At: at})
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.uses) == 1 && p.uses[0].Secret == "sec_github" && p.uses[0].At.Equal(at)
	}, "the use to reach the control plane")
}
