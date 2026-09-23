// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gwebsocket "github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdyconn "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	tunneling "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"

	driver "latere.ai/x/cella/runtime"
)

// echoPort is the declared port the fixtures serve, and closedPort a declared
// port nothing listens on.
const (
	echoPort   = 8080
	closedPort = 9090
)

// withPorts is the fixture spec with both ports declared.
func withPorts(id string) driver.CreateSpec {
	s := spec(id)
	s.Ports = []driver.Port{{Name: "echo", Port: echoPort}, {Name: "closed", Port: closedPort}}
	return s
}

// forwardCall is one session the driver asked the seam for.
type forwardCall struct {
	pod  string
	port int
}

// forwardRecorder stands in for the port forwarding session: it records the
// Pod and port and hands back one end of a pipe, or fails.
type forwardRecorder struct {
	mu    sync.Mutex
	calls []forwardCall
	err   error
}

func (r *forwardRecorder) forward(_ context.Context, pod string, port int) (net.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, forwardCall{pod: pod, port: port})
	if r.err != nil {
		return nil, r.err
	}
	near, far := net.Pipe()
	_ = far.Close()
	return near, nil
}

// TestDialRefusals: every refusal is the contract's sentinel and none opens a
// session, because the driver decides from the two objects before it asks the
// API server for anything.
func TestDialRefusals(t *testing.T) {
	h := newHarness(t)
	seam := &forwardRecorder{}
	h.forward = seam
	const id = "sbx_dial"
	h.created(t, withPorts(id))

	for _, port := range []int{0, -1, 65536} {
		if _, err := h.Dial(t.Context(), id, port); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Dial on port %d: %v, want ErrInvalid", port, err)
		}
	}
	if _, err := h.Dial(t.Context(), "sbx_absent", echoPort); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Dial on an absent sandbox: %v, want ErrNotFound", err)
	}
	if _, err := h.Dial(t.Context(), "not an id", echoPort); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("Dial on an id that is no object name: %v, want ErrNotFound", err)
	}
	_, err := h.Dial(t.Context(), id, 22)
	if !errors.Is(err, driver.ErrNotFound) || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("Dial on an undeclared port: %v, want ErrNotFound naming the rule", err)
	}

	// A Pod that is not running and ready is not a running sandbox: the
	// image still pulling, and the workload not yet ready.
	for name, status := range map[string]corev1.PodStatus{
		"pending": {Phase: corev1.PodPending},
		"starting": {Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: Container, Ready: false},
		}},
	} {
		pod := h.podOf(t, id)
		pod.Status = status
		if _, err := h.cs.CoreV1().Pods(namespace).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Dial(t.Context(), id, echoPort); !errors.Is(err, driver.ErrNotRunning) {
			t.Errorf("Dial on a %s Pod: %v, want ErrNotRunning", name, err)
		}
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Dial(t.Context(), id, echoPort); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("Dial on a stopped sandbox: %v, want ErrNotRunning", err)
	}
	if len(seam.calls) != 0 {
		t.Fatalf("a refused dial opened a session: %+v", seam.calls)
	}

	// A driver built with a client and no cluster configuration has no way
	// to open a session, and says so rather than failing to dial.
	h.created(t, withPorts("sbx_other"))
	h.forward = nil
	if _, err := h.Dial(t.Context(), "sbx_other", echoPort); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Dial with no cluster connection: %v, want ErrUnsupported", err)
	}
}

