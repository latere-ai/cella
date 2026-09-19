// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"API.Example.COM", "api.example.com"},
		{"  api.example.com  ", "api.example.com"},
		{"api.example.com.", "api.example.com"},
		{"api.example.com:443", "api.example.com"},
		{"*.Example.COM.", "*.example.com"},
		{"", ""},
		// An address with several colons is a v6 literal, not a host and a
		// port, and is left whole rather than cut at the last colon.
		{"[2001:db8::1]:443", "[2001:db8::1]:443"},
		{"2001:db8::1", "2001:db8::1"},
	} {
		if got := NormalizeHost(tc.in); got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitHostPattern(t *testing.T) {
	for _, tc := range []struct {
		in       string
		domain   string
		wildcard bool
	}{
		{"api.example.com", "api.example.com", false},
		{"*.example.com", "example.com", true},
		{"  *.Example.com. ", "example.com", true},
		{"", "", false},
		{"   ", "", false},
	} {
		domain, wildcard := SplitHostPattern(tc.in)
		if domain != tc.domain || wildcard != tc.wildcard {
			t.Errorf("SplitHostPattern(%q) = %q, %v, want %q, %v", tc.in, domain, wildcard, tc.domain, tc.wildcard)
		}
	}
}

// TestHostPatternCovers is the reach rule the hosted compiler proved and this
// contract keeps: coverage is directional, a wildcard never covers its own
// apex, and an exact pattern never covers a wildcard.
func TestHostPatternCovers(t *testing.T) {
	for _, tc := range []struct {
		name, outer, inner string
		want               bool
	}{
		{"exactCoversItself", "api.example.com", "api.example.com", true},
		{"exactDoesNotCoverAnother", "api.example.com", "www.example.com", false},
		{"exactDoesNotCoverAWildcard", "api.example.com", "*.example.com", false},
		{"exactDoesNotCoverItsOwnWildcard", "example.com", "*.example.com", false},
		{"wildcardCoversASubLabel", "*.example.com", "api.example.com", true},
		{"wildcardCoversADeepSubLabel", "*.example.com", "a.b.example.com", true},
		{"wildcardDoesNotCoverItsApex", "*.example.com", "example.com", false},
		{"wildcardCoversItself", "*.example.com", "*.example.com", true},
		{"wildcardCoversANarrowerWildcard", "*.example.com", "*.eu.example.com", true},
		{"wildcardDoesNotCoverAWiderWildcard", "*.eu.example.com", "*.example.com", false},
		{"wildcardDoesNotCoverASiblingDomain", "*.example.com", "api.example.net", false},
		{"suffixIsNotEnough", "*.example.com", "notexample.com", false},
		{"caseAndTrailingDotDoNotMatter", "*.Example.COM.", "API.example.com", true},
		{"theEmptyPatternCoversNothing", "", "api.example.com", false},
		{"nothingCoversTheEmptyPattern", "*.example.com", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostPatternCovers(tc.outer, tc.inner); got != tc.want {
				t.Fatalf("HostPatternCovers(%q, %q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
			}
		})
	}
}

func TestHostCovers(t *testing.T) {
	set := []string{"api.example.com", "*.eu.example.com"}
	for _, tc := range []struct {
		inner string
		want  bool
	}{
		{"api.example.com", true},
		{"a.eu.example.com", true},
		{"*.eu.example.com", true},
		{"*.a.eu.example.com", true},
		{"eu.example.com", false},
		{"*.example.com", false},
		{"other.example.com", false},
	} {
		if got := HostCovers(set, tc.inner); got != tc.want {
			t.Errorf("HostCovers(%v, %q) = %v, want %v", set, tc.inner, got, tc.want)
		}
	}
	if HostCovers(nil, "api.example.com") {
		t.Error("an empty set covered a host")
	}
}

// TestHostMatches is the other question the same patterns answer: not whether
// one pattern contains another, but whether a destination is admitted.
func TestHostMatches(t *testing.T) {
	set := []string{"api.example.com", "*.eu.example.com", ""}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"api.example.com", true},
		{"API.example.com.", true},
		{"a.eu.example.com", true},
		{"deep.a.eu.example.com", true},
		{"eu.example.com", false},
		{"example.com", false},
		{"", false},
	} {
		if got := HostMatches(set, tc.host); got != tc.want {
			t.Errorf("HostMatches(%v, %q) = %v, want %v", set, tc.host, got, tc.want)
		}
	}
	if HostMatches(nil, "api.example.com") {
		t.Error("an empty set admitted a host")
	}
}

func TestEgressModeRank(t *testing.T) {
	if EgressModeRank(EgressOpen) >= EgressModeRank(EgressAllowlist) {
		t.Error("open must rank wider than allowlist")
	}
	if EgressModeRank(EgressAllowlist) >= EgressModeRank(EgressNone) {
		t.Error("allowlist must rank wider than none")
	}
	if EgressModeRank("everything") != -1 {
		t.Error("an unknown mode must rank nowhere")
	}
}
