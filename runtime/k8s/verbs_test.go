// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestEveryActionTheDriverTakesIsInTheVerbTable drives a sandbox through
// the operations that write the cluster, with a token so the Secret path
// runs, and holds every action the client saw to the verbs table Preflight
// reviews. A write outside the table is one the Role does not grant and
// Preflight does not name, which is a create that fails after a clean
// start-up: the token Secret was that case.
func TestEveryActionTheDriverTakesIsInTheVerbTable(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_verbs00000000000000000000"
	h.created(t, driver.CreateSpec{ID: id, Name: "verbs", Owner: "alice", Image: "example/image:1", Token: []byte("first")})
	if err := h.Update(t.Context(), id, driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	table := map[string]bool{}
	for _, v := range verbs {
		table[fmt.Sprintf("%s/%s/%s", v.resource, v.subresource, v.verb)] = true
	}
	seen := map[string]bool{}
	for _, action := range h.cs.Actions() {
		resource := action.GetResource().Resource
		if resource == "selfsubjectaccessreviews" {
			continue
		}
		key := fmt.Sprintf("%s/%s/%s", resource, action.GetSubresource(), action.GetVerb())
		if seen[key] {
			continue
		}
		seen[key] = true
		if !table[key] {
			t.Errorf("the driver %s %s%s and the verbs table does not review it", action.GetVerb(), resource, subresourceSuffix(action.GetSubresource()))
		}
	}
}

func subresourceSuffix(sub string) string {
	if sub == "" {
		return ""
	}
	return "/" + sub
}
