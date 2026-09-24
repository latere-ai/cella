// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// call is one request the fixture server saw, which the assertions read.
type call struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

// fixture is a server speaking the envelopes of design 008 and the client
// pointed at it. The handler is the test's own, so a case states the exact
// answer it holds the client to.
type fixture struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	calls  []call
}

func newFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.calls = append(f.calls, call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: string(body)})
		f.mu.Unlock()
		w.Header().Set("X-Request-ID", "req_fromtheserver")
		handler(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// client builds a client against the fixture with the configuration a case
// names.
func (f *fixture) client(cfg client.Config) *client.Client {
	f.t.Helper()
	if cfg.URL == "" {
		cfg.URL = f.server.URL
	}
	if cfg.Token == nil {
		cfg.Token = client.StaticToken("caller-token")
	}
	c, err := client.New(cfg)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return c
}

func (f *fixture) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// last is the request the server saw last.
func (f *fixture) last() call {
	f.t.Helper()
	calls := f.seen()
	if len(calls) == 0 {
		f.t.Fatal("the server saw no request")
	}
	return calls[len(calls)-1]
}

// writeObject answers with one Sandbox, which is what most routes answer.
func writeObject(w http.ResponseWriter, status int, name, id string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"` + name + `"},"spec":{"image":"example/image:1"},"status":{"id":"` + id + `","phase":"Running","owner":"alice"}}`))
}

// writeError answers with the envelope of design 008.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	if _, ok := details["request_id"]; !ok {
		details["request_id"] = "req_fromtheserver"
	}
	httpjson.WriteError(w, status, httpjson.Error{Code: code, Message: message, Details: details})
}

// env is a getenv over a map, so a case states the whole environment
// Environment reads.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestTheAddressAndTheBearerComeFromTheEnvironment is design 011's reaching
// rule read by Environment: the address from its variable, and the bearer
// from the token variable, else the file variable, else the projection.
func TestTheAddressAndTheBearerComeFromTheEnvironment(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{{
		name: "the token variable wins over the file",
		vars: map[string]string{"CELLA_TOKEN": "from-the-variable", "CELLA_TOKEN_FILE": file},
		want: "from-the-variable",
	}, {
		name: "the file variable is read when no token is set",
		vars: map[string]string{"CELLA_TOKEN_FILE": file},
		want: "from-the-file",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.vars["CELLA_URL"] = f.server.URL
			c, err := client.New(client.Environment(env(tc.vars)))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = c.GetSandbox(t.Context(), "dev"); err != nil {
				t.Fatal(err)
			}
			if got := f.last().Header.Get("Authorization"); got != "Bearer "+tc.want {
				t.Fatalf("the request carried %q, want the bearer %q", got, tc.want)
			}
		})
	}
	if cfg := client.Environment(env(map[string]string{"CELLA_URL": "https://cella.example.com"})); cfg.URL != "https://cella.example.com" {
		t.Errorf("the address of %s read as %q", client.URLEnv, cfg.URL)
	}
	// A nil getenv is the process's own environment.
	t.Setenv(client.URLEnv, "https://from-the-process.example.com")
	if cfg := client.Environment(nil); cfg.URL != "https://from-the-process.example.com" {
		t.Errorf("the process's environment read as %q", cfg.URL)
	}
}

// TestTheTokenFileIsReadPerRequest is design 011's rule for a workload: the
// controller re-projects the token before it expires, and a client that read
// the file once would outlive its own bearer.
func TestTheTokenFileIsReadPerRequest(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := f.client(client.Config{Token: client.TokenFile(file)})
	if _, _, err := c.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	calls := f.seen()
	if len(calls) != 2 {
		t.Fatalf("the server saw %d requests, want 2", len(calls))
	}
	if got, want := calls[0].Header.Get("Authorization"), "Bearer first"; got != want {
		t.Errorf("the first request carried %q, want %q", got, want)
	}
	if got, want := calls[1].Header.Get("Authorization"), "Bearer second"; got != want {
		t.Errorf("the second request carried %q, want %q; the file is read per request", got, want)
	}
}

// TestATokenSourceIsAskedPerRequestWithTheCallersContext: a caller with its
// own issuer hands each request the token of that moment, and the source
// sees the request's own context, so a refresh it makes is bounded by the
// call. A nil source sends no bearer, and a source that fails sends nothing.
func TestATokenSourceIsAskedPerRequestWithTheCallersContext(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	type key struct{}
	asked := 0
	source := client.TokenFunc(func(ctx context.Context) (string, error) {
		if ctx.Value(key{}) != "the call's" {
			t.Errorf("the source was asked with a context that is not the call's")
		}
		asked++
		return "minted-" + strconv.Itoa(asked), nil
	})
	c := f.client(client.Config{Token: source})
	ctx := context.WithValue(t.Context(), key{}, "the call's")
	for range 2 {
		if _, _, err := c.GetSandbox(ctx, "dev"); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.seen()
	if got := calls[1].Header.Get("Authorization"); asked != 2 || got != "Bearer minted-2" {
		t.Fatalf("the source was asked %d times and the second request carried %q", asked, got)
	}

	// A source that yields nothing, and no source at all, send no header.
	for name, source := range map[string]client.TokenSource{
		"an empty token": client.StaticToken(""),
		"no source":      nil,
	} {
		c, err := client.New(client.Config{URL: f.server.URL, Token: source})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = c.GetSandbox(t.Context(), "dev"); err != nil {
			t.Fatal(err)
		}
		if got := f.last().Header.Get("Authorization"); got != "" {
			t.Errorf("%s sent the header %q", name, got)
		}
	}

	failing := errors.New("the issuer is down")
	before := len(f.seen())
	c = f.client(client.Config{Token: client.TokenFunc(func(context.Context) (string, error) { return "", failing })})
	if _, _, err := c.GetSandbox(t.Context(), "dev"); !errors.Is(err, failing) {
		t.Fatalf("a failing source ended the call with %v", err)
	}
	if _, err := c.ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}}); !errors.Is(err, failing) {
		t.Fatalf("a failing source ended the socket with %v", err)
	}
	if len(f.seen()) != before {
		t.Fatal("a request went out without the bearer its source failed to yield")
	}
}

// TestATokenFileThatYieldsNothingIsNoBearer: a missing file and an empty one
// are the caller's configuration, reported as NoBearer with the path, and no
// request is sent without the bearer.
func TestATokenFileThatYieldsNothingIsNoBearer(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	missing := filepath.Join(t.TempDir(), "absent")
	c := f.client(client.Config{Token: client.TokenFile(missing)})
	_, _, err := c.GetSandbox(t.Context(), "dev")
	var none *client.NoBearer
	if !errors.As(err, &none) || none.Path != missing || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing token file failed with %v", err)
	}
	empty := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = f.client(client.Config{Token: client.TokenFile(empty)})
	if _, _, err = c.GetSandbox(t.Context(), "dev"); !errors.As(err, &none) || none.Err != nil {
		t.Fatalf("an empty token file failed with %v", err)
	}
	if len(f.seen()) != 0 {
		t.Fatal("a client with no bearer sent a request")
	}
}

// TestTheDefaultTokenFileIsTheProjection: inside a sandbox nothing needs
// configuring, because the file the driver projects is the last source
// Environment reads.
func TestTheDefaultTokenFileIsTheProjection(t *testing.T) {
	if client.DefaultTokenPath != "/run/cella/token" {
		t.Fatalf("the default token file is %q; design 045 projects /run/cella/token", client.DefaultTokenPath)
	}
	c, err := client.New(client.Environment(env(map[string]string{"CELLA_URL": "http://127.0.0.1:1"})))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is projected here, so the failure names the file it tried.
	var none *client.NoBearer
	if _, _, err = c.GetSandbox(t.Context(), "dev"); !errors.As(err, &none) || none.Path != client.DefaultTokenPath {
		t.Fatalf("a client with nothing projected failed with %v", err)
	}
}

// TestABadConfigurationIsRefusedAtOnce: the address is read when the client
// is built, so a mistake is reported before a call.
func TestABadConfigurationIsRefusedAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  client.Config
		want string
	}{
		{"no address", client.Config{}, "no control plane address"},
		{"an address that is no URL", client.Config{URL: "://nowhere"}, "no http or https address"},
		{"an address with no host", client.Config{URL: "https://"}, "no http or https address"},
		{"an address of another scheme", client.Config{URL: "ftp://example.com"}, "no http or https address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() = %v, want a failure naming %q", err, tc.want)
			}
		})
	}
}

// TestTheClientIgnoresProxyVariables is design 011's transport rule: a
// sandbox reaches the control plane directly, and the gateway's map has no
// entry for it, so a proxy variable in the environment is not read.
func TestTheClientIgnoresProxyVariables(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	c := f.client(client.Config{})
	if _, _, err := c.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatalf("the client went through the proxy the environment named: %v", err)
	}
}

// TestEveryRequestCarriesAFreshIdentity: the user agent design 011 fixes and
// a request id inside the rule of design 008, different per request.
func TestEveryRequestCarriesAFreshIdentity(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	c := f.client(client.Config{UserAgent: "cella/v9.9.9"})
	for range 2 {
		if _, _, err := c.GetSandbox(t.Context(), "dev"); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.seen()
	first, second := calls[0].Header.Get("X-Request-Id"), calls[1].Header.Get("X-Request-Id")
	if first == "" || first == second {
		t.Errorf("the request ids are %q and %q; each request carries a fresh one", first, second)
	}
	if !strings.HasPrefix(first, "req_") || len(first) > 128 {
		t.Errorf("the request id %q is outside the rule of design 008", first)
	}
	if got := calls[0].Header.Get("User-Agent"); got != "cella/v9.9.9" {
		t.Errorf("the request carried the user agent %q", got)
	}
}

// TestARefusalDecodesToTheEnvelope is design 008's error table read from the
// client's side: the code a caller decides on, the fixed sentence, the
// server's own request id, and the paths a field error names.
func TestARefusalDecodesToTheEnvelope(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeError(w, 409, "immutable_field", "This field cannot be changed after the object is created.",
			map[string]any{"paths": []any{"spec.image"}, "detail": "image changed", "request_id": "req_theservers", "current": float64(7)})
	})
	c := f.client(client.Config{})
	_, _, err := c.GetSandbox(t.Context(), "dev")
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal is %T: %v", err, err)
	}
	if refusal.Status != 409 || refusal.Code != "immutable_field" {
		t.Errorf("the refusal is %d %s", refusal.Status, refusal.Code)
	}
	if refusal.Message != "This field cannot be changed after the object is created." {
		t.Errorf("the sentence is %q and design 008 fixes it", refusal.Message)
	}
	if refusal.RequestID != "req_theservers" {
		t.Errorf("the request id is %q, want the server's own", refusal.RequestID)
	}
	if strings.Join(refusal.Paths, ",") != "spec.image" {
		t.Errorf("the paths are %v", refusal.Paths)
	}
	if refusal.Detail != "image changed" || refusal.RetryAfter != "30" {
		t.Errorf("the detail is %q and Retry-After is %q", refusal.Detail, refusal.RetryAfter)
	}
	// The details reach the caller whole, a member this client names no
	// field for included.
	if refusal.Details["current"] != float64(7) || refusal.Details["request_id"] != "req_theservers" {
		t.Errorf("the details are %v", refusal.Details)
	}
	if client.CodeOf(err) != "immutable_field" {
		t.Errorf("CodeOf() = %q", client.CodeOf(err))
	}
	if client.CodeOf(nil) != "" {
		t.Error("CodeOf() names a code for no error")
	}
}

// TestABodyThatIsNoEnvelopeStillHasAStatus: a listener that is not this API
// answers something else, and a client that only understood the envelope
// would have no exit for it.
func TestABodyThatIsNoEnvelopeStillHasAStatus(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>a proxy</html>"))
	})
	c := f.client(client.Config{})
	_, _, err := c.GetSandbox(t.Context(), "dev")
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("the failure is %T: %v", err, err)
	}
	if refusal.Status != 502 || refusal.Code != "" {
		t.Fatalf("the failure is %d %q, want the status and no code", refusal.Status, refusal.Code)
	}
	if refusal.RequestID != "req_fromtheserver" {
		t.Errorf("the request id is %q, want the response header's", refusal.RequestID)
	}
	// An empty body falls back to the status text, so a refusal always has a
	// sentence to print.
	empty := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	if _, _, err = empty.client(client.Config{}).GetSandbox(t.Context(), "dev"); err == nil || err.Error() == "" {
		t.Fatalf("a refusal with no body prints %q", err)
	}
}

// TestAServerThatIsNotThereIsUnreachable: the exit scheme separates a
// refusal from an address nothing answers, so the client does too.
func TestAServerThatIsNotThereIsUnreachable(t *testing.T) {
	c, err := client.New(client.Config{URL: "http://127.0.0.1:1", Token: client.StaticToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.GetSandbox(t.Context(), "dev")
	var gone *client.Unreachable
	if !errors.As(err, &gone) {
		t.Fatalf("the failure is %T: %v", err, err)
	}
	if gone.Unwrap() == nil || gone.Error() == "" {
		t.Error("the failure carries neither a cause nor a sentence")
	}
}

// TestTheRoutesOfEveryObjectCall holds each method to the route of its row
// in design 008, and a manifest's body to the media type it was written in.
func TestTheRoutesOfEveryObjectCall(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/secrets"):
			_, _ = w.Write([]byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"api"},"spec":{"kind":"static"},"status":{"id":"sec_1","owner":"alice","version":2}}`))
		case strings.HasPrefix(r.URL.Path, "/v1/environments"):
			_, _ = w.Write([]byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"gpu"},"spec":{"mode":"workers"},"status":{"id":"gpu","phase":"Ready"}}`))
		default:
			writeObject(w, 200, "dev", "sbx_1")
		}
	})
	c := f.client(client.Config{})
	ctx := t.Context()
	sandbox := client.JSON([]byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"dev"},"spec":{}}`))
	secret := client.YAML([]byte("apiVersion: " + v1.APIVersion + "\nkind: Secret\nmetadata:\n  name: api\n"))
	environment := client.Manifest{Body: []byte(`{"kind":"Environment"}`)}
	for _, tc := range []struct {
		name         string
		run          func() error
		method, path string
		media        string
	}{
		{"create a sandbox", func() error { _, _, err := c.CreateSandbox(ctx, sandbox); return err }, "POST", "/v1/sandboxes", client.MediaJSON},
		{"apply a sandbox", func() error { _, _, err := c.ApplySandbox(ctx, "dev", sandbox); return err }, "PUT", "/v1/sandboxes/dev", client.MediaJSON},
		{"create a secret", func() error { _, _, err := c.CreateSecret(ctx, secret); return err }, "POST", "/v1/secrets", client.MediaYAML},
		{"apply a secret", func() error { _, _, err := c.ApplySecret(ctx, "api", secret); return err }, "PUT", "/v1/secrets/api", client.MediaYAML},
		{"create an environment", func() error { _, _, err := c.CreateEnvironment(ctx, environment); return err }, "POST", "/v1/environments", client.MediaJSON},
		{"apply an environment", func() error { _, _, err := c.ApplyEnvironment(ctx, "gpu", environment); return err }, "PUT", "/v1/environments/gpu", client.MediaJSON},
		{"get a sandbox", func() error { _, _, err := c.GetSandbox(ctx, "sbx_1"); return err }, "GET", "/v1/sandboxes/sbx_1", ""},
		{"get a secret", func() error { _, _, err := c.GetSecret(ctx, "api"); return err }, "GET", "/v1/secrets/api", ""},
		{"get an environment", func() error { _, _, err := c.GetEnvironment(ctx, "gpu"); return err }, "GET", "/v1/environments/gpu", ""},
		{"delete a sandbox", func() error { _, err := c.Delete(ctx, client.KindSandbox, "dev"); return err }, "DELETE", "/v1/sandboxes/dev", ""},
		{"delete a secret", func() error { _, err := c.Delete(ctx, client.KindSecret, "api"); return err }, "DELETE", "/v1/secrets/api", ""},
		{"delete an environment", func() error { _, err := c.Delete(ctx, client.KindEnvironment, "gpu"); return err }, "DELETE", "/v1/environments/gpu", ""},
		{"start", func() error { _, _, err := c.StartSandbox(ctx, "dev"); return err }, "POST", "/v1/sandboxes/dev/start", ""},
		{"stop", func() error { _, _, err := c.StopSandbox(ctx, "dev"); return err }, "POST", "/v1/sandboxes/dev/stop", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err != nil {
				t.Fatal(err)
			}
			got := f.last()
			if got.Method != tc.method || got.Path != tc.path {
				t.Fatalf("the call was %s %s, want %s %s", got.Method, got.Path, tc.method, tc.path)
			}
			if media := got.Header.Get("Content-Type"); media != tc.media {
				t.Fatalf("the call carried the media type %q, want %q", media, tc.media)
			}
		})
	}
}

