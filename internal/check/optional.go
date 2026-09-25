// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store/postgres/migrations"
)

// notConfigured is the sentence an optional dependency's line carries when
// its variable is unset. It names the variable, so an operator who expected
// the dependency to be checked sees at once what was not set.
func notConfigured(variable string) string {
	return variable + " is unset; this installation configures no such dependency"
}

// admission is the webhook of spec 007. The client is slice 047's and is not
// in this binary: internal/config reads no admission variable, so a check
// that probed the endpoint would report on a call cellad never makes. The
// line says which release reaches it rather than inventing a probe.
func admission(getenv config.Getenv) Line {
	url := strings.TrimSpace(getenv("CELLA_ADMISSION_URL"))
	if url == "" {
		return Line{Name: "admission", State: Skipped, Detail: notConfigured("CELLA_ADMISSION_URL")}
	}
	return Line{Name: "admission", State: Skipped,
		Detail: "CELLA_ADMISSION_URL is set and this binary dials no admission endpoint; the client lands with the admission slice of spec 007"}
}

// probeBody is what the sink's line posts: a body that is deliberately not
// an event record. The signature is what the line tests, and a sink that
// verified it must not file a record cellad never produced, so the body is
// one every sink refuses.
var probeBody = []byte(`{"probe":"cellad check"}`)

// sink is the event endpoint of spec 009. The line proves the two things an
// operator can get wrong: the endpoint answers, and the two ends share a
// secret. A 2xx is the acknowledgment; a 400 passes too, because the
// signature verified and the body was refused, which spec 009 calls a
// permanent drop and is the right answer to a probe that is no record; a 401
// is a signature the sink did not accept.
func sink(ctx context.Context, cfg config.Config, client *http.Client, now func() time.Time) Line {
	if !cfg.Events.Enabled() {
		return Line{Name: "sink", State: Skipped, Detail: notConfigured("CELLA_EVENTS_URL")}
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Events.URL, bytes.NewReader(probeBody))
	if err != nil {
		return Line{Name: "sink", State: Failed, Detail: "CELLA_EVENTS_URL: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(events.SignatureHeader, events.Header(now(), probeBody, cfg.Events.Secrets...))
	resp, err := client.Do(req)
	if err != nil {
		return Line{Name: "sink", State: Failed, Detail: "CELLA_EVENTS_URL: " + err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Line{Name: "sink", State: Failed,
			Detail: fmt.Sprintf("the sink refused the signature with %d; CELLA_EVENTS_SECRET differs from the sink's", resp.StatusCode)}
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Line{Name: "sink", State: Ok,
			Detail: fmt.Sprintf("%s acknowledged a signed probe with %d", cfg.Events.URL, resp.StatusCode)}
	case resp.StatusCode == http.StatusBadRequest:
		return Line{Name: "sink", State: Ok,
			Detail: fmt.Sprintf("%s verified the signature and refused the probe body with %d, which is the right answer to a body that is no record", cfg.Events.URL, resp.StatusCode)}
	default:
		return Line{Name: "sink", State: Failed,
			Detail: fmt.Sprintf("the sink answered %d; a record would be retried for CELLA_EVENTS_RETRY_WINDOW and never taken", resp.StatusCode)}
	}
}

// store is the durable state of spec 010. The line reads and never writes:
// postgres.Open applies the pending migrations, which is right for a process
// about to serve and wrong for a command an operator points at a live
// database. The cases are the store's own guard, read here without it.
func store(ctx context.Context, cfg config.Config) Line {
	if cfg.DBURL == "" {
		return Line{Name: "store", State: Skipped,
			Detail: notConfigured("CELLA_DB_URL") + "; every state is in memory and recovery is off"}
	}
	highest, err := migrations.Highest()
	if err != nil {
		return Line{Name: "store", State: Failed, Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	cfgPool, err := pgxpool.ParseConfig(cfg.DBURL)
	if err != nil {
		return Line{Name: "store", State: Failed, Detail: "CELLA_DB_URL is not a Postgres URL: " + err.Error()}
	}
	// One connection: a check is a question, not a workload, and the
	// database this points at has a ceiling a serving replica needs.
	cfgPool.MaxConns = 1
	cfgPool.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfgPool)
	if err != nil {
		return Line{Name: "store", State: Failed, Detail: "CELLA_DB_URL: " + err.Error()}
	}
	defer pool.Close()

	var (
		version int64
		dirty   bool
	)
	err = pool.QueryRow(ctx, "select version, dirty from schema_migrations").Scan(&version, &dirty)
	return schemaLine(version, dirty, highest, err)
}

// schemaLine reads what the database said about its schema. The cases are
// the ones internal/store/postgres refuses to start on, stated here as an
// answer rather than as a refusal: a fresh database and a pending migration
// pass, because cellad serve applies the schema at start, and only a schema
// no binary can serve fails.
func schemaLine(version int64, dirty bool, highest uint, err error) Line {
	switch {
	case errors.Is(err, pgx.ErrNoRows), isUndefinedTable(err):
		return Line{Name: "store", State: Ok,
			Detail: fmt.Sprintf("the database answers and carries no schema; cellad serve applies all %d migration(s) at start", highest)}
	case err != nil:
		return Line{Name: "store", State: Failed, Detail: "CELLA_DB_URL: " + err.Error()}
	case dirty:
		return Line{Name: "store", State: Failed,
			Detail: fmt.Sprintf("the schema is dirty at version %d: a migration failed halfway and an operator repairs it before cellad starts", version)}
	case version > int64(highest):
		return Line{Name: "store", State: Failed,
			Detail: fmt.Sprintf("the schema is at version %d and this binary carries %d: run the version of cellad that wrote the schema, or migrate down to %d", version, highest, highest)}
	case version < int64(highest):
		return Line{Name: "store", State: Ok,
			Detail: fmt.Sprintf("the database answers at schema version %d; cellad serve applies the %d pending migration(s) at start", version, int64(highest)-version)}
	default:
		return Line{Name: "store", State: Ok,
			Detail: fmt.Sprintf("the database answers at schema version %d, which is this binary's", version)}
	}
}

// isUndefinedTable is the one Postgres code that means a fresh database.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// gateway is the egress door of spec 018 as a sandbox of this environment
// reaches it. The line resolves the name and does not dial it: the boundary
// an installation deploys admits the sandboxes to the gateway and not the
// control plane, so a refused connection from cellad is the policy working
// and would fail a check on a correct deployment. A name that does not
// resolve is a sandbox that cannot reach its door.
//
// The line also reads the public roots the way serve does, since serve with a
// gateway refuses to start without them: a sandbox given the gateway's
// authority alone verifies no host the gateway passes through.
func gateway(ctx context.Context, cfg config.Config, getenv config.Getenv) Line {
	addr := strings.TrimSpace(cfg.Gateway.ProxyAddr)
	if addr == "" {
		return Line{Name: "gateway", State: Skipped,
			Detail: notConfigured("CELLA_GATEWAY") + "; a boundary that needs a door is refused at create"}
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return Line{Name: "gateway", State: Failed, Detail: "CELLA_GATEWAY: " + err.Error()}
	}
	roots, err := egress.LoadRoots(getenv)
	if err != nil {
		return Line{Name: "gateway", State: Failed, Detail: "the public roots a sandbox behind the gateway verifies against: " + err.Error()}
	}
	return Line{Name: "gateway", State: Ok,
		Detail: fmt.Sprintf("%s resolves to %s; the door is dialed by the sandboxes and not from here; %d public roots from %s",
			addr, strings.Join(addrs, ", "), roots.Count, roots.Source)}
}
