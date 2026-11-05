// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestKindRunsDeclareDial: the Kubernetes driver declares Dial, so both
// conformance runs against the kind stack declare it and hold the server to
// the dial route, both kind tiers require the dial through the deployed
// control plane to pass, and the kind suite runs the driver's own dial and
// port cases against the cluster. A declaration left out would have the suite
// hold the server to refusing a route it serves.
func TestKindRunsDeclareDial(t *testing.T) {
	for _, run := range kindRuns() {
		steps := run.steps(t)
		declared, ok := flagValue(steps, "-capabilities")
		if !ok {
			t.Errorf("%s runs no conformance suite with -capabilities", run.name)
		} else if !slices.Contains(strings.Split(declared, ","), "dial") {
			t.Errorf("%s declares %q to the suite, without dial", run.name, declared)
		}
		if !run.requiresPass(steps, "TestClusterDial") {
			t.Errorf("%s does not require TestClusterDial to pass", run.name)
		}
	}
	script := readKindScript(t)
	for _, want := range []string{
		"CELLA_TEST_KUBECONFIG=",
		"CELLA_TEST_NAMESPACE=cella-driver",
		"-run '^TestClusterConformance$/^(DialReachesAPort|PortsReportListening)$' ./runtime/k8s",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the kind script does not run the driver's dial and port cases against the cluster (%q)", want)
		}
	}
	for _, name := range []string{"DialReachesAPort", "PortsReportListening"} {
		if !strings.Contains(script, "passed driver-kind TestClusterConformance/"+name) {
			t.Errorf("the kind script does not require TestClusterConformance/%s to pass", name)
		}
	}
}

// kindRun is one run of the conformance suite against the kind stack: the
// kind script the shared gate's kind-stack suite runs on every push, and the
// conformance job of the release pipeline.
type kindRun struct {
	name string
	// file is the workflow whose job runs the suite; an empty job means file
	// is the script itself.
	file, job string
	// log names where the run keeps the output of its kind tier, which its
	// pass checks read.
	log string
}

// kindRuns lists the runs that bring the kind stack up.
func kindRuns() []kindRun {
	return []kindRun{
		{name: "tools/kind/e2e.sh", file: filepath.Join("tools", "kind", "e2e.sh"), log: "kind"},
		{name: "the conformance job of release.yml", file: filepath.Join(".github", "workflows", "release.yml"), job: "conformance", log: "/tmp/kind.log"},
	}
}

// steps is the text of the run: the script, or the commands and environment
// of the job's steps.
func (r kindRun) steps(t *testing.T) string {
	t.Helper()
	if r.job == "" {
		data, err := os.ReadFile(r.file)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	return jobSteps(t, r.file, r.job)
}

// requiresPass reports whether the run fails unless the named test passed:
// the script does so through its passed function, the job through a grep of
// the log it tees the test's output to.
func (r kindRun) requiresPass(steps, test string) bool {
	return strings.Contains(steps, "passed "+r.log+" "+test) ||
		strings.Contains(steps, "--- PASS: "+test+"' "+r.log)
}

// readKindScript reads the script of the kind-stack suite.
func readKindScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("tools", "kind", "e2e.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// flagValue is the word after the first occurrence of a flag.
func flagValue(script, flag string) (string, bool) {
	_, after, ok := strings.Cut(script, flag+" ")
	if !ok {
		return "", false
	}
	fields := strings.Fields(after)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}
