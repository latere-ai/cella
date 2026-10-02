// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// variableName matches a whole CELLA_ name, with a trailing `_*` where a
// sentence names every variable under a prefix, as in CELLA_CAPACITY_*.
var variableName = regexp.MustCompile(`CELLA_[A-Z0-9_]*[A-Z0-9](_\*)?`)

// variablesRead reports whether the module reads a CELLA_ name: a non-test Go
// file spells it as a whole string literal, which is how a variable is read.
// A prefix form is read where some literal starts with the prefix.
func variablesRead(t *testing.T) func(name string) bool {
	t.Helper()
	_, literals := declaredNames(t)
	return func(name string) bool {
		if prefix, ok := strings.CutSuffix(name, "*"); ok {
			for literal := range literals {
				if strings.HasPrefix(literal, prefix) && variableName.MatchString(literal) {
					return true
				}
			}
			return false
		}
		return literals[name]
	}
}

// TestTheSourceNamesOnlyVariablesTheModuleReads: a comment or a message in
// the module's own code that names a CELLA_ variable names one the module
// reads, so a reader never sets a variable because a doc comment told them
// it overrides something, when nothing reads it.
func TestTheSourceNamesOnlyVariablesTheModuleReads(t *testing.T) {
	read := variablesRead(t)
	stale := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "out" || d.Name() == "testdata" || d.Name() == "specs"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range variableName.FindAllString(string(body), -1) {
			if !read(name) && !slices.Contains(stale[name], path) {
				stale[name] = append(stale[name], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(stale))
	for name := range stale {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Errorf("%s names %s, which no code in the module reads", strings.Join(stale[name], ", "), name)
	}
}
