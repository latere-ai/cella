// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cella_test holds the tests about the module's shape rather than one
// package's behavior. It has no source file, so it contributes no statement
// to the coverage gate and no import to any build list.
package cella_test

import (
	"bufio"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// module is the import path every package of this repository shares.
const module = "latere.ai/x/cella"

// shared is what any role package may reach: the contract types, the driver
// interface, the boundary compiler, the host-pattern grammar the contract's
// host rule names (spec 003), and the YAML parser its decoder reads a body
// with, each of which the manifest carries into everything that imports it.
// Every entry is itself held to this rule: a parser and a pattern matcher
// compute over bytes, so importing one opens no connection.
var shared = []string{
	module + "/manifest", module + "/manifest/v1", module + "/runtime",
	module + "/runtime/display", module + "/egress",
	"latere.ai/x/pkg/hostmatch", "latere.ai/x/pkg/egress/placeholder",
	"go.yaml.in/yaml/v3",
}

// engines is the client each package may reach beyond shared, one entry per
// package, with the reason it is there. A driver reaches the client of the
// engine it drives and nothing else; a role package reaches none. A slice that
// ports a driver adds its row, and an empty list is the strict case.
var engines = map[string][]string{
	"./manifest":        nil,
	"./manifest/v1":     nil,
	"./runtime":         nil,
	"./runtime/display": nil, // the desktop vocabulary: the standard library alone
	"./runtime/native":  nil, // the native driver runs host processes: no client
	// The podman driver speaks libpod over net/http, so it needs no client
	// module; fileshell is the file programs it shares with the other
	// container driver.
	"./runtime/podman": {module + "/runtime/internal/fileshell"},
	"./controller":     nil,
	"./egress":         nil, // the boundary compiler computes over the contract types
	// The remote driver and the worker role are the two halves of the seam
	// of spec 021. Neither opens a connection of its own: the driver holds a
	// transport its caller supplies, and the worker's own socket is the
	// WebSocket the control plane's streams already admit. Neither reaches
	// the Postgres driver, because a data plane holds no store.
	"./runtime/remote": nil,
}

// TestRootPackagesDialNothing reads each package's whole build list, not its
// own import block. An indirect import opens a connection as well as a direct
// one, and a policy engine, an HTTP client, a database driver, or an identity
// library that arrives two packages down is the shape this catches.
func TestRootPackagesDialNothing(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Go toolchain is not on PATH, so the build lists cannot be read: %v", err)
	}
	for pkg, allow := range engines {
		t.Run(strings.TrimPrefix(pkg, "./"), func(t *testing.T) {
			for _, dep := range deps(t, goBin, pkg) {
				switch {
				case dep == module+strings.TrimPrefix(pkg, "."):
					continue // the package itself
				case slices.Contains(shared, dep), slices.Contains(allow, dep):
					continue
				case strings.HasPrefix(dep, module+"/internal/"):
					t.Errorf("%s reaches %s: a role package holds no policy and imports nothing under internal/", pkg, dep)
				default:
					t.Errorf("%s reaches %s, which is neither the standard library, %v, nor a client it is allowed %v", pkg, dep, shared, allow)
				}
			}
		})
	}
}

// clientReaches is the client package's whole build list beyond the standard
// library: the contract types it decodes into, and the error envelope of
// design 008 with the one package that envelope reaches.
var clientReaches = []string{
	module + "/manifest/v1",
	"latere.ai/x/pkg/httpjson",
	"github.com/google/uuid",
}

// TestTheClientPackageReachesNoServer holds the exported client to what a
// program outside this module can carry: importing it to speak /v1 builds the
// client and the contract types, and no store, driver, policy or package
// under internal/ arrives with it. The list is exact, so an entry the package
// stopped reaching fails as well and the list shrinks with it.
func TestTheClientPackageReachesNoServer(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Go toolchain is not on PATH, so the build list cannot be read: %v", err)
	}
	got := deps(t, goBin, "./client")
	for _, dep := range got {
		switch {
		case dep == module+"/client", slices.Contains(clientReaches, dep):
		case strings.HasPrefix(dep, module+"/internal/"):
			t.Errorf("the client reaches %s: a consumer outside this module cannot build a package under internal/", dep)
		default:
			t.Errorf("the client reaches %s, which is neither the standard library nor %v", dep, clientReaches)
		}
	}
	for _, want := range clientReaches {
		if !slices.Contains(got, want) {
			t.Errorf("the client no longer reaches %s; drop it from the list", want)
		}
	}
}

