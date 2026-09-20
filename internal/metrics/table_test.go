// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/metrics"
)

// specRow is one line of the metric table in specs/017-observability.md.
type specRow struct {
	name    string
	kind    string
	labels  []string
	buckets string
	owners  []string
	line    int
}

// TestMetricsTable holds the declared table to the table in design 017, row
// for row, in name, type, labels, bucket range and owning spec. It reads the
// spec through runtime.Caller because the gate runs the suite from a
// directory that is not this one.
func TestMetricsTable(t *testing.T) {
	spec := readSpecTable(t)
	declared := metrics.Table
	if len(spec) != len(declared) {
		t.Fatalf("design 017 names %d metrics and the table declares %d", len(spec), len(declared))
	}
	for i, want := range spec {
		got := declared[i]
		if got.Name != want.name {
			t.Errorf("row %d: the table declares %q where design 017 line %d names %q", i, got.Name, want.line, want.name)
			continue
		}
		if string(got.Kind) != want.kind {
			t.Errorf("%s: declared %s, design 017 says %s", got.Name, got.Kind, want.kind)
		}
		if !slices.Equal(got.Labels, want.labels) {
			t.Errorf("%s: declared labels %v, design 017 says %v", got.Name, got.Labels, want.labels)
		}
		if !slices.Equal(got.Owners, want.owners) {
			t.Errorf("%s: declared owners %v, design 017 says %v", got.Name, got.Owners, want.owners)
		}
		checkBuckets(t, got, want)
	}
}

// checkBuckets holds a histogram's bounds to the range and count design 017
// states, and holds every other kind to having none.
func checkBuckets(t *testing.T, got metrics.Row, want specRow) {
	t.Helper()
	if want.buckets == "" {
		if len(got.Buckets) != 0 {
			t.Errorf("%s: declared %d bounds where design 017 names none", got.Name, len(got.Buckets))
		}
		return
	}
	low, high, count := parseBounds(t, got.Name, want.buckets)
	switch {
	case len(got.Buckets) != count:
		t.Errorf("%s: declared %d bounds, design 017 says %d", got.Name, len(got.Buckets), count)
	case got.Buckets[0] != low:
		t.Errorf("%s: lowest bound %g, design 017 says %g", got.Name, got.Buckets[0], low)
	case got.Buckets[len(got.Buckets)-1] != high:
		t.Errorf("%s: highest bound %g, design 017 says %g", got.Name, got.Buckets[len(got.Buckets)-1], high)
	case !slices.IsSorted(got.Buckets):
		t.Errorf("%s: the bounds are not ascending: %v", got.Name, got.Buckets)
	}
}

var boundsPattern = regexp.MustCompile(`^([0-9.]+m?s) to ([0-9.]+m?s), (\d+) bounds$`)

func parseBounds(t *testing.T, metric, cell string) (low, high float64, count int) {
	t.Helper()
	m := boundsPattern.FindStringSubmatch(cell)
	if m == nil {
		t.Fatalf("%s: design 017's bucket cell %q is not \"<low> to <high>, <n> bounds\"", metric, cell)
	}
	return seconds(t, m[1]), seconds(t, m[2]), atoi(t, m[3])
}

func seconds(t *testing.T, s string) float64 {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("design 017 writes a bound as %q, which is not a duration: %v", s, err)
	}
	return d.Seconds()
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("design 017 writes a bound count as %q: %v", s, err)
	}
	return n
}

