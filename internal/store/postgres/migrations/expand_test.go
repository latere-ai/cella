// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// contracting are the statements that leave a schema the previous release
// cannot run on: a table, column, type or view that goes away, a rename, a
// column whose type changes or that becomes required, and a required column
// added with nothing to fill it. During a rolling update the new process
// migrates while the old one still serves (spec 076), so an up migration
// holds none of them; a later release drops what an earlier one stopped
// reading.
var contracting = []*regexp.Regexp{
	regexp.MustCompile(`\bdrop\s+(table|column|type|schema|view)\b`),
	regexp.MustCompile(`\balter\s+table\b[^;]*\brename\b`),
	regexp.MustCompile(`\balter\s+column\b[^;]*\b(type|set\s+not\s+null)\b`),
	regexp.MustCompile(`\badd\s+column\b[^;]*\bnot\s+null\b`),
}

// expandOnlyExceptions are the up migrations from before the rule, with why
// each stands.
var expandOnlyExceptions = map[string]string{
	"000004_drop_queue.up.sql": "the queue table was unread by every release that could still be serving when it was dropped",
}

// comment strips SQL line comments, so a sentence about a drop is not a drop.
var comment = regexp.MustCompile(`--[^\n]*`)

// contracts reports the statements of one migration that contract the
// schema.
func contracts(sql string) []string {
	body := strings.ToLower(comment.ReplaceAllString(sql, ""))
	var found []string
	for statement := range strings.SplitSeq(body, ";") {
		statement = strings.Join(strings.Fields(statement), " ")
		if statement == "" {
			continue
		}
		for _, pattern := range contracting {
			if !pattern.MatchString(statement) {
				continue
			}
			// A required column with a default is filled for every row
			// the old binary writes, so it is an expansion.
			if strings.Contains(pattern.String(), "add") && strings.Contains(statement, " default ") {
				continue
			}
			found = append(found, statement)
			break
		}
	}
	return found
}

// TestMigrationsAreExpandOnly: every up migration this binary carries leaves
// a schema the previous release still runs on, but the exceptions named.
func TestMigrationsAreExpandOnly(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		checked++
		body, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		found := contracts(string(body))
		_, excepted := expandOnlyExceptions[e.Name()]
		switch {
		case len(found) > 0 && !excepted:
			t.Errorf("%s contracts the schema the previous release runs on: %q", e.Name(), found)
		case len(found) == 0 && excepted:
			t.Errorf("%s is an exception and contracts nothing; drop it from the exceptions", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no up migration was checked")
	}
}

// TestTheExpandRuleNamesWhatContracts holds the detector itself to what it
// refuses and what it lets through.
func TestTheExpandRuleNamesWhatContracts(t *testing.T) {
	for _, tc := range []struct {
		sql       string
		contracts bool
	}{
		{"drop table queue;", true},
		{"alter table leases drop column address;", true},
		{"alter table objects rename column data to body;", true},
		{"alter table objects rename to things;", true},
		{"alter table objects alter column phase type integer;", true},
		{"alter table objects alter column phase set not null;", true},
		{"alter table leases add column address text not null;", true},
		{"alter table leases add column address text;", false},
		{"alter table leases add column address text not null default '';", false},
		{"create table t (id text primary key);\ncreate index t_id on t (id);", false},
		{"-- the old code would drop table queue here\ncreate table t (id text);", false},
	} {
		if got := len(contracts(tc.sql)) > 0; got != tc.contracts {
			t.Errorf("%q contracts = %v, want %v", tc.sql, got, tc.contracts)
		}
	}
}
