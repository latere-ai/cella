// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// imports returns every import path of the Go files in dir, test files
// included, so a dependency cannot enter through a test either.
func imports(t *testing.T, dir string) map[string]string {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[path] = name
			}
		}
	}
	return out
}

func TestManifestImports(t *testing.T) {
	// manifest/v1 is the schema every consumer types against, so it reaches
	// the standard library and nothing else. A path whose first element has a
	// dot is a module outside it.
	for path, file := range imports(t, "v1") {
		if strings.Contains(strings.Split(path, "/")[0], ".") {
			t.Errorf("manifest/v1 imports %q in %s", path, file)
		}
	}
	for path, file := range imports(t, ".") {
		switch {
		case strings.HasPrefix(path, "latere.ai/x/cella/internal"):
			t.Errorf("manifest imports %q in %s", path, file)
		case path == "latere.ai/x/cella/runtime" || strings.HasPrefix(path, "latere.ai/x/cella/runtime/"):
			t.Errorf("manifest imports %q in %s", path, file)
		}
	}
}

// TestNoLatereCoordinates holds the packages of this slice to the open source
// rule: nothing names the hosted installation. The API group is the one place
// the name appears, and a sandbox path or an example host is not a coordinate.
func TestNoLatereCoordinates(t *testing.T) {
	coordinates := []string{"https://cella.latere.ai", "sandbox.latere.ai", "sandbox-base", "doks", "latere.ai/x/sandbox"}
	for _, dir := range []string{".", "v1"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			// This file holds the list itself.
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == "imports_test.go" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, coordinate := range coordinates {
				if strings.Contains(string(body), coordinate) {
					t.Errorf("%s names the hosted coordinate %q", filepath.Join(dir, entry.Name()), coordinate)
				}
			}
		}
	}
}
