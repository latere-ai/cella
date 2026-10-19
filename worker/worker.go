// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package worker serves one environment from a machine the control plane
// does not own (specs 021 and 082). A worker runs a driver on that machine,
// connects outbound to the control plane with the environment's key, and
// executes the operations the control plane sends for the environment. It is
// what `cellad worker` runs, and a program that embeds it serves an
// environment in its own process, with its own key store, its own prompts
// and its own log.
//
// The worker connects outbound and nothing ever dials it, which is invariant
// 10 of spec 001 and what lets a data plane run behind a firewall against a
// control plane someone else operates. It holds no store, no issuer and no
// authorizer, and mints nothing: the workload token a sandbox carries
// arrives in the create it is handed.
//
// The host has the last word. Before a create runs, the worker compares it
// with what the driver reports of the host and refuses what the host does
// not grant, whatever the control plane resolved; then it asks the
// embedder's HostPolicy, which may hold the create while it asks a person.
// Every other operation is checked against a sandbox the driver holds and
// against the policy, and each one answered is handed to Options.Record.
//
// The embedder learns whether its machine is online from Options.State, which
// is told when the worker connects and each time it stops being connected.
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
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
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

// DefaultAdmitTimeout is how long a create may wait on the host's policy
// when Options.AdmitTimeout is zero.
const DefaultAdmitTimeout = 10 * time.Minute

// cleanupTimeout bounds stopping or deleting the driver's sandboxes when Run
// returns, which runs after the caller's context has already ended.
const cleanupTimeout = time.Minute

// Options builds one worker.
type Options struct {
	// URL is the control plane's public URL.
	URL string
	// Environment is the environment's id, or "self" for the one the key
	// names. Empty is "self".
	Environment string
	// Key returns the environment key. It is read once per connection, so
	// a key kept in an operating system's key store, or one rotated while
	// the worker runs, is read where it lives.
	Key func(context.Context) (string, error)
	// Driver is what the operations are executed with. Its Preflight must
	// pass before the worker registers at all. A driver that implements
	// runtime.HostDescriber reports the host in the registration.
	Driver runtime.Driver
	// Capacity and Labels are what this worker declares about itself at
	// registration, and Version the build identity it reports.
	Capacity v1.Capacity
	Labels   map[string]string
	Version  string
	// Policy is asked before every create and every other operation on a
	// sandbox. Nil admits everything the built-in checks admit.
	Policy HostPolicy
	// Record is told of every operation once it is answered. Nil records
	// nothing.
	Record func(Record)
	// State is told of every change of the worker's connection to the
	// control plane: connected once a registration is taken and its stream
	// is open, and not connected, with the reason, when a registration fails
	// or the stream ends. It runs on Run's own goroutine, in order, and Run
	// waits for it, so it should return promptly. Nil tells nothing.
	State func(State)
	// KeepOnExit leaves the driver's sandboxes running when ctx ends. An
	// operator's fleet may want its sandboxes to outlive a worker restart;
	// a person's computer does not, which is the default.
	KeepOnExit bool
	// AdmitTimeout is how long a create may be held while the host decides.
	// Zero is DefaultAdmitTimeout.
	AdmitTimeout time.Duration
	// HTTPClient makes the registration. Nil takes one with the
	// registration timeout and the tracing transport.
	HTTPClient *http.Client
	// Log takes the worker's own lines. Nil is the default logger.
	Log *slog.Logger
}

// Refusal is a fatal answer from the control plane, which ends Run rather
// than a reconnect: another attempt would meet the same answer. Code is one
// of remote.ReasonServeRefused, remote.ReasonKeyRevoked,
// remote.ReasonWorkerReplaced, remote.ReasonWorkerLimit and
// environment_mismatch; Detail is the control plane's developer detail.
type Refusal struct {
	Code   string
	Detail string
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return "worker: the control plane refused this worker: " + r.Code
	}
	return "worker: the control plane refused this worker: " + r.Code + ": " + r.Detail
}

