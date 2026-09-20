// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// hubFixture is a hub served over loopback, with the gateways a test
// connected to it.
type hubFixture struct {
	hub    *EgressHub
	server *httptest.Server
}

func newHub(t *testing.T, ackTimeout time.Duration, cap int) *hubFixture {
	t.Helper()
	hub := NewEgressHub(EgressHubOptions{Environment: "default", AckTimeout: ackTimeout, RecordsCap: cap})
	f := &hubFixture{hub: hub}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeGateway(w, r, r.URL.Query().Get("environment"))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// gatewayStub is one connected gateway: it takes the frames down and answers
// the acknowledgements a test tells it to.
type gatewayStub struct {
	conn *websocket.Conn

	mu     sync.Mutex
	frames []egress.Frame
	// silent keeps the gateway from acknowledging anything, which is the
	// gateway that is connected and not answering.
	silent bool
}

// connect opens one gateway stream and says hello.
func (f *hubFixture) connect(t *testing.T, environment, principal string, silent bool) *gatewayStub {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") + "?environment=" + environment
	dialer := &websocket.Dialer{Subprotocols: []string{egress.Protocol}, HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.DialContext(t.Context(), url, nil)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("the gateway could not connect: %v", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	g := &gatewayStub{conn: conn, silent: silent}
	hello := egress.Frame{Type: egress.FrameHello, Hello: &egress.Hello{
		Protocol: egress.Protocol, GatewayID: "gw", Principal: principal,
		CAPEM: "-----BEGIN CERTIFICATE-----\nauthority\n-----END CERTIFICATE-----\n",
	}}
	message, err := egress.Encode(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteMessage(websocket.TextMessage, message); err != nil {
		t.Fatal(err)
	}
	go g.read()
	return g
}

// read takes every frame the hub sends and acknowledges what it should.
func (g *gatewayStub) read() {
	for {
		_, message, err := g.conn.ReadMessage()
		if err != nil {
			return
		}
		f, err := egress.Decode(message)
		if err != nil {
			continue
		}
		g.mu.Lock()
		g.frames = append(g.frames, f)
		silent := g.silent
		g.mu.Unlock()
		if silent {
			continue
		}
		switch {
		case f.Put != nil:
			g.ack(f.Put.Principal, f.Put.Version)
		case f.Snapshot != nil:
			for _, m := range f.Snapshot.Maps {
				g.ack(m.Principal, m.Version)
			}
		}
	}
}

func (g *gatewayStub) ack(principal string, version int64) {
	message, err := egress.Encode(egress.Frame{Type: egress.FrameAck, Ack: &egress.Ack{Principal: principal, Version: version}})
	if err != nil {
		return
	}
	_ = g.conn.WriteMessage(websocket.TextMessage, message)
}

func (g *gatewayStub) taken() []egress.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.frames)
}

func (g *gatewayStub) send(t *testing.T, f egress.Frame) {
	t.Helper()
	message, err := egress.Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.conn.WriteMessage(websocket.TextMessage, message); err != nil {
		t.Fatal(err)
	}
}

func boundary(id string, version int64) egress.Map {
	return egress.Map{Principal: egress.Principal(id), Version: version, Credential: "credential-" + id, Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}}
}

// TestSendNeedsAConnectedGateway is the create's gate: a boundary no gateway
// holds is a refusal the controller turns into an unavailable environment.
func TestSendNeedsAConnectedGateway(t *testing.T) {
	f := newHub(t, 200*time.Millisecond, 0)
	if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); !errors.Is(err, controller.ErrNoGateway) {
		t.Fatalf("Send with no gateway = %v, want %v", err, controller.ErrNoGateway)
	}
	// A gateway that is connected and never answers is no better than none,
	// and the wait ends at the timeout rather than at the create.
	f.connect(t, "default", "", true)
	waitUntil(t, func() bool { return f.hub.Connected() == 1 }, "the gateway to connect")
	started := time.Now()
	if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); !errors.Is(err, controller.ErrNoGateway) {
		t.Fatalf("Send with a silent gateway = %v, want %v", err, controller.ErrNoGateway)
	}
	if time.Since(started) < 200*time.Millisecond {
		t.Fatal("the wait ended before the timeout")
	}
}

// TestSendReturnsOnTheFirstAcknowledgement is the fan-out rule: the map goes
// to every gateway and one answer is enough to let the sandbox be created.
func TestSendReturnsOnTheFirstAcknowledgement(t *testing.T) {
	f := newHub(t, 5*time.Second, 0)
	silent := f.connect(t, "default", "", true)
	answering := f.connect(t, "default", "", false)
	waitUntil(t, func() bool { return f.hub.Connected() == 2 }, "both gateways to connect")
	if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); err != nil {
		t.Fatalf("Send = %v", err)
	}
	for name, g := range map[string]*gatewayStub{"the silent one": silent, "the answering one": answering} {
		waitUntil(t, func() bool {
			for _, frame := range g.taken() {
				if frame.Put != nil && frame.Put.Principal == egress.Principal("sbx_a") {
					return true
				}
			}
			return false
		}, "the map to reach "+name)
	}
	// The authority the first gateway said hello with is what the driver
	// projects into every sandbox of the environment.
	if !strings.Contains(f.hub.CA(), "BEGIN CERTIFICATE") {
		t.Fatalf("the hub holds no authority: %q", f.hub.CA())
	}
}

