// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

// TestADocumentThatCannotBePlacedIsRefused: a document that does not parse,
// is not a mapping, or has no paths is refused rather than served unplaced,
// because a document naming rooted paths under a base sends a generated
// client to routes the control plane does not answer.
func TestADocumentThatCannotBePlacedIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"not YAML", "paths: [", "does not parse"},
		{"not a mapping", "- a\n- b\n", "not a mapping"},
		{"no paths", "openapi: 3.1.0\n", "no paths"},
	} {
		if _, err := under([]byte(tc.doc), "/v1/environments"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the refusal is %v, want it to say %q", tc.name, err, tc.want)
		}
	}
	if _, err := HandlerUnder("/v1/environments"); err != nil {
		t.Fatal(err)
	}
}
