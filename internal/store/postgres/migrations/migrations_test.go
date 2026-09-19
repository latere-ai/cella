// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// TestHighestIsWhatIsEmbedded: the number the store guards against is read
// from the files this binary carries, not written down beside them.
func TestHighestIsWhatIsEmbedded(t *testing.T) {
	highest, err := Highest()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migration is embedded")
	}
	if highest != uint(len(entries)/2) {
		t.Fatalf("the highest migration is %d over %d file(s)", highest, len(entries))
	}
}

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

// TestHighestReadsTheNumber: a file whose name does not start with a number
// is a migration golang-migrate would not order, so it is a failure here
// rather than a schema applied out of order.
func TestHighestReadsTheNumber(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files fstest.MapFS
		want  uint
	}{
		{"one", fstest.MapFS{"000001_state.up.sql": {}}, 1},
		{"the newest of several", fstest.MapFS{
			"000001_state.up.sql": {}, "000012_queue.up.sql": {}, "000002_events.up.sql": {},
		}, 12},
		{"nothing to read", fstest.MapFS{}, 0},
		{"a name with no number", fstest.MapFS{"state.up.sql": {}}, 0},
		{"a number that is not one", fstest.MapFS{"schema_state.up.sql": {}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := highestOf(tc.files)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("%v was accepted", tc.files)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("highest = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
}
