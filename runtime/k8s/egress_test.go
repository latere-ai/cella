// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// gatewayLabels are the labels the fixtures' gateway Pods carry. They are an
// example, not an installation's.
var gatewayLabels = map[string]string{"app.kubernetes.io/name": "gateway"}

// withGateway is the option a case sets to drive a driver whose operator
// named the gateway's Pods.
func withGateway(o *Options) {
	o.Gateway = Peer{Namespace: "gateways", Labels: maps.Clone(gatewayLabels)}
}

// ruleOf reads one sandbox's rule from the cluster double.
func (h *harness) ruleOf(t *testing.T, id string) *networkingv1.NetworkPolicy {
	t.Helper()
	rule, err := h.cs.NetworkingV1().NetworkPolicies(namespace).Get(t.Context(), sandboxRuleName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the rule of %s: %v", id, err)
	}
	return rule
}

// ruleHeld reports whether the cluster double holds one sandbox's rule.
func (h *harness) ruleHeld(t *testing.T, id string) bool {
	t.Helper()
	_, err := h.cs.NetworkingV1().NetworkPolicies(namespace).Get(t.Context(), sandboxRuleName(id), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("reading the rule of %s: %v", id, err)
	}
	return err == nil
}

// order records the writes the cluster double accepted, as "verb resource
// name", so a case asserts what came first.
type order struct {
	mu     sync.Mutex
	writes []string
}

func (o *order) watch(h *harness) {
	record := func(action k8stesting.Action) (bool, kruntime.Object, error) {
		name := ""
		switch a := action.(type) {
		case k8stesting.CreateAction:
			if m, ok := a.GetObject().(metav1.Object); ok {
				name = m.GetName()
			}
		case k8stesting.DeleteAction:
			name = a.GetName()
		}
		o.mu.Lock()
		o.writes = append(o.writes, action.GetVerb()+" "+action.GetResource().Resource+" "+name)
		o.mu.Unlock()
		return false, nil, nil
	}
	h.cs.PrependReactor("create", "*", record)
	h.cs.PrependReactor("delete", "*", record)
}

// index is where one write came, or -1.
func (o *order) index(write string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Index(o.writes, write)
}

// TestEgressModesFollowTheGateway: the three modes are declared once the
// gateway's Pods are named, because then every sandbox runs under the rule
// that leaves the gateway as its only way out, and none is declared without
// it, so an installation with no gateway keeps its warning truthfully.
func TestEgressModesFollowTheGateway(t *testing.T) {
	if got := newHarness(t).Capabilities().Egress; len(got) != 0 {
		t.Fatalf("a driver with no gateway declares %v", got)
	}
	got := newHarnessWith(t, withGateway).Capabilities()
	want := []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist, v1.EgressOpen}
	if !reflect.DeepEqual(got.Egress, want) {
		t.Fatalf("a driver with a gateway declares %v, want %v", got.Egress, want)
	}
	// The declaration is a copy: a caller that edits it edits nothing here.
	got.Egress[0] = "edited"
	if newHarnessWith(t, withGateway).Capabilities().Egress[0] != v1.EgressNone {
		t.Fatal("the declaration shares its list with the driver")
	}
}

