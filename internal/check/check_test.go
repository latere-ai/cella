// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/check"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/podman"
)

// opener is cmd/cellad's openRuntime over the two backends a test drives
// without a cluster: the native one, and the podman one whose Preflight
// fails when no engine answers.
func opener(cfg config.Config) (runtime.Driver, func() error, error) {
	noop := func() error { return nil }
	if cfg.Runtime == config.RuntimePodman {
		d, err := podman.New(podman.Options{Socket: cfg.PodmanSocket})
		if err != nil {
			return nil, noop, err
		}
		return d, d.Close, nil
	}
	d, err := native.New(filepath.Join(cfg.DataDir, "native"))
	if err != nil {
		return nil, noop, err
	}
	return d, d.Close, nil
}

// signingKey is generated once: a 2048-bit RSA key costs more than every
// assertion in this package together.
var signingKey = sync.OnceValue(func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
})

// base is an installation that passes every mandatory line and configures
// no optional dependency, which is the first installation an operator has.
func base(t *testing.T, overrides map[string]string) config.Getenv {
	t.Helper()
	m := map[string]string{
		"CELLA_OIDC_ISSUERS":        issuertest.New(t).URL(),
		"CELLA_PUBLIC_URL":          "https://cella.example.com",
		"CELLA_TOKEN_KEY":           signingKey(),
		"CELLA_RUNTIME":             "native",
		"CELLA_ALLOW_UNSAFE_NATIVE": "true",
		"CELLA_DATA_DIR":            t.TempDir(),
	}
	maps.Copy(m, overrides)
	return func(k string) string { return m[k] }
}

// run is check.Run with the opener wired, returning the lines by name.
func run(t *testing.T, getenv config.Getenv) map[string]check.Line {
	t.Helper()
	lines := check.Run(t.Context(), check.Options{Getenv: getenv, Open: opener})
	out := make(map[string]check.Line, len(lines))
	for _, l := range lines {
		if _, seen := out[l.Name]; seen {
			t.Fatalf("two lines are named %q", l.Name)
		}
		out[l.Name] = l
	}
	return out
}

// mandatory and optional are the two halves of spec 014's table, named here
// so a line added to the command without a row here fails the count.
var (
	mandatory = []string{"configuration", "identity", "authorizer", "backend", "data directory"}
	optional  = []string{"admission", "sink", "store", "gateway"}
)

func TestCheckPassesACompleteInstallation(t *testing.T) {
	var out bytes.Buffer
	lines := check.Run(t.Context(), check.Options{Getenv: base(t, nil), Open: opener})
	if failed := check.Report(&out, lines); failed {
		t.Fatalf("a complete installation failed:\n%s", out.String())
	}
	if len(lines) != len(mandatory)+len(optional) {
		t.Fatalf("the command printed %d line(s), and spec 014's table has %d", len(lines), len(mandatory)+len(optional))
	}
	byName := map[string]check.Line{}
	for _, l := range lines {
		byName[l.Name] = l
	}
	for _, name := range mandatory {
		if got := byName[name].State; got != check.Ok {
			t.Errorf("%s = %s (%s), want ok", name, got, byName[name].Detail)
		}
	}
	for _, name := range optional {
		if got := byName[name].State; got != check.Skipped {
			t.Errorf("%s = %s (%s), want skip", name, got, byName[name].Detail)
		}
	}
	// The report lines up, so two installations read side by side.
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if !strings.Contains(line, "  ") {
			t.Errorf("the report line %q has no column separator", line)
		}
	}
}

