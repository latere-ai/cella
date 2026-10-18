// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestScratchPathsAreAddressable: on a sandbox whose driver reports its
// scratch directories in status.paths, a path below the home or the
// temporary directory is admitted by the file routes beside the workspace,
// while the directories themselves, a path that is not clean, and anything
// else outside the workspace are refused; a sandbox that reports no paths
// admits the workspace alone.
func TestScratchPathsAreAddressable(t *testing.T) {
	obj := v1.Sandbox{Spec: v1.SandboxSpec{Workspace: v1.Workspace{Path: "/Users/dev/site"}}}
	obj.Status.Paths = &v1.SandboxPaths{Home: "/data/sandboxes/sbx_1/home", Tmp: "/data/sandboxes/sbx_1/tmp"}
	for p, want := range map[string]bool{
		"/Users/dev/site/index.html":            true,
		"/data/sandboxes/sbx_1/home/bin/helper": true,
		"/data/sandboxes/sbx_1/tmp/upload.png":  true,
		"/data/sandboxes/sbx_1/home":            false,
		"/data/sandboxes/sbx_1/home/../token":   false,
		"/data/sandboxes/sbx_1/run/token":       false,
		"/Users/dev/.ssh/id_ed25519":            false,
	} {
		if got := workspacePathError(obj, p) == nil; got != want {
			t.Errorf("the path %s is admitted %v, want %v", p, got, want)
		}
	}
	obj.Status.Paths = nil
	if workspacePathError(obj, "/data/sandboxes/sbx_1/home/bin/helper") == nil {
		t.Error("a sandbox reporting no scratch paths admitted one")
	}
}
