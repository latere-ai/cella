// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// publicPath is the prefix the cases below serve under: the capability's
// prefix on an origin that serves several services, in the place of /v1.
const publicPath = "/v1/environments"

// underPublicPath sets the public path on a fixture's handler before it has
// answered anything, which is what cellad does from CELLA_PUBLIC_URL.
func underPublicPath(t *testing.T, f *fixture) {
	t.Helper()
	h, ok := f.h.(*handler)
	if !ok {
		t.Fatalf("the fixture serves a %T", f.h)
	}
	h.PublicPath = publicPath
}

// located is one request answered with its status and its Location.
func (f *fixture) located(method, path, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.alice)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode, res.Header.Get("Location")
}

// TestWrittenPathsCarryThePublicPath: the handler serves the rooted routes,
// and every path it writes, the Location of each create and apply and the
// X-Forwarded-Prefix it sends the server inside a sandbox, is under the
// public path in the place of /v1. With no public path each is the rooted
// route, as before.
func TestWrittenPathsCarryThePublicPath(t *testing.T) {
	t.Run("a sandbox", func(t *testing.T) {
		f := setup(t, nil)
		underPublicPath(t, f)
		status, location := f.located(http.MethodPost, "/v1/sandboxes?wait=1", createBody)
		if status != http.StatusCreated || !strings.HasPrefix(location, publicPath+"/sandboxes/sbx_") {
			t.Errorf("the create answered %d with the Location %q", status, location)
		}
		status, location = f.located(http.MethodPut, "/v1/sandboxes/named?wait=1", strings.Replace(createBody, `"work"`, `"named"`, 1))
		if status != http.StatusCreated || !strings.HasPrefix(location, publicPath+"/sandboxes/sbx_") {
			t.Errorf("the apply answered %d with the Location %q", status, location)
		}
		rooted := setup(t, nil)
		if status, location = rooted.located(http.MethodPost, "/v1/sandboxes?wait=1", createBody); !strings.HasPrefix(location, "/v1/sandboxes/sbx_") {
			t.Errorf("with no public path the create answered %d with the Location %q", status, location)
		}
	})

	t.Run("a secret", func(t *testing.T) {
		f := setupSealed(t, nil)
		underPublicPath(t, f)
		status, location := f.located(http.MethodPost, "/v1/secrets", secretBody("github", "api.github.com", "ghp_a"))
		if status != http.StatusCreated || !strings.HasPrefix(location, publicPath+"/secrets/sec_") {
			t.Errorf("the create answered %d with the Location %q", status, location)
		}
		status, location = f.located(http.MethodPut, "/v1/secrets/openai", secretBody("openai", "api.openai.com", "sk-a"))
		if status != http.StatusCreated || !strings.HasPrefix(location, publicPath+"/secrets/sec_") {
			t.Errorf("the apply answered %d with the Location %q", status, location)
		}
	})

	t.Run("an environment", func(t *testing.T) {
		k := setupEnvironments(t)
		underPublicPath(t, k.fixture)
		_, header := k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
		if got := header.Get("Location"); got != publicPath+"/environments/eu-gpu" {
			t.Errorf("the apply answered the Location %q", got)
		}
		created := strings.Replace(environmentBody, `"eu-gpu"`, `"us-gpu"`, 1)
		_, header = k.send(http.MethodPost, "/v1/environments", k.alice, created, nil, http.StatusCreated)
		if got := header.Get("Location"); got != publicPath+"/environments/us-gpu" {
			t.Errorf("the create answered the Location %q", got)
		}
	})

	t.Run("the forwarded prefix", func(t *testing.T) {
		f, _, obj, _ := proxyFixture(t)
		underPublicPath(t, f)
		got := decodeSeen(t, f.proxied(http.MethodGet, "/v1/sandboxes/"+obj.Status.ID+"/ports/web/page", f.alice, nil, nil))
		if want := publicPath + "/sandboxes/" + obj.Status.ID + "/ports/web"; got.Header.Get("X-Forwarded-Prefix") != want {
			t.Errorf("X-Forwarded-Prefix is %q, want %q", got.Header.Get("X-Forwarded-Prefix"), want)
		}
		if got.RequestURI != "/page" {
			t.Errorf("the server inside was asked for %q", got.RequestURI)
		}
		// The redirect stays relative: it resolves against the path the
		// caller asked for, which carries the prefix already.
		res := f.proxied(http.MethodGet, "/v1/sandboxes/"+obj.Status.ID+"/ports/web", f.alice, nil, nil)
		if res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != "web/" {
			t.Errorf("the port path without its slash answered %d with the Location %q", res.StatusCode, res.Header.Get("Location"))
		}
	})

	// The object a create answers names no path, so what a caller follows is
	// the header alone.
	t.Run("the body carries no path", func(t *testing.T) {
		f := setup(t, nil)
		underPublicPath(t, f)
		var obj v1.Sandbox
		if err := json.Unmarshal(f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice, createBody, http.StatusCreated), &obj); err != nil {
			t.Fatal(err)
		}
		raw := string(f.request(http.MethodGet, "/v1/sandboxes/"+obj.Status.ID, f.alice, "", http.StatusOK))
		if strings.Contains(raw, "/v1/") {
			t.Errorf("the object names a path: %s", raw)
		}
	})
}
