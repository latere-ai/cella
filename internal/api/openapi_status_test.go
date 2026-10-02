// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/remote"
)

// driven is what a control plane answered each operation with: the statuses,
// by operationId.
type driven struct {
	t    *testing.T
	seen map[string][]string
}

func (d *driven) note(id string, status int) {
	if code := strconv.Itoa(status); !slices.Contains(d.seen[id], code) {
		d.seen[id] = append(d.seen[id], code)
		slices.Sort(d.seen[id])
	}
}

// send makes one request as the operation and notes the status. A redirect
// is the answer and is not followed.
func (d *driven) send(f *fixture, id, method, path, token, media, body string) []byte {
	d.t.Helper()
	header := http.Header{}
	if media != "" {
		header.Set("Content-Type", media)
	}
	res := f.proxied(method, path, token, strings.NewReader(body), header)
	out, err := io.ReadAll(res.Body)
	if err != nil {
		d.t.Fatalf("%s: reading the answer: %v", id, err)
	}
	d.note(id, res.StatusCode)
	return out
}

// upgrade opens one socket as the operation and notes the status of the
// handshake, then leaves.
func (d *driven) upgrade(f *fixture, id, path, token, protocol string) {
	d.t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{protocol}, HandshakeTimeout: 5 * time.Second}
	conn, res, err := dialer.Dial("ws"+strings.TrimPrefix(f.url, "http")+path,
		http.Header{"Authorization": []string{"Bearer " + token}})
	if res != nil {
		d.note(id, res.StatusCode)
		_ = res.Body.Close()
	}
	if err != nil {
		d.t.Errorf("%s: the socket did not open: %v", id, err)
		return
	}
	_ = conn.Close()
}