// TestDialRefusesAClaimWithoutASpec: the declared ports are the claim's
// record, so a claim that carries none is refused rather than dialed.
func TestDialRefusesAClaimWithoutASpec(t *testing.T) {
	h := newHarness(t)
	h.forward = &forwardRecorder{}
	const id = "sbx_nospec"
	h.created(t, withPorts(id))
	pvc := h.claimOf(t, id)
	delete(pvc.Annotations, annSpec)
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Update(t.Context(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Dial(t.Context(), id, echoPort); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("Dial on a claim with no spec: %v, want ErrInvalid", err)
	}
}

// TestDialForwardsTheDeclaredPort: the session is asked for the sandbox's own
// Pod and the port the caller named, and a session that fails is an error
// naming the sandbox and the port.
func TestDialForwardsTheDeclaredPort(t *testing.T) {
	h := newHarness(t)
	seam := &forwardRecorder{}
	h.forward = seam
	const id = "sbx_Forward"
	h.created(t, withPorts(id))
	conn, err := h.Dial(t.Context(), id, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []forwardCall{{pod: objectName(id), port: echoPort}}; !slices.Equal(seam.calls, want) {
		t.Fatalf("the sessions asked for are %+v, want %+v", seam.calls, want)
	}
	seam.err = errors.New("the API server is gone")
	_, err = h.Dial(t.Context(), id, echoPort)
	if err == nil || !strings.Contains(err.Error(), "port 8080 of "+id) || !strings.Contains(err.Error(), "the API server is gone") {
		t.Fatalf("a failed session: %v", err)
	}
}

// fakeAPIServer stands in for the API server and the kubelet behind it on the
// port forwarding subresource. It answers the WebSocket tunnel and the SPDY
// upgrade with the stream protocol, pairs each error stream with its data
// stream by request id, and serves a pair as the kubelet does: an echo on a
// port that listens, and the kubelet's reason on the error stream, then both
// streams closed, on one that does not.
type fakeAPIServer struct {
	*httptest.Server
	t *testing.T

	mu       sync.Mutex
	requests []string
	headers  []http.Header
	pending  map[string]httpstream.Stream
	sessions []httpstream.Connection

	// refuse answers a method with 403, as a Role without the verb does.
	refuse map[string]bool
	// hang holds the SPDY upgrade until the test ends.
	hang chan struct{}
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	s := &fakeAPIServer{t: t, pending: map[string]httpstream.Stream{}, refuse: map[string]bool{}}
	s.Server = httptest.NewServer(s)
	t.Cleanup(func() {
		s.mu.Lock()
		if s.hang != nil {
			close(s.hang)
		}
		sessions := slices.Clone(s.sessions)
		s.mu.Unlock()
		for _, session := range sessions {
			_ = session.Close()
		}
		s.Close()
	})
	return s
}

// driver builds a driver whose claims and Pods are the harness's and whose
// sessions go to this server.
func (s *fakeAPIServer) driver(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(o *Options) { o.REST = &rest.Config{Host: s.URL} })
}

func (s *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	refused, hang := s.refuse[r.Method], s.hang
	s.mu.Unlock()
	if refused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,`+
			`"message":"pods \"x\" is forbidden: cannot `+strings.ToLower(r.Method)+` resource pods/portforward"}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		upgrader := gwebsocket.Upgrader{Subprotocols: []string{tunneling.WebsocketsSPDYTunnelingPortForwardV1}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			s.t.Errorf("the WebSocket upgrade: %v", err)
			return
		}
		session, err := spdyconn.NewServerConnection(portforward.NewTunnelingConnection("server", conn), s.accept)
		if err != nil {
			s.t.Errorf("the tunneled session: %v", err)
			return
		}
		s.keep(session)
	case http.MethodPost:
		if hang != nil {
			<-hang
			return
		}
		if _, err := httpstream.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
			s.t.Errorf("the protocol handshake: %v", err)
			return
		}
		session := spdyconn.NewResponseUpgrader().UpgradeResponse(w, r, s.accept)
		if session == nil {
			s.t.Error("the SPDY upgrade failed")
			return
		}
		s.keep(session)
	default:
		http.Error(w, "no such method", http.StatusMethodNotAllowed)
	}
}

func (s *fakeAPIServer) keep(session httpstream.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, session)
}

// accept pairs the streams the way the kubelet does, by request id.
func (s *fakeAPIServer) accept(stream httpstream.Stream, replySent <-chan struct{}) error {
	s.mu.Lock()
	s.headers = append(s.headers, stream.Headers().Clone())
	id := stream.Headers().Get(corev1.PortForwardRequestIDHeader)
	other, ok := s.pending[id]
	if !ok {
		s.pending[id] = stream
		s.mu.Unlock()
		return nil
	}
	delete(s.pending, id)
	s.mu.Unlock()
	errs, data := other, stream
	if errs.Headers().Get(corev1.StreamType) != corev1.StreamTypeError {
		errs, data = data, errs
	}
	go serve(errs, data, replySent)
	return nil
}

// serve is the kubelet's half of one pair: the reason written before both
// streams close, the error stream first.
func serve(errs, data httpstream.Stream, replySent <-chan struct{}) {
	<-replySent
	defer func() { _ = data.Close() }()
	defer func() { _ = errs.Close() }()
	port, _ := strconv.Atoi(data.Headers().Get(corev1.PortHeader))
	if port != echoPort {
		_, _ = fmt.Fprintf(errs, "error forwarding port %d to pod x: failed to connect to localhost:%d inside namespace: connection refused", port, port)
		return
	}
	_, _ = io.Copy(data, data)
}

