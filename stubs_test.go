// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// initContainers returns the init containers of a Deployment's or a Job's
// pod template, which is where a sidecar of Kubernetes 1.29 and later
// lives.
func initContainers(o object) []any {
	return list(dig(o, "spec", "template", "spec", "initContainers"))
}

// TestTheStubsRunBesideTheControlPlane is the shape of the kind stack: the
// stubs are a container of the control plane's own Pod, in the Deployment
// and in the check Job alike, so every endpoint is loopback and the rule of
// specs 006 and 007 holds with nothing waived.
func TestTheStubsRunBesideTheControlPlane(t *testing.T) {
	objects := render(t, "deploy/examples/kind-stubs")
	for _, o := range []object{
		find(t, objects, "Deployment", "cellad"),
		find(t, objects, "Job", "cellad-check"),
	} {
		t.Run(o.kind(), func(t *testing.T) {
			var stubs any
			for _, c := range initContainers(o) {
				if str(dig(c, "name")) == "stubs" {
					stubs = c
				}
			}
			if stubs == nil {
				t.Fatalf("the %s runs no stubs beside cellad, so it has no issuer to verify against", o.kind())
			}
			// An init container is a sidecar only with this policy: without
			// it the container runs to completion before cellad starts, and
			// a stub never completes.
			if got := str(dig(stubs, "restartPolicy")); got != "Always" {
				t.Errorf("the stubs container's restartPolicy is %q; a sidecar's is Always", got)
			}
			// Starting is not listening, and `cellad serve` reads the
			// discovery document at start, so the probe is what it waits on.
			if got := str(dig(stubs, "startupProbe", "httpGet", "path")); got != "/.well-known/openid-configuration" {
				t.Errorf("the stubs container's startup probe reads %q; the control plane waits on the discovery document", got)
			}
			if dig(stubs, "securityContext", "readOnlyRootFilesystem") != true {
				t.Error("the stubs container's root filesystem is writable")
			}
			if dig(stubs, "securityContext", "allowPrivilegeEscalation") != false {
				t.Error("the stubs container permits privilege escalation")
			}
		})
	}
	// Every endpoint the control plane is pointed at is loopback, which is
	// what running the stubs in the Pod is for.
	for _, o := range objects {
		for key, want := range map[string]string{
			"CELLA_OIDC_ISSUERS": "http://127.0.0.1:9080", "CELLA_AUTHORIZER_URL": "http://127.0.0.1:9081",
			"CELLA_EVENTS_URL": "http://127.0.0.1:9083",
		} {
			for _, field := range []string{"data", "stringData"} {
				got, ok := dig(o, field, key).(string)
				if ok && got != want {
					t.Errorf("%s names %s as %q, and the endpoint is in this Pod at %q", o.name(), key, got, want)
				}
			}
		}
	}
	// A cluster that enforces policy would drop what the base does not
	// admit, and the base admits the public listener alone.
	policy := find(t, objects, "NetworkPolicy", "cellad")
	var admitted []string
	for _, rule := range list(dig(policy, "spec", "ingress")) {
		for _, p := range list(dig(rule, "ports")) {
			admitted = append(admitted, fmt.Sprint(dig(p, "port")))
		}
	}
	for _, port := range []string{"8080", "9080", "9083"} {
		if !slices.Contains(admitted, port) {
			t.Errorf("the policy admits %v and the host reaches the control plane on 8080, the mint route on 9080 and the sink's feed on 9083", admitted)
		}
	}
	// The host reaches the issuer's mint route and the sink's feed, and
	// nothing else of the stubs.
	service := find(t, objects, "Service", "cella-stubs")
	var ports []string
	for _, p := range list(dig(service, "spec", "ports")) {
		ports = append(ports, str(dig(p, "name")))
	}
	if strings.Join(ports, ",") != "issuer,sink" {
		t.Errorf("the stubs service publishes %v; the host mints a token and reads the records", ports)
	}
}

