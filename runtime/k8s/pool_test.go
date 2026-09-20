// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

// prewarm makes one pool entry on the client double.
func prewarm(t *testing.T, h *harness, id string) driver.State {
	t.Helper()
	if _, err := h.Create(t.Context(), driver.CreateSpec{
		ID: id, Image: image, Prewarm: true, Labels: map[string]string{"shape": "a"},
	}); err != nil {
		t.Fatalf("prewarm %s: %v", id, err)
	}
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestPoolEntryIsUnowned holds the entry's shape: the claim and the Pod carry
// the pool label, nobody owns it, and the projection it will need at adoption
// is already mounted, because the kubelet cannot add a volume to a running Pod.
func TestPoolEntryIsUnowned(t *testing.T) {
	h := newHarness(t)
	state := prewarm(t, h, "sbx_pool")
	if !state.Pool || state.Owner != "" || state.Name != "" {
		t.Fatalf("the entry is %+v, want an unowned pool entry", state)
	}
	pvc, err := h.getClaim(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Labels[labelPool] != "true" {
		t.Errorf("the claim's labels are %v, want the pool stamp", pvc.Labels)
	}
	pod, err := h.getPod(t.Context(), "sbx_pool")
	if err != nil || pod == nil {
		t.Fatalf("the entry has no Pod: %v", err)
	}
	if pod.Labels[labelPool] != "true" {
		t.Errorf("the Pod's labels are %v, want the pool stamp", pod.Labels)
	}
	if !hasTokenProjection(pod) {
		t.Errorf("the entry's Pod carries no token projection, so an adoption could never hand it one")
	}
	yes, no := true, false
	if got := listIDs(t, h, driver.Filter{Pool: &yes}); len(got) != 1 {
		t.Errorf("Filter.Pool true selects %v, want the entry", got)
	}
	if got := listIDs(t, h, driver.Filter{Pool: &no}); len(got) != 0 {
		t.Errorf("Filter.Pool false selects %v, want nothing", got)
	}
	if got := listIDs(t, h, driver.Filter{Owner: "alice"}); len(got) != 0 {
		t.Errorf("an owner filter selects %v, want nothing", got)
	}
}

func hasTokenProjection(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == tokenVolume {
			return true
		}
	}
	return false
}

func listIDs(t *testing.T, h *harness, f driver.Filter) []string {
	t.Helper()
	states, err := h.List(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range states {
		out = append(out, s.ID)
	}
	return out
}

// TestAdoptionRewritesTheClaim is one adoption end to end on the cluster's
// objects: the record, the stamps, the Secret the Pod already mounts, and the
// label that takes the sandbox out of the pool.
func TestAdoptionRewritesTheClaim(t *testing.T) {
	h := newHarness(t)
	entry := prewarm(t, h, "sbx_pool")
	adoption := driver.Adoption{
		Owner: "alice", Name: "adopted", Labels: map[string]string{"team": "a"},
		Env:       map[string]string{"GREETING": "hello"},
		Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute},
		Token:     []byte("workload"),
		Egress:    driver.Egress{Mode: "allowlist", Credential: "cred"},
	}
	if err := h.Update(t.Context(), "sbx_pool", driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	state, err := h.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case state.Pool:
		t.Error("the adopted sandbox still reports Pool")
	case state.Owner != "alice" || state.Name != "adopted":
		t.Errorf("the adopted sandbox is %q/%q, want alice/adopted", state.Owner, state.Name)
	case state.Labels["team"] != "a":
		t.Errorf("the adopted labels are %v", state.Labels)
	case !state.CreatedAt.After(entry.CreatedAt):
		t.Errorf("CreatedAt %v is not after the prewarm's %v", state.CreatedAt, entry.CreatedAt)
	case !state.ExpiresAt.Equal(state.CreatedAt.Add(time.Hour)):
		t.Errorf("ExpiresAt %v, want the adoption plus the ttl", state.ExpiresAt)
	case state.AutoStop != time.Minute:
		t.Errorf("AutoStop %v, want a minute", state.AutoStop)
	case state.Phase != driver.Running:
		t.Errorf("the adopted sandbox is %q, want Running", state.Phase)
	}
	pvc, err := h.getClaim(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, stamped := pvc.Labels[labelPool]; stamped {
		t.Errorf("the adopted claim keeps the pool stamp: %v", pvc.Labels)
	}
	if pvc.Labels[labelName] != "adopted" {
		t.Errorf("the adopted claim's name label is %q", pvc.Labels[labelName])
	}
	// The stored spec is what a later Start renders the Pod from, so the
	// adoption must leave nothing of the pool in it.
	spec, err := specOf(pvc)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Prewarm || spec.Owner != "alice" || spec.Env["GREETING"] != "hello" || spec.Egress.Credential != "cred" {
		t.Errorf("the stored spec after adoption is %+v", spec)
	}
	secret, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName("sbx_pool"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the adopted identity is not in the cluster: %v", err)
	}
	if string(secret.Data[tokenKey]) != "workload" {
		t.Errorf("the adopted token is %q", secret.Data[tokenKey])
	}
	pod, err := h.getPod(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if _, stamped := pod.Labels[labelPool]; stamped || pod.Labels[labelName] != "adopted" {
		t.Errorf("the adopted Pod's labels are %v", pod.Labels)
	}
}

// TestAdoptionIsExclusive is the guarded patch. Two adopters read one entry
// and race; the label the patch tests is what makes exactly one of them the
// owner, and the loser is told the entry is gone.
func TestAdoptionIsExclusive(t *testing.T) {
	h := newHarness(t)
	versioned(h.cs)
	prewarm(t, h, "sbx_pool")
	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	owners := [2]string{"alice", "bob"}
	for i, owner := range owners {
		wg.Go(func() {
			errs[i] = h.Update(t.Context(), "sbx_pool", driver.Change{
				Adopt: &driver.Adoption{Owner: owner, Name: "adopted"}})
		})
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won >= 0:
			t.Fatalf("both adoptions won: %v", errs)
		case err == nil:
			won = i
		case !errors.Is(err, driver.ErrNotFound):
			t.Fatalf("the losing adoption is %v, want ErrNotFound", err)
		}
	}
	if won < 0 {
		t.Fatalf("neither adoption won: %v", errs)
	}
	state, err := h.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if state.Owner != owners[won] {
		t.Errorf("the claim says %q, want the winner %q", state.Owner, owners[won])
	}
}

// TestAdoptionRefusals covers what the contract refuses and what the cluster
// does: a sandbox that is not an entry, a workspace the entry does not have,
// an adoption beside another change, and a negative deadline.
func TestAdoptionRefusals(t *testing.T) {
	h := newHarness(t)
	if _, err := h.Create(t.Context(), driver.CreateSpec{ID: "sbx_owned", Image: image, Owner: "alice", Name: "owned"}); err != nil {
		t.Fatal(err)
	}
	prewarm(t, h, "sbx_pool")
	labels := map[string]string{"a": "1"}
	for _, c := range []struct {
		name string
		id   string
		want error
		ch   driver.Change
	}{
		{"owned", "sbx_owned", driver.ErrNotFound, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}}},
		{"absent", "sbx-missing", driver.ErrNotFound, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}}},
		{"workspace", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob", Workspace: driver.Workspace{Path: "/elsewhere"}}}},
		{"lifecycle", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob", Lifecycle: driver.Lifecycle{AutoStop: -time.Minute}}}},
		{"mixed", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}, Labels: &labels}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := h.Update(t.Context(), c.id, c.ch); !errors.Is(err, c.want) {
				t.Fatalf("Update = %v, want %v", err, c.want)
			}
		})
	}
	state, err := h.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Pool || state.Owner != "" {
		t.Errorf("a refused adoption changed the entry: %+v", state)
	}
}

