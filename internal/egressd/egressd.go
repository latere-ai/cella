// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package egressd is the egress role of cellad: the gateway that stands
// between a sandbox and the network. It holds one boundary per sandbox,
// pushed to it by the control plane over the one stream it opens outbound,
// and it decides every connection against that boundary before any dial.
//
// The role reaches latere.ai/x/pkg/egress, a WebSocket library and the
// standard library, and nothing of the control plane: it verifies no token,
// reads no store, and knows no issuer. What it trusts is the credential the
// map carries, which the control plane minted and the sandbox holds.
package egressd

import (
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
)

// CACommonName is the subject of the authority a gateway generates when the
// operator supplied none. It names the role, not an installation.
const CACommonName = "cella egress"

// Options is the egress role's configuration, as spec 002's table names it.
type Options struct {
	// URL is the control plane the gateway connects to, and Key the
	// environment key that authenticates the stream and names the
	// environment.
	URL string
	Key string
	// ProxyAddr and ReverseAddr are the two doors' listen addresses.
	ProxyAddr   string
	ReverseAddr string
	// ProxyListener and ReverseListener, when set, are doors already bound,
	// which the gateway serves on in place of listening at the two
	// addresses. A caller that has to name the doors to the control plane
	// before the gateway starts binds them first, so no other process can
	// take a port between the choice and the bind.
	ProxyListener   net.Listener
	ReverseListener net.Listener
	// CAPEM carries the authority the gateway terminates TLS with, both its
	// certificate and its private key. An environment with several gateways
	// sets the same value on each, so a sandbox trusts every one of them;
	// an empty value generates one at start, which is what a single gateway
	// wants.
	CAPEM string
	// UpstreamCAPEM are authorities the gateway trusts beside the system
	// roots when it dials a destination.
	UpstreamCAPEM string
	Log           *slog.Logger
	// Dial is the seam toward a destination. It is nil in an installation,
	// where the gateway dials directly.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// Ready, when set, is called once the gateway holds its first snapshot,
	// so a caller knows the doors are answering from the control plane's
	// world rather than an empty one.
	Ready func()
	// Now is the clock the records are stamped with.
	Now func() time.Time
}

// Gateway is one running egress role: two doors over one boundary, and one
// stream to the control plane that fills it.
type Gateway struct {
	opts        Options
	log         *slog.Logger
	store       *store
	gate        *gate
	client      *syncClient
	proxy       *http.Server
	reverse     *http.Server
	proxyLn     net.Listener
	reverseLn   net.Listener
	caPEM       string
	environment string
}

// New builds the role and binds both doors, so a caller that gets no error
// has two listening addresses and a gateway that has not yet connected. The
// context bounds the binding alone; Run takes the one that bounds the role.
func New(ctx context.Context, o Options) (*Gateway, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	environment, err := environmentOf(o.Key)
	if err != nil {
		return nil, err
	}
	stream, err := streamURL(o.URL, environment)
	if err != nil {
		return nil, err
	}
	ca, caPEM, err := authority(o.CAPEM)
	if err != nil {
		return nil, err
	}
	upstreamTLSConfig, err := upstreamTLS(o.UpstreamCAPEM)
	if err != nil {
		return nil, err
	}
	dial := o.Dial
	if dial == nil {
		var dialer net.Dialer
		dial = dialer.DialContext
	}
	// One transport reaches every upstream, the token endpoint of an oauth
	// secret included, so the dial seam and the operator's own authority
	// hold whichever path a request took to get here.
	upstream := &http.Transport{
		DialContext:         dial,
		TLSClientConfig:     upstreamTLSConfig,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	s := newStore(&http.Client{Transport: upstream, Timeout: tokenTimeout})
	client := &syncClient{
		url: stream, key: o.Key, gatewayID: gatewayID(), store: s, caPEM: caPEM,
		records: make(chan egress.Record, recordBuffer), log: o.Log,
		dialer:      &websocket.Dialer{Subprotocols: []string{egress.Protocol}, HandshakeTimeout: 15 * time.Second},
		onConnected: o.Ready,
	}
	g := &gate{
		store: s, controlPlane: hostOf(o.URL), dial: dial,
		gateway: &pkgegress.Gateway{
			Registry:             s.Registry(),
			CA:                   ca,
			Auth:                 credentialAuth{store: s},
			UpstreamTLS:          upstreamTLSConfig,
			BlockLoopbackTargets: true,
			Realm:                Realm,
			Log:                  o.Log,
		},
		upstream: upstream,
		records:  client.Record, log: o.Log, now: o.Now,
	}
	gw := &Gateway{opts: o, log: o.Log, store: s, gate: g, client: client, caPEM: caPEM, environment: environment}
	gw.proxy = &http.Server{Handler: http.HandlerFunc(g.ServeProxy), ReadHeaderTimeout: 30 * time.Second}
	gw.reverse = &http.Server{Handler: http.HandlerFunc(g.ServeReverse), ReadHeaderTimeout: 30 * time.Second}
	var listen net.ListenConfig
	gw.proxyLn = o.ProxyListener
	if gw.proxyLn == nil {
		if gw.proxyLn, err = listen.Listen(ctx, "tcp", o.ProxyAddr); err != nil {
			return nil, fmt.Errorf("CELLA_EGRESS_PROXY_ADDR: %w", err)
		}
	}
	gw.reverseLn = o.ReverseListener
	if gw.reverseLn == nil {
		if gw.reverseLn, err = listen.Listen(ctx, "tcp", o.ReverseAddr); err != nil {
			_ = gw.proxyLn.Close()
			return nil, fmt.Errorf("CELLA_EGRESS_REVERSE_ADDR: %w", err)
		}
	}
	return gw, nil
}

// ProxyAddr and ReverseAddr are the addresses the doors bound to, which a
// test that asked for port zero reads back.
func (g *Gateway) ProxyAddr() string   { return g.proxyLn.Addr().String() }
func (g *Gateway) ReverseAddr() string { return g.reverseLn.Addr().String() }

// CAPEM is the authority's certificate, which the control plane projects into
// every sandbox of the environment.
func (g *Gateway) CAPEM() string { return g.caPEM }

// Environment is the environment the key names.
func (g *Gateway) Environment() string { return g.environment }

// Run serves both doors and the stream until the context ends, then drains
// the doors. Every error is returned; none is dropped.
func (g *Gateway) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.log.InfoContext(ctx, "the egress gateway is up",
		"proxy", g.ProxyAddr(), "reverse", g.ReverseAddr(), "environment", g.environment)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i, door := range []struct {
		name     string
		server   *http.Server
		listener net.Listener
	}{{"proxy", g.proxy, g.proxyLn}, {"reverse", g.reverse, g.reverseLn}} {
		wg.Go(func() {
			defer cancel()
			if err := door.server.Serve(door.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs[i] = fmt.Errorf("the %s door stopped: %w", door.name, err)
			}
		})
	}
	wg.Go(func() {
		defer cancel()
		errs[2] = g.client.Run(ctx)
	})
	<-ctx.Done()
	g.Close(context.WithoutCancel(ctx))
	wg.Wait()
	g.log.InfoContext(context.WithoutCancel(ctx), "the egress gateway is down")
	return errors.Join(errs...)
}