func (s *fakeAPIServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// roundTrip writes one line and reads it back.
func roundTrip(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatalf("writing %q: %v", line, err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading %q back: %v", line, err)
	}
	if got != line+"\n" {
		t.Fatalf("sent %q and read %q", line, got)
	}
}

// TestPortForwardCarriesBytesBothWays: over the WebSocket tunnel an API server
// serves, two connections at once each carry a line both ways, each in a
// session of its own with one error and one data stream naming the port and
// request id 0.
func TestPortForwardCarriesBytesBothWays(t *testing.T) {
	server := newFakeAPIServer(t)
	h := server.driver(t)
	const id = "sbx_bytes"
	h.created(t, withPorts(id))
	first, err := h.Dial(t.Context(), id, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := h.Dial(t.Context(), id, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	roundTrip(t, first, "first")
	roundTrip(t, second, "second")
	roundTrip(t, first, "first again")

	path := "/api/v1/namespaces/" + namespace + "/pods/" + objectName(id) + "/portforward"
	if want := []string{"GET " + path, "GET " + path}; !slices.Equal(server.seen(), want) {
		t.Fatalf("the requests are %v, want %v", server.seen(), want)
	}
	server.mu.Lock()
	headers := slices.Clone(server.headers)
	server.mu.Unlock()
	if len(headers) != 4 {
		t.Fatalf("the sessions opened %d streams, want two pairs", len(headers))
	}
	for i, h := range headers {
		want := corev1.StreamTypeError
		if i%2 == 1 {
			want = corev1.StreamTypeData
		}
		if h.Get(corev1.StreamType) != want || h.Get(corev1.PortHeader) != strconv.Itoa(echoPort) || h.Get(corev1.PortForwardRequestIDHeader) != "0" {
			t.Errorf("stream %d carries %v, want a %s stream for port %d and request 0", i, h, want, echoPort)
		}
	}
}

// TestPortForwardFallsBackToTheUpgrade: where the API server refuses the
// WebSocket, as one that predates it does and as a Role granting create and
// not get does, the session is the SPDY upgrade.
func TestPortForwardFallsBackToTheUpgrade(t *testing.T) {
	server := newFakeAPIServer(t)
	server.refuse[http.MethodGet] = true
	h := server.driver(t)
	const id = "sbx_fallback"
	h.created(t, withPorts(id))
	conn, err := h.Dial(t.Context(), id, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	roundTrip(t, conn, "over the upgrade")
	path := "/api/v1/namespaces/" + namespace + "/pods/" + objectName(id) + "/portforward"
	if want := []string{"GET " + path, "POST " + path}; !slices.Equal(server.seen(), want) {
		t.Fatalf("the requests are %v, want %v", server.seen(), want)
	}
}

// TestPortForwardOutlivesTheDialContext: the dial route cancels its bound as
// soon as Dial returns, so a connection tied to that context would die in the
// caller's hands.
func TestPortForwardOutlivesTheDialContext(t *testing.T) {
	for _, websocketServed := range []bool{true, false} {
		t.Run(map[bool]string{true: "websocket", false: "upgrade"}[websocketServed], func(t *testing.T) {
			server := newFakeAPIServer(t)
			server.refuse[http.MethodGet] = !websocketServed
			h := server.driver(t)
			const id = "sbx_outlives"
			h.created(t, withPorts(id))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			conn, err := h.Dial(ctx, id, echoPort)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			roundTrip(t, conn, "after the context ended")
		})
	}
}

// TestPortForwardReportsTheKubeletsReason: a port nothing listens on opens a
// session, and the kubelet's reason is what the connection's Read returns,
// never a clean end of stream a caller would read as the server closing.
func TestPortForwardReportsTheKubeletsReason(t *testing.T) {
	server := newFakeAPIServer(t)
	h := server.driver(t)
	const id = "sbx_closed"
	h.created(t, withPorts(id))
	conn, err := h.Dial(t.Context(), id, closedPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(conn)
	if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("reading a closed port: %v, want the kubelet's reason", err)
	}
}

// TestPortForwardRefused: an API server that refuses both transports, as one
// facing a Role with neither verb does, is an error from Dial and no
// connection.
func TestPortForwardRefused(t *testing.T) {
	server := newFakeAPIServer(t)
	server.refuse[http.MethodGet] = true
	server.refuse[http.MethodPost] = true
	h := server.driver(t)
	const id = "sbx_refused"
	h.created(t, withPorts(id))
	conn, err := h.Dial(t.Context(), id, echoPort)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a refused session dialed")
	}
	if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("a refused session: %v, want the API server's refusal", err)
	}
}

// TestPortForwardHonorsTheDialContext: an upgrade the API server never answers
// ends with the dial's context rather than holding the caller.
func TestPortForwardHonorsTheDialContext(t *testing.T) {
	server := newFakeAPIServer(t)
	server.refuse[http.MethodGet] = true
	server.hang = make(chan struct{})
	h := server.driver(t)
	const id = "sbx_hangs"
	h.created(t, withPorts(id))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	conn, err := h.Dial(ctx, id, echoPort)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a session nobody answered dialed")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an unanswered upgrade: %v, want the context's deadline", err)
	}
}

