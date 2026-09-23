// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The two markers that fence the runtime stage every image shares, and the
// files that carry it.
const (
	stageOpen  = "# >>> shared runtime base <<<"
	stageClose = "# <<< shared runtime base >>>"
)

var imageFiles = []string{"Dockerfile", "Dockerfile.ci"}

// runtimeStage is the text between the two markers of one Dockerfile. A
// marker that is missing or repeated is a failure and not an empty stage,
// because an empty stage would compare equal to another empty one.
func runtimeStage(t *testing.T, name, text string) string {
	t.Helper()
	if strings.Count(text, stageOpen) != 1 || strings.Count(text, stageClose) != 1 {
		t.Fatalf("%s: the runtime stage markers appear exactly once each", name)
	}
	_, rest, _ := strings.Cut(text, stageOpen)
	stage, _, _ := strings.Cut(rest, stageClose)
	return stage
}

// TestRuntimeStagesMatch is spec 014's rule for the images: Dockerfile and
// Dockerfile.ci are byte-identical between the markers, the stage is the
// distroless static base running as nonroot with both ports exposed, and
// the release file has no build stage, so a released image differs from a
// developer's in where the binary came from and in nothing else.
func TestRuntimeStagesMatch(t *testing.T) {
	stages := map[string]string{}
	for _, name := range imageFiles {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		stage := runtimeStage(t, name, text)
		stages[name] = stage
		if !strings.Contains(stage, "\nFROM gcr.io/distroless/static-debian12:nonroot") {
			t.Errorf("%s: the shared stage is not the distroless static base:\n%s", name, stage)
		}
		for _, want := range []string{"\nEXPOSE 8080 8081\n", "\nUSER nonroot:nonroot\n", "\nVOLUME [\"/var/lib/cella\"]\n"} {
			if !strings.Contains(stage, want) {
				t.Errorf("%s: the shared stage lacks %q", name, strings.TrimSpace(want))
			}
		}
		if name == "Dockerfile" {
			continue
		}
		if strings.Contains(text, "\nRUN ") || strings.Contains(text, "FROM golang") {
			t.Errorf("%s: a release image has no build stage; it copies the binary the pipeline built", name)
		}
		if !strings.Contains(text, "\nARG TARGETARCH\nCOPY dist/cellad_linux_${TARGETARCH} /usr/local/bin/cellad\n") {
			t.Errorf("%s: the binary is COPY dist/cellad_linux_${TARGETARCH}, the file buildx substitutes per platform", name)
		}
		if !strings.Contains(text, "\nENTRYPOINT [\"/usr/local/bin/cellad\"]\n") {
			t.Errorf("%s: the entry point is /usr/local/bin/cellad", name)
		}
	}
	for _, name := range imageFiles[1:] {
		if stages[name] != stages["Dockerfile"] {
			t.Errorf("%s: the runtime stage differs from Dockerfile's between the markers:\n--- Dockerfile\n%s\n--- %s\n%s",
				name, stages["Dockerfile"], name, stages[name])
		}
	}
}

// imageRef matches a registry reference under ghcr.io and captures the
// namespace segment: a literal there would be a fixed namespace.
var imageRef = regexp.MustCompile(`ghcr\.io/([^/\s"'` + "`" + `]+)/`)

// derived reports whether a namespace segment is computed at run time or
// is a placeholder a reader fills, rather than an account. `example` is
// the reserved name a document uses for an image nobody publishes.
func derived(segment string) bool {
	return strings.HasPrefix(segment, "$") || strings.Contains(segment, "<") ||
		segment == "OWNER" || segment == "example"
}

// skipDirs are the trees the walks below do not read: the repository's own
// history, an agent's worktrees, and build output.
//
// specs/ is read by the tests that own it and not by this one: a spec
// states what another repository pins, which is a design decision written
// down and not an artifact anybody installs.
var skipDirs = map[string]bool{
	".git": true, ".claude": true, "out": true, "dist": true, "node_modules": true, "specs": true,
}

