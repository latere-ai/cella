// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
)

func TestConfiguredAudienceSet(t *testing.T) {
	for _, tc := range []struct {
		value, primary string
		invalid        bool
	}{
		{"", "cella", false},
		{" cella , platform.example ", "cella", false},
		{"platform.example", "platform.example", false},
		{",", "", true},
		{"cella,", "", true},
		{"cella,,other", "", true},
		{"cella, cella", "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg, err := Load(env(identity(t, map[string]string{"CELLA_OIDC_AUDIENCE": tc.value})))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid audience set accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OIDCAudience != tc.primary {
				t.Fatalf("primary audience = %q, want %q", cfg.OIDCAudience, tc.primary)
			}
		})
	}
}
