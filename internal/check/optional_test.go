// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/events"
)

// getenv is a configuration as a map.
func getenv(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

// TestEveryOptionalLineNamesItsVariableWhenUnset: an operator who expected
// a dependency to be checked reads which variable was not set.
func TestEveryOptionalLineNamesItsVariableWhenUnset(t *testing.T) {
	empty := getenv(nil)
	for _, tc := range []struct {
		line     Line
		variable string
	}{
		{admission(empty), "CELLA_ADMISSION_URL"},
		{sink(t.Context(), config.Config{}, http.DefaultClient, time.Now), "CELLA_EVENTS_URL"},
		{store(t.Context(), config.Config{}), "CELLA_DB_URL"},
		{gateway(t.Context(), config.Config{}, empty), "CELLA_GATEWAY"},
	} {
		t.Run(tc.line.Name, func(t *testing.T) {
			if tc.line.State != Skipped {
				t.Fatalf("%s = %s, want skip", tc.line.Name, tc.line.State)
			}
			if !strings.Contains(tc.line.Detail, tc.variable) {
				t.Errorf("%s says %q and does not name %s", tc.line.Name, tc.line.Detail, tc.variable)
			}
		})
	}
}

// TestAdmissionSaysWhichReleaseReachesIt: the variable is set on a hosted
// deployment ahead of the client, and a line that claimed to have probed an
// endpoint this binary never dials would be a false answer.
func TestAdmissionSaysWhichReleaseReachesIt(t *testing.T) {
	got := admission(getenv(map[string]string{"CELLA_ADMISSION_URL": "https://admission.example.com/admit"}))
	if got.State != Skipped {
		t.Fatalf("admission = %s, want skip", got.State)
	}
	if !strings.Contains(got.Detail, "007") {
		t.Errorf("the line does not say which spec's client reaches it: %q", got.Detail)
	}
}

// sinkAt is a sink that answers a fixed status and records whether the
// signature over the body verified under the secrets it holds.
func sinkAt(t *testing.T, status int, secrets ...string) (string, *bool) {
	t.Helper()
	verified := new(bool)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		header := r.Header.Get(events.SignatureHeader)
		for _, secret := range secrets {
			if strings.Contains(header, events.Signature(secret, time.Unix(1, 0), body)) {
				*verified = true
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s.URL, verified
}

// TestSinkReadsEachAnswer is spec 009's acknowledgment rule as the line
// reads it: the endpoint answered, and the two ends share a secret.
func TestSinkReadsEachAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   State
		detail string
	}{
		{"an acknowledgment", http.StatusNoContent, Ok, "acknowledged"},
		{"a permanent drop", http.StatusBadRequest, Ok, "verified the signature"},
		{"a refused signature", http.StatusUnauthorized, Failed, "CELLA_EVENTS_SECRET"},
		{"an endpoint that is down", http.StatusServiceUnavailable, Failed, "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, verified := sinkAt(t, tc.status, "first", "second")
			cfg := config.Config{Events: config.Events{URL: url, Secrets: []string{"first", "second"}}}
			got := sink(t.Context(), cfg, http.DefaultClient, func() time.Time { return time.Unix(1, 0) })
			if got.State != tc.want {
				t.Fatalf("sink = %s (%s), want %s", got.State, got.Detail, tc.want)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("sink says %q, and an operator needs %q in it", got.Detail, tc.detail)
			}
			// Each half of a rotating secret signs the delivery, so a sink
			// that has moved to either one verifies the probe.
			if !*verified {
				t.Error("the probe's signature verified under neither secret")
			}
		})
	}
}

// TestSinkThatDoesNotAnswer: an endpoint nothing listens on is the failure
// an operator sees most, and the line names the variable.
func TestSinkThatDoesNotAnswer(t *testing.T) {
	cfg := config.Config{Events: config.Events{URL: "http://127.0.0.1:1/events", Secrets: []string{"s"}}}
	got := sink(t.Context(), cfg, http.DefaultClient, time.Now)
	if got.State != Failed || !strings.Contains(got.Detail, "CELLA_EVENTS_URL") {
		t.Fatalf("sink = %s (%s)", got.State, got.Detail)
	}
}

// TestSinkRefusesAURLThatIsNoRequest covers the one branch a configured URL
// can still fail on before anything is sent.
func TestSinkRefusesAURLThatIsNoRequest(t *testing.T) {
	cfg := config.Config{Events: config.Events{URL: "http://127.0.0.1:1/\x7f", Secrets: []string{"s"}}}
	if got := sink(t.Context(), cfg, http.DefaultClient, time.Now); got.State != Failed {
		t.Fatalf("sink = %s (%s)", got.State, got.Detail)
	}
}