// sources walks every readable text file of the tree, less this file,
// which holds the needles the walks look for.
func sources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || path == "release_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		out[filepath.ToSlash(path)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the walk read no file; the checks would pass vacuously")
	}
	return out
}

// TestReleasePublishesUnderTheOwnersNamespace: no workflow, manifest,
// document, script or Dockerfile names a fixed image namespace under
// ghcr.io. The published reference derives from the repository owner at
// run time, so a fork's tag publishes under the fork, and a repository
// variable overrides it for an installation that publishes elsewhere.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	for path, body := range sources(t) {
		for i, line := range strings.Split(body, "\n") {
			for _, m := range imageRef.FindAllStringSubmatch(line, -1) {
				if !derived(m[1]) {
					t.Errorf("a fixed image namespace: %s:%s: %s", path, strconv.Itoa(i+1), m[0])
				}
			}
		}
	}
	release, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GITHUB_REPOSITORY_OWNER",
		"tr '[:upper:]' '[:lower:]'",
		"RELEASE_IMAGE_NAMESPACE",
	} {
		if !strings.Contains(string(release), want) {
			t.Errorf("release.yml does not read %s; the namespace is derived and overridable, never written down", want)
		}
	}
}

// coordinates name one deployment of one hosted plane: its host, its
// image, its node pool, its namespace, its account. A fork that inherited
// any of them would point at somebody else's infrastructure. The needles
// are assembled from pieces so this file is not its own finding.
var coordinates = []struct{ needle, what string }{
	{"cella." + "latere.ai", "a hosted host name"},
	{"sandbox." + "latere.ai", "a hosted host name"},
	{"auth." + "latere.ai", "a hosted host name"},
	{"environments." + "latere.ai", "a hosted host name"},
	{"sandbox" + "-base", "a hosted image name"},
	{"latere" + "-k8s", "a hosted cluster name"},
	{"sandbox" + "-pool", "a hosted node pool name"},
	{"sandbox" + "-workloads", "a hosted namespace name"},
	{"cella" + "-block-storage", "a hosted storage class"},
	{"ghcr" + "-pull", "a hosted pull secret"},
	{"platformd", "a hosted service"},
	{"sandboxd", "a hosted service"},
}

// released are the trees a release hands an operator, plus the workflows
// that build it and the defaults the binary ships with. The walk below reads
// every file of the repository, and these are held to have been read, so a
// narrowed walk cannot pass by reading nothing where it matters most.
var released = []string{"deploy", "docs", ".github", "internal", "tools", "skills", "cmd", "controller", "runtime", "egress", "manifest", "api", "authorizer", "examples", "test"}

// needleFiles hold the coordinate lists themselves, each assembled so the file
// is not its own finding, and are the one place a needle may be spelled out.
var needleFiles = map[string]bool{
	"runtime/coordinates_test.go": true, "manifest/imports_test.go": true, "rules_test.go": true,
}

// schemaGroup reports whether every occurrence of the API group on a line is
// the schema rather than a host: qualified by the key that follows it, the
// group constant itself, the identity gate's api_group, or a subdomain of an
// example domain. One that follows a scheme is a URL's host.
func schemaGroup(line string) bool {
	group := "cella." + "latere.ai"
	// After a scheme it is a host whatever follows it.
	if strings.Contains(line, "://"+group) {
		return false
	}
	for _, legal := range []string{group + "/", `"` + group + `"`, "api_group: " + group} {
		line = strings.ReplaceAll(line, legal, "")
	}
	for _, example := range []string{".example.org", ".example.com", ".example"} {
		line = strings.ReplaceAll(line, group+example, "")
	}
	return !strings.Contains(line, group)
}

