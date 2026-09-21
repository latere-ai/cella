// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api carries the OpenAPI description of the /v1 surface and serves
// it. Design 008 makes the document one of the two public documents: it needs
// no bearer, because a client generator and a reader of the contract reach it
// before they hold a credential.
package api

import (
	_ "embed"
	"net/http"
)

// Document is the API description as this build serves it. It is the source a
// generated client is built from, and internal/api's TestTheDocumentAndTheMuxAgree
// holds it to the routes the server registers.
//
//go:embed openapi.yaml
var Document []byte

// Path is where design 008 serves the document.
const Path = "/openapi.yaml"

// MediaType is the type the document is served as. It is the first of the
// three YAML types design 003 names, which is the one a generator looks for.
const MediaType = "application/yaml"

// Handler serves the document. It caches for an hour: the description changes
// only when the binary does, and a generator that fetches it on every build
// should not pay for a round trip each time.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(Document)
	})
}