// TestASnapshotIsSentOnConnect makes a gateway whole with one frame, whatever
// it held before and whether or not this control plane has restarted.
func TestASnapshotIsSentOnConnect(t *testing.T) {
	f := newHub(t, time.Second, 0)
	f.hub.Seed([]egress.Map{boundary("sbx_a", 3), boundary("sbx_b", 1)})
	g := f.connect(t, "default", "", false)
	waitUntil(t, func() bool {
		for _, frame := range g.taken() {
			if frame.Snapshot != nil && len(frame.Snapshot.Maps) == 2 {
				return true
			}
		}
		return false
	}, "the snapshot")
	for _, frame := range g.taken() {
		if frame.Snapshot == nil {
			continue
		}
		if frame.Snapshot.Maps[0].Principal != egress.Principal("sbx_a") || frame.Snapshot.Maps[0].Version != 3 {
			t.Fatalf("the snapshot = %+v, want desired state's own maps", frame.Snapshot.Maps)
		}
	}
}

// TestASidecarReceivesOneMap is the per-pod gateway: it asks for one
// principal and is never told about another sandbox's boundary.
func TestASidecarReceivesOneMap(t *testing.T) {
	f := newHub(t, time.Second, 0)
	f.hub.Seed([]egress.Map{boundary("sbx_a", 1), boundary("sbx_b", 1)})
	g := f.connect(t, "default", egress.Principal("sbx_a"), false)
	waitUntil(t, func() bool {
		for _, frame := range g.taken() {
			if frame.Snapshot != nil {
				return true
			}
		}
		return false
	}, "the snapshot")
	for _, frame := range g.taken() {
		if frame.Snapshot == nil {
			continue
		}
		if len(frame.Snapshot.Maps) != 1 || frame.Snapshot.Maps[0].Principal != egress.Principal("sbx_a") {
			t.Fatalf("the sidecar's snapshot = %+v, want its one sandbox", frame.Snapshot.Maps)
		}
	}
	// A map for another sandbox never reaches it.
	if err := f.hub.Send(t.Context(), boundary("sbx_b", 2)); !errors.Is(err, controller.ErrNoGateway) {
		t.Fatalf("Send of another sandbox's map = %v, want no gateway to have taken it", err)
	}
}

func TestPurgeDropsTheMapAndTellsEveryGateway(t *testing.T) {
	f := newHub(t, time.Second, 0)
	g := f.connect(t, "default", "", false)
	waitUntil(t, func() bool { return f.hub.Connected() == 1 }, "the gateway to connect")
	if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); err != nil {
		t.Fatal(err)
	}
	f.hub.Purge(t.Context(), egress.Principal("sbx_a"))
	waitUntil(t, func() bool {
		for _, frame := range g.taken() {
			if frame.Purge != nil && frame.Purge.Principal == egress.Principal("sbx_a") {
				return true
			}
		}
		return false
	}, "the purge to reach the gateway")
	// The next gateway to connect is not told about it either.
	second := f.connect(t, "default", "", false)
	waitUntil(t, func() bool {
		for _, frame := range second.taken() {
			if frame.Snapshot != nil {
				if len(frame.Snapshot.Maps) != 0 {
					t.Errorf("the snapshot still carries %+v", frame.Snapshot.Maps)
				}
				return true
			}
		}
		return false
	}, "the second gateway's snapshot")
}

