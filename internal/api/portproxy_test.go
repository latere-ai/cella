// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// webBody is a sandbox declaring one port by name.
func webBody(name string) string {
	return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
		`"spec":{"network":{"ports":[{"name":"web","port":8080}]}}}`
}

// seen is what a server inside was sent, as it answers it back.
type seen struct {
	Method     string      `json:"method"`
	RequestURI string      `json:"requestURI"`
	Host       string      `json:"host"`
	Header     http.Header `json:"header"`
	Body       string      `json:"body"`
	Sandbox    string      `json:"sandbox"`
}

// upstream is a server standing for one inside a sandbox: it answers every
// request with what it was sent and the name it was given, upgrades /ws to an
// echo socket, and answers /teapot with a status and a header of its own.
func upstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err = conn.WriteMessage(kind, data); err != nil {
					return
				}
			}
		case "/teapot":
			w.Header().Set("X-Upstream", "yes")
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, "short and stout")
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{
			Method: r.Method, RequestURI: r.RequestURI, Host: r.Host, Header: r.Header,
			Body: string(body), Sandbox: name,
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// proxied sends one request through the proxy and returns the answer.
func (f *fixture) proxied(method, path, token string, body io.Reader, header http.Header) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	f.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// decodeSeen reads what the server inside was sent.
func decodeSeen(t *testing.T, res *http.Response) seen {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("the proxy answered %d: %s", res.StatusCode, body)
	}
	var s seen
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// envelopeOf reads an error envelope's code and sentence.
func envelopeOf(t *testing.T, res *http.Response) (string, string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("the answer is no envelope: %v", err)
	}
	return env.Error.Code, env.Error.Message
}

// proxyFixture is a control plane whose driver dials the upstream each
// sandbox id was given, and one sandbox declaring the port "web".
func proxyFixture(t *testing.T) (*fixture, *recordingDialer, v1.Sandbox, *httptest.Server) {
	t.Helper()
	var d *recordingDialer
	f := setupDriver(t, nil, func(base runtime.Driver) runtime.Driver {
		d = &recordingDialer{Driver: base}
		return d
	})
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, webBody("site"), 201), &obj); err != nil {
		t.Fatal(err)
	}
	server := upstream(t, obj.Status.ID)
	d.dial = dialTo(server.Listener.Addr().String())
	return f, d, obj, server
}

