// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// stubPlane is a control plane that speaks the gateway protocol: it takes the
// hello, sends what a test tells it to, and keeps what came back.
type stubPlane struct {
	server *httptest.Server
	down   func(conn *websocket.Conn)

	mu          sync.Mutex
	hellos      []egress.Hello
	acks        []egress.Ack
	records     []egress.Record
	bearer      string
	subprotocol string
	connections int
	// closeAfterHello ends the first connection right after the hello, so a
	// test drives the reconnect.
	closeAfterHello bool
}

func newStubPlane(t *testing.T) *stubPlane {
	t.Helper()
	p := &stubPlane{}
	upgrader := websocket.Upgrader{Subprotocols: []string{egress.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.bearer = r.Header.Get("Authorization")
		p.connections++
		closeEarly := p.closeAfterHello && p.connections == 1
		p.mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		p.mu.Lock()
		p.subprotocol = conn.Subprotocol()
		p.mu.Unlock()
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f, err := egress.Decode(message)
		if err != nil || f.Hello == nil {
			return
		}
		p.mu.Lock()
		p.hellos = append(p.hellos, *f.Hello)
		p.mu.Unlock()
		if closeEarly {
			return
		}
		if p.down != nil {
			p.down(conn)
		}
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			up, err := egress.Decode(message)
			if err != nil {
				continue
			}
			p.mu.Lock()
			switch {
			case up.Ack != nil:
				p.acks = append(p.acks, *up.Ack)
			case up.Record != nil:
				p.records = append(p.records, *up.Record)
			}
			p.mu.Unlock()
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *stubPlane) url(t *testing.T, environment string) string {
	t.Helper()
	stream, err := streamURL(p.server.URL, environment)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func (p *stubPlane) taken() ([]egress.Hello, []egress.Ack, []egress.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]egress.Hello(nil), p.hellos...), append([]egress.Ack(nil), p.acks...), append([]egress.Record(nil), p.records...)
}

// runClient starts the sync client against the stub and stops it with the
// test.
func runClient(t *testing.T, p *stubPlane, s *store, caPEM string) *syncClient {
	t.Helper()
	c := &syncClient{
		url: p.url(t, "default"), key: "key", gatewayID: "gw-test", store: s, caPEM: caPEM,
		records: make(chan egress.Record, recordBuffer), log: slog.Default(),
		dialer: &websocket.Dialer{Subprotocols: []string{egress.Protocol}, HandshakeTimeout: 5 * time.Second},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

// TestTheGatewayOpensWithAHello is the first frame of every connection: who
// connected, what it holds, and the authority the control plane projects.
func TestTheGatewayOpensWithAHello(t *testing.T) {
	p := newStubPlane(t)
	s := newStore()
	s.Apply(boundary("sbx_held", 4, v1.EgressOpen))
	runClient(t, p, s, "-----BEGIN CERTIFICATE-----\nauthority\n-----END CERTIFICATE-----\n")
	hello := waitForHello(t, p, 1)[0]
	if hello.Protocol != egress.Protocol || hello.GatewayID != "gw-test" {
		t.Fatalf("hello = %+v", hello)
	}
	if hello.Versions[egress.Principal("sbx_held")] != 4 {
		t.Fatalf("hello versions = %v, want what the gateway holds", hello.Versions)
	}
	if !strings.Contains(hello.CAPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("hello carries no authority: %q", hello.CAPEM)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bearer != "Bearer key" {
		t.Fatalf("the stream was opened with %q, want the environment key", p.bearer)
	}
	if p.subprotocol != egress.Protocol {
		t.Fatalf("subprotocol = %q, want %q", p.subprotocol, egress.Protocol)
	}
}

// TestTheSnapshotReplacesTheWholeWorld is the property a reconnect rests on:
// what the gateway held before the snapshot is gone after it.
func TestTheSnapshotReplacesTheWholeWorld(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{Maps: []egress.Map{
			boundary("sbx_a", 2, v1.EgressAllowlist, "api.example.com"),
		}}})
	}
	s := newStore()
	s.Apply(boundary("sbx_stale", 1, v1.EgressOpen))
	runClient(t, p, s, "")
	waitFor(t, func() bool {
		_, held := s.Map(egress.Principal("sbx_a"))
		_, stale := s.Map(egress.Principal("sbx_stale"))
		return held && !stale
	}, "the snapshot to replace the gateway's world")
	// Every map of a snapshot is acknowledged, so the control plane learns
	// that a gateway that connected with nothing now holds everything.
	waitFor(t, func() bool {
		_, acks, _ := p.taken()
		return len(acks) == 1 && acks[0].Principal == egress.Principal("sbx_a") && acks[0].Version == 2
	}, "the snapshot to be acknowledged")
}

// TestAPutAppliesAndIsAcknowledged covers the version rule from the
// gateway's side: a higher version lands, a repeat does not, and both are
// acknowledged so the control plane's wait ends either way.
func TestAPutAppliesAndIsAcknowledged(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{}})
		send(conn, egress.Frame{Type: egress.FramePut, Put: mapPtr(boundary("sbx_a", 2, v1.EgressAllowlist, "api.example.com"))})
		send(conn, egress.Frame{Type: egress.FramePut, Put: mapPtr(boundary("sbx_a", 1, v1.EgressAllowlist, "widened.example.com"))})
		send(conn, egress.Frame{Type: egress.FrameHeartbeat})
	}
	s := newStore()
	runClient(t, p, s, "")
	waitFor(t, func() bool {
		_, acks, _ := p.taken()
		return len(acks) == 2
	}, "both puts to be acknowledged")
	m, _ := s.Map(egress.Principal("sbx_a"))
	if m.Version != 2 || m.Allow[0] != "api.example.com" {
		t.Fatalf("map = %+v, want the higher version kept", m)
	}
	_, acks, _ := p.taken()
	for _, a := range acks {
		if a.Version != 2 {
			t.Fatalf("ack = %+v, want the version the gateway holds", a)
		}
	}
}

