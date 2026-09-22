// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The two directories every spec lives in: the queue and the settled ones.
// An archived spec is read too, because its acceptance table is the record
// of what a slice proved and a rename breaks that record the same way.
var specDirs = []string{"specs", filepath.Join("specs", ".archive")}

// threatModel is the file whose Controls table names one test per control.
const threatModel = "specs/013-security-and-threat-model.md"

// testName matches a Go test declaration as a spec writes it, in backticks.
// A conformance case (case008ExecStream) and a suite case (OptimisticConcurrency)
// are not test functions and do not match: the prose beside them names the
// function that runs them, and that function is what this check holds.
var testName = regexp.MustCompile("`((?:Test|Fuzz|Benchmark|Example)[A-Z][A-Za-z0-9_]*)`")

// pendingStates are the openings of a State cell that says the row's test is
// not in the tree yet. Such a row names a design's intent, so its names are
// not held to exist.
var pendingStates = []string{"not built", "open", "not implemented"}

// pendingControls are the controls of the threat model whose test is not
// written yet, one entry per name the Controls table's Test column carries
// and the tree does not. The list is asserted exact in both directions: a
// name here that exists is a stale waiver, and a name in the table that is
// neither here nor in the tree fails. It shrinks as the controls land and
// never grows without a reader seeing it.
var pendingControls = []string{
	"TestAdmissionCannotOpenABoundary",     // spec 007, a webhook that reopens a boundary
	"TestArchiveFetchIsBounded",            // spec 019, the bounded archive fetch
	"TestAttachByIdUnderTheAuthorizer",     // spec 019, attaching a volume by id
	"TestBodiesAndTypes",                   // spec 008, the body caps and the content types
	"TestClusterEgressBoundary",            // spec 018, the boundary on the cluster tier
	"TestClusterNoLateralMovement",         // spec 004, the cluster tier's network policy
	"TestClusterPodIsConfined",             // spec 004, the cluster tier's Pod baseline
	"TestControlPlaneWriter",               // spec 019, the one writer of a volume
	"TestDecoratorCannotWeakenTheBaseline", // spec 004, a decorator against the baseline
	"TestEgressMapCrossesOneHop",           // spec 021, a map that reaches one environment
	"TestMeshReachability",                 // spec 022, two peers on the cluster tier
	"TestNoInboundToTheDataPlane",          // spec 021, no connection toward a worker
	"TestNoSecretLeaks",                    // spec 018, the placeholder's confinement
	"TestRateLimits",                       // spec 008, the token buckets
	"TestReadAuthorizeAct",                 // spec 008, the handler order
	"TestRefusedReferencesLookMissing",     // spec 006, a refusal that reads as missing
	"TestRouteTableActions",                // spec 008, the action per route
	"TestSecretValueIsWriteOnly",           // spec 018, a value never read back
	"TestTiersAreIsolated",                 // spec 012, every tier off the developer's own state
}

// declaredTests is every test function of the module, which is the set
// `go test -list` prints. The files are parsed rather than built, so a test
// behind a build tag counts: a spec's claim does not depend on the tags of
// the run that reads it.
func declaredTests(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "out"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, "_test.go"):
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if testName.MatchString("`" + fn.Name.Name + "`") {
				found[fn.Name.Name] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the module's tests: %v", err)
	}
	// A walk that read nothing would pass every check below without
	// proving anything.
	if len(found) < 200 {
		t.Fatalf("the walk found %d test functions, which is too few to be the whole module", len(found))
	}
	return found
}

// row is one line of a markdown table, already split into its cells.
type row struct {
	file  string
	line  int
	cells []string
}

