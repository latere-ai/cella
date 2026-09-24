// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import "strings"

// versionSegment is the segment every route of the API sits under at the
// root.
const versionSegment = "/v1"

// Route is the path a route is reached at under a base path. A route is
// written as a control plane at the root serves it: /v1/... for the API, and
// /version, /openapi.yaml and /.well-known/jwks.json for the documents beside
// it.
//
// An empty base is the root, where a route is its own path. A base is the
// path a control plane is served under, and it stands in the place of the
// API's /v1, so the public path carries one version segment: under
// /v1/environments the route /v1/sandboxes is /v1/environments/sandboxes and
// the document /openapi.yaml is /v1/environments/openapi.yaml. The server
// mounts its listener, writes its paths and reads a client's URL by this one
// rule.
func Route(base, route string) string {
	if base == "" {
		return route
	}
	if rest, ok := strings.CutPrefix(route, versionSegment); ok && (rest == "" || rest[0] == '/') {
		return base + rest
	}
	return base + route
}
