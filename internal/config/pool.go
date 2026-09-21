// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// The default environment's scheduling and pool, from spec 020. Nothing is
// prewarmed until an operator asks for it: a pool is an acceleration of a
// shape this installation has, and the control plane cannot guess one.
const (
	DefaultSchedulingMode = v1.SchedulingDirect
	// MaxPoolSize bounds what one environment keeps ready. It is a bound on
	// a typo rather than on a deployment: an operator who asks for ten
	// thousand warm sandboxes gets an answer at start-up.
	MaxPoolSize = 1024
	// MaxPoolInFlight bounds how many entries one refill tick prewarms.
	MaxPoolInFlight = 64
)

// Scheduling is the default environment's placement: the mode, the pool, and
// the two bounds of the refill loop (spec 020).
type Scheduling struct {
	// Mode is direct here. Queued is accepted vocabulary and refused until
	// the queue lands, so an operator learns at start-up rather than
	// watching sandboxes never start.
	Mode string
	// Pool is what the environment keeps prewarmed. Size zero is no pool.
	Pool v1.PoolSpec
	// PoolInFlight is how many entries one tick prewarms and PoolGrace how
	// long an entry is left alone before the deletion rules read it.
	PoolInFlight int
	PoolGrace    time.Duration
	// Capacity is what the environment this cellad drives declares it holds,
	// from CELLA_CAPACITY_*. An empty declaration seeds the word auto: the
	// ceiling is the cluster's or the host's and this control plane reads
	// neither (spec 021).
	Capacity v1.Capacity
}

// loadScheduling reads the mode, the pool's shape and the loop's bounds.
func loadScheduling(getenv Getenv, problems *[]string) Scheduling {
	s := Scheduling{
		Mode:         withDefault(getenv("CELLA_SCHEDULING_MODE"), DefaultSchedulingMode),
		PoolInFlight: controller.DefaultPoolInFlight,
		PoolGrace:    controller.DefaultPoolGrace,
	}
	switch s.Mode {
	case v1.SchedulingDirect:
	case v1.SchedulingQueued:
		*problems = append(*problems, "CELLA_SCHEDULING_MODE=queued is not served yet; this control plane starts a sandbox now or fails it")
		s.Mode = DefaultSchedulingMode
	default:
		*problems = append(*problems, fmt.Sprintf("CELLA_SCHEDULING_MODE is %q; %s", s.Mode, v1.SchedulingDirect))
		s.Mode = DefaultSchedulingMode
	}
	s.Pool = v1.PoolSpec{
		Size:  count(getenv, "CELLA_POOL_SIZE", 0, 0, MaxPoolSize, problems),
		Image: strings.TrimSpace(getenv("CELLA_POOL_IMAGE")),
		Resources: v1.Resources{
			CPU:    quantity(getenv, "CELLA_POOL_CPU", problems),
			Memory: quantity(getenv, "CELLA_POOL_MEMORY", problems),
			Disk:   quantity(getenv, "CELLA_POOL_DISK", problems),
		},
	}
	// The loop's two bounds are read whether or not a pool is declared, so a
	// value that is malformed is a start-up problem rather than a setting
	// that is silently accepted until somebody raises the size.
	s.PoolInFlight = count(getenv, "CELLA_POOL_IN_FLIGHT", controller.DefaultPoolInFlight, 1, MaxPoolInFlight, problems)
	s.PoolGrace = interval(getenv, "CELLA_POOL_GRACE", controller.DefaultPoolGrace, problems)
	s.Capacity = workerCapacity(getenv, problems)
	return s
}

// count reads an optional whole number and holds it between two bounds. A
// value outside them is a start-up problem rather than a clamp: an operator
// who asks for something this process will not do gets an answer.
func count(getenv Getenv, name string, def, low, high int, problems *[]string) int {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		*problems = append(*problems, fmt.Sprintf("%s is %q; a whole number between %d and %d", name, raw, low, high))
		return def
	}
	return value
}

// quantity reads an optional compute amount in the manifest's own syntax. It
// is validated here and carried as the operator wrote it, which is what the
// manifest contract does with a caller's own spelling.
func quantity(getenv Getenv, name string, problems *[]string) v1.Quantity {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return ""
	}
	if _, err := manifest.ParseQuantity(v1.Quantity(raw)); err != nil {
		*problems = append(*problems, fmt.Sprintf("%s is %q; a quantity such as 500m, 2 or 2Gi", name, raw))
		return ""
	}
	return v1.Quantity(raw)
}
