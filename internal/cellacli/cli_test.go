// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/cella/internal/cellacli"
	v1 "latere.ai/x/cella/manifest/v1"
)

// call is one request the fake control plane saw.
type call struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

// plane is a fake control plane: it records every call and answers what the
// case told it to. The bodies are the shapes design 008 fixes, written out
// here so a change in either side is a failure and not a silent drift.
type plane struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	calls   []call
	handler func(http.ResponseWriter, *http.Request)
}

func newPlane(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *plane {
	t.Helper()
	p := &plane{t: t, handler: handler}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		p.mu.Lock()
		p.calls = append(p.calls, call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: string(body)})
		p.mu.Unlock()
		w.Header().Set("X-Request-ID", "req_fromtheserver")
		p.handler(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *plane) seen() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]call(nil), p.calls...)
}

func (p *plane) last() call {
	p.t.Helper()
	calls := p.seen()
	if len(calls) == 0 {
		p.t.Fatal("the command reached the server with nothing")
	}
	return calls[len(calls)-1]
}

// result is one run of the command: its exit code and what it wrote.
type result struct {
	code   int
	stdout string
	stderr string
}

// run drives the command exactly as a shell does, against the fake plane.
func (p *plane) run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return runWith(t, cellacli.Env{
		Args:   args,
		Stdin:  strings.NewReader(stdin),
		Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "caller-token"}),
	})
}

// runWith drives the command with an environment the case built, filling
// the streams it does not name.
func runWith(t *testing.T, e cellacli.Env) result {
	t.Helper()
	var out, errOut bytes.Buffer
	e.Stdout, e.Stderr = &out, &errOut
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.Version == "" {
		e.Version = "v0.0.0-test"
	}
	if e.Identity == "" {
		e.Identity = "cella v0.0.0-test (abc1234, 2026-09-20)"
	}
	code := cellacli.Run(context.Background(), e)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// environment is a Getenv over a map, so a case states the whole
// environment the command reads.
func environment(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// sandboxJSON is one Sandbox as the API answers it.
func sandboxJSON(name, id, phase string) string {
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"` + name + `","labels":{"team":"core"}},` +
		`"spec":{"image":"example/image:1"},"status":{"id":"` + id + `","owner":"https://login.example.com|alice","environment":"default",` +
		`"driver":"native","phase":"` + phase + `","conditions":[{"type":"Ready","status":"True"}],"createdAt":"2026-09-20T09:00:00Z"}}` + "\n"
}

// secretJSON is one Secret as the API answers it: no value, ever.
func secretJSON(name, id string) string {
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"` + name + `"},` +
		`"spec":{"kind":"static","scope":{"hosts":["api.example.com"]}},` +
		`"status":{"id":"` + id + `","owner":"https://login.example.com|alice","version":3,"mountedBy":1,` +
		`"createdAt":"2026-09-20T08:00:00Z","updatedAt":"2026-09-20T09:30:00Z"}}` + "\n"
}

// refuse answers the envelope of design 008.
func refuse(w http.ResponseWriter, status int, code, message string) {
	httpjson.WriteError(w, status, httpjson.Error{Code: code, Message: message, Details: map[string]any{
		"request_id": "req_fromtheserver", "detail": "the developer sentence", "paths": []any{"spec.image"},
	}})
}

// routes is the fake plane every command test runs against: one answer per
// route of design 008 that this slice's commands call.
func routes(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/version":
		_, _ = io.WriteString(w, `{"version":"v1.2.3","commit":"abc1234","buildTime":"2026-09-20"}`)
	case r.URL.Path == "/v1/sandboxes" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"items":[`+strings.TrimSuffix(sandboxJSON("dev", "sbx_1", "Running"), "\n")+`],"next":""}`+"\n")
	case r.URL.Path == "/v1/secrets" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"items":[`+strings.TrimSuffix(secretJSON("api", "sec_1"), "\n")+`],"next":""}`+"\n")
	case strings.HasPrefix(r.URL.Path, "/v1/secrets/"):
		_, _ = io.WriteString(w, secretJSON("api", "sec_1"))
	case strings.HasSuffix(r.URL.Path, "/exec"):
		_, _ = io.WriteString(w, `{"exitCode":0,"stdout":"hello\n","stderr":"","truncated":false,"durationMs":4}`+"\n")
	case strings.HasSuffix(r.URL.Path, "/logs"):
		_, _ = io.WriteString(w, "a line\nanother line\n")
	case strings.HasSuffix(r.URL.Path, "/egress"):
		_, _ = io.WriteString(w, `{"items":[{"principal":"sbx_1","at":"2026-09-20T10:00:00Z","door":"proxy","host":"api.example.com","port":443,"decision":"allow","substituted":["api"]}]}`+"\n")
	case strings.HasSuffix(r.URL.Path, "/files/list"):
		_, _ = io.WriteString(w, `{"items":[{"name":"a.txt","path":"/workspace/a.txt","size":3,"mode":"0644","modTime":"2026-09-20T10:00:00Z","isDir":false}],"next":""}`+"\n")
	case strings.HasSuffix(r.URL.Path, "/files/stat"):
		_, _ = io.WriteString(w, `{"name":"a.txt","path":"/workspace/a.txt","size":3,"mode":"0644","modTime":"2026-09-20T10:00:00Z","isDir":false}`+"\n")
	case strings.HasSuffix(r.URL.Path, "/files/content"):
		_, _ = io.WriteString(w, "abc")
	case strings.HasSuffix(r.URL.Path, "/files") && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(tarOf(map[string]string{"a.txt": "one"}))
	case strings.HasSuffix(r.URL.Path, "/files"), strings.HasSuffix(r.URL.Path, "/files/mkdir"), strings.HasSuffix(r.URL.Path, "/files/move"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Deleting"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Running"))
	default:
		_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Running"))
	}
}

