// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// recorder collects the acts the controller emitted, which is the seam a
// store with no journal of its own is read through.
type recorder struct {
	mu   sync.Mutex
	acts []Act
}

func (r *recorder) Emit(_ context.Context, a Act) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, a)
}

func (r *recorder) EmitSecret(context.Context, SecretAct) {}

// of returns every act of one type, in the order they were emitted.
func (r *recorder) of(kind string) []Act {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Act
	for _, a := range r.acts {
		if a.Type == kind {
			out = append(out, a)
		}
	}
	return out
}

// spawning is a controller over the snapshot store with an emitter attached,
// which is the shape a node with no database runs in.
func spawning(t *testing.T) (*Controller, *recorder) {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	events := &recorder{}
	c, err := Open(Options{DataDir: t.TempDir(), Driver: d, Environment: "default", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, events
}

// root is a resolved manifest with spawn rights on both axes.
func root(name string, budget, depth int) v1.Sandbox {
	obj := workspace()
	obj.Metadata.Name = name
	obj.Spec.Mesh.Spawn = v1.Spawn{Budget: budget, Depth: depth}
	return obj
}

// child is a resolved manifest a spawn applies, with the rights a child of
// the root above may carry.
func child(name string, budget, depth int) v1.Sandbox {
	obj := workspace()
	obj.Metadata.Name = name
	obj.Metadata.Labels = map[string]string{"team": "a"}
	obj.Spec.Mesh.Spawn = v1.Spawn{Budget: budget, Depth: depth}
	return obj
}

// TestSpawnInheritance: a child carries its parent's id, the tree's root, the
// inherited mesh and the root's owner, and a grandchild's parent is its
// immediate parent rather than the root.
func TestSpawnInheritance(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 4, 2), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status.Parent != "" || parent.Status.Root != parent.Status.ID {
		t.Fatalf("the root is parent %q of root %q", parent.Status.Parent, parent.Status.Root)
	}
	if parent.Status.Spawn != (v1.SpawnStatus{Budget: 4, Depth: 2}) {
		t.Fatalf("the root's budget is %+v", parent.Status.Spawn)
	}
	first, err := c.Spawn(ctx, child("worker", 1, 1), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.Parent != parent.Status.ID || first.Status.Root != parent.Status.ID {
		t.Fatalf("the child is parent %q of root %q", first.Status.Parent, first.Status.Root)
	}
	if first.Status.Owner != "alice" {
		t.Fatalf("the child's owner is %q, want the root's", first.Status.Owner)
	}
	if first.Status.Spawn != (v1.SpawnStatus{Budget: 1, Depth: 1}) {
		t.Fatalf("the child's budget is %+v", first.Status.Spawn)
	}
	// The debit is the parent's, projected onto its status.
	read, err := c.Get(ctx, parent.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read.Status.Spawn.Used != 1 {
		t.Fatalf("the parent has used %d of its budget, want 1", read.Status.Spawn.Used)
	}

	grandchild, err := c.Spawn(ctx, child("helper", 0, 0), first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if grandchild.Status.Parent != first.Status.ID || grandchild.Status.Root != parent.Status.ID {
		t.Fatalf("the grandchild is parent %q of root %q", grandchild.Status.Parent, grandchild.Status.Root)
	}
	// The tree is every sandbox under one root, the root included.
	tree := c.Tree(parent.Status.ID)
	if len(tree) != 3 {
		t.Fatalf("the tree holds %d sandboxes, want 3", len(tree))
	}
	if got := c.Tree("sbx_nothing"); len(got) != 0 {
		t.Fatalf("the tree of no root holds %d sandboxes", len(got))
	}
}

// TestSpawnMintsAMesh: a root that asked for a mesh is where the id is
// minted, every descendant inherits it, and a root that asked for none leaves
// its tree unnetworked.
func TestSpawnMintsAMesh(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	manifest := root("planner", 2, 2)
	manifest.Spec.Mesh.Enabled = true
	parent, err := c.Create(ctx, manifest, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(parent.Status.Mesh, "msh_") || len(parent.Status.Mesh) != len("msh_")+26 {
		t.Fatalf("the root's mesh is %q, want an msh_ id", parent.Status.Mesh)
	}
	first, err := c.Spawn(ctx, child("worker", 1, 1), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.Mesh != parent.Status.Mesh {
		t.Fatalf("the child is in the mesh %q, want its parent's %q", first.Status.Mesh, parent.Status.Mesh)
	}
	grandchild, err := c.Spawn(ctx, child("helper", 0, 0), first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if grandchild.Status.Mesh != parent.Status.Mesh {
		t.Fatalf("the grandchild is in the mesh %q", grandchild.Status.Mesh)
	}

	// A root that enabled none leaves its tree in no mesh.
	plain, err := c.Create(ctx, root("plain", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Status.Mesh != "" {
		t.Fatalf("a root that enabled no mesh is in %q", plain.Status.Mesh)
	}
	quiet, err := c.Spawn(ctx, child("quiet", 0, 0), plain, 0)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Status.Mesh != "" {
		t.Fatalf("the child of a meshless root is in %q", quiet.Status.Mesh)
	}
	// Two meshes are two ids.
	if parent.Status.Mesh == plain.Status.Mesh {
		t.Fatal("two roots share one mesh id")
	}
}

// TestSpawnDebitIsAtomic: the budget is what the ledger says, not what the
// caller asks for. A parent with one unit left yields one child and refuses
// the next with ErrBudgetExhausted.
func TestSpawnDebitIsAtomic(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Spawn(ctx, child("one", 0, 0), parent, 0); err != nil {
		t.Fatal(err)
	}
	_, err = c.Spawn(ctx, child("two", 0, 0), parent, 0)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("the second spawn answered %v, want ErrBudgetExhausted", err)
	}
	// The refused spawn wrote nothing: no object, no name taken.
	if _, err = c.Get(ctx, "two", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused spawn left a sandbox behind: %v", err)
	}
	read, err := c.Get(ctx, parent.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read.Status.Spawn.Used != 1 {
		t.Fatalf("the parent has used %d, want 1", read.Status.Spawn.Used)
	}
	// A parent narrowed below what it has spent creates nothing more, with
	// no second write of the ledger.
	narrowed := parent
	narrowed.Spec.Mesh.Spawn.Budget = 0
	if _, err = c.Spawn(ctx, child("three", 0, 0), narrowed, 0); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a narrowed parent spawned: %v", err)
	}
}

// TestConcurrentSpawnsRaceForTheLastUnit: two callers that both passed the
// boundary check race at the debit and one of them loses.
func TestConcurrentSpawnsRaceForTheLastUnit(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			name := "racer-" + string(rune('a'+i))
			_, errs[i] = c.Spawn(ctx, child(name, 0, 0), parent, 0)
		})
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrBudgetExhausted):
			lost++
		default:
			t.Fatalf("a racing spawn answered %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("%d spawns won and %d were refused, want one of each", won, lost)
	}
}

// TestSpawnDepthIsTheParentsToGive: a controller whose store counts no budget
// creates no child at all, and a spawn that names no parent is refused.
func TestSpawnNeedsALedgerAndAParent(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Spawn(ctx, child("one", 0, 0), v1.Sandbox{}, 0); err == nil {
		t.Fatal("a spawn with no parent was accepted")
	}
	held := c.spawner
	c.spawner = nil
	if _, err = c.Spawn(ctx, child("one", 0, 0), parent, 0); err == nil {
		t.Fatal("a control plane that counts no budget spawned")
	}
	c.spawner = held
}

// TestFailedSpawnCreditsBack: a create that did not complete returns the unit
// it took, so a driver outage does not spend a tree's budget.
func TestFailedSpawnCreditsBack(t *testing.T) {
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	refusing := &refusingCreate{Driver: d}
	c, err := Open(Options{DataDir: t.TempDir(), Driver: refusing, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	refusing.refuse = true
	if _, err = c.Spawn(ctx, child("one", 0, 0), parent, 0); err == nil {
		t.Fatal("a spawn the driver refused was accepted")
	}
	refusing.refuse = false
	// The unit is back, so the next spawn is the first one all over again.
	if _, err = c.Spawn(ctx, child("two", 0, 0), parent, 0); err != nil {
		t.Fatalf("the refused spawn kept its unit: %v", err)
	}
}

// TestDeletedChildDoesNotCredit: the budget counts children created in total,
// so deleting one does not buy another.
func TestDeletedChildDoesNotCredit(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 1, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Spawn(ctx, child("one", 0, 0), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Act(ctx, first.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Spawn(ctx, child("two", 0, 0), parent, 0); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("a deleted child returned its unit: %v", err)
	}
}

// TestCascade: deleting a root deletes every descendant deepest generation
// first, each with reason Parent, and the root with the request's own reason.
func TestCascade(t *testing.T) {
	c, events := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 4, 2), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Spawn(ctx, child("worker", 2, 1), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Spawn(ctx, child("sibling", 0, 0), parent, 0); err != nil {
		t.Fatal(err)
	}
	grandchild, err := c.Spawn(ctx, child("helper", 0, 0), first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Act(ctx, parent.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	deleted := events.of(MutationDeleted)
	var order []string
	for _, a := range deleted {
		order = append(order, a.Object.Metadata.Name)
	}
	if len(order) != 4 || order[0] != "helper" || order[len(order)-1] != "planner" {
		t.Fatalf("the cascade deleted %v, want the deepest first and the root last", order)
	}
	for _, a := range deleted {
		want := ReasonParent
		if a.Object.Status.ID == parent.Status.ID {
			want = ""
		}
		if a.Object.Status.Reason != want {
			t.Errorf("%s was deleted with reason %q, want %q", a.Object.Metadata.Name, a.Object.Status.Reason, want)
		}
	}
	// Nothing of the tree is left.
	for _, id := range []string{parent.Status.ID, first.Status.ID, grandchild.Status.ID} {
		if _, err = c.Get(ctx, id, "alice"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s outlived the root: %v", id, err)
		}
	}
	if got := c.Descendants(parent.Status.ID); len(got) != 0 {
		t.Errorf("the tree still holds %d sandboxes", len(got))
	}
}

// TestSpawnedEvent: a successful spawn records the act on the parent, with
// the child, the tree and what is left of the budget.
func TestSpawnedEvent(t *testing.T) {
	c, events := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 2, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Spawn(ctx, child("worker", 0, 0), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	acts := events.of(MutationSpawned)
	if len(acts) != 1 {
		t.Fatalf("%d spawn records, want one", len(acts))
	}
	if acts[0].Object.Status.ID != parent.Status.ID {
		t.Fatalf("the record is about %s, want the parent", acts[0].Object.Status.ID)
	}
	data, ok := acts[0].Data.(SpawnedData)
	if !ok {
		t.Fatalf("the record carries %T", acts[0].Data)
	}
	want := SpawnedData{Child: first.Status.ID, Root: parent.Status.ID, BudgetLeft: 1}
	if data != want {
		t.Fatalf("the record says %+v, want %+v", data, want)
	}
}

// TestParentCannotBeNarrowedBelowAChild: an update that would leave a live
// descendant outside its parent's boundary is refused, naming the descendant.
func TestParentCannotBeNarrowedBelowAChild(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	wide := root("planner", 2, 1)
	wide.Spec.Resources = v1.Resources{CPU: "2", Memory: "4Gi"}
	parent, err := c.Create(ctx, wide, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	narrow := child("worker", 0, 0)
	narrow.Spec.Resources = v1.Resources{CPU: "1", Memory: "2Gi"}
	first, err := c.Spawn(ctx, narrow, parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Narrowing to an envelope the child still fits inside is accepted.
	kept := parent
	kept.Spec.Resources.CPU = "1"
	if err = c.NarrowingRefusal(kept); err != nil {
		t.Fatalf("a narrowing the child still fits inside was refused: %v", err)
	}
	// Narrowing below it is refused, and the error names the child.
	below := parent
	below.Spec.Resources.CPU = "500m"
	err = c.NarrowingRefusal(below)
	if err == nil || !strings.Contains(err.Error(), first.Status.ID) {
		t.Fatalf("narrowing below a live child answered %v, want a refusal naming %s", err, first.Status.ID)
	}
}

// refusingCreate is a driver whose Create can be made to refuse, so the undo
// of design 005's create order is reachable.
type refusingCreate struct {
	driver.Driver
	refuse bool
}

func (d *refusingCreate) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	if d.refuse {
		return driver.Ref{}, errors.New("runtime outage")
	}
	return d.Driver.Create(ctx, s)
}

// TestATreeNeverAdoptsAnEntry: a sandbox's place in its tree is create-time
// identity the driver stamps on objects it cannot rewrite, and an adoption
// writes only the half a mutation may reach. A spawn and a mesh root
// therefore take the slow path, so no member of a mesh is a member the
// substrate does not know about.
func TestATreeNeverAdoptsAnEntry(t *testing.T) {
	c, d, _ := newPool(t, poolOptions(2))
	ctx := t.Context()
	if _, err := c.Refill(ctx); err != nil {
		t.Fatal(err)
	}
	// A root that joins a mesh is created outright, with the entries left
	// where they are.
	meshRoot := root("planner", 2, 1)
	meshRoot.Spec.Mesh.Enabled = true
	parent, err := c.Create(ctx, meshRoot, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.adopted() != 0 {
		t.Fatalf("a mesh root adopted %d entries, want none", d.adopted())
	}
	if _, err = c.Spawn(ctx, child("worker", 0, 0), parent, 0); err != nil {
		t.Fatal(err)
	}
	if d.adopted() != 0 {
		t.Fatalf("a spawn adopted %d entries, want none", d.adopted())
	}
	// A sandbox that is neither still takes one, so the rule narrows the
	// acceleration and does not end it.
	if _, err = c.Create(ctx, workspace(), "alice", 0); err != nil {
		t.Fatal(err)
	}
	if d.adopted() != 1 {
		t.Fatalf("an ordinary create adopted %d entries, want one", d.adopted())
	}
}

// TestADeadlineEndsTheTree: the reaper's delete cascades the way a request's
// does, so a parent that expired or was auto-deleted leaves no descendant
// behind.
func TestADeadlineEndsTheTree(t *testing.T) {
	c, _ := spawning(t)
	ctx := t.Context()
	parent, err := c.Create(ctx, root("planner", 2, 1), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Spawn(ctx, child("worker", 1, 1), parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := c.Spawn(ctx, child("helper", 0, 0), first, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	err = c.deleteLocked(ctx, parent.Status.ID, ReasonExpired)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent.Status.ID, first.Status.ID, grandchild.Status.ID} {
		if _, err = c.Get(ctx, id, "alice"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s outlived the deadline that ended its ancestor: %v", id, err)
		}
	}
}
