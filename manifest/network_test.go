// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// enforcing is a container environment whose driver enforces every mode, so a
// test about a field is not also a test about a capability.
func enforcing(name string) v1.Environment {
	env := container(name)
	env.Status.Capabilities.Egress = []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist, v1.EgressOpen}
	return env
}

func enforcingOptions() Options { return environmentOptions(enforcing("default")) }

// withEgress is a manifest whose only interesting field is its boundary.
func withEgress(e v1.Egress) v1.Sandbox {
	obj := sandbox()
	obj.Spec.Network.Egress = e
	return obj
}

func TestHostRule(t *testing.T) {
	for _, tc := range []struct {
		name, pattern string
		ok            bool
	}{
		{"exactName", "api.example.com", true},
		{"wildcard", "*.example.com", true},
		{"twoLabels", "example.com", true},
		{"deepWildcard", "*.eu.example.com", true},
		{"upperCase", "API.Example.COM", true},
		{"trailingDot", "api.example.com.", true},
		{"empty", "", false},
		{"blank", "   ", false},
		{"singleLabel", "localhost", false},
		{"singleLabelName", "intranet", false},
		{"wildcardOverASingleLabel", "*.localhost", false},
		{"localhostName", "db.localhost", false},
		{"mdnsName", "printer.local", false},
		{"ipv4", "10.0.0.1", false},
		{"loopbackAddress", "127.0.0.1", false},
		{"linkLocalAddress", "169.254.169.254", false},
		{"privateAddress", "192.168.1.1", false},
		{"ipv6", "::1", false},
		{"wildcardOverAnAddress", "*.10.0.0.1", false},
		{"withPort", "api.example.com:443", false},
		{"withScheme", "https://api.example.com", false},
		{"innerWildcard", "api.*.example.com", false},
		{"bareWildcard", "*", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHostPattern(pathAllowedHosts+"[0]", tc.pattern)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateHostPattern(%q) = %v, want it accepted", tc.pattern, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateHostPattern(%q) was accepted", tc.pattern)
			}
			if code := codeOf(t, err); code != "invalid_field" {
				t.Fatalf("code = %q, want invalid_field", code)
			}
		})
	}
}

// TestHostRuleReachesBothListsThroughResolve proves the rule is wired into
// stage 1 for each list, not only exported.
func TestHostRuleReachesBothListsThroughResolve(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    v1.Egress
		path string
	}{
		{"allowedHosts", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"ok.example.com", "127.0.0.1"}}, pathAllowedHosts + "[1]"},
		{"deniedHosts", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"*"}}, pathDeniedHosts + "[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refusal(t, withEgress(tc.e), enforcingOptions())
			if err.Code != "invalid_field" || err.Path != tc.path {
				t.Fatalf("error = %+v, want invalid_field at %s", err, tc.path)
			}
		})
	}
}

func TestEgressExclusiveFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		e     v1.Egress
		paths []string
	}{
		{"deniedWithAllowlist", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}, DeniedHosts: []string{"b.example.com"}}, []string{pathEgressMode, pathDeniedHosts}},
		{"allowedWithOpen", v1.Egress{Mode: v1.EgressOpen, AllowedHosts: []string{"a.example.com"}}, []string{pathEgressMode, pathAllowedHosts}},
		{"allowedWithNone", v1.Egress{Mode: v1.EgressNone, AllowedHosts: []string{"a.example.com"}}, []string{pathEgressMode, pathAllowedHosts, pathDeniedHosts}},
		{"deniedWithNone", v1.Egress{Mode: v1.EgressNone, DeniedHosts: []string{"a.example.com"}}, []string{pathEgressMode, pathAllowedHosts, pathDeniedHosts}},
		{"bothListsWithNoMode", v1.Egress{AllowedHosts: []string{"a.example.com"}, DeniedHosts: []string{"b.example.com"}}, []string{pathAllowedHosts, pathDeniedHosts}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refusal(t, withEgress(tc.e), enforcingOptions())
			if err.Code != "exclusive_fields" {
				t.Fatalf("code = %q, want exclusive_fields", err.Code)
			}
			if !slices.Equal(err.Paths, tc.paths) {
				t.Fatalf("paths = %v, want %v", err.Paths, tc.paths)
			}
		})
	}
	// An unknown mode is a value rule, not an exclusive one.
	if err := refusal(t, withEgress(v1.Egress{Mode: "everything"}), enforcingOptions()); err.Code != "invalid_field" || err.Path != pathEgressMode {
		t.Fatalf("error = %+v, want invalid_field at the mode", err)
	}
}

func TestModeInference(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    v1.Egress
		want v1.EgressMode
	}{
		{"nothingSet", v1.Egress{}, v1.EgressOpen},
		{"allowedHostsOnly", v1.Egress{AllowedHosts: []string{"a.example.com"}}, v1.EgressAllowlist},
		{"deniedHostsOnly", v1.Egress{DeniedHosts: []string{"a.example.com"}}, v1.EgressOpen},
		{"modeKept", v1.Egress{Mode: v1.EgressNone}, v1.EgressNone},
		{"modeKeptOverTheList", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}, v1.EgressAllowlist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(t, withEgress(tc.e), enforcingOptions()).Sandbox
			if got.Spec.Network.Egress.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", got.Spec.Network.Egress.Mode, tc.want)
			}
		})
	}
}