// TestPortProxy is design 023's proxy over real HTTP: every method, the path
// suffix with its escaping, the query, the body and the headers reach the
// server inside with the bearer and the hop-by-hop set dropped; its status,
// headers and body come back; a WebSocket upgrade passes through; and each
// refusal answers its code.
func TestPortProxy(t *testing.T) {
	f, d, obj, _ := proxyFixture(t)
	base := "/v1/sandboxes/" + obj.Status.ID + "/ports/web"

	t.Run("every method", func(t *testing.T) {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "PROPFIND"} {
			got := decodeSeen(t, f.proxied(method, base+"/any", f.alice, strings.NewReader("payload"), nil))
			if got.Method != method {
				t.Errorf("%s reached the server inside as %s", method, got.Method)
			}
		}
		res := f.proxied("HEAD", base+"/any", f.alice, nil, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("HEAD answered %d", res.StatusCode)
		}
	})

	t.Run("the path, the query and the body", func(t *testing.T) {
		got := decodeSeen(t, f.proxied("POST", base+"/a%2Fb/c%20d?x=1&y=two&x=3", f.alice, strings.NewReader("the body"), nil))
		if got.RequestURI != "/a%2Fb/c%20d?x=1&y=two&x=3" {
			t.Errorf("the server inside was asked for %q", got.RequestURI)
		}
		if got.Body != "the body" {
			t.Errorf("the body arrived as %q", got.Body)
		}
		root := decodeSeen(t, f.proxied("GET", base+"/", f.alice, nil, nil))
		if root.RequestURI != "/" {
			t.Errorf("the root reached the server inside as %q", root.RequestURI)
		}
	})

	t.Run("the headers", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Custom", "kept")
		header.Set("Connection", "X-Hop")
		header.Set("X-Hop", "dropped")
		header.Set("X-Forwarded-Host", "forged.example.com")
		header.Set("X-Forwarded-Prefix", "/forged")
		got := decodeSeen(t, f.proxied("GET", base+"/h", f.alice, nil, header))
		if got.Header.Get("X-Custom") != "kept" {
			t.Errorf("an end-to-end header was dropped: %v", got.Header)
		}
		if got.Header.Get("Authorization") != "" {
			t.Errorf("the caller's bearer reached the server inside: %v", got.Header)
		}
		if got.Header.Get("X-Hop") != "" {
			t.Errorf("a hop-by-hop header was forwarded: %v", got.Header)
		}
		if got.Host != "localhost:8080" {
			t.Errorf("the server inside was asked for host %q", got.Host)
		}
		if want := strings.TrimPrefix(f.url, "http://"); got.Header.Get("X-Forwarded-Host") != want {
			t.Errorf("X-Forwarded-Host is %q, want %q", got.Header.Get("X-Forwarded-Host"), want)
		}
		if got.Header.Get("X-Forwarded-Prefix") != base || got.Header.Get("X-Forwarded-Proto") != "http" {
			t.Errorf("the forwarded prefix and proto are %q and %q", got.Header.Get("X-Forwarded-Prefix"), got.Header.Get("X-Forwarded-Proto"))
		}
	})

	t.Run("the answer comes back", func(t *testing.T) {
		res := f.proxied("GET", base+"/teapot", f.alice, nil, nil)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusTeapot || res.Header.Get("X-Upstream") != "yes" || string(body) != "short and stout" {
			t.Errorf("the answer came back as %d %v %q", res.StatusCode, res.Header, body)
		}
	})

	t.Run("a websocket passes through", func(t *testing.T) {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+f.alice)
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.url, "http")+base+"/ws", header)
		if err != nil {
			t.Fatalf("the upgrade did not pass through: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if err = conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, data, err := conn.ReadMessage(); err != nil || string(data) != "ping" {
			t.Fatalf("the socket echoed %q, %v", data, err)
		}
	})

	t.Run("an undeclared name", func(t *testing.T) {
		before := len(d.asked())
		res := f.proxied("GET", "/v1/sandboxes/"+obj.Status.ID+"/ports/db/", f.alice, nil, nil)
		if code, _ := envelopeOf(t, res); res.StatusCode != http.StatusNotFound || code != "not_found" {
			t.Errorf("an undeclared name answered %d %s", res.StatusCode, code)
		}
		if len(d.asked()) != before {
			t.Error("an undeclared name reached the dialer")
		}
	})

	t.Run("nothing listens", func(t *testing.T) {
		d.mu.Lock()
		d.dial = dialTo(closedAddr(t))
		d.mu.Unlock()
		res := f.proxied("GET", base+"/", f.alice, nil, nil)
		code, message := envelopeOf(t, res)
		if res.StatusCode != http.StatusBadGateway || code != "upstream_unavailable" || message != "Nothing is listening on that port." {
			t.Errorf("a closed port answered %d %s %q", res.StatusCode, code, message)
		}
	})

	t.Run("a server that answers nothing", func(t *testing.T) {
		d.mu.Lock()
		d.dial = func(context.Context, string, int) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				// Read the request, then hang up without a status line.
				_, _ = http.ReadRequest(bufio.NewReader(server))
				_ = server.Close()
			}()
			return client, nil
		}
		d.mu.Unlock()
		res := f.proxied("GET", base+"/", f.alice, nil, nil)
		if code, _ := envelopeOf(t, res); res.StatusCode != http.StatusBadGateway || code != "upstream_unavailable" {
			t.Errorf("a server that hung up answered %d %s", res.StatusCode, code)
		}
	})

	t.Run("a stopped sandbox", func(t *testing.T) {
		f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
		before := len(d.asked())
		res := f.proxied("GET", base+"/", f.alice, nil, nil)
		if code, _ := envelopeOf(t, res); res.StatusCode != http.StatusBadGateway || code != "upstream_unavailable" {
			t.Errorf("a stopped sandbox answered %d %s", res.StatusCode, code)
		}
		if len(d.asked()) != before {
			t.Error("a stopped sandbox was dialed")
		}
	})
}

// TestPortProxyGates: the proxy reads, authorizes and gates as the dial route
// does, before any driver call.
func TestPortProxyGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wrap   func(runtime.Driver) runtime.Driver
		detail string
	}{
		{"undeclared", func(d runtime.Driver) runtime.Driver { return noDialDriver{d} }, "reaches no port"},
		{"declared and unimplemented", func(d runtime.Driver) runtime.Driver { return dialDriver{d} }, "implements no Dialer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDriver(t, nil, tc.wrap)
			var obj v1.Sandbox
			if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, webBody("site"), 201), &obj); err != nil {
				t.Fatal(err)
			}
			body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/ports/web/", f.alice, "", 422)
			if !strings.Contains(string(body), tc.detail) {
				t.Fatalf("the gate answered %q", body)
			}
		})
	}
	f, _, obj, _ := proxyFixture(t)
	f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/ports/web/", f.bob, "", 403)
	f.request("GET", "/v1/sandboxes/sbx_01j0000000000000000000000/ports/web/", f.alice, "", 404)
}

