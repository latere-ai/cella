// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// combinedCA is an authority as CELLA_EGRESS_CA_KEY carries it: the
// certificate and the private key in one PEM value.
func combinedCA(t *testing.T) string {
	t.Helper()
	_, certPEM, keyPEM, err := pkgegress.GenerateCA(CACommonName)
	if err != nil {
		t.Fatal(err)
	}
	return string(certPEM) + string(keyPEM)
}

func TestAuthority(t *testing.T) {
	t.Run("generatedWhenTheOperatorSuppliedNone", func(t *testing.T) {
		ca, certPEM, err := authority("")
		if err != nil || ca == nil {
			t.Fatalf("authority = %v, %v", ca, err)
		}
		if !strings.Contains(certPEM, "BEGIN CERTIFICATE") {
			t.Fatalf("the generated authority = %q", certPEM)
		}
	})
	t.Run("loadedFromTheOperatorsOwn", func(t *testing.T) {
		combined := combinedCA(t)
		ca, certPEM, err := authority(combined)
		if err != nil || ca == nil {
			t.Fatalf("authority = %v, %v", ca, err)
		}
		// The same value loads twice into the same authority, which is what
		// lets two gateways of one environment sign for one another's
		// sandboxes.
		_, again, err := authority(combined)
		if err != nil || again != certPEM {
			t.Fatalf("a second load = %q, %v", again, err)
		}
		// The key stays with the gateway: what the control plane projects is
		// the certificate alone.
		if strings.Contains(certPEM, "PRIVATE KEY") {
			t.Fatal("the projected authority carries the private key")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		_, certPEM, keyPEM, err := pkgegress.GenerateCA(CACommonName)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ name, value string }{
			{"nothingOfThePair", "not a certificate"},
			{"theCertificateAlone", string(certPEM)},
			{"theKeyAlone", string(keyPEM)},
			{"aKeyThatIsNoKey", string(certPEM) + "-----BEGIN EC PRIVATE KEY-----\nQQ==\n-----END EC PRIVATE KEY-----\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, _, err := authority(tc.value); err == nil {
					t.Fatal("the value was accepted as an authority")
				}
			})
		}
	})
}

func TestNewRefusals(t *testing.T) {
	key := func(t *testing.T) string { return token(t, map[string]any{"sub": "environment:default"}) }
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"aKeyThatNamesNoEnvironment", Options{Key: "abc", URL: "http://127.0.0.1:1"}, "CELLA_ENVIRONMENT_KEY"},
		{"aControlPlaneThatIsNoURL", Options{Key: key(t), URL: "cella.example.com"}, "CELLA_URL"},
		{"anAuthorityThatIsNotOne", Options{Key: key(t), URL: "http://127.0.0.1:1", CAPEM: "nonsense"}, "CELLA_EGRESS_CA_KEY"},
		{"anUpstreamBundleThatIsNotOne", Options{Key: key(t), URL: "http://127.0.0.1:1", UpstreamCAPEM: "nonsense"}, "CELLA_EGRESS_CA_BUNDLE"},
		{"aProxyDoorThatCannotBind", Options{Key: key(t), URL: "http://127.0.0.1:1", ProxyAddr: occupied.Addr().String()}, "CELLA_EGRESS_PROXY_ADDR"},
		{"aReverseDoorThatCannotBind", Options{Key: key(t), URL: "http://127.0.0.1:1", ProxyAddr: "127.0.0.1:0", ReverseAddr: occupied.Addr().String()}, "CELLA_EGRESS_REVERSE_ADDR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, err := New(t.Context(), tc.opts)
			if err == nil {
				gateway.Close(t.Context())
				t.Fatal("the configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestRunServesBothDoorsUntilTheContextEnds is the role's own lifecycle: two
// bound doors, one stream, and a clean stop.
func TestRunServesBothDoorsUntilTheContextEnds(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{}})
	}
	ready := make(chan struct{})
	gateway, err := New(t.Context(), Options{
		URL: p.server.URL, Key: token(t, map[string]any{"sub": "environment:default"}),
		ProxyAddr: "127.0.0.1:0", ReverseAddr: "127.0.0.1:0",
		Ready: func() { close(ready) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if gateway.Environment() != "default" {
		t.Fatalf("environment = %q", gateway.Environment())
	}
	if !strings.Contains(gateway.CAPEM(), "BEGIN CERTIFICATE") {
		t.Fatal("the gateway generated no authority")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- gateway.Run(ctx) }()

	// The proxy door answers, which is what says the listener is served.
	if got := connect(t, gateway.ProxyAddr(), "api.example.com:443", ""); got != http.StatusProxyAuthRequired {
		t.Fatalf("the proxy door answered %d", got)
	}
	resp, err := http.Get("http://" + gateway.ReverseAddr() + "/api.example.com/things")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the reverse door answered %d", resp.StatusCode)
	}
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a clean stop", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
}

func TestCredentialAuthenticate(t *testing.T) {
	s := newStore()
	s.Apply(boundary("sbx_a", 1, v1.EgressOpen))
	a := credentialAuth{store: s}
	principal, ok := a.Authenticate("Basic " + basic(egress.ProxyUser, "credential-sbx_a"))
	if !ok || principal != egress.Principal("sbx_a") {
		t.Fatalf("Authenticate = %q, %v", principal, ok)
	}
	if _, ok = a.Authenticate("Basic " + basic(egress.ProxyUser, "wrong")); ok {
		t.Fatal("a credential no map carries authenticated")
	}
}

func TestGatewayID(t *testing.T) {
	if first, second := gatewayID(), gatewayID(); first == second || !strings.HasPrefix(first, "gw-") {
		t.Fatalf("gatewayID gave %q and %q", first, second)
	}
}
