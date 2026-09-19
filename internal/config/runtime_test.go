// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestNativeRuntimeOptIn(t *testing.T) {
	for _, tc := range []struct {
		value   string
		allowed bool
	}{
		{"", false}, {"false", false}, {"true", true}, {"invalid", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := Load(env(identity(t, map[string]string{"CELLA_RUNTIME": "native", "CELLA_ALLOW_UNSAFE_NATIVE": tc.value})))
			if (err == nil) != tc.allowed {
				t.Fatalf("opt-in %q: %v", tc.value, err)
			}
		})
	}
}

func TestPodmanSocket(t *testing.T) {
	for _, tc := range []struct {
		value, want string
		invalid     bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"/run/user/1000/podman/podman.sock", "/run/user/1000/podman/podman.sock", false},
		{"podman.sock", "", true},
		{"unix:///run/podman/podman.sock", "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := Load(env(identity(t, map[string]string{"CELLA_RUNTIME": "podman", "CELLA_PODMAN_SOCKET": tc.value})))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "CELLA_PODMAN_SOCKET") {
					t.Fatalf("socket %q: %v", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PodmanSocket != tc.want {
				t.Fatalf("PodmanSocket = %q, want %q", cfg.PodmanSocket, tc.want)
			}
		})
	}
}
