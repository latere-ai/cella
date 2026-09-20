// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// The label half of a sandbox's tree position: the mesh a Pod is a member of,
// which is what the policy and the Service select on, and the sandbox that
// spawned it, which is what a list of one sandbox's children reads.
const (
	labelMesh   = prefix + "mesh"
	labelParent = prefix + "parent"
)

// meshHostname is the name a member answers to inside its mesh, and
// meshSubdomain the headless Service that publishes it. Together they are
// what makes <sandbox-name>.<mesh> resolve to one peer: the kubelet writes
// the pair into the cluster's DNS, and a Pod that names neither is in no
// mesh and published nowhere.
//
// A sandbox whose name is no DNS label is published under none: the name is
// the caller's and the contract holds it to a label, so this is the shape of
// a pool entry, which has no name until it is adopted.
func meshHostname(s driver.CreateSpec) string {
	if s.Mesh.ID == "" || !dns1123.MatchString(s.Name) {
		return ""
	}
	return s.Name
}

func meshSubdomain(s driver.CreateSpec) string {
	if meshHostname(s) == "" {
		return ""
	}
	return driver.MeshObjectName(s.Mesh.ID)
}

// meshObjects renders the two objects one mesh needs on a cluster: a
// NetworkPolicy that admits ingress from the mesh's own members and from
// nothing else, and a headless Service that gives each member a name its
// peers resolve.
func (d *Driver) meshObjects(meshID string) (*networkingv1.NetworkPolicy, *corev1.Service) {
	name := driver.MeshObjectName(meshID)
	selector := metav1.LabelSelector{MatchLabels: map[string]string{
		labelManagedBy: managedValue,
		labelMesh:      meshID,
	}}
	labels := map[string]string{labelManagedBy: managedValue, labelMesh: meshID}
	policy := &networkingv1.NetworkPolicy{
		Name: name, Namespace: d.opts.Namespace, Labels: labels,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: selector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// One rule, one source: a member of this mesh. Everything the
			// rule does not name is denied, which is what makes a policy
			// that admits the mesh a policy that denies every other mesh.
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &selector}},
			}},
		},
	}
	service := &corev1.Service{
		Name: name, Namespace: d.opts.Namespace, Labels: labels,
		Spec: corev1.ServiceSpec{
			// A headless Service publishes each member's address under its
			// own hostname rather than one virtual address for the set, which
			// is what makes <name>.<mesh> resolve to one peer.
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 selector.MatchLabels,
			PublishNotReadyAddresses: true,
		},
	}
	return policy, service
}

// joinMesh puts the mesh's two objects in the cluster before the Pod that
// belongs to them, so a member is reachable by its peers and unreachable by
// everything else from the moment it starts. Objects another member already
// made are left as they are: a mesh is the set of its members and these are
// one object each under it, whichever member arrived first.
func (d *Driver) joinMesh(ctx context.Context, meshID string) error {
	if meshID == "" {
		return nil
	}
	policy, service := d.meshObjects(meshID)
	_, err := d.cs.NetworkingV1().NetworkPolicies(d.opts.Namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("mesh policy create: %w", err)
	}
	_, err = d.cs.CoreV1().Services(d.opts.Namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("mesh service create: %w", err)
	}
	return nil
}

// leaveMesh removes those two objects once the sandbox being deleted was the
// mesh's last member. A mesh ends with its last member, so what the cluster
// holds for it ends with it; objects another member still needs are left
// alone, and ones another deletion already removed are not an error.
func (d *Driver) leaveMesh(ctx context.Context, id, meshID string) error {
	if meshID == "" {
		return nil
	}
	members, err := d.List(ctx, driver.Filter{MeshID: meshID})
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.ID != id {
			return nil
		}
	}
	name := driver.MeshObjectName(meshID)
	err = d.cs.NetworkingV1().NetworkPolicies(d.opts.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("mesh policy delete: %w", err)
	}
	err = d.cs.CoreV1().Services(d.opts.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("mesh service delete: %w", err)
	}
	return nil
}
