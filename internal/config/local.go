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
// CELLA_LOCAL_DENY, comma separated paths; CELLA_LOCAL_NETWORK, none,
// allowlist or any; CELLA_LOCAL_HOSTS, the allow list's hosts, or under any
// the host:port entries for ports beyond 443 and 80; and
// CELLA_LOCAL_BLOCKED, the hosts any refuses. Whether each path may be
// granted, and whether this computer can enforce any, is the driver's to
// decide when it opens.
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
	if !slices.Contains([]string{local.NetworkNone, local.NetworkAllowlist, local.NetworkAny}, o.Network.Mode) {
		*problems = append(*problems, "CELLA_LOCAL_NETWORK is none, allowlist or any")
	}
	o.Network.Hosts = splitList(getenv("CELLA_LOCAL_HOSTS"))
	o.Network.Blocked = splitList(getenv("CELLA_LOCAL_BLOCKED"))
	if len(o.Network.Blocked) > 0 && o.Network.Mode != local.NetworkAny {
		*problems = append(*problems, "CELLA_LOCAL_BLOCKED is the deny list of CELLA_LOCAL_NETWORK=any")
	}
	return o
}
