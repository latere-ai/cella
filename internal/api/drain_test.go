// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/runtime"
)

// heldDriver holds the driver's create of one named sandbox until the test
// releases it, and passes every other call through.
type heldDriver struct {
	runtime.Driver
	name    string
	release chan struct{}
}

func (d heldDriver) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	if s.Name == d.name {
		select {
		case <-d.release:
		case <-ctx.Done():
			return runtime.Ref{}, ctx.Err()
		}
	}
	return d.Driver.Create(ctx, s)
}

// TestDrainingEndsWhatWouldOutlastAHandoff: when the handler drains, which a
// writer handing off to another replica does (spec 076), a create held under
// ?wait=1 answers the sandbox as it stands and a terminal socket is closed
// with 1001, going away, so neither holds the handoff open until its bound.
func TestDrainingEndsWhatWouldOutlastAHandoff(t *testing.T) {
	t.Run("a held create", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return heldDriver{Driver: d, name: "held", release: release} })
		drain := make(chan struct{})
		f.h.(*handler).Draining = drain
		// The held request is ended with the test whatever happens, so a
		// handler that does not drain fails this case rather than holding
		// the server's close for the request's hour.
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		held := make(chan int, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+"/v1/sandboxes?wait=1&timeout=1h", strings.NewReader(named("held")))
			if err != nil {
				held <- 0
				return
			}
			req.Header.Set("Authorization", "Bearer "+f.alice)
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				held <- 0
				return
			}
			_ = res.Body.Close()
			held <- res.StatusCode
		}()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if status, _ := f.call("GET", "/v1/sandboxes/held", f.alice, "", ""); status == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the held create was never recorded")
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(drain)
		select {
		case status := <-held:
			if status != http.StatusCreated {
				t.Errorf("the held create answered %d when the handler drained, want 201 with the sandbox as it stands", status)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the held create was still held after the handler drained")
		}
	})
	t.Run("a terminal socket", func(t *testing.T) {
		f := setup(t, nil)
		drain := make(chan struct{})
		f.h.(*handler).Draining = drain
		running := f.sandbox("running")
		_, reader := f.attachTo("/v1/sandboxes/"+running.Status.ID+"/attach", f.alice, `{"command":["sh"],"cols":80,"rows":24}`)
		close(drain)
		deadline := time.Now().Add(10 * time.Second)
		for reader.closeErr() == nil {
			if time.Now().After(deadline) {
				t.Fatal("the terminal socket stayed open after the handler drained")
			}
			time.Sleep(5 * time.Millisecond)
		}
		var closed *websocket.CloseError
		if !errors.As(reader.closeErr(), &closed) || closed.Code != websocket.CloseGoingAway {
			t.Errorf("the terminal socket ended with %v, want 1001 going away", reader.closeErr())
		}
	})
}

// TestAHubThatDrainsClosesItsGatewayStreams: a writer handing off closes every
// gateway's stream with 1001, going away, so the gateway dials the next
// writer at once.
func TestAHubThatDrainsClosesItsGatewayStreams(t *testing.T) {
	hub := NewEgressHub(EgressHubOptions{Environment: "default"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeGateway(w, r, "default")
	}))
	t.Cleanup(server.Close)
	dialer := websocket.Dialer{Subprotocols: []string{egress.Protocol}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	hello, err := egress.Encode(egress.Frame{Type: egress.FrameHello, Hello: &egress.Hello{Protocol: egress.Protocol, GatewayID: "gw-one"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("the snapshot did not arrive: %v", err)
	}
	hub.Drain()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.CloseGoingAway {
			t.Fatalf("the gateway's stream ended with %v, want 1001 going away", err)
		}
		return
	}
}