// TestTheSandboxRuleAdmitsDNSAndTheGateway is the rule of spec 018 as the
// cluster holds it: the sandbox by its own label, no ingress, and egress to
// cluster DNS over both protocols and to the gateway's two doors, each peer
// one element holding both selectors.
func TestTheSandboxRuleAdmitsDNSAndTheGateway(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_ruled"
	h.created(t, spec(id))

	if got := h.podOf(t, id).Labels[labelSandbox]; got != id {
		t.Fatalf("the Pod carries the sandbox label %q, want %q", got, id)
	}
	rule := h.ruleOf(t, id)
	wantSelector := map[string]string{labelManagedBy: managedValue, labelSandbox: id}
	if !reflect.DeepEqual(rule.Spec.PodSelector.MatchLabels, wantSelector) || len(rule.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("the rule selects %+v, want %v", rule.Spec.PodSelector, wantSelector)
	}
	if !reflect.DeepEqual(rule.Labels, wantSelector) {
		t.Errorf("the rule is labeled %v", rule.Labels)
	}
	if want := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}; !reflect.DeepEqual(rule.Spec.PolicyTypes, want) {
		t.Fatalf("the rule's types are %v, want %v", rule.Spec.PolicyTypes, want)
	}
	if len(rule.Spec.Ingress) != 0 {
		t.Fatalf("the rule admits ingress %+v", rule.Spec.Ingress)
	}
	if len(rule.Spec.Egress) != 2 {
		t.Fatalf("the rule has %d egress rules, want DNS and the gateway", len(rule.Spec.Egress))
	}
	for _, c := range []struct {
		what      string
		rule      networkingv1.NetworkPolicyEgressRule
		namespace string
		labels    map[string]string
		ports     []string
	}{
		{"DNS", rule.Spec.Egress[0], DefaultDNSNamespace, map[string]string{DefaultDNSLabelKey: DefaultDNSLabel}, []string{"UDP/53", "TCP/53"}},
		{"the gateway", rule.Spec.Egress[1], "gateways", gatewayLabels, []string{"TCP/3128", "TCP/8080"}},
	} {
		if len(c.rule.To) != 1 {
			t.Fatalf("%s is %d peers; one peer holding both selectors means a Pod that matches both", c.what, len(c.rule.To))
		}
		peer := c.rule.To[0]
		if peer.NamespaceSelector == nil || !reflect.DeepEqual(peer.NamespaceSelector.MatchLabels, map[string]string{corev1.LabelMetadataName: c.namespace}) {
			t.Errorf("%s is in the namespaces %+v, want %s", c.what, peer.NamespaceSelector, c.namespace)
		}
		if peer.PodSelector == nil || !reflect.DeepEqual(peer.PodSelector.MatchLabels, c.labels) {
			t.Errorf("%s is the Pods %+v, want %v", c.what, peer.PodSelector, c.labels)
		}
		if peer.IPBlock != nil {
			t.Errorf("%s admits an address block %+v", c.what, peer.IPBlock)
		}
		var got []string
		for _, p := range c.rule.Ports {
			got = append(got, string(*p.Protocol)+"/"+p.Port.String())
		}
		if !reflect.DeepEqual(got, c.ports) {
			t.Errorf("%s is admitted on %v, want %v", c.what, got, c.ports)
		}
	}
}

// TestTheSandboxRuleWithoutAGatewayConfinesIngress: with no gateway named
// the rule confines ingress alone. Confining egress to DNS with nothing to
// route through would leave the sandbox reaching nothing while its
// condition says the boundary is not enforced.
func TestTheSandboxRuleWithoutAGatewayConfinesIngress(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_open"
	h.created(t, spec(id))
	rule := h.ruleOf(t, id)
	if want := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}; !reflect.DeepEqual(rule.Spec.PolicyTypes, want) {
		t.Fatalf("the rule's types are %v, want %v", rule.Spec.PolicyTypes, want)
	}
	if len(rule.Spec.Ingress) != 0 || len(rule.Spec.Egress) != 0 {
		t.Fatalf("the rule admits ingress %v and egress %v", rule.Spec.Ingress, rule.Spec.Egress)
	}
}

// TestTheSandboxRuleIsWrittenBeforeThePod: the Pod is confined from its
// first packet, because its rule is in the cluster before it is.
func TestTheSandboxRuleIsWrittenBeforeThePod(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	var o order
	o.watch(h)
	const id = "sbx_first"
	h.created(t, spec(id))
	rule, pod := o.index("create networkpolicies "+sandboxRuleName(id)), o.index("create pods "+objectName(id))
	if rule < 0 || pod < 0 || rule > pod {
		t.Fatalf("the writes were %v; the rule comes before the Pod", o.writes)
	}
}

