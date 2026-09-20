// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The JSON paths of a sandbox's mounts.
const pathSecrets = "spec.secrets"

func pathSecretAt(i int, field string) string {
	return pathSecrets + "[" + strconv.Itoa(i) + "]." + field
}

// references is stage 4: every object the manifest names is looked up
// through the actor's own view of the world, and the rules that need the
// referenced object rather than the manifest alone are checked here.
//
// It returns the secrets the manifest mounts, in the manifest's order, so a
// caller that needs them does not look them up twice. A Secret comes back
// without its value: Lookup is a read, and no read returns one.
func references(ctx context.Context, obj *v1.Sandbox, o Options) ([]v1.Secret, error) {
	mounts := obj.Spec.Secrets
	if len(mounts) == 0 {
		return nil, nil
	}
	if err := mountEnvRules(obj); err != nil {
		return nil, err
	}
	secrets := make([]v1.Secret, 0, len(mounts))
	for i, mount := range mounts {
		secret, err := lookupSecret(ctx, o.Lookup, i, mount.Name)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, *secret)
	}
	if err := refuseSharedScopes(secrets); err != nil {
		return nil, err
	}
	return secrets, nil
}

// mountEnvRules holds each mount's environment key to the contract: a POSIX
// name the boundary does not already own, distinct from spec.env, from every
// other mount, and from every mount's companion keys. The companions of both
// injection places are reserved whichever place a secret turns out to use, so
// a secret that later moves from a header to a query parameter cannot
// invalidate a manifest that already resolved.
func mountEnvRules(obj *v1.Sandbox) error {
	taken := map[string]string{}
	for key := range obj.Spec.Env {
		taken[key] = "spec.env." + key
	}
	for i, mount := range obj.Spec.Secrets {
		if strings.TrimSpace(mount.Name) == "" {
			return failAt("missing_field", pathSecretAt(i, "name"), "A mount names the secret it mounts.")
		}
		path := pathSecretAt(i, "env")
		switch {
		case mount.Env == "":
			return failAt("missing_field", path, "A mount names the environment key its placeholder arrives under.")
		case !envPattern.MatchString(mount.Env):
			return failAt("invalid_field", path, "An environment key is a POSIX name.")
		case ReservedEnv(mount.Env):
			return failAt("reserved_prefix", path, "That environment variable is set by the sandbox boundary.")
		}
		for _, key := range append([]string{mount.Env}, companionNames(mount.Env)...) {
			if where, held := taken[key]; held {
				return failPaths("invalid_field",
					"The environment key "+key+" is already taken by "+where+".",
					[]string{path, where})
			}
			taken[key] = path
		}
	}
	return nil
}

// lookupSecret asks the actor's own view for one secret. A secret that does
// not exist and one the authorizer refuses to mount are the same answer, so
// existence does not leak to a caller that may not use it.
func lookupSecret(ctx context.Context, lookup Lookup, i int, nameOrID string) (*v1.Secret, error) {
	path := pathSecretAt(i, "name")
	secret, err := lookup.Secret(ctx, nameOrID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, failAt("not_found", path, "There is no such secret.")
	case err != nil:
		var known *Error
		if errors.As(err, &known) {
			return nil, known
		}
		return nil, failAt("authorizer_unavailable", path, "The permission service is unavailable; retry shortly.")
	case secret == nil:
		return nil, failAt("not_found", path, "There is no such secret.")
	}
	return secret, nil
}

// refuseSharedScopes is the first of the two defects the reference designs
// had, caught before the map is compiled: two secrets whose scopes can match
// one concrete host have no answer for which value to substitute there, so
// the manifest is refused rather than one of the two silently dropped.
func refuseSharedScopes(secrets []v1.Secret) error {
	for i := range secrets {
		for j := i + 1; j < len(secrets); j++ {
			for _, a := range secrets[i].Spec.Scope.Hosts {
				for _, b := range secrets[j].Spec.Scope.Hosts {
					if !hostsOverlap(a, b) {
						continue
					}
					return failPaths("secret_host_conflict",
						"The secrets "+secrets[i].Metadata.Name+" and "+secrets[j].Metadata.Name+" both apply to "+v1.NormalizeHost(a)+".",
						[]string{pathSecretAt(i, "name"), pathSecretAt(j, "name")})
				}
			}
		}
	}
	return nil
}

// hostsOverlap reports whether two patterns can match one concrete host: the
// symmetric reading of the coverage rule, which is what the compiler uses so
// the resolver and the gateway refuse the same pair.
func hostsOverlap(a, b string) bool {
	a, b = v1.NormalizeHost(a), v1.NormalizeHost(b)
	return v1.HostPatternCovers(a, b) || v1.HostPatternCovers(b, a)
}

// narrowingSecrets is the mounts half of the narrowing rule. A workload may
// drop a secret its sandbox holds and may never add one, because a secret it
// did not have is reach its owner did not grant it.
func narrowingSecrets(existing, obj *v1.Sandbox) bool {
	held := make([]string, 0, len(existing.Spec.Secrets))
	for _, mount := range existing.Spec.Secrets {
		held = append(held, mount.Name)
	}
	for _, mount := range obj.Spec.Secrets {
		if !slices.Contains(held, mount.Name) {
			return true
		}
	}
	return false
}
