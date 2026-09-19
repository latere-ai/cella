// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package manifest validates the implemented native Sandbox manifest.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path"
	"regexp"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

type Error struct{ Code, Detail string }

func (e *Error) Error() string       { return e.Code + ": " + e.Detail }
func fail(code, detail string) error { return &Error{code, detail} }

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

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var labelPart = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

// ResolveNative supplies only defaults the native driver can enforce.
func ResolveNative(obj v1.Sandbox, environment string) (v1.Sandbox, error) {
	if obj.APIVersion != v1.APIVersion || obj.Kind != "Sandbox" {
		return obj, fail("invalid_field", "expected the Sandbox API version and kind")
	}
	if obj.Metadata.Name != "" && (len(obj.Metadata.Name) > 63 || !namePattern.MatchString(obj.Metadata.Name)) {
		return obj, fail("invalid_field", "metadata.name must be a DNS label of at most 63 characters")
	}
	for _, m := range []map[string]string{obj.Metadata.Labels, obj.Metadata.Annotations} {
		for k, v := range m {
			if strings.HasPrefix(k, "cella.latere.ai/") {
				return obj, fail("reserved_prefix", "metadata key is reserved")
			}
			if !validKey(k) || len(v) > 4096 {
				return obj, fail("invalid_field", "invalid metadata key or oversized value")
			}
		}
	}
	for _, v := range obj.Metadata.Labels {
		if len(v) > 63 || (v != "" && !labelPart.MatchString(v)) {
			return obj, fail("invalid_field", "metadata label value is invalid")
		}
	}
	if obj.Spec.Environment == "" {
		obj.Spec.Environment = environment
	}
	if obj.Spec.Environment != environment {
		return obj, fail("not_found", "environment is not registered")
	}
	if obj.Spec.Image != "" {
		return obj, fail("capability_unsupported", "native workspaces do not run images")
	}
	if len(obj.Spec.Command) == 0 && len(obj.Spec.Args) > 0 {
		return obj, fail("invalid_field", "args requires a command on native environments")
	}
	if len(obj.Spec.Command) > 0 {
		if err := ValidateExec(append(append([]string{}, obj.Spec.Command...), obj.Spec.Args...), nil, ""); err != nil {
			return obj, err
		}
	}
	if obj.Spec.Workdir == "" {
		obj.Spec.Workdir = "/workspace"
	}
	if obj.Spec.Workdir != "/workspace" {
		return obj, fail("capability_unsupported", "native workspaces require /workspace as initial workdir")
	}
	total := 0
	for k, v := range obj.Spec.Env {
		if !envPattern.MatchString(k) || strings.ContainsRune(v, 0) {
			return obj, fail("invalid_field", "spec.env must contain valid process environment entries")
		}
		if ReservedEnv(k) {
			return obj, fail("reserved_prefix", "spec.env contains a reserved variable")
		}
		total += len(k) + len(v)
	}
	if total > 32768 {
		return obj, fail("invalid_field", "spec.env exceeds 32 KiB")
	}
	obj.Status = v1.SandboxStatus{}
	return obj, nil
}
func validKey(k string) bool {
	parts := strings.Split(k, "/")
	if len(parts) > 2 {
		return false
	}
	value := parts[len(parts)-1]
	if len(value) > 63 || !labelPart.MatchString(value) {
		return false
	}
	if len(parts) == 2 {
		if len(parts[0]) > 253 {
			return false
		}
		for s := range strings.SplitSeq(parts[0], ".") {
			if len(s) > 63 || !namePattern.MatchString(s) {
				return false
			}
		}
	}
	return true
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
	if workdir != "" && (path.Clean(workdir) != workdir || workdir != "/workspace" && !strings.HasPrefix(workdir, "/workspace/")) {
		return fail("invalid_field", "workdir must be an absolute path in /workspace")
	}
	return nil
}
