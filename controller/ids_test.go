// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/ulid"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestMintedIDsKeepTheirPrefixAndCase: a stored id is permanent, so each kind
// keeps its prefix, a lowercase 26 character body, and the order it was minted
// in, even within one millisecond.
func TestMintedIDsKeepTheirPrefixAndCase(t *testing.T) {
	for prefix, mint := range map[string]func() string{
		"sbx_":                newID,
		v1.SecretIDPrefix:     newSecretID,
		v1.VolumeIDPrefix:     newVolumeID,
		manifest.MeshIDPrefix: newMeshID,
	} {
		var minted []string
		for range 256 {
			id := mint()
			if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+26 || id != strings.ToLower(id) {
				t.Fatalf("minted %q, want %s and 26 lowercase characters", id, prefix)
			}
			if !ulid.Valid(prefix, id) {
				t.Fatalf("%q is not a valid %s id", id, prefix)
			}
			minted = append(minted, id)
		}
		if !slices.IsSorted(minted) {
			t.Errorf("ids of %s do not sort in the order they were minted", prefix)
		}
	}
}
