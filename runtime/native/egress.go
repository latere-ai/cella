// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"maps"
	"os"
	"path/filepath"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

// caFile is where the gateway's authority is projected for a native sandbox.
// The workload is a process on this host with no mount namespace of its own,
// so the authority lives beside the sandbox's own directory and the trust
// variables name that path rather than the reserved one a container gets.
const caFile = "egress-ca.pem"

// projectEgress writes the gateway's authority beside the sandbox and returns
// the environment the boundary adds to the workload's own. A create that
// carries no gateway writes nothing and adds nothing. It takes the boundary
// and the environment rather than the create spec, because an adoption writes
// the same projection over an entry that was prewarmed without one.
func projectEgress(dir string, own map[string]string, boundary driver.Egress) (map[string]string, error) {
	env := maps.Clone(own)
	projection := egress.Projection{
		ProxyAddr:   boundary.ProxyAddr,
		ReverseAddr: boundary.ReverseAddr,
		Credential:  boundary.Credential,
	}
	if boundary.CAPEM != "" {
		path := filepath.Join(dir, caFile)
		if err := os.WriteFile(path, []byte(boundary.CAPEM), 0o400); err != nil {
			return nil, err
		}
		projection.CAPath = path
	}
	added := projection.Env()
	if len(added) == 0 {
		return env, nil
	}
	if env == nil {
		env = make(map[string]string, len(added))
	}
	maps.Copy(env, added)
	return env, nil
}