// TestPortProxyIsConfined is the threat model's control on the proxy: the
// dialer is only ever asked for the sandbox's own id and a port it declared,
// whatever host, port, path or header the caller writes, and two sandboxes
// that declare one port each reach their own server, however the requests
// interleave.
func TestPortProxyIsConfined(t *testing.T) {
	var d *recordingDialer
	f := setupDriver(t, nil, func(base runtime.Driver) runtime.Driver {
		d = &recordingDialer{Driver: base}
		return d
	})
	create := func(name string) v1.Sandbox {
		var obj v1.Sandbox
		if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, webBody(name), 201), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	first, second := create("first"), create("second")
	servers := map[string]*httptest.Server{
		first.Status.ID:  upstream(t, first.Status.ID),
		second.Status.ID: upstream(t, second.Status.ID),
	}
	var mu sync.Mutex
	d.dial = func(ctx context.Context, id string, port int) (net.Conn, error) {
		mu.Lock()
		server, ok := servers[id]
		mu.Unlock()
		if !ok || port != 8080 {
			return nil, fmt.Errorf("asked for %s:%d, which no sandbox declared", id, port)
		}
		return dialTo(server.Listener.Addr().String())(ctx, id, port)
	}

	t.Run("each sandbox reaches its own", func(t *testing.T) {
		for range 3 {
			for _, obj := range []v1.Sandbox{first, second, second, first} {
				got := decodeSeen(t, f.proxied("GET", "/v1/sandboxes/"+obj.Status.ID+"/ports/web/", f.alice, nil, nil))
				if got.Sandbox != obj.Status.ID {
					t.Fatalf("a request for %s reached %s", obj.Status.ID, got.Sandbox)
				}
			}
		}
	})

	t.Run("nothing the caller writes moves the target", func(t *testing.T) {
		base := "/v1/sandboxes/" + first.Status.ID + "/ports/web"
		header := http.Header{}
		header.Set("X-Forwarded-Host", second.Status.ID)
		for _, path := range []string{
			base + "/?host=127.0.0.1&port=22",
			base + "/%2e%2e/%2e%2e/" + second.Status.ID + "/ports/web/",
			base + "/http://127.0.0.1:22/",
		} {
			res := f.proxied("GET", path, f.alice, nil, header)
			if res.StatusCode == http.StatusOK {
				if got := decodeSeen(t, res); got.Sandbox != first.Status.ID {
					t.Fatalf("%s reached %s", path, got.Sandbox)
				}
			}
		}
		// A request whose line names another host and whose Host header
		// names another still reaches the sandbox the path names.
		conn, err := net.Dial("tcp", strings.TrimPrefix(f.url, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, err = fmt.Fprintf(conn, "GET http://127.0.0.1:22%s/ HTTP/1.1\r\nHost: 169.254.169.254\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", base, f.alice)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if got := decodeSeen(t, res); got.Sandbox != first.Status.ID {
			t.Fatalf("an absolute request line reached %s", got.Sandbox)
		}
	})

	t.Run("a name that is not declared", func(t *testing.T) {
		before := len(d.asked())
		for _, name := range []string{"8080", "22", "db"} {
			res := f.proxied("GET", "/v1/sandboxes/"+first.Status.ID+"/ports/"+name+"/", f.alice, nil, nil)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("the name %q answered %d", name, res.StatusCode)
			}
		}
		if len(d.asked()) != before {
			t.Error("an undeclared name reached the dialer")
		}
	})

	t.Run("another subject's sandbox", func(t *testing.T) {
		before := len(d.asked())
		res := f.proxied("GET", "/v1/sandboxes/"+first.Status.ID+"/ports/web/", f.bob, nil, nil)
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusNotFound {
			t.Errorf("another subject's request answered %d", res.StatusCode)
		}
		if len(d.asked()) != before {
			t.Error("another subject's request reached the dialer")
		}
	})

	for _, call := range d.asked() {
		if !slices.Contains([]string{first.Status.ID, second.Status.ID}, call.id) || call.port != 8080 {
			t.Errorf("the dialer was asked for %+v", call)
		}
	}
}

// TestProxyPath splits the escaped path into the route and the server's own.
func TestProxyPath(t *testing.T) {
	for _, tc := range []struct{ in, prefix, suffix string }{
		{"/v1/sandboxes/s/ports/web/", "/v1/sandboxes/s/ports/web", "/"},
		{"/v1/sandboxes/s/ports/web/a%2Fb/c", "/v1/sandboxes/s/ports/web", "/a%2Fb/c"},
		{"/v1/sandboxes/s/ports/web", "/v1/sandboxes/s/ports/web", "/"},
	} {
		req := httptest.NewRequest("GET", tc.in, nil)
		prefix, suffix := proxyPath(req.URL)
		if prefix != tc.prefix || suffix != tc.suffix {
			t.Errorf("%s split into %q and %q", tc.in, prefix, suffix)
		}
	}
	if got := unescapedPath("/a%zz"); got != "/a%zz" {
		t.Errorf("an undecodable path became %q", got)
	}
	if got := unescapedPath("/a%2Fb"); got != "/a/b" {
		t.Errorf("the path decoded to %q", got)
	}
}

// TestPortPathWithoutItsSlashRedirectsRelatively: a port named without the
// slash that opens the proxied path answers 307 with a Location relative to
// the path the client used, the escaped segment and a slash, the query kept.
// The redirect reads nothing, so every method, a caller who may not reach the
// sandbox and an id that names none are all answered alike, without a dial.
func TestPortPathWithoutItsSlashRedirectsRelatively(t *testing.T) {
	f, d, obj, _ := proxyFixture(t)
	base := "/v1/sandboxes/" + obj.Status.ID + "/ports/"
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "PROPFIND"} {
		res := f.proxied(method, base+"web", f.alice, strings.NewReader("payload"), nil)
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != "web/" {
			t.Errorf("%s answered %d with the Location %q", method, res.StatusCode, res.Header.Get("Location"))
		}
	}
	for _, tc := range []struct {
		name, path, token, location string
	}{
		{"the query is kept", base + "web?x=1&y=a%20b", f.alice, "web/?x=1&y=a%20b"},
		{"the segment keeps its escaping", base + "w%20b", f.alice, "w%20b/"},
		{"a segment with a colon is not a scheme", base + "a:b", f.alice, "./a:b/"},
		{"a caller who may not reach the sandbox", base + "web", f.bob, "web/"},
		{"an id that names no sandbox", "/v1/sandboxes/sbx_01j0000000000000000000000/ports/web", f.alice, "web/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := f.proxied("GET", tc.path, tc.token, nil, nil)
			if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != tc.location {
				t.Errorf("%s answered %d with the Location %q, want %q", tc.path, res.StatusCode, res.Header.Get("Location"), tc.location)
			}
		})
	}
	if res := f.proxied("GET", base+"web", "", nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a request with no bearer answered %d", res.StatusCode)
	}
	if n := len(d.asked()); n != 0 {
		t.Errorf("a redirect reached the dialer %d times", n)
	}
}

