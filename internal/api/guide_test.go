// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	document "latere.ai/x/cella/api"
	"latere.ai/x/cella/manifest"
)

// guide is docs/api.md, the orientation a reader follows before the OpenAPI
// document.
func guide(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestTheGuideNamesEveryPath holds docs/api.md to the served document: a
// route the document gains is a route the guide names, so the page a caller
// reads first never omits one the server answers.
func TestTheGuideNamesEveryPath(t *testing.T) {
	var doc struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(document.Document, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("the document has no paths")
	}
	page := guide(t)
	paths := make([]string, 0, len(doc.Paths))
	for path := range doc.Paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if !strings.Contains(page, path) {
			t.Errorf("docs/api.md does not name %s, which the document serves", path)
		}
	}
}

// envelopeCodes is every code errorEnvelope can answer: the default it starts
// from and each case of the switch that gives a code its status. It reads the
// source rather than a list kept beside it, so a code added to the switch is
// a code this test asks the guide for.
func envelopeCodes(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "api.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{"driver_unavailable"}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "errorEnvelope" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if tag, ok := sw.Tag.(*ast.Ident); !ok || tag.Name != "code" {
				return true
			}
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						code, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						codes = append(codes, code)
					}
				}
			}
			return false
		})
	}
	if len(codes) < 20 {
		t.Fatalf("found %d codes in errorEnvelope, too few to be the error table", len(codes))
	}
	return codes
}

// TestTheGuideCarriesTheErrorTable holds the guide's table of refusals to the
// envelope: every code the API answers is a row, and each row's status is the
// status the envelope gives that code.
func TestTheGuideCarriesTheErrorTable(t *testing.T) {
	page := guide(t)
	for _, code := range envelopeCodes(t) {
		status, _ := errorEnvelope(&manifest.Error{Code: code}, "req_test")
		row := regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(code) + "` \\| ([0-9]{3}) \\|")
		m := row.FindStringSubmatch(page)
		if m == nil {
			t.Errorf("docs/api.md has no row for the code %s", code)
			continue
		}
		if m[1] != strconv.Itoa(status) {
			t.Errorf("docs/api.md gives %s the status %s, the API answers %d", code, m[1], status)
		}
	}
}
