// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/remote"
)

// TestAWorkerIsReportedAtOnce: an environment's status counts a worker and
// shows its host as soon as its registration is answered, and stops counting
// it as soon as its stream ends, with no tick of the phase loop between. This
// control plane runs no phase loop, so only the registration and the stream's
// end write what is read here.
func TestAWorkerIsReportedAtOnce(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	host := v1.HostInfo{OS: "linux", Arch: "amd64", Grants: []v1.HostGrant{{Path: "/home/dev/site", Write: true}},
		Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com"}}}
	registration, err := json.Marshal(remote.Registration{Driver: "native", Isolation: "none", Host: &host})
	if err != nil {
		t.Fatal(err)
	}
	var registered remote.Registered
	if err = json.Unmarshal(p.request(http.MethodPost, "/v1/environments/self/workers", key, string(registration), http.StatusCreated), &registered); err != nil {
		t.Fatal(err)
	}
	read := func() v1.Environment {
		var obj v1.Environment
		if err := json.Unmarshal(p.request(http.MethodGet, "/v1/environments/default", p.alice, "", http.StatusOK), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	obj := read()
	if obj.Status.Workers != 1 || len(obj.Status.Hosts) != 1 || obj.Status.Hosts[0].Worker != registered.Worker ||
		!obj.Status.Hosts[0].Host.Equal(host) {
		t.Fatalf("right after the registration the environment reads %d workers and hosts %+v", obj.Status.Workers, obj.Status.Hosts)
	}

	dialer := &websocket.Dialer{Subprotocols: []string{remote.Protocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(p.url, "http")+"/v1/environments/self/operations",
		http.Header{"Authorization": []string{"Bearer " + key}})
	if res != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("the worker's stream did not open: %v", err)
	}
	hello, err := remote.EncodeMessage(remote.NoOperation, remote.Message{Type: remote.MessageHello, Worker: registered.Worker})
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.WriteMessage(websocket.BinaryMessage, hello); err != nil {
		t.Fatal(err)
	}
	connected := func() bool {
		for _, w := range p.hub.Workers("default") {
			if w.Connected {
				return true
			}
		}
		return false
	}
	waitUntil(t, connected, "the stream to bind")
	if obj = read(); obj.Status.Workers != 1 || len(obj.Status.Hosts) != 1 {
		t.Errorf("with the stream open the environment reads %d workers and hosts %+v", obj.Status.Workers, obj.Status.Hosts)
	}

	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		obj = read()
		return obj.Status.Workers == 0 && len(obj.Status.Hosts) == 0
	}, "the environment to stop counting the worker whose stream ended")
}
