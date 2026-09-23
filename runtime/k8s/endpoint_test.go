// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdystream "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	driver "latere.ai/x/cella/runtime"
)

// endpoint serves the exec subresource the way an API server does, for the
// part of this driver that needs one: the executor's two transports, the
// terminal's resize messages, the end of stdin, and the exit status. What it
// runs is a line program: every line read is written back, "exit N" ends it
// with N, the end of its input ends it with 0, and every window the terminal
// is given is written as "size WxH". With a stderr stream it writes "err"
// there before it ends, so a case sees whether the two streams stayed apart.
type endpoint struct {
	t *testing.T
	// refuseWebSocket answers the WebSocket upgrade 403, as an API server
	// does for a Role without get on pods/exec, so the executor falls back.
	refuseWebSocket bool

	mu      sync.Mutex
	methods []string
	queries []url.Values
}

// served is a driver stream reaching the endpoint through the real executor.
func (e *endpoint) served(t *testing.T) streamer {
	t.Helper()
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	d, err := New(Options{Namespace: namespace, REST: &rest.Config{Host: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	return d.stream
}

func (e *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/exec") {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	e.methods = append(e.methods, r.Method)
	e.queries = append(e.queries, r.URL.Query())
	e.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if e.refuseWebSocket {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				Kind: "Status", APIVersion: "v1",
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
				Message: `pods "x" is forbidden: cannot get resource "pods/exec"`,
			})
			return
		}
		e.webSocket(w, r)
	case http.MethodPost:
		e.upgrade(w, r)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// seen is what the endpoint was asked, in order.
func (e *endpoint) seen() ([]string, []url.Values) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.methods), slices.Clone(e.queries)
}

// webSocket is the v5 channel protocol: one binary message per write, the
// first byte naming the channel.
func (e *endpoint) webSocket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	read := func(on bool) wsstream.ChannelType {
		if on {
			return wsstream.ReadChannel
		}
		return wsstream.IgnoreChannel
	}
	write := func(on bool) wsstream.ChannelType {
		if on {
			return wsstream.WriteChannel
		}
		return wsstream.IgnoreChannel
	}
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		remotecommandconsts.StreamProtocolV5Name: {Binary: true, Channels: []wsstream.ChannelType{
			read(q.Get("stdin") == "true"), write(q.Get("stdout") == "true"), write(q.Get("stderr") == "true"),
			wsstream.WriteChannel, read(q.Get("tty") == "true"),
		}},
	})
	_, channels, err := conn.Open(w, r)
	if err != nil {
		e.t.Errorf("opening the WebSocket: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	var stderr io.Writer
	if q.Get("stderr") == "true" {
		stderr = channels[remotecommandconsts.StreamStdErr]
	}
	var resize io.Reader
	if q.Get("tty") == "true" {
		resize = channels[remotecommandconsts.StreamResize]
	}
	code := run(channels[remotecommandconsts.StreamStdIn], channels[remotecommandconsts.StreamStdOut], stderr, resize)
	e.status(channels[remotecommandconsts.StreamErr], code)
}

// upgrade is the SPDY transport: the client opens one stream per channel,
// named by its streamType header, and the endpoint waits for each it asked
// for before the program runs.
func (e *endpoint) upgrade(w http.ResponseWriter, r *http.Request) {
	if _, err := httpstream.Handshake(r, w, []string{remotecommandconsts.StreamProtocolV4Name}); err != nil {
		e.t.Errorf("the SPDY handshake: %v", err)
		return
	}
	q := r.URL.Query()
	arrived := make(chan httpstream.Stream, 8)
	conn := spdystream.NewResponseUpgrader().UpgradeResponse(w, r, func(s httpstream.Stream, _ <-chan struct{}) error {
		arrived <- s
		return nil
	})
	if conn == nil {
		e.t.Error("the SPDY upgrade failed")
		return
	}
	defer func() { _ = conn.Close() }()
	want := map[string]bool{corev1.StreamTypeError: true}
	want[corev1.StreamTypeStdin] = q.Get("stdin") == "true"
	want[corev1.StreamTypeStdout] = q.Get("stdout") == "true"
	want[corev1.StreamTypeStderr] = q.Get("stderr") == "true"
	want[corev1.StreamTypeResize] = q.Get("tty") == "true"
	expected := 0
	for _, on := range want {
		if on {
			expected++
		}
	}
	streams := map[string]httpstream.Stream{}
	for len(streams) < expected {
		select {
		case s := <-arrived:
			streams[s.Headers().Get(corev1.StreamType)] = s
		case <-time.After(5 * time.Second):
			e.t.Errorf("the client opened %d of %d streams", len(streams), expected)
			return
		}
	}
	var stdin io.Reader = strings.NewReader("")
	if s, ok := streams[corev1.StreamTypeStdin]; ok {
		stdin = s
	}
	var stderr io.Writer
	if s, ok := streams[corev1.StreamTypeStderr]; ok {
		stderr = s
	}
	var resize io.Reader
	if s, ok := streams[corev1.StreamTypeResize]; ok {
		resize = s
	}
	code := run(stdin, streams[corev1.StreamTypeStdout], stderr, resize)
	e.status(streams[corev1.StreamTypeError], code)
	for _, s := range streams {
		_ = s.Close()
	}
}

// status writes the process's end on the error channel as the kubelet does:
// success, or a failure whose cause carries the exit code.
func (e *endpoint) status(w io.Writer, code int) {
	status := metav1.Status{Status: metav1.StatusSuccess}
	if code != 0 {
		status = metav1.Status{
			Status: metav1.StatusFailure, Reason: remotecommandconsts.NonZeroExitCodeReason,
			Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
				Type: remotecommandconsts.ExitCodeCauseType, Message: strconv.Itoa(code),
			}}},
		}
	}
	if err := json.NewEncoder(w).Encode(status); err != nil {
		e.t.Errorf("writing the status: %v", err)
	}
}

