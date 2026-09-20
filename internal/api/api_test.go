// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

type fixture struct {
	t               *testing.T
	url, alice, bob string
	issuerURL       string
	h               http.Handler
	c               *controller.Controller
	// header is the last response's, for a route whose answer is in them.
	header http.Header
}

func setup(t *testing.T, policy authz.Authorizer) *fixture { return setupDriver(t, policy, nil) }
func setupDriver(t *testing.T, policy authz.Authorizer, wrap func(runtime.Driver) runtime.Driver) *fixture {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{issuer.URL()}, Audience: "cella"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var runtimeDriver runtime.Driver = d
	if wrap != nil {
		runtimeDriver = wrap(d)
	}
	// The activity window is all but open, so a test observes each stamp a
	// handler makes rather than the one the default minute lets through.
	c, err := controller.Open(controller.Options{DataDir: t.TempDir(), Driver: runtimeDriver, Environment: "default", TouchInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if policy == nil {
		policy = &auth.OwnerPolicy{DefaultEnvironment: "default"}
	}
	h, err := New(Options{Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &fixture{t: t, url: server.URL, issuerURL: issuer.URL(), alice: issuer.Mint(issuertest.Claims{Sub: "alice"}), bob: issuer.Mint(issuertest.Claims{Sub: "bob"}), h: h, c: c}
}
func (f *fixture) request(method, path, token, body string, status int) []byte {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if res.StatusCode != status {
		f.t.Fatalf("%s %s: got %d want %d: %s", method, path, res.StatusCode, status, b)
	}
	return b
}

const createBody = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"work","labels":{"team":"a"}},"spec":{}}`

func TestNativeHTTPWorkspaceEndToEnd(t *testing.T) {
	f := setup(t, nil)
	body := f.request("POST", "/v1/sandboxes", f.alice, createBody, 201)
	var obj v1.Sandbox
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Owner == "" || obj.Status.Phase != "Running" || obj.Status.Isolation != "none" {
		t.Fatal(obj)
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	f.request("GET", base, f.alice, "", 200)
	f.request("GET", "/v1/sandboxes/work", f.alice, "", 200)
	f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["sh","-c","printf hello > result; cat result; printf err >&2; exit 7"]}`, 200)
	result := f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["cat","result"]}`, 200)
	var output execResult
	if err := json.Unmarshal(result, &output); err != nil || output.Stdout != "hello" || output.ExitCode != 0 {
		t.Fatal(output, err)
	}
	f.request("POST", base+"/stop", f.alice, "", 200)
	f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["true"]}`, 409)
	f.request("POST", base+"/stop", f.alice, "", 409)
	f.request("POST", base+"/start", f.alice, "", 200)
	result = f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["cat","result"]}`, 200)
	if !bytes.Contains(result, []byte("hello")) {
		t.Fatal(string(result))
	}
	result = f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["sh","-c","sleep 10"],"timeout":"30ms"}`, 200)
	if !bytes.Contains(result, []byte(`"exitCode":124`)) {
		t.Fatal(string(result))
	}
	result = f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["sh","-c","head -c 1100000 /dev/zero | tr '\\000' x; head -c 1100000 /dev/zero | tr '\\000' y >&2"]}`, 200)
	if err := json.Unmarshal(result, &output); err != nil || !output.Truncated || len(output.Stdout) != 1<<20 || len(output.Stderr) != 1<<20 {
		t.Fatalf("bounded output: %d %d %v %v", len(output.Stdout), len(output.Stderr), output.Truncated, err)
	}
	f.request("DELETE", base, f.alice, "", 202)
	f.request("GET", base, f.alice, "", 404)
	f.request("POST", "/v1/sandboxes", f.alice, createBody, 201)
}
func TestAuthorizationCannotCrossOwners(t *testing.T) {
	f := setup(t, nil)
	f.request("POST", "/v1/sandboxes", "", createBody, 401)
	f.request("POST", "/v1/sandboxes", "invalid", createBody, 401)
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID
	for _, tc := range []struct{ method, path, body string }{{"GET", base, ""}, {"DELETE", base, ""}, {"POST", base + "/stop", ""}, {"POST", base + "/start", ""}, {"POST", base + "/exec?wait=1", `{"command":["true"]}`}} {
		f.request(tc.method, tc.path, f.bob, tc.body, 403)
	}
	if b := f.request("GET", "/v1/sandboxes", f.bob, "", 200); !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Fatal(string(b))
	}
	if b := f.request("GET", "/v1/sandboxes?owner=other", f.alice, "", 200); !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Fatal(string(b))
	}
	if b := f.request("GET", "/v1/sandboxes?label=team%3Da&phase=Running&environment=default", f.alice, "", 200); !bytes.Contains(b, []byte(obj.Status.ID)) {
		t.Fatal(string(b))
	}
	f.request("POST", "/v1/sandboxes", f.alice, createBody, 409)
}

type decisionFunc func(context.Context, authz.Request) (authz.Decision, error)

func (f decisionFunc) Authorize(c context.Context, r authz.Request) (authz.Decision, error) {
	return f(c, r)
}
func TestPolicyFiltersAndFailures(t *testing.T) {
	policy := decisionFunc(func(_ context.Context, r authz.Request) (authz.Decision, error) {
		if r.Action == authorizer.ActionSandboxList {
			return authz.Decision{Allow: true, Filter: &authz.Filter{Labels: map[string]string{"team": "b"}}}, nil
		}
		return authz.Decision{Allow: true}, nil
	})
	f := setup(t, policy)
	f.request("POST", "/v1/sandboxes", f.alice, createBody, 201)
	if b := f.request("GET", "/v1/sandboxes?label=team%3Da", f.alice, "", 200); !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Fatal(string(b))
	}
	for _, tc := range []struct {
		name   string
		policy decisionFunc
		status int
	}{
		{"outage", func(context.Context, authz.Request) (authz.Decision, error) {
			return authz.Decision{}, errors.New("offline")
		}, 503},
		{"environment denied", func(_ context.Context, r authz.Request) (authz.Decision, error) {
			return authz.Decision{Allow: r.Action != authorizer.ActionEnvironmentUse}, nil
		}, 404},
		{"unsupported rate limit", func(context.Context, authz.Request) (authz.Decision, error) {
			return authz.Decision{Allow: true, Limits: json.RawMessage(`{"requests_per_minute":10}`)}, nil
		}, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.policy)
			f.request("POST", "/v1/sandboxes", f.alice, createBody, tc.status)
			if len(f.c.List()) != 0 {
				t.Fatal("denial created a runtime")
			}
		})
	}
	f = setup(t, decisionFunc(func(context.Context, authz.Request) (authz.Decision, error) {
		return authz.Decision{Allow: true, Limits: json.RawMessage(`{"max_sandboxes":1}`)}, nil
	}))
	f.request("POST", "/v1/sandboxes", f.alice, createBody, 201)
	f.request("POST", "/v1/sandboxes", f.alice, strings.Replace(createBody, `"work"`, `"other"`, 1), 422)
}
func TestRequestValidation(t *testing.T) {
	f := setup(t, nil)
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/sandboxes", "{", 400}, {"POST", "/v1/sandboxes", strings.Replace(createBody, `"spec":{}`, `"spec":{"scheduling":{"mode":"direct"}}`, 1), 400},
		{"POST", "/v1/sandboxes", strings.Replace(createBody, `"spec":{}`, `"spec":{"image":"ubuntu"}`, 1), 422},
		{"POST", "/v1/sandboxes", strings.Repeat("x", (1<<20)+1), 413}, {"GET", "/v1/sandboxes?limit=0", "", 400}, {"GET", "/v1/sandboxes?limit=201", "", 400}, {"GET", "/v1/sandboxes?limit=no", "", 400}, {"GET", "/v1/sandboxes?label=invalid", "", 400},
		{"POST", base + "/unknown", "", 404}, {"POST", base + "/exec", `{"command":["true"]}`, 422}, {"POST", base + "/exec?wait=1", "{", 400}, {"POST", base + "/exec?wait=1", `{} {}`, 400}, {"POST", base + "/exec?wait=1", `{"unknown":1}`, 400}, {"POST", base + "/exec?wait=1", `{"command":[]}`, 400}, {"POST", base + "/exec?wait=1", `{"command":["true"],"timeout":"bad"}`, 400}, {"POST", base + "/exec?wait=1", `{"command":["true"],"timeout":"0s"}`, 400}, {"POST", base + "/exec?wait=1", `{"command":["true"],"timeout":"2h"}`, 400}, {"POST", base + "/exec?wait=1", strings.Repeat("x", (1<<20)+1), 413},
	} {
		f.request(tc.method, tc.path, f.alice, tc.body, tc.status)
	}
	for _, query := range []string{"?label=team%3Da&label=team%3Db", "?phase=Stopped", "?label=unknown%3D", "?environment=unknown", "?cursor=zzz"} {
		if b := f.request("GET", "/v1/sandboxes"+query, f.alice, "", 200); !bytes.Contains(b, []byte(`"items":[]`)) {
			t.Fatal(string(b))
		}
	}
	f.request("POST", "/v1/sandboxes", f.alice, strings.Replace(createBody, `"work"`, `"second"`, 1), 201)
	var page struct {
		Items []v1.Sandbox `json:"items"`
		Next  string       `json:"next"`
	}
	_ = json.Unmarshal(f.request("GET", "/v1/sandboxes?limit=1", f.alice, "", 200), &page)
	if len(page.Items) != 1 || page.Next == "" {
		t.Fatal(page)
	}
	f.request("GET", "/v1/sandboxes?limit=1&cursor="+page.Next, f.alice, "", 200)
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing options accepted")
	}
}

func TestLifecycleAuthorizesUnchangedProposal(t *testing.T) {
	seen := 0
	f := setup(t, decisionFunc(func(_ context.Context, r authz.Request) (authz.Decision, error) {
		if r.Action == authorizer.ActionSandboxUpdate {
			seen++
			b, err := json.Marshal(r.Resource.Fields["proposed"])
			if err != nil {
				t.Fatal(err)
			}
			var proposed struct {
				Owner    string         `json:"owner"`
				Metadata v1.Metadata    `json:"metadata"`
				Spec     v1.SandboxSpec `json:"spec"`
			}
			if err = json.Unmarshal(b, &proposed); err != nil || proposed.Owner != r.Subject || proposed.Metadata.Name != "work" || proposed.Metadata.Labels["team"] != "a" || proposed.Spec.Environment != "default" {
				t.Errorf("unchanged proposal: %s %v", b, err)
			}
		}
		return authz.Decision{Allow: true}, nil
	}))
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/start", f.alice, "", 200)
	if seen != 2 {
		t.Fatal(seen)
	}
}

type failingExecDriver struct {
	runtime.Driver
	execution *brokenExec
}

func (d failingExecDriver) Exec(context.Context, string, runtime.ExecRequest) (runtime.Exec, error) {
	return d.execution, nil
}

type brokenExec struct{ closed bool }

func (e *brokenExec) Stdout() io.Reader {
	return io.MultiReader(strings.NewReader("partial"), brokenReader{})
}
func (e *brokenExec) Stderr() io.Reader                 { return strings.NewReader("") }
func (e *brokenExec) Wait(context.Context) (int, error) { return 0, nil }
func (e *brokenExec) Close() error                      { e.closed = true; return nil }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestExecStreamFailureCannotReportSuccess(t *testing.T) {
	execution := &brokenExec{}
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return failingExecDriver{d, execution} })
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	b := f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 503)
	if !bytes.Contains(b, []byte("driver_unavailable")) || !execution.closed {
		t.Fatal(string(b), execution.closed)
	}
	f.request("GET", "/v1/sandboxes?root=sbx_unknown", f.alice, "", 422)
}

func TestEnvironmentKeysCannotAccessSandboxRoutes(t *testing.T) {
	var decisions atomic.Int32
	f := setup(t, decisionFunc(func(context.Context, authz.Request) (authz.Decision, error) {
		decisions.Add(1)
		return authz.Decision{Allow: true}, nil
	}))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner(auth.SignerOptions{Issuer: "http://cella.test", Audience: "cella", Keys: []*rsa.PrivateKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{f.issuerURL}, Audience: "cella", LocalIssuer: "http://cella.test", LocalKeys: signer.PublicKeys()})
	if err != nil {
		t.Fatal(err)
	}
	f.h.(*handler).Verifier = verifier
	token, err := signer.MintEnvironmentKey("default", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID
	before := decisions.Load()
	for _, tc := range []struct{ method, path, body string }{{"POST", "/v1/sandboxes", strings.Replace(createBody, `"work"`, `"worker-owned"`, 1)}, {"GET", "/v1/sandboxes", ""}, {"GET", base, ""}, {"DELETE", base, ""}, {"POST", base + "/stop", ""}, {"POST", base + "/start", ""}, {"POST", base + "/exec?wait=1", `{"command":["true"]}`}, {"GET", base + "/files", ""}, {"PUT", base + "/files?dest=/workspace", ""}, {"GET", base + "/logs", ""}} {
		f.request(tc.method, tc.path, token.Value, tc.body, 403)
	}
	if decisions.Load() != before || len(f.c.List()) != 1 {
		t.Fatal("worker reached ordinary authorization or runtime")
	}
	f.request("GET", base, f.alice, "", 200)
}

const sizedBody = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"sized"},"spec":{"user":"1000","resources":{"cpu":"500m","memory":"2Gi","disk":"10Gi"},"workspace":{"path":"/workspace"},"lifecycle":{"autoStop":"15m","ttl":"1h","autoDelete":"never"}}}`

