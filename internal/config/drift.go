// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strconv"
	"strings"
)

// The fields CELLA_TEST_DRIFT_DEFAULT may name. Each is an integer whose
// default in the manifest contract is the literal 0, and neither one alone
// admits a child: a budget with no depth, or a depth with no budget, spawns
// nothing. A server that resolves one of them one unit off therefore answers
// every conformance case but the one that reads the default exactly as an
// honest server does, which is what makes it the fixture that proves the
// suite notices.
const (
	DriftSpawnBudget = "spec.mesh.spawn.budget"
	DriftSpawnDepth  = "spec.mesh.spawn.depth"
)

// DriftFields is every value CELLA_TEST_DRIFT_DEFAULT accepts, in the order
// the problem message lists them.
var DriftFields = []string{DriftSpawnBudget, DriftSpawnDepth}

// loadDriftDefault reads CELLA_TEST_DRIFT_DEFAULT, the test seam of spec 015
// that makes this server resolve one default one unit off. It is refused
// unless the runtime is native, the one runtime that already needs an
// explicit unsafe flag, so an installation that isolates anything does not
// start with it set, and a value outside DriftFields is refused rather than
// drifting nothing.
func loadDriftDefault(getenv Getenv, runtime string, problems *[]string) string {
	field := strings.TrimSpace(getenv("CELLA_TEST_DRIFT_DEFAULT"))
	if field == "" {
		return ""
	}
	if !slices.Contains(DriftFields, field) {
		*problems = append(*problems, "CELLA_TEST_DRIFT_DEFAULT is "+strconv.Quote(field)+"; one of "+strings.Join(DriftFields, ", "))
		return ""
	}
	if runtime != RuntimeNative {
		*problems = append(*problems, "CELLA_TEST_DRIFT_DEFAULT is a test seam that makes this server resolve a default wrongly; it is accepted only with CELLA_RUNTIME=native")
		return ""
	}
	return field
}
