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
	"sync"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/admission"
	"latere.ai/x/cella/internal/api"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/egressd"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/metrics"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	"latere.ai/x/cella/internal/store/postgres"
	"latere.ai/x/cella/internal/version"
	"latere.ai/x/cella/manifest"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/k8s"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/podman"
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
// or runtime failure, 2 on a usage error. The worker role of spec 021 is
// the one row of spec 002's table still unbuilt.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	case "egress":
		return egressRole(ctx, rest, getenv, stdout, stderr)
	case "check":
		return checkRole(ctx, rest, getenv, stdout, stderr)
	case "version":
		return versionRole(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "cellad: unknown subcommand %q; serve, egress, check and version are the subcommands\n", name)
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

// egressRole is the gateway beside the sandboxes: two doors over the
// boundaries the control plane pushes it, and one stream it opens outbound to
// receive them (spec 018). It reads none of the control plane's variables and
// holds no store, no issuer and no authorizer.
func egressRole(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cellad egress", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}
	cfg, err := config.LoadEgress(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	// The gateway opens its two doors and one outbound stream and no
	// listener of its own, so it serves no scrape surface: its spans, its
	// logs and its process telemetry leave over OTLP, and its connection
	// counts are the control plane's (spec 017).
	tel := startTelemetry(ctx, "cellad-egress", stderr)
	defer tel.shutdown()
	gateway, err := egressd.New(ctx, egressd.Options{
		URL: cfg.URL, Key: cfg.EnvironmentKey,
		ProxyAddr: cfg.ProxyAddr, ReverseAddr: cfg.ReverseAddr,
		CAPEM: cfg.CAKeyPEM, UpstreamCAPEM: cfg.CABundlePEM,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("egress: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "cellad: %s egress proxy=%s reverse=%s environment=%s control-plane=%s telemetry=%s\n",
		version.Version, gateway.ProxyAddr(), gateway.ReverseAddr(), gateway.Environment(), cfg.URL, tel.mode)
	if err = gateway.Run(ctx); err != nil {
		return fail(stderr, err)
	}
	return 0
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
	// Telemetry comes up before anything else is built: a collaborator made
	// earlier captures the slog default as it stands, and design 017's
	// redaction is applied by replacing that default (spec 017).
	tel := startTelemetry(ctx, "cellad", stderr)
	defer tel.shutdown()
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fail(stderr, fmt.Errorf("CELLA_DATA_DIR: %w", err))
	}

	// Recovery may change runtime records. Own the state before opening the
	// driver so a second process cannot mutate live workloads.
	desired, lease, storeReady, journal, err := openStore(ctx, cfg)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = desired.Close() }()
	// Design 009's journal. Where desired state is design 010's store, the
	// record of a mutation commits inside that mutation's own transaction
	// and this is the same store read back. Where it is the local snapshot,
	// which keeps no journal, records live in one opened for them alone and
	// the controller hands each act to the emitter instead.
	var controllerEvents controller.Events
	if journal == nil {
		journal, err = memory.Open(memory.Options{})
		if err != nil {
			return fail(stderr, fmt.Errorf("event journal: %w", err))
		}
		defer func() { _ = journal.Close() }()
	}
	delivery := store.Journaled
	if cfg.Events.Enabled() {
		delivery = store.Delivered
	}
	emitter := events.NewEmitter(store.EventJournal(journal, delivery), nil)
	// The revocation list of spec 010: what the verifier asks about a token
	// cellad minted, and what the controller writes when it replaces or ends
	// one. It is the same store the journal is in, so a deployment with a
	// database keeps its revocations across a restart and one without keeps
	// them as long as the tokens it minted live, which is the process.
	revocations := store.NewRevocations(journal)

	// The one registry of design 017. The pull gauges read an index that is
	// already current: the controller's map of desired sandboxes, the hub's
	// connection count, and the journal's backlog. Neither the controller
	// nor the hub exists yet, so each closure reads a variable this function
	// assigns below and the goroutines that serve a scrape are started after
	// both, which is what orders the write before the read.
	var control *controller.Controller
	registry := metrics.New(metrics.Options{
		Environment: cfg.DefaultEnvironment,
		Driver:      cfg.Runtime,
		Sandboxes: func() map[string]int {
			if control == nil {
				return nil
			}
			phases := make([]string, 0, 16)
			for _, obj := range control.List() {
				phases = append(phases, obj.Status.Phase)
			}
			return phaseCounts(phases)
		},
		Pending: func() (int, bool) {
			// A scrape is bounded like the probes of design 002 and
			// outlives the drain: a store that has stopped answering must
			// not hold a scrape open, and a shutdown must not turn every
			// remaining scrape into a warning.
			read, cancel := context.WithTimeout(context.WithoutCancel(ctx), scrapeTimeout)
			defer cancel()
			n, err := store.EventJournal(journal, delivery).Undelivered(read)
			if err != nil {
				tel.log.WarnContext(read, "the journal's backlog could not be read", "err", err)
				return 0, false
			}
			return n, true
		},
	})
	// The store measures itself where it is design 010's, which is the one
	// that carries a transaction worth timing.
	if bound, ok := desired.(*store.Controlled); ok {
		bound.Measure(registry)
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
		Revocations:        revocations,
	})
	if err != nil {
		return fail(stderr, err)
	}
	identity.Authorizer.Measure(registry)
	// Every sandbox carries the identity spec 006 gives it: minted here at
	// create, projected by the driver, re-minted before it expires and
	// revoked with the sandbox.
	tokens, err := auth.NewWorkloadTokens(identity.Signer, revocations)
	if err != nil {
		return fail(stderr, err)
	}
	// A driver that owns local processes or an engine session is closed at
	// shutdown; one that drives a cluster owns nothing this process has to
	// release.
	runtimeDriver, closeRuntime, err := openRuntime(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	if err := runtimeDriver.Preflight(ctx); err != nil {
		return fail(stderr, fmt.Errorf("runtime preflight: %w", err))
	}
	// The gateways of the environment connect to this hub, and the
	// controller pushes each sandbox's boundary through it before the
	// driver is called (spec 018).
	hub := api.NewEgressHub(api.EgressHubOptions{
		Environment: cfg.DefaultEnvironment,
		AckTimeout:  cfg.Gateway.AckTimeout,
		RecordsCap:  cfg.Gateway.RecordsCap,
		Metrics:     registry,
	})
	registry.WithGateways(hub.Connected)
	if _, transactional := desired.(*store.Controlled); !transactional {
		controllerEvents = emitter
	}
	control, err = controller.Open(controller.Options{
		Store: desired, Driver: runtimeDriver, Environment: cfg.DefaultEnvironment,
		Lease: lease, ReapInterval: cfg.ReapInterval, TouchInterval: cfg.TouchInterval,
		LostGrace: cfg.LostGrace, Events: controllerEvents, Tokens: tokens,
		Egress: hub, Gateway: controller.GatewayAddresses{Proxy: cfg.Gateway.ProxyAddr, Reverse: cfg.Gateway.ReverseAddr},
		Pool: cfg.Scheduling.Pool, PoolInFlight: cfg.Scheduling.PoolInFlight, PoolGrace: cfg.Scheduling.PoolGrace,
		Metrics: registry,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("controller: %w", err))
	}
	// A gateway that connects to a control plane that has just restarted
	// receives every live sandbox's map, with the credential that sandbox
	// already holds, rather than an empty world.
	hub.Seed(control.EgressMaps(ctx))
	// The reaper is this process's clock: one tick applies the lifecycle
	// rules to the environment cellad drives (spec 005). One cellad is the
	// only writer of its environment, so it holds its own lease. The refill
	// loop ticks beside it under a lease of its own, keeping the
	// environment's pool at the size an operator asked for (spec 020).
	loopCtx, cancelLoops := context.WithCancel(ctx)
	loopsDone := make(chan struct{})
	go func() {
		defer close(loopsDone)
		var loops sync.WaitGroup
		loops.Go(func() { control.RunReaper(loopCtx) })
		loops.Go(func() { control.RunPool(loopCtx) })
		loops.Wait()
	}()
	stopLoops := sync.OnceFunc(func() { cancelLoops(); <-loopsDone })
	defer stopLoops()
	// Stage 3 of every resolve: the operator's endpoint where one is
	// configured, and the identity step where none is (spec 007).
	admit, err := admissionStep(cfg, registry)
	if err != nil {
		return fail(stderr, err)
	}
	handler, err := api.New(api.Options{
		Controller: control, Verifier: identity.Verifier, Authorizer: identity.Authorizer,
		MaxBodyBytes: cfg.MaxBodyBytes, MaxUploadBytes: cfg.MaxUploadBytes,
		Egress: hub, Events: emitter,
		Admit: admit, Defaults: manifest.Defaults{Image: cfg.Admission.DefaultImage},
		Metrics: registry,
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("API: %w", err))
	}
	// Delivery runs on the replica holding the journal lease, so a replica
	// set posts each record once (spec 009).
	stopDelivery := func() {}
	if cfg.Events.Enabled() {
		deliverer, err := events.NewDeliverer(events.DelivererOptions{
			Journal: store.EventJournal(journal, store.Delivered), Lease: lease,
			URL: cfg.Events.URL, Secrets: cfg.Events.Secrets,
			Timeout: cfg.Events.Timeout, RetryWindow: cfg.Events.RetryWindow,
			Metrics: registry,
		})
		if err != nil {
			return fail(stderr, err)
		}
		deliveryCtx, cancelDelivery := context.WithCancel(ctx)
		deliveryDone := make(chan struct{})
		go func() { defer close(deliveryDone); deliverer.Run(deliveryCtx) }()
		stopDelivery = sync.OnceFunc(func() { cancelDelivery(); <-deliveryDone })
		defer stopDelivery()
	}

	draining := make(chan struct{})
	checks := []health.Check{
		{Name: "draining", Run: notDraining(draining)},
		{Name: "disk", Run: diskWritable(cfg.DataDir)},
		{Name: "runtime", Run: runtimeDriver.Ready},
	}
	// A control plane whose store does not answer serves nothing, so the
	// store is a readiness check wherever there is one to ask (spec 010).
	if storeReady != nil {
		checks = append(checks, health.Check{Name: "store", Run: storeReady})
	}
	probes := health.Handler(health.Options{
		Ready:     health.Checks(checks...),
		Timeout:   2 * time.Second,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
	})

	public := http.NewServeMux()
	// The server span of design 017 starts here and nowhere else: the
	// probes, the key set and the version line draw none, and an inbound
	// traceparent joins the caller's trace at this one seam. The route
	// wrapper behind the mux renames the span once the pattern is known.
	public.Handle("/v1/", otel.Handler(handler, "cellad"))
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

	// Design 002's internal listener answers the probes and, from design
	// 017, the scrape surface. It is the internal address alone: the numbers
	// are the installation's and not a caller's.
	internal := http.NewServeMux()
	internal.Handle("GET /metrics", registry)
	internal.Handle("/", probes)

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
	_, _ = fmt.Fprintf(stdout, "cellad: %s listening public=%s internal=%s runtime=%s issuers=%d authorizer=%s admission=%s %s telemetry=%s\n",
		version.Version, publicLn.Addr(), internalLn.Addr(), cfg.Runtime, len(cfg.OIDCIssuers), identity.Mode,
		cfg.Admission.Mode(), recovery(cfg, control), tel.mode)

	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second},
		{Handler: internal, ReadHeaderTimeout: 10 * time.Second},
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
	// The loops drive the runtime, so they end before it does, and delivery
	// ends with them: what it had not posted stays on the journal.
	stopLoops()
	stopDelivery()
	if err := closeRuntime(); err != nil {
		return fail(stderr, fmt.Errorf("runtime shutdown: %w", err))
	}
	return 0
}

