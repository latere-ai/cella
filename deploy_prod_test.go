// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

// The tests of what the release pipeline deploys: the shape of the deploy job
// in release.yml, the production overlay (deploy/prod) and the rollout
// identity it is applied with (deploy/prod-bootstrap). The workflow is read
// as text, because it carries expressions the manifests do not; the overlay
// is rendered.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	prodBootstrap = "deploy/prod-bootstrap"
	// nullDigest is a digest no image has, standing in for the one the
	// build stage pushes.
	nullDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func readFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// jobBlock returns the block of one job of release.yml, from its key under
// `jobs:` to the next line at the jobs' own indentation: the next job, or the
// comment above it.
func jobBlock(t *testing.T, name string) string {
	t.Helper()
	text := readFile(t, filepath.Join(".github", "workflows", "release.yml"))
	_, after, ok := strings.Cut(text, "\n  "+name+":\n")
	if !ok {
		t.Fatalf("release.yml has no job named %q", name)
	}
	lines := strings.Split(after, "\n")
	for i, line := range lines {
		if len(line) > 2 && strings.HasPrefix(line, "  ") && line[2] != ' ' {
			lines = lines[:i]
			break
		}
	}
	return "\n" + strings.Join(lines, "\n")
}

// TestTheDeployJobIsGatedAndCallsTheDeployStage holds the one job that
// reaches a cluster to the two things that keep it off every other
// repository, the repository variable that gates it and the secret it hands
// the shared stage, and holds it to the stage: it writes no kubectl itself.
func TestTheDeployJobIsGatedAndCallsTheDeployStage(t *testing.T) {
	block := jobBlock(t, "deploy")
	for _, want := range []string{
		"\n    if: ${{ vars.CELLA_RELEASE_DEPLOY != '' }}",
		"\n    uses: " + stageDir + "image-deploy.yml@v1\n",
		"\n      DEPLOY_KUBECONFIG: ${{ secrets.CELLA_KUBECONFIG }}",
		"\n    permissions:\n      contents: read\n",
		"\n      deployments: |\n        cellad\n        cellad-egress\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the deploy job does not carry %q", strings.TrimSpace(want))
		}
	}
	for _, gone := range []string{"kubectl", "kustomize", "sed -i", "tools/smoke/release.sh"} {
		if strings.Contains(block, gone) {
			t.Errorf("the deploy job runs %q itself; image-deploy.yml does", gone)
		}
	}
}

// TestTheDeployWaitsForEveryJobThatProvesTheImage: production never rolls
// before the release's own tests have passed the candidate.
func TestTheDeployWaitsForEveryJobThatProvesTheImage(t *testing.T) {
	block := jobBlock(t, "deploy")
	if !strings.Contains(block, "\n    needs: [build, conformance]\n") {
		t.Error("the deploy job does not need both the build and the conformance run of the candidate")
	}
}

// TestTheDeployPassesOnlyCelladByDigest: the build pushes the images by
// digest under no tag, and the publish stage gives the tag only after the
// deploy has passed, so the stage is given the digest and never the tag,
// under the name the overlay gives the image. The desktop and the stubs are
// not rolled by it.
func TestTheDeployPassesOnlyCelladByDigest(t *testing.T) {
	block := jobBlock(t, "deploy")
	want := "\n      images: ghcr.io/latere-ai/cellad=${{ needs.build.outputs.ref_1 }}@${{ needs.build.outputs.digest_1 }}\n"
	if !strings.Contains(block, want) {
		t.Error("the deploy job does not pass cellad as the overlay's name for it, pinned by digest")
	}
	for _, not := range []string{"needs.build.outputs.images", "ref_2", "digest_2", "ref_3", "digest_3", "github.ref_name"} {
		if strings.Contains(block, not) {
			t.Errorf("the deploy job names %q; it passes only the cellad image, by digest", not)
		}
	}
	if n := strings.Count(block, "\n      images:"); n != 1 {
		t.Errorf("the deploy job has %d images inputs, want 1", n)
	}
}

