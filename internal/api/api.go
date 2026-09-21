// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api serves the authenticated direct-workspace API.
package api

import (
	"archive/tar"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
)

// Verifier verifies a bearer without interpreting hosted identity claims.
type Verifier interface {
	// VerifyContext verifies one bearer under the request's own context,
	// which bounds the revocation read a token cellad minted costs.
	VerifyContext(context.Context, string) (auth.Caller, error)
}
type Options struct {
	Controller     *controller.Controller
	Verifier       Verifier
	Authorizer     *auth.Authorizer
	MaxBodyBytes   int64
	MaxUploadBytes int64
	// Egress is the environment's gateways. It is optional: with none, the
	// sync stream answers not found and a sandbox's records are empty.
	Egress *EgressHub
	// Events is design 009's emitter. Nil journals no operation; the
	// mutations below it are journaled by the store either way.
	Events *events.Emitter
	// Keys mints and revokes the credential a data plane carries (spec
	// 021). Nil serves no key route, which is a control plane whose data
	// plane roles were keyed elsewhere.
	Keys EnvironmentKeys
	// Workers is the control plane's side of every worker stream (spec
	// 021). Nil serves no worker environment: the two routes an
	// environment key reaches on a worker's behalf answer
	// capability_unsupported.
	Workers *remote.Hub
	// Admit is stage 3 of a resolve, design 007's admission step. Nil is
	// the identity: the operator configured no endpoint.
	Admit manifest.AdmitFunc
	// Defaults are the values an absent manifest field takes, from the
	// operator's configuration. Only the image is read from a variable
	// today; the six figures of design 007 reach the resolver the same
	// way once they are loaded.
	Defaults manifest.Defaults
	// Metrics is design 017's recorder. It is optional: with none the API
	// counts nothing and answers the same.
	Metrics Metrics
	// Log takes the one line per request of design 017. Nil is the default
	// logger, which is the redacting one cellad installs before it builds
	// anything.
	Log *slog.Logger
}
type handler struct {
	Options
	mux     *http.ServeMux
	metrics Metrics
	log     *slog.Logger
	// patterns is every route this handler registered, in registration
	// order. It is the half of design 008's document that the server knows;
	// TestTheDocumentAndTheMuxAgree reads it against api/openapi.yaml, so a
	// route added here and left out of the document fails the build.
	patterns []string
}

func New(o Options) (http.Handler, error) {
	if o.Controller == nil || o.Verifier == nil || o.Authorizer == nil {
		return nil, errors.New("API requires controller, verifier and authorizer")
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 65536
	}
	if o.MaxUploadBytes <= 0 {
		o.MaxUploadBytes = 1 << 30
	}
	h := &handler{
		Options: o, mux: http.NewServeMux(),
		metrics: cmp.Or(o.Metrics, Metrics(nopMetrics{})),
		log:     cmp.Or(o.Log, slog.Default()),
	}
	// The routes whose answer is one object or one page, in the syntax the
	// request negotiated.
	h.handle("DELETE /v1/sandboxes/{id}/files", h.fileRemove)
	h.handle("GET /v1/sandboxes/{id}/files/stat", h.fileStat)
	h.handle("GET /v1/sandboxes/{id}/files/list", h.fileList)
	h.handle("POST /v1/sandboxes/{id}/files/mkdir", h.fileMkdir)
	h.handle("POST /v1/sandboxes/{id}/files/move", h.fileMove)
	h.handle("GET /v1/sandboxes/{id}/egress", h.egressRecords)
	h.handle("POST /v1/sandboxes", h.create)
	h.handle("GET /v1/sandboxes", h.list)
	h.handle("GET /v1/sandboxes/{id}", h.item)
	h.handle("DELETE /v1/sandboxes/{id}", h.item)
	h.handle("POST /v1/sandboxes/{id}/{verb}", h.item)
	h.handle("GET /v1/sandboxes/{id}/display", h.display)
	h.handle("POST /v1/sandboxes/{id}/input", h.input)
	h.handle("GET /v1/sandboxes/{id}/ports", h.ports)
	h.handle("GET /v1/events", h.eventFeed)
	h.handle("POST /v1/secrets", h.createSecret)
	h.handle("GET /v1/secrets", h.listSecrets)
	h.handle("PUT /v1/secrets/{key}", h.applySecret)
	h.handle("GET /v1/secrets/{key}", h.secretItem)
	h.handle("DELETE /v1/secrets/{key}", h.secretItem)
	h.handle("GET /v1/environments", h.environmentList)
	h.handle("GET /v1/environments/{id}", h.environmentItem)
	h.handle("POST /v1/environments/{id}/keys", h.environmentKeyMint)
	h.handle("DELETE /v1/environments/{id}/keys/{jti}", h.environmentKeyRevoke)
	// The routes whose content type is the route's own: an archive, a file
	// body, a frame, a log, a framed stream and the four sockets. A caller
	// asking for one of those asks for the route and not for a syntax, so
	// there is nothing to negotiate.
	h.stream("GET /v1/sandboxes/{id}/files", h.files)
	h.stream("PUT /v1/sandboxes/{id}/files", h.filesPut)
	h.stream("GET /v1/sandboxes/{id}/files/content", h.fileContent)
	h.stream("GET /v1/sandboxes/{id}/logs", h.logs)
	h.stream("POST /v1/sandboxes/{id}/exec", h.execRoute)
	h.stream("GET /v1/sandboxes/{id}/exec", h.execSocket)
	h.stream("GET /v1/sandboxes/{id}/attach", h.attachSocket)
	h.stream("GET /v1/sandboxes/{id}/dial/{port}", h.dial)
	h.stream("GET /v1/sandboxes/{id}/screenshot", h.screenshot)
	h.stream("GET /v1/sandboxes/{id}/screen", h.screen)
	return h, nil
}

