// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"
)

// clients are the import paths that open a Postgres connection or apply a
// migration. One of them in another package would be a second place that
// knows the schema, which is what design 010 gives internal/store alone.
var clients = []string{
	"github.com/jackc/pgx",
	"github.com/golang-migrate/migrate",
	"latere.ai/x/pkg/pgxmigrate",
}

// allowed is where those imports belong: this package, and nothing else.
const allowed = "latere.ai/x/cella/internal/store/postgres"

// TestDriverIsConfined reads what every package imports, not what it reaches:
// cmd/cellad reaches pgx through this package by design, and the rule is that
// no other package writes the import itself.
func TestDriverIsConfined(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list",
		"-f", "{{.ImportPath}} {{join .Imports \" \"}} {{join .TestImports \" \"}}", "./...").Output()
	if err != nil {
		t.Fatalf("reading the imports: %v", err)
	}
	read := 0
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		pkg, imports, found := strings.Cut(scanner.Text(), " ")
		if !found {
			continue
		}
		read++
		if pkg == allowed {
			continue
		}
		for _, client := range clients {
			for imported := range strings.FieldsSeq(imports) {
				if imported == client || strings.HasPrefix(imported, client+"/") {
					t.Errorf("%s imports %s: the Postgres driver and the migrator live in %s alone", pkg, imported, allowed)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the imports: %v", err)
	}
	if read == 0 {
		t.Fatal("no package was read, so the check would pass over nothing")
	}
}
