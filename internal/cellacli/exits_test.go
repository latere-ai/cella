// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/cellacli"
)

// table is the error table of design 008: every code, the status it
// carries, and the exit of design 011 outside a session. Under a session
// every refusal is 125, less the one that says the command never started.
var table = []struct {
	code     string
	status   int
	wantExit int
}{
	{"unsupported_media_type", 415, 3},
	{"not_acceptable", 406, 3},
	{"multi_document", 400, 3},
	{"unsupported_version", 400, 3},
	{"unsupported_kind", 400, 3},
	{"unknown_field", 400, 3},
	{"missing_field", 400, 3},
	{"invalid_field", 400, 3},
	{"reserved_prefix", 400, 3},
	{"exclusive_fields", 400, 3},
	{"path_conflict", 400, 3},
	{"bad_request", 400, 3},
	{"body_too_large", 413, 3},
	{"unauthenticated", 401, 3},
	{"forbidden", 403, 3},
	{"not_found", 404, 4},
	{"immutable_field", 409, 5},
	{"boundary_widened", 409, 5},
	{"name_taken", 409, 5},
	{"version_conflict", 409, 5},
	{"phase_conflict", 409, 5},
	{"volume_busy", 409, 5},
	{"secret_host_conflict", 409, 5},
	{"secret_out_of_scope", 422, 3},
	{"ceiling_exceeded", 422, 3},
	{"capability_unsupported", 422, 3},
	{"environment_mismatch", 422, 3},
	{"admission_refused", 422, 3},
	{"quota_exceeded", 422, 3},
	{"boundary_exceeded", 422, 3},
	{"spawn_budget_exhausted", 422, 3},
	{"rate_limited", 429, 3},
	{"cursor_expired", 410, 3},
	{"admission_unavailable", 503, 1},
	{"authorizer_unavailable", 503, 1},
	{"driver_unavailable", 503, 1},
	{"upstream_unavailable", 502, 1},
	// A code this client has never seen exits by its status class, which is
	// what makes the scheme survive a newer server.
	{"a_code_from_a_newer_server", 422, 3},
	{"another_code_from_a_newer_server", 503, 1},
}

// TestEveryErrorCodeBecomesItsExit is design 011's exit scheme over the
// whole error table of design 008, outside a session and under one.
func TestEveryErrorCodeBecomesItsExit(t *testing.T) {
	for _, tc := range table {
		t.Run(tc.code, func(t *testing.T) {
			p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
				refuse(w, tc.status, tc.code, "The server refused this.")
			})
			got := p.run(t, "", "get", "sandbox", "dev")
			if got.code != tc.wantExit {
				t.Fatalf("exit %d, want %d for %d %s", got.code, tc.wantExit, tc.status, tc.code)
			}
			if got.stdout != "" {
				t.Errorf("a refusal wrote to stdout: %q", got.stdout)
			}
			if got.stderr != "The server refused this.\n" {
				t.Errorf("stderr is %q, want the API's sentence as one line", got.stderr)
			}
			// Under exec every refusal that is not a missing capability is
			// one code: the command itself failed and no child ran.
			under := p.run(t, "", "exec", "dev", "--", "echo", "hi")
			if under.code != 125 {
				t.Fatalf("under exec the exit is %d, want 125", under.code)
			}
		})
	}
}

