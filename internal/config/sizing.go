// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// Sizing is the memory an installation gives a sandbox that names none
// (spec 078). Both amounts are empty unless the operator sets them, which
// leaves every sandbox at the driver's fixed default.
type Sizing struct {
	// StartMemory is CELLA_DEFAULT_MEMORY, the memory such a sandbox starts
	// at, and MemoryCeiling is CELLA_DEFAULT_MAX_MEMORY, the most it may
	// grow to. They are set together or not at all.
	StartMemory, MemoryCeiling v1.Quantity
}

// loadSizing reads the two variables. Each is a quantity above zero, the
// ceiling is not below the start, and neither stands without the other: a
// small start with no room to grow is the failure a ceiling exists to avoid.
// A value that is refused leaves the sizing empty, so a start-up that reports
// the problem never carries half of a pair.
func loadSizing(getenv Getenv, problems *[]string) Sizing {
	const startName, ceilingName = "CELLA_DEFAULT_MEMORY", "CELLA_DEFAULT_MAX_MEMORY"
	before := len(*problems)
	start := positive(getenv, startName, problems)
	ceiling := positive(getenv, ceilingName, problems)
	named := func(name string) bool { return strings.TrimSpace(getenv(name)) != "" }
	switch {
	case named(startName) && !named(ceilingName):
		*problems = append(*problems, startName+" is set without "+ceilingName+"; a sandbox that starts small needs a ceiling to grow to")
	case named(ceilingName) && !named(startName):
		*problems = append(*problems, ceilingName+" is set without "+startName+"; the ceiling needs the amount a sandbox starts at")
	case start != "" && ceiling != "":
		low, _ := manifest.ParseQuantity(start)
		high, _ := manifest.ParseQuantity(ceiling)
		if high < low {
			*problems = append(*problems, fmt.Sprintf("%s is %q, below %s of %q", ceilingName, string(ceiling), startName, string(start)))
		}
	}
	if len(*problems) > before {
		return Sizing{}
	}
	return Sizing{StartMemory: start, MemoryCeiling: ceiling}
}

// positive reads an optional quantity that must be above zero.
func positive(getenv Getenv, name string, problems *[]string) v1.Quantity {
	q := quantity(getenv, name, problems)
	if q == "" {
		return ""
	}
	if n, _ := manifest.ParseQuantity(q); n <= 0 {
		*problems = append(*problems, fmt.Sprintf("%s is %q; an amount above zero", name, string(q)))
		return ""
	}
	return q
}
