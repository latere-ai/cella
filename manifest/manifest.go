// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package manifest decodes, validates, defaults and resolves the Sandbox
// manifest. Resolve is the one function every surface hands a manifest to and
// gets back the fully defaulted form the data plane is asked for.
package manifest

import (
	"fmt"
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

// The two codes of the admission step (spec 007). They are declared here
// because the step is a seam an operator fills: the client of spec 047
// writes them and Resolve passes them through, and one declaration keeps
// the two halves from drifting apart.
const (
	// CodeAdmissionRefused is a policy refusal: the step decided, and the
	// answer was no. The developer detail is the step's own reason.
	CodeAdmissionRefused = "admission_refused"
	// CodeAdmissionUnavailable is no decision at all. It is never a pass.
	CodeAdmissionUnavailable = "admission_unavailable"
)

func fail(code, detail string) error { return &Error{Code: code, Detail: detail} }
func failAt(code, path, detail string) error {
	return &Error{Code: code, Path: path, Detail: detail}
}
func failPaths(code, detail string, paths []string) error {
	return &Error{Code: code, Path: paths[0], Detail: detail, Paths: paths}
}

// Decode reads one Sandbox manifest, in any syntax design 003 admits, and
// discards the status a client sent before any authorization resource is
// built from it.
func Decode(body []byte, contentType string) (v1.Sandbox, error) {
	var obj v1.Sandbox
	if err := decode(body, contentType, v1.KindSandbox, &obj); err != nil {
		return v1.Sandbox{}, err
	}
	obj.Status = v1.SandboxStatus{}
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

// ValidateExec validates command overrides before they reach a driver, with
// a workdir held to the default workspace.
func ValidateExec(command []string, env map[string]string, workdir string) error {
	return validateExec(command, env, workdir, DefaultWorkspacePath)
}

// ValidateExecFor validates command overrides for one sandbox: a workdir may
// be in the default workspace, in the sandbox's own workspace path (a host
// folder bound in place, spec 081), or in its scratch directories.
func ValidateExecFor(obj v1.Sandbox, command []string, env map[string]string, workdir string) error {
	roots := []string{DefaultWorkspacePath}
	if ws := obj.Spec.Workspace.Path; ws != "" && ws != DefaultWorkspacePath {
		roots = append(roots, ws)
	}
	if p := obj.Status.Paths; p != nil {
		for _, dir := range []string{p.Home, p.Tmp} {
			if dir != "" {
				roots = append(roots, dir)
			}
		}
	}
	return validateExec(command, env, workdir, roots...)
}

func validateExec(command []string, env map[string]string, workdir string, roots ...string) error {
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
	if workdir == "" {
		return nil
	}
	if path.Clean(workdir) == workdir {
		for _, root := range roots {
			if workdir == root || strings.HasPrefix(workdir, strings.TrimSuffix(root, "/")+"/") {
				return nil
			}
		}
	}
	if len(roots) == 1 {
		return fail("invalid_field", "workdir must be an absolute path in "+roots[0])
	}
	return fail("invalid_field", "workdir must be an absolute path in one of "+strings.Join(roots, ", "))
}
