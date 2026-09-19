// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cella_test holds the tests about the module's shape rather than one
// package's behaviour. It has no source file, so it contributes no statement
// to the coverage gate and no import to any build list.
package cella_test

import (
	"bufio"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// module is the import path every package of this repository shares.
const module = "latere.ai/x/cella"

// shared is what any role package may reach: the contract types, the driver
// interface, and the host-pattern grammar the contract's host rule names
// (spec 003), which the manifest carries into everything that imports it.
// They hold no client, so importing one opens no connection.
var shared = []string{module + "/manifest", module + "/manifest/v1", module + "/runtime", "latere.ai/x/pkg/hostmatch"}

// engines is the client each package may reach beyond shared, one entry per
// package, with the reason it is there. A driver reaches the client of the
// engine it drives and nothing else; a role package reaches none. A slice that
// ports a driver adds its row, and an empty list is the strict case.
var engines = map[string][]string{
	"./manifest":       nil,
	"./manifest/v1":    nil,
	"./runtime":        nil,
	"./runtime/native": nil, // the native driver runs host processes: no client
	"./runtime/podman": nil, // the podman driver speaks libpod over net/http: no client module
	"./controller":     nil,
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
