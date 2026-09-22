// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTheArchiveHelpers: one file in and out, and an archive that holds
// neither the entry a case asked for nor a readable header.
func TestTheArchiveHelpers(t *testing.T) {
	archive := tarOf("suite.txt", "the bytes")
	got, err := tarEntry(archive, "suite.txt")
	if err != nil || got != "the bytes" {
		t.Fatalf("the entry is %q, %v", got, err)
	}
	if _, err := tarEntry(archive, "other.txt"); err == nil || !strings.Contains(err.Error(), "no entry named") {
		t.Errorf("an archive without the entry was read as one with it: %v", err)
	}
	if _, err := tarEntry([]byte("this is no archive"), "suite.txt"); err == nil {
		t.Error("a body that is no archive was read as one")
	}
}

// TestTheBodyHelpers: the three functions that build and compare bodies, each
// over a value they cannot read.
func TestTheBodyHelpers(t *testing.T) {
	body := replaceField([]byte(`{"kind":"Sandbox"}`), "kind", "Widget")
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil || decoded["kind"] != "Widget" {
		t.Errorf("the field was not replaced: %s", body)
	}
	if got := replaceField([]byte("not json"), "kind", "Widget"); string(got) != "not json" {
		t.Errorf("a body that does not decode was rewritten: %s", got)
	}
	if got := canonical(json.RawMessage(`{"b":1,"a":2}`)); string(got) != `{"a":2,"b":1}` {
		t.Errorf("the canonical form is %s", got)
	}
	if got := canonical(json.RawMessage("not json")); string(got) != "not json" {
		t.Errorf("a body that does not decode was rewritten: %s", got)
	}
	if !jsonEqual([]byte(`{"a":1,"env":"x"}`), []byte(`{"a":1,"env":"y"}`), "env") {
		t.Error("two objects that differ only in the member named are not equal")
	}
	if jsonEqual([]byte(`{"a":1}`), []byte(`{"a":2}`)) {
		t.Error("two objects that differ are equal")
	}
	if !jsonEqual([]byte("same"), []byte("same")) {
		t.Error("two bodies that do not decode are compared as bytes")
	}
}

// TestTheLiteralDefaultsAreReadOffTheAnswer: each literal default is read at
// its path, a zero one may be left out, and an answer that moves one, leaves
// out one that is not zero, or is no object at all is a disagreement naming
// what arrived.
func TestTheLiteralDefaultsAreReadOffTheAnswer(t *testing.T) {
	honest := `{"workdir":"/workspace","workspace":{"path":"/workspace","source":"empty"},"network":{"egress":{"mode":"open"}}}`
	if err := holdsLiteralDefaults(json.RawMessage(honest)); err != nil {
		t.Fatalf("an answer with every literal default: %v", err)
	}
	zeros := `{"workdir":"/workspace","workspace":{"path":"/workspace","source":"empty"},"network":{"egress":{"mode":"open"}},"mesh":{"enabled":false,"spawn":{"budget":0,"depth":0}}}`
	if err := holdsLiteralDefaults(json.RawMessage(zeros)); err != nil {
		t.Fatalf("an answer that writes the zero defaults out: %v", err)
	}
	for _, tc := range []struct{ spec, want string }{
		{`{"workdir":"/workspace","workspace":{"source":"empty"},"network":{"egress":{"mode":"open"}}}`, "spec.workspace.path absent"},
		{`{"workdir":"/","workspace":{"path":"/workspace","source":"empty"},"network":{"egress":{"mode":"open"}}}`, "spec.workdir /"},
		{`{"workdir":"/workspace","workspace":{"path":"/workspace","source":"empty"},"network":{"egress":{"mode":"open"}},"mesh":{"spawn":{"depth":1}}}`, "spec.mesh.spawn.depth 1"},
		{`{"workdir":"/workspace","workspace":"/workspace"}`, "spec.workspace.path absent"},
		{`["no", "object"]`, "a specification that is a JSON object"},
	} {
		err := holdsLiteralDefaults(json.RawMessage(tc.spec))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want a disagreement carrying %q", tc.spec, err, tc.want)
		}
	}
}