// TestEveryCommandCallsTheRouteOfItsRow is the command table of design 011,
// one case per row: the method, the route and the selectors it sends, and
// what it writes.
func TestEveryCommandCallsTheRouteOfItsRow(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(manifest, []byte(`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"example/image:1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(secret, []byte(`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","metadata":{"name":"api"},"spec":{"kind":"static"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		args         []string
		method, path string
		query        map[string]string
		stdout       string
	}{{
		name: "apply a sandbox", args: []string{"apply", "-f", manifest},
		method: "POST", path: "/v1/sandboxes", stdout: "sandbox/dev applied\n",
	}, {
		name: "apply a secret", args: []string{"apply", "-f", secret},
		method: "PUT", path: "/v1/secrets/api", stdout: "secret/api applied\n",
	}, {
		name: "get one sandbox", args: []string{"get", "sandbox", "dev"},
		method: "GET", path: "/v1/sandboxes/dev",
	}, {
		name:   "list sandboxes with every selector",
		args:   []string{"get", "sandboxes", "-l", "team=core", "--phase", "Running", "--owner", "alice", "--environment", "default", "--limit", "10"},
		method: "GET", path: "/v1/sandboxes",
		query: map[string]string{"label": "team=core", "phase": "Running", "owner": "alice", "environment": "default", "limit": "10"},
	}, {
		name: "list secrets", args: []string{"get", "secrets"}, method: "GET", path: "/v1/secrets",
	}, {
		name: "delete", args: []string{"delete", "sandbox", "dev"},
		method: "DELETE", path: "/v1/sandboxes/dev", stdout: "sandbox/dev deleted\n",
	}, {
		name: "start", args: []string{"start", "dev"},
		method: "POST", path: "/v1/sandboxes/dev/start", stdout: "sandbox/dev started\n",
	}, {
		name: "stop", args: []string{"stop", "dev"},
		method: "POST", path: "/v1/sandboxes/dev/stop", stdout: "sandbox/dev stopped\n",
	}, {
		name: "exec", args: []string{"exec", "dev", "--", "echo", "hello"},
		method: "POST", path: "/v1/sandboxes/dev/exec", query: map[string]string{"wait": "1"}, stdout: "hello\n",
	}, {
		name: "logs", args: []string{"logs", "dev", "-f", "--tail", "5"},
		method: "GET", path: "/v1/sandboxes/dev/logs", query: map[string]string{"follow": "1", "tail": "5"},
		stdout: "a line\nanother line\n",
	}, {
		name: "egress", args: []string{"egress", "dev", "--limit", "5"},
		method: "GET", path: "/v1/sandboxes/dev/egress", query: map[string]string{"limit": "5"},
	}, {
		name: "files ls", args: []string{"files", "ls", "dev:/workspace"},
		method: "GET", path: "/v1/sandboxes/dev/files/list", query: map[string]string{"path": "/workspace"},
	}, {
		name: "files stat", args: []string{"files", "stat", "dev:/workspace/a.txt"},
		method: "GET", path: "/v1/sandboxes/dev/files/stat", query: map[string]string{"path": "/workspace/a.txt"},
	}, {
		name: "files get to stdout", args: []string{"files", "get", "dev:/workspace/a.txt"},
		method: "GET", path: "/v1/sandboxes/dev/files/content", stdout: "abc",
	}, {
		name: "files put from stdin", args: []string{"files", "put", "-", "dev:/workspace/b.txt", "--mode", "0600"},
		method: "PUT", path: "/v1/sandboxes/dev/files",
		query:  map[string]string{"path": "/workspace/b.txt", "mode": "0600"},
		stdout: "dev:/workspace/b.txt written\n",
	}, {
		name: "files mkdir", args: []string{"files", "mkdir", "dev:/workspace/sub"},
		method: "POST", path: "/v1/sandboxes/dev/files/mkdir", stdout: "dev:/workspace/sub made\n",
	}, {
		name: "files rm", args: []string{"files", "rm", "dev:/workspace/b.txt"},
		method: "DELETE", path: "/v1/sandboxes/dev/files",
		query: map[string]string{"path": "/workspace/b.txt"}, stdout: "dev:/workspace/b.txt removed\n",
	}, {
		name: "files mv", args: []string{"files", "mv", "dev:/workspace/a.txt", "dev:/workspace/sub/a.txt"},
		method: "POST", path: "/v1/sandboxes/dev/files/move", stdout: "dev:/workspace/sub/a.txt moved\n",
	}, {
		name: "version", args: []string{"version"}, method: "GET", path: "/version",
		stdout: "cella v0.0.0-test (abc1234, 2026-09-20)\nserver v1.2.3 (abc1234, 2026-09-20)\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlane(t, routes)
			got := p.run(t, "written by the caller", tc.args...)
			if got.code != 0 {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
			seen := p.last()
			if seen.Method != tc.method || seen.Path != tc.path {
				t.Fatalf("the call was %s %s, want %s %s", seen.Method, seen.Path, tc.method, tc.path)
			}
			for key, want := range tc.query {
				if seen.Query.Get(key) != want {
					t.Errorf("the %s selector is %q, want %q", key, seen.Query.Get(key), want)
				}
			}
			if tc.stdout != "" && got.stdout != tc.stdout {
				t.Errorf("stdout is %q, want %q", got.stdout, tc.stdout)
			}
		})
	}
}

// TestApplyReadsTheDocumentAndSendsItUnchanged: the command reads the kind
// to choose the route and sends the caller's own bytes, so what the server
// refuses is what the caller wrote.
func TestApplyReadsTheDocumentAndSendsItUnchanged(t *testing.T) {
	p := newPlane(t, routes)
	body := `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"example/image:1"}}`
	got := p.run(t, body, "apply", "-f", "-")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	if p.last().Body != body {
		t.Fatalf("the document that arrived is %q", p.last().Body)
	}
	if ct := p.last().Method; ct != "POST" {
		t.Fatalf("the apply was %s", ct)
	}
}

// TestASecretValueIsPlacedInTheDocumentAndPrintedNowhere: --value-from-env
// and --value-file are the two sources, and the value reaches the request
// and no output.
func TestASecretValueIsPlacedInTheDocumentAndPrintedNowhere(t *testing.T) {
	const canary = "s3cr3t-canary-value"
	p := newPlane(t, routes)
	document := `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"api"},"spec":{"kind":"static"}}`
	got := runWith(t, cellacli.Env{
		Args:  []string{"apply", "-f", "-", "--value-from-env", "TOKEN_FOR_API", "-v"},
		Stdin: strings.NewReader(document),
		Getenv: environment(map[string]string{
			"CELLA_URL": p.server.URL, "CELLA_TOKEN": "caller-token", "TOKEN_FOR_API": canary,
		}),
	})
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(p.last().Body), &sent); err != nil {
		t.Fatal(err)
	}
	spec, _ := sent["spec"].(map[string]any)
	if spec["value"] != canary {
		t.Fatalf("the value the request carried is %v", spec["value"])
	}
	if strings.Contains(got.stdout, canary) || strings.Contains(got.stderr, canary) {
		t.Fatalf("the value reached an output: %q %q", got.stdout, got.stderr)
	}

	// The same value from a file, and the same silence.
	file := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(file, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = runWith(t, cellacli.Env{
		Args:   []string{"apply", "-f", "-", "--value-file", file},
		Stdin:  strings.NewReader(document),
		Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "caller-token"}),
	})
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	if err := json.Unmarshal([]byte(p.last().Body), &sent); err != nil {
		t.Fatal(err)
	}
	spec, _ = sent["spec"].(map[string]any)
	if spec["value"] != canary {
		t.Fatalf("the value the request carried is %v", spec["value"])
	}
	if strings.Contains(got.stdout, canary) || strings.Contains(got.stderr, canary) {
		t.Fatalf("the value reached an output: %q %q", got.stdout, got.stderr)
	}
}

