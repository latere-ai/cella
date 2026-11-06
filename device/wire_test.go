// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestActivityKeepsAnEntryWithInvalidUTF8 holds a command or path that is not
// UTF-8 to a line in the log with U+FFFD in its place, never a lost entry.
func TestActivityKeepsAnEntryWithInvalidUTF8(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev-x")
	a, err := OpenActivity(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	if err = a.Write(Entry{Time: at, Type: "Exec", Command: []string{"cat", "caf\xe9"}, Path: "/tmp/\xff"}); err != nil {
		t.Fatalf("the entry was refused: %v", err)
	}
	got, err := ReadActivity(dir, time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("the log holds %v (%v), want the entry", got, err)
	}
	if got[0].Command[1] != "caf�" || got[0].Path != "/tmp/�" {
		t.Errorf("the entry reads back as %+v", got[0])
	}
}

// TestActivityLineBytes holds one line of the log to its exact bytes: members
// in declaration order, omitted when empty, one newline at the end.
func TestActivityLineBytes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev-x")
	a, err := OpenActivity(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err = a.Write(Entry{Time: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), Type: "Exec", Command: []string{"a<b"}}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the log is %v (%v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"time":"2026-11-05T09:00:00Z","type":"Exec","command":["a<b"]}` + "\n"; string(raw) != want {
		t.Fatalf("line is %q, want %q", raw, want)
	}
}

// TestKeyFileBytesAreStable holds the key file to one encoding of the same
// keys, entries in key order.
func TestKeyFileBytesAreStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	f := KeyFile{Path: path}
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for _, env := range []string{"dev-c", "dev-a", "dev-b"} {
		if err := f.Put(Key{Environment: env, Token: "t", JTI: "j", MintedAt: at, ExpiresAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	a, b, c := strings.Index(text, `"dev-a": {`), strings.Index(text, `"dev-b": {`), strings.Index(text, `"dev-c": {`)
	if a < 0 || a > b || b > c {
		t.Fatalf("keys are not in order:\n%s", text)
	}
	if !strings.HasPrefix(text, "{\n  \"dev-a\": {\n    \"environment\": \"dev-a\",") {
		t.Fatalf("file is not indented by two spaces:\n%s", text)
	}
}

// TestConsentIsStable holds the digest of the terms and facts shown to the
// same bytes for the same content.
func TestConsentIsStable(t *testing.T) {
	terms := Terms{Name: "n", SessionStart: SessionStartAllow, Folders: []Folder{}, Network: Network{Hosts: []string{"a.example"}}}
	facts := []Fact{{Kind: "k", Text: "t"}}
	first := Consent(terms, facts)
	for range 8 {
		if got := Consent(terms, facts); got != first {
			t.Fatalf("digest changed: %s then %s", first, got)
		}
	}
}

// A path or fact that carries a byte that is not UTF-8 still has a consent
// digest, the same one each time.
func TestConsentOfInvalidUTF8(t *testing.T) {
	terms := Terms{Name: "x", Folders: []Folder{{Path: "/tmp/\xff"}}}
	facts := []Fact{{Kind: FactLog, Text: "caf\xe9"}}
	first, second := Consent(terms, facts), Consent(terms, facts)
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digests %q and %q", first, second)
	}
}
