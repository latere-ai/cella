// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roots are the trees the check covers, relative to this package's directory.
// The walk is recursive, so "." carries every driver package under runtime/
// and a slice that ports one in adds no root; a slice that adds a sibling
// tree of runtime/ adds it here.
var roots = []string{".", "../controller"}

// drivers are the driver packages the walk has to reach. Naming them keeps a
// later narrowing of the walk from silently dropping a driver's tree, which is
// where an inherited image or namespace would land.
var drivers = []string{"native", "k8s"}

// group is the API group the contract stamps on an object. A hostname is a
// coordinate of one deployment; the group is part of the schema, so the
// qualified prefix "<group>/" is the one legal occurrence.
//
// The needles are assembled from pieces so this file, which lives inside the
// tree it walks, is not its own finding.
const group = "cella." + "latere.ai"

// coordinates name one deployment of the hosted plane: its image, its cluster,
// its node pool, its namespace. A fork that inherits any of them points at
// somebody else's infrastructure.
var coordinates = []struct{ needle, what string }{
	{"sandbox" + "-base", "a hosted image name"},
	{"latere" + "-k8s", "a hosted cluster name"},
	{"sandbox" + "-pool", "a hosted node pool name"},
	{"sandbox" + "-workloads", "a hosted namespace name"},
}

func TestNoLatereCoordinates(t *testing.T) {
	for _, root := range roots {
		for path, body := range files(t, root) {
			for _, finding := range scan(path, body) {
				t.Error(finding)
			}
		}
	}
}

// TestNoLatereCoordinatesCoversEveryDriver pins what the walk reaches: a
// clean tree passes whether or not the files under it were read.
func TestNoLatereCoordinatesCoversEveryDriver(t *testing.T) {
	walked := files(t, ".")
	for _, pkg := range drivers {
		found := false
		for path := range walked {
			if strings.HasPrefix(path, pkg+"/") && strings.HasSuffix(path, ".go") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the walk reaches no file of runtime/%s, so its coordinates are unchecked", pkg)
		}
	}
}

// TestNoLatereCoordinatesCatchesEachName drives the scan over planted bodies:
// a walk over a clean tree passes whether or not the check works.
func TestNoLatereCoordinatesCatchesEachName(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"image", "Image: \"registry.example.com/" + "sandbox" + "-base:latest\"\n", 1},
		{"cluster", "cluster: " + "latere" + "-k8s\n", 1},
		{"pool", "nodePool: " + "sandbox" + "-pool\n", 1},
		{"namespace", "namespace: " + "sandbox" + "-workloads\n", 1},
		{"hostInAURL", "url := \"https://" + group + "/v1/sandboxes\"\n", 1},
		{"bareHost", "issuer = \"" + group + "\"\n", 1},
		{"hostInAComment", "// the plane at " + group + " is one consumer\n", 1},
		{"apiGroup", "const Group = \"" + group + "/v1\"\n", 0},
		{"labelKey", "labels[\"" + group + "/owner\"] = owner\n", 0},
		{"clean", "// a sandbox base image named by the operator\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scan("planted.go", tc.body); len(got) != tc.want {
				t.Errorf("scan found %d findings, want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

// scan returns one finding per coordinate the body names.
func scan(path, body string) []string {
	var out []string
	for _, c := range coordinates {
		if strings.Contains(body, c.needle) {
			out = append(out, fmt.Sprintf("%s names %s (%q); this repository is public and a fork inherits its defaults", path, c.what, c.needle))
		}
	}
	for _, line := range hosts(body) {
		out = append(out, fmt.Sprintf("%s:%d reaches the host %q; only the API group prefix %q is allowed", path, line, group, group+"/"))
	}
	return out
}

// files reads every regular file under root. Every file, not every Go file: a
// coordinate in a testdata fixture, a YAML example, or a doc comment reaches a
// fork the same way.
func files(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(path)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no file; the check would pass vacuously", root)
	}
	return out
}

// hosts returns the line number of every occurrence of the group that names a
// host rather than the API group. Two bytes tell them apart: the group prefix
// is followed by "/" and a key, and a URL authority is preceded by "//". Both
// are read here rather than written as a pattern, because Go's regexp has no
// lookahead and the rule is one byte on each side.
func hosts(body string) []int {
	var lines []int
	for i := 0; ; {
		at := strings.Index(body[i:], group)
		if at < 0 {
			return lines
		}
		at += i
		end := at + len(group)
		bare := end >= len(body) || body[end] != '/'
		authority := at > 0 && body[at-1] == '/'
		if bare || authority {
			lines = append(lines, strings.Count(body[:at], "\n")+1)
		}
		i = end
	}
}