func TestNarrowingIsForWorkloads(t *testing.T) {
	allowlist := func(hosts ...string) v1.Egress {
		return v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: hosts}
	}
	open := func(hosts ...string) v1.Egress {
		return v1.Egress{Mode: v1.EgressOpen, DeniedHosts: hosts}
	}
	for _, tc := range []struct {
		name     string
		was, now v1.Egress
		widened  []string
	}{
		{"sameBoundary", allowlist("a.example.com"), allowlist("a.example.com"), nil},
		{"removeAnAllowedHost", allowlist("a.example.com", "b.example.com"), allowlist("a.example.com"), nil},
		{"narrowAWildcard", allowlist("*.example.com"), allowlist("a.example.com"), nil},
		{"allowlistFromOpen", open(), allowlist("a.example.com"), nil},
		{"noneFromAllowlist", allowlist("a.example.com"), v1.Egress{Mode: v1.EgressNone}, nil},
		{"addADeniedHost", open("a.example.com"), open("a.example.com", "b.example.com"), nil},
		{"widenTheMode", allowlist("a.example.com"), open(), []string{pathEgressMode}},
		{"openFromNone", v1.Egress{Mode: v1.EgressNone}, open(), []string{pathEgressMode}},
		{"allowlistFromNone", v1.Egress{Mode: v1.EgressNone}, allowlist("a.example.com"), []string{pathEgressMode}},
		{"addAnAllowedHost", allowlist("a.example.com"), allowlist("a.example.com", "b.example.com"), []string{pathAllowedHosts}},
		{"wildcardOverAnAllowedHost", allowlist("a.example.com"), allowlist("*.example.com"), []string{pathAllowedHosts}},
		{"removeADeniedHost", open("a.example.com", "b.example.com"), open("a.example.com"), []string{pathDeniedHosts}},
		{"narrowADeniedWildcard", open("*.example.com"), open("a.example.com"), []string{pathDeniedHosts}},
		{"aWiderModeIsNamedAloneBecauseTheOtherListIsTheNewModes", allowlist("a.example.com"), open("b.example.com"), []string{pathEgressMode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := resolve(t, withEgress(tc.was), enforcingOptions()).Sandbox
			o := enforcingOptions()
			o.Existing = &existing
			o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
			next := withEgress(tc.now)
			if tc.widened == nil {
				got := resolve(t, next, o).Sandbox
				if got.Spec.Network.Egress.Mode != tc.now.Mode && tc.now.Mode != "" {
					t.Fatalf("mode = %q, want %q", got.Spec.Network.Egress.Mode, tc.now.Mode)
				}
				return
			}
			err := refusal(t, next, o)
			if err.Code != "boundary_widened" {
				t.Fatalf("code = %q, want boundary_widened", err.Code)
			}
			if !slices.Equal(err.Paths, tc.widened) {
				t.Fatalf("paths = %v, want %v", err.Paths, tc.widened)
			}
			// The same change by the owner is the owner's to make.
			o.Actor = Actor{Subject: "https://login.example.com|alice"}
			resolve(t, next, o)
		})
	}
}

func TestEgressCapability(t *testing.T) {
	t.Run("aModeTheEnvironmentDoesNotEnforce", func(t *testing.T) {
		env := container("default")
		env.Status.Capabilities.Egress = []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}
		o := environmentOptions(env)
		err := refusal(t, withEgress(v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"a.example.com"}}), o)
		if err.Code != "capability_unsupported" || err.Path != pathEgressMode {
			t.Fatalf("error = %+v, want capability_unsupported at the mode", err)
		}
	})
	t.Run("aModeItDoesEnforce", func(t *testing.T) {
		got := resolve(t, withEgress(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}), enforcingOptions())
		if len(got.Warnings) != 0 {
			t.Fatalf("warnings = %v, want none", got.Warnings)
		}
	})
	t.Run("anEnvironmentThatEnforcesNothingWarns", func(t *testing.T) {
		for _, e := range []v1.Egress{
			{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}},
			{Mode: v1.EgressNone},
			{Mode: v1.EgressOpen, DeniedHosts: []string{"a.example.com"}},
		} {
			got := resolve(t, withEgress(e), containerOptions())
			if !slices.Contains(got.Warnings, WarningEgressNotEnforced) {
				t.Fatalf("mode %q: warnings = %v, want the unenforced boundary warning", e.Mode, got.Warnings)
			}
		}
	})
	t.Run("aBoundaryThatAsksForNothingDoesNotWarn", func(t *testing.T) {
		got := resolve(t, sandbox(), containerOptions())
		for _, w := range got.Warnings {
			if strings.Contains(w, "egress") {
				t.Fatalf("warnings = %v, want nothing about egress", got.Warnings)
			}
		}
	})
}

// TestEgressSurvivesTheRoundTrip holds the boundary through clone: a resolved
// manifest that shares a list with its input would let a later caller's edit
// reach desired state.
func TestEgressSurvivesTheRoundTrip(t *testing.T) {
	in := withEgress(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}})
	got := resolve(t, in, enforcingOptions()).Sandbox
	in.Spec.Network.Egress.AllowedHosts[0] = "evil.example.com"
	if got.Spec.Network.Egress.AllowedHosts[0] != "a.example.com" {
		t.Fatalf("allowedHosts = %v, want the list the resolver returned", got.Spec.Network.Egress.AllowedHosts)
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var known *Error
	if !errors.As(err, &known) {
		t.Fatalf("error = %v, want a manifest error", err)
	}
	return known.Code
}
