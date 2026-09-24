// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api carries the OpenAPI description of the /v1 surface and serves
// it. Design 008 makes the document one of the two public documents: it needs
// no bearer, because a client generator and a reader of the contract reach it
// before they hold a credential.
package api

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/http"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/cella/client"
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
func Handler() http.Handler { return serve(Document) }

// HandlerUnder serves the document a control plane with the public path
// publicPath answers with, Under(publicPath), built once.
func HandlerUnder(publicPath string) (http.Handler, error) {
	doc, err := Under(publicPath)
	if err != nil {
		return nil, err
	}
	return serve(doc), nil
}

func serve(doc []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", MediaType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(doc)
	})
}

// Under is the document as a control plane whose public URL has the path
// publicPath describes itself: each key of paths is the route under that path
// by client.Route, so the servers entry, the address the document was served
// from, plus a path is the address a caller reaches the route at. Nothing else
// changes. An empty path is the carried document, byte for byte.
func Under(publicPath string) ([]byte, error) { return under(Document, publicPath) }

// under is Under over any document, so the refusals of a document that is not
// the carried one are exercised too.
func under(doc []byte, publicPath string) ([]byte, error) {
	if publicPath == "" {
		return doc, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("the API document does not parse: %w", err)
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the API document is not a mapping")
	}
	top := root.Content[0].Content
	rebased := false
	for i := 0; i+1 < len(top); i += 2 {
		if top[i].Value != "paths" || top[i+1].Kind != yaml.MappingNode {
			continue
		}
		paths := top[i+1].Content
		for j := 0; j < len(paths); j += 2 {
			paths[j].Value = client.Route(publicPath, paths[j].Value)
		}
		rebased = true
	}
	if !rebased {
		return nil, errors.New("the API document has no paths to place under " + publicPath)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("the API document does not encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("the API document does not encode: %w", err)
	}
	return out.Bytes(), nil
}
