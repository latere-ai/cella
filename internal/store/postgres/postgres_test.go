// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/postgres"
	"latere.ai/x/cella/internal/store/storetest"
)

// The suite runs against a real server, because half of what this adapter is
// for lives in Postgres and not in Go: the partial unique index behind a name,
// the conditional upsert behind a lease, the transaction behind a rollback.
//
// The server is an ephemeral container started once per test binary. Where no
// container runtime answers, every case here skips and says so; the memory
// adapter's run of the same suite still covers the contract.

// TestMain terminates the container this binary started, whatever the suite
// did, so a failed run leaves no server behind.
func TestMain(m *testing.M) {
	code := m.Run()
	terminate()
	os.Exit(code)
}

// TestPostgresStore runs the whole contract of design 010 against Postgres.
func TestPostgresStore(t *testing.T) {
	admin := server(t)
	storetest.Run(t, func(t storetest.TB, key []byte) store.Store {
		return open(t, database(t, admin), key, time.Hour)
	})
}

// TestAFollowerReadsAnotherReplicasAppend: two replicas over one database. A
// follower of one object on the first reads a record the second committed,
// which never reaches the first's own subscriptions and arrives through the
// read the follower makes on its interval, and then a record the first
// committed itself, in sequence.
func TestAFollowerReadsAnotherReplicasAppend(t *testing.T) {
	dsn := database(t, server(t))
	here, there := open(t, dsn, storetest.Key, time.Hour), open(t, dsn, storetest.Key, time.Hour)
	e := events.NewEmitter(store.EventJournal(here, store.Journaled), slog.New(slog.DiscardHandler))
	f, err := e.Follow(t.Context(), "sbx_a", events.FromNow)
	if err != nil {
		t.Fatalf("following: %v", err)
	}
	defer f.Close()
	// The deadline turns a follower that never reads the journal into a
	// failure rather than a hung test; the wait ends on the record.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for i, replica := range []store.Store{there, here} {
		record, err := events.Mutation(events.TypeExec, "", events.Object{
			Kind: events.KindSandbox, ID: "sbx_a", Name: "work", Owner: "alice",
		}, events.Phase{Phase: "Running"}, events.Actor{Subject: "alice"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EventJournal(replica, store.Journaled).Append(ctx, record); err != nil {
			t.Fatalf("appending on replica %d: %v", i, err)
		}
		got, err := f.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for replica %d's record: %v", i, err)
		}
		if len(got) != 1 || got[0].ID != record.ID || got[0].Seq != int64(i+1) {
			t.Fatalf("the follower sent %+v for replica %d's record %s", got, i, record.ID)
		}
	}
}

// TestTheQueueTableIsGone: the scheduler's queue is the Queued rows of desired
// state (spec 057), so a migrated database holds no second record of it.
func TestTheQueueTableIsGone(t *testing.T) {
	dsn := database(t, server(t))
	if err := open(t, dsn, storetest.Key, time.Hour).Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var found *string
	if err := conn.QueryRow(t.Context(), `select to_regclass('public.queue')::text`).Scan(&found); err != nil {
		t.Fatalf("asking for the table: %v", err)
	}
	if found != nil {
		t.Fatalf("the migrated schema still holds %s", *found)
	}
}

// TestSchemaGuards: a schema the binary does not know is a start-up failure,
// not a server that runs statements against columns it cannot see.
func TestSchemaGuards(t *testing.T) {
	admin := server(t)
	for _, tc := range []struct {
		name, statement, want string
	}{
		{"ahead of the binary", `update schema_migrations set version = 9999, dirty = false`, "9999"},
		{"dirty", `update schema_migrations set dirty = true`, "dirty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := database(t, admin)
			// One clean start writes schema_migrations, which the case moves.
			if err := open(t, dsn, storetest.Key, time.Hour).Close(); err != nil {
				t.Fatalf("closing the first store: %v", err)
			}
			execute(t, dsn, tc.statement)
			_, err := postgres.Open(t.Context(), postgres.Options{URL: dsn, Key: storetest.Key})
			if err == nil {
				t.Fatal("the store started against a schema it does not know")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the failure does not name %q: %v", tc.want, err)
			}
		})
	}
}

// TestLeaseRenewal: the holder keeps a lease whose term is shorter than the
// gap between two reaper ticks, and gives it up at once when it closes.
func TestLeaseRenewal(t *testing.T) {
	admin := server(t)
	dsn := database(t, admin)
	const term = 200 * time.Millisecond
	holder := open(t, dsn, storetest.Key, 40*time.Millisecond)
	other := open(t, dsn, storetest.Key, time.Hour)
	if !acquire(t, holder, "reaper", "replica-one", term) {
		t.Fatal("the first holder did not take the lease")
	}
	time.Sleep(3 * term)
	if acquire(t, other, "reaper", "replica-two", term) {
		t.Fatal("a second replica took a lease the holder is renewing")
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("closing the holder: %v", err)
	}
	if !acquire(t, other, "reaper", "replica-two", term) {
		t.Fatal("a lease the holder released on Close was not free")
	}
}