// State is the worker's connection to the control plane at one change, as
// Options.State is told of it.
type State struct {
	// Connected says the control plane took the registration and the stream
	// is open, so operations reach this worker. Registered is the control
	// plane's answer to that registration.
	Connected  bool
	Registered remote.Registered
	// Err is why the worker is not connected: the registration that failed,
	// a *RegistrationError where the control plane answered it with an
	// error, or what ended the stream. When Run ends because its context
	// ended, Err is the context's cause. It is nil while Connected.
	Err error
	// Reconnect says the worker registers again after RetryIn. It is false
	// when Run is about to return.
	Reconnect bool
	RetryIn   time.Duration
	// Refusal is the control plane's answer the worker stops on, when that
	// is what ends Run. Err is then the same refusal.
	Refusal *Refusal
}

// RegistrationError is a registration the control plane answered with an
// error the worker does not stop on, such as a key it refused with 401 or a
// control plane that is not ready. The worker registers again after its
// backoff. Code is the error code the answer's body carried, empty where it
// carried none.
type RegistrationError struct {
	StatusCode int
	Status     string
	Code       string
	Body       string
}

func (e *RegistrationError) Error() string {
	return "worker: the control plane refused the registration with " + e.Status + ": " + e.Body
}

// errStreamClosed is a stream that ended with no error of its own, which is
// the control plane closing it.
var errStreamClosed = errors.New("worker: the control plane closed the stream")

// codeEnvironmentMismatch is the registration refusal of a worker whose
// driver differs from the one the environment already recorded.
const codeEnvironmentMismatch = "environment_mismatch"

// fatal is every code a worker stops on.
var fatal = []string{
	remote.ReasonServeRefused, remote.ReasonKeyRevoked, remote.ReasonWorkerReplaced,
	remote.ReasonWorkerLimit, codeEnvironmentMismatch,
}

// ends reports whether a refusal also ends the host's work: the key was
// revoked or the environment may no longer be served, so nothing the driver
// holds belongs to anybody any longer.
func (r *Refusal) ends() bool {
	return r.Code == remote.ReasonServeRefused || r.Code == remote.ReasonKeyRevoked
}

// Run serves the environment until ctx ends or the control plane answers
// with a Refusal, registering again and reconnecting with backoff in
// between. A worker that restarts registers again and its old claims lapse,
// so nothing is held by an id nobody answers for.
//
// Preflight runs once, before anything: a worker whose own driver cannot work
// must not register, because the control plane would place sandboxes on it.
//
// Before it returns, Run ends the host's work. When ctx ends it stops every
// sandbox the driver lists, unless KeepOnExit is set; on a revoked key or a
// refused serve it stops and deletes every one whatever KeepOnExit says; on
// a replaced or refused registration it touches nothing, because another
// process holding the key now serves the environment.
//
// Options.State is told when the worker connects and each time it stops
// being connected, the last time with Reconnect false just before Run ends
// the host's work and returns.
func Run(ctx context.Context, o Options) error {
	w, err := newWorker(o)
	if err != nil {
		return err
	}
	return w.run(ctx)
}

// worker is one data plane process: the driver, the key, and the one stream
// it holds open to the control plane.
type worker struct {
	options Options
	client  *http.Client
	log     *slog.Logger
	gate    *gate
	// environment is the id the key named, learned from the registration,
	// id the worker id the control plane minted for this process, and
	// registered the whole answer, which Options.State is told.
	environment string
	id          string
	registered  remote.Registered
	// after is the reconnect's timer, time.After unless a test replaces it
	// to read the waits the loop chose.
	after func(time.Duration) <-chan time.Time
}

