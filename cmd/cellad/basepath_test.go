// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"

	apidoc "latere.ai/x/cella/api"
	"latere.ai/x/cella/client"
	driver "latere.ai/x/cella/runtime"
)

// basePath is the prefix the cases below mount the control plane under: the
// capability's prefix on an origin that serves several services.
const basePath = "/v1/environments"

// mounted is a running `cellad serve` and a caller who administers it.
type mounted struct {
	// origin is the public listener's scheme and address, and internal the
	// internal listener's. public is CELLA_PUBLIC_URL as the node was given
	// it, and base the path the listener answers under.
	origin, internal, public, base string
	log                            func() string
	alice                          string
}

// serveUnder runs `cellad serve` with a base path and a public URL, either of
// which may be empty, and an administrator's bearer. The public URL names a
// host nothing resolves: the node reads its path and signs with it, and the
// tests reach the listener by its address.
func serveUnder(t *testing.T, base, public string) *mounted {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	origin, internal, log, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":   issuer.URL(),
		"CELLA_ADMIN_SUBJECTS": issuer.URL() + "|alice",
		"CELLA_BASE_PATH":      base,
		"CELLA_PUBLIC_URL":     public,
	})
	t.Cleanup(func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	})
	return &mounted{origin: origin, internal: internal, public: public, base: base, log: log,
		alice: issuer.Mint(issuertest.Claims{Sub: "alice"})}
}

// answer is one exchange as a case reads it.
type answer struct {
	status int
	header http.Header
	body   string
}

// fetch sends one request, with a bearer when token is set, and does not
// follow a redirect, so a case reads the Location itself.
func fetch(t *testing.T, method, url, token, body string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{status: res.StatusCode, header: res.Header, body: string(data)}
}

// route is one path a listener is asked for, with what it should answer.
type route struct {
	path   string
	bearer bool
	status int
	// holds is what the body starts with or contains, empty for no check.
	holds string
}

// ask asks every route of a table and reports each answer that differs.
func (m *mounted) ask(t *testing.T, routes []route) {
	t.Helper()
	for _, r := range routes {
		token := ""
		if r.bearer {
			token = m.alice
		}
		got := fetch(t, http.MethodGet, m.origin+r.path, token, "")
		if got.status != r.status || !strings.Contains(got.body, r.holds) {
			t.Errorf("GET %s answered %d %.120q, want %d holding %q", r.path, got.status, got.body, r.status, r.holds)
		}
	}
}