type callerKey struct{}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Design 017's count is one per request, including the ones refused
	// before the mux chose an endpoint. The slot rides the context so the
	// wrapper behind the mux can tell this one which route it reached.
	slot := &routeSlot{}
	rw := &observed{ResponseWriter: w}
	defer h.observe(r.Context(), slot, rw, time.Now())
	w = rw
	w.Header().Set(RequestIDHeader, requestID(r))
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		respondError(w, &auth.Error{Code: auth.CodeUnauthenticated, Detail: "missing bearer"})
		return
	}
	caller, err := h.Verifier.VerifyContext(r.Context(), token)
	if err != nil {
		respondError(w, err)
		return
	}
	// An environment key authorizes the data plane's own streams and
	// nothing else: it names an environment, not a subject, so no route
	// that decides on a subject can be reached with one.
	if environment, isEnvironment := caller.Environment(); isEnvironment {
		if id, ok := egressStreamPath(r); ok && h.Egress != nil {
			// The stream is served before the mux, so it names its own
			// route: design 017's label is the endpoint and never the path,
			// which carries the environment's name.
			slot.route = EgressStreamRoute
			nameSpan(r.Context(), EgressStreamRoute)
			if id != environment {
				respondError(w, &auth.Error{Code: auth.CodeForbidden, Detail: "the key names another environment"})
				return
			}
			h.Egress.ServeGateway(w, r, environment)
			return
		}
		if route, ok := workerPath(r); ok {
			h.serveWorkerRoute(w, r, route, environment)
			return
		}
		respondError(w, &auth.Error{Code: auth.CodeForbidden, Detail: "environment keys authorize data plane streams only"})
		return
	}
	slot.subject, slot.requestID = caller.Subject, w.Header().Get(RequestIDHeader)
	stampSpan(r.Context(), caller.Subject, slot.requestID)
	ctx := context.WithValue(r.Context(), callerKey{}, caller)
	// The slot rides the context from here, so the wrapper behind the mux
	// can tell this handler which route the request reached. A request
	// refused above never chose an endpoint and carries none.
	ctx = context.WithValue(ctx, slotKey{}, slot)
	// The actor rides the context from here: a mutation several calls below
	// this handler records who asked for it and under which request id, and
	// a controller loop that runs under no request records neither.
	ctx = events.WithActor(ctx, actorOf(w, caller))
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}
func caller(r *http.Request) auth.Caller {
	c, _ := r.Context().Value(callerKey{}).(auth.Caller)
	return c
}
func requestInfo(r *http.Request) authz.Caller {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return authz.Caller{IP: ip, UserAgent: r.UserAgent()}
}
func resource(obj v1.Sandbox) authz.Resource {
	return (auth.Sandbox{ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner,
		Environment: obj.Spec.Environment, Parent: obj.Status.Parent, Root: obj.Status.Root,
		Labels: obj.Metadata.Labels}).Resource()
}
func (h *handler) decide(r *http.Request, action string, res authz.Resource) (auth.Decision, error) {
	return h.decideAs(r, caller(r), action, res)
}

