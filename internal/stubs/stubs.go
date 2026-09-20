// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package stubs is spec 012's operator endpoints as one process: the
// issuer a caller's token is minted at, the authorizer every decision is
// asked of, the admission endpoint every manifest passes, and the sink
// every record is delivered to. A clean clone therefore has a working
// system with `make run`, and a tier drives the real clients of specs 006,
// 007 and 009 against a real HTTP peer rather than a fake in the same
// process.
//
// Two roles are the family's own. The issuer is latere.ai/x/pkg/authkit/
// issuertest and the authorizer is latere.ai/x/pkg/authz/stub, each behind
// a listener of this package. The other two are written here, because a
// stub that stands opposite the code under test proves nothing when it
// shares that code's implementation: the sink verifies Cella-Signature
// with its own HMAC over the formula spec 009 states, and the admission
// endpoint decodes spec 007's envelope into its own struct.
//
// Every role listens on loopback by default, logs one line per request,
// and holds its state in memory: a restart is an empty stub.
package stubs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Role is one stub endpoint.
type Role string

// The four roles, in the order they are started and printed.
const (
	RoleIssuer     Role = "issuer"
	RoleAuthorizer Role = "authorizer"
	RoleAdmission  Role = "admission"
	RoleSink       Role = "sink"
)

// RoleOrder is every role in that order.
var RoleOrder = []Role{RoleIssuer, RoleAuthorizer, RoleAdmission, RoleSink}

// DefaultAddrs is where each role listens when its flag is left alone.
// They are loopback, so a stub started by accident is reachable from this
// machine and from nowhere else.
var DefaultAddrs = map[Role]string{
	RoleIssuer:     "127.0.0.1:9080",
	RoleAuthorizer: "127.0.0.1:9081",
	RoleAdmission:  "127.0.0.1:9082",
	RoleSink:       "127.0.0.1:9083",
}

// Options is what the binary parses out of its flags. A role whose Addr
// is empty is not started.
type Options struct {
	Issuer     IssuerOptions
	Authorizer AuthorizerOptions
	Admission  AdmissionOptions
	Sink       SinkOptions
	// Log receives one line per request, "<role> <method> <path>
	// <status>". A nil Log logs nothing, which is what a test that reads
	// the answers rather than the trace wants.
	Log io.Writer
}

// shutdownGrace bounds Close: a stub holds no work worth draining, and a
// hung request is one a fail mode put there on purpose.
const shutdownGrace = 2 * time.Second

// Stubs is the roles that were started.
type Stubs struct {
	mu      sync.Mutex
	log     io.Writer
	addrs   map[Role]string
	servers map[Role]*http.Server
	closers []func()
	wg      sync.WaitGroup
}

// Start listens for every configured role and serves it. The listeners
// are open when Start returns, so a caller may read URL and dial at once,
// and a port of 0 is resolved to the port the kernel gave. The context
// bounds the listening and not the serving: a stub runs until Close.
func Start(ctx context.Context, o Options) (*Stubs, error) {
	s := &Stubs{log: o.Log, addrs: map[Role]string{}, servers: map[Role]*http.Server{}}
	build := map[Role]func(addr string) (http.Handler, func(), error){
		RoleIssuer:     func(addr string) (http.Handler, func(), error) { return newIssuer(addr, o.Issuer) },
		RoleAuthorizer: func(string) (http.Handler, func(), error) { return newAuthorizer(o.Authorizer) },
		RoleAdmission:  func(string) (http.Handler, func(), error) { return newAdmission(o.Admission) },
		RoleSink:       func(string) (http.Handler, func(), error) { return newSink(o.Sink) },
	}
	addrs := map[Role]string{
		RoleIssuer: o.Issuer.Addr, RoleAuthorizer: o.Authorizer.Addr,
		RoleAdmission: o.Admission.Addr, RoleSink: o.Sink.Addr,
	}
	for _, role := range RoleOrder {
		if strings.TrimSpace(addrs[role]) == "" {
			continue
		}
		if err := s.serve(ctx, role, addrs[role], build[role]); err != nil {
			// The roles that did start are stopped on a context of their
			// own: the caller's may already be the reason this failed.
			_ = s.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("%s: %w", role, err)
		}
	}
	if len(s.servers) == 0 {
		return nil, errors.New("every role is turned off; a stub process with no listener serves nobody")
	}
	return s, nil
}

// serve opens one role's listener and runs its handler on it.
func (s *Stubs) serve(ctx context.Context, role Role, addr string, build func(addr string) (http.Handler, func(), error)) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	handler, closer, err := build(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.logged(role, handler), ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.addrs[role], s.servers[role] = ln.Addr().String(), srv
	if closer != nil {
		s.closers = append(s.closers, closer)
	}
	s.mu.Unlock()
	s.wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("%s stopped: %v", role, err)
		}
	})
	return nil
}

// Addr is the address a role listens on, and the empty string for a role
// that was not started.
func (s *Stubs) Addr(role Role) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addrs[role]
}

// URL is a role's base URL, and the empty string for a role that was not
// started. It is what the matching CELLA_ variable takes.
func (s *Stubs) URL(role Role) string {
	if addr := s.Addr(role); addr != "" {
		return "http://" + addr
	}
	return ""
}

// Close stops every listener. It is safe to call twice, so a caller that
// defers it and calls it on a signal does not have to track which ran.
func (s *Stubs) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, shutdownGrace)
	defer cancel()
	s.mu.Lock()
	servers, closers := s.servers, s.closers
	s.servers, s.closers = map[Role]*http.Server{}, nil
	s.mu.Unlock()
	// The fail modes park a request until the role is released, and a
	// graceful shutdown waits for every request in flight, so the release
	// comes first or the two wait for each other.
	for _, closer := range closers {
		closer()
	}
	var errs []error
	for _, role := range RoleOrder {
		if srv := servers[role]; srv != nil {
			if err := srv.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", role, err))
				_ = srv.Close()
			}
		}
	}
	s.wg.Wait()
	return errors.Join(errs...)
}

// logged writes one line per request: the role, the method, the path and
// the status the handler answered.
func (s *Stubs) logged(role Role, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		s.logf("%s %s %s %d", role, r.Method, r.URL.Path, rec.status())
	})
}

func (s *Stubs) logf(format string, args ...any) {
	if s.log == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(s.log, format+"\n", args...)
}

// recorder remembers the status a handler wrote, which is the one part of
// an answer the log line carries.
type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// status is what was written, or the 200 a handler that wrote nothing
// leaves behind.
func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}