// TestTheDeploySmokesTheOriginTheSmokeScriptDocuments: the URL the stage
// hands tools/smoke/release.sh as BASE_URL is the one the script names as
// production's.
func TestTheDeploySmokesTheOriginTheSmokeScriptDocuments(t *testing.T) {
	url := "https://api.latere.ai/v1/environments"
	if !strings.Contains(jobBlock(t, "deploy"), "\n      url: "+url+"\n") {
		t.Errorf("the deploy job does not smoke %s", url)
	}
	if !strings.Contains(readFile(t, "tools/smoke/release.sh"), "production is "+url) {
		t.Errorf("tools/smoke/release.sh does not name %s as production's BASE_URL", url)
	}
}

// TestEveryJobAfterTheDeployToleratesASkippedDeploy: deploy is skipped on
// every repository that sets no CELLA_RELEASE_DEPLOY, and a job that leans on
// the implicit success() is skipped whenever a job above it in the graph is.
// Each job below it names the results it needs, and publish fails on a
// deploy that failed or was cancelled.
func TestEveryJobAfterTheDeployToleratesASkippedDeploy(t *testing.T) {
	publish := jobBlock(t, "publish")
	for _, want := range []string{
		"always()",
		"needs.build.result == 'success'",
		"needs.conformance.result == 'success'",
		"needs.deploy.result != 'failure'",
		"needs.deploy.result != 'cancelled'",
	} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish does not carry %q in its condition", want)
		}
	}
	// The smoke evidence exists only when the deploy ran; an evidence
	// pattern that names no file fails the publish stage.
	if !strings.Contains(publish, "evidence: ${{ needs.deploy.result == 'success' && 'smoke-evidence' || '' }}") {
		t.Error("publish does not name the smoke evidence only when the deploy succeeded")
	}
	if !strings.Contains(jobBlock(t, "install-release"), "if: ${{ always() && needs.build.result == 'success' && needs.publish.result == 'success' }}") {
		t.Error("install-release does not run on a skipped deploy")
	}
	if !strings.Contains(jobBlock(t, "release-verify"), "if: ${{ always() && needs.publish.result == 'success' }}") {
		t.Error("release-verify does not run on a skipped deploy")
	}
}

// TestTheDeployArchiveLeavesOutTheOwnInstallation: the archive attached to a
// release is for whoever installs the core, and Latere's own overlay and its
// operator-applied half are not theirs.
func TestTheDeployArchiveLeavesOutTheOwnInstallation(t *testing.T) {
	text := readFile(t, filepath.Join(".github", "workflows", "release.yml"))
	for _, dir := range []string{"deploy/prod", "deploy/prod-bootstrap"} {
		if !strings.Contains(text, "--exclude="+dir+" ") {
			t.Errorf("the deploy archive does not exclude %s", dir)
		}
	}
}

// grants maps each kind in the production tree to the group and resource the
// rollout identity must grant for it.
var grants = map[string][2]string{
	"Deployment":          {"apps", "deployments"},
	"Service":             {"", "services"},
	"ServiceAccount":      {"", "serviceaccounts"},
	"Ingress":             {"networking.k8s.io", "ingresses"},
	"NetworkPolicy":       {"networking.k8s.io", "networkpolicies"},
	"CiliumNetworkPolicy": {"cilium.io", "ciliumnetworkpolicies"},
	"PodDisruptionBudget": {"policy", "poddisruptionbudgets"},
}

// TestProdIsNamespacedAndHoldsNoCredential: the rollout identity is a Role in
// one namespace, so every object the pipeline applies is in it; the overlay
// is a public file that names the Secrets the Deployments read and holds none
// of their values.
func TestProdIsNamespacedAndHoldsNoCredential(t *testing.T) {
	for _, o := range render(t, productionTree) {
		if ns := str(dig(o, "metadata", "namespace")); ns != "latere" {
			t.Errorf("%s %s is in %q; the rollout identity writes in latere alone", o.kind(), o.name(), ns)
		}
		if o.kind() == "Secret" {
			t.Errorf("deploy/prod renders the Secret %s; a Secret is created outside the tree", o.name())
		}
		if _, ok := grants[o.kind()]; !ok {
			t.Errorf("deploy/prod renders a %s, which the rollout identity's grant does not cover; add it to %s/rollout-identity.yaml and to this test", o.kind(), prodBootstrap)
		}
	}
}