// TestAManifestTravelsInItsOwnSyntax: the body is the caller's bytes
// unchanged, under the media type that names its syntax, and a typed object
// is encoded as JSON.
func TestAManifestTravelsInItsOwnSyntax(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 201, "dev", "sbx_1") })
	c := f.client(client.Config{})
	yaml := "apiVersion: " + v1.APIVersion + "\nkind: Sandbox\n# a comment the server reads past\nspec: {}\n"
	obj, raw, err := c.ApplySandbox(t.Context(), "dev", client.YAML([]byte(yaml)))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Body != yaml || got.Header.Get("Content-Type") != client.MediaYAML || got.Path != "/v1/sandboxes/dev" {
		t.Fatalf("the apply sent %s %q under %q", got.Path, got.Body, got.Header.Get("Content-Type"))
	}
	if obj.Status.ID != "sbx_1" || !strings.Contains(string(raw), `"id":"sbx_1"`) {
		t.Fatalf("the answer decoded as %+v from %s", obj, raw)
	}

	typed := v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "dev"}}
	m, err := client.Encode(typed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.CreateSandbox(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	var sent v1.Sandbox
	if got := f.last(); got.Header.Get("Content-Type") != client.MediaJSON || json.Unmarshal([]byte(got.Body), &sent) != nil || sent.Metadata.Name != "dev" {
		t.Fatalf("the typed object was sent as %q under %q", got.Body, got.Header.Get("Content-Type"))
	}
	if _, err = client.Encode(func() {}); err == nil {
		t.Fatal("a value JSON cannot encode became a manifest")
	}
}

// TestAnAnswerThatIsNotTheObjectIsReported: a route that answers something
// else is a failure the caller reads, not a zero object.
func TestAnAnswerThatIsNotTheObjectIsReported(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	c := f.client(client.Config{})
	if _, _, err := c.GetSandbox(t.Context(), "dev"); err == nil {
		t.Fatal("a list decoded as one object")
	}
	if _, _, err := c.ListSandboxes(t.Context(), client.ListOptions{}); err == nil {
		t.Fatal("a list answer that is no page was accepted")
	}
	bad := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"items":[1],"next":""}`)) })
	if _, _, err := bad.client(client.Config{}).ListSandboxes(t.Context(), client.ListOptions{}); err == nil {
		t.Fatal("a page holding no object was accepted")
	}
}

// TestAListFollowsTheCursorAndCarriesTheSelectors is design 008's list
// grammar: every selector becomes its query parameter, a page is asked for
// at most at the ceiling, and the cursor is followed to the end.
func TestAListFollowsTheCursorAndCarriesTheSelectors(t *testing.T) {
	pages := map[string]string{
		"":      `{"items":[{"metadata":{"name":"a"},"status":{"id":"sbx_1"}}],"next":"sbx_1"}`,
		"sbx_1": `{"items":[{"metadata":{"name":"b"},"status":{"id":"sbx_2"}}],"next":""}`,
	}
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pages[r.URL.Query().Get("cursor")]))
	})
	c := f.client(client.Config{})
	items, raws, err := c.ListSandboxes(t.Context(), client.ListOptions{
		Labels: []string{"team=core", "tier=dev"}, Phase: "Running", Owner: "alice", Environment: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Metadata.Name != "a" || items[1].Metadata.Name != "b" {
		t.Fatalf("the list holds %d object(s): %+v", len(items), items)
	}
	if len(raws) != 2 || !strings.Contains(string(raws[1]), `"name":"b"`) {
		t.Fatalf("the items' own bytes are %q", raws)
	}
	first := f.seen()[0]
	if got := first.Query["label"]; strings.Join(got, ",") != "team=core,tier=dev" {
		t.Errorf("the label selectors are %v", got)
	}
	for key, want := range map[string]string{"phase": "Running", "owner": "alice", "environment": "default", "limit": "200"} {
		if got := first.Query.Get(key); got != want {
			t.Errorf("the %s selector is %q, want %q", key, got, want)
		}
	}
	if got := f.seen()[1].Query.Get("cursor"); got != "sbx_1" {
		t.Errorf("the second page asked for cursor %q", got)
	}
}

// TestALimitStopsTheListAndNeverAsksPastTheCeiling: design 008 refuses a
// page above 200, so a caller that wants more pages instead.
func TestALimitStopsTheListAndNeverAsksPastTheCeiling(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "300" {
			writeError(w, 400, "invalid_field", "A field has a value it cannot take.", nil)
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"status":{"id":"sbx_1"}},{"status":{"id":"sbx_2"}}],"next":"sbx_2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"status":{"id":"sbx_3"}}],"next":""}`))
	})
	c := f.client(client.Config{})
	items, _, err := c.ListSandboxes(t.Context(), client.ListOptions{Limit: 300})
	if err != nil {
		t.Fatalf("a limit above the ceiling was sent to the server: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("the list holds %d object(s)", len(items))
	}
	if got := f.seen()[0].Query.Get("limit"); got != "200" {
		t.Errorf("the first page asked for limit %q, and design 008 refuses more than 200", got)
	}
	// One object wanted is one object returned, and the cursor is not
	// followed past it.
	one, _, err := c.ListSandboxes(t.Context(), client.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("a limit of one returned %d objects", len(one))
	}
}