// openRuntime opens the backend CELLA_RUNTIME selects and returns it with
// the function that releases it: a driver that owns local processes or an
// engine session is closed at shutdown, and one that drives a cluster owns
// nothing this process has to release. The serve role and the check role
// open the same way, so the check drives the driver the node would.
func openRuntime(cfg config.Config) (runtime.Driver, func() error, error) {
	noop := func() error { return nil }
	switch cfg.Runtime {
	case config.RuntimeK8s:
		driver, err := k8s.New(cfg.K8s)
		if err != nil {
			return nil, noop, fmt.Errorf("runtime: %w", err)
		}
		return driver, noop, nil
	case config.RuntimePodman:
		driver, err := podman.New(podman.Options{Socket: cfg.PodmanSocket})
		if err != nil {
			return nil, noop, fmt.Errorf("runtime: %w", err)
		}
		return driver, driver.Close, nil
	default:
		driver, err := native.New(filepath.Join(cfg.DataDir, "native"))
		if err != nil {
			return nil, noop, fmt.Errorf("runtime: %w", err)
		}
		return driver, driver.Close, nil
	}
}

// admissionStep is spec 007's stage 3. With CELLA_ADMISSION_URL unset it is
// nil, which the resolver reads as the identity: the core carries no policy
// of its own. With it set, every apply is one call to that endpoint, which
// fails closed and is never retried.
func admissionStep(cfg config.Config, registry *metrics.Registry) (manifest.AdmitFunc, error) {
	if !cfg.Admission.Enabled() {
		return nil, nil
	}
	client, err := admission.New(admission.Options{
		URL: cfg.Admission.URL, Token: cfg.Admission.Token, Timeout: cfg.Admission.Timeout,
		// Design 007's own seam is design 017's admission row: one call, its
		// result and how long it took.
		Observe: func(result string, seconds float64) {
			registry.Decision(metrics.EndpointAdmission, admissionOutcome(result),
				time.Duration(seconds*float64(time.Second)))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}
	return client.Admit, nil
}

// admissionOutcome maps design 007's three results onto design 017's decision
// vocabulary: a manifest came back, a policy refused, or the endpoint gave no
// decision at all.
func admissionOutcome(result string) string {
	switch result {
	case admission.ResultAllow:
		return metrics.OutcomeAllow
	case admission.ResultRefused:
		return metrics.OutcomeDeny
	default:
		return metrics.OutcomeUnavailable
	}
}

// openStore opens desired state: the Postgres of spec 010 where CELLA_DB_URL
// names one, and the single-process snapshot under CELLA_DATA_DIR otherwise.
// It returns the store, the lease the reaper runs under, and the readiness
// check to mount, which is nil where the store is local.
func openStore(ctx context.Context, cfg config.Config) (controller.Store, controller.Lease, func(context.Context) error, store.Store, error) {
	delivery := store.Journaled
	if cfg.Events.Enabled() {
		delivery = store.Delivered
	}
	if cfg.DBURL == "" {
		// The snapshot store seals a secret value under the same envelope
		// the durable store uses, and holds none where the operator set no
		// key (spec 010).
		var sealer controller.Sealer
		if len(cfg.SecretKey) > 0 {
			envelope, err := store.NewEnvelope(cfg.SecretKey)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("controller store: %w", err)
			}
			sealer = envelope
		}
		local, err := controller.OpenSealedFileStore(filepath.Join(cfg.DataDir, "controller"), sealer)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("controller store: %w", err)
		}
		return local, controller.LocalLease{}, nil, nil, nil
	}
	durable, err := postgres.Open(ctx, postgres.Options{URL: cfg.DBURL, PoolURL: cfg.DBPoolURL, MaxConns: cfg.DBMaxConns, Key: cfg.SecretKey})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	bound := store.ForController(durable, cfg.DefaultEnvironment, delivery)
	return bound, bound, bound.Ready, durable, nil
}

// recovery is what the start-up line says about state: which store is in use,
// and what a sandbox the data plane lost gets (spec 001, State).
func recovery(cfg config.Config, control *controller.Controller) string {
	kind := "file"
	if cfg.DBURL != "" {
		kind = "postgres"
	}
	if control.Recovers() {
		return "store=" + kind + " recovery=on"
	}
	return fmt.Sprintf("store=%s recovery=off lost-grace=%s", kind, cfg.LostGrace)
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