func TestAPurgeDropsThePrincipal(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{Maps: []egress.Map{boundary("sbx_a", 1, v1.EgressOpen)}}})
		send(conn, egress.Frame{Type: egress.FramePurge, Purge: &egress.Purge{Principal: egress.Principal("sbx_a")}})
	}
	s := newStore()
	runClient(t, p, s, "")
	waitFor(t, func() bool {
		_, held := s.Map(egress.Principal("sbx_a"))
		return !held
	}, "the purge to drop the principal")
}

// TestTheGatewayReconnects keeps a gateway serving across an interruption and
// makes it whole again: the second hello carries what the first connection
// left it holding.
func TestTheGatewayReconnects(t *testing.T) {
	p := newStubPlane(t)
	p.closeAfterHello = true
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{Maps: []egress.Map{boundary("sbx_a", 3, v1.EgressOpen)}}})
	}
	s := newStore()
	runClient(t, p, s, "")
	waitFor(t, func() bool { return len(waitNothing(p)) >= 2 }, "the gateway to reconnect")
	waitFor(t, func() bool {
		m, held := s.Map(egress.Principal("sbx_a"))
		return held && m.Version == 3
	}, "the second connection's snapshot to land")
}

// TestRecordsGoUpTheSameStream is the telemetry half of the protocol: the
// gateway reports every connection on the stream it already holds, so the
// control plane needs no inbound route.
func TestRecordsGoUpTheSameStream(t *testing.T) {
	p := newStubPlane(t)
	p.down = func(conn *websocket.Conn) {
		send(conn, egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{}})
	}
	c := runClient(t, p, newStore(), "")
	c.Record(egress.Record{Principal: egress.Principal("sbx_a"), Host: "api.example.com", Decision: egress.DecisionAllowed})
	waitFor(t, func() bool {
		_, _, records := p.taken()
		return len(records) == 1 && records[0].Host == "api.example.com"
	}, "the record to reach the control plane")
}

// TestAFullRecordBufferDropsTheOldest keeps a gateway serving connections
// when the control plane is not reading: a record is telemetry, and the
// boundary is not.
func TestAFullRecordBufferDropsTheOldest(t *testing.T) {
	c := &syncClient{records: make(chan egress.Record, 2), log: slog.Default()}
	for i := range 5 {
		c.Record(egress.Record{Principal: egress.Principal("sbx_a"), Port: i})
	}
	if len(c.records) != 2 {
		t.Fatalf("the buffer holds %d records, want its cap of 2", len(c.records))
	}
	first := <-c.records
	if first.Port != 3 {
		t.Fatalf("the buffer holds the record from round %d, want the newest two", first.Port)
	}
}

func TestEnvironmentOf(t *testing.T) {
	good := token(t, map[string]any{"sub": "environment:default"})
	if got, err := environmentOf(good); err != nil || got != "default" {
		t.Fatalf("environmentOf = %q, %v", got, err)
	}
	for _, tc := range []struct{ name, key string }{
		{"empty", ""},
		{"notAToken", "abc"},
		{"twoParts", "a.b"},
		{"payloadIsNotBase64", "a.!!!.c"},
		{"payloadIsNotJSON", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c"},
		{"aWorkloadToken", token(t, map[string]any{"sub": "sandbox:sbx_a"})},
		{"aPersonsToken", token(t, map[string]any{"sub": "https://login.example.com|alice"})},
		{"noSubject", token(t, map[string]any{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := environmentOf(tc.key); err == nil {
				t.Fatalf("environmentOf(%q) was accepted", tc.key)
			}
		})
	}
}

func TestStreamURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://cella.example.com", "wss://cella.example.com/v1/environments/default/egress"},
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080/v1/environments/default/egress"},
		{"https://cella.example.com/base/", "wss://cella.example.com/base/v1/environments/default/egress"},
	} {
		got, err := streamURL(tc.in, "default")
		if err != nil || got != tc.want {
			t.Errorf("streamURL(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "cella.example.com", "ftp://cella.example.com", "://"} {
		if _, err := streamURL(bad, "default"); err == nil {
			t.Errorf("streamURL(%q) was accepted", bad)
		}
	}
}

// ---- helpers ----

func mapPtr(m egress.Map) *egress.Map { return &m }

func send(conn *websocket.Conn, f egress.Frame) {
	message, err := egress.Encode(f)
	if err != nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, message)
}

func token(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func waitForHello(t *testing.T, p *stubPlane, n int) []egress.Hello {
	t.Helper()
	waitFor(t, func() bool { hellos, _, _ := p.taken(); return len(hellos) >= n }, "the gateway to say hello")
	hellos, _, _ := p.taken()
	return hellos
}

func waitNothing(p *stubPlane) []egress.Hello {
	hellos, _, _ := p.taken()
	return hellos
}

// waitFor polls until the condition holds or the test's patience runs out.
func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
