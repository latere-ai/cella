// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"slices"
	"strings"
	"testing"
)

// TestAWrongServerIsReportedFailed: a server that answers a refusal with a
// sentence outside the table fails the case that provoked it, and the report
// carries the request and the answer that disagreed.
func TestAWrongServerIsReportedFailed(t *testing.T) {
	f := newFake(t)
	f.wrongMessage = true
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Failed, "case008NameTaken") {
		t.Fatalf("the wrong sentence did not fail the case; failed: %v", report.Failed)
	}
	if report.OK() {
		t.Error("a report with a failed case is green")
	}
	var found *Result
	for i, res := range report.Results {
		if res.Name == "case008NameTaken" {
			found = &report.Results[i]
		}
	}
	var d *Disagreement
	if found == nil || !asDisagreement(found.Err, &d) {
		t.Fatalf("the failure carries no disagreement: %+v", found)
	}
	for _, want := range []string{"POST", "/v1/sandboxes", "message", "Something went wrong."} {
		if !strings.Contains(d.Error(), want) {
			t.Errorf("the disagreement does not name %q:\n%s", want, d.Error())
		}
	}
	if !strings.Contains(report.String(), "fail case008NameTaken"[:4]) {
		t.Error("the report does not print the failure")
	}
}

// TestAServerThatAgreesPasses: the same fake, answering the table, passes the
// cases it serves and fails none of them, so a failure above is the wrong
// answer and not the suite.
func TestAServerThatAgreesPasses(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case008NameTaken", "case003DefaultsAreReturned", "case006Unauthenticated"} {
		if !slices.Contains(report.Passed, name) {
			t.Errorf("%s did not pass against a server that answers it", name)
		}
	}
}

// TestGroupsAndSkips: every group of design 015 exists, and a case whose
// capability or input the configuration does not carry is reported skipped
// with what is missing named, never silently and never as a pass.
func TestGroupsAndSkips(t *testing.T) {
	want := []string{
		"decode", "resolve", "lifecycle", "identity", "list", "streams", "errors",
		"events", "secrets", "volumes", "sets", "environments", "egress", "spawn",
		"agent", "computer use", "indistinguishability", "capability",
	}
	var got []string
	for _, group := range Groups() {
		got = append(got, group.Name)
		if len(group.Cases) == 0 {
			t.Errorf("the %s group holds no case", group.Name)
		}
		for _, c := range group.Cases {
			if c.Group != group.Name {
				t.Errorf("%s is filed under %s and listed in %s", c.Name, c.Group, group.Name)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the groups are %v, want %v", got, want)
	}
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	skips := map[string]string{
		"case008Dial":                  "dial",
		"case019VolumeLifecycle":       "volumes",
		"case020SetRunsToCompletion":   "QueuedEnvironment",
		"case021EnvironmentRead":       "Admin",
		"case011AgentScenario":         "Cella",
		"case023BrowserReady":          "DisplayImage",
		"case001Indistinguishable":     "WorkerEnvironment",
		"case006AuthorizerUnavailable": "AuthorizerControl",
		"case007AdmissionRefused":      "AdmissionControl",
		"case009DeliveredInOrder":      "SinkControl",
	}
	for name, input := range skips {
		if !slices.Contains(report.Skipped, name) {
			t.Errorf("%s did not skip where %s is empty", name, input)
			continue
		}
		if reason := report.Reasons[name]; !strings.Contains(reason, input) {
			t.Errorf("%s skipped with %q, which does not name %s", name, reason, input)
		}
	}
	// A case skipped by request names that, so a report says why a case did
	// not run whatever the reason was.
	cfg := f.config()
	cfg.Skip = []string{"decode", "case005StopAndStart"}
	report, err = Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case003MultiDocument", "case005StopAndStart"} {
		if !strings.Contains(report.Reasons[name], "by request") {
			t.Errorf("%s was not skipped by request: %q", name, report.Reasons[name])
		}
	}
}

// TestADeclaredGapIsNotAFailure: a server may declare the cases it fails,
// and the run stays green while the report names each one. A declaration
// that no longer describes a gap fails the run instead, so it cannot outlive
// what it describes.
func TestADeclaredGapIsNotAFailure(t *testing.T) {
	f := newFake(t)
	f.wrongMessage = true
	cfg := f.config()
	cfg.Known = map[string]string{"case008NameTaken": "the fake answers its own sentence"}
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Known, "case008NameTaken") {
		t.Fatalf("the declared case is not in the report's declarations: %v", report.Known)
	}
	if slices.Contains(report.Failed, "case008NameTaken") {
		t.Error("a declared gap is counted as a failure")
	}
	if report.Reasons["case008NameTaken"] == "" {
		t.Error("the report does not carry the reason the gap was declared with")
	}
	f.wrongMessage = false
	report, err = Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Undeclared, "case008NameTaken") {
		t.Fatal("a declared case that passed is not reported")
	}
	if report.OK() {
		t.Error("a declaration that has outlived its gap leaves the run green")
	}
	if !strings.Contains(report.String(), "declared as failing but passed") {
		t.Error("the report does not print the declaration that outlived its gap")
	}
}