// TestNoLatereCoordinatesInReleasedArtifacts is spec 001's rule as a test:
// no file of the repository names a coordinate of one installation. specs/
// is left to the tests that own it, because a design record names what it was
// written against. The API group is the one legal occurrence of the name, in
// the forms the schema spells it.
func TestNoLatereCoordinatesInReleasedArtifacts(t *testing.T) {
	read := sources(t)
	covered := map[string]bool{}
	for path, body := range read {
		for _, root := range released {
			if strings.HasPrefix(path, root+"/") {
				covered[root] = true
			}
		}
		if needleFiles[path] {
			continue
		}
		for _, c := range coordinates {
			for i, line := range strings.Split(body, "\n") {
				if !strings.Contains(line, c.needle) {
					continue
				}
				if c.needle == "cella."+"latere.ai" && schemaGroup(line) {
					continue
				}
				t.Errorf("%s:%d names %s (%q); this repository is public and a fork inherits its defaults",
					path, i+1, c.what, c.needle)
			}
		}
	}
	for _, root := range released {
		if !covered[root] {
			t.Errorf("the walk read no file under %s, so its coordinates are unchecked", root)
		}
	}
}

// TestSchemaGroupTellsTheGroupFromAHost drives the exemption over planted
// lines: a walk over a clean tree passes whether or not the rule works.
func TestSchemaGroupTellsTheGroupFromAHost(t *testing.T) {
	group := "cella." + "latere.ai"
	for line, want := range map[string]bool{
		`apiVersion: ` + group + `/v1beta1`:             true,
		`labels["` + group + `/owner"] = owner`:         true,
		`ReservedKeyDomain = "` + group + `"`:           true,
		`  api_group: ` + group:                         true,
		`"` + group + `.example.org/note": "x"`:         true,
		`url := "https://` + group + `/v1"`:             false,
		`issuer = "https://` + group + `"`:              false,
		`// the plane at ` + group + ` is one consumer`: false,
		`host: ` + group:                                false,
		`apiVersion: ` + group + `/v1, host: ` + group:  false,
	} {
		if got := schemaGroup(line); got != want {
			t.Errorf("schemaGroup(%q) = %v, want %v", line, got, want)
		}
	}
}

