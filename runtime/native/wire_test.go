// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogWriterWritesOneLinePerChunk holds the log file to one JSON value per
// line, each ended by a newline, with the output bytes as base64.
func TestLogWriterWritesOneLinePerChunk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &logWriter{file: f}
	for _, p := range []string{"one\n", "two\xff\n"} {
		if _, err = w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(string(raw), "}\n") {
		t.Fatalf("log is %q, want two newline-ended lines", raw)
	}
	if !strings.Contains(lines[0], `"Data":"b25lCg=="`) || !strings.Contains(lines[1], `"Data":"dHdv/wo="`) {
		t.Fatalf("log lines are %q", lines)
	}
}