// TestAFailedCreateRemovesTheSandboxRule: a create that fails after the
// claim leaves no rule selecting nothing, whichever step failed.
func TestAFailedCreateRemovesTheSandboxRule(t *testing.T) {
	for _, resource := range []string{"pods", "secrets", "networkpolicies"} {
		t.Run(resource, func(t *testing.T) {
			h := newHarnessWith(t, withGateway)
			h.cs.PrependReactor("create", resource, func(k8stesting.Action) (bool, kruntime.Object, error) {
				return true, nil, errors.New("the cluster refused the " + resource)
			})
			const id = "sbx_refused"
			s := tokenSpec(id, "first")
			if _, err := h.Create(t.Context(), s); err == nil || !strings.Contains(err.Error(), "refused the "+resource) {
				t.Fatalf("Create = %v, want the refusal", err)
			}
			if h.ruleHeld(t, id) {
				t.Fatal("a failed create left its rule behind")
			}
			if _, err := h.Inspect(t.Context(), id); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("a failed create left the sandbox behind: %v", err)
			}
		})
	}
}

// TestDeleteRemovesTheSandboxRule: the rule goes with the sandbox, and only
// once its Pod is gone, so no instant has a running Pod and no rule.
func TestDeleteRemovesTheSandboxRule(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_gone"
	h.created(t, spec(id))
	var o order
	o.watch(h)
	if err := h.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if h.ruleHeld(t, id) {
		t.Fatal("the rule outlived the sandbox")
	}
	pod, rule := o.index("delete pods "+objectName(id)), o.index("delete networkpolicies "+sandboxRuleName(id))
	if pod < 0 || rule < 0 || rule < pod {
		t.Fatalf("the writes were %v; the Pod goes before its rule", o.writes)
	}
	// A rule another delete already removed is not an error.
	if err := h.deleteSandboxRule(t.Context(), id); err != nil {
		t.Fatalf("a second removal: %v", err)
	}
}

// TestStartRewritesTheSandboxRule: a start writes the rule the driver's
// options describe before the new Pod, so a sandbox stopped under another
// rule, or under none, starts confined as the driver now confines.
func TestStartRewritesTheSandboxRule(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_restart"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// A rule in an older shape: ingress alone, the way a driver with no
	// gateway wrote it.
	stale := h.ruleOf(t, id)
	if err := h.cs.NetworkingV1().NetworkPolicies(namespace).Delete(t.Context(), stale.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	stale.Spec.PolicyTypes, stale.Spec.Egress = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, nil
	stale.ResourceVersion = ""
	if _, err := h.cs.NetworkingV1().NetworkPolicies(namespace).Create(t.Context(), stale, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var o order
	o.watch(h)
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := h.ruleOf(t, id); len(got.Spec.Egress) != 2 {
		t.Fatalf("the started sandbox runs under %d egress rules, want the gateway's shape", len(got.Spec.Egress))
	}
	rule, pod := o.index("create networkpolicies "+sandboxRuleName(id)), o.index("create pods "+objectName(id))
	if rule < 0 || pod < 0 || rule > pod {
		t.Fatalf("the writes were %v; the rule comes before the Pod", o.writes)
	}
	// A sandbox that has no rule at all takes one at its start.
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.deleteSandboxRule(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if !h.ruleHeld(t, id) {
		t.Fatal("a start left the sandbox without a rule")
	}
}

// TestStartStopsAtARuleItCannotWrite: a start whose rule the cluster refuses
// starts no Pod, because a Pod with no rule would run outside the boundary.
func TestStartStopsAtARuleItCannotWrite(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_norule"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	h.cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the cluster refused the rule")
	})
	if err := h.Start(t.Context(), id); err == nil || !strings.Contains(err.Error(), "refused the rule") {
		t.Fatalf("Start = %v, want the refusal", err)
	}
	if pod, err := h.getPod(t.Context(), id); err != nil || pod != nil {
		t.Fatalf("a Pod was started without its rule: %v, %v", pod, err)
	}
}

