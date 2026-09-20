// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// kustomizations are every directory `kubectl kustomize` is run over: the
// base and one per example. A directory added under deploy/ without a row
// here fails TestEveryKustomizationIsRendered.
var kustomizations = []string{
	"deploy/base",
	"deploy/examples/kind",
	"deploy/examples/generic",
}

// object is one rendered Kubernetes document, read as a tree so a test
// asserts over the fields it names and nothing else.
type object map[string]any

func (o object) kind() string { return str(o["kind"]) }

func (o object) name() string {
	meta, _ := o["metadata"].(map[string]any)
	return str(meta["name"])
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// dig walks a path of map keys and returns what is there, or nil. A
// rendered document arrives as an object at the top and as plain maps
// below it, so both are one step here.
func dig(o any, path ...string) any {
	for _, key := range path {
		switch m := o.(type) {
		case object:
			o = m[key]
		case map[string]any:
			o = m[key]
		default:
			return nil
		}
	}
	return o
}

// list reads a field that holds a sequence.
func list(v any) []any {
	s, _ := v.([]any)
	return s
}

// render runs `kubectl kustomize` over a directory and returns the
// documents. kubectl is on every runner the workflows use and on a
// developer's machine; where it is absent the test skips by name rather
// than passing quietly, and the `install` job of verify.yml is the run
// that always has it.
func render(t *testing.T, dir string) []object {
	t.Helper()
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skipf("kubectl is not on PATH, so the overlays are not rendered here: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), bin, "kustomize", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var objects []object
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("%s produced a document that does not parse: %v", dir, err)
		}
		objects = append(objects, o)
	}
	if len(objects) == 0 {
		t.Fatalf("%s rendered nothing; the assertions would pass vacuously", dir)
	}
	return objects
}

// find returns the one object of a kind and name, and fails when there is
// none.
func find(t *testing.T, objects []object, kind, name string) object {
	t.Helper()
	for _, o := range objects {
		if o.kind() == kind && o.name() == name {
			return o
		}
	}
	t.Fatalf("no %s named %s in the rendered output", kind, name)
	return nil
}

// containers returns the containers of a Deployment's or a Job's pod
// template.
func containers(o object) []any {
	return list(dig(o, "spec", "template", "spec", "containers"))
}

