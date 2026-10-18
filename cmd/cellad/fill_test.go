// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFillDispatchesAndRefusesAMisuse: `cellad fill` is a subcommand, and a
// mode it does not know, a wrong count of paths, a negative cap or an
// unknown flag is a usage error, exit 2.
func TestFillDispatchesAndRefusesAMisuse(t *testing.T) {
	for _, args := range [][]string{
		{"fill"},
		{"fill", "unpack", "/a"},
		{"fill", "stage"},
		{"fill", "stage", "/a", "/b"},
		{"fill", "copy", "/a"},
		{"fill", "copy", "--max-bytes", "-1", "/a", "/b"},
		{"fill", "copy", "--no-such-flag", "/a", "/b"},
	} {
		var errOut bytes.Buffer
		if code := run(t.Context(), args, env(nil), io.Discard, &errOut); code != 2 || errOut.Len() == 0 {
			t.Errorf("%v = %d %q, want a usage error", args, code, errOut.String())
		}
	}
}

// TestFillStagesAndCopies: stage writes this executable where it is told,
// and copy writes a tree into a volume and prints what it did.
func TestFillStagesAndCopies(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "cellad")
	if code := run(t.Context(), []string{"fill", "stage", staged}, env(nil), io.Discard, io.Discard); code != 0 {
		t.Fatalf("stage exited %d", code)
	}
	if info, err := os.Stat(staged); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the staged program is %v %v", info, err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "grader"), []byte("grade"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "volume")
	var out bytes.Buffer
	if code := run(t.Context(), []string{"fill", "copy", "--max-bytes", "100", src, dst}, env(nil), &out, io.Discard); code != 0 {
		t.Fatalf("copy exited %d", code)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "grader")); err != nil || string(body) != "grade" {
		t.Fatalf("the copy holds %q %v", body, err)
	}
	if !strings.Contains(out.String(), "copied 1 files") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// TestFillLeavesItsReasonInTheTerminationLog: a copy past the cap writes
// SourceTooLarge at the head of the termination log, which is what the
// driver reads the reason from, and any other failure writes its sentence
// with no reason; a log that does not exist is not created.
func TestFillLeavesItsReasonInTheTerminationLog(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "termination-log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	args := []string{"copy", "--max-bytes", "10", src, filepath.Join(t.TempDir(), "v")}
	if code := fillWith(args, io.Discard, &errOut, logPath); code != 1 {
		t.Fatalf("a copy past the cap exited %d", code)
	}
	written, err := os.ReadFile(logPath)
	if err != nil || !strings.HasPrefix(string(written), "SourceTooLarge: ") || !strings.Contains(errOut.String(), "SourceTooLarge: ") {
		t.Fatalf("the termination log holds %q (%v), stderr %q", written, err, errOut.String())
	}
	if code := fillWith([]string{"copy", filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "v")}, io.Discard, io.Discard, logPath); code != 1 {
		t.Fatalf("a copy of an absent root exited %d", code)
	}
	if written, err := os.ReadFile(logPath); err != nil || strings.HasPrefix(string(written), "Source") || len(written) == 0 {
		t.Fatalf("the termination log holds %q (%v), want the failure with no reason", written, err)
	}
	absent := filepath.Join(t.TempDir(), "no-log")
	if code := fillWith([]string{"stage", filepath.Join(t.TempDir(), "absent", "cellad")}, io.Discard, io.Discard, absent); code != 1 {
		t.Fatalf("a stage into a missing directory exited %d", code)
	}
	if _, err := os.Stat(absent); err == nil {
		t.Fatal("a termination log was created outside a Pod")
	}
}
