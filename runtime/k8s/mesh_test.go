// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

const testMesh = "msh_01j9zk2p7q8r9s0t1u2v3w4x5y"

// meshSpec is a create spec for one member of the test mesh.
func meshSpec(id, name string) driver.CreateSpec {
	return driver.CreateSpec{ID: id, Name: name, Owner: "alice", Image: "img", Mesh: driver.Mesh{ID: testMesh}}
}

// TestRenderMesh holds the Pod to the two fields the cluster's DNS reads: the
// member's own name and the mesh's headless Service, so a peer is reached at
// <name>.<mesh>. A sandbox in no mesh names neither.
func TestRenderMesh(t *testing.T) {
	h := newHarness(t)
	pod, err := h.pod(meshSpec("sbx_one", "planner"), h.clock.at, false)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Hostname != "planner" || pod.Spec.Subdomain != driver.MeshObjectName(testMesh) {
		t.Fatalf("the Pod is published as %q.%q", pod.Spec.Hostname, pod.Spec.Subdomain)
	}
	if pod.Labels[labelMesh] != testMesh {
		t.Fatalf("the Pod carries the mesh label %q", pod.Labels[labelMesh])
	}
	alone, err := h.pod(driver.CreateSpec{ID: "sbx_alone", Name: "alone", Owner: "alice", Image: "img"}, h.clock.at, false)
	if err != nil {
		t.Fatal(err)
	}
	if alone.Spec.Hostname != "" || alone.Spec.Subdomain != "" || alone.Labels[labelMesh] != "" {
		t.Fatalf("a sandbox in no mesh is published as %q.%q", alone.Spec.Hostname, alone.Spec.Subdomain)
	}
	// A prewarmed entry has no name until it is adopted, so it is published
	// nowhere rather than under an empty hostname.
	entry := meshSpec("sbx_entry", "")
	if got := meshHostname(entry); got != "" {
		t.Fatalf("an unnamed entry is published as %q", got)
	}
}

// TestMeshPolicyAdmitsTheMeshAndNothingElse is the east-west rule: the policy
// selects the mesh's members and admits ingress from the same set, so a peer
// reaches a member and every other mesh reaches none.
func TestMeshPolicyAdmitsTheMeshAndNothingElse(t *testing.T) {
	h := newHarness(t)
	policy, service := h.meshObjects(testMesh)
	want := map[string]string{labelManagedBy: managedValue, labelMesh: testMesh}
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, want) {
		t.Fatalf("the policy selects %v, want %v", policy.Spec.PodSelector.MatchLabels, want)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
		t.Fatalf("the policy has %d rules, want one source", len(policy.Spec.Ingress))
	}
	from := policy.Spec.Ingress[0].From[0]
	if from.PodSelector == nil || !reflect.DeepEqual(from.PodSelector.MatchLabels, want) {
		t.Fatalf("the policy admits %+v, want the mesh's own members", from)
	}
	if from.NamespaceSelector != nil || from.IPBlock != nil {
		t.Fatalf("the policy admits more than the mesh: %+v", from)
	}
	if !slices.Contains(policy.Spec.PolicyTypes, "Ingress") {
		t.Fatalf("the policy types are %v, want Ingress", policy.Spec.PolicyTypes)
	}
	if service.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("the Service has the cluster IP %q, want a headless one", service.Spec.ClusterIP)
	}
	if !reflect.DeepEqual(service.Spec.Selector, want) {
		t.Fatalf("the Service selects %v, want %v", service.Spec.Selector, want)
	}
	if policy.Name != driver.MeshObjectName(testMesh) || service.Name != policy.Name {
		t.Fatalf("the objects are named %q and %q", policy.Name, service.Name)
	}
}

