// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/postgres"
	"latere.ai/x/cella/internal/store/postgres/migrations"
	"latere.ai/x/cella/internal/store/storetest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// sandbox is a resolved sandbox whose environment holds env and whose
// boundary holds credential.
func sandbox(id, name string, env map[string]string, credential string) v1.Sandbox {
	return v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: v1.KindSandbox,
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"team": "a"}},
		Spec:     v1.SandboxSpec{Image: "img", Env: env},
		Status: v1.SandboxStatus{
			ID: id, Owner: "alice", Environment: "default", Phase: "Running",
			EgressState: &v1.EgressState{Credential: credential, Version: 3},
		},
	}
}

// noRowHolds reads every row of every table of the database as text and fails
// the test where one holds any of the values. A bytea column reads as hex, so
// a sealed value cannot match.
func noRowHolds(t *testing.T, dsn string, values ...string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(t.Context(), `select table_name from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE' order by table_name`)
	if err != nil {
		t.Fatalf("listing the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("reading the tables: %v", err)
	}
	read := 0
	for _, table := range tables {
		rows, err := conn.Query(t.Context(), `select t::text from `+pgx.Identifier{table}.Sanitize()+` t`)
		if err != nil {
			t.Fatalf("reading %s: %v", table, err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("reading %s: %v", table, err)
		}
		for _, text := range texts {
			read++
			for _, value := range values {
				if strings.Contains(text, value) {
					t.Errorf("a row of %s holds %q: %s", table, value, text)
				}
			}
		}
	}
	if read == 0 {
		t.Fatal("no row was read, so the check would pass over nothing")
	}
}

// someRowHolds is the inverse, for the rows a case expects in the clear.
func someRowHolds(t *testing.T, dsn, value string) {
	t.Helper()
	var n int
	query(t, dsn, `select count(*) from objects t where t::text like '%' || $1 || '%'`, []any{value}, &n)
	if n == 0 {
		t.Fatalf("no row holds %q", value)
	}
}

// query runs one statement that answers one row and scans it.
func query(t *testing.T, dsn, statement string, args []any, into ...any) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := conn.QueryRow(t.Context(), statement, args...).Scan(into...); err != nil {
		t.Fatalf("running %q: %v", statement, err)
	}
}

// writeAsAnEarlierRelease inserts a sandbox row the way every release before
// the sealed columns wrote one: the object and its status as JSON, the
// environment and the credential in them.
func writeAsAnEarlierRelease(t *testing.T, dsn string, obj v1.Sandbox, deleted bool) {
	t.Helper()
	status := obj.Status
	obj.Status = v1.SandboxStatus{}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := json.Marshal(obj.Metadata.Labels)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), `insert into objects (id, kind, owner, name, environment, phase, labels, data, status, deleted_at)
		values ($1, 'Sandbox', $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, case when $9 then now() end)`,
		status.ID, status.Owner, obj.Metadata.Name, status.Environment, status.Phase,
		string(labels), string(data), string(encoded), deleted); err != nil {
		t.Fatalf("writing the row an earlier release wrote: %v", err)
	}
}

// loaded is what a controller loading the store reads.
func loaded(t *testing.T, s store.Store) map[string]v1.Sandbox {
	t.Helper()
	objects, err := store.ForController(s, "default", store.Journaled).Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	return objects
}

// holds asserts the environment and the credential one loaded sandbox reads.
func holds(t *testing.T, objects map[string]v1.Sandbox, id string, env map[string]string, credential string) {
	t.Helper()
	got, ok := objects[id]
	if !ok {
		t.Fatalf("%s was not loaded", id)
	}
	if !maps.Equal(got.Spec.Env, env) {
		t.Errorf("%s reads the environment %v, want %v", id, got.Spec.Env, env)
	}
	if got.Status.EgressState == nil || got.Status.EgressState.Credential != credential || got.Status.EgressState.Version != 3 {
		t.Errorf("%s reads the boundary %+v, want the credential %q at version 3", id, got.Status.EgressState, credential)
	}
}