// TestNoTokenReachesAnOutput is design 011's rule: the bearer and the token
// file's bytes reach no stdout or stderr byte, under -v included, on a run
// that succeeded and on one that was refused.
func TestNoTokenReachesAnOutput(t *testing.T) {
	const canary = "token-canary-abc123"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		refuse(w, 403, "forbidden", "You do not have permission to do this.")
	})
	answering := newPlane(t, routes)
	for _, tc := range []struct {
		name string
		p    *plane
		args []string
	}{
		{"a refusal under -v", refused, []string{"get", "sandboxes", "-v"}},
		{"a call that succeeded", answering, []string{"get", "sandboxes", "-v", "--json"}},
		{"an exec under -v", answering, []string{"exec", "dev", "-v", "--", "echo", "hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runWith(t, cellacli.Env{
				Args: append(tc.args, "--token-file", file),
				Getenv: environment(map[string]string{
					"CELLA_URL": tc.p.server.URL, "CELLA_TOKEN_FILE": file,
				}),
			})
			if strings.Contains(got.stdout, canary) || strings.Contains(got.stderr, canary) {
				t.Fatalf("the bearer reached an output: %q %q", got.stdout, got.stderr)
			}
		})
	}
}

// tarOf builds one archive for the fake plane's export route.
func tarOf(files map[string]string) []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, body := range files {
		_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = w.Write([]byte(body))
	}
	_ = w.Close()
	return buf.Bytes()
}