// TestPortRedirectThroughAPrefixProxy is the redirect behind a proxy that
// serves the control plane under a prefix of its own and forwards the
// Location unchanged: a client that names the port without its slash and
// follows the answer stays under the prefix and reaches the server inside,
// with the method, the body and the query it sent.
func TestPortRedirectThroughAPrefixProxy(t *testing.T) {
	f, _, obj, _ := proxyFixture(t)
	target, err := url.Parse(f.url)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(http.StripPrefix("/cella", httputil.NewSingleHostReverseProxy(target)))
	t.Cleanup(front.Close)
	port := front.URL + "/cella/v1/sandboxes/" + obj.Status.ID + "/ports/web"
	for _, method := range []string{"GET", "POST"} {
		req, err := http.NewRequest(method, port+"?x=1", strings.NewReader("the body"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		got := decodeSeen(t, res)
		if res.Request.URL.Path != "/cella/v1/sandboxes/"+obj.Status.ID+"/ports/web/" {
			t.Errorf("%s was redirected to %s, outside the prefix or without the slash", method, res.Request.URL)
		}
		if got.Method != method || got.RequestURI != "/?x=1" {
			t.Errorf("%s reached the server inside as %s %s", method, got.Method, got.RequestURI)
		}
		if method == "POST" && got.Body != "the body" {
			t.Errorf("the redirected POST carried %q", got.Body)
		}
	}
}