// tableRows returns the rows of the first table under heading, without its
// header and rule lines. The result is empty where the section holds no
// table, which the early specs' bullet lists are; a caller that needs a
// table says so.
func tableRows(t *testing.T, path, heading string) []row {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(body), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == heading })
	if start < 0 {
		t.Fatalf("%s has no %q heading", path, heading)
	}
	var rows []row
	for i := start + 1; i < len(lines); i++ {
		text := strings.TrimSpace(lines[i])
		if strings.HasPrefix(text, "## ") {
			break // the next section began before the table did
		}
		if !strings.HasPrefix(text, "|") {
			if len(rows) > 0 {
				break // the table ended
			}
			continue // the prose between the heading and the table
		}
		cells := strings.Split(strings.Trim(text, "|"), "|")
		for j := range cells {
			cells[j] = strings.TrimSpace(cells[j])
		}
		rows = append(rows, row{file: path, line: i + 1, cells: cells})
	}
	// A markdown table opens with its header and the rule under it. Both are
	// the table's shape and not its content.
	if len(rows) >= 2 && strings.HasPrefix(rows[1].cells[0], "---") {
		rows = rows[2:]
	}
	return rows
}

// specFiles is every spec of the tree, the archived ones included.
func specFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range specDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || entry.Name() == "README.md" {
				continue
			}
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	if len(files) < 20 {
		t.Fatalf("the tree holds %d specs, which is too few to be all of them", len(files))
	}
	return files
}

// pending reports whether a State cell says its row's test is not in the tree
// yet.
func pending(state string) bool {
	low := strings.ToLower(state)
	return slices.ContainsFunc(pendingStates, func(p string) bool { return strings.HasPrefix(low, p) })
}

// TestAcceptanceCriteriaNameRealTests is spec 013's first control read over
// the whole tree: a row that claims evidence names the evidence. Every test
// a spec names, in the Test cell or in the State cell, is held to exist
// unless the State cell says the row is not built, in which case the name is
// a design's intent and not a claim. A test that landed under another name
// is fixed in the spec and never in the tree: the tree is what runs.
func TestAcceptanceCriteriaNameRealTests(t *testing.T) {
	tests := declaredTests(t)
	var missing []string
	tabled := 0
	for _, path := range specFiles(t) {
		rows := tableRows(t, path, "## Acceptance criteria")
		if len(rows) > 0 {
			tabled++
		}
		for _, r := range rows {
			if len(r.cells) < 3 || pending(r.cells[2]) {
				continue
			}
			var named []string
			for _, cell := range r.cells[1:3] {
				for _, m := range testName.FindAllStringSubmatch(cell, -1) {
					if _, ok := tests[m[1]]; !ok && !slices.Contains(named, m[1]) {
						named = append(named, m[1])
					}
				}
			}
			if len(named) > 0 {
				missing = append(missing, r.file+":"+strconv.Itoa(r.line)+" names "+strings.Join(named, ", ")+
					", which no test declares: "+truncate(r.cells[0], 60))
			}
		}
	}
	// A spec whose table moved out from under its heading would pass this
	// check without proving anything, so the count of tables is held too.
	if tabled < 20 {
		t.Fatalf("%d specs carry an acceptance table, which is too few to be the tree", tabled)
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Error(m)
	}
	if len(missing) > 0 {
		t.Logf("%d rows name a test the tree does not declare; correct the cell to the test that runs", len(missing))
	}
}

// TestThreatModelControlsHaveTests holds the Controls table of spec 013 to
// the same rule. The table has no State column, so the rule is written the
// other way round: pendingControls carries the names whose control is not
// built, and the list is exact in both directions.
func TestThreatModelControlsHaveTests(t *testing.T) {
	tests := declaredTests(t)
	named := map[string]bool{}
	controls := tableRows(t, threatModel, "### Controls")
	if len(controls) == 0 {
		t.Fatalf("%s has no rows under its Controls heading", threatModel)
	}
	for _, r := range controls {
		if len(r.cells) < 4 {
			t.Errorf("%s:%d is a control row with %d cells, want threat, control, spec and test", r.file, r.line, len(r.cells))
			continue
		}
		for _, m := range testName.FindAllStringSubmatch(r.cells[3], -1) {
			named[m[1]] = true
		}
	}
	if len(named) < 40 {
		t.Fatalf("the Controls table names %d tests, which is too few to be the model", len(named))
	}
	for name := range named {
		_, exists := tests[name]
		if !exists && !slices.Contains(pendingControls, name) {
			t.Errorf("the control naming %s has no such test and no entry in pendingControls", name)
		}
	}
	for _, name := range pendingControls {
		switch {
		case !named[name]:
			t.Errorf("pendingControls carries %s, which no control row names", name)
		case tests[name] != "":
			t.Errorf("pendingControls carries %s, which %s declares: the waiver is stale", name, tests[name])
		}
	}
}