// TestTheCommandSpeaksThroughTheExportedClient: the command and every other
// caller in this module use the one client a consumer outside it imports, so
// there is no second copy under internal/ for the two to drift apart in.
func TestTheCommandSpeaksThroughTheExportedClient(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the Go toolchain is not on PATH, so the build list cannot be read: %v", err)
	}
	for _, pkg := range []string{"./cmd/cella", "./test/conformance"} {
		got := deps(t, goBin, pkg)
		if !slices.Contains(got, module+"/client") {
			t.Errorf("%s does not reach the exported client", pkg)
		}
		for _, dep := range got {
			if strings.HasSuffix(dep, "/cellaclient") {
				t.Errorf("%s reaches %s, a client beside the exported one", pkg, dep)
			}
		}
	}
	if _, err := os.Stat(filepath.Join("internal", "cellaclient")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("internal/cellaclient is still in the tree: %v", err)
	}
}

// deps returns the non-standard packages in the build list of pkg.
func deps(t *testing.T, goBin, pkg string) []string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), goBin, "list", "-deps",
		"-f", "{{.ImportPath}} {{.Standard}}", pkg)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing the build list of %s: %v: %s", pkg, err, strings.TrimSpace(stderr.String()))
	}
	var deps []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		path, std, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			t.Fatalf("go list wrote a line without a Standard field: %q", sc.Text())
		}
		if std == "false" {
			deps = append(deps, path)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the build list of %s: %v", pkg, err)
	}
	if len(deps) == 0 {
		t.Fatalf("the build list of %s holds no package of this module; the check would pass vacuously", pkg)
	}
	return deps
}

// confined names the calls that return a secret's plaintext and the one
// caller each of them is allowed. Design 010 gives the decrypting method one
// caller and design 018 says which: the control plane's compile path, which
// hands what it reads to the map the gateway receives and to nothing else.
var confined = []struct {
	// what names the call, for the failure sentence; match reports whether
	// one call expression is it; caller is the only function allowed to
	// make it, written as the receiver type and the method name.
	what   string
	match  func(*ast.SelectorExpr) bool
	caller string
}{
	{
		what:   "Values().Open",
		match:  func(sel *ast.SelectorExpr) bool { return sel.Sel.Name == "Open" && receiverCall(sel) == "Values" },
		caller: "Controlled.OpenValue",
	},
	{
		what:   "OpenValue",
		match:  func(sel *ast.SelectorExpr) bool { return sel.Sel.Name == "OpenValue" },
		caller: "Controller.secretViews",
	},
}

// TestValuesAreConfined parses every non-test file of this module and reports
// any call that returns a secret's plaintext from a function the table above
// does not name. It reads the syntax rather than grepping, so a call written
// across two lines, behind a variable of the interface type, or in a file a
// grep pattern did not anticipate is still a failure here.
func TestValuesAreConfined(t *testing.T) {
	callers := map[string]map[string]bool{}
	fset := token.NewFileSet()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		// storetest is the store contract's own suite: every file in it is
		// a test that happens not to carry the suffix, because one suite
		// runs against two adapters from their own packages.
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata" || d.Name() == "storetest"):
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			from := qualifiedName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				for _, c := range confined {
					if !c.match(sel) {
						continue
					}
					if callers[c.what] == nil {
						callers[c.what] = map[string]bool{}
					}
					callers[c.what][from] = true
				}
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("reading the module's files: %v", walkErr)
	}
	for _, c := range confined {
		found := callers[c.what]
		if len(found) == 0 {
			t.Errorf("%s has no caller at all; the confinement check would pass vacuously", c.what)
			continue
		}
		for from := range found {
			if from != c.caller {
				t.Errorf("%s is called from %s; design 010 gives it one caller, %s", c.what, from, c.caller)
			}
		}
	}
}

// receiverCall is the method name of a call the selector is taken on, as in
// the "Values" of tx.Values().Open, and the empty string when the selector
// stands on anything else.
func receiverCall(sel *ast.SelectorExpr) string {
	call, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return ""
	}
	inner, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return inner.Sel.Name
}

// qualifiedName is a function as the table names it: the receiver's type and
// the method, or the function's own name where it has no receiver.
func qualifiedName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