// TestSchemaLineReadsEveryAnswerTheDatabaseCanGive is the store's guard as
// an answer: a fresh database and a pending migration pass, because cellad
// serve applies the schema at start, and only a schema no binary can serve
// fails.
func TestSchemaLineReadsEveryAnswerTheDatabaseCanGive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int64
		dirty   bool
		highest uint
		err     error
		want    State
		detail  string
	}{
		{"a database with no rows", 0, false, 4, pgx.ErrNoRows, Ok, "carries no schema"},
		{"a database with no schema", 0, false, 4, &pgconn.PgError{Code: "42P01"}, Ok, "carries no schema"},
		{"a database that did not answer", 0, false, 4, errors.New("no route to host"), Failed, "CELLA_DB_URL"},
		{"a schema a migration left halfway", 2, true, 4, nil, Failed, "dirty"},
		{"a schema this binary does not know", 9, false, 4, nil, Failed, "migrate down to 4"},
		{"a schema with migrations pending", 2, false, 4, nil, Ok, "2 pending"},
		{"a schema at this binary's version", 4, false, 4, nil, Ok, "which is this binary's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaLine(tc.version, tc.dirty, tc.highest, tc.err)
			if got.State != tc.want {
				t.Fatalf("store = %s (%s), want %s", got.State, got.Detail, tc.want)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("store says %q, and an operator needs %q in it", got.Detail, tc.detail)
			}
		})
	}
}

// TestStoreRefusesAURLThatIsNoDatabase and TestStoreThatDoesNotAnswer cover
// the two failures before the schema is read: an unparseable URL and a
// database nothing listens for.
func TestStoreRefusesAURLThatIsNoDatabase(t *testing.T) {
	got := store(t.Context(), config.Config{DBURL: "postgres://user:%zz@host/db"})
	if got.State != Failed || !strings.Contains(got.Detail, "CELLA_DB_URL") {
		t.Fatalf("store = %s (%s)", got.State, got.Detail)
	}
}

func TestStoreThatDoesNotAnswer(t *testing.T) {
	got := store(t.Context(), config.Config{DBURL: "postgres://cella@127.0.0.1:1/cella?connect_timeout=1"})
	if got.State != Failed || !strings.Contains(got.Detail, "CELLA_DB_URL") {
		t.Fatalf("store = %s (%s)", got.State, got.Detail)
	}
}

// TestGatewayResolvesAndDoesNotDial: the boundary a plane deploys admits
// the sandboxes to the door and not the control plane, so the line is a
// resolution and a dial would fail a correct deployment.
func TestGatewayResolvesAndDoesNotDial(t *testing.T) {
	// Nothing listens on this port, and the line passes: it resolves.
	cfg := config.Config{Gateway: config.EgressGateway{ProxyAddr: "127.0.0.1:1"}}
	system := getenv(nil)
	got := gateway(t.Context(), cfg, system)
	if got.State != Ok || !strings.Contains(got.Detail, "127.0.0.1") || !strings.Contains(got.Detail, "public roots") {
		t.Fatalf("gateway = %s (%s)", got.State, got.Detail)
	}
	cfg.Gateway = config.EgressGateway{ProxyAddr: "gateway.invalid:3128"}
	if got := gateway(t.Context(), cfg, system); got.State != Failed || !strings.Contains(got.Detail, "CELLA_GATEWAY") {
		t.Fatalf("gateway = %s (%s), want a failure naming the variable", got.State, got.Detail)
	}
	// A door named without a port is a host, which is what spec 002's
	// table admits.
	cfg.Gateway = config.EgressGateway{ProxyAddr: "localhost"}
	if got := gateway(t.Context(), cfg, system); got.State != Ok {
		t.Fatalf("gateway = %s (%s)", got.State, got.Detail)
	}
}

// TestGatewayFailsWithoutPublicRoots: serve with a gateway refuses to start
// without the public roots, so the line that answers for the gateway fails
// on the same file, naming it.
func TestGatewayFailsWithoutPublicRoots(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	cfg := config.Config{Gateway: config.EgressGateway{ProxyAddr: "127.0.0.1:1"}}
	got := gateway(t.Context(), cfg, getenv(map[string]string{"SSL_CERT_FILE": missing}))
	if got.State != Failed || !strings.Contains(got.Detail, "public roots") || !strings.Contains(got.Detail, missing) {
		t.Fatalf("gateway = %s (%s), want a failure naming the roots and the file", got.State, got.Detail)
	}
}