// TestTheStubsImageIsNoInstallation: `cella-stubs` is a test image. The
// base an operator installs from and the two overlays that are not the test
// stack never name it, and the image says so about itself.
func TestTheStubsImageIsNoInstallation(t *testing.T) {
	for _, dir := range []string{"deploy/base", "deploy/examples/kind", "deploy/examples/generic"} {
		t.Run(dir, func(t *testing.T) {
			for _, o := range render(t, dir) {
				for _, c := range append(containers(o), initContainers(o)...) {
					if image := str(dig(c, "image")); strings.Contains(image, "cella-stubs") {
						t.Errorf("%s runs %s, and a stub is never part of an installation", o.name(), image)
					}
				}
			}
		})
	}
	stubs, err := os.ReadFile("Dockerfile.stubs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"test-only", "./cmd/cella-stubs", "nonroot"} {
		if !strings.Contains(string(stubs), want) {
			t.Errorf("Dockerfile.stubs does not carry %q", want)
		}
	}
}

// TestMakeRunNeedsNoIssuerOfYourOwn: the target is the bootstrap script's
// name, and neither of them asks the developer for an issuer, which is the
// whole point of the stubs.
func TestMakeRunNeedsNoIssuerOfYourOwn(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("tools", "run", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "tools/run/up.sh") {
		t.Error("the run target does not call the bootstrap, so the two could say different things")
	}
	for name, body := range map[string]string{"the Makefile": string(makefile), "the bootstrap": string(script)} {
		if strings.Contains(body, "CELLA_OIDC_ISSUERS=<") || strings.Contains(body, "needs CELLA_OIDC_ISSUERS") {
			t.Errorf("%s still asks for an issuer of your own", name)
		}
	}
	// The bootstrap wires the three endpoints the control plane dials, and
	// not the admission endpoint, whose variables no binary here reads.
	for _, want := range []string{"CELLA_OIDC_ISSUERS=", "CELLA_AUTHORIZER_URL=", "CELLA_EVENTS_URL=", "/mint"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("the bootstrap does not wire %s", want)
		}
	}
	if strings.Contains(string(script), "CELLA_ADMISSION_URL=") {
		t.Error("the bootstrap exports an admission variable, and nothing in this tree reads one")
	}
	for _, target := range []string{"test:", "tier-podman:", "tier-kind:"} {
		if !strings.Contains(string(makefile), target) {
			t.Errorf("the Makefile has no %s target, and spec 012 names the tier", strings.TrimSuffix(target, ":"))
		}
	}
}

// TestTheInstallJobWalksTheDocument is the every-push half of spec 014,
// which slice 048 left gated: the walk runs against a kind cluster with the
// stubs, and the kind tier runs against the cluster it left standing.
func TestTheInstallJobWalksTheDocument(t *testing.T) {
	steps := jobSteps(t, filepath.Join(".github", "workflows", "verify.yml"), "install")
	for _, want := range []string{
		"docs/install.md",
		"deploy/examples/kind-stubs/up.sh",
		"Dockerfile.stubs",
		"TestClusterLifecycle",
		"TestRunBootstrap",
		// The contract of spec 015, against both stacks this job brings up.
		"TestRunConformance",
		"TestContract",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("the install job does not run %q", want)
		}
	}
	// One cluster per run: the stack's own script creates it and the walk
	// and the tier share it.
	if got := strings.Count(steps, "kind create cluster"); got != 0 {
		t.Errorf("the install job creates a cluster in %d step(s) of its own; up.sh creates the one cluster", got)
	}
}

