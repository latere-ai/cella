// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

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
