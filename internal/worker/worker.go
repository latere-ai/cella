// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package worker is the data plane role of spec 021: one process beside the
// sandboxes, running the driver its own configuration names, claiming the
// operations a control plane enqueues for its environment.
//
// It connects outbound and nothing ever dials it, which is invariant 10 of
// spec 001 and what lets an operator run a data plane behind a firewall
// against a control plane someone else operates. It reads none of the control
// plane's variables, holds no store, no issuer and no authorizer, and mints
// nothing: the workload token a sandbox carries arrives in the create it is
// handed.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/client"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
)

// The bounds of the reconnect. A worker that cannot reach the control plane
// keeps the sandboxes it already runs and tries again, because a sandbox is
// still running whatever the control plane can see.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// registerTimeout bounds one registration call, so a control plane that
// accepts a connection and answers nothing does not hold the worker.
const registerTimeout = 30 * time.Second

// Options builds one worker.
type Options struct {
	// URL is the control plane's public URL and Key the environment key
	// that authenticates every call and the stream.
	URL string
	Key string
	// Driver is what the operations are executed with. Its Preflight must
	// pass before the worker registers at all.
	Driver runtime.Driver
	// Capacity and Labels are what this worker declares about itself at
	// registration.
	Capacity remote.Registration
	// Version is the build identity this worker reports.
	Version string
	// HTTP is the client the registration is made with. Nil takes one with
	// the registration timeout.
	HTTP *http.Client
	Log  *slog.Logger
}

// Worker is one data plane process: the driver, the key, and the one stream
// it holds open to the control plane.
type Worker struct {
	options Options
	client  *http.Client
	log     *slog.Logger
	// environment is the id the key named, learned from the registration,
	// and worker the id the control plane minted for this process.
	environment string
	worker      string
}

// New builds the worker. It fails where the URL is not one a credential may
// travel over, which is the rule every role that carries a key applies.
func New(o Options) (*Worker, error) {
	switch {
	case o.URL == "":
		return nil, errors.New("worker: CELLA_URL is unset, and the worker connects outbound to it")
	case o.Key == "":
		return nil, errors.New("worker: CELLA_ENVIRONMENT_KEY is unset, and it is what authenticates the worker's stream")
	case o.Driver == nil:
		return nil, errors.New("worker: a worker runs a driver, and none was opened")
	}
	if _, err := url.Parse(o.URL); err != nil {
		return nil, fmt.Errorf("worker: CELLA_URL is not a URL: %w", err)
	}
	w := &Worker{options: o, client: o.HTTP, log: o.Log}
	if w.client == nil {
		w.client = &http.Client{Timeout: registerTimeout, Transport: otel.Transport(nil)}
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	return w, nil
}

// Environment is the environment this worker serves, empty until it has
// registered once.
func (w *Worker) Environment() string { return w.environment }

// ID is the worker id the control plane minted, empty until registration.
func (w *Worker) ID() string { return w.worker }

// Run holds one stream open until the context ends, registering again and
// reconnecting with backoff. A worker that restarts registers again and its
// old claims lapse, so nothing is held by an id nobody answers for.
//
// Preflight runs once, before anything: a worker whose own driver cannot work
// must not register, because the control plane would place sandboxes on it.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.options.Driver.Preflight(ctx); err != nil {
		return fmt.Errorf("worker: the driver this worker runs is not usable: %w", err)
	}
	backoff := minBackoff
	for {
		err := w.connect(ctx)
		if ctx.Err() != nil {
			// The worker was asked to stop. Whatever ended the connection is
			// that request, not a failure of the worker's own act.
			return nil //nolint:nilerr // the context ending is the caller stopping the role
		}
		if err != nil {
			w.log.WarnContext(ctx, "the worker's stream to the control plane ended", "err", err, "retryIn", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// connect registers and holds one stream. Registration comes first every
// time: the control plane mints a worker id per registration, so a worker
// that reconnects is a new claimer and the operations its predecessor held
// are redelivered rather than waiting on a connection that is gone.
func (w *Worker) connect(ctx context.Context) error {
	if err := w.register(ctx); err != nil {
		return err
	}
	conn, err := w.dial(ctx)
	if err != nil {
		return err
	}
	server := remote.NewServer(conn, remote.ServerOptions{
		Worker: w.worker, Driver: w.options.Driver, Log: w.log,
	})
	return server.Run(ctx)
}

// register declares this worker and its driver and takes the id it claims
// under. The key names the environment, so the route carries no environment
// of its own beyond what the control plane reads from the bearer.
func (w *Worker) register(ctx context.Context) error {
	registration := w.options.Capacity
	registration.Driver = w.options.Driver.Name()
	registration.Isolation = w.options.Driver.Isolation()
	registration.Capabilities = w.options.Driver.Capabilities()
	registration.Version = w.options.Version
	registration.Worker = ""

	body, err := json.Marshal(registration)
	if err != nil {
		return fmt.Errorf("worker: the registration did not encode: %w", err)
	}
	endpoint, err := url.Parse(strings.TrimRight(w.options.URL, "/"))
	if err != nil {
		return fmt.Errorf("worker: CELLA_URL is not a URL: %w", err)
	}
	endpoint.Path = client.Route(endpoint.Path, "/v1/environments/self/workers")
	endpoint.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.options.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker: registering: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("worker: registering: %w", err)
	}
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return fmt.Errorf("worker: the control plane refused the registration with %s: %s",
			res.Status, strings.TrimSpace(string(answer)))
	}
	var registered remote.Registered
	if err = json.Unmarshal(answer, &registered); err != nil {
		return fmt.Errorf("worker: the registration answer did not decode: %w", err)
	}
	if registered.Worker == "" {
		return errors.New("worker: the control plane accepted the registration and named no worker")
	}
	w.worker, w.environment = registered.Worker, registered.Environment
	w.log.InfoContext(ctx, "the worker registered",
		"environment", w.environment, "worker", w.worker, "driver", registration.Driver)
	return nil
}

// dial opens the one stream. The key is the bearer and the subprotocol is the
// version of the vocabulary, so a control plane of another release refuses
// the upgrade rather than half understanding what follows.
func (w *Worker) dial(ctx context.Context) (remote.FrameConn, error) {
	endpoint, err := StreamURL(w.options.URL, w.environment)
	if err != nil {
		return nil, err
	}
	dialer := &websocket.Dialer{
		Subprotocols:     []string{remote.Protocol},
		HandshakeTimeout: registerTimeout,
	}
	conn, res, err := dialer.DialContext(ctx, endpoint, http.Header{
		"Authorization": []string{"Bearer " + w.options.Key},
	})
	if res != nil {
		// A refused upgrade carries a body the caller owns; a successful one
		// carries an empty body, and both are closed here.
		defer func() { _ = res.Body.Close() }()
	}
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("worker: opening the stream: %w (%s)", err, res.Status)
		}
		return nil, fmt.Errorf("worker: opening the stream: %w", err)
	}
	if conn.Subprotocol() != remote.Protocol {
		_ = conn.Close()
		return nil, fmt.Errorf("worker: the control plane does not speak %s", remote.Protocol)
	}
	return NewSocket(conn), nil
}

// StreamURL turns the control plane's URL into the worker stream's, keeping
// the scheme's WebSocket equivalent and composing the route under the URL's
// path by client.Route, the base the control plane is served under. An empty
// environment is the one the key names.
func StreamURL(base, environment string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("worker: CELLA_URL is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	if environment == "" {
		environment = "self"
	}
	u.Path = client.Route(u.Path, "/v1/environments/"+environment+"/operations")
	u.RawPath = ""
	return u.String(), nil
}