// TestProdSelectsOnlyItsOwnPods: a rollout identity may write an object that
// selects its own service's Pods and no other's, so every selector at the top
// of a policy in deploy/prod names cellad or its gateway. The policy that
// selects platformd's Pods is in deploy/prod-bootstrap.
func TestProdSelectsOnlyItsOwnPods(t *testing.T) {
	own := map[string]bool{"cellad": true, "cellad-egress": true}
	policies := 0
	for _, o := range render(t, productionTree) {
		var app any
		switch o.kind() {
		case "NetworkPolicy":
			app = dig(o, "spec", "podSelector", "matchLabels", "app")
		case "CiliumNetworkPolicy":
			app = dig(o, "spec", "endpointSelector", "matchLabels", "app")
		default:
			continue
		}
		policies++
		if !own[str(app)] {
			t.Errorf("%s %s selects the app %q, which is not the control plane's or its gateway's", o.kind(), o.name(), str(app))
		}
	}
	if policies == 0 {
		t.Fatal("deploy/prod renders no policy, so the selectors are unchecked")
	}
	tls := readFile(t, prodBootstrap+"/networkpolicy.yaml")
	if !strings.Contains(tls, "name: cellad-platformd-tls") || !strings.Contains(tls, "      app: platformd\n") {
		t.Error("deploy/prod-bootstrap does not hold the policy that admits cellad to platformd")
	}
}

// TestProdTakesTheDigestTheDeployPins: the deploy stage runs `kustomize edit
// set image <name>=<ref>@<digest>` in deploy/prod and then requires the
// render to hold that reference. So the overlay names the image the way the
// Deployments write it, every Deployment runs it, and the image a volume is
// filled from follows it.
func TestProdTakesTheDigestTheDeployPins(t *testing.T) {
	const name = "ghcr.io/latere-ai/cellad"
	dir := t.TempDir()
	copyTree(t, productionTree, dir)
	path := filepath.Join(dir, "kustomization.yaml")
	text := readFile(t, path)
	pinned := regexp.MustCompile(`(?m)^(\s+- name: ` + regexp.QuoteMeta(name) + `)\n\s+newTag: \S+$`)
	if !pinned.MatchString(text) {
		t.Fatalf("deploy/prod/kustomization.yaml has no images entry named %s with a newTag", name)
	}
	// What `kustomize edit set image` writes for a digest.
	text = pinned.ReplaceAllString(text, "$1\n    newName: "+name+"\n    digest: "+nullDigest)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	objects := render(t, dir)
	want := name + "@" + nullDigest
	deployments := 0
	for _, o := range objects {
		if o.kind() != "Deployment" {
			continue
		}
		deployments++
		for _, c := range containers(o) {
			if got := str(dig(c, "image")); got != want {
				t.Errorf("Deployment %s runs %q, want %q", o.name(), got, want)
			}
			if str(dig(c, "name")) != "cellad" {
				continue
			}
			for _, e := range list(dig(c, "env")) {
				if str(dig(e, "name")) == "CELLA_K8S_FILL_IMAGE" && str(dig(e, "value")) != want {
					t.Errorf("the fill image is %q, want the control plane's %q", str(dig(e, "value")), want)
				}
			}
		}
	}
	if deployments != 2 {
		t.Errorf("deploy/prod renders %d Deployments, want cellad and cellad-egress", deployments)
	}
}