// TestTheDocumentNamesTheStatusesAnswered drives every operation of the
// document against a control plane and holds the success statuses the
// document names for it to the ones the server answered: a status the handler
// writes and the document does not name, and one the document names that no
// request here was answered with, both fail. An apply is driven twice, for
// its create and its update. An operation this test does not reach fails too,
// so a route added to the document is driven here.
func TestTheDocumentNamesTheStatusesAnswered(t *testing.T) {
	d := &driven{t: t, seen: map[string][]string{}}
	renamed := func(body, from, to string) string { return strings.Replace(body, from, to, 1) }
	const octets = "application/octet-stream"

	// Sandboxes, commands, files, the feed and secrets, over the native
	// driver with a journal and a store that holds secret values.
	f := setupRecorded(t).fixture
	var obj v1.Sandbox
	if err := json.Unmarshal(d.send(f, "createSandbox", "POST", "/v1/sandboxes?wait=1", f.alice, jsonMedia, createBody), &obj); err != nil {
		t.Fatalf("the created sandbox did not decode: %v", err)
	}
	base, notes := "/v1/sandboxes/"+obj.Status.ID, "?path="+obj.Spec.Workspace.Path+"/notes.txt"
	d.send(f, "listSandboxes", "GET", "/v1/sandboxes", f.alice, "", "")
	d.send(f, "applySandbox", "PUT", "/v1/sandboxes/work", f.alice, jsonMedia, createBody)
	d.send(f, "applySandbox", "PUT", "/v1/sandboxes/second", f.alice, jsonMedia, renamed(createBody, `"work"`, `"second"`))
	d.send(f, "readSandbox", "GET", base, f.alice, "", "")
	d.send(f, "execSandbox", "POST", base+"/exec?wait=1", f.alice, jsonMedia, `{"command":["true"]}`)
	d.send(f, "execSandbox", "POST", base+"/exec", f.alice, jsonMedia, `{"command":["true"]}`)
	d.upgrade(f, "execSocket", base+"/exec", f.alice, subprotocol)
	d.upgrade(f, "attachSandbox", base+"/attach", f.alice, subprotocol)
	d.send(f, "importFiles", "PUT", base+"/files"+notes, f.alice, octets, "hello\n")
	d.send(f, "exportFiles", "GET", base+"/files"+notes, f.alice, "", "")
	d.send(f, "readFile", "GET", base+"/files/content"+notes, f.alice, "", "")
	d.send(f, "statFile", "GET", base+"/files/stat"+notes, f.alice, "", "")
	d.send(f, "listFiles", "GET", base+"/files/list?path="+obj.Spec.Workspace.Path, f.alice, "", "")
	d.send(f, "makeDirectory", "POST", base+"/files/mkdir", f.alice, jsonMedia, `{"path":"`+obj.Spec.Workspace.Path+`/reports"}`)
	d.send(f, "moveFile", "POST", base+"/files/move", f.alice, jsonMedia,
		`{"from":"`+obj.Spec.Workspace.Path+`/notes.txt","to":"`+obj.Spec.Workspace.Path+`/reports/notes.txt"}`)
	d.send(f, "removeFile", "DELETE", base+"/files?path="+obj.Spec.Workspace.Path+"/reports", f.alice, "", "")
	d.send(f, "sandboxLogs", "GET", base+"/logs", f.alice, "", "")
	d.send(f, "sandboxEgress", "GET", base+"/egress", f.alice, "", "")
	d.send(f, "sandboxPorts", "GET", base+"/ports", f.alice, "", "")
	d.send(f, "stopSandbox", "POST", base+"/stop", f.alice, "", "")
	d.send(f, "startSandbox", "POST", base+"/start", f.alice, "", "")
	d.send(f, "objectEvents", "GET", "/v1/events?object="+obj.Status.ID, f.alice, "", "")
	d.send(f, "deleteSandbox", "DELETE", base, f.alice, "", "")
	d.send(f, "createSecret", "POST", "/v1/secrets", f.alice, jsonMedia, secretBody("api-key", "api.example.com", "one"))
	d.send(f, "listSecrets", "GET", "/v1/secrets", f.alice, "", "")
	d.send(f, "applySecret", "PUT", "/v1/secrets/api-key", f.alice, jsonMedia, secretBody("api-key", "api.example.com", "two"))
	d.send(f, "applySecret", "PUT", "/v1/secrets/second", f.alice, jsonMedia, secretBody("second", "api.example.com", "one"))
	d.send(f, "readSecret", "GET", "/v1/secrets/api-key", f.alice, "", "")
	d.send(f, "deleteSecret", "DELETE", "/v1/secrets/api-key", f.alice, "", "")

	// The dial socket, the port proxy and its redirect, over a driver that
	// dials a server standing for the one inside.
	f, _, obj, _ = proxyFixture(t)
	base = "/v1/sandboxes/" + obj.Status.ID
	d.upgrade(f, "dialSandbox", base+"/dial/8080", f.alice, dialSubprotocol)
	for _, method := range anyMethod {
		verb := method[:1] + strings.ToLower(method[1:])
		d.send(f, "proxyPort"+verb, method, base+"/ports/web/any", f.alice, "", "")
		d.send(f, "redirectPort"+verb, method, base+"/ports/web", f.alice, "", "")
	}

	// The desktop, over a driver with a screen.
	f, _, id := desk(t, nil)
	base = "/v1/sandboxes/" + id
	d.send(f, "sandboxDisplay", "GET", base+"/display", f.alice, "", "")
	d.send(f, "sandboxScreenshot", "GET", base+"/screenshot", f.alice, "", "")
	d.upgrade(f, "sandboxScreen", base+"/screen", f.alice, screenSubprotocol)
	d.send(f, "sandboxInput", "POST", base+"/input", f.alice, jsonMedia, `{"events":[{"type":"click","button":"left"}]}`)

	// The Environment kind, as its administrator.
	f = setupEnvironments(t).fixture
	d.send(f, "listEnvironments", "GET", "/v1/environments", f.alice, "", "")
	d.send(f, "createEnvironment", "POST", "/v1/environments", f.alice, jsonMedia, environmentBody)
	d.send(f, "readEnvironment", "GET", "/v1/environments/eu-gpu", f.alice, "", "")
	d.send(f, "applyEnvironment", "PUT", "/v1/environments/eu-gpu", f.alice, jsonMedia, environmentBody)
	d.send(f, "applyEnvironment", "PUT", "/v1/environments/us-east", f.alice, jsonMedia, renamed(environmentBody, "eu-gpu", "us-east"))
	d.send(f, "deleteEnvironment", "DELETE", "/v1/environments/us-east", f.alice, "", "")

	// The key routes and the three routes an environment key reaches, on a
	// control plane that signs keys and holds a worker hub and a gateway hub.
	f = setupKeyed(t, nil).fixture
	f.h.(*handler).Egress = NewEgressHub(EgressHubOptions{Environment: "default"})
	var key mintedKey
	if err := json.Unmarshal(d.send(f, "mintEnvironmentKey", "POST", "/v1/environments/default/keys", f.alice, "", ""), &key); err != nil {
		t.Fatalf("the minted key did not decode: %v", err)
	}
	d.send(f, "listEnvironmentKeys", "GET", "/v1/environments/default/keys", f.alice, "", "")
	d.send(f, "registerWorker", "POST", "/v1/environments/self/workers", key.Token, jsonMedia,
		`{"driver":"native","isolation":"none","capabilities":{"files":true}}`)
	d.upgrade(f, "workerOperations", "/v1/environments/self/operations", key.Token, remote.Protocol)
	d.upgrade(f, "gatewaySync", "/v1/environments/default/egress", key.Token, egress.Protocol)
	d.send(f, "revokeEnvironmentKey", "DELETE", "/v1/environments/default/keys/"+key.JTI, f.alice, "", "")

	described := operations(t)
	for id, op := range described {
		switch answered, named := d.seen[id], op.successes(); {
		case len(answered) == 0:
			t.Errorf("%s: no request of this test reaches it; drive it here, so its status is held", id)
		case !slices.Equal(answered, named):
			t.Errorf("%s: the document names %v and the server answered %v", id, named, answered)
		}
	}
	for id := range d.seen {
		if _, held := described[id]; !held {
			t.Errorf("this test drives %s and the document describes no such operation", id)
		}
	}
}