// TestTheReleaseNamesEveryArtifactItBuilds holds the pipeline to the names
// spec 014 fixes: a reader of the release page, and the deploy that pins
// an image, both read these strings and not a pattern.
func TestTheReleaseNamesEveryArtifactItBuilds(t *testing.T) {
	build, err := os.ReadFile(filepath.Join("tools", "release", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{"cellad", "cella"} {
		if !strings.Contains(string(build), binary+`_${tag}_${os}_${arch}.tar.gz`) {
			t.Errorf("build.sh does not write %s_<tag>_<os>_<arch>.tar.gz, which is the archive name the install document names", binary)
		}
		if !strings.Contains(string(build), "./cmd/"+binary) {
			t.Errorf("build.sh builds no ./cmd/%s; the release carries both binaries (spec 011)", binary)
		}
	}
	for _, pair := range [][2]string{{"linux", "darwin"}, {"amd64", "arm64"}} {
		for _, want := range pair {
			if !strings.Contains(string(build), want) {
				t.Errorf("build.sh builds no %s binary; the release covers linux and darwin, amd64 and arm64", want)
			}
		}
	}
	release, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"deploy-${GITHUB_REF_NAME}.tar.gz",
		// The four archives of each binary and the deploy archive: the
		// release-verify job counts them and names them.
		"for binary in cellad cella; do",
		`[ "$(grep -c . checksums.txt)" -eq 9 ]`,
		"cella_${GITHUB_REF_NAME}_linux_amd64.tar.gz",
		"checksums.txt",
		"checksums.txt.sigstore.json",
		"sbom-module.spdx.json",
		"sbom-cellad.spdx.json",
		"cosign sign --yes",
		"cosign verify-blob",
		"cosign verify ",
		"gh attestation verify",
	} {
		if !strings.Contains(string(release), want) {
			t.Errorf("release.yml does not carry %q", want)
		}
	}
}

// TestTheReleaseRunsSpec014sJobsInOrder: the pipeline's shape is a
// criterion of its own, because a job that ran after publish would let an
// unverified release reach the page.
func TestTheReleaseRunsSpec014sJobsInOrder(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err := unmarshalYAML(data, &o); err != nil {
		t.Fatal(err)
	}
	jobs, ok := o["jobs"].(map[string]any)
	if !ok {
		t.Fatal("release.yml declares no jobs")
	}
	want := map[string][]string{
		"gate-green":      nil,
		"build":           {"gate-green"},
		"conformance":     {"build"},
		"publish":         {"build", "conformance"},
		"install-release": {"build", "publish"},
		"release-verify":  {"build", "publish"},
	}
	for name, needs := range want {
		job, ok := jobs[name].(map[string]any)
		if !ok {
			t.Errorf("release.yml has no job %s; spec 014 names it", name)
			continue
		}
		got := needsOf(job["needs"])
		if strings.Join(got, ",") != strings.Join(needs, ",") {
			t.Errorf("%s needs %v, and spec 014's order makes it %v", name, got, needs)
		}
	}
	for name := range jobs {
		if _, ok := want[name]; !ok {
			t.Errorf("release.yml has the job %s, which spec 014 does not name", name)
		}
	}
}

// TestVerifyRendersTheDeployTreeOnEveryPush is spec 014's every-push
// half: the overlays render on a runner that has kubectl, so the Go test
// that skips without it cannot be the one nobody runs.
func TestVerifyRendersTheDeployTreeOnEveryPush(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "verify.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err := unmarshalYAML(data, &o); err != nil {
		t.Fatal(err)
	}
	jobs, _ := o["jobs"].(map[string]any)
	job, ok := jobs["install"].(map[string]any)
	if !ok {
		t.Fatal("verify.yml has no install job; spec 014 walks the deploy tree on every push")
	}
	var steps string
	piped := false
	for _, s := range list(job["steps"]) {
		run := str(dig(s, "run"))
		steps += run + "\n"
		// A step whose command is a pipe needs a shell with pipefail: the
		// default is `bash -e`, `tee` exits 0, and a failing `go test`
		// would be swallowed by the pipe.
		if strings.Contains(run, "| tee ") {
			piped = true
			if str(dig(s, "shell")) != "bash" || !strings.Contains(run, "set -o pipefail") {
				t.Error("a step pipes into tee without `shell: bash` and `set -o pipefail`, so a failing command would pass")
			}
		}
	}
	if !piped {
		t.Error("the install job runs no test whose output is read, so the skip check has nothing to read")
	}
	for _, want := range []string{"kubectl version --client", "--- SKIP"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the install job does not run %q, so a skipped render would pass unnoticed", want)
		}
	}
}

