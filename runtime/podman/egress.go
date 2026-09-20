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
// store that holds the gateway's authority. A create that carries no gateway
// adds nothing, so a sandbox on an installation with none is unchanged.
func egressEnv(s driver.CreateSpec) map[string]string {
	env := maps.Clone(s.Env)
	projection := egress.Projection{
		ProxyAddr:   s.Egress.ProxyAddr,
		ReverseAddr: s.Egress.ReverseAddr,
		Credential:  s.Egress.Credential,
	}
	if s.Egress.CAPEM != "" {
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

// putEgressCA writes the gateway's authority into the container, read-only to
// the workload, at the path the trust variables name. It runs between the
// create and the start, so the workload's first request already trusts the
// door it is pointed at.
func (d *Driver) putEgressCA(ctx context.Context, id, certPEM string) error {
	return d.putFile(ctx, id, egress.CAPath, []byte(certPEM), 0o444, 0, 0, "the gateway's authority")
}
