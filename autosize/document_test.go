// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package autosize

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// figures is every figure of the policy as the spec's table writes it, each
// rendered from the constant and from nothing else.
func figures() map[string]string {
	return map[string]string{
		"`CELLA_AUTOSIZE_INTERVAL`":     "`" + short(DefaultInterval) + "`",
		"`autosize.DiskGrowAt`":         fmt.Sprintf("%.2f", DiskGrowAt),
		"`autosize.DiskStepMin`":        Quantity(DiskStepMin),
		"`autosize.MemoryGrowAt`":       fmt.Sprintf("%.2f", MemoryGrowAt),
		"`autosize.MemoryGrowReadings`": strconv.Itoa(MemoryGrowReadings),
		"`autosize.MemoryStepFactor`":   strconv.FormatFloat(MemoryStepFactor, 'g', -1, 64),
		"`autosize.MemoryStepMin`":      Quantity(MemoryStepMin),
		"`autosize.MemoryQuantum`":      Quantity(MemoryQuantum),
		"`autosize.CeilingWarnAt`":      fmt.Sprintf("%.2f", CeilingWarnAt),
		"`autosize.MoveBackoff`":        "`" + short(MoveBackoff) + "`",

		"`autosize.SameSizeRestarts`":       strconv.Itoa(SameSizeRestarts),
		"`autosize.SameSizeRestartBackoff`": "`" + short(SameSizeRestartBackoff) + "`",
	}
}

// short writes a duration as a setting is written: 1m, not 1m0s.
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(append([]string{".."}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestTheSpecsTableIsTheConstants holds the constants table of spec 078 to
// this package in both directions: every row names a constant here with the
// value it has, and every constant is a row. A figure changed in one place
// fails here until it is changed in the other.
func TestTheSpecsTableIsTheConstants(t *testing.T) {
	spec := readFile(t, "specs", "078-self-sizing-sandboxes.md")
	_, table, found := strings.Cut(spec, "### Limits and constants")
	if !found {
		t.Fatal("spec 078 has no Limits and constants section")
	}
	table, _, _ = strings.Cut(table, "\n### ")
	want := figures()
	seen := map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 || !strings.HasPrefix(strings.TrimSpace(cells[0]), "`") {
			continue
		}
		name, value := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])
		expected, known := want[name]
		if !known {
			t.Errorf("the spec's table names %s, which this package does not declare", name)
			continue
		}
		seen[name] = true
		if value != expected {
			t.Errorf("the spec gives %s as %s, and the constant is %s", name, value, expected)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s is a figure of the policy with no row in the spec's table", name)
		}
	}
}

// TestThePagesStateTheConstants holds the figures the user and operator
// pages state to the constants, so a page never tells a reader a threshold
// the control plane does not use.
func TestThePagesStateTheConstants(t *testing.T) {
	percent := func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
	manifest := readFile(t, "docs", "manifest.md")
	_, section, found := strings.Cut(manifest, "### Sizing inside a ceiling")
	if !found {
		t.Fatal("docs/manifest.md has no section on sizing inside a ceiling")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	for what, phrase := range map[string]string{
		"the grow line":         percent(MemoryGrowAt) + " or more of what it has",
		"the readings in a row": fmt.Sprintf("at %d readings in a row", MemoryGrowReadings),
		"the step factor":       strconv.FormatFloat(MemoryStepFactor, 'g', -1, 64) + " times",
		"the smallest step":     "to " + Quantity(MemoryStepMin) + " more",
		"the quantum":           "a\n  multiple of " + Quantity(MemoryQuantum),
		"the warning line":      "uses " + percent(CeilingWarnAt) + " or more of it",
		"the starts at a size":  fmt.Sprintf("That is done\n%d times in a row", SameSizeRestarts),
	} {
		if !strings.Contains(section, phrase) {
			t.Errorf("docs/manifest.md does not state %s as %q", what, phrase)
		}
	}
	for path, phrases := range map[string][]string{
		"kubernetes.md": {
			fmt.Sprintf("left alone for %.0f minutes", MoveBackoff.Minutes()),
			fmt.Sprintf("up to %d times in a row, at least %.0f seconds apart", SameSizeRestarts, SameSizeRestartBackoff.Seconds()),
		},
		"events.md":        {"uses " + percent(CeilingWarnAt) + " or more of it"},
		"configuration.md": {"| `CELLA_AUTOSIZE_INTERVAL` | `" + short(DefaultInterval) + "` |"},
	} {
		page := readFile(t, "docs", path)
		for _, phrase := range phrases {
			if !strings.Contains(page, phrase) {
				t.Errorf("docs/%s does not state %q", path, phrase)
			}
		}
	}
}