// TestThePublicListenerMountsUnderTheBasePath: with a base, the API, the key
// set, the API document, the build identity and the build line answer under
// it, with the base in the place of /v1; nothing answers outside it; the
// probes are the internal listener's, unchanged; and the start-up line names
// the base.
func TestThePublicListenerMountsUnderTheBasePath(t *testing.T) {
	m := serveUnder(t, basePath, "https://api.example.com"+basePath)
	m.ask(t, []route{
		{path: basePath + "/sandboxes", bearer: true, status: http.StatusOK, holds: `"items"`},
		{path: basePath + "/secrets", bearer: true, status: http.StatusOK, holds: `"items"`},
		// The feed needs an object; its refusal is the API's own envelope.
		{path: basePath + "/events", bearer: true, status: http.StatusBadRequest, holds: `"invalid_field"`},
		{path: basePath + "/environments", bearer: true, status: http.StatusOK, holds: `"default"`},
		{path: basePath + "/environments/default", bearer: true, status: http.StatusOK, holds: `"kind":"Environment"`},
		{path: basePath + "/sandboxes", status: http.StatusUnauthorized, holds: `"unauthenticated"`},
		{path: basePath + "/.well-known/jwks.json", status: http.StatusOK, holds: `"keys"`},
		{path: basePath + "/openapi.yaml", status: http.StatusOK, holds: "\n  " + basePath + "/sandboxes:\n"},
		{path: basePath + "/version", status: http.StatusOK, holds: `"version":"dev"`},
		{path: basePath + "/", status: http.StatusOK, holds: "cellad dev ("},
		// The probes are not public under a base: the path is inside the
		// API, which answers it as it answers any path without a bearer.
		{path: basePath + "/livez", status: http.StatusUnauthorized},
		// Outside the base there is nothing, not even an envelope.
		{path: "/v1/sandboxes", bearer: true, status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/v1/environments/v1/sandboxes", bearer: true, status: http.StatusNotFound},
		{path: "/.well-known/jwks.json", status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/openapi.yaml", status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/version", status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/livez", status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/readyz", status: http.StatusNotFound, holds: "404 page not found"},
		{path: "/", status: http.StatusNotFound, holds: "404 page not found"},
	})
	for _, p := range []string{"/livez", "/readyz"} {
		if got := fetch(t, http.MethodGet, m.internal+p, "", ""); got.status != http.StatusOK || got.body != "ok\n" {
			t.Errorf("the internal listener answered %s with %d %q", p, got.status, got.body)
		}
	}
	if got := fetch(t, http.MethodGet, m.internal+"/version", "", ""); got.status != http.StatusOK {
		t.Errorf("the internal listener answered /version with %d", got.status)
	}
	if got := fetch(t, http.MethodGet, m.origin+basePath+"/openapi.yaml", "", ""); strings.Contains(got.body, "\n  /v1/sandboxes:\n") {
		t.Error("the document served under the base names a rooted path")
	}
	if !strings.Contains(m.log(), " base="+basePath+"\n") {
		t.Errorf("the start-up line does not name the base: %q", m.log())
	}
}

// TestAnEmptyBasePathIsTheRoot: with neither a base nor a path on the public
// URL, every route answers at the root as it always has, the document is the
// carried one byte for byte, and the start-up line says the root.
func TestAnEmptyBasePathIsTheRoot(t *testing.T) {
	m := serveUnder(t, "", "https://cella.example.com")
	m.ask(t, []route{
		{path: "/v1/sandboxes", bearer: true, status: http.StatusOK, holds: `"items"`},
		{path: "/v1/environments/default", bearer: true, status: http.StatusOK, holds: `"kind":"Environment"`},
		{path: "/.well-known/jwks.json", status: http.StatusOK, holds: `"keys"`},
		{path: "/version", status: http.StatusOK, holds: `"version":"dev"`},
		{path: "/livez", status: http.StatusOK, holds: "ok"},
		{path: "/readyz", status: http.StatusOK, holds: "ok"},
		{path: "/", status: http.StatusOK, holds: "cellad dev ("},
	})
	if got := fetch(t, http.MethodGet, m.origin+"/openapi.yaml", "", ""); got.body != string(apidoc.Document) {
		t.Error("the document served at the root is not the carried one")
	}
	created := fetch(t, http.MethodPost, m.origin+"/v1/sandboxes", m.alice,
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"rooted"},"spec":{}}`)
	if created.status != http.StatusCreated || !strings.HasPrefix(created.header.Get("Location"), "/v1/sandboxes/sbx_") {
		t.Errorf("the create answered %d with the Location %q", created.status, created.header.Get("Location"))
	}
	if !strings.Contains(m.log(), " base=/\n") {
		t.Errorf("the start-up line does not name the root: %q", m.log())
	}
}

// TestBehindARewriteTheListenerStaysAtTheRoot: a public URL with a path and no
// base is a control plane behind a proxy that rewrites the prefix away. The
// listener answers at the root, and every path it writes, a Location and the
// served document's paths, is under the public path the proxy serves.
func TestBehindARewriteTheListenerStaysAtTheRoot(t *testing.T) {
	m := serveUnder(t, "", "https://api.example.com"+basePath)
	m.ask(t, []route{
		{path: "/v1/sandboxes", bearer: true, status: http.StatusOK, holds: `"items"`},
		{path: "/openapi.yaml", status: http.StatusOK, holds: "\n  " + basePath + "/sandboxes:\n"},
		{path: "/livez", status: http.StatusOK, holds: "ok"},
	})
	created := fetch(t, http.MethodPost, m.origin+"/v1/sandboxes", m.alice,
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"rewritten"},"spec":{}}`)
	if created.status != http.StatusCreated || !strings.HasPrefix(created.header.Get("Location"), basePath+"/sandboxes/sbx_") {
		t.Errorf("the create answered %d with the Location %q", created.status, created.header.Get("Location"))
	}
	if !strings.Contains(m.log(), " base=/\n") {
		t.Errorf("the start-up line does not name the root: %q", m.log())
	}
}

