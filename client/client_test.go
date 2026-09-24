// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	if cfg.Token == "" && cfg.TokenFile == "" && cfg.Getenv == nil {
		cfg.Token = "caller-token"
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

// env is a Getenv over a map, so a case states the whole environment the
// client reads.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestTheAddressAndTheBearerComeFromTheEnvironment is design 011's reaching
// rule: the address and the token are read from the two variables, a flag
// overrides each, and neither is a configuration file or a login.
func TestTheAddressAndTheBearerComeFromTheEnvironment(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cfg  client.Config
		want string
	}{{
		name: "the flag wins over everything",
		cfg: client.Config{Token: "from-the-flag", TokenFile: file,
			Getenv: env(map[string]string{"CELLA_TOKEN": "from-the-variable"})},
		want: "from-the-flag",
	}, {
		name: "the variable wins over the file",
		cfg: client.Config{TokenFile: file,
			Getenv: env(map[string]string{"CELLA_TOKEN": "from-the-variable"})},
		want: "from-the-variable",
	}, {
		name: "the file flag is read when neither is set",
		cfg:  client.Config{TokenFile: file, Getenv: env(nil)},
		want: "from-the-file",
	}, {
		name: "the file variable is read when no flag names one",
		cfg:  client.Config{Getenv: env(map[string]string{"CELLA_TOKEN_FILE": file})},
		want: "from-the-file",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg.URL == "" {
				cfg.URL = f.server.URL
			}
			c, err := client.New(cfg)
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
	// The address comes from the variable when no flag names one.
	c, err := client.New(client.Config{Getenv: env(map[string]string{
		"CELLA_URL": f.server.URL, "CELLA_TOKEN": "from-the-variable",
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatalf("the address of %s was not read: %v", client.URLEnv, err)
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
	c := f.client(client.Config{TokenFile: file, Getenv: env(nil)})
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

// TestNoBearerIsNamedByItsVariables: a client with no token anywhere names
// what to set rather than sending a request without one.
func TestNoBearerIsNamedByItsVariables(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeObject(w, 200, "dev", "sbx_1") })
	c := f.client(client.Config{TokenFile: filepath.Join(t.TempDir(), "absent"), Getenv: env(nil)})
	_, _, err := c.GetSandbox(t.Context(), "dev")
	if err == nil {
		t.Fatal("a client with no bearer sent a request")
	}
	if !strings.Contains(err.Error(), client.TokenEnv) || !strings.Contains(err.Error(), client.TokenFileEnv) {
		t.Fatalf("the failure is %q, and it names neither variable", err)
	}
	empty := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = f.client(client.Config{TokenFile: empty, Getenv: env(nil)})
	if _, _, err = c.GetSandbox(t.Context(), "dev"); err == nil {
		t.Fatal("an empty token file was sent as a bearer")
	}
}

// TestTheDefaultTokenFileIsTheProjection: inside a sandbox the command needs
// no flag, because the file the driver projects is the default.
func TestTheDefaultTokenFileIsTheProjection(t *testing.T) {
	if client.DefaultTokenPath != "/run/cella/token" {
		t.Fatalf("the default token file is %q; design 045 projects /run/cella/token", client.DefaultTokenPath)
	}
	c, err := client.New(client.Config{URL: "http://127.0.0.1:1", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is projected here, so the failure names the file it tried.
	if _, _, err = c.GetSandbox(t.Context(), "dev"); err == nil {
		t.Fatal("a request was sent with no token")
	}
}

// TestABadConfigurationIsRefusedAtOnce: the address and the trust store are
// read when the client is built, so a mistake is reported before a call.
func TestABadConfigurationIsRefusedAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  client.Config
		want string
	}{
		{"no address", client.Config{Getenv: env(nil)}, client.URLEnv},
		{"an address that is no URL", client.Config{URL: "://nowhere", Getenv: env(nil)}, "no http or https address"},
		{"an address of another scheme", client.Config{URL: "ftp://example.com", Getenv: env(nil)}, "no http or https address"},
		{"a certificate authority that is not there", client.Config{URL: "https://example.com", CAFile: filepath.Join(t.TempDir(), "absent"), Getenv: env(nil)}, "certificate authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() = %v, want a failure naming %q", err, tc.want)
			}
		})
	}
	// A file that is no certificate is refused as well.
	pem := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(pem, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.New(client.Config{URL: "https://example.com", CAFile: pem, Getenv: env(nil)}); err == nil {
		t.Fatal("a file holding no certificate was accepted as a trust store")
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
			map[string]any{"paths": []any{"spec.image"}, "detail": "image changed", "request_id": "req_theservers"})
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
	c, err := client.New(client.Config{URL: "http://127.0.0.1:1", Token: "t", Getenv: env(nil)})
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
// in design 008.
func TestTheRoutesOfEveryObjectCall(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/secrets") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"api"},"spec":{"kind":"static"},"status":{"id":"sec_1","owner":"alice","version":2}}`))
			return
		}
		writeObject(w, 200, "dev", "sbx_1")
	})
	c := f.client(client.Config{})
	ctx := t.Context()
	body := []byte(`{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"dev"},"spec":{}}`)
	for _, tc := range []struct {
		name         string
		run          func() error
		method, path string
	}{
		{"apply a sandbox", func() error { _, _, err := c.CreateSandbox(ctx, body); return err }, "POST", "/v1/sandboxes"},
		{"apply a secret", func() error { _, _, err := c.ApplySecret(ctx, "api", body); return err }, "PUT", "/v1/secrets/api"},
		{"get a sandbox", func() error { _, _, err := c.GetSandbox(ctx, "sbx_1"); return err }, "GET", "/v1/sandboxes/sbx_1"},
		{"get a secret", func() error { _, _, err := c.GetSecret(ctx, "api"); return err }, "GET", "/v1/secrets/api"},
		{"delete a sandbox", func() error { _, err := c.Delete(ctx, client.KindSandbox, "dev"); return err }, "DELETE", "/v1/sandboxes/dev"},
		{"delete a secret", func() error { _, err := c.Delete(ctx, client.KindSecret, "api"); return err }, "DELETE", "/v1/secrets/api"},
		{"start", func() error { _, _, err := c.Act(ctx, "dev", "start"); return err }, "POST", "/v1/sandboxes/dev/start"},
		{"stop", func() error { _, _, err := c.Act(ctx, "dev", "stop"); return err }, "POST", "/v1/sandboxes/dev/stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err != nil {
				t.Fatal(err)
			}
			got := f.last()
			if got.Method != tc.method || got.Path != tc.path {
				t.Fatalf("the call was %s %s, want %s %s", got.Method, got.Path, tc.method, tc.path)
			}
		})
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
	c := f.client(client.Config{TokenFile: filepath.Join(t.TempDir(), "absent"), Getenv: env(nil)})
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