// TestPutSandboxRuleReportsWhatTheClusterSaid: each call of the replacement
// that fails is named, and a rule another call wrote between the delete and
// the create is the rule this one would have written.
func TestPutSandboxRuleReportsWhatTheClusterSaid(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWith(t, withGateway)
	if err := h.putSandboxRule(ctx, "sbx_x", ""); err != nil {
		t.Fatal(err)
	}
	h.cs.PrependReactor("delete", "networkpolicies", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the cluster refused the delete")
	})
	if err := h.putSandboxRule(ctx, "sbx_x", ""); err == nil || !strings.Contains(err.Error(), "sandbox rule delete") {
		t.Fatalf("a refused delete reads as %v", err)
	}
	if err := h.deleteSandboxRule(ctx, "sbx_x"); err == nil || !strings.Contains(err.Error(), "sandbox rule delete") {
		t.Fatalf("a refused removal reads as %v", err)
	}

	h = newHarnessWith(t, withGateway)
	if err := h.putSandboxRule(ctx, "sbx_x", ""); err != nil {
		t.Fatal(err)
	}
	creates := 0
	h.cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, kruntime.Object, error) {
		creates++
		if creates == 1 {
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: networkGroup, Resource: "networkpolicies"}, sandboxRuleName("sbx_x"))
		}
		return true, nil, errors.New("the cluster refused the second create")
	})
	if err := h.putSandboxRule(ctx, "sbx_x", ""); err == nil || !strings.Contains(err.Error(), "second create") {
		t.Fatalf("a refused second create reads as %v", err)
	}

	h = newHarnessWith(t, withGateway)
	h.cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: networkGroup, Resource: "networkpolicies"}, sandboxRuleName("sbx_y"))
	})
	if err := h.putSandboxRule(ctx, "sbx_y", ""); err != nil {
		t.Fatalf("a rule written between the delete and the create: %v", err)
	}
}

// TestTheFileHelperRunsUnderTheSandboxRule: the helper of a transfer on a
// stopped sandbox runs the sandbox's image, so it runs under the sandbox's
// rule, written before the helper for a sandbox stopped without one.
func TestTheFileHelperRunsUnderTheSandboxRule(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_helped"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.deleteSandboxRule(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var o order
	o.watch(h)
	var labels map[string]string
	h.cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		labels = action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Labels
		return false, nil, nil
	})
	h.exec.handle = func(_ context.Context, _ execCall, stdin io.Reader, stdout, _ io.Writer) error {
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		_, _ = stdout.Write(archive(t, entry{name: "kept.txt", body: "kept"}))
		return nil
	}
	var out bytes.Buffer
	if err := h.ExportTar(t.Context(), id, nil, &out); err != nil {
		t.Fatalf("ExportTar while stopped: %v", err)
	}
	if labels[labelSandbox] != id || labels[labelID] != "" {
		t.Fatalf("the helper is labeled %v, want the sandbox label and no id", labels)
	}
	helper := objectName(id) + "-files"
	rule, pod := o.index("create networkpolicies "+sandboxRuleName(id)), o.index("create pods "+helper)
	if rule < 0 || pod < 0 || rule > pod {
		t.Fatalf("the writes were %v; the rule comes before the helper", o.writes)
	}

	// A rule the cluster refuses starts no helper.
	h.cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the cluster refused the rule")
	})
	if err := h.ExportTar(t.Context(), id, nil, &out); err == nil || !strings.Contains(err.Error(), "refused the rule") {
		t.Fatalf("a transfer without its rule reads as %v", err)
	}
}

