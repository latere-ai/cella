// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
