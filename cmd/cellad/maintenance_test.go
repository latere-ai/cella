// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/postgres"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestMaintenanceRefusesItsConfiguration: unseal and rotate need the
// database and the key, and rotate the next key, and each says which is
// missing; both answer -version and refuse an unknown flag.
func TestMaintenanceRefusesItsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		args []string
		vars map[string]string
		want string
	}{
		{[]string{"unseal"}, nil, "CELLA_DB_URL is unset"},
		{[]string{"unseal"}, map[string]string{"CELLA_DB_URL": "postgres://cella@db.example.com/cella"}, "CELLA_SECRET_KEY is unset"},
		{[]string{"rotate"}, map[string]string{"CELLA_DB_URL": "postgres://cella@db.example.com/cella",
			"CELLA_SECRET_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))}, "CELLA_SECRET_KEY_NEXT is unset"},
	} {
		var errOut bytes.Buffer
		if code := run(t.Context(), tc.args, env(tc.vars), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), tc.want) {
			t.Errorf("%v = %d %q, want 1 naming %q", tc.args, code, errOut.String(), tc.want)
		}
	}
	for _, name := range []string{"unseal", "rotate"} {
		var out bytes.Buffer
		if code := run(t.Context(), []string{name, "-version"}, env(nil), &out, io.Discard); code != 0 || out.Len() == 0 {
			t.Errorf("%s -version = %d %q", name, code, out.String())
		}
		if code := run(t.Context(), []string{name, "-no-such-flag"}, env(nil), io.Discard, io.Discard); code != 2 {
			t.Errorf("%s with an unknown flag = %d, want 2", name, code)
		}
	}
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"no-such-subcommand"}, env(nil), io.Discard, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "unseal, rotate") {
		t.Errorf("an unknown subcommand = %d %q, want the list naming unseal and rotate", code, errOut.String())
	}
}

// TestRotateThenUnsealADatabase runs both commands against a database this
// release sealed: rotate moves it to the next key, unseal under the old key
// is refused, unseal under the new one puts the row back in the clear at the
// schema an earlier release runs on, and a second unseal does nothing. No
// output carries a value.
func TestRotateThenUnsealADatabase(t *testing.T) {
	url := replicaDatabase(t)
	key, next := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	s, err := postgres.Open(t.Context(), postgres.Options{URL: url, Key: key, RenewEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	obj := v1.Sandbox{APIVersion: v1.APIVersion, Kind: v1.KindSandbox, Metadata: v1.Metadata{Name: "work"},
		Spec:   v1.SandboxSpec{Image: "img", Env: map[string]string{"API_KEY": "command-canary"}},
		Status: v1.SandboxStatus{ID: "sbx_a", Owner: "alice", Phase: "Running", EgressState: &v1.EgressState{Credential: "command-credential"}}}
	if err := store.ForController(s, "default", store.Journaled).Write(t.Context(), obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := func(k []byte) string { return base64.StdEncoding.EncodeToString(k) }
	command := func(name string, vars map[string]string) (int, string, string) {
		var out, errOut bytes.Buffer
		vars["CELLA_DB_URL"] = url
		code := run(t.Context(), []string{name}, env(vars), &out, &errOut)
		for _, value := range []string{"command-canary", "command-credential"} {
			if strings.Contains(out.String()+errOut.String(), value) {
				t.Errorf("cellad %s printed %q", name, value)
			}
		}
		return code, out.String(), errOut.String()
	}
	if code, out, errOut := command("rotate", map[string]string{"CELLA_SECRET_KEY": encoded(key), "CELLA_SECRET_KEY_NEXT": encoded(next)}); code != 0 || !strings.Contains(out, "moved 1 data key(s)") {
		t.Fatalf("rotate = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := command("unseal", map[string]string{"CELLA_SECRET_KEY": encoded(key)}); code != 1 || !strings.Contains(errOut, "CELLA_SECRET_KEY does not open") {
		t.Errorf("unseal under the key the rotation left = %d %q", code, errOut)
	}
	for i, want := range []string{"unsealed 1 sandbox row(s); the schema is at version 6", "unsealed 0 sandbox row(s); the schema is at version 6"} {
		if code, out, errOut := command("unseal", map[string]string{"CELLA_SECRET_KEY": encoded(next)}); code != 0 || !strings.Contains(out, want) {
			t.Fatalf("unseal %d = %d %q %q, want %q", i+1, code, out, errOut, want)
		}
	}
}
