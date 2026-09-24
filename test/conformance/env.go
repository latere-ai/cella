// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	cellaclient "latere.ai/x/cella/client"
)

// APIVersion is the group and version of every manifest the suite sends. It
// is the API group of the contract and not one installation's address.
const APIVersion = "cella.latere.ai/v1beta1"

// pollInterval is how often a case asks for a phase. No case sleeps for a
// state: it polls until the case's own deadline.
const pollInterval = 50 * time.Millisecond

// Env is what a case is given: the clients of the identities the run holds,
// the configuration, the capabilities the environment declares, and the
// recorder that remembers every object the run made.
type Env struct {
	cfg Config
	// caller is the subject every case acts as. other is a second subject,
	// nil when the configuration mints none. admin is a subject the server
	// treats as an administrator, nil when none was supplied.
	caller *client
	other  *client
	admin  *client
	// run is this run's prefix, so two runs against one server never meet.
	run  string
	caps map[string]bool

	mu      sync.Mutex
	objects []stored
	counter int
}

// stored is one object the run created: what deletes it and where.
type stored struct {
	path string
	c    *client
	id   string
	// deleted is set once the case that made the object ended and its
	// delete was sent, so the run's own cleanup does not send it again.
	deleted bool
}

// newEnv resolves the identities and reads what the environment declares.
func newEnv(ctx context.Context, cfg Config) (*Env, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("no server: set URL")
	}
	base, err := url.Parse(cfg.URL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("%q is no http or https address", cfg.URL)
	}
	e := &Env{cfg: cfg, run: strings.ToLower(rand.Text()[:6]), caps: map[string]bool{}}
	for _, name := range cfg.Capabilities {
		e.caps[strings.ToLower(strings.TrimSpace(name))] = true
	}
	trimmed := strings.TrimSuffix(base.String(), "/")
	caller := cfg.Caller
	if caller == "" && cfg.Token != nil {
		if caller, err = cfg.Token(ctx, "alice"); err != nil {
			return nil, fmt.Errorf("minting the caller's token: %w", err)
		}
	}
	if caller == "" {
		return nil, fmt.Errorf("no caller: set Caller or Token")
	}
	e.caller = newClient(trimmed, caller)
	if cfg.Token != nil {
		second, err := cfg.Token(ctx, "bob")
		if err != nil {
			return nil, fmt.Errorf("minting the second subject's token: %w", err)
		}
		e.other = newClient(trimmed, second)
	}
	if cfg.Admin != "" {
		e.admin = newClient(trimmed, cfg.Admin)
	}
	return e, nil
}

// client is one identity's HTTP client. The suite reads the wire, so every
// request is built here and no helper hides a header.
type client struct {
	// base is the address the client was given, a server's public URL with
	// its path. origin is its scheme and host, and prefix its path: a case
	// writes each route as a server at the root serves it, and the request
	// goes to the route under the prefix by the rule every client composes
	// with, so a server mounted under a base runs the same cases.
	base, origin, prefix string
	token                string
	http                 *http.Client
}

// newClient builds one identity's client. The transport is the suite's own:
// no proxy, because a run reaches the server it was pointed at and nothing
// else, and a bound on the first response byte rather than on the whole
// exchange, because a case that reads a stream is bounded by its own
// deadline and not by a clock inside the client.
func newClient(base, token string) *client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	c := &client{base: base, origin: base, token: token, http: &http.Client{Transport: transport}}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		c.prefix = strings.TrimSuffix(u.EscapedPath(), "/")
		c.origin = strings.TrimSuffix(base, u.EscapedPath())
	}
	return c
}

// request is one call's inputs. A zero request is a GET with no body.
type request struct {
	Body        []byte
	Reader      io.Reader
	ContentType string
	Accept      string
	Header      map[string]string
	// NoBearer sends no Authorization header, for the routes that need none
	// and for the unauthenticated case.
	NoBearer bool
	// Token overrides the client's own bearer.
	Token string
}

// exchange is one call and its answer.
type exchange struct {
	Method string
	Path   string
	Status int
	Header http.Header
	Body   []byte
}

