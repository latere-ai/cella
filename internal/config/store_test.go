// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store/postgres"
)

// TestStoreDefaults: a deployment that names no database keeps every state in
// the snapshot under CELLA_DATA_DIR, and the defaults here are the ones the
// packages that read them hold.
func TestStoreDefaults(t *testing.T) {
	cfg, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBURL != "" || cfg.SecretKey != nil {
		t.Errorf("an unset database left %q and a key of %d bytes", cfg.DBURL, len(cfg.SecretKey))
	}
	if cfg.DBMaxConns != DefaultDBMaxConns || cfg.LostGrace != DefaultLostGrace {
		t.Errorf("the defaults are %d connections and a grace of %s", cfg.DBMaxConns, cfg.LostGrace)
	}
	if DefaultLostGrace != controller.DefaultLostGrace {
		t.Errorf("the configuration and the controller disagree on the grace: %s and %s",
			DefaultLostGrace, controller.DefaultLostGrace)
	}
	if DefaultDBMaxConns != postgres.DefaultMaxConns || MaxDBMaxConns != postgres.MaxMaxConns {
		t.Errorf("the configuration and the store disagree on the pool: %d/%d and %d/%d",
			DefaultDBMaxConns, MaxDBMaxConns, postgres.DefaultMaxConns, postgres.MaxMaxConns)
	}
}

// TestStoreConfiguration reads every variable this slice adds and refuses
// every value that would fail later instead of at start-up.
func TestStoreConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for _, tc := range []struct {
		name, variable, value string
		invalid               bool
		check                 func(Config) bool
	}{
		{name: "a postgres url", variable: "CELLA_DB_URL", value: "postgres://cella@db.example:5432/cella",
			check: func(c Config) bool { return c.DBURL == "postgres://cella@db.example:5432/cella" }},
		{name: "the longer scheme", variable: "CELLA_DB_URL", value: "postgresql://db.example/cella",
			check: func(c Config) bool { return c.DBURL != "" }},
		{name: "another engine", variable: "CELLA_DB_URL", value: "mysql://db.example/cella", invalid: true},
		{name: "not a url", variable: "CELLA_DB_URL", value: "://db.example", invalid: true},
		{name: "no host", variable: "CELLA_DB_URL", value: "postgres:///cella", invalid: true},
		{name: "a pool", variable: "CELLA_DB_MAX_CONNS", value: "8",
			check: func(c Config) bool { return c.DBMaxConns == 8 }},
		{name: "no connection", variable: "CELLA_DB_MAX_CONNS", value: "0", invalid: true},
		{name: "a negative pool", variable: "CELLA_DB_MAX_CONNS", value: "-1", invalid: true},
		{name: "more than the ceiling", variable: "CELLA_DB_MAX_CONNS", value: "33", invalid: true},
		{name: "not a number", variable: "CELLA_DB_MAX_CONNS", value: "many", invalid: true},
		{name: "a key", variable: "CELLA_SECRET_KEY", value: key,
			check: func(c Config) bool { return len(c.SecretKey) == 32 }},
		{name: "a key of the wrong length", variable: "CELLA_SECRET_KEY", value: base64.StdEncoding.EncodeToString([]byte("short")), invalid: true},
		{name: "a key that is not base64", variable: "CELLA_SECRET_KEY", value: "not a key!", invalid: true},
		{name: "a grace", variable: "CELLA_LOST_GRACE", value: "5m",
			check: func(c Config) bool { return c.LostGrace == 5*time.Minute }},
		{name: "a grace of never", variable: "CELLA_LOST_GRACE", value: "never", invalid: true},
		{name: "a grace of nothing", variable: "CELLA_LOST_GRACE", value: "0s", invalid: true},
		{name: "a grace past the bound", variable: "CELLA_LOST_GRACE", value: "2h", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(identity(t, map[string]string{tc.variable: tc.value})))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), tc.variable) {
					t.Fatalf("%s=%s was accepted: %v", tc.variable, tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s=%s was refused: %v", tc.variable, tc.value, err)
			}
			if !tc.check(cfg) {
				t.Fatalf("%s=%s read back wrong: %+v", tc.variable, tc.value, cfg)
			}
		})
	}
}

// TestPooledEndpointNeedsTheDirectOne: migrations hold a session lock, which
// a transaction-mode pooler drops, so the pooled name never stands alone.
func TestPooledEndpointNeedsTheDirectOne(t *testing.T) {
	_, err := Load(env(identity(t, map[string]string{"CELLA_DB_POOL_URL": "postgres://cella@pooler.example:6432/cella"})))
	if err == nil || !strings.Contains(err.Error(), "CELLA_DB_POOL_URL needs CELLA_DB_URL") {
		t.Fatalf("Load = %v, want the pooled endpoint refused without the direct one", err)
	}
	cfg, err := Load(env(identity(t, map[string]string{
		"CELLA_DB_URL":      "postgres://cella@db.example:5432/cella",
		"CELLA_DB_POOL_URL": "postgres://cella@pooler.example:6432/cella",
	})))
	if err != nil || cfg.DBPoolURL != "postgres://cella@pooler.example:6432/cella" {
		t.Fatalf("Load = %+v, %v, want both endpoints accepted together", cfg.DBPoolURL, err)
	}
	if _, err := Load(env(identity(t, map[string]string{
		"CELLA_DB_URL":      "postgres://cella@db.example:5432/cella",
		"CELLA_DB_POOL_URL": "mysql://pooler.example/cella",
	}))); err == nil || !strings.Contains(err.Error(), "CELLA_DB_POOL_URL must be a postgres:// URL") {
		t.Fatalf("Load = %v, want a pooled endpoint of another engine refused by name", err)
	}
}