// TestRenewalStopsWhenTheLeaseMoves: a holder whose lease another replica took
// forgets it, so the renewal loop does not fight over a lease this process no
// longer holds.
func TestRenewalStopsWhenTheLeaseMoves(t *testing.T) {
	admin := server(t)
	dsn := database(t, admin)
	const term = 150 * time.Millisecond
	first := open(t, dsn, storetest.Key, 30*time.Millisecond)
	second := open(t, dsn, storetest.Key, 30*time.Millisecond)
	if !acquire(t, first, "scheduler", "replica-one", term) {
		t.Fatal("the first holder did not take the lease")
	}
	// The first holder stops renewing when its store closes; the term then
	// lapses for good and the second replica keeps what it takes.
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first holder: %v", err)
	}
	if !acquire(t, second, "scheduler", "replica-two", term) {
		t.Fatal("the second replica did not take the free lease")
	}
	time.Sleep(3 * term)
	if !acquire(t, second, "scheduler", "replica-two", term) {
		t.Fatal("the second replica lost the lease it renews")
	}
}

// TestOpenRefusesAURLThatIsNotPostgres keeps a misconfigured deployment from
// starting: CELLA_DB_URL selects the store, and a URL no driver registers
// would otherwise fail on the first statement.
func TestOpenRefusesAURLThatIsNotPostgres(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"not a URL", "://nope"},
		{"another scheme", "mysql://user@localhost:3306/cella"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := postgres.Open(t.Context(), postgres.Options{URL: tc.url}); err == nil {
				t.Fatalf("%q was accepted", tc.url)
			}
		})
	}
}

// TestOpenRefusesAPooledURLThatIsNotPostgres: the serving path opens the
// pooled endpoint where one is named, so a bad one fails at start-up too.
func TestOpenRefusesAPooledURLThatIsNotPostgres(t *testing.T) {
	_, err := postgres.Open(t.Context(), postgres.Options{URL: "postgres://localhost/cella", PoolURL: "mysql://pooler/cella"})
	if err == nil || !strings.Contains(err.Error(), "CELLA_DB_POOL_URL") {
		t.Fatalf("Open = %v, want the pooled URL refused by name", err)
	}
}

// TestOpenRefusesAKeyOfTheWrongLength: a short key fails at start-up rather
// than on the first secret.
func TestOpenRefusesAKeyOfTheWrongLength(t *testing.T) {
	if _, err := postgres.Open(t.Context(), postgres.Options{URL: "postgres://localhost/cella", Key: []byte("short")}); err == nil {
		t.Fatal("a five byte key was accepted")
	}
}

// TestClosedStoreServesNothing: a store whose pool is closed answers no
// transaction and no probe, and closing it twice is not an error.
func TestClosedStoreServesNothing(t *testing.T) {
	admin := server(t)
	s := open(t, database(t, admin), storetest.Key, time.Hour)
	if err := s.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing the store twice: %v", err)
	}
	if err := s.Tx(t.Context(), func(store.Tx) error { return nil }); err == nil {
		t.Error("a closed store ran a transaction")
	}
	if err := s.Ready(t.Context()); err == nil {
		t.Error("a closed store reports itself ready")
	}
}

// TestPoolIsBounded: the pool never grows past the ceiling the fleet shares,
// whatever CELLA_DB_MAX_CONNS asks for.
func TestPoolIsBounded(t *testing.T) {
	admin := server(t)
	for _, tc := range []struct {
		name     string
		maxConns int32
	}{{"the default", 0}, {"asked for more than the ceiling", 1000}, {"one", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := postgres.Open(t.Context(), postgres.Options{
				URL: database(t, admin), MaxConns: tc.maxConns, Key: storetest.Key, RenewEvery: time.Hour,
			})
			if err != nil {
				t.Fatalf("opening the store: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.Ready(t.Context()); err != nil {
				t.Fatalf("the store is not ready: %v", err)
			}
		})
	}
}