// TestAPageThatRepeatsItselfEndsTheList: a server answering a cursor with an
// empty page ends the walk rather than looping.
func TestAPageThatRepeatsItselfEndsTheList(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"next":"sbx_9"}`))
	})
	items, _, err := f.client(client.Config{}).ListSandboxes(t.Context(), client.ListOptions{})
	if err != nil || len(items) != 0 {
		t.Fatalf("the list returned %d item(s), %v", len(items), err)
	}
}

// TestExecIsTheSynchronousRoute: without input or a terminal, design 008's
// ?wait=1 answer, whose fields the command prints.
func TestExecIsTheSynchronousRoute(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"exitCode":3,"stdout":"out","stderr":"err","truncated":true,"durationMs":12}`))
	})
	c := f.client(client.Config{})
	result, raw, err := c.Exec(t.Context(), "dev", client.ExecRequest{
		Command: []string{"sh", "-c", "exit 3"}, Env: map[string]string{"K": "V"}, Workdir: "/workspace", Timeout: "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 3 || result.Stdout != "out" || result.Stderr != "err" || !result.Truncated || result.DurationMS != 12 {
		t.Fatalf("the result is %+v", result)
	}
	if !strings.Contains(string(raw), `"exitCode":3`) {
		t.Errorf("the answer's own bytes are %q", raw)
	}
	seen := f.last()
	if seen.Method != "POST" || seen.Path != "/v1/sandboxes/dev/exec" || seen.Query.Get("wait") != "1" {
		t.Fatalf("the call was %s %s?%s", seen.Method, seen.Path, seen.Query.Encode())
	}
	var sent map[string]any
	if err = json.Unmarshal([]byte(seen.Body), &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["cols"]; ok {
		t.Error("the synchronous route was sent a window, which it refuses as an unknown field")
	}
	if sent["workdir"] != "/workspace" || sent["timeout"] != "5s" {
		t.Errorf("the body is %s", seen.Body)
	}
}

// TestTheEgressRecordsAreTheGatewaysRows: one sandbox's connections, newest
// first, with the limit the caller asked for.
func TestTheEgressRecordsAreTheGatewaysRows(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"principal":"sbx_1","at":"2026-09-20T10:00:00Z","door":"proxy","host":"api.example.com","port":443,"decision":"allow","substituted":["api"]}]}`))
	})
	c := f.client(client.Config{})
	records, raw, err := c.EgressRecords(t.Context(), "dev", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Host != "api.example.com" || records[0].Decision != "allow" {
		t.Fatalf("the records are %+v", records)
	}
	if !strings.Contains(string(raw), "api.example.com") {
		t.Errorf("the answer's own bytes are %q", raw)
	}
	if got := f.last().Query.Get("limit"); got != "5" {
		t.Errorf("the call asked for limit %q", got)
	}
	bad := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`"records"`)) })
	if _, _, err = bad.client(client.Config{}).EgressRecords(t.Context(), "dev", 0); err == nil {
		t.Fatal("an answer that is no record list was accepted")
	}
}

// TestTheServerIdentityNeedsNoBearer: /version is outside /v1 and is what
// `cella version` reads to report the skew.
func TestTheServerIdentityNeedsNoBearer(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("the identity was asked for at %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"version":"v1.2.3","commit":"abc1234","buildTime":"2026-09-20"}`))
	})
	c := f.client(client.Config{Token: client.TokenFile(filepath.Join(t.TempDir(), "absent"))})
	build, err := c.ServerVersion(t.Context())
	if err != nil {
		t.Fatalf("the identity was not read without a bearer: %v", err)
	}
	if build.Version != "v1.2.3" || build.Commit != "abc1234" || build.BuildTime != "2026-09-20" {
		t.Fatalf("the identity is %+v", build)
	}
	if got := f.last().Header.Get("Authorization"); got != "" {
		t.Errorf("the identity call carried %q", got)
	}
	bad := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) })
	if _, err = bad.client(client.Config{}).ServerVersion(t.Context()); err == nil {
		t.Fatal("an answer that is no identity was accepted")
	}
}

