// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

// TestReplicasReadsItsVariables: spec 076's three variables take their
// defaults, read what an operator sets, and refuse an address a forward
// cannot use, an address without the store that names the writer, and a wait
// that is not a positive duration of at most five minutes.
func TestReplicasReadsItsVariables(t *testing.T) {
	const db = "postgres://cella@db.example.com/cella"
	defaults, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Replicas != (Replicas{ForwardHold: 15 * time.Second, HandoffTimeout: 10 * time.Second}) {
		t.Errorf("the defaults are %+v", defaults.Replicas)
	}
	set, err := Load(env(identity(t, map[string]string{
		"CELLA_DB_URL": db, "CELLA_ADVERTISE_URL": "http://10.0.0.7:8080/",
		"CELLA_FORWARD_HOLD": "20s", "CELLA_HANDOFF_TIMEOUT": "4s",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if set.Replicas != (Replicas{AdvertiseURL: "http://10.0.0.7:8080", ForwardHold: 20 * time.Second, HandoffTimeout: 4 * time.Second}) {
		t.Errorf("the set values read as %+v", set.Replicas)
	}
	for _, tc := range []struct {
		vars    map[string]string
		problem string
	}{
		{map[string]string{"CELLA_DB_URL": db, "CELLA_ADVERTISE_URL": "10.0.0.7:8080"}, "CELLA_ADVERTISE_URL is"},
		{map[string]string{"CELLA_DB_URL": db, "CELLA_ADVERTISE_URL": "http://10.0.0.7:8080/v1/environments"}, "no path"},
		{map[string]string{"CELLA_ADVERTISE_URL": "http://10.0.0.7:8080"}, "CELLA_ADVERTISE_URL needs CELLA_DB_URL"},
		{map[string]string{"CELLA_FORWARD_HOLD": "0s"}, "CELLA_FORWARD_HOLD is 0s"},
		{map[string]string{"CELLA_HANDOFF_TIMEOUT": "1h"}, "CELLA_HANDOFF_TIMEOUT is 1h0m0s; at most 5m0s"},
	} {
		_, err := Load(env(identity(t, tc.vars)))
		if err == nil || !strings.Contains(err.Error(), tc.problem) {
			t.Errorf("%v answered %v, want a problem naming %q", tc.vars, err, tc.problem)
		}
	}
}