// decideAs is decide for a caller the endpoint enriched: a workload create
// carries the calling sandbox's status, so the envelope's workload member is
// the store's record of the tree rather than the token's claim.
func (h *handler) decideAs(r *http.Request, c auth.Caller, action string, res authz.Resource) (auth.Decision, error) {
	d, err := h.Authorizer.Decide(r.Context(), c, requestInfo(r), action, res)
	if err == nil && d.Limits.RequestsPerMinute > 0 {
		err = &manifest.Error{Code: "capability_unsupported", Detail: "requests_per_minute limit is not implemented"}
	}
	return d, err
}
func (h *handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBodyBytes))
}

// resolveOptions is what one apply hands the resolver: the environment
// this node drives with the capabilities its driver declares, the secrets
// this caller may mount, the operator's defaults, and the admission step of
// design 007 with everything that step is told about the caller. The
// capabilities are what a field asking for an optional behaviour resolves
// against, so a manifest asking for a desktop where the driver declares no
// Display is refused at the field rather than at create. Defaults and
// admission run on every apply, whatever the environment declares. The
// request id is the one this response already carries, so a refusal at the
// endpoint and the error a caller reads name the same apply.
func (h *handler) resolveOptions(w http.ResponseWriter, r *http.Request) (manifest.Options, error) {
	c := caller(r)
	o := manifest.DriverOptions(h.Controller.Environment(), h.Controller.DriverName(), h.Controller.Isolation(), h.Controller.Capabilities(), h.secretLookup(r))
	o.Actor = manifestActor(r)
	o.Claims = c.Claims
	o.Defaults = h.Defaults
	o.Admit = h.Admit
	o.RequestID = w.Header().Get(RequestIDHeader)
	id, workload := c.Sandbox()
	if !workload {
		return o, nil
	}
	// A sandbox applying through its own token is named as one, and its own
	// sandbox is the parent every boundary rule is read against. An
	// admission step that will not grant a workload the authority of its
	// owner cannot see the difference from the subject alone, so a calling
	// sandbox this node cannot read is a refusal and not an apply the
	// endpoint decides on as if a person had made it.
	calling, err := h.Controller.Get(r.Context(), id, c.Subject)
	if err != nil {
		return o, err
	}
	status := calling.Status
	o.Workload = &status
	o.Parent = &calling
	return o, nil
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	obj, err := manifest.Decode(body, r.Header.Get("Content-Type"))
	if err != nil {
		respondError(w, err)
		return
	}
	options, err := h.resolveOptions(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	obj, _, err = manifest.ResolveNativeWith(r.Context(), obj, options)
	if err != nil {
		respondError(w, err)
		return
	}
	parent := options.Parent
	c := caller(r)
	// Ownership is established before authorizing; client status has already gone.
	// A spawned child takes the root's owner, so one tree has one owner and
	// one namespace of names (spec 022).
	obj.Status.Owner = c.Subject
	if parent != nil {
		obj.Status.Owner = parent.Status.Owner
		obj.Status.Parent = parent.Status.ID
		obj.Status.Root = parent.Status.Root
		c = c.WithSandbox(parent.Status)
	}
	d, err := h.decideAs(r, c, authorizer.ActionSandboxCreate, resource(obj))
	if err != nil {
		respondError(w, err)
		return
	}
	// The environment of a spawn is its parent's by boundary rule 8, and
	// that environment's use was decided when the root was created. Asking
	// again would ask a sandbox whether it may use an environment, which is
	// a question about a person.
	if parent == nil {
		envDecision, err := h.Authorizer.Lookup(r.Context(), c, requestInfo(r), authorizer.ActionEnvironmentUse, (auth.Environment{ID: obj.Spec.Environment, Name: obj.Spec.Environment, Isolation: h.Controller.Isolation()}).Resource())
		if err == nil && envDecision.Limits.RequestsPerMinute > 0 {
			err = &manifest.Error{Code: "capability_unsupported", Detail: "requests_per_minute limit is not implemented"}
		}
		if err != nil {
			respondError(w, err)
			return
		}
	}
	if parent != nil {
		obj, err = h.Controller.Spawn(r.Context(), obj, *parent, d.Limits.MaxSandboxes)
	} else {
		obj, err = h.Controller.Create(r.Context(), obj, obj.Status.Owner, d.Limits.MaxSandboxes)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/sandboxes/"+obj.Status.ID)
	respond(w, http.StatusCreated, obj)
}

// execRoute serves POST /v1/sandboxes/{id}/exec. It is its own pattern rather
// than a verb of the item route because its two forms answer in two ways: a
// JSON result under ?wait=1, in the syntax the request negotiates, and the
// framed stream of design 008 otherwise, whose content type is the route's.
func (h *handler) execRoute(w http.ResponseWriter, r *http.Request) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return
	}
	h.exec(w, r, obj)
}