// TestRunCleansUp: a run deletes every object it made and nothing else, and
// two runs against one server carry different prefixes, so neither reaches
// the other's objects.
func TestRunCleansUp(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) == 0 {
		t.Fatal("the run created nothing, so there is no cleanup to prove")
	}
	f.mu.Lock()
	left, deleted := len(f.objects), slices.Clone(f.deleted)
	f.mu.Unlock()
	if left != 0 {
		f.mu.Lock()
		names := keys(f.objects)
		f.mu.Unlock()
		t.Errorf("%d object(s) outlived the run: %v; created %v; deleted %v", left, names, report.Created, deleted)
	}
	for _, id := range report.Created {
		if !slices.Contains(deleted, id) {
			t.Errorf("%s was created and not deleted", id)
		}
	}
	for _, id := range deleted {
		if !slices.Contains(report.Created, id) {
			t.Errorf("%s was deleted and not created by this run", id)
		}
	}
	first, err := newEnv(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	second, err := newEnv(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if first.name() == second.name() {
		t.Error("two runs name their objects alike, so one would reach the other's")
	}
}

// TestTheReportCarriesTheMarker: a report names the version the server
// reports and the suite's own, which is what makes two reports comparable.
func TestTheReportCarriesTheMarker(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if report.Marker.Server != "fake" || report.Marker.Suite != Version {
		t.Errorf("the marker is %+v, want the server's version and %s", report.Marker, Version)
	}
	if !strings.Contains(report.String(), "conformance suite "+Version+" against server fake") {
		t.Errorf("the report does not open with the marker:\n%s", report.String())
	}
}

// TestASuiteWithNoServerRefusesToRun: a configuration that names no server
// or no caller is a usage error and never an empty green report.
func TestASuiteWithNoServerRefusesToRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no URL", Config{Caller: "t"}, "no server"},
		{"no scheme", Config{URL: "cella.example.com", Caller: "t"}, "no http"},
		{"no caller", Config{URL: "http://127.0.0.1:1"}, "no caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Execute(t.Context(), tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the suite answered %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestSubtestNames: a case runs as <NNN>/<Name>, which is what a Go test run
// and the report agree on.
func TestSubtestNames(t *testing.T) {
	for name, want := range map[string]string{
		"case008ExecWait":   "008/ExecWait",
		"case001Indistinct": "001/Indistinct",
		"notACase":          "notACase",
		"case01":            "case01",
	} {
		if got := subtestName(name); got != want {
			t.Errorf("%s runs as %s, want %s", name, got, want)
		}
	}
}

// asDisagreement is errors.As without the import in every assertion.
func asDisagreement(err error, into **Disagreement) bool {
	d, ok := err.(*Disagreement)
	if ok {
		*into = d
	}
	return ok
}
