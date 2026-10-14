// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

// schemaAt reads the version the database's schema is at.
func schemaAt(t *testing.T, dsn string) int64 {
	t.Helper()
	var version int64
	query(t, dsn, `select version from schema_migrations`, nil, &version)
	return version
}

// connect opens one connection to the database, closed when the test ends.
func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TestUnsealTakesTheDatabaseBack is the rollback: rows sealed by this release
// are put back in the clear and the schema is migrated down to the one before
// the sealed columns, where every row decodes as the release before them
// decodes it, values and all. A second run does nothing and leaves the
// schema there, and this release opening the database again seals it again.
func TestUnsealTakesTheDatabaseBack(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(s, "default", store.Journaled)
	want := map[string]v1.Sandbox{
		"sbx_a": sandbox("sbx_a", "one", map[string]string{"API_KEY": "a-env"}, "a-credential"),
		"sbx_b": sandbox("sbx_b", "two", nil, "b-credential"),
	}
	for _, obj := range want {
		if err := bridge.Write(ctx, obj, controller.MutationCreated); err != nil {
			t.Fatal(err)
		}
	}
	writeAsAnEarlierRelease(t, dsn, sandbox("sbx_old", "old", map[string]string{"API_KEY": "old-env"}, "old-credential"), false)
	want["sbx_old"] = sandbox("sbx_old", "old", map[string]string{"API_KEY": "old-env"}, "old-credential")
	if n, err := s.SealCredentials(ctx); err != nil || n != 1 {
		t.Fatalf("SealCredentials = %d, %v", n, err)
	}
	noRowHolds(t, dsn, "a-env", "a-credential", "b-credential", "old-env", "old-credential")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := postgres.Unseal(ctx, postgres.Options{URL: dsn}); !errors.Is(err, store.ErrNoSecretKey) {
		t.Errorf("Unseal without a key = %v, want ErrNoSecretKey", err)
	}
	result, err := postgres.Unseal(ctx, postgres.Options{URL: dsn, Key: storetest.Key, RenewEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	below := migrations.SealedCredentials - 1
	if result.Unsealed != 3 || result.Schema != below {
		t.Fatalf("Unseal = %+v, want 3 rows and the schema at %d", result, below)
	}
	if got := schemaAt(t, dsn); got != int64(below) {
		t.Fatalf("the schema is at %d, want %d", got, below)
	}
	var columns int
	query(t, dsn, `select count(*) from information_schema.columns where table_name = 'objects' and column_name in ('sealed', 'sealed_key')`, nil, &columns)
	if columns != 0 {
		t.Errorf("the schema the release before sealing runs on still holds %d sealed column(s)", columns)
	}
	decodesAsAnEarlierRelease(t, dsn, want)

	again, err := postgres.Unseal(ctx, postgres.Options{URL: dsn, Key: storetest.Key, RenewEvery: time.Hour})
	if err != nil || again.Unsealed != 0 || again.Schema != below || schemaAt(t, dsn) != int64(below) {
		t.Errorf("a second Unseal = %+v, %v, at schema %d, want nothing done", again, err, schemaAt(t, dsn))
	}

	// This release opening the database again migrates it up and reads the
	// rows in the clear, and its rewrite seals them.
	back := open(t, dsn, storetest.Key, time.Hour)
	objects := loaded(t, back)
	for id, obj := range want {
		holds(t, objects, id, obj.Spec.Env, obj.Status.EgressState.Credential)
	}
	if n, err := back.SealCredentials(ctx); err != nil || n != 3 {
		t.Errorf("SealCredentials after the rollback was undone = %d, %v, want 3", n, err)
	}
}

// decodesAsAnEarlierRelease reads every sandbox row as the release before the
// sealed columns does: its data as a v1.Sandbox and its status as a
// v1.SandboxStatus, which refuse the sealed marker, and with every value in
// the row.
func decodesAsAnEarlierRelease(t *testing.T, dsn string, want map[string]v1.Sandbox) {
	t.Helper()
	conn := connect(t, dsn)
	rows, err := conn.Query(t.Context(), `select id, data, status from objects where kind = 'Sandbox' and deleted_at is null order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	read := 0
	for rows.Next() {
		var (
			id           string
			data, status []byte
		)
		if err := rows.Scan(&id, &data, &status); err != nil {
			t.Fatal(err)
		}
		read++
		var obj v1.Sandbox
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Errorf("an earlier release cannot decode %s: %v", id, err)
			continue
		}
		if err := json.Unmarshal(status, &obj.Status); err != nil {
			t.Errorf("an earlier release cannot decode the status of %s: %v", id, err)
			continue
		}
		expected := want[id]
		if !maps.Equal(obj.Spec.Env, expected.Spec.Env) {
			t.Errorf("%s reads the environment %v, want %v", id, obj.Spec.Env, expected.Spec.Env)
		}
		if obj.Status.EgressState == nil || obj.Status.EgressState.Credential != expected.Status.EgressState.Credential {
			t.Errorf("%s reads the boundary %+v", id, obj.Status.EgressState)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if read != len(want) {
		t.Errorf("%d sandbox rows were read, want %d", read, len(want))
	}
}

// TestAMaintenanceRunWaitsForNoWriter: unsealing and rotating are refused
// while a replica holds the writer lease, and change nothing.
func TestAMaintenanceRunWaitsForNoWriter(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	if err := store.ForController(s, "default", store.Journaled).Write(ctx,
		sandbox("sbx_a", "work", map[string]string{"API_KEY": "v"}, "c"), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	if !acquire(t, s, store.WriterLease, "replica-one", time.Minute) {
		t.Fatal("the replica did not take the writer lease")
	}
	o := postgres.Options{URL: dsn, Key: storetest.Key, RenewEvery: time.Hour}
	if _, err := postgres.Unseal(ctx, o); !errors.Is(err, postgres.ErrWriterRunning) {
		t.Errorf("Unseal under a writer = %v, want ErrWriterRunning", err)
	}
	if _, err := postgres.Rotate(ctx, o, storetest.NextKey); !errors.Is(err, postgres.ErrWriterRunning) {
		t.Errorf("Rotate under a writer = %v, want ErrWriterRunning", err)
	}
	if got := schemaAt(t, dsn); got != int64(migrations.SealedCredentials) {
		t.Errorf("a refused run moved the schema to %d", got)
	}
	holds(t, loaded(t, s), "sbx_a", map[string]string{"API_KEY": "v"}, "c")
}

// TestRotateMovesTheKey: a rotation moves every wrapped data key, a secret
// value's and a sealed sandbox row's, after which the old key is refused at
// Open and the new one reads everything; a rotation from a key that is not
// the database's is refused before it moves anything.
func TestRotateMovesTheKey(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "rotate-env"}, "rotate-credential")
	if err := store.ForController(s, "default", store.Journaled).Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Values().Put(ctx, "sec_a", []byte("secret-value"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Rotate(ctx, postgres.Options{URL: dsn, Key: storetest.NextKey}, storetest.Key); !errors.Is(err, postgres.ErrWrongKey) {
		t.Errorf("a rotation from another key = %v, want ErrWrongKey", err)
	}
	if _, err := postgres.Rotate(ctx, postgres.Options{URL: dsn, Key: storetest.Key}, nil); err == nil {
		t.Error("a rotation to no key was run")
	}
	n, err := postgres.Rotate(ctx, postgres.Options{URL: dsn, Key: storetest.Key, RenewEvery: time.Hour}, storetest.NextKey)
	if err != nil || n != 2 {
		t.Fatalf("Rotate = %d, %v, want the secret value's key and the sandbox row's", n, err)
	}
	refused(t, dsn, storetest.Key, postgres.ErrWrongKey, "sbx_a")
	next := open(t, dsn, storetest.NextKey, time.Hour)
	holds(t, loaded(t, next), "sbx_a", obj.Spec.Env, "rotate-credential")
	if err := next.Tx(ctx, func(tx store.Tx) error {
		value, _, err := tx.Values().Open(ctx, "sec_a")
		if err == nil && string(value) != "secret-value" {
			t.Errorf("the secret value reads %q after the rotation", value)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestARotationThatRacesAWriteRotatesNothing: a row sealed again between the
// rotation's read of its key and its write holds a key the rotation never
// read, so the rotation fails whole and every row still opens under the key
// it had.
func TestARotationThatRacesAWriteRotatesNothing(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	writer := store.ForController(open(t, dsn, storetest.Key, time.Hour), "default", store.Journaled)
	obj := sandbox("sbx_a", "work", map[string]string{"API_KEY": "first"}, "c")
	if err := writer.Write(ctx, obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	obj.Spec.Env = map[string]string{"API_KEY": "second"}
	_, err := postgres.RewrapRacing(ctx, s, storetest.Key, storetest.NextKey, func() {
		if err := writer.Write(ctx, obj, controller.MutationUpdated); err != nil {
			t.Errorf("the racing write: %v", err)
		}
	})
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("a rotation raced by a write = %v, want ErrVersionConflict", err)
	}
	holds(t, loaded(t, open(t, dsn, storetest.Key, time.Hour)), "sbx_a", obj.Spec.Env, "c")
	refused(t, dsn, storetest.NextKey, postgres.ErrWrongKey, "sbx_a")
}

// TestASealedPairDoesNotOpenInAnotherRow: the sealed values are bound to the
// row's id, so a pair copied from one sandbox's row into another's does not
// open there.
func TestASealedPairDoesNotOpenInAnotherRow(t *testing.T) {
	dsn := database(t, server(t))
	ctx := t.Context()
	s := open(t, dsn, storetest.Key, time.Hour)
	bridge := store.ForController(s, "default", store.Journaled)
	for _, id := range []string{"sbx_a", "sbx_b"} {
		if err := bridge.Write(ctx, sandbox(id, id, map[string]string{"API_KEY": id}, id), controller.MutationCreated); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, dsn, `update objects set (sealed_key, sealed) = (select sealed_key, sealed from objects where id = 'sbx_a') where id = 'sbx_b'`)
	if _, err := store.ForController(open(t, dsn, storetest.Key, time.Hour), "default", store.Journaled).Load(); err == nil {
		t.Fatal("sbx_b opened the values sealed for sbx_a")
	}
}

// TestOpenRefusesAKeyThatDoesNotOpenTheRows: once a row is sealed, a store
// opened without the key or with another one is refused at Open, naming the
// variable and the first row, rather than taking the writer lease and failing
// its load.
func TestOpenRefusesAKeyThatDoesNotOpenTheRows(t *testing.T) {
	dsn := database(t, server(t))
	s := open(t, dsn, storetest.Key, time.Hour)
	if err := store.ForController(s, "default", store.Journaled).Write(t.Context(),
		sandbox("sbx_a", "work", map[string]string{"API_KEY": "v"}, "c"), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	refused(t, dsn, nil, store.ErrNoSecretKey, "CELLA_SECRET_KEY")
	refused(t, dsn, storetest.NextKey, postgres.ErrWrongKey, "sbx_a")
}

// TestUnsealReadsTheSchemaFirst: a database with no schema holds nothing
// sealed and is left alone, and a schema that is dirty or ahead of this
// binary is refused before anything is opened, as Open refuses it.
func TestUnsealReadsTheSchemaFirst(t *testing.T) {
	admin := server(t)
	fresh := database(t, admin)
	result, err := postgres.Unseal(t.Context(), postgres.Options{URL: fresh, Key: storetest.Key})
	if err != nil || result.Unsealed != 0 || result.Schema != 0 {
		t.Errorf("Unseal of a database with no schema = %+v, %v, want nothing done", result, err)
	}
	for _, tc := range []struct{ name, statement, want string }{
		{"dirty", `update schema_migrations set dirty = true`, "dirty"},
		{"ahead", `update schema_migrations set version = 9999, dirty = false`, "9999"},
	} {
		dsn := database(t, admin)
		if err := open(t, dsn, storetest.Key, time.Hour).Close(); err != nil {
			t.Fatal(err)
		}
		execute(t, dsn, tc.statement)
		if _, err := postgres.Unseal(t.Context(), postgres.Options{URL: dsn, Key: storetest.Key}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Unseal of a schema %s = %v, want it named", tc.name, err)
		}
		if _, err := postgres.Unseal(t.Context(), postgres.Options{URL: "postgres://cella@127.0.0.1:1/cella?connect_timeout=1", Key: storetest.Key}); err == nil {
			t.Error("Unseal of a database that does not answer reported nothing")
		}
	}
	if _, err := postgres.Rotate(t.Context(), postgres.Options{URL: fresh}, storetest.NextKey); !errors.Is(err, store.ErrNoSecretKey) {
		t.Errorf("Rotate without the current key = %v, want ErrNoSecretKey", err)
	}
}
