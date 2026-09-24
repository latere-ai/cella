// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestBasePathRules holds CELLA_BASE_PATH and the path of CELLA_PUBLIC_URL to
// one spelling and to each other: the root by default, a set base only with
// the public URL that names it, a public path alone accepted as the core
// behind a rewriting proxy, and every refusal naming the variable it is
// about.
func TestBasePathRules(t *testing.T) {
	for _, tc := range []struct {
		name              string
		base, public      string
		wantBase, wantP   string
		problem, problem2 string
	}{
		{name: "the root by default", public: "https://cella.example.com"},
		{name: "blank is unset", base: "  ", public: "https://cella.example.com/"},
		{name: "mounted", base: "/v1/environments", public: "https://api.example.com/v1/environments",
			wantBase: "/v1/environments", wantP: "/v1/environments"},
		{name: "a trailing slash on the public URL is dropped", base: "/v1/environments", public: "https://api.example.com/v1/environments/",
			wantBase: "/v1/environments", wantP: "/v1/environments"},
		{name: "behind a rewrite", public: "https://api.example.com/v1/environments", wantP: "/v1/environments"},
		{name: "the unreserved characters", base: "/a-b/c.d/e_f/g~h/0", public: "https://api.example.com/a-b/c.d/e_f/g~h/0",
			wantBase: "/a-b/c.d/e_f/g~h/0", wantP: "/a-b/c.d/e_f/g~h/0"},
		{name: "a base the public URL does not name", base: "/v1/environments", public: "https://api.example.com",
			problem: `CELLA_BASE_PATH is "/v1/environments" and CELLA_PUBLIC_URL has no path`},
		{name: "a base that differs from the public path", base: "/v1/environments", public: "https://api.example.com/v1/sandboxes",
			problem: `CELLA_BASE_PATH is "/v1/environments" and CELLA_PUBLIC_URL has the path "/v1/sandboxes"`},
		{name: "not rooted", base: "v1/environments", public: "https://api.example.com/v1/environments",
			problem: `CELLA_BASE_PATH is "v1/environments"; a path that begins with /`},
		{name: "a trailing slash", base: "/v1/environments/", public: "https://api.example.com/v1/environments",
			problem: `CELLA_BASE_PATH is "/v1/environments/"`},
		{name: "the root spelled out", base: "/", public: "https://api.example.com", problem: `CELLA_BASE_PATH is "/"`},
		{name: "not clean", base: "/v1/../environments", public: "https://api.example.com/environments",
			problem: `CELLA_BASE_PATH is "/v1/../environments"`},
		{name: "an empty segment", base: "/v1//environments", public: "https://api.example.com/v1/environments",
			problem: `CELLA_BASE_PATH is "/v1//environments"`},
		{name: "a query", base: "/v1/environments?x=1", public: "https://api.example.com/v1/environments",
			problem: `CELLA_BASE_PATH is "/v1/environments?x=1"`},
		{name: "an escape", base: "/v1/env%20s", public: "https://api.example.com/v1/environments",
			problem: `CELLA_BASE_PATH is "/v1/env%20s"`},
		{name: "a public URL with a query", public: "https://api.example.com/v1/environments?tenant=a",
			problem: `CELLA_PUBLIC_URL is "https://api.example.com/v1/environments?tenant=a", which carries a query or a fragment`},
		{name: "a public URL with a fragment", public: "https://api.example.com/v1/environments#top",
			problem: `CELLA_PUBLIC_URL is "https://api.example.com/v1/environments#top", which carries a query or a fragment`},
		{name: "a public path that is not clean", public: "https://api.example.com/v1/./environments",
			problem: `CELLA_PUBLIC_URL has the path "/v1/./environments"`},
		// A public URL that is refused is one problem, not a second one about
		// the base that would have matched it.
		{name: "an unreadable public URL beside a base", base: "/v1/environments", public: "api.example.com/v1/environments",
			problem: `CELLA_PUBLIC_URL is "api.example.com/v1/environments", not an absolute`, problem2: "CELLA_BASE_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(identity(t, map[string]string{
				"CELLA_BASE_PATH":  tc.base,
				"CELLA_PUBLIC_URL": tc.public,
			})))
			if tc.problem != "" {
				if err == nil {
					t.Fatalf("Load accepted CELLA_BASE_PATH %q beside CELLA_PUBLIC_URL %q", tc.base, tc.public)
				}
				if !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("the problem is %q, want it to say %q", err, tc.problem)
				}
				if tc.problem2 != "" && strings.Contains(err.Error(), tc.problem2) {
					t.Fatalf("the problem %q also names %s", err, tc.problem2)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.BasePath != tc.wantBase || c.PublicPath != tc.wantP {
				t.Fatalf("the base is %q and the public path %q, want %q and %q", c.BasePath, c.PublicPath, tc.wantBase, tc.wantP)
			}
		})
	}
}
