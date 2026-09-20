// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress_test

import (
	"slices"
	"testing"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/manifest"
)

// TestReservedKeysMatchTheGateway holds the two copies of one list equal.
// This package owns what the boundary sets inside a sandbox; the manifest
// refuses the same keys in spec.env, because a value a caller wrote there
// would be overwritten and the caller would never know.
func TestReservedKeysMatchTheGateway(t *testing.T) {
	for _, key := range egress.ReservedEnv() {
		if !manifest.ReservedEnv(key) {
			t.Errorf("the gateway sets %q and the manifest does not reserve it", key)
		}
	}
	// The reverse holds for the keys the manifest reserves by name. The
	// CELLA_ prefix is reserved as a whole and is not a list.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if !contains(egress.ReservedEnv(), key) {
			t.Errorf("the manifest reserves %q and the gateway's list does not name it", key)
		}
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}
