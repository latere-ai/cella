// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestCheckSubcommandReportsEveryRequirement: the installation the test
// harness builds is complete, so every mandatory line passes, every
// optional one is reported not configured, and the process exits 0.
func TestCheckSubcommandReportsEveryRequirement(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(t.Context(), []string{"check"}, env(identity(t, map[string]string{
		"CELLA_DATA_DIR": t.TempDir(),
	})), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	for _, want := range []string{"identity", "authorizer", "backend", "data directory", "admission", "sink", "store", "gateway"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report has no %s line:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("a complete installation reported a failure:\n%s", out.String())
	}
}

// TestCheckSubcommandExitsOneOnAnyFailure is what makes the command usable
// as a Job or an init container: the exit code is the answer.
func TestCheckSubcommandExitsOneOnAnyFailure(t *testing.T) {
	var out bytes.Buffer
	code := run(t.Context(), []string{"check"}, env(map[string]string{}), &out, io.Discard)
	if code != 1 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("the failure is not in the report:\n%s", out.String())
	}
}

func TestCheckSubcommandVersionAndBadFlag(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.Context(), []string{"check", "-version"}, env(nil), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "cellad dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
	if code := run(t.Context(), []string{"check", "-no-such-flag"}, env(nil), io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

// TestVersionSubcommandPrintsTheIdentity: a Job is one image and one
// argument, so the build identity a release pins is a subcommand and not
// only a flag.
func TestVersionSubcommandPrintsTheIdentity(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.Context(), []string{"version"}, env(nil), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "cellad dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
	if code := run(t.Context(), []string{"version", "-no-such-flag"}, env(nil), io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
