// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strings"

	"latere.ai/x/cella/runtime/local"
)

// loadLocal reads the local driver's host policy (spec 081):
// CELLA_LOCAL_GRANTS, comma separated folders each ending in :rw or :ro,
// read-only where it names neither; CELLA_LOCAL_READ_PATHS and
// CELLA_LOCAL_DENY, comma separated paths; CELLA_LOCAL_NETWORK, none or
// allowlist; and CELLA_LOCAL_HOSTS, the allow list's hosts. Whether each
// path may be granted is the driver's to decide when it opens.
func loadLocal(getenv Getenv, problems *[]string) local.Options {
	var o local.Options
	for _, entry := range splitList(getenv("CELLA_LOCAL_GRANTS")) {
		grant := local.Grant{Path: entry}
		if at := strings.LastIndex(entry, ":"); at > 0 {
			switch entry[at+1:] {
			case "rw":
				grant = local.Grant{Path: entry[:at], Write: true}
			case "ro":
				grant = local.Grant{Path: entry[:at]}
			}
		}
		o.Grants = append(o.Grants, grant)
	}
	o.ReadPaths = splitList(getenv("CELLA_LOCAL_READ_PATHS"))
	o.Deny = splitList(getenv("CELLA_LOCAL_DENY"))
	o.Network.Mode = withDefault(getenv("CELLA_LOCAL_NETWORK"), local.NetworkNone)
	if !slices.Contains([]string{local.NetworkNone, local.NetworkAllowlist}, o.Network.Mode) {
		*problems = append(*problems, "CELLA_LOCAL_NETWORK is none or allowlist")
	}
	o.Network.Hosts = splitList(getenv("CELLA_LOCAL_HOSTS"))
	return o
}
