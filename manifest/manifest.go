// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package manifest decodes, validates, defaults and resolves the Sandbox
// manifest. Resolve is the one function every surface hands a manifest to and
// gets back the fully defaulted form the data plane is asked for.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// Error is the one manifest error: a code from the contract's table, the JSON
// path of the offending field, and one sentence in the user register. Paths
// carries every offending path where a rule names more than one, such as an
// update that changes several immutable fields.
type Error struct {
	Code   string
	Path   string
	Detail string
	Paths  []string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code + ": " + e.Path + ": " + e.Detail
}

func fail(code, detail string) error { return &Error{Code: code, Detail: detail} }
func failAt(code, path, detail string) error {
	return &Error{Code: code, Path: path, Detail: detail}
}
func failPaths(code, detail string, paths []string) error {
	return &Error{Code: code, Path: paths[0], Detail: detail, Paths: paths}
}

// Decode accepts exactly one JSON manifest, refuses unknown fields, and
// discards client status before any authorization resource is constructed.
func Decode(body []byte, contentType string) (v1.Sandbox, error) {
	var obj v1.Sandbox
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || media != "application/json" {
		return obj, fail("unsupported_media_type", "expected application/json")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		code := "bad_request"
		if strings.Contains(err.Error(), "unknown field") {
			code = "unknown_field"
		}
		return obj, fail(code, err.Error())
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return obj, fail("multi_document", "expected exactly one JSON object")
	}
	obj.Status = v1.SandboxStatus{}
	if obj.APIVersion != v1.APIVersion {
		return obj, fail("unsupported_version", "apiVersion must be "+v1.APIVersion)
	}
	if obj.Kind != "Sandbox" {
		return obj, fail("unsupported_kind", "kind must be Sandbox")
	}
	return obj, nil
}

// ReservedEnv identifies process settings managed by Cella's boundary.
func ReservedEnv(key string) bool {
	if strings.HasPrefix(key, "CELLA_") {
		return true
	}
	switch strings.ToUpper(key) {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE":
		return true
	}
	return false
}

// ValidateExec validates command overrides before they reach a driver.
func ValidateExec(command []string, env map[string]string, workdir string) error {
	if len(command) == 0 || command[0] == "" {
		return fail("invalid_field", "command must not be empty")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return fail("invalid_field", "command contains NUL")
		}
	}
	for k, v := range env {
		if !envPattern.MatchString(k) || ReservedEnv(k) || strings.ContainsRune(v, 0) {
			return fail("invalid_field", fmt.Sprintf("invalid exec environment variable %q", k))
		}
	}
	if workdir != "" && (path.Clean(workdir) != workdir || workdir != DefaultWorkspacePath && !strings.HasPrefix(workdir, DefaultWorkspacePath+"/")) {
		return fail("invalid_field", "workdir must be an absolute path in "+DefaultWorkspacePath)
	}
	return nil
}