func TestNativeManifestFieldsEndToEnd(t *testing.T) {
	f := setup(t, nil)
	var created v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, sizedBody, 201), &created); err != nil {
		t.Fatal(err)
	}
	if created.Spec.User != "1000" || created.Spec.Resources.CPU != "500m" || created.Spec.Resources.Memory != "2Gi" || created.Spec.Resources.Disk != "10Gi" {
		t.Fatalf("spec = %+v", created.Spec)
	}
	if created.Spec.Workspace.Path != "/workspace" || created.Spec.Workspace.Source != v1.WorkspaceSourceEmpty {
		t.Fatalf("workspace = %+v", created.Spec.Workspace)
	}
	if created.Spec.Lifecycle != (v1.Lifecycle{AutoStop: "15m", TTL: "1h", AutoDelete: v1.DurationNever}) {
		t.Fatalf("lifecycle = %+v", created.Spec.Lifecycle)
	}
	// The native environment records what it cannot enforce and says so.
	want := []string{manifest.WarningResourcesNotEnforced, manifest.WarningUserNotApplied}
	if !slices.Equal(created.Status.Warnings, want) {
		t.Fatalf("warnings = %v, want %v", created.Status.Warnings, want)
	}
	if expiry := created.Status.CreatedAt.Add(time.Hour); created.Status.ExpiresAt.Sub(expiry).Abs() > time.Minute {
		t.Fatalf("expiresAt = %v, want about %v", created.Status.ExpiresAt, expiry)
	}
	var read v1.Sandbox
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/"+created.Status.ID, f.alice, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.Status.Warnings, want) || !read.Status.ExpiresAt.Equal(created.Status.ExpiresAt) {
		t.Fatalf("read back %+v", read.Status)
	}
	// A manifest with no name resolves without one and the controller names
	// the object, the API's own name generator not being wired yet.
	var unnamed v1.Sandbox
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"resources":{"cpu":"1"}}}`
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &unnamed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(unnamed.Metadata.Name, "sandbox-") || unnamed.Spec.Resources.CPU != "1" {
		t.Fatalf("unnamed create = %+v", unnamed.Metadata)
	}
	// Each refusal of the new fields is the caller's error, with its code.
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"quantity", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"resources":{"cpu":"one"}}}`, "invalid_field", 400},
		{"duration", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"lifecycle":{"ttl":"soon"}}}`, "invalid_field", 400},
		{"idle stop past the life", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"lifecycle":{"autoStop":"2h","ttl":"1h"}}}`, "invalid_field", 400},
		{"reserved label", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad","labels":{"pool.cella.latere.ai/owner":"evil"}},"spec":{}}`, "reserved_prefix", 400},
		{"workspace source", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"workspace":{"source":"git"}}}`, "capability_unsupported", 422},
		{"workspace path", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"workspace":{"path":"/srv/work"}}}`, "capability_unsupported", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body struct{ Error struct{ Code string } }
			if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, tc.body, tc.status), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", body.Error.Code, tc.code)
			}
		})
	}
}

// TestResolverErrorStatuses holds the codes the resolver raises on an update
// or under an operator's policy to the statuses the API contract gives them.
func TestResolverErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{&manifest.Error{Code: "immutable_field", Detail: "spec.image changed", Paths: []string{"spec.image", "spec.env"}}, 409},
		{&manifest.Error{Code: "ceiling_exceeded", Detail: "cpu is above the ceiling"}, 422},
		{&manifest.Error{Code: "admission_refused", Detail: "the policy refused"}, 422},
	} {
		w := httptest.NewRecorder()
		respondError(w, tc.err)
		if w.Code != tc.status {
			t.Fatalf("%v: status = %d, want %d", tc.err, w.Code, tc.status)
		}
		var body struct {
			Error struct {
				Code    string
				Details struct {
					Paths []string
				}
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var known *manifest.Error
		if !errors.As(tc.err, &known) || body.Error.Code != known.Code {
			t.Fatalf("code = %q, want %q", body.Error.Code, known.Code)
		}
		if !slices.Equal(body.Error.Details.Paths, known.Paths) {
			t.Fatalf("paths = %v, want %v", body.Error.Details.Paths, known.Paths)
		}
	}
}
