// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestDefaultMemorySizing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		vars       map[string]string
		start, max string
		problem    string
	}{
		{"unset keeps every sandbox at the fixed default", nil, "", "", ""},
		{"a start and a ceiling", map[string]string{"CELLA_DEFAULT_MEMORY": "128Mi", "CELLA_DEFAULT_MAX_MEMORY": "1Gi"}, "128Mi", "1Gi", ""},
		{"equal start and ceiling", map[string]string{"CELLA_DEFAULT_MEMORY": "1Gi", "CELLA_DEFAULT_MAX_MEMORY": "1024Mi"}, "1Gi", "1024Mi", ""},
		{"a start without a ceiling", map[string]string{"CELLA_DEFAULT_MEMORY": "128Mi"}, "", "", "CELLA_DEFAULT_MEMORY is set without CELLA_DEFAULT_MAX_MEMORY"},
		{"a ceiling without a start", map[string]string{"CELLA_DEFAULT_MAX_MEMORY": "1Gi"}, "", "", "CELLA_DEFAULT_MAX_MEMORY is set without CELLA_DEFAULT_MEMORY"},
		{"a ceiling below the start", map[string]string{"CELLA_DEFAULT_MEMORY": "1Gi", "CELLA_DEFAULT_MAX_MEMORY": "512Mi"}, "", "", "CELLA_DEFAULT_MAX_MEMORY is \"512Mi\", below CELLA_DEFAULT_MEMORY"},
		{"a start that is not a quantity", map[string]string{"CELLA_DEFAULT_MEMORY": "plenty", "CELLA_DEFAULT_MAX_MEMORY": "1Gi"}, "", "", "CELLA_DEFAULT_MEMORY is \"plenty\""},
		{"a ceiling of zero", map[string]string{"CELLA_DEFAULT_MEMORY": "128Mi", "CELLA_DEFAULT_MAX_MEMORY": "0"}, "", "", "CELLA_DEFAULT_MAX_MEMORY is \"0\""},
		{"a start below zero", map[string]string{"CELLA_DEFAULT_MEMORY": "-1Mi", "CELLA_DEFAULT_MAX_MEMORY": "1Gi"}, "", "", "CELLA_DEFAULT_MEMORY is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(env(identity(t, tc.vars)))
			if tc.problem != "" {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("got %v, want a problem naming %q", err, tc.problem)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(cfg.Sizing.StartMemory) != tc.start || string(cfg.Sizing.MemoryCeiling) != tc.max {
				t.Fatalf("sizing = %+v, want %q and %q", cfg.Sizing, tc.start, tc.max)
			}
		})
	}
}
