// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command cellad is the Cella server: it takes a Sandbox manifest and makes
// the environment it describes exist on a runtime backend. This file is
// the entry point and holds wiring only: configuration, the listeners, and
// the run group. The behaviour lives in the packages under internal/ and in
// the exported packages at the module root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/api"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/version"
	"latere.ai/x/cella/runtime/native"
)

// Shutdown timing of spec 002: readiness answers 503 at once, the drain
// delay lets a load balancer notice, then the servers close with the
// grace period.
const (
	drainDelay  = 3 * time.Second
	gracePeriod = 60 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run dispatches the subcommand and returns the process exit code, so
// tests drive it without a subprocess: 0 on a clean stop, 1 on a start-up
// or runtime failure, 2 on a usage error. serve is the only subcommand
// today; spec 002 keeps the table.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "cellad: unknown subcommand %q; serve is the default and the only one\n", name)
		return 2
	}
}

// subcommand is spec 002's rule: the first argument that does not start
// with a dash names the subcommand, and the arguments around it are the
// subcommand's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	return "", args
}

// serve is the node: the two listeners and the probes of spec 002. The
// runtime, the API, and the controllers of later specs mount here.
func serve(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cellad serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fail(stderr, fmt.Errorf("CELLA_DATA_DIR: %w", err))
	}

	// Identity comes up before the listeners, so a deployment whose
	// issuer or signing key is wrong fails to start rather than binding
	// a port and refusing every request (spec 006).
	identity, err := auth.Start(ctx, auth.Options{
		Issuers:            cfg.OIDCIssuers,
		Audience:           cfg.OIDCAudience,
		Audiences:          cfg.OIDCAudiences,
		PublicURL:          cfg.PublicURL,
		TokenKeys:          cfg.TokenKeys,
		AuthorizerURL:      cfg.AuthorizerURL,
		AuthorizerToken:    cfg.AuthorizerToken,
		AuthorizerTimeout:  cfg.AuthorizerTimeout,
		AdminSubjects:      cfg.AdminSubjects,
		DefaultEnvironment: cfg.DefaultEnvironment,
	})
	if err != nil {
		return fail(stderr, err)
	}

	if cfg.Runtime != config.RuntimeNative {
		return fail(stderr, fmt.Errorf("CELLA_RUNTIME=%s is not implemented; native is available for trusted development with CELLA_ALLOW_UNSAFE_NATIVE=true", cfg.Runtime))
	}
	runtimeDriver, err := native.New(filepath.Join(cfg.DataDir, "native"))
	if err != nil {
		return fail(stderr, fmt.Errorf("runtime: %w", err))
	}
	defer func() { _ = runtimeDriver.Close() }()
	if err := runtimeDriver.Preflight(ctx); err != nil {
		return fail(stderr, fmt.Errorf("runtime preflight: %w", err))
	}
	control, err := controller.Open(controller.Options{DataDir: filepath.Join(cfg.DataDir, "controller"), Driver: runtimeDriver, Environment: cfg.DefaultEnvironment})
	if err != nil {
		return fail(stderr, fmt.Errorf("controller: %w", err))
	}
	defer func() { _ = control.Close() }()
	handler, err := api.New(api.Options{Controller: control, Verifier: identity.Verifier, Authorizer: identity.Authorizer})
	if err != nil {
		return fail(stderr, fmt.Errorf("API: %w", err))
	}

	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready:     health.Checks(health.Check{Name: "draining", Run: notDraining(draining)}, health.Check{Name: "disk", Run: diskWritable(cfg.DataDir)}),
		Timeout:   2 * time.Second,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
	})

	public := http.NewServeMux()
	public.Handle("/v1/", handler)
	for _, p := range []string{"/livez", "/readyz", "/version"} {
		public.Handle("GET "+p, probes)
	}
	// The key set every workload token and environment key verifies
	// against, so a platform or a third service trusts a sandbox without
	// asking cellad (spec 006).
	public.Handle("GET "+auth.JWKSPath, identity.Signer.JWKS())
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, version.String())
	})

	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", cfg.PublicAddr)
	if err != nil {
		return fail(stderr, fmt.Errorf("CELLA_PUBLIC_ADDR: %w", err))
	}
	internalLn, err := lc.Listen(ctx, "tcp", cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		return fail(stderr, fmt.Errorf("CELLA_INTERNAL_ADDR: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "cellad: %s listening public=%s internal=%s runtime=%s issuers=%d authorizer=%s\n",
		version.Version, publicLn.Addr(), internalLn.Addr(), cfg.Runtime, len(cfg.OIDCIssuers), identity.Mode)

	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second},
		{Handler: probes, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for i, ln := range []net.Listener{publicLn, internalLn} {
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(servers[i], ln)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fail(stderr, err)
	}
	// The stop signal has fired, so the shutdown runs on a context that
	// keeps the request's values and outlives its cancellation.
	close(draining)
	stopping := context.WithoutCancel(ctx)
	sleepCtx(stopping, drainDelay)
	shutdownCtx, cancel := context.WithTimeout(stopping, gracePeriod)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return 0
}

// fail writes the one line an operator reads on a start-up or runtime
// failure and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "cellad: %v\n", err)
	return 1
}

// notDraining fails readiness once shutdown has begun, so a load balancer
// stops routing before the servers close.
func notDraining(draining <-chan struct{}) func(context.Context) error {
	return func(context.Context) error {
		select {
		case <-draining:
			return errors.New("shutting down")
		default:
			return nil
		}
	}
}

// diskWritable creates and removes a file under dir: the readiness check
// that the local disk cellad keeps state on is present and writable.
func diskWritable(dir string) func(context.Context) error {
	return func(context.Context) error {
		f, err := os.CreateTemp(dir, ".readyz-*")
		if err != nil {
			return err
		}
		name := f.Name()
		_ = f.Close()
		return os.Remove(filepath.Clean(name))
	}
}

// sleepCtx waits d or until ctx ends, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
