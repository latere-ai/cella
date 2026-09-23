// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestKindRunsDeclareDial: the Kubernetes driver declares Dial, so both
// conformance runs against the kind stack declare it and hold the server to
// the dial route, both kind tiers require the dial through the deployed
// control plane to pass, and the install job runs the driver's own dial and
// port cases against the cluster. A declaration left out would have the suite
// hold the server to refusing a route it serves.
func TestKindRunsDeclareDial(t *testing.T) {
	for _, run := range []struct{ file, job string }{
		{"verify.yml", "install"},
		{"release.yml", "conformance"},
	} {
		steps := jobSteps(t, filepath.Join(".github", "workflows", run.file), run.job)
		declared, ok := flagValue(steps, "-capabilities")
		if !ok {
			t.Errorf("the %s job of %s runs no conformance suite with -capabilities", run.job, run.file)
		} else if !slices.Contains(strings.Split(declared, ","), "dial") {
			t.Errorf("the %s job of %s declares %q to the suite, without dial", run.job, run.file, declared)
		}
		if !strings.Contains(steps, "--- PASS: TestClusterDial") {
			t.Errorf("the %s job of %s does not require TestClusterDial to pass", run.job, run.file)
		}
	}
	install := jobSteps(t, filepath.Join(".github", "workflows", "verify.yml"), "install")
	for _, want := range []string{
		"CELLA_TEST_KUBECONFIG=",
		"CELLA_TEST_NAMESPACE=cella-driver",
		"-run '^TestClusterConformance$/^(DialReachesAPort|PortsReportListening)$' ./runtime/k8s",
		"--- PASS: TestClusterConformance/DialReachesAPort",
		"--- PASS: TestClusterConformance/PortsReportListening",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("the install job does not run the driver's dial and port cases against the cluster (%q)", want)
		}
	}
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
