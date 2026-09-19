// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestRequestBodyBounds(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int64
		invalid bool
	}{
		{"", 1 << 30, false}, {"1Gi", 1 << 30, false}, {"2Mi", 2 << 20, false}, {"64Ki", 64 << 10, false}, {"12345", 12345, false},
		{"0", 0, true}, {"-1", 0, true}, {"garbage", 0, true}, {"9223372036854775807Gi", 0, true}, {"1.5Gi", 0, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := Load(env(identity(t, map[string]string{"CELLA_MAX_UPLOAD_BYTES": tc.raw})))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "CELLA_MAX_UPLOAD_BYTES") {
					t.Fatalf("invalid size: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxUploadBytes != tc.want || cfg.MaxBodyBytes != 65536 {
				t.Fatalf("bounds: upload=%d body=%d", cfg.MaxUploadBytes, cfg.MaxBodyBytes)
			}
		})
	}
	cfg, err := Load(env(identity(t, map[string]string{"CELLA_MAX_BODY_BYTES": "32768"})))
	if err != nil || cfg.MaxBodyBytes != 32768 {
		t.Fatalf("body bound: %d %v", cfg.MaxBodyBytes, err)
	}
}
