// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime/k8s"
)

// TestKindRunsTheGateway: the kind stack runs the egress gateway with a key
// up.sh mints at the control plane and an upstream a sandbox may reach; its
// ConfigMap names the gateway's Pods to the driver by labels those Pods
// carry, on ports they listen on, and points the sandboxes at the gateway's
// Service; and both kind conformance runs declare egress, pass that upstream,
// and require the three cluster tests of the boundary. A selector that
// matched no Pod would confine every sandbox to nothing, and a run that did
// not declare egress would hold the server to a warning it no longer gives.
func TestKindRunsTheGateway(t *testing.T) {
	objects := render(t, "deploy/examples/kind-stubs")
	config := find(t, objects, "ConfigMap", "cellad")
	gateway := find(t, objects, "Deployment", "cellad-egress")
	namespace := str(dig(gateway, "metadata", "namespace"))

	selector := map[string]string{}
	for pair := range strings.SplitSeq(str(dig(config, "data", "CELLA_K8S_GATEWAY_SELECTOR")), ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			t.Fatalf("CELLA_K8S_GATEWAY_SELECTOR holds %q, which is no key=value", pair)
		}
		selector[key] = value
	}
	labels, _ := dig(gateway, "spec", "template", "metadata", "labels").(map[string]any)
	for key, value := range selector {
		if str(labels[key]) != value {
			t.Errorf("the driver selects the gateway by %s=%s, and its Pods carry %v", key, value, labels)
		}
	}
	if len(selector) == 0 {
		t.Fatal("the stack names no gateway Pods, so its sandboxes are not confined")
	}

	cs := containers(gateway)
	if len(cs) != 1 {
		t.Fatalf("the gateway's Pod holds %d containers", len(cs))
	}
	c := cs[0]
	if args := list(dig(c, "args")); len(args) != 1 || str(args[0]) != "egress" {
		t.Errorf("the gateway runs %v, want the egress role", args)
	}
	var listens []string
	for _, p := range list(dig(c, "ports")) {
		listens = append(listens, fmt.Sprint(dig(p, "containerPort")))
	}
	for _, port := range k8s.DefaultGatewayPorts() {
		if !slices.Contains(listens, fmt.Sprint(port)) {
			t.Errorf("the driver admits the gateway on %d and its container listens on %v", port, listens)
		}
	}
	key := false
	for _, e := range list(dig(c, "env")) {
		if str(dig(e, "name")) == "CELLA_ENVIRONMENT_KEY" && str(dig(e, "valueFrom", "secretKeyRef", "name")) == "cellad-egress" {
			key = true
		}
	}
	if !key {
		t.Error("the gateway does not read its environment key from the cellad-egress Secret")
	}

	// The sandboxes are pointed at the gateway's own Service.
	find(t, objects, "Service", "cellad-egress")
	door := str(dig(config, "data", "CELLA_GATEWAY"))
	if host, _, err := net.SplitHostPort(door); err != nil || host != "cellad-egress."+namespace+".svc" {
		t.Errorf("CELLA_GATEWAY is %q, want the gateway's Service in %s", door, namespace)
	}

	// The key is minted the way an operator mints one, and the stack waits
	// for the gateway to connect before it hands the cluster over.
	script, err := os.ReadFile(filepath.Join("deploy", "examples", "kind-stubs", "up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/v1/environments/default/keys",
		"create secret generic cellad-egress",
		"rollout restart deploy/cellad-egress",
		`"gateways":[1-9]`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("up.sh does not carry %q", want)
		}
	}

	upstream := find(t, objects, "Service", "cella-upstream")
	find(t, objects, "Deployment", "cella-upstream")
	ports := list(dig(upstream, "spec", "ports"))
	if len(ports) != 1 {
		t.Fatalf("the upstream publishes %d ports", len(ports))
	}
	want := fmt.Sprintf("cella-upstream.%s.svc:%v", namespace, dig(ports[0], "port"))
	for _, run := range []struct{ file, job, log string }{
		{"verify.yml", "install", "kind.txt"},
		{"release.yml", "conformance", "/tmp/kind.log"},
	} {
		steps := jobSteps(t, filepath.Join(".github", "workflows", run.file), run.job)
		declared, _ := flagValue(steps, "-capabilities")
		if !slices.Contains(strings.Split(declared, ","), "egress") {
			t.Errorf("the %s job of %s declares %q to the suite, without egress", run.job, run.file, declared)
		}
		if got, _ := flagValue(steps, "-upstream"); got != want {
			t.Errorf("the %s job of %s passes the upstream %q, want the stack's %q", run.job, run.file, got, want)
		}
		for _, test := range []string{"TestClusterEgressBoundary", "TestClusterNoLateralMovement", "TestClusterMeshReachability"} {
			if !strings.Contains(steps, "--- PASS: "+test+"' "+run.log) {
				t.Errorf("the %s job of %s does not require %s to pass", run.job, run.file, test)
			}
		}
	}
}
