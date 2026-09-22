// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/config"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// TestServeNamesTheStore: the start-up line says which store is in use and
// what a sandbox the data plane lost gets, which is the sentence design 001's
// State section asks an operator to be able to read.
func TestServeNamesTheStore(t *testing.T) {
	_, _, log, stop := startServeWithLog(t, map[string]string{"CELLA_LOST_GRACE": "3m"})
	defer func() { stop() }()
	line := log()
	for _, want := range []string{"store=file", "recovery=off", "lost-grace=3m"} {
		if !strings.Contains(line, want) {
			t.Errorf("the start-up line does not say %q:\n%s", want, line)
		}
	}
}

// TestServeRefusesADatabaseItCannotReach: CELLA_DB_URL selects the store, so a
// database that does not answer is a start-up failure and not a server that
// accepts requests it cannot record.
func TestServeRefusesADatabaseItCannotReach(t *testing.T) {
	var errOut bytes.Buffer
	cfg := identity(t, map[string]string{
		"CELLA_DATA_DIR": t.TempDir(),
		// The port is closed: 127.0.0.1:1 answers nothing on any machine.
		"CELLA_DB_URL": "postgres://cella:cella@127.0.0.1:1/cella?sslmode=disable&connect_timeout=1",
	})
	if code := run(t.Context(), nil, env(cfg), io.Discard, &errOut); code != 1 {
		t.Fatalf("serve returned %d against a database that does not answer: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "cellad:") {
		t.Fatalf("the failure was not reported: %q", errOut.String())
	}
}

// durableStore is a store whose desired state outlives the process, which is
// what the start-up line reports as recovery=on. The adapters of
// internal/store are the real ones; this is the answer the line reads.
type durableStore struct{ objects map[string]v1.Sandbox }

func (s *durableStore) Load() (map[string]v1.Sandbox, error)                  { return s.objects, nil }
func (s *durableStore) Save(map[string]v1.Sandbox) error                      { return nil }
func (s *durableStore) Close() error                                          { return nil }
func (s *durableStore) Durable() bool                                         { return true }
func (s *durableStore) Remove(context.Context, string, string) error          { return nil }
func (s *durableStore) Write(context.Context, v1.Sandbox, string) error       { return nil }
func (s *durableStore) Rebuild(context.Context, string, []driver.State) error { return nil }

// TestTheStartUpLineReportsRecovery: with a durable store a lost sandbox is
// recreated and the line says so, with none it is reaped after the grace and
// the line says that instead (spec 001, State).
func TestTheStartUpLineReportsRecovery(t *testing.T) {
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c, err := controller.Open(t.Context(), controller.Options{
		Store: &durableStore{objects: map[string]v1.Sandbox{}}, Driver: d, Environment: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := recovery(config.Config{DBURL: "postgres://db.example/cella"}, c); got != "store=postgres recovery=on" {
		t.Fatalf("the line says %q", got)
	}
}

// TestOpenStoreReportsADatabaseItCannotReach: the store is opened before the
// listeners, so a database that does not answer is a start-up failure.
func TestOpenStoreReportsADatabaseItCannotReach(t *testing.T) {
	cfg := config.Config{DBURL: "postgres://cella:cella@127.0.0.1:1/cella?sslmode=disable&connect_timeout=1"}
	if _, _, _, _, err := openStore(t.Context(), cfg); err == nil {
		t.Fatal("a database that does not answer was opened")
	}
}