// TestTheExitsThatAreNotARefusal covers the rest of design 011's scheme:
// usage, the server unreachable, a command that could not start, a stream
// that ended early, and the child's own code.
func TestTheExitsThatAreNotARefusal(t *testing.T) {
	answering := newPlane(t, routes)

	t.Run("a usage error is 2", func(t *testing.T) {
		for _, args := range [][]string{
			{},
			{"frobnicate"},
			{"get"},
			{"get", "volumes"},
			{"get", "sandbox", "dev", "spare"},
			{"get", "sandboxes", "-o", "toml"},
			{"delete", "sandbox"},
			{"delete", "volume", "v"},
			{"start"},
			{"stop", "a", "b"},
			{"exec", "dev"},
			{"exec"},
			{"exec", "dev", "extra", "--", "sh"},
			{"exec", "dev", "--env", "NOTANENTRY", "--", "sh"},
			{"apply"},
			{"apply", "-f", "-"},
			{"apply", "-f", filepath.Join(t.TempDir(), "absent.json")},
			{"apply", "-f", "-", "extra"},
			{"logs"},
			{"logs", "dev", "--since", "yesterday"},
			{"egress"},
			{"cp", "one"},
			{"cp", "here", "there"},
			{"cp", "a:/workspace", "b:/workspace"},
			{"files"},
			{"files", "chown", "dev:/workspace"},
			{"files", "ls"},
			{"files", "ls", "/workspace"},
			{"files", "get"},
			{"files", "put", "one"},
			{"files", "put", "-", "/workspace/a"},
			{"files", "mv", "dev:/workspace/a", "other:/workspace/b"},
			{"get", "--nosuchflag", "sandboxes"},
		} {
			got := answering.run(t, "", args...)
			if got.code != 2 {
				t.Errorf("`cella %s` exited %d, want 2: %q%q", strings.Join(args, " "), got.code, got.stdout, got.stderr)
			}
		}
	})

	t.Run("a document that is not JSON is 2", func(t *testing.T) {
		for _, body := range []string{"apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\n", "{}", `{"kind":"Volume","apiVersion":"v"}`, `{"apiVersion":"v","kind":"Secret","spec":{}}`} {
			got := answering.run(t, body, "apply", "-f", "-")
			if got.code != 2 {
				t.Errorf("a document %q exited %d, want 2", body, got.code)
			}
		}
	})

	t.Run("no address and no token are 2", func(t *testing.T) {
		got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes"}, Getenv: environment(nil)})
		if got.code != 2 {
			t.Fatalf("exit %d, want 2", got.code)
		}
		if !strings.Contains(got.stderr, "CELLA_URL") {
			t.Fatalf("stderr is %q, and it names no variable to set", got.stderr)
		}
		missing := filepath.Join(t.TempDir(), "absent")
		got = runWith(t, cellacli.Env{Args: []string{"get", "sandboxes"}, Getenv: environment(map[string]string{
			"CELLA_URL": answering.server.URL, "CELLA_TOKEN_FILE": missing,
		})})
		if got.code != 2 || !strings.Contains(got.stderr, "CELLA_TOKEN") {
			t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
		}
	})

	t.Run("a server that is not there is 7, and 127 under exec", func(t *testing.T) {
		gone := environment(map[string]string{"CELLA_URL": "http://127.0.0.1:1", "CELLA_TOKEN": "t"})
		if got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes"}, Getenv: gone}); got.code != 7 {
			t.Errorf("exit %d, want 7", got.code)
		}
		if got := runWith(t, cellacli.Env{Args: []string{"exec", "dev", "--", "echo"}, Getenv: gone}); got.code != 127 {
			t.Errorf("under exec the exit is %d, want 127", got.code)
		}
		if got := runWith(t, cellacli.Env{Args: []string{"exec", "dev", "-i", "--", "sh"}, Getenv: gone}); got.code != 127 {
			t.Errorf("under an exec session the exit is %d, want 127", got.code)
		}
	})

	t.Run("a session that cannot start is 126", func(t *testing.T) {
		p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
			refuse(w, 422, "capability_unsupported", "The environment cannot provide this.")
		})
		for _, args := range [][]string{
			{"exec", "dev", "-i", "--", "sh"},
			{"exec", "dev", "-t", "--", "sh"},
			{"attach", "dev"},
		} {
			got := p.run(t, "", args...)
			if got.code != 126 {
				t.Errorf("`cella %s` exited %d, want 126", strings.Join(args, " "), got.code)
			}
		}
		// The same refusal outside a session is an ordinary 422.
		if got := p.run(t, "", "get", "sandbox", "dev"); got.code != 3 {
			t.Errorf("outside a session the same refusal exited %d, want 3", got.code)
		}
	})

	t.Run("the child's own code leaves the process", func(t *testing.T) {
		for _, code := range []int{0, 3, 7, 124} {
			p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"exitCode":`+strconv.Itoa(code)+`,"stdout":"","stderr":"","truncated":false,"durationMs":1}`)
			})
			got := p.run(t, "", "exec", "dev", "--", "sh", "-c", "exit "+strconv.Itoa(code))
			if got.code != code {
				t.Errorf("the child exited %d and the command exited %d", code, got.code)
			}
		}
	})

	t.Run("output that was cut is said so, and the code still passes through", func(t *testing.T) {
		p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"exitCode":2,"stdout":"out","stderr":"err","truncated":true,"durationMs":1}`)
		})
		got := p.run(t, "", "exec", "dev", "--", "sh")
		if got.code != 2 || got.stdout != "out" {
			t.Fatalf("exit %d, stdout %q", got.code, got.stdout)
		}
		if !strings.Contains(got.stderr, "err") || !strings.Contains(got.stderr, "cut") {
			t.Fatalf("stderr is %q", got.stderr)
		}
	})

	t.Run("a transfer that ended early is 1", func(t *testing.T) {
		p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Trailer", "X-Cella-Error")
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(tarOf(map[string]string{"a.txt": "one"}))
			w.Header().Set("X-Cella-Error", "driver_unavailable")
		})
		got := p.run(t, "", "cp", "dev:/workspace", t.TempDir())
		if got.code != 1 {
			t.Fatalf("exit %d, want 1: %q", got.code, got.stderr)
		}
	})

	t.Run("help is asked for and answered on stdout", func(t *testing.T) {
		for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
			got := runWith(t, cellacli.Env{Args: args, Getenv: environment(nil)})
			if got.code != 0 || !strings.Contains(got.stdout, "usage: cella <command>") {
				t.Errorf("`cella %s` exited %d and wrote %q", strings.Join(args, " "), got.code, got.stdout)
			}
		}
		// With no command at all the same text goes to stderr, and the exit
		// is the usage error it is.
		got := runWith(t, cellacli.Env{Args: nil, Getenv: environment(nil)})
		if got.code != 2 || !strings.Contains(got.stderr, "usage: cella <command>") {
			t.Errorf("exit %d, stderr %q", got.code, got.stderr)
		}
	})
}