func (h *handler) item(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")
	action := authorizer.ActionSandboxRead
	switch r.Method {
	case http.MethodDelete:
		verb = "delete"
		action = authorizer.ActionSandboxDelete
	case http.MethodPost:
		switch verb {
		case "start", "stop":
			action = authorizer.ActionSandboxUpdate
		default:
			respondError(w, controller.ErrNotFound)
			return
		}
	}
	obj, err := h.Controller.Get(r.Context(), r.PathValue("id"), caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	res := resource(obj)
	if action == authorizer.ActionSandboxUpdate {
		res.Fields["proposed"] = map[string]any{"owner": obj.Status.Owner, "metadata": obj.Metadata, "spec": obj.Spec}
	}
	if _, err = h.decide(r, action, res); err != nil {
		respondError(w, err)
		return
	}
	switch verb {
	case "start", "stop", "delete":
		obj, err = h.Controller.Act(r.Context(), obj.Status.ID, verb)
	default:
		obj, err = h.Controller.Refresh(r.Context(), obj)
	}
	if err != nil {
		respondError(w, err)
		return
	}
	if verb == "delete" {
		respond(w, http.StatusAccepted, obj)
		return
	}
	respond(w, http.StatusOK, obj)
}
func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	d, err := h.decide(r, authorizer.ActionSandboxList, auth.List(authorizer.ActionSandboxList))
	if err != nil {
		respondError(w, err)
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, err = strconv.Atoi(q)
		if err != nil || limit < 1 || limit > 200 {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "limit must be between 1 and 200"})
			return
		}
	}
	labels := map[string]string{}
	for _, q := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(q, "=")
		if !ok || k == "" {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "label selector requires key=value"})
			return
		}
		if prior, ok := labels[k]; ok && prior != v {
			respond(w, 200, map[string]any{"items": []v1.Sandbox{}, "next": ""})
			return
		}
		labels[k] = v
	}
	items := []v1.Sandbox{}
	next := ""
	q := r.URL.Query()
	// The tree selector of design 008 narrows the page to one root's
	// sandboxes, the root included; every other selector still applies, and
	// each row is still read one authorizer decision at a time.
	objects := h.Controller.List()
	if root := q.Get("root"); root != "" {
		objects = h.Controller.Tree(root)
	}
	for _, obj := range objects {
		if obj.Status.ID <= q.Get("cursor") || (q.Get("owner") != "" && q.Get("owner") != obj.Status.Owner) || (q.Get("environment") != "" && q.Get("environment") != obj.Spec.Environment) || !matches(obj, labels) {
			continue
		}
		if d.Filter != nil {
			if len(d.Filter.Owners) > 0 && !slices.Contains(d.Filter.Owners, obj.Status.Owner) {
				continue
			}
			if !matches(obj, d.Filter.Labels) {
				continue
			}
		}
		if _, err = h.decide(r, authorizer.ActionSandboxRead, resource(obj)); err != nil {
			if auth.CodeOf(err) == auth.CodeForbidden {
				continue
			}
			respondError(w, err)
			return
		}
		obj, err = h.Controller.Refresh(r.Context(), obj)
		if err != nil {
			respondError(w, err)
			return
		}
		if q.Get("phase") != "" && q.Get("phase") != obj.Status.Phase {
			continue
		}
		if len(items) == limit {
			next = items[len(items)-1].Status.ID
			break
		}
		items = append(items, obj)
	}
	respond(w, 200, map[string]any{"items": items, "next": next})
}
func matches(obj v1.Sandbox, labels map[string]string) bool {
	for k, v := range labels {
		got, ok := obj.Metadata.Labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

type execRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}
type execResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"durationMs"`
}

func (h *handler) exec(w http.ResponseWriter, r *http.Request, obj v1.Sandbox) {
	if r.URL.Query().Get("wait") != "1" {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "only exec?wait=1 is currently supported"})
		return
	}
	// The bounded form answers one JSON result, so it negotiates like every
	// other object route. The framed form above answers the route's own
	// content type and negotiates nothing.
	if !acceptable(w, r) {
		return
	}
	body, err := h.readBody(w, r)
	if err != nil {
		respondError(w, err)
		return
	}
	var req execRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err == nil {
		var tail any
		if dec.Decode(&tail) != io.EOF {
			err = errors.New("expected one JSON object")
		}
	}
	if err != nil {
		respondError(w, &manifest.Error{Code: "invalid_field", Detail: err.Error()})
		return
	}
	if err = manifest.ValidateExec(req.Command, req.Env, req.Workdir); err != nil {
		respondError(w, err)
		return
	}
	timeout := 10 * time.Minute
	if req.Timeout != "" {
		timeout, err = time.ParseDuration(req.Timeout)
		if err != nil || timeout <= 0 || timeout > time.Hour {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "timeout must be positive and at most 1h"})
			return
		}
	}
	h.touch(r, obj)
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	running, err := h.Controller.Exec(ctx, obj.Status.ID, driver.ExecRequest{Command: req.Command, Env: req.Env, Workdir: req.Workdir, Timeout: timeout})
	if err != nil {
		respondError(w, err)
		return
	}
	defer func() { _ = running.Close() }()
	var stdout, stderr cappedBuffer
	copied := make(chan error, 2)
	go func() { _, copyErr := io.Copy(&stdout, running.Stdout()); copied <- copyErr }()
	go func() { _, copyErr := io.Copy(&stderr, running.Stderr()); copied <- copyErr }()
	var copyErr error
	for range 2 {
		if failure := <-copied; failure != nil {
			copyErr = errors.Join(copyErr, failure)
			_ = running.Close()
		}
	}
	if copyErr != nil {
		respondError(w, &manifest.Error{Code: "driver_unavailable", Detail: "exec output stream failed: " + copyErr.Error()})
		return
	}
	code, err := running.Wait(ctx)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = 124
		err = nil
	}
	if err != nil {
		respondError(w, err)
		return
	}
	elapsed := time.Since(started).Milliseconds()
	respond(w, 200, execResult{code, string(stdout.data), string(stderr.data), stdout.truncated || stderr.truncated, elapsed})
	h.emit(r, obj, events.TypeExec, events.Exec{ExitCode: code, DurationMS: elapsed})
}

type cappedBuffer struct {
	data      []byte
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

// respond writes one object in the syntax this request negotiated: JSON in
// the Go type's own order, or YAML where the caller named one of design 003's
// three types. An error is always JSON, because an envelope a client cannot
// parse says less than one in the syntax it did not ask for.
func respond(w http.ResponseWriter, status int, body any) {
	if wantsYAML(w) {
		respondYAML(w, status, body)
		return
	}
	httpjson.Write(w, status, body)
}
func respondError(w http.ResponseWriter, err error) {
	status, envelope := errorEnvelope(err, w.Header().Get(RequestIDHeader))
	noteCode(w, envelope.Code)
	httpjson.WriteError(w, status, envelope)
}

// errorEnvelope is the error table of design 008: one code, one status and one
// fixed user sentence per failure, with the developer sentence in details. The
// WebSocket routes send the same envelope in a text frame, so a client reads
// one error shape whether it failed before or after the upgrade.
func errorEnvelope(err error, requestID string) (int, httpjson.Error) {
	code := "driver_unavailable"
	status := 503
	message := "The environment is unavailable; retry shortly."
	var me *manifest.Error
	var tooLarge *http.MaxBytesError
	switch {
	case auth.CodeOf(err) != "":
		code = string(auth.CodeOf(err))
	case errors.As(err, &me):
		code = me.Code
	case errors.As(err, &tooLarge):
		code = "body_too_large"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, tar.ErrHeader):
		code = "bad_request"
	case errors.Is(err, controller.ErrNotFound), errors.Is(err, driver.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		code = "not_found"
	case errors.Is(err, controller.ErrNameTaken), errors.Is(err, driver.ErrAlreadyExists):
		code = "name_taken"
	case errors.Is(err, controller.ErrNoSecretKey):
		code = "capability_unsupported"
	case errors.Is(err, controller.ErrQuota):
		code = "quota_exceeded"
	case errors.Is(err, controller.ErrBudgetExhausted):
		code = "spawn_budget_exhausted"
	case errors.Is(err, controller.ErrPhase), errors.Is(err, driver.ErrNotRunning):
		code = "phase_conflict"
	case errors.Is(err, driver.ErrUnsupported):
		code = "capability_unsupported"
	case errors.Is(err, driver.ErrInvalid):
		code = "invalid_field"
	case errors.Is(err, driver.ErrTooLarge):
		code = "body_too_large"
	}
	switch code {
	case "unauthenticated":
		status = 401
		message = "Sign in and send a valid token."
	case "forbidden":
		status = 403
		message = "You do not have permission to do this."
	case "not_found":
		status = 404
		message = "There is no such object."
	case "authorizer_unavailable":
		status = 503
		message = "The permission service is unavailable; retry shortly."
	case "name_taken":
		status = 409
		message = "You already have an object with this name."
	case "phase_conflict":
		status = 409
		message = "The sandbox is not in a state that allows this."
	case "quota_exceeded":
		status = 422
		message = "You have reached your sandbox limit."
	case "body_too_large":
		status = 413
		message = "The request body is larger than this server accepts."
	case "unsupported_media_type":
		status = 415
		message = "Send the manifest as JSON or YAML."
	case "not_acceptable":
		status = 406
		message = "This endpoint answers in JSON or YAML."
	case "capability_unsupported":
		status = 422
		message = "The environment cannot provide this."
	case "environment_mismatch":
		status = 422
		message = "This worker does not match the environment it registered on."
	case "invalid_field":
		status = 400
		message = "A field has a value it cannot take."
	case "bad_request":
		status = 400
		message = "The request could not be read."
	case "unknown_field":
		status = 400
		message = "The manifest has a field this schema does not know."
	case "multi_document":
		status = 400
		message = "Send one manifest per request."
	case "unsupported_version":
		status = 400
		message = "This server serves cella.latere.ai/v1beta1."
	case "unsupported_kind":
		status = 400
		message = "This server does not serve that kind."
	case "reserved_prefix":
		status = 400
		message = "That name is reserved for the control plane."
	case "exclusive_fields":
		status = 400
		message = "Two fields that cannot be set together are set."
	case "immutable_field":
		status = 409
		message = "This field cannot be changed after the object is created."
	case "ceiling_exceeded":
		status = 422
		message = "The value is above what this server allows."
	case "admission_refused":
		status = 422
		message = "The request was refused by this server's policy."
	case "admission_unavailable":
		status = 503
		message = "The policy service is unavailable; retry shortly."
	case "missing_field":
		status = 400
		message = "A required field is missing."
	case "secret_host_conflict":
		status = 409
		message = "Two mounted secrets apply to the same host."
	case "secret_out_of_scope":
		status = 422
		message = "The secret does not cover the host it is used for."
	case "boundary_widened":
		status = 409
		message = "A sandbox cannot widen its own boundary."
	case "boundary_exceeded":
		status = 422
		message = "A child sandbox cannot exceed its parent's boundary."
	case "spawn_budget_exhausted":
		status = 422
		message = "The sandbox has no spawn budget left."
	}
	details := map[string]any{"request_id": requestID, "detail": fmt.Sprint(err)}
	if me != nil && len(me.Paths) > 0 {
		details["paths"] = me.Paths
	}
	return status, httpjson.Error{Code: code, Message: message, Details: details}
}