// TestCheckNamesEachFailure removes one mandatory requirement at a time and
// asserts the line that owns it fails, alone.
func TestCheckNamesEachFailure(t *testing.T) {
	// An authorizer that allows everything, which is an endpoint that does
	// not read the request.
	allows := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allow":true}`))
	}))
	defer allows.Close()

	for _, tc := range []struct {
		name, line string
		env        map[string]string
		detail     string
		// only is set where the failure ends the run before any other line,
		// and also names the lines that share one cause with this one.
		only bool
		also []string
	}{
		{
			name: "a configuration that does not load", line: "configuration", only: true,
			env: map[string]string{"CELLA_RUNTIME": "docker"}, detail: "CELLA_RUNTIME",
		},
		{
			name: "an issuer that does not answer", line: "identity",
			env: map[string]string{"CELLA_OIDC_ISSUERS": "http://127.0.0.1:1"}, detail: "127.0.0.1:1",
		},
		{
			name: "an authorizer that allows the probe", line: "authorizer",
			env: map[string]string{
				"CELLA_AUTHORIZER_URL":   allows.URL,
				"CELLA_AUTHORIZER_TOKEN": "probe-token",
			},
			detail: "probe",
		},
		{
			name: "a backend that does not answer", line: "backend",
			env: map[string]string{
				"CELLA_RUNTIME":       "podman",
				"CELLA_PODMAN_SOCKET": filepath.Join(t.TempDir(), "absent.sock"),
			},
			detail: "no engine answered",
		},
		{
			name: "a data directory that cannot be written", line: "data directory",
			env:    map[string]string{"CELLA_DATA_DIR": "/dev/null/cella"},
			detail: "CELLA_DATA_DIR",
			// The native backend keeps its sandboxes under the same
			// directory, so one unwritable path is two failed lines.
			also: []string{"backend"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := check.Run(t.Context(), check.Options{Getenv: base(t, tc.env), Open: opener})
			var out bytes.Buffer
			if !check.Report(&out, lines) {
				t.Fatalf("the installation passed with %s removed:\n%s", tc.line, out.String())
			}
			byName := map[string]check.Line{}
			for _, l := range lines {
				byName[l.Name] = l
			}
			got, ok := byName[tc.line]
			if !ok {
				t.Fatalf("no line is named %q; the report is:\n%s", tc.line, out.String())
			}
			if got.State != check.Failed {
				t.Fatalf("%s = %s, want FAIL", tc.line, got.State)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("%s says %q, and an operator needs %q in it", tc.line, got.Detail, tc.detail)
			}
			if tc.only && len(lines) != 1 {
				t.Errorf("a configuration that does not load produced %d lines; nothing else is knowable", len(lines))
			}
			for _, l := range lines {
				if l.Name != tc.line && l.State == check.Failed && !slices.Contains(tc.also, l.Name) {
					t.Errorf("%s also failed: %s", l.Name, l.Detail)
				}
			}
		})
	}
}

// TestIdentityFailureSkipsTheAuthorizer: the authorizer cannot be asked
// without one, and a second failure for one cause is noise.
func TestIdentityFailureSkipsTheAuthorizer(t *testing.T) {
	lines := run(t, base(t, map[string]string{"CELLA_OIDC_ISSUERS": "http://127.0.0.1:1"}))
	if got := lines["authorizer"]; got.State != check.Skipped || !strings.Contains(got.Detail, "identity") {
		t.Fatalf("authorizer = %s (%s), want a skip naming the identity", got.State, got.Detail)
	}
}

// TestCheckHoldsUnderTheOwnerPolicyAndAnEndpoint: the probe is denied by
// both authorizers spec 006 admits, so the line is not a property of one.
func TestCheckHoldsUnderTheOwnerPolicyAndAnEndpoint(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	for name, env := range map[string]map[string]string{
		"the owner policy": nil,
		"an endpoint": {
			"CELLA_AUTHORIZER_URL":   s.URL(),
			"CELLA_AUTHORIZER_TOKEN": s.Token(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := run(t, base(t, env))["authorizer"]
			if got.State != check.Ok {
				t.Fatalf("authorizer = %s (%s)", got.State, got.Detail)
			}
			if !strings.Contains(got.Detail, auth.ProbeID) {
				t.Errorf("the line does not name the probe id: %q", got.Detail)
			}
		})
	}
}

// TestBackendWithoutAnOpener: the seam is required, and a caller that
// forgot it gets a line that says so rather than a nil dereference.
func TestBackendWithoutAnOpener(t *testing.T) {
	lines := check.Run(t.Context(), check.Options{Getenv: base(t, nil)})
	for _, l := range lines {
		if l.Name == "backend" {
			if l.State != check.Skipped {
				t.Fatalf("backend = %s (%s), want a skip", l.State, l.Detail)
			}
			return
		}
	}
	t.Fatal("no backend line")
}

// TestReportRendersEveryState so the three words an operator greps for are
// the three the command prints.
func TestReportRendersEveryState(t *testing.T) {
	var out bytes.Buffer
	failed := check.Report(&out, []check.Line{
		{Name: "one", State: check.Ok, Detail: "fine"},
		{Name: "two-longer", State: check.Skipped, Detail: "unset"},
		{Name: "three", State: check.Failed, Detail: "broken"},
	})
	if !failed {
		t.Error("a report with a failure returned false")
	}
	for _, want := range []string{"one         ok    fine", "two-longer  skip  unset", "three       FAIL  broken"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
}

// contextAlreadyDone proves a cancelled context reaches every line rather
// than hanging one of them.
func TestCancelledContextFailsRatherThanHangs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lines := check.Run(ctx, check.Options{Getenv: base(t, nil), Open: opener})
	if !check.Report(&bytes.Buffer{}, lines) {
		t.Fatal("a cancelled run reported no failure")
	}
}
