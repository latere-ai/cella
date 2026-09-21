// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/cellacli"
)

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite the golden files of this package")

// atTheSameHour is the clock every golden run is measured against, so the
// AGE column is a fact of the fixture and not of the day the test runs.
var atTheSameHour = time.Date(2026, 9, 20, 10, 15, 0, 0, time.UTC)

// TestTheOutputsAreWhatTheGoldenFilesHold pins both outputs of every
// command: the columns a person reads and the JSON a program reads.
func TestTheOutputsAreWhatTheGoldenFilesHold(t *testing.T) {
	p := newPlane(t, routes)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sandboxes-columns", []string{"get", "sandboxes"}},
		{"sandboxes-wide", []string{"get", "sandboxes", "-o", "wide"}},
		{"sandboxes-name", []string{"get", "sandboxes", "-o", "name"}},
		{"sandboxes-json", []string{"get", "sandboxes", "--json"}},
		{"sandbox-columns", []string{"get", "sandbox", "dev"}},
		{"sandbox-name", []string{"get", "sandbox", "dev", "-o", "name"}},
		{"sandbox-json", []string{"get", "sandbox", "dev", "-o", "json"}},
		{"secrets-columns", []string{"get", "secrets"}},
		{"secrets-wide", []string{"get", "secrets", "-o", "wide"}},
		{"secrets-json", []string{"get", "secrets", "--json"}},
		{"secret-columns", []string{"get", "secret", "api"}},
		{"secret-name", []string{"get", "secret", "api", "-o", "name"}},
		{"egress-columns", []string{"egress", "dev"}},
		{"egress-json", []string{"egress", "dev", "--json"}},
		{"files-ls-columns", []string{"files", "ls", "dev:/workspace"}},
		{"files-ls-json", []string{"files", "ls", "dev:/workspace", "--json"}},
		{"files-stat-json", []string{"files", "stat", "dev:/workspace/a.txt", "--json"}},
		{"files-mkdir-json", []string{"files", "mkdir", "dev:/workspace/sub", "--json"}},
		{"exec-json", []string{"exec", "dev", "--json", "--", "echo", "hello"}},
		{"apply-json", []string{"apply", "-f", "-", "--json"}},
		{"delete-json", []string{"delete", "sandbox", "dev", "--json"}},
		{"start-json", []string{"start", "dev", "--json"}},
		{"version-json", []string{"version", "--json"}},
		{"help", []string{"help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runWith(t, cellacli.Env{
				Args:   tc.args,
				Stdin:  strings.NewReader(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"example/image:1"}}`),
				Now:    func() time.Time { return atTheSameHour },
				Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "caller-token"}),
			})
			compare(t, tc.name, got.stdout)
		})
	}
}

// compare holds one output to its golden file.
func compare(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run `go test ./internal/cellacli -update` to write it", err)
	}
	if got != string(want) {
		t.Fatalf("the output is\n%s\nand %s holds\n%s", got, path, want)
	}
}

// TestOneObjectUnderJSONIsTheAPIsOwnBytes is design 011's fidelity rule: the
// response body is written as received, never decoded and re-encoded, so the
// field order is the API's.
func TestOneObjectUnderJSONIsTheAPIsOwnBytes(t *testing.T) {
	body := sandboxJSON("dev", "sbx_1", "Running")
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
	got := p.run(t, "", "get", "sandbox", "dev", "-o", "json")
	if got.stdout != body {
		t.Fatalf("the command wrote\n%s\nand the API answered\n%s", got.stdout, body)
	}
}

// TestAListThatSpannedPagesIsOneEnvelope: the pages are followed to the end
// and rendered as one envelope whose cursor is empty and whose items are
// unchanged.
func TestAListThatSpannedPagesIsOneEnvelope(t *testing.T) {
	first := strings.TrimSuffix(sandboxJSON("one", "sbx_1", "Running"), "\n")
	second := strings.TrimSuffix(sandboxJSON("two", "sbx_2", "Stopped"), "\n")
	p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[` + first + `],"next":"sbx_1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[` + second + `],"next":""}`))
	})
	got := p.run(t, "", "get", "sandboxes", "--json")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	var envelope struct {
		Items []json.RawMessage `json:"items"`
		Next  string            `json:"next"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &envelope); err != nil {
		t.Fatalf("the output is no envelope: %v\n%s", err, got.stdout)
	}
	if envelope.Next != "" {
		t.Errorf("the envelope carries the cursor %q, and every page was followed", envelope.Next)
	}
	if len(envelope.Items) != 2 {
		t.Fatalf("the envelope holds %d item(s)", len(envelope.Items))
	}
	if string(envelope.Items[0]) != first || string(envelope.Items[1]) != second {
		t.Fatalf("the items were re-encoded:\n%s\n%s", envelope.Items[0], envelope.Items[1])
	}
	if len(p.seen()) != 2 {
		t.Errorf("the list made %d call(s), and it spans two pages", len(p.seen()))
	}
}

// TestTheAgeColumnReadsAtEveryScale: a column a person reads is one unit at
// the scale of the value.
func TestTheAgeColumnReadsAtEveryScale(t *testing.T) {
	for _, tc := range []struct {
		created string
		want    string
	}{
		{"2026-09-20T10:14:30Z", "30s"},
		{"2026-09-20T09:34:00Z", "41m"},
		{"2026-09-20T02:15:00Z", "8h"},
		{"2026-09-17T10:15:00Z", "3d"},
		{"2026-09-20T10:20:00Z", "0s"},
	} {
		p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"i"},` +
				`"status":{"id":"sbx_1","phase":"Running","owner":"alice","createdAt":"` + tc.created + `"}}`))
		})
		got := runWith(t, cellacli.Env{
			Args:   []string{"get", "sandbox", "dev"},
			Now:    func() time.Time { return atTheSameHour },
			Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "t"}),
		})
		if !strings.Contains(got.stdout, tc.want) {
			t.Errorf("a sandbox created at %s reads\n%s\nand the age column should be %s", tc.created, got.stdout, tc.want)
		}
	}
}