// run is the line program the endpoint executes.
func run(stdin io.Reader, stdout, stderr io.Writer, resize io.Reader) int {
	var mu sync.Mutex
	say := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = io.WriteString(stdout, s)
	}
	if resize != nil {
		go func() {
			sizes := json.NewDecoder(resize)
			for {
				var size remotecommand.TerminalSize
				if sizes.Decode(&size) != nil {
					return
				}
				say(fmt.Sprintf("size %dx%d\n", size.Width, size.Height))
			}
		}()
	}
	code := 0
	lines := bufio.NewScanner(stdin)
	for lines.Scan() {
		if n, ok := strings.CutPrefix(lines.Text(), "exit "); ok {
			parsed, err := strconv.Atoi(n)
			if err != nil {
				// A shell answers a word it cannot read as a number with 2.
				parsed = 2
			}
			code = parsed
			break
		}
		say(lines.Text() + "\n")
	}
	if stderr != nil {
		_, _ = io.WriteString(stderr, "err")
	}
	return code
}

// attachOver runs one terminal session against the endpoint and checks what
// both ends saw: the window, bytes both ways, a resize, and the exit code.
func attachOver(t *testing.T, e *endpoint, code int) {
	t.Helper()
	h := newHarness(t)
	const id = "sbx_wire"
	h.created(t, spec(id))
	h.stream = e.served(t)

	session, err := h.Attach(t.Context(), id, driver.AttachRequest{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	readUntil(t, session, "size 80x24")
	if _, err := io.WriteString(session, "hello\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, session, "hello")
	if err := session.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	readUntil(t, session, "size 120x40")
	if _, err := fmt.Fprintf(session, "exit %d\n", code); err != nil {
		t.Fatal(err)
	}
	if got, err := waited(t, session); err != nil || got != code {
		t.Fatalf("Wait = %d %v, want the status's exit code %d", got, err, code)
	}
	if _, err := io.ReadAll(session); err != nil {
		t.Fatalf("the stream did not end with the exec: %v", err)
	}

	// The command reads its input to the end, which the executor signals
	// when the caller's reader ends, and its two streams stay apart.
	x, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("ping\n")})
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := drain(x)
	if got, err := x.Wait(t.Context()); err != nil || got != 0 {
		t.Fatalf("Exec with stdin: Wait = %d %v", got, err)
	}
	if out != "ping\n" || errOut != "err" {
		t.Fatalf("Exec with stdin: streams %q %q", out, errOut)
	}
}

// terminalQuery is what the session's exec asked the API server for.
func terminalQuery(t *testing.T, q url.Values) {
	t.Helper()
	if q.Get("tty") != "true" || q.Get("stdin") != "true" || q.Get("stdout") != "true" || q.Get("stderr") == "true" {
		t.Errorf("the terminal's exec asked for %v, want tty, stdin and stdout and no stderr", q)
	}
	if argv := q["command"]; len(argv) == 0 || argv[0] != "env" || argv[len(argv)-1] != "sh" {
		t.Errorf("the terminal's exec ran %q, want the image's shell under the wrapper", argv)
	}
	if q.Get("container") != Container {
		t.Errorf("the terminal's exec reached the container %q", q.Get("container"))
	}
}

// TestTheExecSubresourceCarriesATerminal drives the real executor against a
// served endpoint speaking v5.channel.k8s.io over a WebSocket, which is what
// a current API server answers, so the terminal's query, its stdin, its
// resize channel and its exit status are exercised end to end short of a
// cluster.
func TestTheExecSubresourceCarriesATerminal(t *testing.T) {
	e := &endpoint{t: t}
	attachOver(t, e, 5)
	methods, queries := e.seen()
	if !slices.Equal(methods, []string{http.MethodGet, http.MethodGet}) {
		t.Fatalf("the executor reached the endpoint with %v, want the WebSocket's GET for each exec", methods)
	}
	terminalQuery(t, queries[0])
	if q := queries[1]; q.Get("tty") == "true" || q.Get("stdin") != "true" || q.Get("stderr") != "true" {
		t.Errorf("the exec with stdin asked for %v, want stdin and stderr and no tty", q)
	}
}

// TestTheExecStreamFallsBackToSPDY: an API server that refuses the WebSocket
// upgrade is reached over SPDY with the terminal intact, and the HTTP method
// of each transport is a verb Preflight reviews, since the API server
// authorizes a GET as get and a POST as create.
func TestTheExecStreamFallsBackToSPDY(t *testing.T) {
	e := &endpoint{t: t, refuseWebSocket: true}
	attachOver(t, e, 6)
	methods, queries := e.seen()
	if !slices.Equal(methods, []string{http.MethodGet, http.MethodPost, http.MethodGet, http.MethodPost}) {
		t.Fatalf("the executor reached the endpoint with %v, want a refused GET and then a POST for each exec", methods)
	}
	terminalQuery(t, queries[1])
	verb := map[string]string{http.MethodGet: "get", http.MethodPost: "create"}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if !slices.ContainsFunc(verbs, func(v struct{ group, resource, subresource, verb string }) bool {
			return v.group == "" && v.resource == "pods" && v.subresource == "exec" && v.verb == verb[method]
		}) {
			t.Errorf("the executor sends %s, which the API server authorizes as %s on pods/exec, and the verb table does not review it", method, verb[method])
		}
	}
}
