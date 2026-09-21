// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cellaclient is the typed client the cella command speaks /v1
// with: one method per route of design 008, the error envelope decoded into
// one error type, and the streams of the exec and attach sockets over the
// package's own WebSocket implementation.
//
// It reaches the standard library, this module's contract types and the
// error envelope of latere.ai/x/pkg/httpjson, which is what design 011 fixes
// the agent client's build list to.
package cellaclient

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
	"os"
	"strconv"
	"strings"
	"time"
)

// The environment design 011 reads, and nothing else: no configuration file
// and no login, because a token comes from the caller's issuer or, inside a
// sandbox, from the projection.
const (
	URLEnv       = "CELLA_URL"
	TokenEnv     = "CELLA_TOKEN"
	TokenFileEnv = "CELLA_TOKEN_FILE"
)

// DefaultTokenPath is where a driver projects a sandbox's own token
// (design 045). It is the default of --token-file, so a workload inside a
// sandbox authenticates with no flag at all.
const DefaultTokenPath = "/run/cella/token"

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

// Config is what a caller supplies. Every field has an environment default
// design 011 names; a field set here overrides it.
type Config struct {
	// URL is the control plane's address, CELLA_URL when empty.
	URL string
	// Token is the bearer, CELLA_TOKEN when empty, and the token file when
	// both are.
	Token string
	// TokenFile is the file the bearer is read from per request,
	// CELLA_TOKEN_FILE when empty and DefaultTokenPath when both are.
	TokenFile string
	// CAFile adds one certificate authority to the system roots.
	CAFile string
	// UserAgent is the identity every request carries.
	UserAgent string
	// Getenv reads the environment. Nil is os.Getenv.
	Getenv func(string) string
}

// Client speaks /v1. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	http      *http.Client
	tls       *tls.Config
	token     string
	tokenFile string
	agent     string
}

// New builds a client. It resolves the address and the token source and
// refuses a configuration that names neither.
func New(cfg Config) (*Client, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := cfg.URL
	if raw == "" {
		raw = getenv(URLEnv)
	}
	if raw == "" {
		return nil, fmt.Errorf("no control plane address: set %s or --url", URLEnv)
	}
	base, err := url.Parse(raw)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("%s is no http or https address: %s", URLEnv, raw)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	token := cfg.Token
	if token == "" {
		token = getenv(TokenEnv)
	}
	file := cfg.TokenFile
	if file == "" {
		file = getenv(TokenFileEnv)
	}
	if token == "" && file == "" {
		file = DefaultTokenPath
	}
	trust, err := roots(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	// The transport reads no proxy and no trust-store variable: a run
	// inside a sandbox reaches CELLA_URL through the driver's own rule and
	// not through the egress gateway, whose map has no entry for the
	// control plane (design 018).
	tlsConfig := &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: firstByteTimeout,
		ForceAttemptHTTP2:     false,
	}
	agent := cfg.UserAgent
	if agent == "" {
		agent = "cella"
	}
	return &Client{
		base:      base,
		http:      &http.Client{Transport: transport},
		tls:       tlsConfig,
		token:     token,
		tokenFile: file,
		agent:     agent,
	}, nil
}

// roots are the system authorities plus the one a caller named.
func roots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate authority: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", caFile)
	}
	return pool, nil
}

// NoBearer reports that no token could be resolved: neither flag, neither
// variable, and no readable file at the projection's path. It is a separate
// type because a command invoked without a credential is a usage error and
// not a server that refused.
type NoBearer struct {
	// Path is the file that was read, where one was.
	Path string
	Err  error
}

func (e *NoBearer) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("no bearer: set %s, or %s to a readable file (%v)", TokenEnv, TokenFileEnv, e.Err)
	}
	return fmt.Sprintf("no bearer: %s holds none and %s names none", e.Path, TokenEnv)
}
func (e *NoBearer) Unwrap() error { return e.Err }

// bearer resolves the token for one request. The file is read per request
// and never once at start: the controller re-projects it before expiry and
// a followed log outlives one token.
func (c *Client) bearer() (string, error) {
	if c.token != "" {
		return c.token, nil
	}
	data, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", &NoBearer{Path: c.tokenFile, Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", &NoBearer{Path: c.tokenFile}
	}
	return token, nil
}

// request builds one call: the path under the base address, the bearer of
// this moment, a fresh request id, and the identity every request carries.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	token, err := c.bearer()
	if err != nil {
		return nil, err
	}
	target := *c.base
	target.Path = c.base.Path + path
	if query != nil {
		target.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("X-Request-Id", RequestID())
	return req, nil
}

// RequestID is the id a request carries, within the rule of design 008: a
// prefix and printable ASCII.
func RequestID() string { return "req_" + rand.Text() }

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
