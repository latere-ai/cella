// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/internal/auth"
)

// TestTheAuthorizerHearsTheRequestID: the request block of a question names
// the id the answer carries, a minted one and a client's own alike, so an
// operator's authorizer and cellad's log name one request.
func TestTheAuthorizerHearsTheRequestID(t *testing.T) {
	var mu sync.Mutex
	var heard []authz.Caller
	owner := &auth.OwnerPolicy{DefaultEnvironment: "default"}
	f := setup(t, decisionFunc(func(ctx context.Context, r authz.Request) (authz.Decision, error) {
		mu.Lock()
		heard = append(heard, r.Request)
		mu.Unlock()
		return owner.Authorize(ctx, r)
	}))
	for _, sent := range []string{"", "req_from-a-client"} {
		req, err := http.NewRequest(http.MethodGet, f.url+"/v1/sandboxes", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		req.Header.Set("User-Agent", "requestinfo-test/1")
		if sent != "" {
			req.Header.Set(RequestIDHeader, sent)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("the list answered %d", res.StatusCode)
		}
		id := res.Header.Get(RequestIDHeader)
		mu.Lock()
		if len(heard) == 0 {
			mu.Unlock()
			t.Fatal("the list asked the authorizer nothing")
		}
		last := heard[len(heard)-1]
		mu.Unlock()
		if last.ID == "" || last.ID != id {
			t.Errorf("sent %q: the authorizer heard the request id %q, the answer carries %q", sent, last.ID, id)
		}
		if sent != "" && id != sent {
			t.Errorf("a client id within the rule was replaced: sent %q, got %q", sent, id)
		}
		if last.IP != "127.0.0.1" || last.UserAgent != "requestinfo-test/1" {
			t.Errorf("the authorizer heard %q from %q", last.UserAgent, last.IP)
		}
	}
}

// TestClientIPReadsThroughTrustedProxiesOnly is CELLA_TRUSTED_PROXIES: the
// peer is the caller unless it is inside a trusted range, and then the
// rightmost X-Forwarded-For entry outside the ranges is. A header from any
// other peer, and every header when no range is set, is never read.
func TestClientIPReadsThroughTrustedProxiesOnly(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	for _, tc := range []struct {
		name      string
		peer      string
		forwarded []string
		trusted   []netip.Prefix
		want      string
	}{
		{"no ranges keeps the peer and reads no header", "10.1.2.3:4567", []string{"198.51.100.7"}, nil, "10.1.2.3"},
		{"a header from an untrusted peer is ignored", "203.0.113.9:4567", []string{"198.51.100.7"}, trusted, "203.0.113.9"},
		{"a trusted proxy names the client", "10.1.2.3:4567", []string{"198.51.100.7"}, trusted, "198.51.100.7"},
		{"a chain through two trusted proxies names the client", "10.1.2.3:4567", []string{"198.51.100.7, 10.9.9.9"}, trusted, "198.51.100.7"},
		{"what the client wrote left of its own address is not read", "10.1.2.3:4567", []string{"192.0.2.1, 198.51.100.7, 10.9.9.9"}, trusted, "198.51.100.7"},
		{"the chain spans header lines", "10.1.2.3:4567", []string{"192.0.2.1, 198.51.100.7", "10.9.9.9"}, trusted, "198.51.100.7"},
		{"an entry with a port", "10.1.2.3:4567", []string{"198.51.100.7:5555"}, trusted, "198.51.100.7"},
		{"an IPv6 peer and entry with a port", "[fd00::1]:4567", []string{"[2001:db8::7]:5555"}, trusted, "2001:db8::7"},
		{"a mapped IPv4 hop meets an IPv4 range", "10.1.2.3:4567", []string{"198.51.100.7, ::ffff:10.9.9.9"}, trusted, "198.51.100.7"},
		{"a mapped IPv4 peer meets an IPv4 range", "[::ffff:10.1.2.3]:4567", []string{"198.51.100.7"}, trusted, "198.51.100.7"},
		{"a trusted peer with no header keeps the peer", "10.1.2.3:4567", nil, trusted, "10.1.2.3"},
		{"only trusted hops keeps the peer", "10.1.2.3:4567", []string{"10.9.9.9"}, trusted, "10.1.2.3"},
		{"an entry that is no address keeps the peer", "10.1.2.3:4567", []string{"198.51.100.7, unknown"}, trusted, "10.1.2.3"},
		{"an empty entry keeps the peer", "10.1.2.3:4567", []string{"198.51.100.7,"}, trusted, "10.1.2.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.forwarded {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, tc.trusted); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAForwardKeepsTheChainItArrivedWith: a standby hands the writer the
// X-Forwarded-For the hops before it wrote, with its own peer appended, so the
// writer reads the caller's address through CELLA_TRUSTED_PROXIES as it reads
// a request that reached it directly.
func TestAForwardKeepsTheChainItArrivedWith(t *testing.T) {
	writer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Join(r.Header.Values("X-Forwarded-For"), ", "))
	}))
	t.Cleanup(writer.Close)
	front := httptest.NewServer(NewForwarder(ForwarderOptions{Writer: fixedWriter(writer.URL), Hold: time.Second}))
	t.Cleanup(front.Close)
	req, err := http.NewRequest(http.MethodGet, front.URL+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "198.51.100.7, 127.0.0.1"; got != want {
		t.Fatalf("the writer read X-Forwarded-For %q, want %q", got, want)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
	r.RemoteAddr = "127.0.0.1:4567"
	r.Header.Set("X-Forwarded-For", string(body))
	if got := clientIP(r, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}); got != "198.51.100.7" {
		t.Fatalf("the writer reads the caller as %q through the standby", got)
	}
}