// Close stops both doors. A connection in flight is given the drain period,
// after which it is cut: a tunnel has no request boundary to wait for.
func (g *Gateway) Close(ctx context.Context) {
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	_ = g.proxy.Shutdown(drain)
	_ = g.reverse.Shutdown(drain)
	_ = g.proxy.Close()
	_ = g.reverse.Close()
}

// drainTimeout is how long an in-flight connection has when the gateway is
// asked to stop, and tokenTimeout how long one mint at an oauth secret's
// endpoint has.
const (
	drainTimeout = 5 * time.Second
	tokenTimeout = 30 * time.Second
)

// credentialAuth is what the substitution engine's own proxy authenticates
// with: the same per-sandbox credential the gate read, looked up in the same
// store, so the two can never disagree about who is calling.
type credentialAuth struct{ store *store }

func (a credentialAuth) Authenticate(header string) (string, bool) {
	return a.store.Principal(proxyCredential(header))
}

// authority loads the operator's certificate authority or generates one. The
// value carries both PEM blocks, the certificate and the private key, because
// an authority is the pair: a gateway given only a key could not tell a
// sandbox what to trust.
func authority(combined string) (*pkgegress.CA, string, error) {
	if strings.TrimSpace(combined) == "" {
		// The generated key lives for this process only, which is why an
		// environment with several gateways sets one on each instead.
		ca, certPEM, _, err := pkgegress.GenerateCA(CACommonName)
		if err != nil {
			return nil, "", fmt.Errorf("generating the gateway's authority: %w", err)
		}
		return ca, string(certPEM), nil
	}
	certPEM, keyPEM, err := splitPEM(combined)
	if err != nil {
		return nil, "", err
	}
	ca, err := pkgegress.LoadCA(certPEM, keyPEM)
	if err != nil {
		return nil, "", fmt.Errorf("CELLA_EGRESS_CA_KEY: %w", err)
	}
	return ca, string(certPEM), nil
}

// splitPEM takes the certificate and the private key out of one PEM value, in
// whichever order they were written.
func splitPEM(combined string) (certPEM, keyPEM []byte, err error) {
	rest := []byte(combined)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE" && certPEM == nil:
			certPEM = pem.EncodeToMemory(block)
		case strings.HasSuffix(block.Type, "PRIVATE KEY") && keyPEM == nil:
			keyPEM = pem.EncodeToMemory(block)
		}
	}
	if certPEM == nil || keyPEM == nil {
		return nil, nil, errors.New("CELLA_EGRESS_CA_KEY carries the gateway's authority: one CERTIFICATE block and one PRIVATE KEY block")
	}
	return certPEM, keyPEM, nil
}

// gatewayID names this process on the stream, so a control plane's log says
// which of an environment's gateways acknowledged what.
func gatewayID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "gw"
	}
	return "gw-" + fmt.Sprintf("%x", b)
}

// hostOf is the host of a URL, which is what the gate refuses as a
// destination.
func hostOf(raw string) string {
	trimmed := strings.TrimSpace(raw)
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if rest, ok := strings.CutPrefix(trimmed, scheme); ok {
			trimmed = rest
			break
		}
	}
	if i := strings.IndexAny(trimmed, "/?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return hostOnly(trimmed)
}
