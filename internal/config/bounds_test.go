// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
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

func TestReaperIntervals(t *testing.T) {
	// The defaults a library caller gets when Options leaves the interval
	// zero are the defaults an operator gets when the variable is unset.
	if DefaultReapInterval != controller.DefaultReapInterval || DefaultTouchInterval != controller.DefaultTouchInterval {
		t.Fatalf("the configuration and the controller disagree: %v/%v and %v/%v",
			DefaultReapInterval, DefaultTouchInterval, controller.DefaultReapInterval, controller.DefaultTouchInterval)
	}
	cfg, err := Load(env(identity(t, nil)))
	if err != nil || cfg.ReapInterval != DefaultReapInterval || cfg.TouchInterval != DefaultTouchInterval {
		t.Fatalf("defaults: %v %v %v", cfg.ReapInterval, cfg.TouchInterval, err)
	}
	for _, name := range []string{"CELLA_REAP_INTERVAL", "CELLA_TOUCH_INTERVAL"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(env(identity(t, map[string]string{name: "5s"})))
			if err != nil {
				t.Fatal(err)
			}
			if got := map[string]time.Duration{"CELLA_REAP_INTERVAL": cfg.ReapInterval, "CELLA_TOUCH_INTERVAL": cfg.TouchInterval}[name]; got != 5*time.Second {
				t.Fatalf("%s is %v", name, got)
			}
			for _, raw := range []string{"never", "-5s", "0s", "999ms", "2h"} {
				_, err := Load(env(identity(t, map[string]string{name: raw})))
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("%s=%s was accepted: %v", name, raw, err)
				}
			}
		})
	}
}
