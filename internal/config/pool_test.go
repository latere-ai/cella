// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestSchedulingDefaults is what an installation that asks for nothing gets:
// a sandbox starts now or fails, and nothing is kept warm.
func TestSchedulingDefaults(t *testing.T) {
	c, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheduling.Mode != v1.SchedulingDirect {
		t.Errorf("the mode is %q, want direct", c.Scheduling.Mode)
	}
	if c.Scheduling.Pool != (v1.PoolSpec{}) {
		t.Errorf("an installation that asked for no pool got %+v", c.Scheduling.Pool)
	}
	if c.Scheduling.PoolInFlight != controller.DefaultPoolInFlight || c.Scheduling.PoolGrace != controller.DefaultPoolGrace {
		t.Errorf("the loop's bounds are %d and %s", c.Scheduling.PoolInFlight, c.Scheduling.PoolGrace)
	}
	if c.Scheduling.ScheduleInterval != controller.DefaultScheduleInterval {
		t.Errorf("the schedule interval is %s", c.Scheduling.ScheduleInterval)
	}
}

// TestLoadScheduling: the queued mode seeds the default environment in that
// mode (spec 057), and the scheduler's tick is read with it.
func TestLoadScheduling(t *testing.T) {
	c, err := Load(env(identity(t, map[string]string{
		"CELLA_SCHEDULING_MODE": "queued", "CELLA_SCHEDULE_INTERVAL": "2s",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheduling.Mode != v1.SchedulingQueued || c.Scheduling.ScheduleInterval != 2*time.Second {
		t.Fatalf("the scheduling is %s every %s", c.Scheduling.Mode, c.Scheduling.ScheduleInterval)
	}
}

// TestPoolConfig reads the whole table of spec 020's configuration section and
// holds each refusal the start-up answers.
func TestPoolConfig(t *testing.T) {
	c, err := Load(env(identity(t, map[string]string{
		"CELLA_SCHEDULING_MODE": "direct",
		"CELLA_POOL_SIZE":       "4",
		"CELLA_POOL_IMAGE":      "registry.example.com/base:1",
		"CELLA_POOL_CPU":        "500m",
		"CELLA_POOL_MEMORY":     "2Gi",
		"CELLA_POOL_DISK":       "10Gi",
		"CELLA_POOL_IN_FLIGHT":  "3",
		"CELLA_POOL_GRACE":      "2m",
	})))
	if err != nil {
		t.Fatal(err)
	}
	want := v1.PoolSpec{
		Size: 4, Image: "registry.example.com/base:1",
		Resources: v1.Resources{CPU: "500m", Memory: "2Gi", Disk: "10Gi"},
	}
	if c.Scheduling.Pool != want {
		t.Errorf("the pool is %+v, want %+v", c.Scheduling.Pool, want)
	}
	if c.Scheduling.PoolInFlight != 3 || c.Scheduling.PoolGrace != 2*time.Minute {
		t.Errorf("the loop's bounds are %d and %s", c.Scheduling.PoolInFlight, c.Scheduling.PoolGrace)
	}
}

func TestPoolConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"a schedule interval below the floor", map[string]string{"CELLA_SCHEDULE_INTERVAL": "10ms"}, "CELLA_SCHEDULE_INTERVAL"},
		{"an unknown mode", map[string]string{"CELLA_SCHEDULING_MODE": "soon"}, `CELLA_SCHEDULING_MODE is "soon"`},
		{"a size that is not a number", map[string]string{"CELLA_POOL_SIZE": "four"}, "CELLA_POOL_SIZE"},
		{"a negative size", map[string]string{"CELLA_POOL_SIZE": "-1"}, "CELLA_POOL_SIZE"},
		{"a size beyond the bound", map[string]string{"CELLA_POOL_SIZE": "100000"}, "CELLA_POOL_SIZE"},
		{"a cpu that is not a quantity", map[string]string{"CELLA_POOL_SIZE": "1", "CELLA_POOL_CPU": "half"}, "CELLA_POOL_CPU"},
		{"a memory that is not a quantity", map[string]string{"CELLA_POOL_SIZE": "1", "CELLA_POOL_MEMORY": "lots"}, "CELLA_POOL_MEMORY"},
		{"a disk that is not a quantity", map[string]string{"CELLA_POOL_SIZE": "1", "CELLA_POOL_DISK": "big"}, "CELLA_POOL_DISK"},
		{"an in-flight cap of zero", map[string]string{"CELLA_POOL_SIZE": "1", "CELLA_POOL_IN_FLIGHT": "0"}, "CELLA_POOL_IN_FLIGHT"},
		{"a grace beyond the bound", map[string]string{"CELLA_POOL_SIZE": "1", "CELLA_POOL_GRACE": "48h"}, "CELLA_POOL_GRACE"},
		{"a malformed bound with no pool", map[string]string{"CELLA_POOL_GRACE": "soon"}, "CELLA_POOL_GRACE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(identity(t, tc.vars)))
			if err == nil {
				t.Fatalf("a start-up accepted %v", tc.vars)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the problem is %q, want it to name %q", err, tc.want)
			}
		})
	}
}