// TestAMeshMemberReachesItsPeersAlone: under confinement a member's own rule
// admits its mesh's members on any port, and only them; a sandbox in no mesh
// has no such rule.
func TestAMeshMemberReachesItsPeersAlone(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	h.created(t, meshSpec("sbx_member", "planner"))
	rule := h.ruleOf(t, "sbx_member")
	if len(rule.Spec.Egress) != 3 {
		t.Fatalf("a member's rule has %d egress rules, want DNS, the gateway and its peers", len(rule.Spec.Egress))
	}
	peers := rule.Spec.Egress[2]
	want := map[string]string{labelManagedBy: managedValue, labelMesh: testMesh}
	if len(peers.To) != 1 || peers.To[0].PodSelector == nil || !reflect.DeepEqual(peers.To[0].PodSelector.MatchLabels, want) {
		t.Fatalf("a member reaches %+v, want the members of its own mesh", peers.To)
	}
	if peers.To[0].NamespaceSelector != nil || peers.To[0].IPBlock != nil || len(peers.Ports) != 0 {
		t.Fatalf("the peer rule reaches beyond the mesh's own namespace or narrows its ports: %+v", peers)
	}
	h.created(t, spec("sbx_loner"))
	if got := len(h.ruleOf(t, "sbx_loner").Spec.Egress); got != 2 {
		t.Fatalf("a sandbox in no mesh has %d egress rules, want 2", got)
	}
	// Without a gateway no egress is confined, so no peer rule is needed.
	open := newHarness(t)
	open.created(t, meshSpec("sbx_member", "planner"))
	if got := open.ruleOf(t, "sbx_member").Spec.Egress; len(got) != 0 {
		t.Fatalf("an unconfined member's rule has egress rules %v", got)
	}
}

// gatewaySpec is a create that runs behind a gateway with both doors and an
// authority. The addresses are examples.
func gatewaySpec(id string) driver.CreateSpec {
	s := tokenSpec(id, "first")
	s.Egress = driver.Egress{
		Mode: "allowlist", AllowedHosts: []string{"api.example.com"},
		ProxyAddr: "gateway.example.com:3128", ReverseAddr: "gateway.example.com:8080",
		Credential: "credential", CAPEM: "-----BEGIN CERTIFICATE-----\nexample\n-----END CERTIFICATE-----\n",
	}
	return s
}

// TestThePodCarriesTheGatewayProjection: the workload container carries the
// doors, the credential and the trust variables the projection renders, the
// authority is a key of the sandbox's Secret projected at the reserved path,
// and the desktop container carries none of it.
func TestThePodCarriesTheGatewayProjection(t *testing.T) {
	h := newHarnessWith(t, func(o *Options) {
		withGateway(o)
		o.DisplayImage = "registry.example.com/display:1"
	})
	const id = "sbx_behind"
	s := gatewaySpec(id)
	s.Display = &driver.Geometry{Width: 1280, Height: 800}
	h.created(t, s)
	pod := h.podOf(t, id)
	env := envOf(pod)
	want := egress.Projection{
		ProxyAddr: s.Egress.ProxyAddr, ReverseAddr: s.Egress.ReverseAddr,
		Credential: s.Egress.Credential, CAPath: egress.CAPath,
	}.Env()
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	if env[driver.TokenFileEnv] != driver.TokenPath {
		t.Errorf("the token variable is lost beside the projection: %v", env)
	}
	for _, c := range pod.Spec.Containers[1:] {
		for _, e := range c.Env {
			if _, projected := want[e.Name]; projected {
				t.Errorf("the %s container carries %s", c.Name, e.Name)
			}
		}
	}
	if got := string(h.secretOf(t, id).Data[authorityKey]); got != s.Egress.CAPEM {
		t.Errorf("the Secret holds the authority %q", got)
	}
	if got := string(h.secretOf(t, id).Data[tokenKey]); got != "first" {
		t.Errorf("the Secret holds the token %q", got)
	}
	mountNamed(t, pod, tokenVolume)
	// A start renders the same projection from the claim's record.
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := envOf(h.podOf(t, id))["HTTPS_PROXY"]; got != want["HTTPS_PROXY"] {
		t.Errorf("a started Pod's HTTPS_PROXY is %q", got)
	}

	// An authority with no token still mounts the projection.
	bare := gatewaySpec("sbx_notoken")
	bare.Token = nil
	h.created(t, bare)
	mountNamed(t, h.podOf(t, "sbx_notoken"), tokenVolume)
	if _, ok := envOf(h.podOf(t, "sbx_notoken"))[driver.TokenFileEnv]; ok {
		t.Error("a sandbox with no token names a token file")
	}
}

