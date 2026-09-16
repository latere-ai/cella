// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
)

// TestVocabularyMatchesSpec006 holds the table to spec 006's, read from
// the spec in this repository rather than from a copy of it: the table's
// text is the contract, an action the spec names and the package does
// not is a question no endpoint can be asked, and an action the package
// names and the spec does not is a row nobody decided on.
func TestVocabularyMatchesSpec006(t *testing.T) {
	want := actionsOfSpec006(t)
	if len(want) != 32 {
		t.Fatalf("spec 006's table names %d actions, want the thirty-two: %v", len(want), want)
	}
	if got := Vocabulary().Actions; !slices.Equal(got, want) {
		t.Errorf("the vocabulary drifted from spec 006:\n got %v\nwant %v", got, want)
	}
	for _, a := range want {
		if !Known(a.Name) {
			t.Errorf("%s: spec 006 names it and the package does not know it", a.Name)
		}
		if got := Kind(a.Name); got != a.Kind {
			t.Errorf("%s acts on %q; spec 006's row says %q", a.Name, got, a.Kind)
		}
	}
}

// TestUnknownActionIsUnknown: a string outside the table has no kind and
// is not known, which is what the client refuses before the wire and
// what an endpoint answers a 400 for.
func TestUnknownActionIsUnknown(t *testing.T) {
	for _, action := range []string{"", "sandbox", "sandbox.explode", "Sandbox.read", "sandbox.read "} {
		if Known(action) || Kind(action) != "" {
			t.Errorf("%q reads as one of the vocabulary", action)
		}
	}
}

// TestVocabularyIsWellFormed: the declared table is one the shared
// contract accepts, which is what proves no row names an action twice or
// leaves a kind empty.
func TestVocabularyIsWellFormed(t *testing.T) {
	v, err := authz.NewVocabulary(Core, table...)
	if err != nil {
		t.Fatalf("the declared table is not a vocabulary: %v", err)
	}
	if got := Vocabulary(); got.Core != v.Core || !slices.Equal(got.Actions, v.Actions) {
		t.Errorf("Vocabulary() = %+v; the validated table is %+v", got, v)
	}
	if want := []string{KindSandbox, KindSecret, KindVolume, KindSandboxSet, KindEnvironment}; !slices.Equal(v.Kinds(), want) {
		t.Errorf("the table names the kinds %v; Cella's five are %v", v.Kinds(), want)
	}
}

// TestListActionsAreNamedList: every action whose answer is a page is
// named with the .list suffix the scaffold routes by, and no other
// action carries that suffix. One kind, one list action.
func TestListActionsAreNamedList(t *testing.T) {
	byKind := map[string][]string{}
	for _, a := range table {
		if !authz.IsList(a.Name) {
			if strings.Contains(a.Name, "list") {
				t.Errorf("%s reads as a list action and does not end in .list", a.Name)
			}
			continue
		}
		byKind[a.Kind] = append(byKind[a.Kind], a.Name)
	}
	for _, kind := range Vocabulary().Kinds() {
		if len(byKind[kind]) != 1 {
			t.Errorf("%s has the list actions %v; a kind is paged by exactly one", kind, byKind[kind])
		}
	}
}

// TestActionsIsACopy: the table is the package's and a caller that sorts
// or appends to what it was handed changes nothing here.
func TestActionsIsACopy(t *testing.T) {
	first := Actions()
	first[0] = "sandbox.explode"
	if Actions()[0] != ActionSandboxCreate {
		t.Error("Actions returns the table itself; it returns a copy")
	}
	v := Vocabulary()
	v.Actions[0] = authz.Action{Name: "sandbox.explode", Kind: KindSandbox}
	if Vocabulary().Actions[0].Name != ActionSandboxCreate {
		t.Error("Vocabulary returns the table itself; it returns a copy")
	}
	if got := Actions(); len(got) != len(table) {
		t.Errorf("Actions lists %d of %d actions", len(got), len(table))
	}
}

var (
	// backticked matches one action name in the table's first column,
	// whole or abbreviated to its verb: `sandbox.read`, `.update`.
	backticked = regexp.MustCompile("`([a-z.]+)`")
	// kindOfRow matches the resource kind in the table's second column.
	kindOfRow = regexp.MustCompile(`"kind":\s*"([A-Za-z]+)"`)
)

// actionsOfSpec006 reads the action table of specs/006-identity.md: the
// rows under the "| Action | `resource` |" header, each row's first
// column expanded from its abbreviated form and each row's kind read
// from the resource shape in its second column, or carried over from the
// previous row of the same prefix where the shape is named in prose.
func actionsOfSpec006(t *testing.T) []authz.Action {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "specs", "006-identity.md"))
	if err != nil {
		t.Fatalf("spec 006: %v", err)
	}
	var out []authz.Action
	kinds, prefix, inTable := map[string]string{}, "", false
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if !inTable {
			inTable = strings.HasPrefix(line, "| Action | `resource` |")
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		names := backticked.FindAllStringSubmatch(cells[0], -1)
		if len(names) == 0 {
			continue
		}
		kind := ""
		if m := kindOfRow.FindStringSubmatch(cells[1]); m != nil {
			kind = m[1]
		}
		for _, m := range names {
			name := m[1]
			if strings.HasPrefix(name, ".") {
				name = prefix + name
			} else {
				prefix, _, _ = strings.Cut(name, ".")
			}
			if kind == "" {
				if kind = kinds[prefix]; kind == "" {
					t.Fatalf("spec 006's row for %s names no resource kind", name)
				}
			}
			kinds[prefix] = kind
			out = append(out, authz.Action{Name: name, Kind: kind})
		}
	}
	if !inTable {
		t.Fatal("spec 006 has no action table")
	}
	return out
}