// TestEveryKindNamesItsCollection: a kind's routes are its plural under /v1,
// and its manifest kind is the name a document declares.
func TestEveryKindNamesItsCollection(t *testing.T) {
	for _, tc := range []struct {
		kind         client.Kind
		plural, path string
		manifestKind string
	}{
		{client.KindSandbox, "sandboxes", "/v1/sandboxes", "Sandbox"},
		{client.KindSecret, "secrets", "/v1/secrets", "Secret"},
		{client.KindEnvironment, "environments", "/v1/environments", "Environment"},
	} {
		if tc.kind.Plural() != tc.plural || tc.kind.Path() != tc.path || tc.kind.ManifestKind() != tc.manifestKind {
			t.Errorf("%s names %q, %q and %q", tc.kind, tc.kind.Plural(), tc.kind.Path(), tc.kind.ManifestKind())
		}
	}
	if client.Kind("").ManifestKind() != "" {
		t.Error("no kind declared a manifest kind")
	}
}

// TestACancelledContextIsTheCallersOwn: a caller that stopped waiting reads
// its own cancellation and not a server that was not there.
func TestACancelledContextIsTheCallersOwn(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		writeObject(w, 200, "dev", "sbx_1")
	})
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	c := f.client(client.Config{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := c.GetSandbox(ctx, "dev")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the failure is %v, want the caller's cancellation", err)
	}
}