// TestTheAuthoritySurvivesARotation: a token rotation writes the token's key
// alone, so the gateway's authority stays where the workload trusts it.
func TestTheAuthoritySurvivesARotation(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_rotated"
	s := gatewaySpec(id)
	h.created(t, s)
	if err := h.Update(t.Context(), id, driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	secret := h.secretOf(t, id)
	if string(secret.Data[tokenKey]) != "second" || string(secret.Data[authorityKey]) != s.Egress.CAPEM {
		t.Fatalf("after a rotation the Secret holds %v", secret.Data)
	}
}

// TestAnAdoptionWritesTheAuthority: an entry prewarmed under the rule and
// with both keys of the projection listed receives the adopting sandbox's
// authority through its Secret.
func TestAnAdoptionWritesTheAuthority(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	prewarm(t, h, "sbx_pool")
	if !h.ruleHeld(t, "sbx_pool") {
		t.Fatal("a prewarmed entry runs without its rule")
	}
	boundary := gatewaySpec("sbx_pool").Egress
	if err := h.Update(t.Context(), "sbx_pool", driver.Change{Adopt: &driver.Adoption{
		Owner: "alice", Name: "adopted", Token: []byte("workload"), Egress: boundary,
	}}); err != nil {
		t.Fatal(err)
	}
	secret := h.secretOf(t, "sbx_pool")
	if string(secret.Data[authorityKey]) != boundary.CAPEM || string(secret.Data[tokenKey]) != "workload" {
		t.Fatalf("the adopted entry's Secret holds %v", secret.Data)
	}
	volume := volumeNamed(t, h.podOf(t, "sbx_pool"), tokenVolume)
	items := volume.Projected.Sources[0].Secret.Items
	if !slices.ContainsFunc(items, func(i corev1.KeyToPath) bool { return i.Key == authorityKey }) {
		t.Fatalf("the entry's projection lists %v, without the authority", items)
	}
	// The running entry's environment cannot change, so the proxy variables
	// arrive with its next Pod, rendered from the adopted record.
	if _, ok := envOf(h.podOf(t, "sbx_pool"))["HTTPS_PROXY"]; ok {
		t.Fatal("the entry's running Pod was rewritten with the proxy variables")
	}
	if err := h.Stop(t.Context(), "sbx_pool"); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), "sbx_pool"); err != nil {
		t.Fatal(err)
	}
	want := egress.Projection{ProxyAddr: boundary.ProxyAddr, Credential: boundary.Credential}.Env()["HTTPS_PROXY"]
	if got := envOf(h.podOf(t, "sbx_pool"))["HTTPS_PROXY"]; got != want {
		t.Fatalf("the adopted sandbox's next Pod carries HTTPS_PROXY %q, want %q", got, want)
	}
	// An adoption that carries neither a token nor an authority writes no
	// Secret key.
	prewarm(t, h, "sbx_bare")
	if err := h.Update(t.Context(), "sbx_bare", driver.Change{Adopt: &driver.Adoption{Owner: "alice", Name: "bare"}}); err != nil {
		t.Fatal(err)
	}
	if secret := h.secretOf(t, "sbx_bare"); len(secret.Data[authorityKey]) != 0 {
		t.Fatalf("an adoption with no gateway wrote %v", secret.Data)
	}
}

