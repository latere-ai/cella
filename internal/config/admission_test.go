// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

// TestAdmissionDefaults: an installation that configures no endpoint runs
// the built-in identity step, with no image default and the contract's
// deadline waiting for the day one is configured.
func TestAdmissionDefaults(t *testing.T) {
	c, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Admission.Enabled() || c.Admission.Mode() != "builtin" {
		t.Fatalf("admission = %+v", c.Admission)
	}
	if c.Admission.DefaultImage != "" {
		t.Fatalf("default image = %q, want none", c.Admission.DefaultImage)
	}
	if c.Admission.Timeout != DefaultAdmissionTimeout {
		t.Fatalf("timeout = %v, want %v", c.Admission.Timeout, DefaultAdmissionTimeout)
	}
}

// TestAdmissionIsRead: every variable of spec 007 reaches the field named
// for it, and a configured endpoint puts the start-up line in webhook mode.
func TestAdmissionIsRead(t *testing.T) {
	c, err := Load(env(identity(t, map[string]string{
		"CELLA_ADMISSION_URL":     "https://admission.example/hook",
		"CELLA_ADMISSION_TOKEN":   "  secret  ",
		"CELLA_ADMISSION_TIMEOUT": "5s",
		"CELLA_DEFAULT_IMAGE":     "registry.example/base:1",
	})))
	if err != nil {
		t.Fatal(err)
	}
	want := Admission{
		URL: "https://admission.example/hook", Token: "secret",
		Timeout: 5 * time.Second, DefaultImage: "registry.example/base:1",
	}
	if c.Admission != want {
		t.Fatalf("admission = %+v, want %+v", c.Admission, want)
	}
	if !c.Admission.Enabled() || c.Admission.Mode() != "webhook" {
		t.Fatalf("mode = %q", c.Admission.Mode())
	}
}

// TestAdmissionStartupRules: the endpoint carries a manifest and a bearer,
// so a URL with no bearer, a URL that is not one, and a cleartext URL off
// a loopback address each stop the start rather than the first apply.
func TestAdmissionStartupRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		says string
	}{
		{"no bearer", map[string]string{
			"CELLA_ADMISSION_URL": "https://admission.example/hook",
		}, "CELLA_ADMISSION_TOKEN is unset"},
		{"not a URL", map[string]string{
			"CELLA_ADMISSION_URL": "admission.example", "CELLA_ADMISSION_TOKEN": "secret",
		}, "not an absolute http:// or https:// URL"},
		{"cleartext off loopback", map[string]string{
			"CELLA_ADMISSION_URL": "http://admission.example/hook", "CELLA_ADMISSION_TOKEN": "secret",
		}, "other than loopback"},
		{"a deadline too short", map[string]string{
			"CELLA_ADMISSION_TIMEOUT": "10ms",
		}, "CELLA_ADMISSION_TIMEOUT is 10ms"},
		{"a deadline too long", map[string]string{
			"CELLA_ADMISSION_TIMEOUT": "5m",
		}, "CELLA_ADMISSION_TIMEOUT is 5m0s"},
		{"a deadline that is not one", map[string]string{
			"CELLA_ADMISSION_TIMEOUT": "soon",
		}, "CELLA_ADMISSION_TIMEOUT is \"soon\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(identity(t, tc.vars)))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("err = %v, want one naming %q", err, tc.says)
			}
		})
	}
	// A cleartext endpoint on this machine is the one place a bearer
	// travels in the clear, which is how a local endpoint is developed.
	if _, err := Load(env(identity(t, map[string]string{
		"CELLA_ADMISSION_URL": "http://127.0.0.1:9100/hook", "CELLA_ADMISSION_TOKEN": "secret",
	}))); err != nil {
		t.Fatal(err)
	}
}