// newWorker builds the worker. It fails where the URL is not one a
// credential may travel over, which is the rule every role that carries a
// key applies.
func newWorker(o Options) (*worker, error) {
	switch {
	case o.URL == "":
		return nil, errors.New("worker: no control plane URL was given, and the worker connects outbound to it")
	case o.Key == nil:
		return nil, errors.New("worker: no environment key was given, and it is what authenticates the worker's stream")
	case o.Driver == nil:
		return nil, errors.New("worker: a worker runs a driver, and none was given")
	}
	if _, err := url.Parse(o.URL); err != nil {
		return nil, fmt.Errorf("worker: the control plane URL is not a URL: %w", err)
	}
	if o.Environment == "" {
		o.Environment = "self"
	}
	if o.AdmitTimeout <= 0 {
		o.AdmitTimeout = DefaultAdmitTimeout
	}
	w := &worker{options: o, client: o.HTTPClient, log: o.Log, after: time.After}
	if w.client == nil {
		w.client = &http.Client{Timeout: registerTimeout, Transport: otel.Transport(nil)}
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	w.gate = newGate(o, w.log)
	return w, nil
}

func (w *worker) run(ctx context.Context) error {
	if err := w.options.Driver.Preflight(ctx); err != nil {
		return fmt.Errorf("worker: the driver this worker runs is not usable: %w", err)
	}
	backoff := minBackoff
	for {
		accepted, err := w.connect(ctx)
		if ctx.Err() != nil {
			// The worker was asked to stop. Whatever ended the connection is
			// that request, not a failure of the worker's own act.
			w.tell(State{Err: context.Cause(ctx)})
			w.release(ctx, !w.options.KeepOnExit, false)
			return nil //nolint:nilerr // the context ending is the caller stopping the worker
		}
		var refusal *Refusal
		if errors.As(err, &refusal) {
			w.log.WarnContext(ctx, "the control plane refused this worker", "code", refusal.Code, "detail", refusal.Detail)
			w.tell(State{Err: refusal, Refusal: refusal})
			if refusal.ends() {
				w.release(ctx, true, true)
			}
			return refusal
		}
		// A stream the control plane opened and then ended is a control
		// plane that went away, such as a writer handing off to another
		// replica, so the next dial waits the shortest wait rather than what
		// earlier refusals grew the wait to.
		if accepted {
			backoff = minBackoff
		}
		if err == nil {
			err = errStreamClosed
		}
		w.log.WarnContext(ctx, "the worker's stream to the control plane ended", "err", err, "retryIn", backoff)
		w.tell(State{Err: err, Reconnect: true, RetryIn: backoff})
		select {
		case <-ctx.Done():
			w.tell(State{Err: context.Cause(ctx)})
			w.release(ctx, !w.options.KeepOnExit, false)
			return nil
		case <-w.after(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// tell hands one change of the connection to Options.State.
func (w *worker) tell(s State) {
	if w.options.State != nil {
		w.options.State(s)
	}
}

// release ends what the driver holds: it stops every sandbox when stop is
// set and deletes every one when remove is. It runs after the caller's
// context ended, so it keeps that context's values and drops its
// cancellation, and a failure on one sandbox is logged and does not keep the
// others running.
func (w *worker) release(parent context.Context, stop, remove bool) {
	if !stop && !remove {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
	defer cancel()
	states, err := w.options.Driver.List(ctx, runtime.Filter{})
	if err != nil {
		w.log.WarnContext(ctx, "the worker could not list its sandboxes to end them", "err", err)
		return
	}
	for _, s := range states {
		if err := w.options.Driver.Stop(ctx, s.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) &&
			!errors.Is(err, runtime.ErrNotRunning) {
			w.log.WarnContext(ctx, "the worker could not stop a sandbox", "sandbox", s.ID, "err", err)
		}
		if !remove {
			continue
		}
		if err := w.options.Driver.Delete(ctx, s.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			w.log.WarnContext(ctx, "the worker could not delete a sandbox", "sandbox", s.ID, "err", err)
		}
	}
}

// connect registers and holds one stream. Registration comes first every
// time: the control plane mints a worker id per registration, so a worker
// that reconnects is a new claimer and the operations its predecessor held
// are redelivered rather than waiting on a connection that is gone. It
// reports whether the control plane accepted the stream, which is the
// registration taken and the stream opened.
func (w *worker) connect(ctx context.Context) (bool, error) {
	key, err := w.options.Key(ctx)
	if err != nil {
		return false, fmt.Errorf("worker: reading the environment key: %w", err)
	}
	if strings.TrimSpace(key) == "" {
		return false, errors.New("worker: the environment key is empty")
	}
	if err = w.register(ctx, key); err != nil {
		return false, err
	}
	conn, err := w.dial(ctx, key)
	if err != nil {
		return false, err
	}
	server := remote.NewServer(conn, remote.ServerOptions{
		Worker: w.id, Driver: w.options.Driver, Log: w.log,
		Admit: w.gate.admit, Answered: w.gate.answered,
	})
	w.tell(State{Connected: true, Registered: w.registered})
	return true, closeRefusal(server.Run(ctx))
}

// closeRefusal turns a stream the control plane closed with one of the
// refusals a worker stops on into that Refusal, and leaves every other end
// of a stream as it is.
func closeRefusal(err error) error {
	var closed *websocket.CloseError
	if errors.As(err, &closed) && closed.Code == remote.ClosePolicy && slices.Contains(fatal, closed.Text) {
		return &Refusal{Code: closed.Text, Detail: "the control plane closed the stream"}
	}
	return err
}

// register declares this worker, its driver and its host, and takes the id
// it claims under.
func (w *worker) register(ctx context.Context, key string) error {
	d := w.options.Driver
	registration := remote.Registration{
		Driver: d.Name(), Isolation: d.Isolation(), Capabilities: d.Capabilities(),
		Capacity: w.options.Capacity, Labels: w.options.Labels, Version: w.options.Version,
	}
	if describer, ok := d.(runtime.HostDescriber); ok {
		host := describer.Host()
		registration.Host = &host
	}
	body, err := json.Marshal(registration)
	if err != nil {
		return fmt.Errorf("worker: the registration did not encode: %w", err)
	}
	endpoint, err := url.Parse(strings.TrimRight(w.options.URL, "/"))
	if err != nil {
		return fmt.Errorf("worker: the control plane URL is not a URL: %w", err)
	}
	endpoint.Path = client.Route(endpoint.Path, "/v1/environments/"+url.PathEscape(w.options.Environment)+"/workers")
	endpoint.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
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
		refusal, fatal := refusalOf(answer)
		if fatal {
			return &refusal
		}
		return &RegistrationError{
			StatusCode: res.StatusCode, Status: res.Status, Code: refusal.Code, Body: strings.TrimSpace(string(answer)),
		}
	}
	var registered remote.Registered
	if err = json.Unmarshal(answer, &registered); err != nil {
		return fmt.Errorf("worker: the registration answer did not decode: %w", err)
	}
	if registered.Worker == "" {
		return errors.New("worker: the control plane accepted the registration and named no worker")
	}
	w.id, w.environment, w.registered = registered.Worker, registered.Environment, registered
	w.log.InfoContext(ctx, "the worker registered",
		"environment", w.environment, "worker", w.id, "driver", registration.Driver)
	return nil
}

// refusalOf reads the error envelope of a refused registration and reports
// the Refusal it carries, and whether its code is one the worker stops on.
// An answer that is no envelope is one the worker retries on.
func refusalOf(answer []byte) (Refusal, bool) {
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return Refusal{Detail: err.Error()}, false
	}
	detail, _ := envelope.Error.Details["detail"].(string)
	return Refusal{Code: envelope.Error.Code, Detail: detail}, slices.Contains(fatal, envelope.Error.Code)
}

// dial opens the one stream. The key is the bearer and the subprotocol is the
// version of the vocabulary, so a control plane of another release refuses
// the upgrade rather than half understanding what follows.
func (w *worker) dial(ctx context.Context, key string) (remote.FrameConn, error) {
	endpoint, err := StreamURL(w.options.URL, w.environment)
	if err != nil {
		return nil, err
	}
	dialer := &websocket.Dialer{
		Subprotocols:     []string{remote.Protocol},
		HandshakeTimeout: registerTimeout,
	}
	conn, res, err := dialer.DialContext(ctx, endpoint, http.Header{
		"Authorization": []string{"Bearer " + key},
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
		return "", fmt.Errorf("worker: the control plane URL is not a URL: %w", err)
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
