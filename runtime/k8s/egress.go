// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Peer is a set of Pods a sandbox's network rule admits egress to: the
// labels they carry, the namespace they run in, and the ports they listen
// on. The ports are the Pods' own, because a NetworkPolicy matches the
// destination after a Service has translated the address a client dialed.
type Peer struct {
	// Namespace holds the Pods. Empty is the namespace the default names.
	Namespace string
	// Labels select the Pods in that namespace.
	Labels map[string]string
	// Ports are the TCP ports admitted, and for cluster DNS the ports
	// admitted over UDP and TCP both.
	Ports []int32
}

// The defaults of the two peers. Cluster DNS is the convention nearly every
// distribution keeps, CoreDNS included: kube-system, k8s-app=kube-dns, port
// 53. The gateway's ports are the listen defaults of `cellad egress`, its
// proxy door and its reverse door.
const (
	DefaultDNSNamespace = "kube-system"
	DefaultDNSLabelKey  = "k8s-app"
	DefaultDNSLabel     = "kube-dns"
	DefaultDNSPort      = int32(53)
)

// DefaultGatewayPorts are the gateway's two doors as its Pods listen on them.
func DefaultGatewayPorts() []int32 {
	return []int32{egress.DefaultProxyPort, egress.DefaultReversePort}
}

// labelSandbox names the sandbox whose network rule a Pod runs under. The
// sandbox's own Pod carries it, and so does the helper Pod a transfer on a
// stopped sandbox runs in, because the helper runs the sandbox's image and
// its programs are the workload's. The id label cannot serve: List reads
// the Pod that carries it as the sandbox's, and the helper is not.
const labelSandbox = prefix + "sandbox"

// egressModes is what a driver that confines every sandbox to the gateway
// enforces. The rule is the same for all three: nothing leaves but through
// the gateway, and the gateway decides per connection which hosts the
// sandbox's mode admits.
var egressModes = []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist, v1.EgressOpen}

// confines reports whether the operator named the gateway's Pods, which is
// what turns on the egress half of the rule and the declared modes.
func (d *Driver) confines() bool { return len(d.opts.Gateway.Labels) > 0 }

// withPeerDefaults fills the two peers. The maps and slices are copied, so
// a caller that keeps its Options keeps them unshared.
func (o Options) withPeerDefaults() Options {
	o.Gateway = Peer{Namespace: o.Gateway.Namespace, Labels: maps.Clone(o.Gateway.Labels), Ports: slices.Clone(o.Gateway.Ports)}
	if o.Gateway.Namespace == "" {
		o.Gateway.Namespace = o.Namespace
	}
	if len(o.Gateway.Ports) == 0 {
		o.Gateway.Ports = DefaultGatewayPorts()
	}
	o.DNS = Peer{Namespace: o.DNS.Namespace, Labels: maps.Clone(o.DNS.Labels), Ports: slices.Clone(o.DNS.Ports)}
	if o.DNS.Namespace == "" {
		o.DNS.Namespace = DefaultDNSNamespace
	}
	if len(o.DNS.Labels) == 0 {
		o.DNS.Labels = map[string]string{DefaultDNSLabelKey: DefaultDNSLabel}
	}
	if len(o.DNS.Ports) == 0 {
		o.DNS.Ports = []int32{DefaultDNSPort}
	}
	return o
}

// Check refuses a peer the API server would refuse in a NetworkPolicy: a
// namespace that is no DNS label, a label key or value the label grammar
// does not take, and a port outside 1 to 65535. The sentence names the
// first thing wrong, so a start-up problem says what to fix.
func (p Peer) Check() error {
	if p.Namespace != "" {
		if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
			return fmt.Errorf("the namespace %q is not a namespace name: %s", p.Namespace, errs[0])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(p.Labels)) {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("the label key %q is not a label key: %s", key, errs[0])
		}
		if errs := validation.IsValidLabelValue(p.Labels[key]); len(errs) > 0 {
			return fmt.Errorf("the label %s=%q is not a label value: %s", key, p.Labels[key], errs[0])
		}
	}
	for _, port := range p.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("the port %d is not between 1 and 65535", port)
		}
	}
	return nil
}

// sandboxRuleName is the rule of one sandbox. The prefix keeps it apart from
// the mesh's own policy, whose name a sandbox id could otherwise spell.
func sandboxRuleName(id string) string { return "cella-sandbox-" + objectName(id) }