// build renders one request.
func (c *client) build(ctx context.Context, method, path string, req request) (*http.Request, error) {
	body := req.Reader
	if body == nil && req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.origin+cellaclient.Route(c.prefix, path), body)
	if err != nil {
		return nil, err
	}
	if !req.NoBearer {
		token := req.Token
		if token == "" {
			token = c.token
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if req.ContentType != "" {
		r.Header.Set("Content-Type", req.ContentType)
	}
	if req.Accept != "" {
		r.Header.Set("Accept", req.Accept)
	}
	for name, value := range req.Header {
		r.Header.Set(name, value)
	}
	return r, nil
}

// open sends one request and leaves the body open, for a stream.
func (c *client) open(ctx context.Context, method, path string, req request) (*http.Response, error) {
	r, err := c.build(ctx, method, path, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// send is one call, its answer read whole. A body larger than 8 MiB is
// truncated: no case asserts over more.
func (c *client) send(ctx context.Context, method, path string, req request) (*exchange, error) {
	resp, err := c.open(ctx, method, path, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	return &exchange{Method: method, Path: path, Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// get, post, put and del are the four calls a case makes.
func (c *client) get(ctx context.Context, path string) (*exchange, error) {
	return c.send(ctx, http.MethodGet, path, request{})
}

func (c *client) post(ctx context.Context, path string, body []byte) (*exchange, error) {
	return c.send(ctx, http.MethodPost, path, request{Body: body, ContentType: "application/json"})
}

func (c *client) put(ctx context.Context, path string, body []byte, contentType string) (*exchange, error) {
	return c.send(ctx, http.MethodPut, path, request{Body: body, ContentType: contentType})
}

func (c *client) del(ctx context.Context, path string) (*exchange, error) {
	return c.send(ctx, http.MethodDelete, path, request{})
}

// disagree is the error a case returns when the answer is not what the spec
// states.
func (x *exchange) disagree(want, got string) error {
	return &Disagreement{Method: x.Method, Path: x.Path, Want: want, Got: got, Body: string(x.Body)}
}

// status asserts one status code.
func (x *exchange) status(want int) error {
	if x.Status == want {
		return nil
	}
	return x.disagree(fmt.Sprintf("status %d", want), fmt.Sprintf("status %d", x.Status))
}

// envelope is the error body of design 008.
type envelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// refusal asserts one refusal: the status of the code's row, the code, the
// fixed sentence, and the request id every envelope carries.
func (x *exchange) refusal(code string) error {
	row, ok := errorTable[code]
	if !ok {
		return fmt.Errorf("no row in the error table for %q", code)
	}
	var env envelope
	if err := json.Unmarshal(x.Body, &env); err != nil {
		return x.disagree("the error envelope of design 008", "a body that is no envelope: "+err.Error())
	}
	if env.Error.Code != code {
		return x.disagree("code "+code, "code "+cmpOr(env.Error.Code, "none"))
	}
	if x.Status != row.Status {
		return x.disagree(fmt.Sprintf("status %d for %s", row.Status, code), fmt.Sprintf("status %d", x.Status))
	}
	if env.Error.Message != row.Message {
		return x.disagree("message "+row.Message, "message "+env.Error.Message)
	}
	if id, _ := env.Error.Details["request_id"].(string); id == "" {
		return x.disagree("details.request_id on every envelope", "no request id in the details")
	}
	return nil
}

// paths reads the field paths a refusal named.
func (x *exchange) paths() []string {
	var env envelope
	if json.Unmarshal(x.Body, &env) != nil {
		return nil
	}
	raw, _ := env.Error.Details["paths"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// object is the wire shape of one object: the fields every kind carries and
// the status fields the cases read. The spec stays raw, because a case
// asserts what the server returned and not what this module's types hold.
type object struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   objectMetadata  `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
	Status     objectStatus    `json:"status"`
}

type objectMetadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type objectStatus struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Environment string `json:"environment"`
	Driver      string `json:"driver"`
	Isolation   string `json:"isolation"`
	Phase       string `json:"phase"`
}

// list is the list envelope of design 008.
type listEnvelope struct {
	Items []object `json:"items"`
	Next  string   `json:"next"`
}

// decode reads one object from an answer that must be one.
func (x *exchange) object() (object, error) {
	var obj object
	if err := json.Unmarshal(x.Body, &obj); err != nil {
		return obj, x.disagree("one object as JSON", "a body that does not decode: "+err.Error())
	}
	return obj, nil
}

// name is an object name of this run: conformance-<run>-<n>. Every object a
// case creates carries it, and the run deletes by id and never by prefix.
func (e *Env) name() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counter++
	return fmt.Sprintf("conformance-%s-%d", e.run, e.counter)
}

// record remembers one object so the run deletes it, whatever the case did
// after creating it.
func (e *Env) record(c *client, path, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.objects = append(e.objects, stored{path: path, c: c, id: id})
}

// created is every id the run made, in the order it made them.
func (e *Env) created() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.objects))
	for _, o := range e.objects {
		out = append(out, o.id)
	}
	return out
}

// cleanup deletes every id this run made and has not deleted yet, and
// nothing else. A delete that fails is left alone: the run never widens its
// reach to clean up.
func (e *Env) cleanup(ctx context.Context) {
	e.release(ctx, 0)
}

// mark is the position in the run's objects a case starts at.
func (e *Env) mark() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.objects)
}

// release deletes the objects made from the mark on, newest first, and
// returns their ids. Each is deleted once however often release reaches it.
func (e *Env) release(ctx context.Context, mark int) []string {
	e.mu.Lock()
	var objects []stored
	var ids []string
	for i := mark; i < len(e.objects); i++ {
		ids = append(ids, e.objects[i].id)
		if !e.objects[i].deleted {
			objects = append(objects, e.objects[i])
			e.objects[i].deleted = true
		}
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for _, o := range slices.Backward(objects) {
		_, _ = o.c.del(ctx, o.path+"/"+o.id)
	}
	return ids
}

// manifest is the body of one Sandbox apply: the smallest manifest this
// environment takes, with the caller's changes applied to the decoded body
// so a case names a field and not a string of JSON.
func (e *Env) manifest(name string, mutate ...func(body map[string]any)) []byte {
	spec := map[string]any{}
	if e.cfg.Image != "" {
		spec["image"] = e.cfg.Image
	}
	spec["command"] = []any{"sleep", "300"}
	body := map[string]any{
		"apiVersion": APIVersion,
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}
	for _, m := range mutate {
		m(body)
	}
	return mustJSON(body)
}

// refuseApply posts one manifest that must be refused with the code named.
// An object that came back instead is recorded before the case fails, so a
// server that created what it was asked to refuse does not leave the object
// behind as well.
func (e *Env) refuseApply(ctx context.Context, req request, code string) error {
	x, err := e.caller.send(ctx, http.MethodPost, "/v1/sandboxes", req)
	if err != nil {
		return err
	}
	if obj, decodeErr := x.object(); decodeErr == nil && obj.Status.ID != "" {
		e.record(e.caller, "/v1/sandboxes", obj.Status.ID)
	}
	return x.refusal(code)
}

// mustJSON encodes a body the suite itself built. A value this package
// writes and cannot encode is a defect in the suite and not an answer from a
// server, so it is not an error a case returns.
func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic("the suite built a body it cannot encode: " + err.Error())
	}
	return out
}

// create applies one manifest and records the object. It returns the created
// object, so a case reads the status the server resolved.
func (e *Env) create(ctx context.Context, c *client, body []byte) (object, error) {
	x, err := c.post(ctx, "/v1/sandboxes", body)
	if err != nil {
		return object{}, err
	}
	if err := x.status(http.StatusCreated); err != nil {
		return object{}, err
	}
	obj, err := x.object()
	if err != nil {
		return obj, err
	}
	if obj.Status.ID == "" {
		return obj, x.disagree("status.id on a created object", "no id")
	}
	e.record(c, "/v1/sandboxes", obj.Status.ID)
	return obj, nil
}

// sandbox is one running sandbox for a case: created, awaited, recorded.
func (e *Env) sandbox(ctx context.Context, c *client, mutate ...func(body map[string]any)) (object, error) {
	obj, err := e.create(ctx, c, e.manifest(e.name(), mutate...))
	if err != nil {
		return obj, err
	}
	return e.await(ctx, c, obj.Status.ID, "Running")
}

// await polls one object until its phase is one of those named or the case's
// deadline passes. It never sleeps for a state.
func (e *Env) await(ctx context.Context, c *client, id string, phases ...string) (object, error) {
	var last object
	want := "phase " + strings.Join(phases, " or ") + " before the case's deadline"
	for {
		x, err := c.get(ctx, "/v1/sandboxes/"+id)
		if err != nil {
			// A deadline that cuts the read is the same fact as one that
			// cuts the poll: the phase did not arrive in time. The report
			// names the phase the case wanted and the last one it saw, not
			// the transport's sentence.
			if ctx.Err() != nil {
				seen := "no answer read"
				if last.Status.Phase != "" {
					seen = "phase " + last.Status.Phase
				}
				return last, &Disagreement{Method: http.MethodGet, Path: "/v1/sandboxes/" + id, Want: want, Got: seen}
			}
			return last, err
		}
		if err := x.status(http.StatusOK); err != nil {
			return last, err
		}
		if last, err = x.object(); err != nil {
			return last, err
		}
		if slices.Contains(phases, last.Status.Phase) {
			return last, nil
		}
		if last.Status.Phase == "Failed" && !slices.Contains(phases, "Failed") {
			return last, x.disagree("phase "+strings.Join(phases, " or "), "phase Failed: "+string(x.Body))
		}
		select {
		case <-ctx.Done():
			return last, x.disagree(want, "phase "+last.Status.Phase)
		case <-time.After(pollInterval):
		}
	}
}

// exec runs one command inside a sandbox and returns the result of the
// bounded form. It is the building block of the cases that need a fact from
// inside the sandbox rather than a fact about the stream.
type execResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"durationMs"`
}

func (e *Env) exec(ctx context.Context, c *client, id string, command ...string) (execResult, *exchange, error) {
	x, err := c.post(ctx, "/v1/sandboxes/"+id+"/exec?wait=1", mustJSON(map[string]any{"command": command}))
	if err != nil {
		return execResult{}, nil, err
	}
	var result execResult
	if x.Status == http.StatusOK {
		if err := json.Unmarshal(x.Body, &result); err != nil {
			return result, x, x.disagree("an exec result as JSON", "a body that does not decode: "+err.Error())
		}
	}
	return result, x, nil
}

// need reports the capability a case requires, or a skip naming it.
func (e *Env) need(capability string) error {
	if len(e.caps) == 0 {
		return skipf("the environment declares no capability set; pass Capabilities to run the %s cases", capability)
	}
	if !e.caps[capability] {
		return skipf("the environment does not declare %s", capability)
	}
	return nil
}

// second is the second subject's client or a skip naming what is missing.
func (e *Env) second() (*client, error) {
	if e.other == nil {
		return nil, skipf("no second subject: set Token so the suite can mint one")
	}
	return e.other, nil
}

// control puts a stub into a failure mode, or clears it with an empty mode.
// The contract is one route, POST <control>/fail with {"mode": "..."}, which
// the tier that owns the stub serves.
func (e *Env) control(ctx context.Context, base, mode string) error {
	body := mustJSON(map[string]string{"mode": mode})
	c := newClient(strings.TrimSuffix(base, "/"), "")
	x, err := c.send(ctx, http.MethodPost, "/fail", request{Body: body, ContentType: "application/json", NoBearer: true})
	if err != nil {
		return err
	}
	if x.Status/100 != 2 {
		return fmt.Errorf("the control endpoint answered %d to mode %q", x.Status, mode)
	}
	return nil
}

// serverVersion is the identity the server reports, for the report's marker.
// A server that serves no version route is reported as unknown rather than
// refused: the marker names what it learned.
func (e *Env) serverVersion(ctx context.Context) string {
	x, err := e.caller.send(ctx, http.MethodGet, "/version", request{NoBearer: true})
	if err != nil || x.Status != http.StatusOK {
		return ""
	}
	var build struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(x.Body, &build) != nil {
		return ""
	}
	return build.Version
}

// cmpOr is the first non-empty of two strings.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// keys is the sorted keys of a map, for a message that names what is there.
func keys[V any](m map[string]V) []string {
	out := slices.Collect(maps.Keys(m))
	slices.Sort(out)
	return out
}
