// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/cella/manifest"
)

// The syntaxes an object response takes. Design 008 gives every one of them
// JSON in the order the Go type declares, or YAML where the caller named one
// of the three types design 003 also accepts on a body.
const (
	answerJSON = iota
	answerYAML
)

// yamlTypes are the three media types design 003 names for YAML, which design
// 008 answers in as well as reads.
var yamlTypes = []string{"application/yaml", "application/x-yaml", "text/yaml"}

// negotiate reads one Accept header and answers the syntax a response takes,
// or false where the header excludes both. An absent header is JSON, which is
// what a client that stated no preference reads.
//
// JSON wins wherever both are acceptable: it is the syntax the OpenAPI
// document describes and the one every generated client reads. Preference
// weights are not ranked, only read for the exclusion q=0 states, because the
// choice here is between two syntaxes of one document and not between
// representations a caller would rank.
func negotiate(accept string) (int, bool) {
	if strings.TrimSpace(accept) == "" {
		return answerJSON, true
	}
	acceptsJSON, acceptsYAML := false, false
	for entry := range strings.SplitSeq(accept, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
		if err != nil {
			continue
		}
		if q, ok := params["q"]; ok {
			if weight, err := strconv.ParseFloat(q, 64); err == nil && weight == 0 {
				continue
			}
		}
		switch {
		case media == "*/*", media == "application/*", media == "application/json":
			acceptsJSON = true
		case slices.Contains(yamlTypes, media):
			acceptsYAML = true
		}
	}
	switch {
	case acceptsJSON:
		return answerJSON, true
	case acceptsYAML:
		return answerYAML, true
	}
	return answerJSON, false
}

// acceptable negotiates one request and records the answer's syntax on the
// response writer. It refuses before the handler runs, so a create whose
// Accept this server cannot satisfy is refused before the object exists
// rather than after.
func acceptable(w http.ResponseWriter, r *http.Request) bool {
	form, ok := negotiate(r.Header.Get("Accept"))
	if !ok {
		respondError(w, &manifest.Error{Code: "not_acceptable",
			Detail: "Accept names neither JSON nor one of application/yaml, application/x-yaml and text/yaml"})
		return false
	}
	if form == answerYAML {
		noteYAML(w)
	}
	return true
}

// noteYAML records on the response writer that this request's answer is YAML.
// The writer carries it because respond is reached from handlers several
// calls down that hold no request.
func noteYAML(w http.ResponseWriter) {
	for {
		if o, ok := w.(*observed); ok {
			o.yaml = true
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

// wantsYAML reports what noteYAML recorded.
func wantsYAML(w http.ResponseWriter) bool {
	for {
		if o, ok := w.(*observed); ok {
			return o.yaml
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// yamlOf renders one JSON document as YAML through a node, which keeps the
// order the encoder wrote. A converter that decoded into a Go map would sort
// the keys and lose the struct order design 008's document records.
func yamlOf(raw []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	return yaml.Marshal(&node)
}

// respondYAML writes one object as YAML. A body that cannot be rendered is
// the server's own failure and is answered as one, because a half-written
// document would be read as the whole.
func respondYAML(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err == nil {
		var out []byte
		if out, err = yamlOf(raw); err == nil {
			w.Header().Set("Content-Type", yamlTypes[0])
			w.WriteHeader(status)
			_, _ = w.Write(out)
			return
		}
	}
	respondError(w, &manifest.Error{Code: "bad_request", Detail: "the answer could not be rendered as YAML: " + err.Error()})
}
