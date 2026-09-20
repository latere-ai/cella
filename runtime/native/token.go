// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"maps"
	"os"
	"path/filepath"

	driver "latere.ai/x/cella/runtime"
)

// tokenDir and tokenFile are where a native sandbox's identity is projected.
// The workload is a process on this host with no mount namespace of its own,
// so the reserved path of the contract cannot be used; the file lives in a
// directory the driver owns beside the sandbox, rather than in the workspace,
// which is the user's to write. The environment names the path it took.
const (
	tokenDir  = "run/cella"
	tokenFile = "token"
)

// tokenPath is where one sandbox's token lives on this host.
func tokenPath(dir string) string { return filepath.Join(dir, tokenDir, tokenFile) }

// projectToken writes the workload token beside the sandbox and returns the
// environment the projection adds to the workload's own. A create that
// carries no token writes nothing and adds nothing. It takes the token rather
// than the create spec, so an adoption projects through the same path.
func projectToken(dir string, token []byte, env map[string]string) (map[string]string, error) {
	if len(token) == 0 {
		return env, nil
	}
	if err := writeToken(dir, token); err != nil {
		return nil, err
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out[driver.TokenFileEnv] = tokenPath(dir)
	return out, nil
}

// writeToken puts the token at its path, readable by its owner alone. The
// write is a replace rather than a truncate, so a workload reading the file
// while the controller re-projects it reads one whole token or the other and
// never half of each.
func writeToken(dir string, token []byte) error {
	path := tokenPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, token, 0o400); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
