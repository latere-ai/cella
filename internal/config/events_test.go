// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

// TestSinkStartupRules: a half-configured sink is a start-up failure, because
// the alternative is a deployment that believes it delivers and does not, or
// one posting records nobody signed.
func TestSinkStartupRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "a URL with no secret",
			env:  map[string]string{"CELLA_EVENTS_URL": "https://sink.example/events"},
			want: "CELLA_EVENTS_URL is set without CELLA_EVENTS_SECRET",
		},
		{
			name: "a secret with no URL",
			env:  map[string]string{"CELLA_EVENTS_SECRET": "s"},
			want: "CELLA_EVENTS_SECRET is set without CELLA_EVENTS_URL",
		},
		{
			name: "a plaintext sink on another host",
			env: map[string]string{
				"CELLA_EVENTS_URL": "http://sink.example/events", "CELLA_EVENTS_SECRET": "s",
			},
			want: "CELLA_EVENTS_INSECURE_SINK=1",
		},
		{
			name: "a URL that is not one",
			env: map[string]string{
				"CELLA_EVENTS_URL": "sink.example/events", "CELLA_EVENTS_SECRET": "s",
			},
			want: "CELLA_EVENTS_URL must be an http:// or https:// URL",
		},
		{
			name: "a scheme that is not HTTP",
			env: map[string]string{
				"CELLA_EVENTS_URL": "ftp://sink.example/events", "CELLA_EVENTS_SECRET": "s",
			},
			want: "CELLA_EVENTS_URL must be an http:// or https:// URL",
		},
		{
			name: "a timeout that is not a duration",
			env: map[string]string{
				"CELLA_EVENTS_URL": "https://sink.example", "CELLA_EVENTS_SECRET": "s",
				"CELLA_EVENTS_TIMEOUT": "soon",
			},
			want: "CELLA_EVENTS_TIMEOUT",
		},
		{
			name: "a timeout longer than a minute",
			env: map[string]string{
				"CELLA_EVENTS_URL": "https://sink.example", "CELLA_EVENTS_SECRET": "s",
				"CELLA_EVENTS_TIMEOUT": "2m",
			},
			want: "CELLA_EVENTS_TIMEOUT is 2m0s; between 1s and 1m",
		},
		{
			name: "a retry window shorter than a minute",
			env: map[string]string{
				"CELLA_EVENTS_URL": "https://sink.example", "CELLA_EVENTS_SECRET": "s",
				"CELLA_EVENTS_RETRY_WINDOW": "5s",
			},
			want: "CELLA_EVENTS_RETRY_WINDOW is 5s; at least 1m",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(identity(t, tc.env)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the start-up said %v, want %q", err, tc.want)
			}
		})
	}
}

// TestSinkAccepted: the shapes a deployment and the stubs run with.
func TestSinkAccepted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		url     string
		secrets []string
	}{
		{name: "no sink", env: nil},
		{
			name: "one secret over TLS",
			env: map[string]string{
				"CELLA_EVENTS_URL": "https://sink.example/events", "CELLA_EVENTS_SECRET": "one",
			},
			url: "https://sink.example/events", secrets: []string{"one"},
		},
		{
			name: "two secrets while one is being rotated",
			env: map[string]string{
				"CELLA_EVENTS_URL": "https://sink.example/events", "CELLA_EVENTS_SECRET": " one , two ",
			},
			url: "https://sink.example/events", secrets: []string{"one", "two"},
		},
		{
			name: "a loopback sink needs no escape hatch",
			env: map[string]string{
				"CELLA_EVENTS_URL": "http://127.0.0.1:9000/events", "CELLA_EVENTS_SECRET": "one",
			},
			url: "http://127.0.0.1:9000/events", secrets: []string{"one"},
		},
		{
			name: "localhost is loopback",
			env: map[string]string{
				"CELLA_EVENTS_URL": "http://localhost:9000/events", "CELLA_EVENTS_SECRET": "one",
			},
			url: "http://localhost:9000/events", secrets: []string{"one"},
		},
		{
			name: "the escape hatch admits a plaintext sink elsewhere",
			env: map[string]string{
				"CELLA_EVENTS_URL": "http://sink.example/events", "CELLA_EVENTS_SECRET": "one",
				"CELLA_EVENTS_INSECURE_SINK": "1",
			},
			url: "http://sink.example/events", secrets: []string{"one"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(identity(t, tc.env)))
			if err != nil {
				t.Fatalf("the start-up refused a usable sink: %v", err)
			}
			if cfg.Events.URL != tc.url {
				t.Errorf("the sink is %q, want %q", cfg.Events.URL, tc.url)
			}
			if cfg.Events.Enabled() != (tc.url != "") {
				t.Errorf("Enabled() = %v with URL %q", cfg.Events.Enabled(), cfg.Events.URL)
			}
			if len(cfg.Events.Secrets) != len(tc.secrets) {
				t.Fatalf("the secrets are %v, want %v", cfg.Events.Secrets, tc.secrets)
			}
			for i := range tc.secrets {
				if cfg.Events.Secrets[i] != tc.secrets[i] {
					t.Errorf("the secrets are %v, want %v", cfg.Events.Secrets, tc.secrets)
				}
			}
		})
	}
}

// TestSinkDefaults: the two bounds an operator does not have to name.
func TestSinkDefaults(t *testing.T) {
	cfg, err := Load(env(identity(t, map[string]string{
		"CELLA_EVENTS_URL": "https://sink.example", "CELLA_EVENTS_SECRET": "one",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Events.Timeout != 10*time.Second || cfg.Events.RetryWindow != 24*time.Hour {
		t.Errorf("the defaults are %s and %s", cfg.Events.Timeout, cfg.Events.RetryWindow)
	}
	set, err := Load(env(identity(t, map[string]string{
		"CELLA_EVENTS_URL": "https://sink.example", "CELLA_EVENTS_SECRET": "one",
		"CELLA_EVENTS_TIMEOUT": "3s", "CELLA_EVENTS_RETRY_WINDOW": "1h",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if set.Events.Timeout != 3*time.Second || set.Events.RetryWindow != time.Hour {
		t.Errorf("the set values are %s and %s", set.Events.Timeout, set.Events.RetryWindow)
	}
}
