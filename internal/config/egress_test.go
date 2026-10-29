// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// egressEnv is the smallest environment the gateway starts in, with extra
// values over it.
func egressEnv(extra map[string]string) Getenv {
	values := map[string]string{
		"CELLA_URL":             "https://control.example.test",
		"CELLA_ENVIRONMENT_KEY": "a-key",
	}
	maps.Copy(values, extra)
	return func(name string) string { return values[name] }
}

// TestLoadEgressReadsPrivateRanges: unset, the gateway dials public addresses
// alone; set, each range is read, masked, whichever separator it came with.
func TestLoadEgressReadsPrivateRanges(t *testing.T) {
	c, err := LoadEgress(egressEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PrivateRanges) != 0 {
		t.Fatalf("PrivateRanges = %v, want none by default", c.PrivateRanges)
	}
	c, err = LoadEgress(egressEnv(map[string]string{"CELLA_EGRESS_PRIVATE_RANGES": " 10.96.0.0/12,192.168.7.9/24\nfd00::/8 "}))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.96.0.0/12"), netip.MustParsePrefix("192.168.7.0/24"), netip.MustParsePrefix("fd00::/8")}
	if !slices.Equal(c.PrivateRanges, want) {
		t.Fatalf("PrivateRanges = %v, want %v", c.PrivateRanges, want)
	}
}

// TestLoadEgressRefusesARangeThatIsNotOne names the entry, so the operator
// sees which of several is wrong.
func TestLoadEgressRefusesARangeThatIsNotOne(t *testing.T) {
	for _, raw := range []string{"registry.internal", "10.0.0.1", "10.0.0.0/8,10.0.0.0/33"} {
		_, err := LoadEgress(egressEnv(map[string]string{"CELLA_EGRESS_PRIVATE_RANGES": raw}))
		if err == nil || !strings.Contains(err.Error(), "CELLA_EGRESS_PRIVATE_RANGES") {
			t.Fatalf("LoadEgress(%q) = %v, want the variable named", raw, err)
		}
	}
}