// TestPrewarmRefusesACallersCreate is the create side of the same rule.
func TestPrewarmRefusesACallersCreate(t *testing.T) {
	h := newHarness(t)
	for _, spec := range []driver.CreateSpec{
		{ID: "sbx_x", Image: image, Prewarm: true, Owner: "alice"},
		{ID: "sbx_x", Image: image, Prewarm: true, Command: []string{"sh"}},
		{ID: "sbx_x", Image: image, Prewarm: true, Token: []byte("t")},
	} {
		if _, err := h.Create(t.Context(), spec); !errors.Is(err, driver.ErrInvalid) {
			t.Fatalf("Create %+v = %v, want ErrInvalid", spec, err)
		}
	}
}

// TestAdoptionDiscardsTheEntryWhenTheProjectionFails is the claim-then-project
// order's consequence: the entry is already out of the pool when the identity
// cannot be written, so it is deleted rather than left carrying half of one
// caller's sandbox.
func TestAdoptionDiscardsTheEntryWhenTheProjectionFails(t *testing.T) {
	h := newHarness(t)
	prewarm(t, h, "sbx_pool")
	h.cs.PrependReactor("*", "secrets", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, errors.New("the cluster refuses every secret")
	})
	err := h.Update(t.Context(), "sbx_pool", driver.Change{
		Adopt: &driver.Adoption{Owner: "alice", Name: "adopted", Token: []byte("workload")}})
	if err == nil {
		t.Fatal("the adoption reported success with no identity projected")
	}
	if _, err := h.Inspect(t.Context(), "sbx_pool"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the half-adopted entry is still there: %v", err)
	}
}
