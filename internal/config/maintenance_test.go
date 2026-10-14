// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/base64"
	"maps"
	"strings"
	"testing"
)

// TestLoadMaintenance: a maintenance run reads the database and the key, and
// the next key for a rotation, and refuses one that is missing or unusable,
// naming each.
func TestLoadMaintenance(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	next := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	base := map[string]string{
		"CELLA_DB_URL":          "postgres://cella@db.example.com:5432/cella",
		"CELLA_SECRET_KEY":      key,
		"CELLA_SECRET_KEY_NEXT": next,
	}
	got, err := LoadMaintenance(env(base), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.DBURL != base["CELLA_DB_URL"] || len(got.SecretKey) != 32 || got.NextSecretKey[0] != 2 {
		t.Fatalf("LoadMaintenance = %+v", got)
	}
	if got, err := LoadMaintenance(env(base), false); err != nil || got.NextSecretKey != nil {
		t.Fatalf("an unseal read the next key: %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		unset  string
		set    map[string]string
		rotate bool
		want   string
	}{
		{"no database", "CELLA_DB_URL", nil, false, "CELLA_DB_URL is unset"},
		{"no key", "CELLA_SECRET_KEY", nil, false, "CELLA_SECRET_KEY is unset"},
		{"no next key", "CELLA_SECRET_KEY_NEXT", nil, true, "CELLA_SECRET_KEY_NEXT is unset"},
		{"a short key", "", map[string]string{"CELLA_SECRET_KEY": "c2hvcnQ="}, false, "CELLA_SECRET_KEY is not usable"},
		{"a short next key", "", map[string]string{"CELLA_SECRET_KEY_NEXT": "c2hvcnQ="}, true, "CELLA_SECRET_KEY_NEXT is not usable"},
		{"not a database", "", map[string]string{"CELLA_DB_URL": "mysql://db"}, false, "CELLA_DB_URL must be a postgres:// URL"},
	} {
		vars := map[string]string{}
		for k, v := range base {
			if k != tc.unset {
				vars[k] = v
			}
		}
		maps.Copy(vars, tc.set)
		if _, err := LoadMaintenance(env(vars), tc.rotate); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: LoadMaintenance = %v, want %q", tc.name, err, tc.want)
		}
	}
}