// TestSecretsListLikeSandboxesDo: one grammar per kind, and no answer
// carries a value.
func TestSecretsListLikeSandboxesDo(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"api"},"spec":{"kind":"static"},"status":{"id":"sec_1","version":2}}],"next":""}`))
	})
	items, raws, err := f.client(client.Config{}).ListSecrets(t.Context(), client.ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "api" || items[0].Status.Version != 2 {
		t.Fatalf("the list holds %+v", items)
	}
	if strings.Contains(string(raws[0]), "value") {
		t.Fatalf("a secret's answer carries a value: %s", raws[0])
	}
	seen := f.last()
	if seen.Path != "/v1/secrets" || seen.Query.Get("limit") != "10" {
		t.Fatalf("the list called %s?%s", seen.Path, seen.Query.Encode())
	}
}

// TestWritingToASessionThatEndedFails: the input pump reports a connection
// that is gone rather than dropping what a caller typed.
func TestWritingToASessionThatEndedFails(t *testing.T) {
	f := newSocketFixture(t, func(_ *socketFixture, conn *websocket.Conn) {
		exit(conn, 0)
	})
	s, err := f.client().ExecSession(t.Context(), "dev", client.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err = s.Write([]byte("typed")); err == nil {
		t.Error("a write to a session that ended reported success")
	}
	if err = s.Resize(80, 24); err == nil {
		t.Error("a resize on a session that ended reported success")
	}
}
