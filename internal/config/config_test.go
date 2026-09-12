// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: ":8080", InternalAddr: ":8081", DataDir: "/var/lib/cella", Runtime: "k8s"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	c, err := Load(env(map[string]string{
		"CELLA_PUBLIC_ADDR":   "127.0.0.1:9000",
		"CELLA_INTERNAL_ADDR": "127.0.0.1:9001",
		"CELLA_DATA_DIR":      "/tmp/cella",
		"CELLA_RUNTIME":       "native",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001", DataDir: "/tmp/cella", Runtime: "native"}
	if c != want {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CELLA_RUNTIME":       "docker",
		"CELLA_PUBLIC_ADDR":   "nope",
		"CELLA_INTERNAL_ADDR": "nope",
	}))
	if err == nil {
		t.Fatal("Load() accepted a bad runtime and two bad addresses")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`CELLA_INTERNAL_ADDR is "nope", not a host:port address`,
		`CELLA_PUBLIC_ADDR is "nope", not a host:port address`,
		`CELLA_RUNTIME is "docker"; one of k8s, podman, native`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if i, j := strings.Index(got, "CELLA_INTERNAL_ADDR is"), strings.Index(got, "CELLA_PUBLIC_ADDR"); i > j {
		t.Errorf("problems are not sorted by name:\n%s", got)
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(env(map[string]string{"CELLA_RUNTIME": "  ", "CELLA_DATA_DIR": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime != DefaultRuntime || c.DataDir != DefaultDataDir {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"CELLA_PUBLIC_ADDR": "127.0.0.1:9000", "CELLA_INTERNAL_ADDR": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "must differ from CELLA_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"CELLA_PUBLIC_ADDR": "127.0.0.1:0", "CELLA_INTERNAL_ADDR": "127.0.0.1:0"})); err != nil {
		t.Fatal(err)
	}
}