// TestNoGatewayProjectsNothing: a boundary with no door adds no variable, no
// authority and no mount, so a sandbox on an installation with no gateway is
// unchanged.
func TestNoGatewayProjectsNothing(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_nodoor"
	s := spec(id)
	s.Egress = driver.Egress{Mode: "open"}
	h.created(t, s)
	pod := h.podOf(t, id)
	for _, key := range egress.ReservedEnv() {
		if _, ok := envOf(pod)[key]; ok {
			t.Errorf("%s is set on a sandbox with no gateway", key)
		}
	}
	if slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == tokenVolume }) {
		t.Error("a projection was mounted for a sandbox with no token and no authority")
	}
	if got := egressEnv(nil, driver.Egress{}); got != nil {
		t.Errorf("no boundary renders %v", got)
	}
	if got := egressEnv(nil, driver.Egress{ProxyAddr: "gateway.example.com", Credential: "c"}); got["HTTPS_PROXY"] == "" || got["SSL_CERT_FILE"] != "" {
		t.Errorf("a door with no authority renders %v", got)
	}
}

// TestNewRefusesAMalformedPeer: a peer the API server would refuse in a
// NetworkPolicy is refused before any sandbox, naming what is wrong.
func TestNewRefusesAMalformedPeer(t *testing.T) {
	for _, c := range []struct {
		name string
		opts func(*Options)
		want string
	}{
		{"namespace", func(o *Options) { o.Gateway = Peer{Namespace: "Not_A_Namespace", Labels: gatewayLabels} }, "namespace"},
		{"label key", func(o *Options) { o.Gateway = Peer{Labels: map[string]string{"bad key": "x"}} }, "label key"},
		{"label value", func(o *Options) { o.Gateway = Peer{Labels: map[string]string{"app": "no spaces"}} }, "label value"},
		{"port", func(o *Options) { o.Gateway = Peer{Labels: gatewayLabels, Ports: []int32{70000}} }, "port"},
		{"DNS", func(o *Options) { o.DNS = Peer{Ports: []int32{0}} }, "DNS"},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := Options{Namespace: namespace, Client: newHarness(t).cs}
			c.opts(&opts)
			_, err := New(opts)
			if !errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New = %v, want an invalid %s", err, c.want)
			}
		})
	}
}

// TestPeerDefaults: the gateway's namespace is the sandboxes' own and its
// ports the two doors' listen defaults; DNS is kube-system's resolver on 53.
// The options a caller holds are copied, never shared.
func TestPeerDefaults(t *testing.T) {
	labels := map[string]string{"app": "gateway"}
	o := Options{Namespace: "work", Gateway: Peer{Labels: labels}}.withDefaults()
	if o.Gateway.Namespace != "work" || !reflect.DeepEqual(o.Gateway.Ports, []int32{egress.DefaultProxyPort, egress.DefaultReversePort}) {
		t.Errorf("the gateway defaults to %+v", o.Gateway)
	}
	if o.DNS.Namespace != DefaultDNSNamespace || o.DNS.Labels[DefaultDNSLabelKey] != DefaultDNSLabel || !reflect.DeepEqual(o.DNS.Ports, []int32{DefaultDNSPort}) {
		t.Errorf("DNS defaults to %+v", o.DNS)
	}
	o.Gateway.Labels["app"] = "edited"
	if labels["app"] != "gateway" {
		t.Error("the driver shares the caller's labels")
	}
}

// TestParsePorts reads the operator's list and refuses what is no port.
func TestParsePorts(t *testing.T) {
	got, err := ParsePorts(" 3128, 8080 ,")
	if err != nil || !reflect.DeepEqual(got, []int32{3128, 8080}) {
		t.Fatalf("ParsePorts = %v, %v", got, err)
	}
	for _, bad := range []string{"0", "65536", "http", "-1"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("ParsePorts(%q) was accepted", bad)
		}
	}
	if got, err := ParsePorts(""); err != nil || got != nil {
		t.Errorf("an empty list reads as %v, %v", got, err)
	}
}
