// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"maps"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

// egressEnv is the sandbox's own environment with the boundary's added: the
// two doors, the credential that authenticates it at them, and the trust
// variables naming the file that holds the public roots and the gateway's
// authority. A create that carries no gateway
// adds nothing, so a sandbox on an installation with none is unchanged. It
// takes the environment and the boundary rather than the create spec, because
// an adoption writes the same pair over an entry prewarmed without either.
func egressEnv(own map[string]string, boundary driver.Egress) map[string]string {
	env := maps.Clone(own)
	projection := egress.Projection{
		ProxyAddr:   boundary.ProxyAddr,
		ReverseAddr: boundary.ReverseAddr,
		Credential:  boundary.Credential,
	}
	if boundary.CAPEM != "" {
		projection.CAPath = egress.CAPath
	}
	added := projection.Env()
	if len(added) == 0 {
		return env
	}
	if env == nil {
		env = make(map[string]string, len(added))
	}
	maps.Copy(env, added)
	return env
}

// putEgressCA writes the trust file into the container, read-only to the
// workload, at the path the trust variables name: the driver's public roots,
// then the gateway's authority. It runs between the create and the start, so
// the workload's first request already verifies the door it is pointed at and
// every host that door tunnels.
func (d *Driver) putEgressCA(ctx context.Context, id, authority string) error {
	return d.putFile(ctx, id, egress.CAPath, egress.TrustBundle(d.trustRoots, authority), 0o444, 0, 0, "the trust file")
}