// TestNoRowHoldsACredential: a create, an update, a status write and a delete
// of a sandbox leave no environment value and no gateway credential in any
// row of any table, and every read puts both back.
func TestNoRowHoldsACredential(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(s, "default", store.Journaled)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "created-env-canary", "PLAIN": "x"}, "credential-canary")
	if err := bridge.Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	noRowHolds(t, dsn, "created-env-canary", "credential-canary")
	holds(t, loaded(t, s), "sbx_a", obj.Spec.Env, "credential-canary")

	// The row says its values are sealed in a form every earlier release
	// refuses to decode, so a controller of one fails to load it rather than
	// serving the sandbox without its environment and its credential.
	var data []byte
	query(t, dsn, `select data from objects where id = 'sbx_a'`, nil, &data)
	var earlier v1.Sandbox
	if err := json.Unmarshal(data, &earlier); err == nil {
		t.Errorf("an earlier release decodes the sealed row as %+v", earlier.Spec)
	}

	obj.Spec.Env = map[string]string{"API_KEY": "updated-env-canary"}
	if err := bridge.Write(ctx, obj, controller.MutationUpdated); err != nil {
		t.Fatal(err)
	}
	noRowHolds(t, dsn, "created-env-canary", "updated-env-canary", "credential-canary")
	holds(t, loaded(t, s), "sbx_a", obj.Spec.Env, "credential-canary")

	// A status write seals the credential it carries beside the sealed
	// environment.
	status, err := json.Marshal(v1.SandboxStatus{ID: "sbx_a", Phase: "Stopped",
		EgressState: &v1.EgressState{Credential: "status-canary", Version: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		return tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_a", status)
	}); err != nil {
		t.Fatal(err)
	}
	noRowHolds(t, dsn, "updated-env-canary", "credential-canary", "status-canary")
	holds(t, loaded(t, s), "sbx_a", obj.Spec.Env, "status-canary")
	var row store.Object
	if err := s.Tx(ctx, func(tx store.Tx) error {
		row, err = tx.Desired().Get(ctx, store.KindSandbox, "sbx_a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var read v1.SandboxStatus
	if err := json.Unmarshal(row.Status, &read); err != nil || read.Phase != "Stopped" {
		t.Errorf("the status reads back %+v, %v", read, err)
	}

	if err := bridge.Remove(ctx, "sbx_a", controller.MutationDeleted); err != nil {
		t.Fatal(err)
	}
	noRowHolds(t, dsn, "updated-env-canary", "status-canary")
	var sealed bool
	query(t, dsn, `select sealed is not null or sealed_key is not null from objects where id = 'sbx_a'`, nil, &sealed)
	if sealed {
		t.Error("a deleted sandbox's row still holds its sealed credentials")
	}
}

// TestRowsAnEarlierReleaseWroteAreRead and then sealed: a row in the clear
// reads as it always did, SealCredentials seals each live one and clears each
// deleted one once, the rows keep their versions so a controller that loaded
// them before still writes, and a second run has nothing to do. It runs over
// a direct connection and over one in the exec mode a transaction pooler
// selects, since the rewrite runs on the serving pool.
func TestRowsAnEarlierReleaseWroteAreRead(t *testing.T) {
	admin := server(t)
	for _, pooled := range []bool{false, true} {
		name := "direct"
		if pooled {
			name = "exec mode"
		}
		t.Run(name, func(t *testing.T) {
			dsn := database(t, admin)
			serving := ""
			if pooled {
				serving = execMode(t, dsn)
			}
			rowsAnEarlierReleaseWrote(t, dsn, openPooled(t, dsn, serving, storetest.Key, time.Hour))
		})
	}
}

func rowsAnEarlierReleaseWrote(t *testing.T, dsn string, s *postgres.Store) {
	ctx := t.Context()
	old := sandbox("sbx_old", "old", map[string]string{"API_KEY": "old-env-canary"}, "old-credential-canary")
	writeAsAnEarlierRelease(t, dsn, old, false)
	writeAsAnEarlierRelease(t, dsn, sandbox("sbx_gone", "gone", map[string]string{"API_KEY": "gone-env-canary"}, "gone-credential-canary"), true)
	plain := sandbox("sbx_plain", "plain", nil, "")
	plain.Status.EgressState = nil
	writeAsAnEarlierRelease(t, dsn, plain, false)
	someRowHolds(t, dsn, "old-env-canary")

	bridge := store.ForController(s, "default", store.Journaled)
	objects, err := bridge.Load()
	if err != nil {
		t.Fatal(err)
	}
	holds(t, objects, "sbx_old", old.Spec.Env, "old-credential-canary")
	if _, ok := objects["sbx_plain"]; !ok {
		t.Fatal("the sandbox with no credentials was not loaded")
	}

	sealedCount, err := s.SealCredentials(ctx)
	if err != nil || sealedCount != 2 {
		t.Fatalf("SealCredentials = %d, %v, want the live row sealed and the deleted one cleared", sealedCount, err)
	}
	noRowHolds(t, dsn, "old-env-canary", "old-credential-canary", "gone-env-canary", "gone-credential-canary")
	holds(t, loaded(t, s), "sbx_old", old.Spec.Env, "old-credential-canary")

	// The controller that loaded before the rewrite writes at the version
	// it read.
	old.Spec.Env = map[string]string{"API_KEY": "after-env-canary"}
	if err := bridge.Write(ctx, old, controller.MutationUpdated); err != nil {
		t.Fatalf("a write after the rewrite: %v", err)
	}
	noRowHolds(t, dsn, "after-env-canary", "old-credential-canary")
	if sealedCount, err = s.SealCredentials(ctx); err != nil || sealedCount != 0 {
		t.Errorf("a second SealCredentials = %d, %v, want nothing to do", sealedCount, err)
	}
}

// TestAWriteInTheClearAfterASealStands: a row a writer that does not seal
// wrote after the values were sealed, which drops the marker and leaves the
// sealed columns behind, reads as that writer wrote it and not as the stale
// sealed values, and the next rewrite seals what it wrote.
func TestAWriteInTheClearAfterASealStands(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "sealed-canary"}, "credential-canary")
	if err := store.ForController(s, "default", store.Journaled).Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	execute(t, dsn, `update objects set data = jsonb_set(data, '{spec,env}', '{"API_KEY":"clear-canary"}'),
		status = jsonb_set(status, '{egressState,credential}', '"clear-credential"') where id = 'sbx_a'`)
	holds(t, loaded(t, s), "sbx_a", map[string]string{"API_KEY": "clear-canary"}, "clear-credential")
	if n, err := s.SealCredentials(ctx); err != nil || n != 1 {
		t.Fatalf("SealCredentials = %d, %v, want the row sealed again", n, err)
	}
	noRowHolds(t, dsn, "clear-canary", "clear-credential", "sealed-canary")
	holds(t, loaded(t, s), "sbx_a", map[string]string{"API_KEY": "clear-canary"}, "clear-credential")
}

// TestAStoreWithoutAKeyKeepsTheValues: with no key the values stay in the
// clear as an earlier release kept them, which SealCredentials leaves for a
// store with a key, and a row sealed by one is refused by a store without it
// rather than read without its values.
func TestAStoreWithoutAKeyKeepsTheValues(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	keyless := open(t, dsn, nil, time.Hour)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "keyless-canary"}, "keyless-credential")
	if err := store.ForController(keyless, "default", store.Journaled).Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	someRowHolds(t, dsn, "keyless-canary")
	holds(t, loaded(t, keyless), "sbx_a", obj.Spec.Env, "keyless-credential")
	if n, err := keyless.SealCredentials(ctx); err != nil || n != 0 {
		t.Fatalf("SealCredentials without a key = %d, %v, want the live row left", n, err)
	}
	someRowHolds(t, dsn, "keyless-canary")

	keyed := open(t, dsn, storetest.Key, time.Hour)
	if n, err := keyed.SealCredentials(ctx); err != nil || n != 1 {
		t.Fatalf("SealCredentials with a key = %d, %v, want the row sealed", n, err)
	}
	noRowHolds(t, dsn, "keyless-canary", "keyless-credential")
	if _, err := store.ForController(keyless, "default", store.Journaled).Load(); !errors.Is(err, store.ErrNoSecretKey) {
		t.Errorf("a store without a key loaded a sealed row: %v", err)
	}
	other := open(t, dsn, storetest.NextKey, time.Hour)
	if _, err := store.ForController(other, "default", store.Journaled).Load(); err == nil {
		t.Error("a store under another key loaded a sealed row")
	}
}