// TestTheListHelpers: the page readers a list case builds its verdict from.
func TestTheListHelpers(t *testing.T) {
	page := listEnvelope{Items: []object{{Status: objectStatus{ID: "sbx_1"}}, {Status: objectStatus{ID: "sbx_2"}}}}
	if got := idsOf(page); !slices.Equal(got, []string{"sbx_1", "sbx_2"}) {
		t.Errorf("the ids are %v", got)
	}
	if first(nil) != "" || first([]string{"a", "b"}) != "a" {
		t.Error("the first selector value is not read")
	}
}

// TestAwaitReportsWhatItWaitedFor: a phase that never arrives is a
// disagreement naming the phase the case wanted and the one it saw, and a
// sandbox that failed on the way is not waited for to the deadline.
func TestAwaitReportsWhatItWaitedFor(t *testing.T) {
	f := newFake(t)
	cfg := f.config()
	cfg.Timeout = 300 * time.Millisecond
	e, err := newEnv(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := e.create(t.Context(), e.caller, e.manifest(e.name()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := e.await(ctx, e.caller, obj.Status.ID, "Stopped"); err == nil ||
		!strings.Contains(err.Error(), "Stopped") {
		t.Errorf("waiting for a phase that never arrives answered %v", err)
	}
	f.mu.Lock()
	status(f.objects[obj.Status.ID])["phase"] = "Failed"
	f.mu.Unlock()
	if _, err := e.await(t.Context(), e.caller, obj.Status.ID, "Running"); err == nil ||
		!strings.Contains(err.Error(), "Failed") {
		t.Errorf("a sandbox that failed was waited for: %v", err)
	}
	if _, err := e.await(t.Context(), e.caller, missingID, "Running"); err == nil {
		t.Error("an object that is not there was waited for")
	}
}

// TestRunMirrorsTheReportOntoSubtests: the wrapper design 015 fixes, which
// is what a Go test run reads. The cases the fake does not serve are declared
// from a first run, so the second one takes every arm of the mirror, a pass,
// a skip and a declared gap, and reports no failure of its own.
func TestRunMirrorsTheReportOntoSubtests(t *testing.T) {
	f := newFake(t)
	cfg := f.config()
	first, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Failed) == 0 {
		t.Fatal("the fake serves every case, so the declaration arm cannot be taken")
	}
	cfg.Known = map[string]string{}
	for _, name := range first.Failed {
		cfg.Known[name] = "the fake does not serve this route"
	}
	cfg.Skip = []string{"case005StopAndStart"}
	report := Run(t, cfg)
	if len(report.Results) != len(Names()) {
		t.Errorf("the report holds %d results for %d cases", len(report.Results), len(Names()))
	}
	if !slices.Contains(report.Skipped, "case005StopAndStart") {
		t.Error("the case skipped by request is not in the report")
	}
	if len(report.Known) == 0 {
		t.Error("no declared gap was reported, so the declaration arm was never taken")
	}
	if len(report.Passed) == 0 {
		t.Error("no case passed, so the pass arm was never taken")
	}
	if !report.OK() {
		t.Errorf("the run is not green: %v failed, %v declared and passed", report.Failed, report.Undeclared)
	}
}

// TestMinterTakesATokenPerSubject: the function a tier hands the suite as
// its Token, over an issuer that mints, one that refuses, one that answers
// something else, and one that is not there.
func TestMinterTakesATokenPerSubject(t *testing.T) {
	f := newFake(t)
	mint := Minter(f.server.URL + "/")
	token, err := mint(t.Context(), "alice")
	if err != nil || token != "fake-token-alice" {
		t.Fatalf("the issuer minted %q, %v", token, err)
	}
	for mode, want := range map[string]string{
		"status": "answered 503",
		"body":   "no token",
		"empty":  "minted nothing",
	} {
		f.mintFailure = mode
		if _, err := mint(t.Context(), "alice"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("an issuer in mode %s answered %v, want an error naming %q", mode, err, want)
		}
	}
	f.server.Close()
	if _, err := mint(t.Context(), "alice"); err == nil || !strings.Contains(err.Error(), "minting a token") {
		t.Errorf("an issuer that is not there answered %v", err)
	}
}

// TestAServerWithNoSecretKeySkipsTheSecrets: an installation that holds no
// key to seal a value under says so, and the group skips with the server's
// own sentence rather than failing.
func TestAServerWithNoSecretKeySkipsTheSecrets(t *testing.T) {
	f := newFake(t)
	f.noSecretKey = true
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case018SecretWriteOnly", "case018SecretRotates", "case018SecretDelete"} {
		if !slices.Contains(report.Skipped, name) {
			t.Errorf("%s did not skip against a server that stores no secret value", name)
		}
		if reason := report.Reasons[name]; !strings.Contains(reason, "stores no secret value") {
			t.Errorf("%s skipped with %q", name, reason)
		}
	}
}

// TestTheReportPrintsEveryBucket: the report a person reads names each case
// with what happened to it, and the counts at the end.
func TestTheReportPrintsEveryBucket(t *testing.T) {
	report := Report{
		Marker: Marker{Server: "srv", Suite: Version},
		Results: []Result{
			{Group: "decode", Name: "case008ExecWait", Status: StatusPassed, Elapsed: time.Millisecond},
			{Group: "decode", Name: "case008Logs", Status: StatusSkipped, Reason: "no capability"},
			{Group: "streams", Name: "case008FilesTar", Status: StatusFailed,
				Err: &Disagreement{Method: "GET", Path: "/v1", Want: "a", Got: "b"}},
			{Group: "streams", Name: "case008Dial", Status: StatusKnown, Reason: "not built",
				Err: &Disagreement{Method: "GET", Path: "/v1", Want: "a", Got: "b"}},
		},
		Passed: []string{"case008ExecWait"}, Failed: []string{"case008FilesTar"},
		Skipped: []string{"case008Logs"}, Known: []string{"case008Dial"},
		Reasons: map[string]string{}, Created: []string{"sbx_1"},
	}
	text := report.String()
	for _, want := range []string{
		"conformance suite " + Version + " against server srv", "decode", "streams",
		"008/ExecWait", "008/Logs", "no capability", "008/FilesTar", "want: a",
		"1 passed, 1 failed, 1 skipped, 1 declared gap(s), 1 object(s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not carry %q:\n%s", want, text)
		}
	}
	if report.OK() {
		t.Error("a report with a failed case is green")
	}
	empty := Report{Marker: Marker{Suite: Version}}
	if !strings.Contains(empty.String(), "against server unknown") {
		t.Errorf("a report from a server that names no version reads:\n%s", empty.String())
	}
	if !empty.OK() {
		t.Error("a report with no failure is not green")
	}
}

// TestGroupsAndNamesAgree: every case is listed once, under one group, and
// the names the marker rule reads are the names the groups hold.
func TestGroupsAndNamesAgree(t *testing.T) {
	names := Names()
	if !slices.IsSorted(names) {
		t.Error("the names are not sorted, so two reports order them differently")
	}
	seen := map[string]bool{}
	count := 0
	for _, group := range Groups() {
		for _, c := range group.Cases {
			if seen[c.Name] {
				t.Errorf("%s is listed twice", c.Name)
			}
			seen[c.Name] = true
			count++
			if c.Run == nil {
				t.Errorf("%s has no function", c.Name)
			}
		}
	}
	if count != len(names) {
		t.Errorf("the groups hold %d cases and Names reports %d", count, len(names))
	}
}
