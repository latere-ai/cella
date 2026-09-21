// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command plane is a platform built on Cella's packages rather than on
// cellad: it imports the manifest contract, the runtime driver and the
// controller, and puts its own API, its own identity and its own policy
// around them. It is the second door of the plane guide as code, kept
// short enough to read in one sitting and complete enough to run.
//
// What a platform supplies is marked in one place each: authenticate,
// which turns a request into a subject, catalogue, which is the admission
// step where plans and images are decided, and ceilings, which is what a
// plan may not exceed. Everything else is the core's.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/native"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config{
		Addr:  value("PLANE_ADDR", "127.0.0.1:8080"),
		Root:  value("PLANE_DATA_DIR", "./plane-data"),
		Image: value("PLANE_IMAGE", "registry.example/base:1"),
	}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "plane:", err)
		os.Exit(1)
	}
}

// config is what this plane reads from its environment.
type config struct {
	Addr  string // where the platform's own API listens
	Root  string // where the driver and the controller keep their state
	Image string // the image a manifest that names none is given
}

func value(key, fallback string) string {
	if set := os.Getenv(key); set != "" {
		return set
	}
	return fallback
}

// run composes the packages into one server and serves until ctx ends.
func run(ctx context.Context, cfg config, out io.Writer) error {
	driver, err := native.New(cfg.Root)
	if err != nil {
		return fmt.Errorf("opening the runtime: %w", err)
	}
	core, err := controller.Open(controller.Options{
		DataDir:     cfg.Root,
		Driver:      driver,
		Environment: "default",
		Log:         slog.New(slog.NewTextHandler(out, nil)),
	})
	if err != nil {
		return fmt.Errorf("opening the controller: %w", err)
	}
	defer func() { _ = core.Close() }()
	go core.RunReaper(ctx)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           (&plane{core: core, cfg: cfg}).routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintln(out, "plane listening on", cfg.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// plane is the platform's own API over the controller.
type plane struct {
	core *controller.Controller
	cfg  config
}

func (p *plane) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", p.create)
	mux.HandleFunc("GET /sandboxes/{id}", p.read)
	mux.HandleFunc("DELETE /sandboxes/{id}", p.delete)
	return mux
}

// authenticate is the platform's half of identity: its own sessions, its
// own issuer, its own anything. The core never sees a token; it is handed
// a subject, and every object it keeps is owned by one.
func (p *plane) authenticate(r *http.Request) (string, bool) {
	subject, _, ok := r.BasicAuth()
	return subject, ok && subject != ""
}

// create resolves a manifest under this platform's options and hands the
// resolved object to the controller. Resolve is the whole contract: the
// schema, the defaults, the admission step, the ceilings and the boundary
// check run inside it, in that order.
func (p *plane) create(w http.ResponseWriter, r *http.Request) {
	subject, ok := p.authenticate(r)
	if !ok {
		http.Error(w, "who are you", http.StatusUnauthorized)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	document, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	object, err := manifest.Decode(document, r.Header.Get("Content-Type"))
	if err != nil {
		refuse(w, err)
		return
	}
	resolved, err := manifest.Resolve(r.Context(), &object, p.options(subject))
	if err != nil {
		refuse(w, err)
		return
	}
	// The count ceiling is the platform's: the core enforces the number it
	// is handed and decides nothing about plans.
	created, err := p.core.Create(r.Context(), resolved.Sandbox, subject, ceilings(subject).Sandboxes)
	if err != nil {
		refuse(w, err)
		return
	}
	write(w, http.StatusCreated, created)
}

func (p *plane) read(w http.ResponseWriter, r *http.Request) {
	subject, ok := p.authenticate(r)
	if !ok {
		http.Error(w, "who are you", http.StatusUnauthorized)
		return
	}
	object, err := p.core.Get(r.Context(), r.PathValue("id"), subject)
	if err != nil {
		refuse(w, err)
		return
	}
	write(w, http.StatusOK, object)
}

func (p *plane) delete(w http.ResponseWriter, r *http.Request) {
	subject, ok := p.authenticate(r)
	if !ok {
		http.Error(w, "who are you", http.StatusUnauthorized)
		return
	}
	if _, err := p.core.Get(r.Context(), r.PathValue("id"), subject); err != nil {
		refuse(w, err)
		return
	}
	object, err := p.core.Act(r.Context(), r.PathValue("id"), "delete")
	if err != nil {
		refuse(w, err)
		return
	}
	write(w, http.StatusAccepted, object)
}

// options are what this platform resolves every manifest under: who is
// applying, what an absent field takes, what a plan may not exceed, and
// the admission step that is this platform's catalogue.
func (p *plane) options(subject string) manifest.Options {
	plan := ceilings(subject)
	return manifest.Options{
		Actor:  manifest.Actor{Subject: subject, Sub: subject},
		Lookup: manifest.FixedEnvironment(manifest.NativeEnvironment("default")),
		Defaults: manifest.Defaults{
			CPU: "1", Memory: "2Gi", Disk: "10Gi",
			AutoStop: "15m", TTL: "8h", AutoDelete: "24h",
			Image: p.cfg.Image,
		},
		Ceilings: manifest.Ceilings{CPU: plan.CPU, Memory: plan.Memory, TTL: plan.TTL},
		Admit:    p.catalogue,
	}
}

// plan is what this platform sells. A real one reads it from its own
// database under the subject's account.
type plan struct {
	Sandboxes int
	CPU       v1.Quantity
	Memory    v1.Quantity
	TTL       v1.Duration
}

func ceilings(subject string) plan {
	if strings.HasSuffix(subject, "@example.com") {
		return plan{Sandboxes: 100, CPU: "8", Memory: "32Gi", TTL: "168h"}
	}
	return plan{Sandboxes: 3, CPU: "2", Memory: "4Gi", TTL: "8h"}
}

// catalogue is this platform's admission step: the image catalogue, the
// labels it stamps, and the translation of its own vocabulary into the
// core's kinds. It runs inside Resolve, and what it returns is validated
// again, so a step that writes a field the schema does not have is a
// refusal and not a surprise later.
func (p *plane) catalogue(_ context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
	// A step that cannot decide says so with the code. A plain error is a
	// refusal, which is the safe default and the wrong answer when the
	// catalogue itself is the thing that is down.
	if p.cfg.Image == "" {
		return nil, nil, &manifest.Error{
			Code:   manifest.CodeAdmissionUnavailable,
			Detail: "this plane has no image catalogue configured",
		}
	}
	out := *in
	if out.Spec.Image == "" || out.Spec.Image == "default" {
		out.Spec.Image = p.cfg.Image
	}
	if !strings.HasPrefix(out.Spec.Image, "registry.example/") {
		return nil, nil, errors.New("the image is not in this platform's catalogue")
	}
	if out.Metadata.Labels == nil {
		out.Metadata.Labels = map[string]string{}
	}
	out.Metadata.Labels["plane.example.com/account"] = strings.ReplaceAll(req.Actor.Sub, "@", "-at-")
	return &out, nil, nil
}

// refuse turns a core error into this platform's own answer. The manifest
// error carries a code and a path, which is what an API of your own shape
// renders; the controller's sentinels are the rest.
func refuse(w http.ResponseWriter, err error) {
	var known *manifest.Error
	switch {
	case errors.As(err, &known):
		write(w, statusFor(known.Code), map[string]any{
			"error": known.Code, "path": known.Path, "detail": known.Detail,
		})
	case errors.Is(err, controller.ErrNotFound):
		http.Error(w, "no such sandbox", http.StatusNotFound)
	case errors.Is(err, controller.ErrNameTaken):
		http.Error(w, "that name is taken", http.StatusConflict)
	case errors.Is(err, controller.ErrQuota):
		http.Error(w, "your plan is full", http.StatusForbidden)
	case errors.Is(err, controller.ErrPhase):
		http.Error(w, "the sandbox is not in a phase that allows this", http.StatusConflict)
	default:
		http.Error(w, "the plane could not serve this", http.StatusInternalServerError)
	}
}

func statusFor(code string) int {
	switch code {
	case "not_found":
		return http.StatusNotFound
	case "ceiling_exceeded", "admission_refused":
		return http.StatusForbidden
	case "admission_unavailable", "authorizer_unavailable":
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