// TestEveryPipedStepFailsOnTheCommandAndNotTheTee is the same rule over the
// release pipeline: a step that reads a command's output through a pipe
// reports the command's exit code and not the reader's.
func TestEveryPipedStepFailsOnTheCommandAndNotTheTee(t *testing.T) {
	for _, name := range []string{"release.yml", "verify.yml", "conformance.yml"} {
		data, err := os.ReadFile(filepath.Join(".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var o object
		if err := unmarshalYAML(data, &o); err != nil {
			t.Fatal(err)
		}
		jobs, _ := o["jobs"].(map[string]any)
		for job, spec := range jobs {
			for _, s := range list(dig(spec, "steps")) {
				run := str(dig(s, "run"))
				if !strings.Contains(run, "| tee ") {
					continue
				}
				if str(dig(s, "shell")) != "bash" || !strings.Contains(run, "set -o pipefail") {
					t.Errorf("%s: %s pipes into tee without `shell: bash` and `set -o pipefail`", name, job)
				}
			}
		}
	}
}

// TestTheExternalRunIsTheDocumentedCommand holds the conformance workflow to
// docs/conformance.md: on dispatch, a clean checkout runs the command the page
// documents against the address given, with the same go test flags, the same
// package and the same suite flags in the same order, the values free. The
// bearer is a repository secret that reaches the step through its
// environment, and no expression is part of the script, so neither a secret
// nor an input is printed with it or becomes a command.
func TestTheExternalRunIsTheDocumentedCommand(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("docs", "conformance.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "## Run it against an installation")
	if !ok {
		t.Fatal("docs/conformance.md has no section that runs the suite against an installation")
	}
	documented := shellCommand(t, "docs/conformance.md", fencedBlock(after, "sh"), "go test")
	for _, flag := range []string{"-count=1", "-timeout"} {
		if !slices.Contains(documented, flag) {
			t.Errorf("the documented command carries no %s, so a run can be replayed or cut short", flag)
		}
	}

	path := filepath.Join(".github", "workflows", "conformance.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err := unmarshalYAML(data, &o); err != nil {
		t.Fatal(err)
	}
	// YAML 1.1 reads the bare key `on` as a boolean, which the JSON the
	// decoder converts through spells "true".
	triggers, _ := o["on"].(map[string]any)
	if triggers == nil {
		triggers, _ = o["true"].(map[string]any)
	}
	if len(triggers) != 1 || triggers["workflow_dispatch"] == nil {
		t.Fatalf("the workflow runs on %v; the external run is on dispatch alone", keysOf(triggers))
	}
	if dig(triggers, "workflow_dispatch", "inputs", "url", "required") != true {
		t.Error("the dispatch does not require the address of the server under test")
	}
	for name := range dig(triggers, "workflow_dispatch", "inputs").(map[string]any) {
		if strings.Contains(strings.ToLower(name), "token") {
			t.Errorf("the dispatch takes %s as an input, which the run's summary shows to every reader", name)
		}
	}
	if str(dig(o, "permissions", "contents")) != "read" {
		t.Error("the workflow does not hold its token to reading the repository")
	}
	job, ok := dig(o, "jobs", "conformance-external").(map[string]any)
	if !ok {
		t.Fatal("the workflow has no conformance-external job")
	}
	var step map[string]any
	for _, s := range list(job["steps"]) {
		m, _ := s.(map[string]any)
		if strings.Contains(str(m["run"]), "TestContract") {
			step = m
		}
		if uses := str(m["uses"]); uses != "" && !regexp.MustCompile(`@[0-9a-f]{40}$`).MatchString(uses) {
			t.Errorf("%s is not pinned by commit", uses)
		}
	}
	if step == nil {
		t.Fatal("no step of the job runs the suite")
	}
	script := str(step["run"])
	if strings.Contains(script, "${{") {
		t.Error("the script carries an expression, so a value is written into the text the runner prints and executes")
	}
	env := envOf(step["env"])
	if !strings.Contains(env["CELLA_TEST_TOKEN"], "secrets.") {
		t.Errorf("the bearer comes from %q and not from a repository secret", env["CELLA_TEST_TOKEN"])
	}
	if !strings.Contains(env["CELLA_TEST_URL"], "inputs.url") {
		t.Errorf("the address comes from %q and not from the dispatch", env["CELLA_TEST_URL"])
	}
	run := shellCommand(t, path, script, "go test")

	// The go test half is equal token for token; the suite's flags are the
	// same flags in the same order, each with its own value.
	split := func(cmd []string) (head, flags []string) {
		i := slices.Index(cmd, "-args")
		if i < 0 {
			t.Fatalf("%v passes the suite no flags", cmd)
		}
		for j := i + 1; j < len(cmd); j += 2 {
			flags = append(flags, cmd[j])
		}
		return cmd[:i+1], flags
	}
	docHead, docFlags := split(documented)
	runHead, runFlags := split(run)
	if !slices.Equal(runHead, docHead) {
		t.Errorf("the job runs %v, the page documents %v", runHead, docHead)
	}
	if !slices.Equal(runFlags, docFlags) {
		t.Errorf("the job gives the suite %v, the page documents %v", runFlags, docFlags)
	}
}

// fencedBlock is the first fenced code block of one language in a text.
func fencedBlock(text, lang string) string {
	_, rest, ok := strings.Cut(text, "```"+lang+"\n")
	if !ok {
		return ""
	}
	block, _, _ := strings.Cut(rest, "```")
	return block
}

// shellCommand reads the one command of a script that begins with prefix, its
// continuation lines joined, split into words, and cut at the first pipe or
// redirection, which is where the command ends and its reader begins.
func shellCommand(t *testing.T, where, script, prefix string) []string {
	t.Helper()
	joined := strings.ReplaceAll(script, "\\\n", " ")
	for line := range strings.SplitSeq(joined, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix+" ") {
			continue
		}
		var words []string
		for word := range strings.FieldsSeq(line) {
			if word == "|" || strings.HasPrefix(word, "2>") || strings.HasPrefix(word, ">") {
				break
			}
			words = append(words, word)
		}
		return words
	}
	t.Fatalf("%s runs no %q command", where, prefix)
	return nil
}

// keysOf is the keys of a map, for a message.
func keysOf(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// needsOf reads a job's dependencies. YAML writes one as a scalar and
// several as a sequence, and both are one list here.
func needsOf(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	var out []string
	for _, n := range list(v) {
		out = append(out, str(n))
	}
	return out
}

// TestRunBlocksRunsAFencedProgram drives the document runner over a
// fixture: the blocks run in order in one shell, so a variable one block
// exports reaches the next, and prose in another language is not run.
func TestRunBlocksRunsAFencedProgram(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "walk.md")
	const body = "# A walk\n\n```sh\nexport GREETING=hello\n```\n\nSome prose.\n\n```yaml\nnot: a command\n```\n\n```sh\necho \"$GREETING from the second block\"\n```\n"
	if err := os.WriteFile(doc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, listing, err := runBlocks(t, doc)
	if err != nil {
		t.Fatalf("the runner failed: %v\n%s%s", err, listing, out)
	}
	if !strings.Contains(out, "hello from the second block") {
		t.Errorf("the blocks did not run in one shell:\n%s", out)
	}
	if strings.Contains(listing, "not: a command") {
		t.Errorf("a block that is not sh was extracted:\n%s", listing)
	}
	// A failing command ends the walk, which is what makes the document a
	// build rather than a suggestion.
	failing := filepath.Join(dir, "broken.md")
	if err := os.WriteFile(failing, []byte("```sh\nfalse\necho unreachable\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runBlocks(t, failing)
	if err == nil {
		t.Errorf("a failing command did not end the walk:\n%s", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("the walk continued past a failure:\n%s", out)
	}
	// A document with no block is a document nobody can walk.
	empty := filepath.Join(dir, "prose.md")
	if err := os.WriteFile(empty, []byte("# Prose\n\nNothing to run.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, err := runBlocks(t, empty); err == nil {
		t.Errorf("a document with no block walked green:\n%s", out)
	}
}

// TestTheInstallDocumentIsAProgram: every command a reader would type is
// in a block the runner executes, and the document ends on the check.
func TestTheInstallDocumentIsAProgram(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if n := strings.Count(body, "```sh\n"); n < 8 {
		t.Errorf("the install document holds %d runnable block(s); the walk is the document", n)
	}
	for _, want := range []string{
		"cellad_<tag>_<os>_<arch>.tar.gz",
		"deploy-<tag>.tar.gz",
		"ghcr.io/<owner>/cellad:<tag>",
		"cellad check",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the install document does not name %q", want)
		}
	}
	check := strings.LastIndex(body, "cellad check")
	first := strings.Index(body, "```sh\n")
	if check < first {
		t.Error("the install document does not end with cellad check")
	}
}

// runBlocks runs the document runner over a path and returns what the
// document's commands wrote and what the runner itself wrote, which are
// two streams: the runner prints the program it extracted to stderr, so a
// failed job shows what it executed, and reading the two together would
// find a command in its own listing.
func runBlocks(t *testing.T, doc string) (stdout, stderr string, err error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("tools", "docs", "run-blocks.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/bash", script, doc)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// TestReleasePublishesTheDisplayImage: the desktop image of spec 014 takes
// the control plane's path. It is built for both architectures and pushed
// by digest under no tag, signed, given an attested bill of materials and
// provenance, tagged only in publish, its bill of materials attached, and
// verified by tag from the clean runner.
func TestReleasePublishesTheDisplayImage(t *testing.T) {
	path := filepath.Join(".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err := unmarshalYAML(data, &o); err != nil {
		t.Fatal(err)
	}
	if got := str(dig(o, "env", "DISPLAY_IMAGE")); got != "cella-display" {
		t.Fatalf("the workflow's DISPLAY_IMAGE is %q, want cella-display", got)
	}
	var build, attested []string
	for _, s := range list(dig(o, "jobs", "build", "steps")) {
		if str(dig(s, "id")) == "display" {
			build = append(build, str(dig(s, "run")))
		}
		if strings.HasSuffix(str(dig(s, "with", "subject-name")), "/cella-display") {
			attested = append(attested, str(dig(s, "uses")))
		}
	}
	if len(build) != 1 {
		t.Fatalf("the build job has %d display steps, want one", len(build))
	}
	for _, want := range []string{
		"-f images/display/Dockerfile",
		"--platform linux/amd64,linux/arm64",
		"push-by-digest=true",
		`echo "digest=${digest}" >> "$GITHUB_OUTPUT"`,
	} {
		if !strings.Contains(build[0], want) {
			t.Errorf("the display build does not carry %q", want)
		}
	}
	if strings.Contains(build[0], "GITHUB_REF_NAME") {
		t.Error("the display build names the tag; the digest is tagged only in publish")
	}
	for _, action := range []string{"actions/attest-sbom@", "actions/attest-build-provenance@"} {
		if !slices.ContainsFunc(attested, func(uses string) bool { return strings.HasPrefix(uses, action) }) {
			t.Errorf("the display image is not attested by %s", strings.TrimSuffix(action, "@"))
		}
	}
	if got := str(dig(o, "jobs", "build", "outputs", "display")); got != "${{ steps.display.outputs.digest }}" {
		t.Errorf("the build job's display output is %q", got)
	}
	for job, wants := range map[string][]string{
		"build": {
			`cosign sign --yes "${REGISTRY}/${OWNER}/${DISPLAY_IMAGE}@${{ steps.display.outputs.digest }}"`,
		},
		"publish": {
			`-t "${REGISTRY}/${OWNER}/${DISPLAY_IMAGE}:${GITHUB_REF_NAME}"`,
			`"${REGISTRY}/${OWNER}/${DISPLAY_IMAGE}@${{ needs.build.outputs.display }}"`,
		},
		"release-verify": {
			`for image in "${IMAGE}" "${DISPLAY_IMAGE}"; do`,
			"cosign verify ",
			"gh attestation verify",
			"sbom-cella-display.spdx.json",
		},
	} {
		steps := jobSteps(t, path, job)
		for _, want := range wants {
			if !strings.Contains(steps, want) {
				t.Errorf("the %s job does not carry %q", job, want)
			}
		}
	}
	if !strings.Contains(string(data), "output-file: dist/sbom-cella-display.spdx.json") {
		t.Error("the display image's bill of materials is not written into dist, so it is not a release asset")
	}
}