// TestTheReleaseRunsTheStubsAndTheStack: the pipeline publishes the stubs
// image spec 014 names and runs the kind stack in the two jobs that waited
// on it, with no repository variable in the way.
func TestTheReleaseRunsTheStubsAndTheStack(t *testing.T) {
	release, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Dockerfile.stubs", "cella-stubs:${GITHUB_REF_NAME}"} {
		if !strings.Contains(string(release), want) {
			t.Errorf("release.yml does not build and push the stubs image (%q)", want)
		}
	}
	for job, wants := range map[string][]string{
		"conformance":     {"deploy/examples/kind-stubs/up.sh", "TestClusterLifecycle", "TestContract"},
		"install-release": {"deploy/examples/kind-stubs/up.sh", "run-blocks.sh", "docs/install.md"},
	} {
		steps := jobSteps(t, filepath.Join(".github", "workflows", "release.yml"), job)
		for _, want := range wants {
			if !strings.Contains(steps, want) {
				t.Errorf("the %s job does not run %q", job, want)
			}
		}
	}
	if strings.Contains(string(release), "RELEASE_INSTALL_KIND") || strings.Contains(string(release), "RELEASE_INSTALL_ISSUER") {
		t.Error("the walk is still behind a repository variable, and the stub issuer is what it was waiting for")
	}
}

// jobSteps is every `run` and every `env` of one job, as one string to
// assert over. A workflow is data, and what a job runs is the claim.
func jobSteps(t *testing.T, path, job string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err := unmarshalYAML(data, &o); err != nil {
		t.Fatal(err)
	}
	jobs, _ := o["jobs"].(map[string]any)
	spec, ok := jobs[job].(map[string]any)
	if !ok {
		t.Fatalf("%s has no job %s", path, job)
	}
	var out strings.Builder
	for _, s := range list(spec["steps"]) {
		out.WriteString(str(dig(s, "run")) + "\n")
		for name, value := range envOf(dig(s, "env")) {
			out.WriteString(name + "=" + value + "\n")
		}
	}
	return out.String()
}

// envOf reads a step's environment as strings.
func envOf(v any) map[string]string {
	out := map[string]string{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for name, value := range m {
		out[name] = str(value)
	}
	return out
}

// TestKindStackRunsTheDesktop: the kind stack declares a desktop, so the
// computer-use case runs against a real API server instead of skipping.
// up.sh builds and loads the display image, the overlay names it to the
// driver, and both kind conformance runs declare display and input and
// pass the image the case creates its sandbox from.
func TestKindStackRunsTheDesktop(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("deploy", "examples", "kind-stubs", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`-f "$root/images/display/Dockerfile" -t cella-display:dev`,
		"kind load docker-image cellad:dev cella-stubs:dev cella-display:dev",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("up.sh does not carry %q", want)
		}
	}
	config := find(t, render(t, "deploy/examples/kind-stubs"), "ConfigMap", "cellad")
	if got := str(dig(config, "data", "CELLA_K8S_DISPLAY_IMAGE")); got != "cella-display:dev" {
		t.Errorf("the stack's CELLA_K8S_DISPLAY_IMAGE is %q, want the image up.sh loads", got)
	}
	const declared = "-capabilities files,pool,mesh,attach,display,input"
	const image = "-display-image cella-display:dev"
	for _, run := range []struct{ workflow, job string }{
		{"verify.yml", "install"},
		{"release.yml", "conformance"},
	} {
		steps := jobSteps(t, filepath.Join(".github", "workflows", run.workflow), run.job)
		for _, want := range []string{declared, image} {
			if !strings.Contains(steps, want) {
				t.Errorf("the kind conformance run of %s %s does not pass %q", run.workflow, run.job, want)
			}
		}
	}
	// Each job that brings the stack up supplies the image up.sh loads: a
	// build from the checkout in verify, the published bytes in release.
	for _, run := range []struct{ workflow, job, want string }{
		{"verify.yml", "install", "docker build -f images/display/Dockerfile -t cella-display:dev ."},
		{"release.yml", "conformance", `docker tag "${REGISTRY}/${OWNER}/${DISPLAY_IMAGE}@${DISPLAY_DIGEST}" cella-display:dev`},
		{"release.yml", "install-release", `docker tag "${REGISTRY}/${OWNER}/${DISPLAY_IMAGE}:${GITHUB_REF_NAME}" cella-display:dev`},
	} {
		if steps := jobSteps(t, filepath.Join(".github", "workflows", run.workflow), run.job); !strings.Contains(steps, run.want) {
			t.Errorf("the %s job of %s does not supply the display image (%q)", run.job, run.workflow, run.want)
		}
	}
}
