// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheLocalJobRunsUnderBubblewrap is spec 012's local tier in the verify
// workflow: the job installs the sandbox runtime on Linux, runs every local
// runtime test under it, fails on a test it skipped, and names the cases
// that only the real runtime proves.
func TestTheLocalJobRunsUnderBubblewrap(t *testing.T) {
	steps := jobSteps(t, filepath.Join(".github", "workflows", "verify.yml"), "e2e-local")
	for _, want := range []string{
		"bubblewrap", "@anthropic-ai/sandbox-runtime", "kernel.apparmor_restrict_unprivileged_userns=0", "srt echo ok",
		"-run '^TestLocal' ./runtime/local ./cmd/cellad", "'^--- SKIP'",
		"TestLocalConformanceUnderSRT", "TestLocalGrantsNeverReachTheDenyTable", "TestLocalReadsOutsideGrantsAreRefused",
		"TestLocalDataDirInsideHome", "TestLocalEndsAnOrphanedAttachedCommandUnderSRT", "TestLocalWorkerEndToEnd",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("the e2e-local job does not carry %q", want)
		}
	}
}

// TestEveryTestThatNeedsTheSandboxRuntimeIsALocalTest holds the job's reach:
// a test that skips where the sandbox runtime is missing runs in the
// e2e-local job only if its name starts with TestLocal, which is what the job
// selects.
func TestEveryTestThatNeedsTheSandboxRuntimeIsALocalTest(t *testing.T) {
	marks := []string{"underSRT(", "the sandbox runtime is not installed here"}
	found := 0
	for _, dir := range []string{filepath.Join("runtime", "local"), filepath.Join("cmd", "cellad")} {
		paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, source, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
					continue
				}
				body := string(source[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
				needs := false
				for _, mark := range marks {
					needs = needs || strings.Contains(body, mark)
				}
				if !needs {
					continue
				}
				found++
				if !strings.HasPrefix(fn.Name.Name, "TestLocal") {
					t.Errorf("%s in %s needs the sandbox runtime and is not a TestLocal test, so the e2e-local job never runs it",
						fn.Name.Name, path)
				}
			}
		}
	}
	if found == 0 {
		t.Error("no test needs the sandbox runtime, so this check reads nothing")
	}
}
