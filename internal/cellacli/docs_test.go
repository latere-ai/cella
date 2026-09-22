// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/cellacli"
)

// TestTheDocumentCarriesTheCommandsHelp is design 011's rule for
// docs/cli.md: the command table a reader follows is the binary's own, so a
// command added or removed without the document is a failure.
func TestTheDocumentCarriesTheCommandsHelp(t *testing.T) {
	help := runWith(t, cellacli.Env{Args: []string{"help"}, Getenv: environment(nil)})
	if help.code != 0 {
		t.Fatalf("`cella help` exited %d", help.code)
	}
	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(document)
	// Every command line of the help text, which is the table between
	// `Commands:` and the blank line after it.
	_, rest, ok := strings.Cut(help.stdout, "Commands:\n")
	if !ok {
		t.Fatal("the help text has no command table")
	}
	table, _, _ := strings.Cut(rest, "\n\n")
	for line := range strings.SplitSeq(strings.TrimRight(table, "\n"), "\n") {
		row := strings.TrimSpace(line)
		if !strings.Contains(body, row) {
			t.Errorf("docs/cli.md does not carry the row %q of `cella help`", row)
		}
	}
	// The exit codes a reader decides on are the ones the command uses.
	for _, want := range []string{
		"CELLA_URL", "CELLA_TOKEN", "/run/cella/token",
		"| 0 |", "| 1 |", "| 2 |", "| 3 |", "| 4 |", "| 5 |", "| 7 |",
		"125", "126", "127",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/cli.md does not name %q", want)
		}
	}
	// Every command the binary has is in the document.
	for _, name := range []string{
		"apply", "get", "delete", "start", "stop", "exec", "attach", "logs", "cp", "files", "egress", "version",
		"port-forward",
	} {
		if !strings.Contains(body, "cella "+name) {
			t.Errorf("docs/cli.md does not show `cella %s`", name)
		}
	}
}

// TestTheSkillIsSmallEnoughToBeResident is design 011's rule for the skill:
// its frontmatter is what an agent carries whether it uses the command or
// not, so the two fields together are at most 256 bytes.
func TestTheSkillIsSmallEnoughToBeResident(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "skills", "cella", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(document)
	if !strings.HasPrefix(body, "---\n") {
		t.Fatal("the skill has no frontmatter")
	}
	frontmatter, rest, ok := strings.Cut(strings.TrimPrefix(body, "---\n"), "---\n")
	if !ok {
		t.Fatal("the skill's frontmatter does not end")
	}
	if len(frontmatter) > 256 {
		t.Fatalf("the frontmatter is %d bytes, and design 011 bounds it at 256", len(frontmatter))
	}
	for _, field := range []string{"name:", "description:"} {
		if !strings.Contains(frontmatter, field) {
			t.Errorf("the frontmatter has no %s", field)
		}
	}
	// The body teaches what the conformance scenario needs: the two
	// variables, the in-sandbox default, a manifest, the verbs and how to
	// read a refusal.
	for _, want := range []string{
		"CELLA_URL", "CELLA_TOKEN", "/run/cella/token",
		"cella apply", "cella get", "cella exec", "cella files", "cella logs", "cella delete",
		"-v", "| Exit |",
	} {
		if !strings.Contains(rest, want) {
			t.Errorf("the skill does not teach %q", want)
		}
	}
	// A skill an agent reads names no host of any one installation. The API
	// group is the one legal occurrence of the name, and the slash that
	// follows it is what tells it from a host.
	for _, coordinate := range []string{"cella." + "latere.ai", "sandbox." + "latere.ai"} {
		for line := range strings.SplitSeq(rest, "\n") {
			if strings.Contains(line, coordinate) && !strings.Contains(line, coordinate+"/") {
				t.Errorf("the skill names %q, and this repository is public", coordinate)
			}
		}
	}
}
