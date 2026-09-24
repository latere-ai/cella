// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/url"

	apidoc "latere.ai/x/cella/api"
	"latere.ai/x/cella/client"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/version"
)

// publicRoutes are what the public listener answers: the /v1 API, the two
// public documents of design 008, and the probes handler, whose /version is
// the build identity a client reads without a bearer.
type publicRoutes struct {
	api      http.Handler
	keySet   http.Handler
	document http.Handler
	probes   http.Handler
}

// publicHandler is the public listener. With no base it is the listener
// design 002 describes: the API under /v1, the documents and the probes at
// the root, and the build line at /.
//
// Under a base every route is reached by client.Route: the base takes the
// place of /v1, and the documents and the build identity sit under it. The
// probes are not public under a base, because the orchestrator reads them on
// the internal listener, and a path outside the base reaches no pattern and
// takes the mux's bare 404. The API itself is not told the base: a request
// under it reaches the API with the base replaced by /v1, so its patterns, its
// spans, its metrics and its log see the route they see at the root.
func publicHandler(base string, r publicRoutes) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+client.Route(base, auth.JWKSPath), r.keySet)
	mux.Handle("GET "+client.Route(base, apidoc.Path), r.document)
	if base == "" {
		mux.Handle("/v1/", r.api)
		for _, p := range []string{"/livez", "/readyz", "/version"} {
			mux.Handle("GET "+p, r.probes)
		}
		mux.HandleFunc("GET /{$}", buildLine)
		return mux
	}
	mux.Handle(base+"/", http.StripPrefix(base, underVersion(r.api)))
	mux.Handle("GET "+client.Route(base, "/version"), http.StripPrefix(base, r.probes))
	mux.HandleFunc("GET "+base+"/{$}", buildLine)
	return mux
}

// underVersion puts /v1 back in front of a path its base was stripped from,
// in the decoded and in the escaped form, which is the inverse of
// client.Route for a route of the API.
func underVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rooted := new(http.Request)
		*rooted = *r
		rooted.URL = new(url.URL)
		*rooted.URL = *r.URL
		rooted.URL.Path = "/v1" + r.URL.Path
		if r.URL.RawPath != "" {
			rooted.URL.RawPath = "/v1" + r.URL.RawPath
		}
		next.ServeHTTP(w, rooted)
	})
}

// buildLine is the one line a person who opens the listener's address reads.
func buildLine(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, version.String())
}