// truncate shortens a criterion for a failure sentence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// securityPolicy is the file a reader of this repository is pointed at.
const securityPolicy = "SECURITY.md"

// The headings of the policy this check reads.
const (
	policyCommitments = "## Commitments"
	policyAssets      = "## What is protected, and what proves it"
)

// commitments pairs each promise the policy makes with a phrase of the
// control row that carries it in the threat model. Both halves are held to
// appear, so a commitment without a control and a control whose sentence
// moved out from under a commitment each fail.
var commitments = []struct{ policy, control string }{
	{"nothing acts before the authorizer has", "reads, authorizes, then acts"},
	{"never an allow", "fail closed on every non-200"},
	{"only through its egress gateway", "routes every connection to the gateway"},
	{"once that sandbox is gone", "refuses a revoked `jti`"},
}

// bullets are the items of the list under heading, one string per item with
// its continuation lines joined.
func bullets(t *testing.T, path, heading string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(body), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == heading })
	if start < 0 {
		t.Fatalf("%s has no %q heading", path, heading)
	}
	var items []string
	for i := start + 1; i < len(lines); i++ {
		text := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(text, "## "):
			i = len(lines)
		case strings.HasPrefix(text, "- "):
			items = append(items, strings.TrimPrefix(text, "- "))
		case text == "":
			if len(items) > 0 {
				i = len(lines)
			}
		case len(items) > 0:
			items[len(items)-1] += " " + text
		}
	}
	return items
}

// TestSecurityPolicyMatchesTheModel holds the root file to the threat model.
// Every test the policy names is a test that exists and a control the model
// carries, so the table is evidence a reader can follow rather than a claim,
// and every commitment maps to a control row, so the file promises nothing
// the design does not answer.
func TestSecurityPolicyMatchesTheModel(t *testing.T) {
	tests := declaredTests(t)
	controls := tableRows(t, threatModel, "### Controls")
	named := map[string]bool{}
	var text strings.Builder
	for _, r := range controls {
		if len(r.cells) < 4 {
			continue
		}
		text.WriteString(r.cells[1] + "\n")
		for _, m := range testName.FindAllStringSubmatch(r.cells[3], -1) {
			named[m[1]] = true
		}
	}

	assets := tableRows(t, securityPolicy, policyAssets)
	if len(assets) < 10 {
		t.Fatalf("the policy names %d assets, which is too few to be what Cella protects", len(assets))
	}
	for _, r := range assets {
		if len(r.cells) != 3 {
			t.Errorf("%s:%d has %d cells, want the asset, the control and the tests", r.file, r.line, len(r.cells))
			continue
		}
		found := testName.FindAllStringSubmatch(r.cells[2], -1)
		if len(found) == 0 {
			t.Errorf("%s:%d names no test for %q", r.file, r.line, truncate(r.cells[0], 40))
		}
		for _, m := range found {
			if _, exists := tests[m[1]]; !exists {
				t.Errorf("%s:%d names %s, which no test declares", r.file, r.line, m[1])
			}
			if !named[m[1]] {
				t.Errorf("%s:%d names %s, which is no control of the threat model", r.file, r.line, m[1])
			}
		}
	}

	promises := bullets(t, securityPolicy, policyCommitments)
	if len(promises) != len(commitments) {
		t.Fatalf("the policy makes %d commitments and this check holds %d; a commitment added or removed is a change to the model",
			len(promises), len(commitments))
	}
	for _, pair := range commitments {
		if !slices.ContainsFunc(promises, func(p string) bool { return strings.Contains(p, pair.policy) }) {
			t.Errorf("no commitment of the policy carries %q", pair.policy)
		}
		if !strings.Contains(text.String(), pair.control) {
			t.Errorf("no control of the threat model carries %q, so the commitment it answers stands alone", pair.control)
		}
	}
}