// TestARefusalUnderVerboseNamesTheCodeThePathsAndTheRequest is design 011's
// refusal output: one sentence without -v, and the code, the paths and the
// server's own request id with it.
func TestARefusalUnderVerboseNamesTheCodeThePathsAndTheRequest(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		refuse(w, 409, "immutable_field", "This field cannot be changed after the object is created.")
	})
	plain := p.run(t, "", "get", "sandbox", "dev")
	if plain.stderr != "This field cannot be changed after the object is created.\n" {
		t.Fatalf("stderr is %q; Retry-After belongs to a 429 and to no other line", plain.stderr)
	}
	verbose := p.run(t, "", "get", "sandbox", "dev", "-v")
	for _, want := range []string{
		"This field cannot be changed after the object is created.",
		"code: immutable_field",
		"paths: spec.image",
		"detail: the developer sentence",
		"request: req_fromtheserver",
	} {
		if !strings.Contains(verbose.stderr, want) {
			t.Errorf("stderr under -v is %q, and it lacks %q", verbose.stderr, want)
		}
	}
}

// TestA429PrintsItsRetryAfter is the one line design 011 puts the header in.
func TestA429PrintsItsRetryAfter(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		refuse(w, 429, "rate_limited", "Too many requests; wait and retry.")
	})
	got := p.run(t, "", "get", "sandboxes")
	if got.code != 3 || !strings.Contains(got.stderr, "Retry after 30 seconds.") {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// TestARefusalThatIsNoEnvelopeStillPrintsALine: a listener that is not this
// API answers something else, and the command still says what happened and
// exits by the status.
func TestARefusalThatIsNoEnvelopeStillPrintsALine(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "a proxy")
	})
	got := p.run(t, "", "get", "sandboxes")
	if got.code != 1 || !strings.Contains(got.stderr, "a proxy") {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// TestAFileTheCommandCannotWriteIsReported: the destination of a download
// belongs to the caller, and a failure there is the caller's own.
func TestAFileTheCommandCannotWriteIsReported(t *testing.T) {
	p := newPlane(t, routes)
	got := p.run(t, "", "cp", "dev:/workspace", filepath.Join(os.DevNull, "under-a-file"))
	if got.code == 0 {
		t.Fatalf("a destination that cannot be written reported success: %q", got.stdout)
	}
}