// TestMeshLifetime: the two objects are in the cluster with the first member
// and gone after the last, and a member arriving second finds them made.
func TestMeshLifetime(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	name := driver.MeshObjectName(testMesh)

	if _, err := h.Create(ctx, meshSpec("sbx_one", "planner")); err != nil {
		t.Fatal(err)
	}
	h.meshHeld(t, name, true)
	if _, err := h.Create(ctx, meshSpec("sbx_two", "worker")); err != nil {
		t.Fatalf("the second member could not join the mesh its peer made: %v", err)
	}

	// The stamped identity answers both selectors without reading an object.
	states, err := h.List(ctx, driver.Filter{MeshID: testMesh})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("the mesh holds %d members, want 2", len(states))
	}

	if err = h.Delete(ctx, "sbx_one"); err != nil {
		t.Fatal(err)
	}
	h.meshHeld(t, name, true)
	if err = h.Delete(ctx, "sbx_two"); err != nil {
		t.Fatal(err)
	}
	h.meshHeld(t, name, false)
}

// TestMeshlessSandboxMakesNoMeshObject: a sandbox in no mesh leaves the
// cluster with no mesh policy and no Service of its own. Its own network
// rule is the sandbox's and not a mesh's.
func TestMeshlessSandboxMakesNoMeshObject(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.Create(ctx, driver.CreateSpec{ID: "sbx_alone", Name: "alone", Owner: "alice", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	services, err := h.cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	policies, err := h.cs.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var mesh []string
	for _, p := range policies.Items {
		if p.Name != sandboxRuleName("sbx_alone") {
			mesh = append(mesh, p.Name)
		}
	}
	if len(services.Items) != 0 || len(mesh) != 0 {
		t.Fatalf("the cluster holds %d services and the policies %v, want none", len(services.Items), mesh)
	}
	if err = h.Delete(ctx, "sbx_alone"); err != nil {
		t.Fatal(err)
	}
}

// TestMeshObjectsAlreadyGone: objects another deletion already removed are not
// an error, so two members deleted at once do not leave one delete failing on
// what the other took.
func TestMeshObjectsAlreadyGone(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.Create(ctx, meshSpec("sbx_one", "planner")); err != nil {
		t.Fatal(err)
	}
	name := driver.MeshObjectName(testMesh)
	if err := h.cs.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.cs.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx, "sbx_one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// meshHeld asserts whether the cluster holds one mesh's two objects.
func (h *harness) meshHeld(t *testing.T, name string, want bool) {
	t.Helper()
	_, policyErr := h.cs.NetworkingV1().NetworkPolicies(namespace).Get(t.Context(), name, metav1.GetOptions{})
	_, serviceErr := h.cs.CoreV1().Services(namespace).Get(t.Context(), name, metav1.GetOptions{})
	for _, c := range []struct {
		what string
		err  error
	}{{"policy", policyErr}, {"service", serviceErr}} {
		held := c.err == nil
		if !held && !apierrors.IsNotFound(c.err) {
			t.Fatalf("reading the mesh %s: %v", c.what, c.err)
		}
		if held != want {
			t.Fatalf("the mesh %s %s is held: %v, want %v", c.what, name, held, want)
		}
	}
}

// TestMeshJoinFailureLeavesNoObjects: a first member whose mesh could not be
// made leaves neither the sandbox nor a policy nobody selects, because the
// objects are made inside the create's own rollback.
func TestMeshJoinFailureLeavesNoObjects(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.cs.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		if action.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).Name != driver.MeshObjectName(testMesh) {
			return false, nil, nil
		}
		return true, nil, errors.New("the cluster refused the policy")
	})
	if _, err := h.Create(ctx, meshSpec("sbx_one", "planner")); err == nil {
		t.Fatal("a create whose mesh could not be made was accepted")
	}
	if _, err := h.Inspect(ctx, "sbx_one"); err == nil {
		t.Fatal("the refused create left a sandbox behind")
	}
	services, err := h.cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 0 {
		t.Fatalf("the refused create left %d services behind", len(services.Items))
	}
}
