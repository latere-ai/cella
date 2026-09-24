// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// variable matches a whole CELLA_ name, and not the bare prefix a sentence
// uses to say every name that starts with it.
var variable = regexp.MustCompile(`CELLA_[A-Z0-9_]*[A-Z0-9]`)

// moduleRoot is the checkout, two directories above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// namesIn returns every CELLA_ name the non-test Go files under dir spell as
// a string literal, which is how a variable is read.
func namesIn(t *testing.T, dir string) map[string]bool {
	t.Helper()
	literal := regexp.MustCompile(`"(` + variable.String() + `)"`)
	names := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "specs", "out", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(body), -1) {
			names[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestTheConfigurationPageNamesEveryVariable holds docs/configuration.md to
// the code both ways. Every variable this package or the client reads is on
// the page, so an operator never meets a name the reference does not carry,
// and every variable the page names is one the module still spells, so a
// removed variable does not linger as a row somebody sets for nothing.
func TestTheConfigurationPageNamesEveryVariable(t *testing.T) {
	root := moduleRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)

	read := namesIn(t, filepath.Join(root, "internal", "config"))
	for name := range namesIn(t, filepath.Join(root, "client")) {
		read[name] = true
	}
	if len(read) < 50 {
		t.Fatalf("found %d variables read, too few to be the configuration", len(read))
	}
	var missing []string
	for name := range read {
		if !strings.Contains(page, "`"+name+"`") {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	for _, name := range missing {
		t.Errorf("docs/configuration.md does not name %s, which the configuration reads", name)
	}

	spelled := namesIn(t, root)
	var stale []string
	for _, name := range variable.FindAllString(page, -1) {
		if !spelled[name] && !slices.Contains(stale, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("docs/configuration.md names %s, which no code in the module reads or sets", name)
	}
}
