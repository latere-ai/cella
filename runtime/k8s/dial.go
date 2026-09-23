// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdyconn "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	tunneling "k8s.io/apimachinery/pkg/util/portforward"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	spdytransport "k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/transport/websocket"

	driver "latere.ai/x/cella/runtime"
)

var _ driver.Dialer = (*Driver)(nil)

// flushBound is how long closing a forwarded connection waits for the
// caller's last bytes to reach the stream before it ends the session. A Pod
// that stopped reading cannot hold a session open past it.
const flushBound = 5 * time.Second

// forwarder opens one connection to one port of one Pod. It is an interface
// for the reason streamer is: the session needs a live API server, and
// everything around it is exercised against a client double.
type forwarder interface {
	forward(ctx context.Context, pod string, port int) (net.Conn, error)
}

// Dial connects to a declared port of a running sandbox through the API
// server's port forwarding subresource. The kubelet makes the connection from
// inside the Pod's own network namespace to its loopback, so no
// NetworkPolicy stands between the control plane and the port, and a server
// that binds only loopback is reached as on the native driver.
//
// Only a declared port is dialed: the subresource reaches any port of the
// Pod, the desktop container's included, and the other container driver
// reaches only what the manifest declared. A port nothing listens on opens
// a connection whose first Read returns the kubelet's reason.
func (d *Driver) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: port %d is outside 1 to 65535", driver.ErrInvalid, port)
	}
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return nil, err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return nil, err
	}
	// The phase table and not the Pod's own phase: a Pod still pulling its
	// image, or one whose workload has not reported ready, is not running.
	if current, _, _, _ := phase(pvc, pod); current != driver.Running {
		return nil, driver.ErrNotRunning
	}
	spec, err := specOf(pvc)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(spec.Ports, func(p driver.Port) bool { return p.Port == port }) {
		return nil, fmt.Errorf("%w: port %d of %s is not declared; only a declared port is dialed", driver.ErrNotFound, port, id)
	}
	if d.forward == nil {
		return nil, fmt.Errorf("%w: this driver was built without a cluster connection", driver.ErrUnsupported)
	}
	conn, err := d.forward.forward(ctx, objectName(id), port)
	if err != nil {
		return nil, fmt.Errorf("k8s: dialing port %d of %s: %w", port, id, err)
	}
	return conn, nil
}

// portForward opens sessions on the pods/portforward subresource of one
// namespace. core builds the subresource's address from the same
// configuration the sessions authenticate with.
type portForward struct {
	cfg       *rest.Config
	core      rest.Interface
	namespace string
}

func newPortForward(cfg *rest.Config, namespace string) (*portForward, error) {
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &portForward{cfg: cfg, core: core.RESTClient(), namespace: namespace}, nil
}