// TestAFieldTheServerLeftEmptyIsADash: a column with nothing in it is
// visible as a column, and an object carrying fields this client does not
// know still renders.
func TestAFieldTheServerLeftEmptyIsADash(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},` +
			`"spec":{},"status":{"id":"sbx_1","phase":"Pending","somethingNewer":{"a":1}}}`))
	})
	got := runWith(t, cellacli.Env{
		Args:   []string{"get", "sandbox", "dev"},
		Now:    func() time.Time { return atTheSameHour },
		Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "t"}),
	})
	if got.code != 0 {
		t.Fatalf("a field this client does not know ended the command: %q", got.stderr)
	}
	if !strings.Contains(got.stdout, "Unknown") || !strings.Contains(got.stdout, "-") {
		t.Fatalf("the row is\n%s", got.stdout)
	}
}

// TestOneObjectUnderYAMLIsTheAPIsOwnBytes: -o yaml asks the server for the
// syntax and writes what arrives, so the bytes are the API's and the command
// holds no encoder of its own.
func TestOneObjectUnderYAMLIsTheAPIsOwnBytes(t *testing.T) {
	const document = "apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\nmetadata:\n    name: dev\n"
	var asked string
	p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(document))
	})
	got := p.run(t, "", "get", "sandbox", "dev", "-o", "yaml")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	if asked != "application/yaml" {
		t.Errorf("the command asked with Accept %q", asked)
	}
	if got.stdout != document {
		t.Fatalf("the command wrote\n%s\nand the API answered\n%s", got.stdout, document)
	}
	// A list asks the same way and writes the page through.
	const page = "items:\n    - kind: Sandbox\nnext: \"\"\n"
	p = newPlane(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("Accept")
		_, _ = w.Write([]byte(page))
	})
	if got = p.run(t, "", "get", "sandboxes", "-o", "yaml"); got.stdout != page || asked != "application/yaml" {
		t.Fatalf("a list wrote %q under Accept %q", got.stdout, asked)
	}
}
