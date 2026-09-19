// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api serves the authenticated direct-workspace API.
package api

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Verifier verifies a bearer without interpreting hosted identity claims.
type Verifier interface {
	Verify(string) (auth.Caller, error)
}
type Options struct {
	Controller     *controller.Controller
	Verifier       Verifier
	Authorizer     *auth.Authorizer
	MaxBodyBytes   int64
	MaxUploadBytes int64
}
type handler struct {
	Options
	mux *http.ServeMux
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
	h := &handler{Options: o, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /v1/sandboxes/{id}/files", h.files)
	h.mux.HandleFunc("PUT /v1/sandboxes/{id}/files", h.files)
	h.mux.HandleFunc("GET /v1/sandboxes/{id}/logs", h.logs)
	h.mux.HandleFunc("POST /v1/sandboxes", h.create)
	h.mux.HandleFunc("GET /v1/sandboxes", h.list)
	h.mux.HandleFunc("GET /v1/sandboxes/{id}", h.item)
	h.mux.HandleFunc("DELETE /v1/sandboxes/{id}", h.item)
	h.mux.HandleFunc("POST /v1/sandboxes/{id}/{verb}", h.item)
	return h, nil
}

type callerKey struct{}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-ID", rand.Text())
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		respondError(w, &auth.Error{Code: auth.CodeUnauthenticated, Detail: "missing bearer"})
		return
	}
	caller, err := h.Verifier.Verify(token)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, worker := caller.Environment(); worker {
		respondError(w, &auth.Error{Code: auth.CodeForbidden, Detail: "environment keys authorize worker routes only"})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller))
	h.mux.ServeHTTP(w, r)
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
	return (auth.Sandbox{ID: obj.Status.ID, Name: obj.Metadata.Name, Owner: obj.Status.Owner, Environment: obj.Spec.Environment, Labels: obj.Metadata.Labels}).Resource()
}
func (h *handler) decide(r *http.Request, action string, res authz.Resource) (auth.Decision, error) {
	d, err := h.Authorizer.Decide(r.Context(), caller(r), requestInfo(r), action, res)
	if err == nil && d.Limits.RequestsPerMinute > 0 {
		err = &manifest.Error{Code: "capability_unsupported", Detail: "requests_per_minute limit is not implemented"}
	}
	return d, err
}
func (h *handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBodyBytes))
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
	obj, err = manifest.ResolveNative(r.Context(), obj, h.Controller.Environment())
	if err != nil {
		respondError(w, err)
		return
	}
	// Ownership is established before authorizing; client status has already gone.
	obj.Status.Owner = caller(r).Subject
	d, err := h.decide(r, authorizer.ActionSandboxCreate, resource(obj))
	if err != nil {
		respondError(w, err)
		return
	}
	envDecision, err := h.Authorizer.Lookup(r.Context(), caller(r), requestInfo(r), authorizer.ActionEnvironmentUse, (auth.Environment{ID: obj.Spec.Environment, Name: obj.Spec.Environment, Isolation: h.Controller.Isolation()}).Resource())
	if err == nil && envDecision.Limits.RequestsPerMinute > 0 {
		err = &manifest.Error{Code: "capability_unsupported", Detail: "requests_per_minute limit is not implemented"}
	}
	if err != nil {
		respondError(w, err)
		return
	}
	obj, err = h.Controller.Create(r.Context(), obj, caller(r).Subject, d.Limits.MaxSandboxes)
	if err != nil {
		respondError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/sandboxes/"+obj.Status.ID)
	respond(w, http.StatusCreated, obj)
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
		case "exec":
			action = authorizer.ActionSandboxExec
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
	case "exec":
		h.exec(w, r, obj)
		return
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
	if r.URL.Query().Get("root") != "" {
		respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: "spawn tree selectors are not implemented"})
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
	for _, obj := range h.Controller.List() {
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
	respond(w, 200, execResult{code, string(stdout.data), string(stderr.data), stdout.truncated || stderr.truncated, time.Since(started).Milliseconds()})
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
func respond(w http.ResponseWriter, status int, body any) {
	httpjson.Write(w, status, body)
}
func respondError(w http.ResponseWriter, err error) {
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
	case errors.Is(err, controller.ErrQuota):
		code = "quota_exceeded"
	case errors.Is(err, controller.ErrPhase), errors.Is(err, driver.ErrNotRunning):
		code = "phase_conflict"
	case errors.Is(err, driver.ErrUnsupported):
		code = "capability_unsupported"
	case errors.Is(err, driver.ErrInvalid):
		code = "invalid_field"
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
	case "capability_unsupported":
		status = 422
		message = "The environment cannot provide this."
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
	case "immutable_field":
		status = 409
		message = "This field cannot be changed after the object is created."
	case "ceiling_exceeded":
		status = 422
		message = "The value is above what this server allows."
	case "admission_refused":
		status = 422
		message = "The request was refused by this server's policy."
	}
	details := map[string]any{"request_id": w.Header().Get("X-Request-ID"), "detail": fmt.Sprint(err)}
	if me != nil && len(me.Paths) > 0 {
		details["paths"] = me.Paths
	}
	httpjson.WriteError(w, status, httpjson.Error{Code: code, Message: message, Details: details})
}
