// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// examplePlane is the platform of the plane guide, built from the exported
// packages and nothing else.
const examplePlane = "examples/plane"

// planeGuide is the guide the example belongs to, and concernsTable the
// heading of the table this file grounds.
const (
	planeSpec     = "specs/016-building-a-plane.md"
	concernsTable = "### Where each concern goes"
)

// sharedMechanisms are the names the concerns table takes from the shared
// authorization contract rather than from this module: a platform writes
// them against latere.ai/x/pkg/authz, whose answer carries them. The list is
// exact in both directions, so one that moves into this module stops being
// an exception and one added to the table is seen.
var sharedMechanisms = []string{
	"limits", // the member an allow carries, which authorizer.WireLimits writes
}

// TestExamplePlaneBuilds compiles the example and holds it to the promise it
// is an example of: a platform composes the exported packages, so a line of
// it that reached inside internal/ would be an example of something no
// platform can build. The binary is written to a temporary directory, so the
// check is a compile and leaves nothing in the tree.
func TestExamplePlaneBuilds(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Go toolchain is not on PATH, so the example cannot be built: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), goBin, "build",
		"-o", filepath.Join(t.TempDir(), "plane"), "./"+examplePlane)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", examplePlane, err, out)
	}
	reached := 0
	for path, imports := range sourceImports(t, examplePlane) {
		for _, imported := range imports {
			switch {
			case strings.HasPrefix(imported, module+"/internal/"):
				t.Errorf("%s imports %s: a platform cannot, so neither may the example of one", path, imported)
			case strings.HasPrefix(imported, module+"/"):
				reached++
			}
		}
	}
	if reached < 3 {
		t.Fatalf("the example reaches %d packages of this module, which is too few to be composed of them", reached)
	}
}

// sourceImports is the import paths of every non-test Go file of dir, by
// file.
func sourceImports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, entry := range entries {
		name := filepath.Join(dir, entry.Name())
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = append(out[name], path)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no Go file", dir)
	}
	return out
}

// mechanism matches the names the concerns table writes in backticks. A
// token with a space or a brace is prose about a mechanism rather than its
// name, and a token that opens with a slash is a route.
var mechanism = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_./]*)`")

// TestConcernsTableIsGrounded holds the plane guide's concerns table to the
// tree: every mechanism a row names is a package of this module, a name it
// declares, or a string it carries. A guide that points a platform at
// something that does not exist is worse than one that says nothing.
func TestConcernsTableIsGrounded(t *testing.T) {
	declared, literals := declaredNames(t)
	elsewhere := map[string]bool{}
	checked := 0
	for _, r := range tableRows(t, planeSpec, concernsTable) {
		for _, cell := range r.cells {
			for _, m := range mechanism.FindAllStringSubmatch(cell, -1) {
				written := m[1]
				checked++
				switch {
				case strings.Contains(written, "/"):
					if _, err := os.Stat(written); err != nil {
						t.Errorf("%s:%d names the package %s, which this module does not have", r.file, r.line, written)
					}
				default:
					name := written
					if _, after, qualified := strings.Cut(written, "."); qualified {
						name = after
					}
					if declared[name] || literals[written] || literals[name] {
						continue
					}
					if slices.Contains(sharedMechanisms, name) {
						elsewhere[name] = true
						continue
					}
					t.Errorf("%s:%d names %s, which nothing in the tree declares", r.file, r.line, written)
				}
			}
		}
	}
	if checked < 8 {
		t.Fatalf("the concerns table names %d mechanisms, which is too few to be the table", checked)
	}
	for _, name := range sharedMechanisms {
		switch {
		case !elsewhere[name]:
			t.Errorf("sharedMechanisms carries %s, which the concerns table no longer names", name)
		case declared[name]:
			t.Errorf("sharedMechanisms carries %s, which this module now declares: the exception is stale", name)
		}
	}
}

// declaredNames is every name this module declares, and every string
// literal and struct tag it carries. The first is what a mechanism written
// as an identifier is held to; the second is what an action, a field path
// or a variable value is held to.
func declaredNames(t *testing.T) (declared, literals map[string]bool) {
	t.Helper()
	declared, literals = map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "out"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				declared[node.Name.Name] = true
			case *ast.TypeSpec:
				declared[node.Name.Name] = true
			case *ast.ValueSpec:
				for _, name := range node.Names {
					declared[name.Name] = true
				}
			case *ast.Field:
				for _, name := range node.Names {
					declared[name.Name] = true
				}
				if node.Tag != nil {
					literals[strings.Trim(node.Tag.Value, "`")] = true
					for _, part := range strings.FieldsFunc(strings.Trim(node.Tag.Value, "`"), func(r rune) bool {
						return r == '"' || r == ',' || r == ':' || r == ' '
					}) {
						literals[part] = true
					}
				}
			case *ast.BasicLit:
				if node.Kind == token.STRING {
					if value, err := strconv.Unquote(node.Value); err == nil {
						literals[value] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("reading the module: %v", err)
	}
	if len(declared) < 500 {
		t.Fatalf("the walk found %d declared names, which is too few to be the module", len(declared))
	}
	return declared, literals
}