// TestRecordsAreKeptPerSandboxNewestFirst is the telemetry surface: a ring
// per sandbox, so a busy sandbox evicts its own oldest record and no other
// sandbox's.
func TestRecordsAreKeptPerSandboxNewestFirst(t *testing.T) {
	f := newHub(t, time.Second, 3)
	g := f.connect(t, "default", "", false)
	waitUntil(t, func() bool { return f.hub.Connected() == 1 }, "the gateway to connect")
	for i := range 5 {
		g.send(t, egress.Frame{Type: egress.FrameRecord, Record: &egress.Record{
			Principal: egress.Principal("sbx_a"), Host: "api.example.com", Port: i, Decision: egress.DecisionAllowed,
		}})
	}
	g.send(t, egress.Frame{Type: egress.FrameRecord, Record: &egress.Record{
		Principal: egress.Principal("sbx_b"), Host: "other.example.com", Decision: egress.DecisionDenied,
	}})
	waitUntil(t, func() bool { return len(f.hub.Records("sbx_a", 0)) == 3 && len(f.hub.Records("sbx_b", 0)) == 1 }, "the records")
	kept := f.hub.Records("sbx_a", 0)
	if kept[0].Port != 4 || kept[2].Port != 2 {
		t.Fatalf("records = %+v, want the newest three, newest first", kept)
	}
	if limited := f.hub.Records("sbx_a", 1); len(limited) != 1 || limited[0].Port != 4 {
		t.Fatalf("the limited page = %+v", limited)
	}
	// A record the contract refuses is dropped rather than kept.
	g.send(t, egress.Frame{Type: egress.FrameRecord, Record: &egress.Record{Principal: "environment:env_1"}})
	g.send(t, egress.Frame{Type: egress.FrameHeartbeat})
	if len(f.hub.Records("", 0)) != 0 {
		t.Fatal("a record naming no sandbox was kept")
	}
}

// TestTheStreamRefusesWhatItCannotServe keeps a stream from opening where the
// protocol does not agree.
func TestTheStreamRefusesWhatItCannotServe(t *testing.T) {
	f := newHub(t, time.Second, 0)
	t.Run("anotherEnvironment", func(t *testing.T) {
		url := "ws" + strings.TrimPrefix(f.server.URL, "http") + "?environment=other"
		dialer := &websocket.Dialer{Subprotocols: []string{egress.Protocol}}
		conn, resp, err := dialer.DialContext(t.Context(), url, nil)
		if err == nil {
			_ = conn.Close()
			t.Fatal("a gateway of another environment was served")
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	})
	t.Run("anotherSubprotocol", func(t *testing.T) {
		url := "ws" + strings.TrimPrefix(f.server.URL, "http") + "?environment=default"
		dialer := &websocket.Dialer{Subprotocols: []string{"something.else"}}
		conn, resp, err := dialer.DialContext(t.Context(), url, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err == nil {
			t.Fatal("a stream of another protocol was served")
		}
	})
	t.Run("aFirstFrameThatIsNotAHello", func(t *testing.T) {
		url := "ws" + strings.TrimPrefix(f.server.URL, "http") + "?environment=default"
		dialer := &websocket.Dialer{Subprotocols: []string{egress.Protocol}}
		conn, resp, err := dialer.DialContext(t.Context(), url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		defer func() { _ = conn.Close() }()
		message, err := egress.Encode(egress.Frame{Type: egress.FrameHeartbeat})
		if err != nil {
			t.Fatal(err)
		}
		if err = conn.WriteMessage(websocket.TextMessage, message); err != nil {
			t.Fatal(err)
		}
		if _, _, err = conn.ReadMessage(); err == nil {
			t.Fatal("a stream that opened with no hello was served")
		}
	})
}

func TestEgressStreamPath(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		id           string
		ok           bool
	}{
		{http.MethodGet, "/v1/environments/default/egress", "default", true},
		{http.MethodGet, "/v1/environments/eu-1/egress", "eu-1", true},
		{http.MethodPost, "/v1/environments/default/egress", "", false},
		{http.MethodGet, "/v1/environments//egress", "", false},
		{http.MethodGet, "/v1/environments/a/b/egress", "", false},
		{http.MethodGet, "/v1/environments/default/operations", "", false},
		{http.MethodGet, "/v1/sandboxes/sbx_a/egress", "", false},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		id, ok := egressStreamPath(req)
		if id != tc.id || ok != tc.ok {
			t.Errorf("egressStreamPath(%s %s) = %q, %v, want %q, %v", tc.method, tc.path, id, ok, tc.id, tc.ok)
		}
	}
}

// waitUntil polls until the condition holds or the test's patience runs out.
func waitUntil(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheRecordsRouteIsTheSandboxOwnersToRead serves what left one sandbox,
// under the same decision that serves the sandbox itself.
func TestTheRecordsRouteIsTheSandboxOwnersToRead(t *testing.T) {
	f := setup(t, nil)
	body := f.request("POST", "/v1/sandboxes", f.alice, createBody, 201)
	var obj v1.Sandbox
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + obj.Status.ID + "/egress"
	// An installation that runs no gateway holds no record, and the route
	// answers the empty page rather than a refusal.
	answer := f.request("GET", base, f.alice, "", 200)
	var page struct {
		Items []egress.Record `json:"items"`
	}
	if err := json.Unmarshal(answer, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("items = %+v, want none", page.Items)
	}
	f.request("GET", base+"?limit=0", f.alice, "", 400)
	f.request("GET", base+"?limit=many", f.alice, "", 400)
	// Another subject's read is the authorizer's refusal, not a page.
	f.request("GET", base, f.bob, "", 403)
	f.request("GET", "/v1/sandboxes/sbx_nothing/egress", f.alice, "", 404)
}
