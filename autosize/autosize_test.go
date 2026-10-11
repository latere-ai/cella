// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package autosize

import (
	"math"
	"testing"
	"time"
)

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

// TestNextMemory is the step rule over the sizes a sandbox meets: the small
// start a dense installation gives, the factor taking over from the smallest
// step, the quantum, and the ceiling cutting a step short.
func TestNextMemory(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		limit, ceiling, want int64
	}{
		{"the smallest step from a small start", 256 * mi, 8 * gi, 768 * mi},
		{"the second step of that start", 768 * mi, 8 * gi, 1280 * mi},
		{"the factor above the smallest step", 2 * gi, 16 * gi, 3 * gi},
		{"rounded up to the quantum", 1100 * mi, 16 * gi, 1792 * mi},
		{"a size off the quantum lands on it", 300 * mi, 8 * gi, 1024 * mi},
		{"the ceiling cuts the step short", 768 * mi, gi, gi},
		{"a ceiling off the quantum is still the most", 256 * mi, 600 * mi, 600 * mi},
		{"a ceiling equal to the size changes nothing", gi, gi, gi},
		{"a ceiling below the size changes nothing", 2 * gi, gi, 2 * gi},
		{"no size is no step", 0, gi, 0},
		{"a size near the largest integer goes to its ceiling", math.MaxInt64 - gi, math.MaxInt64, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextMemory(tc.limit, tc.ceiling); got != tc.want {
				t.Fatalf("NextMemory(%d, %d) = %d, want %d", tc.limit, tc.ceiling, got, tc.want)
			}
		})
	}
}

// TestASmallStartReachesItsCeilingInTwoSteps: 256Mi under a ceiling of 1Gi is
// 768Mi and then 1Gi, and a sandbox at its ceiling is asked for nothing more.
func TestASmallStartReachesItsCeilingInTwoSteps(t *testing.T) {
	size, steps := 256*mi, 0
	for {
		next := NextMemory(size, gi)
		if next == size {
			break
		}
		if next < size || next > gi {
			t.Fatalf("a step from %d went to %d under a ceiling of %d", size, next, gi)
		}
		size = next
		steps++
	}
	if size != gi || steps != 2 {
		t.Fatalf("256Mi reached %d in %d steps, want 1Gi in 2", size, steps)
	}
}

// TestEveryStepGrowsAndIsOnTheQuantum walks a range of sizes: a step always
// raises a size below its ceiling, never past the ceiling, and lands on the
// quantum unless the ceiling is what stopped it.
func TestEveryStepGrowsAndIsOnTheQuantum(t *testing.T) {
	const ceiling = 64 * gi
	for size := 16 * mi; size < ceiling; size += 37 * mi {
		next := NextMemory(size, ceiling)
		if next <= size || next > ceiling {
			t.Fatalf("NextMemory(%d) = %d under %d", size, next, ceiling)
		}
		if next != ceiling && next%MemoryQuantum != 0 {
			t.Fatalf("NextMemory(%d) = %d, which is no multiple of the quantum", size, next)
		}
		if next != ceiling && next-size < MemoryStepMin {
			t.Fatalf("NextMemory(%d) = %d, a step below the smallest", size, next)
		}
	}
}

func TestMemoryPressedAndAtCeiling(t *testing.T) {
	limit := 1000 * mi
	line := int64(math.Ceil(MemoryGrowAt * float64(limit)))
	if MemoryPressed(line-1, limit) || !MemoryPressed(line, limit) || !MemoryPressed(limit, limit) {
		t.Errorf("the grow line of %d is not at %d", limit, line)
	}
	if MemoryPressed(1, 0) {
		t.Error("a sandbox with no limit is pressed")
	}
	warn := int64(math.Ceil(CeilingWarnAt * float64(limit)))
	if AtCeiling(warn-1, limit) || !AtCeiling(warn, limit) {
		t.Errorf("the warning line of %d is not at %d", limit, warn)
	}
	if AtCeiling(1, 0) {
		t.Error("a sandbox with no ceiling is at it")
	}
}

func TestQuantity(t *testing.T) {
	for bytes, want := range map[int64]string{
		768 * mi: "768Mi", gi: "1Gi", 6 * gi: "6Gi", 1536 * mi: "1536Mi",
		4096: "4Ki", 1000: "1000", 0: "0", gi + 1: "1073741825",
	} {
		if got := Quantity(bytes); got != want {
			t.Errorf("Quantity(%d) = %q, want %q", bytes, got, want)
		}
	}
}

// TestReadingsCountSamplesInARow: the grow line is met by MemoryGrowReadings
// distinct samples in a row, a sample read twice is one reading, one below
// the line starts over, and a raise starts over at the new limit.
func TestReadingsCountSamplesInARow(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	limit := 256 * mi
	high, low := 240*mi, 100*mi
	var r Readings
	if r.Usage(high, limit, at) {
		t.Fatal("one reading at the line raised memory")
	}
	if r.Usage(high, limit, at) {
		t.Fatal("one sample read twice counted as two readings")
	}
	if !r.Usage(high, limit, at.Add(time.Minute)) {
		t.Fatalf("%d readings in a row at the line did not raise memory", MemoryGrowReadings)
	}
	r.Raised()
	if r.Usage(high, limit, at.Add(2*time.Minute)) {
		t.Fatal("the first reading after a raise raised memory again")
	}
	if r.Usage(low, limit, at.Add(3*time.Minute)) || r.Usage(high, limit, at.Add(4*time.Minute)) {
		t.Fatal("a reading below the line did not start the count again")
	}
	if !r.Usage(high, limit, at.Add(5*time.Minute)) {
		t.Fatal("two readings in a row after one below the line did not raise memory")
	}
}

// TestReadingsKeepABaselinePerInstance: the first kill count of a workload
// instance raises nothing, a rise within the instance raises once, and the
// count of a new instance is a new baseline whichever way it compares.
func TestReadingsKeepABaselinePerInstance(t *testing.T) {
	var r Readings
	if r.Kills(3, "a") {
		t.Fatal("a count that was already there raised memory")
	}
	if r.Kills(3, "a") {
		t.Fatal("a count that did not move raised memory")
	}
	if !r.Kills(6, "a") {
		t.Fatal("a count that rose did not raise memory")
	}
	if r.Kills(6, "a") {
		t.Fatal("one rise raised memory twice")
	}
	for _, count := range []int64{0, 9} {
		if r.Kills(count, "b") {
			t.Fatalf("the first count of a new instance, %d, raised memory", count)
		}
		if !r.Kills(count+1, "b") {
			t.Fatal("a rise within the new instance did not raise memory")
		}
		r.Kills(6, "a")
	}
}