// TestPortForwardCloseEndsTheSession: closing the connection delivers what
// was written before it and ends the session, so the kubelet's end of the
// pair closes too.
func TestPortForwardCloseEndsTheSession(t *testing.T) {
	server := newFakeAPIServer(t)
	h := server.driver(t)
	const id = "sbx_close"
	h.created(t, withPorts(id))
	conn, err := h.Dial(t.Context(), id, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn, "before the close")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if _, err := conn.Write([]byte("after")); err == nil {
		t.Fatal("a write after Close succeeded")
	}
	server.mu.Lock()
	session := server.sessions[0]
	server.mu.Unlock()
	select {
	case <-session.CloseChan():
	case <-time.After(10 * time.Second):
		t.Fatal("the server's end of the session is still open after the connection closed")
	}
}

// TestEveryDialRequestIsInTheVerbTable: each request the dial makes, over the
// WebSocket and over the upgrade it falls back to, is an access Preflight
// reviews. A request outside the table is one the Role does not grant and
// Preflight does not name.
func TestEveryDialRequestIsInTheVerbTable(t *testing.T) {
	table := map[string]bool{}
	for _, v := range verbs {
		table[fmt.Sprintf("%s/%s/%s", v.resource, v.subresource, v.verb)] = true
	}
	// The verb the API server authorizes a request as, by its method.
	verbOf := map[string]string{http.MethodGet: "get", http.MethodPost: "create"}
	server := newFakeAPIServer(t)
	h := server.driver(t)
	const id = "sbx_verbs"
	h.created(t, withPorts(id))
	for _, websocketServed := range []bool{true, false} {
		server.mu.Lock()
		server.refuse[http.MethodGet] = !websocketServed
		server.mu.Unlock()
		conn, err := h.Dial(t.Context(), id, echoPort)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range server.seen() {
		method, path, _ := strings.Cut(request, " ")
		parts := strings.Split(strings.Trim(path, "/"), "/")
		// /api/v1/namespaces/<ns>/pods/<name>/<subresource>
		if len(parts) != 7 || parts[4] != "pods" {
			t.Errorf("the dial requested %s, which is no subresource of a Pod", request)
			continue
		}
		key := fmt.Sprintf("%s/%s/%s", parts[4], parts[6], verbOf[method])
		if !table[key] {
			t.Errorf("the dial sends %s, which the API server authorizes as %s and the verbs table does not review", request, key)
		}
	}
}

// TestPortForwardDials: a session to an API server that does not answer is an
// error of the connection, returned as it is rather than retried over the
// upgrade, which would fail the same way.
func TestPortForwardDials(t *testing.T) {
	d, err := New(Options{Namespace: namespace, REST: &rest.Config{Host: "https://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := d.forward.forward(ctx, "pod", echoPort)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a session to a closed port dialed")
	}
	if httpstream.IsUpgradeFailure(err) {
		t.Fatalf("a refused connection reads as a refused upgrade: %v", err)
	}
}

// TestNewRefusesAConfigurationThePortForwardCannotUse: the session's address
// is built from the configuration at New, so one it cannot read is a
// construction error rather than a failure at the first dial.
func TestNewRefusesAConfigurationThePortForwardCannotUse(t *testing.T) {
	_, err := New(Options{Namespace: namespace, Client: newHarness(t).cs, REST: &rest.Config{Host: "http://[::1"}})
	if err == nil {
		t.Fatal("a configuration with an unreadable host built a driver")
	}
}