// forward opens one session and one connection in it. The negotiation takes
// no context of its own past the handshakes, so it runs apart from the caller
// and is abandoned when ctx ends; a session that arrives after that is closed
// at once. The connection returned does not depend on ctx.
func (f *portForward) forward(ctx context.Context, pod string, port int) (net.Conn, error) {
	target := f.core.Post().Resource("pods").Namespace(f.namespace).Name(pod).SubResource("portforward").URL()
	type opened struct {
		conn net.Conn
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		conn, err := f.open(ctx, target, port)
		done <- opened{conn, err}
	}()
	select {
	case o := <-done:
		return o.conn, o.err
	case <-ctx.Done():
		go func() {
			if late := <-done; late.conn != nil {
				_ = late.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// open negotiates the session and pairs one connection in it.
func (f *portForward) open(ctx context.Context, target *url.URL, port int) (net.Conn, error) {
	session, err := f.negotiate(ctx, target)
	if err != nil {
		return nil, err
	}
	conn, err := pair(session, port)
	if err != nil {
		if cerr := session.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, err
	}
	return conn, nil
}

// negotiate is the order current kubectl takes: the WebSocket that tunnels
// the stream protocol, which an API server serves by default from 1.31, and
// the SPDY upgrade where the server refuses the WebSocket, as an older
// server does and as a Role that grants create and not get on the
// subresource does.
func (f *portForward) negotiate(ctx context.Context, target *url.URL) (httpstream.Connection, error) {
	session, err := f.tunnel(ctx, target)
	if err == nil {
		return session, nil
	}
	if !httpstream.IsUpgradeFailure(err) && !httpstream.IsHTTPSProxyError(err) {
		return nil, err
	}
	return f.upgrade(ctx, target)
}

// tunnel is the WebSocket session: a GET, which the API server authorizes as
// get on pods/portforward.
func (f *portForward) tunnel(ctx context.Context, target *url.URL) (httpstream.Connection, error) {
	transport, holder, err := websocket.RoundTripperFor(f.cfg)
	if err != nil {
		return nil, fmt.Errorf("port forward websocket: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("port forward websocket: %w", err)
	}
	conn, err := websocket.Negotiate(transport, holder, req, tunneling.WebsocketsSPDYTunnelingPrefix+portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, err
	}
	session, err := spdyconn.NewClientConnectionWithPings(portforward.NewTunnelingConnection("client", conn), portforward.PingPeriod)
	if err != nil {
		if cerr := conn.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, fmt.Errorf("port forward websocket: %w", err)
	}
	return session, nil
}

// upgrade is the SPDY session: a POST, which the API server authorizes as
// create on pods/portforward.
func (f *portForward) upgrade(ctx context.Context, target *url.URL) (httpstream.Connection, error) {
	transport, upgrader, err := spdytransport.RoundTripperFor(f.cfg)
	if err != nil {
		return nil, fmt.Errorf("port forward upgrade: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("port forward upgrade: %w", err)
	}
	session, _, err := spdytransport.Negotiate(upgrader, &http.Client{Transport: transport}, req, portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("port forward upgrade: %w", err)
	}
	return session, nil
}

// pair opens the two streams of one forwarded connection, as the kubelet
// reads them: the error stream first, which only the kubelet writes, then
// the data stream, both naming the port and one request id.
func pair(session httpstream.Connection, port int) (net.Conn, error) {
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(port))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errs, err := session.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("port forward error stream: %w", err)
	}
	// The driver never writes the error stream, so its half is closed now.
	if err := errs.Close(); err != nil {
		return nil, fmt.Errorf("port forward error stream: %w", err)
	}
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := session.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("port forward data stream: %w", err)
	}
	return newForwarded(session, data, errs), nil
}

// forwarded is one forwarded connection as a net.Conn: the caller's end of an
// in-memory pipe whose other end is pumped to the data stream, so deadlines
// behave as on any connection and the streams, which have none, need none.
type forwarded struct {
	net.Conn
	session httpstream.Connection
	// sent is closed once the caller's bytes are all on the data stream.
	sent chan struct{}
	once sync.Once

	mu     sync.Mutex
	reason error
}

// newForwarded starts the two pumps. When the inside's end finishes, the
// pump waits for the error stream to end as well, because the kubelet writes
// its reason there before it closes both streams; the reason is then what
// the caller's Read returns in place of io.EOF.
func newForwarded(session httpstream.Connection, data, errs httpstream.Stream) *forwarded {
	near, far := net.Pipe()
	c := &forwarded{Conn: near, session: session, sent: make(chan struct{})}
	said := make(chan error, 1)
	go func() {
		// A read that fails here is the session ending, which the data
		// stream's own read reports; only what the kubelet wrote is a reason.
		message, _ := io.ReadAll(errs)
		if len(message) > 0 {
			said <- fmt.Errorf("the port forwarding session ended: %s", message)
			return
		}
		said <- nil
	}()
	go func() {
		_, err := io.Copy(far, data)
		if reason := <-said; reason != nil {
			err = reason
		}
		c.end(err)
		// Closing a pipe end twice is not an error, and the other pump may
		// have closed it first.
		_ = far.Close()
	}()
	go func() {
		defer close(c.sent)
		if _, err := io.Copy(data, far); err != nil {
			// The stream refused the caller's bytes, so the connection is
			// over in both directions: without this the caller's next
			// Write would wait on a pipe nobody reads.
			c.end(err)
			_ = far.Close()
			return
		}
		// The caller closed its end: the inside reads the end of its bytes.
		if err := data.Close(); err != nil {
			c.end(err)
		}
	}()
	return c
}

// end records why the inside's end finished, once.
func (c *forwarded) end(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason == nil {
		c.reason = err
	}
}

// Read is the pipe's read, with the kubelet's reason in place of the end of
// the stream where it gave one.
func (c *forwarded) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) {
		c.mu.Lock()
		reason := c.reason
		c.mu.Unlock()
		if reason != nil {
			return n, reason
		}
	}
	return n, err
}

// Close ends the caller's end at once and the session once the caller's last
// bytes are on the stream, as closing a socket sends what was written before
// it, or after flushBound at the latest.
func (c *forwarded) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		go func() {
			select {
			case <-c.sent:
			case <-time.After(flushBound):
			}
			// The session is this connection's alone, and a failure to
			// close it is one no caller can act on after its own Close.
			if err := c.session.Close(); err != nil {
				slog.Debug("the port forwarding session did not close cleanly", "error", err)
			}
		}()
	})
	return err
}
