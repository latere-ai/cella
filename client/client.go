// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package client is the typed client of the /v1 API: one method per route of
// design 008, the error envelope decoded into one error type, and the streams
// of the exec, attach and dial sockets over the package's own WebSocket
// implementation.
//
// It reaches the standard library, this module's contract types and the
// error envelope of latere.ai/x/pkg/httpjson, which is what design 011 fixes
// the agent client's build list to.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent is the identity a request carries when the caller names
// none.
const DefaultUserAgent = "cella-client"

// firstByteTimeout is design 011's one deadline: ten seconds to the first
// response byte and none on the stream that follows. It is expressed as
// Transport.ResponseHeaderTimeout, which starts after the request body is
// written, so an upload of any size is not cut short by it.
const firstByteTimeout = 10 * time.Second

// dialTimeout bounds reaching the address before any status exists.
const dialTimeout = 10 * time.Second

// maxErrorBytes bounds the body read from a refusal, which is an envelope
// and never a stream.
const maxErrorBytes = 1 << 20

// Config is what a caller supplies. Nothing in it is read from anywhere else:
// Environment is the call that fills one from the process's environment.
type Config struct {
	// URL is the control plane's base address, http or https. It is
	// required, and a path on it prefixes every route.
	URL string
	// Token is asked for the bearer once per request, with that request's
	// context. Nil sends no Authorization header, which only /version
	// answers.
	Token TokenSource
	// HTTPClient carries every call, the upgrade of the exec, attach and
	// dial sockets included, so a caller's proxy, dialer, trust and
	// instrumentation reach all of them. Nil is a client over the
	// package's own transport. A Timeout on it bounds a whole exchange,
	// which cuts a followed stream short and leaves a socket nothing to
	// write to; bound a call with its context instead.
	HTTPClient *http.Client
	// RootCAs are the authorities the package's own transport trusts, the
	// system roots when nil. A caller that supplies HTTPClient sets its
	// trust there instead.
	RootCAs *x509.CertPool
	// UserAgent is the identity every request carries, DefaultUserAgent
	// when empty.
	UserAgent string
}

// Client speaks /v1. It is safe for concurrent use.
type Client struct {
	base  *url.URL
	http  *http.Client
	token TokenSource
	agent string
}

// New builds a client. It refuses a configuration with no address or with an
// address that is not http or https, so a mistake is reported before the
// first call rather than by it.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("the configuration names no control plane address")
	}
	base, err := url.Parse(cfg.URL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("%s is no http or https address", cfg.URL)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	carrier := cfg.HTTPClient
	if carrier == nil {
		carrier = &http.Client{Transport: transport(cfg.RootCAs)}
	}
	agent := cfg.UserAgent
	if agent == "" {
		agent = DefaultUserAgent
	}
	return &Client{base: base, http: carrier, token: cfg.Token, agent: agent}, nil
}

// transport is the package's own: it reads no proxy and no trust-store
// variable, because a run inside a sandbox reaches the control plane through
// the driver's own rule and not through the egress gateway, whose map has no
// entry for the control plane (design 018). HTTP/2 is not attempted; an
// upgrade is an HTTP/1.1 exchange, and a connection that negotiated h2 would
// leave nothing to upgrade.
func transport(roots *x509.CertPool) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
		ForceAttemptHTTP2:     false,
	}
}

// authorize puts the bearer of this moment on a request, where the caller
// configured a source. An empty token is no bearer.
func (c *Client) authorize(req *http.Request) error {
	if c.token == nil {
		return nil
	}
	token, err := c.token.Token(req.Context())
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// request builds one call: the path under the base address, the bearer of
// this moment, a fresh request id, and the identity every request carries.
//
// The path arrives escaped, each reference in it escaped as one segment, and
// is set as the URL's escaped form beside its decoded one. Setting only the
// decoded form would escape the escapes a second time, and the server would
// read a reference with a space in it as one with "%20" in it.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return nil, err
	}
	target := *c.base
	target.Path = c.base.Path + decoded
	target.RawPath = c.base.EscapedPath() + path
	if query != nil {
		target.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if err = c.authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("X-Request-Id", requestID())
	return req, nil
}

// requestID is the id a request carries, within the rule of design 008: a
// prefix and printable ASCII.
func requestID() string { return "req_" + rand.Text() }

// do sends a request and turns anything that is not a 2xx into an Error and
// anything that reached no status into an Unreachable.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unreachable(req, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	return nil, errorFrom(resp, body)
}

// unreachable names a failure that carried no status. A context a caller
// cancelled is that caller's own and is passed through.
func unreachable(req *http.Request, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &Unreachable{Op: req.Method + " " + req.URL.Path, Err: err}
}

// send is the common shape: one call whose whole answer is a JSON body.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body []byte, contentType string) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.request(ctx, method, path, query, reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// accepting sends one GET under an Accept header and returns the answer's own
// bytes. It is what -o yaml takes: design 008 renders the syntax, and this
// client passes the bytes through rather than decoding and re-encoding them.
func (c *Client) accepting(ctx context.Context, path string, query url.Values, accept string) ([]byte, error) {
	req, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// stream is a call whose answer the caller reads: the body is returned open
// for the caller to close, with the trailer that says a transfer which had
// already begun failed. Design 008 puts that failure in a trailer because a
// status can no longer carry it.
func (c *Client) stream(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (io.ReadCloser, http.Header, error) {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, nil, err
	}
	// The trailer is the same map the transport fills once the body has been
	// read to its end, so a caller holding it reads what arrived after the
	// last byte.
	return resp.Body, resp.Trailer, nil
}

// errorTrailer is the header design 008 names for a failure after the first
// byte.
const errorTrailer = "X-Cella-Error"

// limitValue renders a page size the API accepts: at most 200, which is the
// ceiling of design 008, so a caller asking for more pages instead of being
// refused.
func limitValue(want int) string {
	if want <= 0 || want > maxPage {
		want = maxPage
	}
	return strconv.Itoa(want)
}

// maxPage is design 008's ceiling on one page.
const maxPage = 200
