// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"path"
	"strconv"
	"strings"
)

// basePathRule is the shape a base path and the path of CELLA_PUBLIC_URL
// take, written for the problem sentence.
const basePathRule = "a path that begins with / and does not end with one, such as /v1/environments, " +
	"each segment letters, digits, -, ., _ or ~"

// loadBasePath reads CELLA_BASE_PATH, the prefix the public listener answers
// under. Empty is the root. A set value is held to one spelling, because the
// proxy that claims the prefix, the listener's patterns and every path the
// core writes carry it literally, and to the path of CELLA_PUBLIC_URL,
// because the listener answers under the one and the core writes every path
// under the other. An empty base beside a public URL with a path is the core
// behind a proxy that rewrites the prefix away, and is accepted.
//
// publicPath is the path of CELLA_PUBLIC_URL, and publicOK reports whether
// that URL was read at all: an unreadable URL is its own problem, and a
// comparison with it would be a second sentence about the same mistake.
func loadBasePath(getenv Getenv, publicPath string, publicOK bool, problems *[]string) string {
	base := strings.TrimSpace(getenv("CELLA_BASE_PATH"))
	if base == "" {
		return ""
	}
	if !cleanBasePath(base) {
		*problems = append(*problems, "CELLA_BASE_PATH is "+strconv.Quote(base)+"; "+basePathRule)
		return ""
	}
	if publicOK && base != publicPath {
		public := "has no path"
		if publicPath != "" {
			public = "has the path " + strconv.Quote(publicPath)
		}
		*problems = append(*problems, "CELLA_BASE_PATH is "+strconv.Quote(base)+" and CELLA_PUBLIC_URL "+public+
			"; the listener answers under the base and every path the core writes is under the public URL, so the two are one path")
	}
	return base
}

// cleanBasePath reports whether a nonempty value is a base path: rooted, no
// trailing slash, already clean, and every segment made of the characters a
// path carries unescaped.
func cleanBasePath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || path.Clean(p) != p {
		return false
	}
	for segment := range strings.SplitSeq(p[1:], "/") {
		if segment == "" || strings.IndexFunc(segment, func(r rune) bool { return !unreserved(r) }) >= 0 {
			return false
		}
	}
	return true
}

// unreserved is the set RFC 3986 lets a path segment carry without escaping,
// less the sub-delimiters, which a proxy's routing rule may read as syntax.
func unreserved(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '-' || r == '.' || r == '_' || r == '~'
}
