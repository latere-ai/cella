// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// markerName is the form design 015 fixes for a criterion that is a
// conformance case: the words conformance case, and the case's own name in
// code quotes on the same line.
var markerName = regexp.MustCompile("`(case[0-9]{3}[A-Z][A-Za-z0-9]*)`")

// TestEveryCriterionHasACase is the marker rule of design 015: a criterion
// that names a conformance case has a function of that name here, and a
// function here is named by a criterion. A criterion that cites the driver
// suite is that suite's and is not collected.
func TestEveryCriterionHasACase(t *testing.T) {
	marked := map[string]string{}
	for _, dir := range []string{filepath.Join("..", "..", "specs"), filepath.Join("..", "..", "specs", ".archive")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(string(data), "\n") {
				if !strings.Contains(line, "conformance case") && !strings.Contains(line, "conformance `case") {
					continue
				}
				if strings.Contains(line, "runtimetest") {
					continue
				}
				for _, match := range markerName.FindAllStringSubmatch(line, -1) {
					marked[match[1]] = path
				}
			}
		}
	}
	if len(marked) == 0 {
		t.Fatal("no criterion in the specs names a conformance case; the marker rule has nothing to read")
	}
	names := Names()
	for name, path := range marked {
		if !slices.Contains(names, name) {
			t.Errorf("%s names the conformance case %s, which this suite does not hold", path, name)
		}
	}
	for _, name := range names {
		if _, ok := marked[name]; !ok {
			t.Errorf("%s is a case no criterion names; mark the criterion it proves", name)
		}
	}
}

// TestTheDeclaredGapsNameRealCases: this repository's own declaration is a
// file the suite can read, every line names a case, and every line says why.
func TestTheDeclaredGapsNameRealCases(t *testing.T) {
	known, err := LoadDeclaration("known.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(known) == 0 {
		t.Skip("this server declares no gap, which is what the suite is for")
	}
	for name, reason := range known {
		if !slices.Contains(Names(), name) {
			t.Errorf("%s is declared and is no case", name)
		}
		if len(reason) < 20 {
			t.Errorf("%s is declared with %q, which does not say what the gap is", name, reason)
		}
	}
}

// TestLoadDeclarationRefusesWhatItCannotUse: a declaration that names a case
// the suite does not hold, or gives no reason, is refused rather than read
// as an empty one.
func TestLoadDeclarationRefusesWhatItCannotUse(t *testing.T) {
	if known, err := LoadDeclaration(""); err != nil || known != nil {
		t.Errorf("no path is no declaration: %v, %v", known, err)
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, content, want string
	}{
		{"unknown case", `{"note":"n","cases":{"case999Nothing":"a reason long enough"}}`, "no case of this suite"},
		{"no reason", `{"note":"n","cases":{"case008ExecWait":""}}`, "no reason"},
		{"unknown field", `{"cases":{},"nonesuch":1}`, "no declaration"},
		{"not JSON", `nonesuch`, "no declaration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "known.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadDeclaration(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the declaration was read as %v, want an error naming %q", err, tc.want)
			}
		})
	}
	if _, err := LoadDeclaration(filepath.Join(dir, "nothing.json")); err == nil {
		t.Error("a declaration that is not there was read")
	}
}