// TestDurable: this is the store the lost rule of design 005 recovers from.
func TestDurable(t *testing.T) {
	admin := server(t)
	dsn := database(t, admin)
	s := open(t, dsn, storetest.Key, time.Hour)
	if !s.Durable() {
		t.Fatal("the Postgres store reports itself not durable")
	}
	// What one process wrote, the next process reads: the whole point.
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		_, err := tx.Desired().Put(t.Context(), store.Object{
			Kind: store.KindSandbox, ID: "sbx_durable", Owner: "alice", Name: "one",
			Environment: "env_one", Phase: "Running", Data: []byte(`{}`),
		}, 0)
		return err
	}); err != nil {
		t.Fatalf("writing the object: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	next := open(t, dsn, storetest.Key, time.Hour)
	if err := next.Tx(t.Context(), func(tx store.Tx) error {
		obj, err := tx.Desired().Get(t.Context(), store.KindSandbox, "sbx_durable")
		if err != nil {
			return err
		}
		if obj.Name != "one" || obj.Phase != "Running" {
			t.Errorf("the object reads back %+v", obj)
		}
		return nil
	}); err != nil {
		t.Fatalf("reading the object after a restart: %v", err)
	}
}

// open is one store on one database, closed by the test that opened it unless
// it closes it itself.
func open(t storetest.TB, dsn string, key []byte, renew time.Duration) *postgres.Store {
	t.Helper()
	s, err := postgres.Open(context.Background(), postgres.Options{URL: dsn, Key: key, RenewEvery: renew})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// acquire takes one lease and fails the test on an error.
func acquire(t *testing.T, s *postgres.Store, name, holder string, ttl time.Duration) bool {
	t.Helper()
	var got bool
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		var err error
		got, err = tx.Leases().Acquire(t.Context(), name, holder, ttl)
		return err
	}); err != nil {
		t.Fatalf("acquiring %s: %v", name, err)
	}
	return got
}

// execute runs one statement on one database.
func execute(t *testing.T, dsn, statement string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), statement); err != nil {
		t.Fatalf("running %q: %v", statement, err)
	}
}

// database creates an empty database on the server and returns its URL, so
// one case cannot read another's rows and the migrations run on each.
func database(t storetest.TB, admin string) string {
	t.Helper()
	name := "cella_" + strings.ToLower(rand.Text()[:12])
	conn, err := pgx.Connect(context.Background(), admin)
	if err != nil {
		t.Fatalf("connecting to the server: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drop, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("connecting to drop the database: %v", err)
			return
		}
		defer func() { _ = drop.Close(ctx) }()
		if _, err := drop.Exec(ctx, "drop database if exists "+name+" with (force)"); err != nil {
			t.Errorf("dropping the database: %v", err)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("the server URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

var (
	once      sync.Once
	serverURL string
	serverErr error
	container testcontainers.Container
)

// server is the Postgres every case runs against, started once per binary.
func server(t *testing.T) string {
	t.Helper()
	once.Do(start)
	if serverErr != nil {
		t.Skipf("no container runtime answered, so the Postgres suite did not run: %v", serverErr)
	}
	return serverURL
}

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	socket(ctx)
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("cella_admin"),
		tcpostgres.WithUsername("cella"),
		tcpostgres.WithPassword("cella"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		serverErr = err
		return
	}
	container = ctr
	serverURL, serverErr = ctr.ConnectionString(ctx, "sslmode=disable")
}

func terminate() {
	if container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = container.Terminate(ctx)
	container = nil
}

// socket points the container library at a Podman machine where no Docker
// socket answers, which is what a developer on macOS has. Ryuk, the reaper
// container, is unreliable on rootless Podman, and TestMain terminates what
// this binary started anyway.
func socket(ctx context.Context) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		reaperFor(strings.TrimPrefix(host, "unix://"))
		return
	}
	for _, path := range []string{"/var/run/docker.sock", filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock")} {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			reaperFor(path)
			return
		}
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := runPodman(probe)
	if err != nil {
		return
	}
	path := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		return
	}
	_ = os.Setenv("DOCKER_HOST", "unix://"+path)
	_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
}

// reaperFor turns Ryuk off where the Docker socket that answers is a Podman
// machine's under another name: the link macOS's Podman helper installs at
// /var/run/docker.sock is one, and Ryuk there asks for a bridge network the
// machine does not have, which skips the whole suite.
func reaperFor(path string) {
	if onPodman(path) {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// onPodman reports whether a socket path resolves into a Podman machine's
// directory.
func onPodman(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.Contains(resolved, "podman")
}

func runPodman(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("asking podman for its socket: %w", err)
	}
	return string(out), nil
}

// TestTheReaperIsOffBehindAPodmanLink: a Docker socket that is a link into a
// Podman machine's directory is Podman, and the reaper is turned off for it;
// a Docker socket of Docker's own leaves the reaper as the library sets it.
func TestTheReaperIsOffBehindAPodmanLink(t *testing.T) {
	dir := t.TempDir()
	machine := filepath.Join(dir, "podman", "machine")
	if err := os.MkdirAll(machine, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(machine, "podman.sock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "docker.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "docker", "docker.sock")
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "")
	reaperFor(own)
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "" {
		t.Fatalf("Docker's own socket turned the reaper off: %q", got)
	}
	reaperFor(filepath.Join(dir, "absent.sock"))
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "" {
		t.Fatalf("a socket that does not resolve turned the reaper off: %q", got)
	}
	reaperFor(link)
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "true" {
		t.Fatalf("a link into a Podman machine left the reaper at %q", got)
	}
}