// TestServingUnderABasePathEndToEnd is a control plane mounted under a base,
// driven the way a caller behind the origin drives it: the Location of a
// create is followed, a sandbox's workload token and an environment key carry
// the public URL with its path as their issuer and are accepted, the port path
// without its slash redirects under the base to the server inside, which is
// told the prefix it is reached at, and the dial and exec sockets open under
// the base.
func TestServingUnderABasePathEndToEnd(t *testing.T) {
	public := "https://api.example.com" + basePath
	m := serveUnder(t, basePath, public)
	under := m.origin + basePath

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, free, err := net.SplitHostPort(freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(free)
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal([]string{exe, serveArg, strconv.Itoa(port)})
	if err != nil {
		t.Fatal(err)
	}
	created := fetch(t, http.MethodPost, under+"/sandboxes", m.alice,
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"web"},`+
			`"spec":{"command":`+string(command)+`,"network":{"ports":[{"name":"web","port":`+strconv.Itoa(port)+`}]}}}`)
	if created.status != http.StatusCreated {
		t.Fatalf("the create under the base answered %d %s", created.status, created.body)
	}
	location := created.header.Get("Location")
	if !strings.HasPrefix(location, basePath+"/sandboxes/sbx_") {
		t.Fatalf("the create answered the Location %q, want it under %s", location, basePath)
	}
	id := strings.TrimPrefix(location, basePath+"/sandboxes/")
	if got := fetch(t, http.MethodGet, m.origin+location, m.alice, ""); got.status != http.StatusOK || !strings.Contains(got.body, id) {
		t.Fatalf("the Location answered %d %s", got.status, got.body)
	}

	t.Run("the tokens name the public URL and are accepted", func(t *testing.T) {
		cella, err := client.New(client.Config{URL: under, Token: client.StaticToken(m.alice)})
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := cella.Exec(t.Context(), id, client.ExecRequest{Command: []string{"sh", "-c", `cat "$` + driver.TokenFileEnv + `"`}})
		if err != nil {
			t.Fatal(err)
		}
		workload := strings.TrimSpace(result.Stdout)
		if iss := issuerOf(t, workload); iss != public {
			t.Errorf("the workload token names the issuer %q, want %q", iss, public)
		}
		if got := fetch(t, http.MethodGet, under+"/sandboxes/"+id, workload, ""); got.status != http.StatusOK {
			t.Errorf("the sandbox's own token was refused under the base: %d %s", got.status, got.body)
		}
		minted := fetch(t, http.MethodPost, under+"/environments/default/keys", m.alice, "")
		var key struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(minted.body), &key); minted.status != http.StatusCreated || err != nil {
			t.Fatalf("the key mint under the base answered %d %s", minted.status, minted.body)
		}
		if iss := issuerOf(t, key.Token); iss != public {
			t.Errorf("the environment key names the issuer %q, want %q", iss, public)
		}
		// The key is verified and then held to the data plane's routes: a
		// forbidden answer is a key the verifier accepted.
		if got := fetch(t, http.MethodGet, under+"/sandboxes", key.Token, ""); got.status != http.StatusForbidden {
			t.Errorf("an environment key on a caller's route answered %d %s", got.status, got.body)
		}
	})

	t.Run("the port redirect and the proxy", func(t *testing.T) {
		portPath := under + "/sandboxes/" + id + "/ports/web"
		waitFor(t, "the proxy to reach the server inside", func() bool {
			return fetch(t, http.MethodGet, portPath+"/hello?x=1", m.alice, "").status == http.StatusOK
		})
		redirect := fetch(t, http.MethodGet, portPath, m.alice, "")
		if redirect.status != http.StatusTemporaryRedirect || redirect.header.Get("Location") != "web/" {
			t.Fatalf("the port path without its slash answered %d with the Location %q", redirect.status, redirect.header.Get("Location"))
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, portPath+"?x=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+m.alice)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.Request.URL.Path != basePath+"/sandboxes/"+id+"/ports/web/" || string(body) != servedBody+"/?x=1" {
			t.Errorf("the redirect was followed to %s and answered %q", res.Request.URL, body)
		}
		prefix := fetch(t, http.MethodGet, portPath+prefixPath, m.alice, "")
		if want := basePath + "/sandboxes/" + id + "/ports/web"; prefix.body != want {
			t.Errorf("the server inside was told the prefix %q, want %q", prefix.body, want)
		}
	})

	t.Run("the sockets", func(t *testing.T) {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+m.alice)
		dialer := websocket.Dialer{Subprotocols: []string{"cella.dial.v1"}, HandshakeTimeout: 5 * time.Second}
		conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(under, "http")+"/sandboxes/"+id+"/dial/"+strconv.Itoa(port), header)
		if err != nil {
			t.Fatalf("the dial socket did not open under the base: %v", err)
		}
		_ = conn.Close()

		cella, err := client.New(client.Config{URL: under, Token: client.StaticToken(m.alice)})
		if err != nil {
			t.Fatal(err)
		}
		session, err := cella.ExecSession(t.Context(), id, client.ExecRequest{Command: []string{"sh", "-c", "echo under the base"}})
		if err != nil {
			t.Fatalf("the exec socket did not open under the base: %v", err)
		}
		defer func() { _ = session.Close() }()
		var out bytes.Buffer
		if _, err = io.Copy(&out, session); err != nil {
			t.Fatal(err)
		}
		if code, err := session.Wait(); code != 0 || err != nil || !strings.Contains(out.String(), "under the base") {
			t.Errorf("the exec socket ran with %d %v and wrote %q", code, err, out.String())
		}
	})
}

// issuerOf reads the iss a token names, without verifying it: the case asks
// what the node wrote, and the node's own verifier is asked by the request
// that follows.
func issuerOf(t *testing.T, token string) string {
	t.Helper()
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := jwt.DecodePayload(token, &claims); err != nil {
		t.Fatalf("the token does not decode: %v", err)
	}
	return claims.Iss
}

// TestWorkerAndGatewayUnderABasePath: a worker and a gateway whose CELLA_URL
// is the public URL of a control plane mounted under a base register, hold
// their stream, and serve the sandboxes placed on them.
func TestWorkerAndGatewayUnderABasePath(t *testing.T) {
	proxyAddr, reverseAddr := freePort(t), freePort(t)
	p := startPlaneWith(t, proxyAddr, reverseAddr, map[string]string{
		"CELLA_BASE_PATH":  basePath,
		"CELLA_PUBLIC_URL": "https://control.example.com" + basePath,
	})
	if !strings.HasSuffix(p.url, basePath) {
		t.Fatalf("the plane's public URL is %s", p.url)
	}

	// The gateway's stream, opened by the egress role with the public URL:
	// a boundary that needs a gateway is created only once one acknowledges
	// the sandbox's map.
	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	go func() {
		codec <- run(ctx, []string{"egress"}, env(map[string]string{
			"CELLA_URL":                 p.url,
			"CELLA_ENVIRONMENT_KEY":     p.environmentKey(t),
			"CELLA_EGRESS_PROXY_ADDR":   proxyAddr,
			"CELLA_EGRESS_REVERSE_ADDR": reverseAddr,
		}), &out, &errOut)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-codec:
			if code != 0 {
				t.Errorf("the egress role exited %d; stderr %q", code, errOut.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("the egress role did not stop")
		}
	})
	waitFor(t, "the egress role to open its doors", func() bool { return strings.Contains(out.String(), "egress proxy=") })
	if sandbox := p.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`); sandbox == "" {
		t.Fatal("the create returned no sandbox")
	}

	// The worker's registration and its stream: an environment it serves
	// becomes ready, and a sandbox placed on it runs.
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"eu-gpu"},` +
		`"spec":{"mode":"worker","isolation":"none","capacity":{"cpu":"8","memory":"16Gi","sandboxes":10}}}`
	if status, answer := p.do(t, http.MethodPut, "/v1/environments/eu-gpu", strings.NewReader(body)); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	status, answer := p.do(t, http.MethodPost, "/v1/environments/eu-gpu/keys", nil)
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(answer), &minted); status != http.StatusCreated || err != nil {
		t.Fatalf("the key mint answered %d: %s", status, answer)
	}
	worker := startWorker(t, p, minted.Token, nil)
	waitFor(t, "the environment to report the worker", func() bool {
		return p.environmentNamed(t, "eu-gpu").Status.Phase == "Ready"
	})
	create := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"there"},"spec":{"environment":"eu-gpu","command":["sleep","300"]}}`
	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(create))
	var sandbox struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(answer), &sandbox); status != http.StatusCreated || err != nil {
		t.Fatalf("the create on the worker's environment answered %d: %s", status, answer)
	}
	waitFor(t, "the sandbox to run on the worker", func() bool {
		_, answer := p.do(t, http.MethodGet, "/v1/sandboxes/"+sandbox.Status.ID, nil)
		return strings.Contains(answer, `"phase":"Running"`)
	})
	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes/"+sandbox.Status.ID+"/exec?wait=1",
		strings.NewReader(`{"command":["sh","-c","echo across the seam"]}`))
	if status != http.StatusOK || !strings.Contains(answer, "across the seam") {
		t.Fatalf("the exec on the worker answered %d: %s", status, answer)
	}
	if !strings.Contains(worker.out.String(), "control-plane="+p.url) {
		t.Errorf("the worker did not report the control plane it reached: %q", worker.out.String())
	}
}