// copyTree copies the files of one directory, without descending, which is
// all a kustomization of this repository's production overlay holds.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("%s holds the directory %s; copyTree reads a flat overlay", from, e.Name())
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), []byte(readFile(t, filepath.Join(from, e.Name()))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheDeployJobWaitsForEveryDeploymentInProd: the Deployments the job
// waits for are the ones the overlay renders.
func TestTheDeployJobWaitsForEveryDeploymentInProd(t *testing.T) {
	var rendered []string
	for _, o := range render(t, productionTree) {
		if o.kind() == "Deployment" {
			rendered = append(rendered, o.name())
		}
	}
	slices.Sort(rendered)
	block := jobBlock(t, "deploy")
	_, after, ok := strings.Cut(block, "\n      deployments: |\n")
	if !ok {
		t.Fatal("the deploy job names no Deployments")
	}
	var waited []string
	for line := range strings.SplitSeq(after, "\n") {
		if !strings.HasPrefix(line, "        ") {
			break
		}
		waited = append(waited, strings.TrimSpace(line))
	}
	slices.Sort(waited)
	if !slices.Equal(waited, rendered) {
		t.Errorf("the deploy job waits for %v and deploy/prod renders %v", waited, rendered)
	}
}

// TestTheRolloutIdentityCoversExactlyWhatProdApplies: the Role allows apply
// on the kinds deploy/prod renders and no others, never touches Secrets or
// grants a Role, and never deletes; the token Secret is bound to the account
// it names.
func TestTheRolloutIdentityCoversExactlyWhatProdApplies(t *testing.T) {
	identity := readFile(t, prodBootstrap+"/rollout-identity.yaml")
	role := identity[strings.Index(identity, "kind: Role\n"):strings.Index(identity, "kind: RoleBinding")]

	group := ""
	var granted []string
	for line := range strings.SplitSeq(role, "\n") {
		if g, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")), "apiGroups: "); ok {
			group = strings.Trim(strings.TrimSpace(g), `[]"`)
		}
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), "resources: ["); ok {
			for res := range strings.SplitSeq(strings.TrimSuffix(r, "]"), ",") {
				granted = append(granted, group+"/"+strings.Trim(strings.TrimSpace(res), `"`))
			}
		}
	}
	slices.Sort(granted)
	granted = slices.Compact(granted)

	var want []string
	for _, o := range render(t, productionTree) {
		g := grants[o.kind()]
		want = append(want, g[0]+"/"+g[1])
	}
	slices.Sort(want)
	want = slices.Compact(want)
	if !slices.Equal(granted, want) {
		t.Errorf("the Role grants %v, deploy/prod applies %v", granted, want)
	}
	for _, not := range []string{`"secrets"`, `"delete"`, `"namespaces"`, `"*"`, "ClusterRole", `"roles"`, `"rolebindings"`} {
		if strings.Contains(identity, not) {
			t.Errorf("the rollout identity names %s; it holds no such grant", not)
		}
	}
	if !strings.Contains(role, `resources: ["deployments"]`+"\n"+`    verbs: ["watch"]`) {
		t.Error("the Role does not let the rollout watch the Deployments")
	}
	for _, w := range []string{
		"name: cellad-rollout\n  namespace: latere",
		"kubernetes.io/service-account.name: cellad-rollout",
		"type: kubernetes.io/service-account-token",
		"the `CELLA_KUBECONFIG` repository secret",
	} {
		if !strings.Contains(identity, w) {
			t.Errorf("the rollout identity does not carry %q", w)
		}
	}
}

// TestEverythingProdLeavesOutIsInProdBootstrap: the cluster scoped objects,
// the workloads namespace and the policy that selects another service's Pods
// are the operator's, so none is in deploy/prod and each is in the folder the
// pipeline never reads.
func TestEverythingProdLeavesOutIsInProdBootstrap(t *testing.T) {
	for _, o := range render(t, productionTree) {
		switch o.kind() {
		case "Namespace", "ClusterRole", "ClusterRoleBinding", "StorageClass", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "Role", "RoleBinding", "DaemonSet":
			t.Errorf("deploy/prod renders the %s %s, which the rollout identity may not write", o.kind(), o.name())
		}
	}
	for file, kinds := range map[string][]string{
		"namespace.yaml":        {"kind: Namespace"},
		"storageclass.yaml":     {"kind: StorageClass"},
		"admissionpolicy.yaml":  {"kind: ValidatingAdmissionPolicy\n", "kind: ValidatingAdmissionPolicyBinding"},
		"rbac.yaml":             {"kind: Role\n", "kind: RoleBinding", "kind: ClusterRole\n", "kind: ClusterRoleBinding"},
		"prepull.yaml":          {"kind: DaemonSet"},
		"networkpolicy.yaml":    {"name: cella-workloads-default-deny", "name: cellad-platformd-tls"},
		"rollout-identity.yaml": {"kind: Role\n", "kind: Secret"},
	} {
		body := readFile(t, prodBootstrap+"/"+file)
		for _, k := range kinds {
			if !strings.Contains(body, k) {
				t.Errorf("%s/%s does not hold %q", prodBootstrap, file, strings.TrimSpace(k))
			}
		}
	}
}