// sandboxRule renders the network rule of one sandbox (spec 018): no ingress
// but what another policy admits, and, once the gateway's Pods are named,
// egress to cluster DNS, to the gateway, and to the members of the
// sandbox's own mesh, and to nothing else.
//
// Each peer is one element of `to` holding both a namespace selector and a
// Pod selector, which the API reads as a Pod that matches both. Two elements
// would be read as either, and every Pod of the gateway's namespace would be
// reachable on the gateway's ports.
func (d *Driver) sandboxRule(id, meshID string) *networkingv1.NetworkPolicy {
	selector := map[string]string{labelManagedBy: managedValue, labelSandbox: id}
	policy := &networkingv1.NetworkPolicy{
		Name: sandboxRuleName(id), Namespace: d.opts.Namespace, Labels: maps.Clone(selector),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// No ingress rule: nothing reaches the sandbox but what another
			// policy admits, which is a mesh member's peers through the
			// mesh's own policy. The control plane reaches a sandbox through
			// the API server and the kubelet, where no policy applies.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
		},
	}
	if !d.confines() {
		return policy
	}
	policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
	dns := d.opts.DNS
	policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{
		{To: []networkingv1.NetworkPolicyPeer{peerOf(dns)}, Ports: ports(dns.Ports, corev1.ProtocolUDP, corev1.ProtocolTCP)},
		{To: []networkingv1.NetworkPolicyPeer{peerOf(d.opts.Gateway)}, Ports: ports(d.opts.Gateway.Ports, corev1.ProtocolTCP)},
	}
	if meshID != "" {
		// A member reaches its own peers on any port and no other mesh's:
		// the peers are the mesh's members in this namespace, the same set
		// the mesh's policy admits ingress from.
		peers := metav1.LabelSelector{MatchLabels: map[string]string{labelManagedBy: managedValue, labelMesh: meshID}}
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &peers}},
		})
	}
	return policy
}

// peerOf is one peer as a single element of `to`: the namespace by its name
// label, which the API server sets on every namespace, and the Pods by
// theirs.
func peerOf(p Peer) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: p.Namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: maps.Clone(p.Labels)},
	}
}

// ports admits each port over each protocol named.
func ports(numbers []int32, protocols ...corev1.Protocol) []networkingv1.NetworkPolicyPort {
	out := make([]networkingv1.NetworkPolicyPort, 0, len(numbers)*len(protocols))
	for _, number := range numbers {
		for _, protocol := range protocols {
			port := intstr.FromInt32(number)
			out = append(out, networkingv1.NetworkPolicyPort{Protocol: ptr(protocol), Port: &port})
		}
	}
	return out
}

// putSandboxRule writes the sandbox's rule, replacing one that is there. It
// runs where no Pod of the sandbox is running: before the first Pod of a
// create, before the new Pod of a start, and before the helper of a
// transfer on a stopped sandbox. The replacement is a delete and a create,
// which are the two verbs the Role grants on NetworkPolicies, and the gap
// between them confines nothing that runs.
func (d *Driver) putSandboxRule(ctx context.Context, id, meshID string) error {
	rule := d.sandboxRule(id, meshID)
	policies := d.cs.NetworkingV1().NetworkPolicies(d.opts.Namespace)
	_, err := policies.Create(ctx, rule, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return wrap(err, "sandbox rule create")
	}
	if err := policies.Delete(ctx, rule.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("sandbox rule delete: %w", err)
	}
	// A rule another call wrote between the two is rendered from the same
	// options, so it is the rule this call would have written.
	if _, err := policies.Create(ctx, rule, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("sandbox rule create: %w", err)
	}
	return nil
}

// deleteSandboxRule removes the sandbox's rule. A rule that is already gone
// is the state this asks for.
func (d *Driver) deleteSandboxRule(ctx context.Context, id string) error {
	err := d.cs.NetworkingV1().NetworkPolicies(d.opts.Namespace).Delete(ctx, sandboxRuleName(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("sandbox rule delete: %w", err)
	}
	return nil
}

// wrap names the call a failure came from, and is nil for no failure.
func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// egressEnv is the sandbox's own environment with the gateway's projection
// added: the two doors, the credential both authenticate, and the trust
// variables naming the gateway's authority where the create handed one
// over. A boundary with no door adds nothing, so a sandbox on an
// installation that runs no gateway is unchanged.
func egressEnv(own map[string]string, boundary driver.Egress) map[string]string {
	projection := egress.Projection{
		ProxyAddr:   boundary.ProxyAddr,
		ReverseAddr: boundary.ReverseAddr,
		Credential:  boundary.Credential,
	}
	if boundary.CAPEM != "" {
		projection.CAPath = egress.CAPath
	}
	added := projection.Env()
	if len(added) == 0 {
		return own
	}
	out := maps.Clone(own)
	if out == nil {
		out = make(map[string]string, len(added))
	}
	maps.Copy(out, added)
	return out
}

// ParsePorts reads a comma-separated list of ports, the form the operator's
// variable takes.
func ParsePorts(raw string) ([]int32, error) {
	var out []int32
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.ParseInt(field, 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q is not a port between 1 and 65535", field)
		}
		out = append(out, int32(n))
	}
	return out, nil
}
