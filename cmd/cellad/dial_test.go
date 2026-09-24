// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/internal/cellacli"
)

// serveArg turns this test binary into the HTTP server a native sandbox runs
// as its main command, which is what the dial and proxy end-to-end reaches.
// The binary is already on the host, so the test needs no tool of the host's.
const serveArg = "cella-test-serve"

// servedBody is what the server inside answers, followed by the path it was
// asked for.
const servedBody = "served from inside the sandbox at "

// prefixPath is the one path the server inside answers with the
// X-Forwarded-Prefix it was sent, which is the prefix a caller reached it at.
const prefixPath = "/forwarded-prefix"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == serveArg {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, servedBody+r.URL.RequestURI())
		})
		mux.HandleFunc(prefixPath, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Header.Get("X-Forwarded-Prefix"))
		})
		server := &http.Server{Addr: net.JoinHostPort("127.0.0.1", os.Args[2]), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		fmt.Fprintln(os.Stderr, server.ListenAndServe())
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestDialAndPortProxy is design 008's row on a running node: a native
// sandbox serving HTTP on the port it declared is reached through the dial
// socket, with a request written as raw bytes and the answer read back, and
// through the port proxy by the port's name; once the sandbox stops, the proxy
// answers 502 and the socket is refused.
func TestDialAndPortProxy(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})

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
	obj := create(t, base, alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"web"},`+
		`"spec":{"command":`+string(command)+`,"network":{"ports":[{"name":"web","port":`+strconv.Itoa(port)+`}]}}}`)
	proxy := base + "/v1/sandboxes/" + obj.Status.ID + "/ports/web/hello?x=1"

	// The server inside comes up after the create answers, so the proxy is
	// asked until it stops answering 502.
	var answer string
	waitFor(t, "the proxy to reach the server inside", func() bool {
		status, body := call(t, http.MethodGet, proxy, alice)
		answer = body
		return status == http.StatusOK
	})
	if answer != servedBody+"/hello?x=1" {
		t.Fatalf("the proxy answered %q", answer)
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+alice)
	dialer := websocket.Dialer{Subprotocols: []string{"cella.dial.v1"}, HandshakeTimeout: 5 * time.Second}
	socket := "ws" + strings.TrimPrefix(base, "http") + "/v1/sandboxes/" + obj.Status.ID + "/dial/" + strconv.Itoa(port)
	conn, _, err := dialer.Dial(socket, header)
	if err != nil {
		t.Fatalf("the dial socket did not open: %v", err)
	}
	defer func() { _ = conn.Close() }()
	request := "GET /raw HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	if err = conn.WriteMessage(websocket.BinaryMessage, []byte(request)); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if kind == websocket.BinaryMessage {
				if _, err = pw.Write(data); err != nil {
					return
				}
			}
		}
	}()
	if err = conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(pr), nil)
	if err != nil {
		t.Fatalf("no HTTP answer came back through the dial socket: %v", err)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil && !strings.Contains(err.Error(), "close 1000") {
		t.Fatalf("reading the answer through the dial socket: %v", err)
	}
	if res.StatusCode != http.StatusOK || string(body) != servedBody+"/raw" {
		t.Fatalf("the dial socket carried back %d %q", res.StatusCode, body)
	}

	forwardThroughTheCommand(t, base, alice, obj.Status.ID, port)

	if status, body := call(t, http.MethodPost, base+"/v1/sandboxes/"+obj.Status.ID+"/stop", alice); status != http.StatusOK {
		t.Fatalf("stop answered %d %s", status, body)
	}
	status, refusal := call(t, http.MethodGet, proxy, alice)
	if status != http.StatusBadGateway || !strings.Contains(refusal, "upstream_unavailable") {
		t.Fatalf("the proxy to a stopped sandbox answered %d %s", status, refusal)
	}
	if _, res, err := dialer.Dial(socket, header); err == nil || res == nil || res.StatusCode != http.StatusConflict {
		t.Fatalf("the dial socket to a stopped sandbox was not refused: %v", err)
	}
}

// call is one request under a bearer, answered with its status and body.
func call(t *testing.T, method, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

// forwardThroughTheCommand runs `cella port-forward` against the node and
// reads the server inside through the loopback port it listens on, which is
// the client's own WebSocket against the server's.
func forwardThroughTheCommand(t *testing.T, base, token, id string, port int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var out, errOut syncBuffer
	codes := make(chan int, 1)
	go func() {
		codes <- cellacli.Run(ctx, cellacli.Env{
			Args: []string{"port-forward", id, "0:" + strconv.Itoa(port)}, Stdin: strings.NewReader(""),
			Stdout: &out, Stderr: &errOut, Version: "v0.0.0-test",
			Getenv: func(k string) string {
				return map[string]string{"CELLA_URL": base, "CELLA_TOKEN": token}[k]
			},
		})
	}()
	defer func() {
		cancel()
		if code := <-codes; code != 0 {
			t.Errorf("port-forward exited %d: %s", code, errOut.String())
		}
	}()
	line := regexp.MustCompile(`Forwarding (127\.0\.0\.1:\d+) `)
	var local string
	waitFor(t, "port-forward to listen", func() bool {
		m := line.FindStringSubmatch(out.String())
		if m != nil {
			local = m[1]
		}
		return m != nil
	})
	res, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + local + "/forwarded")
	if err != nil {
		t.Fatalf("no answer through port-forward: %v; %s", err, errOut.String())
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || string(body) != servedBody+"/forwarded" {
		t.Fatalf("port-forward carried back %d %q", res.StatusCode, body)
	}
}
