// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package check is the check role of cellad: it reads the whole
// configuration and answers, one line per requirement, whether this
// installation meets what spec 014 requires of it before the first manifest
// is applied.
//
// Nothing here re-implements a requirement. A line is the start-up path of
// cellad serve asked one question at a time, so an installation that passes
// check is one that would have started, and a rule that moves moves in one
// place.
package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/runtime"
)

// State is how one requirement answered.
type State string

const (
	// Ok is a requirement this installation meets.
	Ok State = "ok"
	// Skipped is an optional dependency this installation does not
	// configure, or a requirement that cannot be asked because an earlier
	// one failed. It is never a failure.
	Skipped State = "skip"
	// Failed is a requirement this installation does not meet. One is
	// enough to exit 1.
	Failed State = "FAIL"
)

// Line is one requirement's answer: the name an operator reads, the state,
// and the sentence that says what to fix or why the line does not apply.
type Line struct {
	Name   string
	State  State
	Detail string
}

// Budget bounds one line. A check that hangs on an endpoint tells an
// operator less than one that says the endpoint did not answer in time.
const Budget = 10 * time.Second

// Options is what Run needs from the process around it. Every seam has a
// default, so cmd/cellad passes the environment and the driver opener and
// nothing else.
type Options struct {
	// Getenv reads the configuration. Required.
	Getenv config.Getenv
	// Open returns the backend the configuration selects and the function
	// that releases it. It is the opener cellad serve uses, so the check
	// drives the same driver the node would. Required.
	Open func(config.Config) (runtime.Driver, func() error, error)
	// HTTP sends the discovery reads, the authorizer's probe and the sink's.
	// An instrumented client is built when none is given.
	HTTP *http.Client
	// Now is the clock the sink's signature is stamped with.
	Now func() time.Time
}

// Run asks every requirement and returns one line each, in the order an
// operator reads them: the mandatory rows first, then the optional
// dependencies in the order spec 002's table lists them.
//
// A configuration that does not load ends the run: nothing else is knowable
// about an installation whose variables could not be read.
func Run(ctx context.Context, o Options) []Line {
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: Budget, Transport: otel.Transport(nil)}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	cfg, err := config.Load(o.Getenv)
	if err != nil {
		return []Line{{Name: "configuration", State: Failed, Detail: err.Error()}}
	}
	lines := []Line{{Name: "configuration", State: Ok,
		Detail: fmt.Sprintf("%d issuer(s), runtime %s, data directory %s", len(cfg.OIDCIssuers), cfg.Runtime, cfg.DataDir)}}

	identity, idLine := identity(ctx, cfg, client)
	lines = append(lines, idLine)
	lines = append(lines, authorizer(ctx, identity))
	lines = append(lines, backend(ctx, cfg, o.Open))
	lines = append(lines, dataDir(cfg))
	lines = append(lines, admission(o.Getenv))
	lines = append(lines, sink(ctx, cfg, client, now))
	lines = append(lines, store(ctx, cfg))
	lines = append(lines, gateway(ctx, cfg))
	return lines
}

// Failed reports whether any line failed, which is the process's exit code.
func (l Line) Failed() bool { return l.State == Failed }

// Report writes one line per requirement and reports whether any failed.
// The name column is padded so two installations' output lines up.
func Report(w io.Writer, lines []Line) bool {
	width := 0
	for _, l := range lines {
		width = max(width, len(l.Name))
	}
	failed := false
	for _, l := range lines {
		failed = failed || l.Failed()
		_, _ = fmt.Fprintf(w, "%-*s  %-4s  %s\n", width, l.Name, l.State, l.Detail)
	}
	return failed
}

// identity starts the verifier, the signer and the authorizer exactly as
// the node does, so the line answers for the issuer discovery, the key set,
// CELLA_TOKEN_KEY and the authorizer's bearer at once. Every one of those is
// a start-up failure of spec 006, and the message names the variable.
func identity(ctx context.Context, cfg config.Config, client *http.Client) (*auth.Identity, Line) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	id, err := auth.Start(ctx, auth.Options{
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
		HTTP:               client,
	})
	if err != nil {
		return nil, Line{Name: "identity", State: Failed, Detail: err.Error()}
	}
	return id, Line{Name: "identity", State: Ok,
		Detail: fmt.Sprintf("every issuer answered, the signing key set holds %d key(s), audience %s",
			len(cfg.TokenKeys), cfg.OIDCAudience)}
}

// authorizer sends the reserved probe id of spec 006 and reads the answer.
// A deny is the one right answer: an endpoint that allows the probe is one
// that does not read the request, and an installation would run with an
// authorization decision that means nothing. The built-in owner policy
// denies it too, so the line holds in every configuration.
func authorizer(ctx context.Context, id *auth.Identity) Line {
	if id == nil {
		return Line{Name: "authorizer", State: Skipped, Detail: "the identity did not start"}
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	if err := id.Authorizer.Check(ctx); err != nil {
		return Line{Name: "authorizer", State: Failed, Detail: err.Error()}
	}
	return Line{Name: "authorizer", State: Ok,
		Detail: fmt.Sprintf("%s denies the probe id %s", id.Mode, auth.ProbeID)}
}

// backend opens the driver the configuration selects and runs its
// Preflight, which is the same call the node makes before it binds a port.
// On Kubernetes that is one access review per entry of the driver's verb
// table, which is the list a Role is written from, plus the storage class;
// on podman it is a ping of each candidate socket; on the native backend it
// is the state directory. None of them writes anything.
func backend(ctx context.Context, cfg config.Config, open func(config.Config) (runtime.Driver, func() error, error)) Line {
	if open == nil {
		return Line{Name: "backend", State: Skipped, Detail: "no backend opener was given"}
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	driver, closeDriver, err := open(cfg)
	if err != nil {
		return Line{Name: "backend", State: Failed, Detail: err.Error()}
	}
	defer func() { _ = closeDriver() }()
	if err := driver.Preflight(ctx); err != nil {
		return Line{Name: "backend", State: Failed, Detail: err.Error()}
	}
	return Line{Name: "backend", State: Ok,
		Detail: fmt.Sprintf("%s answers, isolation %s", driver.Name(), driver.Isolation())}
}

// dataDir creates the directory the node creates at start and writes the
// file the readiness probe writes. An installation that fails this line
// serves 503 from its first request.
func dataDir(cfg config.Config) Line {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return Line{Name: "data directory", State: Failed, Detail: "CELLA_DATA_DIR: " + err.Error()}
	}
	f, err := os.CreateTemp(cfg.DataDir, ".check-*")
	if err != nil {
		return Line{Name: "data directory", State: Failed, Detail: "CELLA_DATA_DIR: " + err.Error()}
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(filepath.Clean(name)); err != nil {
		return Line{Name: "data directory", State: Failed, Detail: "CELLA_DATA_DIR: " + err.Error()}
	}
	return Line{Name: "data directory", State: Ok, Detail: cfg.DataDir + " is writable"}
}