// TestARotationRewrapsTheSealedCredentials: a key rotation moves the data key
// of every sealed sandbox row with the secret values, and the store reads
// them under the new key alone.
func TestARotationRewrapsTheSealedCredentials(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(s, "default", store.Journaled)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "rotated-canary"}, "rotated-credential")
	if err := bridge.Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	n, err := bridge.Rewrap(ctx, storetest.Key, storetest.NextKey)
	if err != nil || n != 1 {
		t.Fatalf("Rewrap = %d, %v, want the one sealed row", n, err)
	}
	holds(t, loaded(t, s), "sbx_a", obj.Spec.Env, "rotated-credential")
	holds(t, loaded(t, open(t, dsn, storetest.NextKey, time.Hour)), "sbx_a", obj.Spec.Env, "rotated-credential")
	if _, err := store.ForController(open(t, dsn, storetest.Key, time.Hour), "default", store.Journaled).Load(); err == nil {
		t.Error("the key the rotation moved away from still opens the row")
	}
	noRowHolds(t, dsn, "rotated-canary", "rotated-credential")
}

// TestSealCredentialsReportsWhatItCannotSeal: a row whose sealed values do not
// open under the store's key is named and left as it stands, the rest are
// sealed, and a closed store seals nothing.
func TestSealCredentialsReportsWhatItCannotSeal(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	other := open(t, dsn, storetest.NextKey, time.Hour)
	foreign := sandbox("sbx_foreign", "foreign", map[string]string{"API_KEY": "foreign"}, "foreign-credential")
	if err := store.ForController(other, "default", store.Journaled).Write(ctx, foreign, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	// A credential written in the clear beside the row's sealed values makes
	// it a row to rewrite, and the rewrite opens the values first.
	execute(t, dsn, `update objects set status = jsonb_set(status, '{egressState,credential}', '"clear-credential"') where id = 'sbx_foreign'`)
	writeAsAnEarlierRelease(t, dsn, sandbox("sbx_old", "old", map[string]string{"API_KEY": "old-canary"}, "old-credential"), false)
	s := open(t, dsn, storetest.Key, time.Hour)
	n, err := s.SealCredentials(ctx)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "sbx_foreign") {
		t.Fatalf("SealCredentials = %d, %v, want sbx_old sealed and sbx_foreign reported", n, err)
	}
	noRowHolds(t, dsn, "old-canary", "old-credential")
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SealCredentials(gone); err == nil {
		t.Error("a rewrite whose caller gave up reported nothing")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SealCredentials(ctx); !errors.Is(err, postgres.ErrClosed) {
		t.Errorf("a closed store's SealCredentials = %v, want ErrClosed", err)
	}
}

// TestTheSealedColumnsOutliveTheirValues: the down migration refuses a table
// in which a row still holds sealed credentials, which no statement could put
// back, and drops the columns from one in which none does.
func TestTheSealedColumnsOutliveTheirValues(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(s, "default", store.Journaled)
	if err := bridge.Write(ctx, sandbox("sbx_a", "work", map[string]string{"API_KEY": "v"}, "c"), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(migrations.FS, "000007_sealed_credentials.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("the down migration over a sealed row = %v, want it refused", err)
	}
	if err := bridge.Remove(ctx, "sbx_a", controller.MutationDeleted); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(down)); err != nil {
		t.Fatalf("the down migration over no sealed row: %v", err)
	}
	var columns int
	query(t, dsn, `select count(*) from information_schema.columns where table_name = 'objects' and column_name in ('sealed', 'sealed_key')`, nil, &columns)
	if columns != 0 {
		t.Errorf("the down migration left %d sealed column(s)", columns)
	}
}

// TestSealCredentialsReadsEveryPage: a table holding more rows in the clear
// than one page has every one of them sealed in one run.
func TestSealCredentialsReadsEveryPage(t *testing.T) {
	dsn := database(t, server(t))
	const rows = store.DefaultPageLimit + 7
	s := open(t, dsn, storetest.Key, time.Hour)
	execute(t, dsn, `insert into objects (id, kind, owner, name, data, status)
		select 'sbx_' || lpad(n::text, 4, '0'), 'Sandbox', 'alice', 'n' || n,
			jsonb_build_object('spec', jsonb_build_object('env', jsonb_build_object('API_KEY', 'paged-canary'))),
			jsonb_build_object('egressState', jsonb_build_object('credential', 'paged-credential'))
		from generate_series(1, `+strconv.Itoa(rows)+`) n`)
	n, err := s.SealCredentials(t.Context())
	if err != nil || n != rows {
		t.Fatalf("SealCredentials = %d, %v, want %d", n, err, rows)
	}
	noRowHolds(t, dsn, "paged-canary", "paged-credential")
	if got := len(loaded(t, s)); got != rows {
		t.Errorf("%d sandboxes loaded after the rewrite, want %d", got, rows)
	}
}

// TestAWriteCannotForgeTheMarker: an object whose environment is already the
// marker is refused rather than stored as a sealed row with nothing sealed,
// and a status write over a row this store cannot open fails rather than
// dropping what it holds.
func TestAWriteCannotForgeTheMarker(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	forged := store.Object{Kind: store.KindSandbox, ID: "sbx_forged", Owner: "alice", Name: "forged",
		Data: []byte(`{"spec":{"env":"sealed"}}`), Status: []byte(`{"egressState":{"credential":"c"}}`)}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, forged, 0)
		return err
	}); err == nil {
		t.Error("an object already marked sealed was stored")
	}
	other := open(t, dsn, storetest.NextKey, time.Hour)
	if err := store.ForController(other, "default", store.Journaled).Write(ctx,
		sandbox("sbx_a", "work", map[string]string{"API_KEY": "v"}, "c"), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		return tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_a", []byte(`{"phase":"Stopped"}`))
	}); err == nil || !strings.Contains(err.Error(), "sbx_a") {
		t.Errorf("a status write over a row sealed under another key = %v", err)
	}
	keyless := open(t, dsn, nil, time.Hour)
	if err := keyless.Tx(ctx, func(tx store.Tx) error {
		return tx.Desired().PutStatus(ctx, store.KindSandbox, "sbx_gone", []byte(`{}`))
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a status write on a store without a key over no row = %v, want ErrNotFound", err)
	}
}
