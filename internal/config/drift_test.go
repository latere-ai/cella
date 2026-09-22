// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
)

// TestTheDriftDefaultIsATestSeam: CELLA_TEST_DRIFT_DEFAULT is unset in every
// deployment, names one field of a closed set when a test sets it, and is
// refused with any runtime but the native one, so an installation that
// isolates anything cannot start with it by accident.
func TestTheDriftDefaultIsATestSeam(t *testing.T) {
	native := map[string]string{"CELLA_RUNTIME": RuntimeNative, "CELLA_ALLOW_UNSAFE_NATIVE": "true"}
	with := func(extra map[string]string) map[string]string {
		out := identity(t, native)
		maps.Copy(out, extra)
		return out
	}

	c, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.DriftDefault != "" {
		t.Fatalf("an unset variable drifts %q", c.DriftDefault)
	}

	for _, field := range DriftFields {
		c, err := Load(env(with(map[string]string{"CELLA_TEST_DRIFT_DEFAULT": " " + field + " "})))
		if err != nil {
			t.Fatalf("%s on the native runtime: %v", field, err)
		}
		if c.DriftDefault != field {
			t.Errorf("DriftDefault = %q, want %q", c.DriftDefault, field)
		}
	}

	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"a field outside the set", with(map[string]string{"CELLA_TEST_DRIFT_DEFAULT": "spec.workdir"}),
			`CELLA_TEST_DRIFT_DEFAULT is "spec.workdir"; one of spec.mesh.spawn.budget, spec.mesh.spawn.depth`},
		{"the default runtime", identity(t, map[string]string{"CELLA_TEST_DRIFT_DEFAULT": DriftSpawnBudget}),
			"accepted only with CELLA_RUNTIME=native"},
		{"the container runtime", identity(t, map[string]string{"CELLA_TEST_DRIFT_DEFAULT": DriftSpawnDepth, "CELLA_RUNTIME": RuntimePodman}),
			"accepted only with CELLA_RUNTIME=native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.vars))
			if err == nil {
				t.Fatal("the start-up accepted the seam")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the problem is %q, want it to carry %q", err, tc.want)
			}
		})
	}
}