// TestKindsAreReadSingularOrPlural: a caller writes either and the route is
// the plural.
func TestKindsAreReadSingularOrPlural(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want client.Kind
	}{{"sandbox", client.KindSandbox}, {"sandboxes", client.KindSandbox}, {"Sandbox", client.KindSandbox},
		{"secret", client.KindSecret}, {"secrets", client.KindSecret}, {"volume", ""}, {"sandboxs", ""}} {
		got, ok := client.ParseKind(tc.in)
		if tc.want == "" {
			if ok {
				t.Errorf("ParseKind(%q) = %q, and this API serves no such kind", tc.in, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("ParseKind(%q) = %q, %v", tc.in, got, ok)
		}
	}
	if client.KindSandbox.Path() != "/v1/sandboxes" || client.KindSecret.Path() != "/v1/secrets" {
		t.Errorf("the collection routes are %q and %q", client.KindSandbox.Path(), client.KindSecret.Path())
	}
	if client.KindSandbox.ManifestKind() != "Sandbox" || client.KindSecret.ManifestKind() != "Secret" {
		t.Error("the manifest kinds are not the names a document declares")
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
	if client.KindSecret.Plural() != "secrets" || client.KindSandbox.Plural() != "sandboxes" {
		t.Error("the collection names are not the plural of the kinds")
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
