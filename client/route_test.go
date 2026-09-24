// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/cella/client"
)

// TestRouteReplacesTheVersionSegment: under a base the API's /v1 is the base,
// a document sits under the base, and with no base every route is its own
// path.
func TestRouteReplacesTheVersionSegment(t *testing.T) {
	for _, tc := range []struct{ base, route, want string }{
		{"", "/v1/sandboxes", "/v1/sandboxes"},
		{"", "/.well-known/jwks.json", "/.well-known/jwks.json"},
		{"/v1/environments", "/v1/sandboxes", "/v1/environments/sandboxes"},
		{"/v1/environments", "/v1/environments/gpu", "/v1/environments/environments/gpu"},
		{"/v1/environments", "/v1/sandboxes?limit=5", "/v1/environments/sandboxes?limit=5"},
		{"/v1/environments", "/v1", "/v1/environments"},
		{"/v1/environments", "/v1/", "/v1/environments/"},
		{"/v1/environments", "/.well-known/jwks.json", "/v1/environments/.well-known/jwks.json"},
		{"/v1/environments", "/openapi.yaml", "/v1/environments/openapi.yaml"},
		{"/v1/environments", "/version", "/v1/environments/version"},
		{"/v1/environments", "/", "/v1/environments/"},
		// A segment that only starts with v1 is not the version.
		{"/v1/environments", "/v1beta/x", "/v1/environments/v1beta/x"},
		{"/v1", "/v1/sandboxes", "/v1/sandboxes"},
	} {
		if got := client.Route(tc.base, tc.route); got != tc.want {
			t.Errorf("Route(%q, %q) = %q, want %q", tc.base, tc.route, got, tc.want)
		}
	}
}

// TestEveryCallComposesUnderABaseURL: a URL with a path is the base the
// control plane is served under, and every call, the three sockets and the
// build identity included, reaches its route under it with the base in the
// place of /v1. The server refuses each call, which is enough: the path is
// what it saw.
func TestEveryCallComposesUnderABaseURL(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "That object does not exist.", nil)
	})
	c := f.client(client.Config{URL: f.server.URL + "/v1/environments/"})
	ctx := t.Context()
	m := client.JSON([]byte(`{}`))
	for _, tc := range []struct {
		name         string
		call         func() error
		method, path string
	}{
		{"create a sandbox", func() error { _, _, err := c.CreateSandbox(ctx, m); return err }, "POST", "/v1/environments/sandboxes"},
		{"apply a sandbox", func() error { _, _, err := c.ApplySandbox(ctx, "dev", m); return err }, "PUT", "/v1/environments/sandboxes/dev"},
		{"get a sandbox", func() error { _, _, err := c.GetSandbox(ctx, "dev"); return err }, "GET", "/v1/environments/sandboxes/dev"},
		{"a reference that needs escaping", func() error { _, _, err := c.GetSandbox(ctx, "a b"); return err }, "GET", "/v1/environments/sandboxes/a b"},
		{"list sandboxes", func() error { _, _, err := c.ListSandboxes(ctx, client.ListOptions{}); return err }, "GET", "/v1/environments/sandboxes"},
		{"stop", func() error { _, _, err := c.StopSandbox(ctx, "dev"); return err }, "POST", "/v1/environments/sandboxes/dev/stop"},
		{"delete a secret", func() error { _, err := c.Delete(ctx, client.KindSecret, "api"); return err }, "DELETE", "/v1/environments/secrets/api"},
		{"exec", func() error {
			_, _, err := c.Exec(ctx, "dev", client.ExecRequest{Command: []string{"true"}})
			return err
		}, "POST", "/v1/environments/sandboxes/dev/exec"},
		{"logs", func() error { _, err := c.Logs(ctx, "dev", client.LogOptions{}); return err }, "GET", "/v1/environments/sandboxes/dev/logs"},
		{"a file", func() error { _, err := c.FileGet(ctx, "dev", "a.txt"); return err }, "GET", "/v1/environments/sandboxes/dev/files/content"},
		{"egress records", func() error { _, _, err := c.EgressRecords(ctx, "dev", 10); return err }, "GET", "/v1/environments/sandboxes/dev/egress"},
		{"an environment", func() error { _, _, err := c.GetEnvironment(ctx, "gpu"); return err }, "GET", "/v1/environments/environments/gpu"},
		{"list environments", func() error { _, _, err := c.ListEnvironments(ctx, client.ListOptions{}); return err }, "GET", "/v1/environments/environments"},
		{"mint a key", func() error { _, _, err := c.MintEnvironmentKey(ctx, "gpu"); return err }, "POST", "/v1/environments/environments/gpu/keys"},
		{"list the keys", func() error { _, err := c.ListEnvironmentKeys(ctx, "gpu"); return err }, "GET", "/v1/environments/environments/gpu/keys"},
		{"revoke a key", func() error { return c.RevokeEnvironmentKey(ctx, "gpu", "01JTI") }, "DELETE", "/v1/environments/environments/gpu/keys/01JTI"},
		{"events", func() error { _, _, err := c.Events(ctx, "dev", client.EventOptions{}); return err }, "GET", "/v1/environments/events"},
		{"follow events", func() error { _, err := c.FollowEvents(ctx, client.FollowOptions{}); return err }, "GET", "/v1/environments/events"},
		{"the exec socket", func() error {
			_, err := c.ExecSession(ctx, "dev", client.ExecRequest{Command: []string{"sh"}})
			return err
		}, "GET", "/v1/environments/sandboxes/dev/exec"},
		{"the attach socket", func() error { _, err := c.AttachSession(ctx, "dev", client.ExecRequest{}); return err }, "GET", "/v1/environments/sandboxes/dev/attach"},
		{"the dial socket", func() error { _, err := c.Dial(ctx, "dev", 8080); return err }, "GET", "/v1/environments/sandboxes/dev/dial/8080"},
		{"the build identity", func() error { _, err := c.ServerVersion(ctx); return err }, "GET", "/v1/environments/version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.seen())
			if err := tc.call(); err == nil {
				t.Fatal("the refused call succeeded")
			}
			calls := f.seen()
			if len(calls) == before {
				t.Fatal("the call reached no server")
			}
			got := calls[len(calls)-1]
			if got.Method != tc.method || got.Path != tc.path {
				t.Fatalf("the call was %s %s, want %s %s", got.Method, got.Path, tc.method, tc.path)
			}
			if strings.Contains(got.Path, "/v1/environments/v1/") {
				t.Fatalf("the call carried two versions: %s", got.Path)
			}
		})
	}
}