// TestAwaitingRowsAreRegisteredByNobody holds the two halves of the table
// apart: a row with an owning spec that has landed is registered and appears
// in a scrape, and a row waiting on one is declared, named by the rules file,
// and registered nowhere.
func TestAwaitingRowsAreRegisteredByNobody(t *testing.T) {
	scraped := scrape(t, metrics.New(metrics.Options{}))
	for _, row := range metrics.Table {
		if !metrics.Declared(row.Name) {
			t.Errorf("%s is in the table and Declared says it is not", row.Name)
		}
		_, present := scraped[row.Name]
		switch {
		case row.Await == "" && metrics.Registered(row.Name) != true:
			t.Errorf("%s waits on no spec and Registered says it is not registered", row.Name)
		case row.Await != "" && metrics.Registered(row.Name):
			t.Errorf("%s waits on spec %s and Registered says it is registered", row.Name, row.Await)
		case row.Await != "" && present:
			t.Errorf("%s waits on spec %s and a scrape carries it", row.Name, row.Await)
		case row.Await == "" && row.Kind != metrics.KindGauge && !present:
			t.Errorf("%s is registered and a scrape of a fresh registry does not carry it", row.Name)
		}
	}
	if metrics.Declared("cella_not_a_metric") || metrics.Registered("cella_not_a_metric") {
		t.Error("a name that is in no row reads as declared or registered")
	}
	if _, ok := metrics.LabelsOf("cella_not_a_metric"); ok {
		t.Error("LabelsOf answered for a name that is in no row")
	}
	labels, ok := metrics.LabelsOf("cella_requests_total")
	if !ok || !slices.Equal(labels, []string{"route", "status", "code"}) {
		t.Errorf("LabelsOf(cella_requests_total) = %v, %v", labels, ok)
	}
}

// TestEveryRegisteredRowHasHelp holds the exposition readable: an operator
// reads a scrape without design 017 open beside it.
func TestEveryRegisteredRowHasHelp(t *testing.T) {
	r := metrics.New(metrics.Options{
		Environment: "default", Driver: "native",
		Sandboxes: func() map[string]int { return map[string]int{"Running": 1} },
		Gateways:  func() int { return 0 },
		Pending:   func() (int, bool) { return 0, true },
	})
	r.LeaseHeld(metrics.LeaseReaper, true)
	r.PoolSize(0, 0)
	out := exposition(t, r)
	for _, row := range metrics.Table {
		if row.Await != "" {
			continue
		}
		if !strings.Contains(out, "# HELP "+row.Name+" ") {
			t.Errorf("%s is registered and the exposition carries no HELP line for it", row.Name)
		}
	}
}

// readSpecTable parses the metric table out of design 017. It takes the first
// table whose header names a metric, which is the one the design calls the
// table; the alert table below it has a different header.
func readSpecTable(t *testing.T) []specRow {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the caller's own file is unknown, so the spec cannot be found")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "specs", "017-observability.md")
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading design 017: %v", err)
	}
	defer func() { _ = f.Close() }()

	var rows []specRow
	inTable := false
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "| Metric | Type | Labels | Buckets | Owner |":
			inTable = true
			continue
		case !inTable:
			continue
		case !strings.HasPrefix(line, "|"):
			inTable = false
			continue
		case strings.HasPrefix(line, "|---"):
			continue
		}
		cells := tableCells(line)
		if len(cells) != 5 {
			t.Fatalf("design 017 line %d has %d cells, want 5: %q", n, len(cells), line)
		}
		rows = append(rows, specRow{
			name:    unquote(cells[0]),
			kind:    cells[1],
			labels:  parseLabels(cells[2]),
			buckets: cells[3],
			owners:  splitList(cells[4]),
			line:    n,
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading design 017: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("design 017 holds no metric table; the check would pass vacuously")
	}
	return rows
}

func tableCells(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// parseLabels reads the label names out of a cell, dropping the parenthesised
// vocabularies design 017 writes beside some of them.
func parseLabels(cell string) []string {
	cell = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(cell, "")
	if strings.TrimSpace(cell) == "none" || strings.TrimSpace(cell) == "" {
		return nil
	}
	var out []string
	for _, part := range splitList(cell) {
		if name := unquote(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func splitList(cell string) []string {
	var out []string
	for part := range strings.SplitSeq(cell, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, unquote(p))
		}
	}
	return out
}

func unquote(s string) string { return strings.Trim(strings.TrimSpace(s), "`") }
