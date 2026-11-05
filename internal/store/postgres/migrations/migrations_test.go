// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// TestEveryMigrationIsReversible: golang-migrate reads a pair, and a release
// that carried only one half would fail on the first down.
func TestEveryMigrationIsReversible(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		up, found := strings.CutSuffix(e.Name(), ".up.sql")
		if !found {
			continue
		}
		if _, err := fs.Stat(FS, up+".down.sql"); err != nil {
			t.Errorf("%s has no down migration: %v", e.Name(), err)
		}
	}
}

// TestSealedCredentialsNamesItsMigration: the number a rollback migrates down
// past is the migration that adds the sealed columns.
func TestSealedCredentialsNamesItsMigration(t *testing.T) {
	matches, err := fs.Glob(FS, fmt.Sprintf("%06d_*.up.sql", SealedCredentials))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("migration %d is %v, want one file", SealedCredentials, matches)
	}
	body, err := fs.ReadFile(FS, matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "add column sealed_key") || !strings.Contains(string(body), "add column sealed ") {
		t.Errorf("%s does not add the sealed columns", matches[0])
	}
}