// TestEveryKustomizationIsRendered: a directory added under deploy/ with a
// kustomization joins the rendered set, so a new overlay cannot be the one
// nothing checks.
func TestEveryKustomizationIsRendered(t *testing.T) {
	var found []string
	err := filepath.WalkDir("deploy", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "kustomization.yaml" {
			return err
		}
		found = append(found, filepath.ToSlash(filepath.Dir(path)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	want := slices.Clone(kustomizations)
	slices.Sort(want)
	if !slices.Equal(found, want) {
		t.Fatalf("deploy/ holds the kustomizations %v and the tests render %v", found, want)
	}
}

// TestOverlaysRender is spec 014's row for the deploy tree: the base and
// every example build, the base pins no namespace and no registry, and an
// overlay pins a namespace.
func TestOverlaysRender(t *testing.T) {
	for _, dir := range kustomizations {
		t.Run(dir, func(t *testing.T) {
			objects := render(t, dir)
			deployment := find(t, objects, "Deployment", "cellad")
			namespace := str(dig(deployment, "metadata", "namespace"))
			if dir == "deploy/base" {
				if namespace != "" {
					t.Errorf("the base pins the namespace %q; an overlay names one", namespace)
				}
				for _, c := range containers(deployment) {
					if image := str(dig(c, "image")); image != "cellad" {
						t.Errorf("the base names the image %q; the placeholder is `cellad`, which an overlay or the release pipeline points at a registry", image)
					}
				}
				return
			}
			if namespace == "" {
				t.Error("an overlay names the namespace every object lands in")
			}
			// The Job runs the same build as the Deployment, so an
			// overlay that points one at a release points both.
			job := find(t, objects, "Job", "cellad-check")
			if got := str(dig(job, "metadata", "namespace")); got != namespace {
				t.Errorf("the check Job is in %q and the Deployment in %q", got, namespace)
			}
			// Every installation value reaches the container, so the
			// ConfigMap the base reads through envFrom has to exist.
			find(t, objects, "ConfigMap", "cellad")
		})
	}
}

// TestBaseIsConfined is the Pod security the release spec lists. It is not
// the sandbox baseline: this container mounts a service account token and
// reaches the API server, and a sandbox does neither.
func TestBaseIsConfined(t *testing.T) {
	objects := render(t, "deploy/base")
	for _, o := range []object{find(t, objects, "Deployment", "cellad"), find(t, objects, "Job", "cellad-check")} {
		t.Run(o.kind(), func(t *testing.T) {
			pod := dig(o, "spec", "template", "spec")
			if dig(pod, "securityContext", "runAsNonRoot") != true {
				t.Error("the pod does not declare runAsNonRoot")
			}
			if uid := dig(pod, "securityContext", "runAsUser"); uid == nil || fmt.Sprint(uid) == "0" {
				t.Errorf("the pod runs as uid %v; the image's non-root user is the one it was built for", uid)
			}
			if dig(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
				t.Error("the pod does not ask for the default seccomp profile")
			}
			cs := containers(o)
			if len(cs) != 1 {
				t.Fatalf("the pod holds %d container(s); one image, one role per process", len(cs))
			}
			c := cs[0]
			if dig(c, "securityContext", "allowPrivilegeEscalation") != false {
				t.Error("the container permits privilege escalation")
			}
			if dig(c, "securityContext", "readOnlyRootFilesystem") != true {
				t.Error("the container's root filesystem is writable")
			}
			drop := list(dig(c, "securityContext", "capabilities", "drop"))
			if len(drop) != 1 || str(drop[0]) != "ALL" {
				t.Errorf("the container drops %v, and the baseline drops every capability", drop)
			}
			// The read-only root filesystem needs CELLA_DATA_DIR
			// writable, which the readiness probe writes to at every
			// poll and the driver keeps its state under.
			var mounted bool
			for _, m := range list(dig(c, "volumeMounts")) {
				mounted = mounted || str(dig(m, "mountPath")) == "/var/lib/cella"
			}
			if !mounted {
				t.Error("CELLA_DATA_DIR is not a writable mount under a read-only root filesystem")
			}
		})
	}
}

// driverVerbs is the verb table of runtime/k8s, which is the list the Role
// is written from. The driver proves each one with an access review at
// start, so a Role that grants less is a start-up failure and a Role that
// grants more is access nothing uses.
var driverVerbs = map[string][]string{
	"pods":                   {"get", "list", "create", "delete", "patch"},
	"pods/exec":              {"create"},
	"pods/log":               {"get"},
	"persistentvolumeclaims": {"get", "list", "create", "delete", "patch"},
}

// TestRoleMatchesTheDriversVerbs holds the Role to that table exactly, in
// both directions, and refuses anything cluster-wide.
func TestRoleMatchesTheDriversVerbs(t *testing.T) {
	objects := render(t, "deploy/base")
	for _, o := range objects {
		if o.kind() == "ClusterRole" || o.kind() == "ClusterRoleBinding" {
			t.Errorf("the base holds a %s; the driver reaches one namespace and nothing cluster-wide", o.kind())
		}
	}
	granted := map[string][]string{}
	for _, rule := range list(find(t, objects, "Role", "cellad")["rules"]) {
		var verbs []string
		for _, v := range list(dig(rule, "verbs")) {
			verbs = append(verbs, str(v))
		}
		for _, r := range list(dig(rule, "resources")) {
			granted[str(r)] = verbs
		}
	}
	if !reflect.DeepEqual(granted, driverVerbs) {
		t.Errorf("the Role grants %v and the driver uses %v", granted, driverVerbs)
	}
}

// TestTheCheckJobRunsTheDeploymentsConfiguration: `cellad check` answers
// for the installation the control plane would meet, which holds only
// while the two carry one configuration. The two env lists are written
// twice, in two documents kustomize cannot share a container between, and
// this is what keeps them one.
func TestTheCheckJobRunsTheDeploymentsConfiguration(t *testing.T) {
	objects := render(t, "deploy/base")
	deployment := containers(find(t, objects, "Deployment", "cellad"))[0]
	job := containers(find(t, objects, "Job", "cellad-check"))[0]

	if !reflect.DeepEqual(dig(deployment, "env"), dig(job, "env")) {
		t.Errorf("the check Job's environment differs from the Deployment's:\n%v\n%v", dig(deployment, "env"), dig(job, "env"))
	}
	if !reflect.DeepEqual(dig(deployment, "envFrom"), dig(job, "envFrom")) {
		t.Error("the check Job reads a different ConfigMap from the Deployment")
	}
	if got := str(dig(job, "image")); got != str(dig(deployment, "image")) {
		t.Errorf("the check Job runs the image %q and the Deployment runs %q", got, str(dig(deployment, "image")))
	}
	for _, tc := range []struct {
		container any
		want      string
	}{{deployment, "serve"}, {job, "check"}} {
		args := list(dig(tc.container, "args"))
		if len(args) != 1 || str(args[0]) != tc.want {
			t.Errorf("the container's args are %v, want the %s role", args, tc.want)
		}
	}
}

// TestTheAudienceIsNamedInEveryContainer is the rule the identity gate
// reads over deploy/: a container that runs this repository's binary names
// the audience it verifies, as a literal value. A container that named
// none would accept a token addressed to anything.
func TestTheAudienceIsNamedInEveryContainer(t *testing.T) {
	objects := render(t, "deploy/base")
	for _, o := range objects {
		for _, c := range containers(o) {
			if str(dig(c, "image")) != "cellad" {
				continue
			}
			var found string
			for _, e := range list(dig(c, "env")) {
				if str(dig(e, "name")) == "CELLA_OIDC_AUDIENCE" {
					found = str(dig(e, "value"))
				}
			}
			if found == "" {
				t.Errorf("the %s's container names no CELLA_OIDC_AUDIENCE value", o.kind())
			}
		}
	}
}

// TestNoCredentialIsSharedBetweenTwoVariables is the other rule the
// identity gate reads: every credential between two services is per
// endpoint, so two variables of one container never read one Secret key.
func TestNoCredentialIsSharedBetweenTwoVariables(t *testing.T) {
	objects := render(t, "deploy/base")
	for _, o := range objects {
		for _, c := range containers(o) {
			seen := map[string]string{}
			for _, e := range list(dig(c, "env")) {
				ref := dig(e, "valueFrom", "secretKeyRef")
				if ref == nil {
					continue
				}
				key := str(dig(ref, "name")) + "/" + str(dig(ref, "key"))
				if first, ok := seen[key]; ok {
					t.Errorf("%s and %s both read %s", first, str(dig(e, "name")), key)
				}
				seen[key] = str(dig(e, "name"))
			}
		}
	}
}
