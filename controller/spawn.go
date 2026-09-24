// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"sort"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// ErrBudgetExhausted is a spawn against a sandbox that has already created
// every child its budget allows. Design 008 answers it with 422
// spawn_budget_exhausted.
var ErrBudgetExhausted = errors.New("the sandbox has no spawn budget left")

// Spawner is the spawn ledger of design 022 as the controller reads it. A
// store that implements it enforces the budget; one that does not creates no
// child at all, because a budget nothing counts is no budget.
//
// The budget travels with the debit rather than living in a row of its own:
// it is desired state, so a root narrowed after its children exist takes
// effect at the next debit with no second write.
type Spawner interface {
	// WriteSpawn writes the child and debits the parent in one transaction.
	// ErrBudgetExhausted leaves both untouched.
	WriteSpawn(ctx context.Context, obj v1.Sandbox, mutation, parentID string, budget int) error
	// CreditSpawn returns one unit to a parent whose child never started.
	CreditSpawn(ctx context.Context, parentID string) error
	// SpawnsUsed is how many children the parent has created in total.
	SpawnsUsed(ctx context.Context, parentID string) (int, error)
	// ForgetSpawns drops one sandbox's row at the delete that ends it.
	ForgetSpawns(ctx context.Context, parentID string) error
}

// Journaller is the half of design 010's journal an act with a payload of its
// own needs: a record whose data no field of the object carries, appended
// without a desired write. The spawn of design 022 is its one caller, because
// the record is about the parent and names the child.
type Journaller interface {
	WriteRecord(ctx context.Context, obj v1.Sandbox, mutation string, data any) error
}

// MutationSpawned is the act a workload took when it created a child. The
// record is about the parent; the child's own create is a separate object's
// record and is unordered relative to it.
const MutationSpawned = "sandbox.spawned"

// Spawn is a create whose actor is the parent sandbox's own workload token.
// It runs design 005's create order with three additions: the child inherits
// the parent's root and mesh, the parent's budget is debited in the same
// transaction that writes the child, and a successful create records
// sandbox.spawned on the parent.
//
// The manifest was already held to the parent's boundary at resolve
// ([[003-manifest-contract]] stage 6). This is the gate resolve cannot be:
// two callers that both passed the check race for the last unit here, and one
// of them loses.
func (c *Controller) Spawn(ctx context.Context, obj v1.Sandbox, parent v1.Sandbox, max int) (v1.Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if parent.Status.ID == "" {
		return obj, errors.New("a spawn names a parent sandbox")
	}
	if c.spawner == nil {
		return obj, errors.New("this control plane counts no spawn budget, so a sandbox creates no child")
	}
	return c.create(ctx, obj, parent.Status.Owner, max, &parent)
}

// spawnStatus is the tree position and budget the controller writes on a new
// sandbox: its parent and root, the mesh it inherits or the one this create
// mints, and the two axes its own manifest declared.
func (c *Controller) spawnStatus(obj *v1.Sandbox, parent *v1.Sandbox) error {
	obj.Status.Spawn = v1.SpawnStatus{
		Budget: obj.Spec.Mesh.Spawn.Budget,
		Depth:  obj.Spec.Mesh.Spawn.Depth,
	}
	if parent == nil {
		// A sandbox a subject applied is its own root, and a root that asked
		// for a mesh is where the id is minted.
		obj.Status.Root = obj.Status.ID
		if obj.Spec.Mesh.Enabled {
			id, err := newMeshID()
			if err != nil {
				return err
			}
			obj.Status.Mesh = id
		}
		return nil
	}
	obj.Status.Parent = parent.Status.ID
	obj.Status.Root = parent.Status.Root
	obj.Status.Mesh = parent.Status.Mesh
	return nil
}

// debit records the child against its parent in the transaction that writes
// the child. The budget is read from the parent's desired state, so a parent
// narrowed since its last child is judged by what it declares now.
func (c *Controller) debit(ctx context.Context, obj v1.Sandbox, parent *v1.Sandbox) error {
	if parent == nil {
		return c.persist(ctx, obj, MutationCreated)
	}
	id := obj.Status.ID
	previous, held := c.objects[id]
	c.objects[id] = clone(obj)
	err := c.spawner.WriteSpawn(ctx, clone(obj), MutationCreated, parent.Status.ID, parent.Spec.Mesh.Spawn.Budget)
	if err != nil {
		if held {
			c.objects[id] = previous
		} else {
			delete(c.objects, id)
		}
		return err
	}
	// A snapshot store has no journal of its own, so the emitter takes the
	// act here, as it does for every other write.
	if c.durable == nil {
		c.emit(ctx, MutationCreated, clone(obj))
	}
	// The parent reads the debit at once, since the child's create answers
	// before the loop realizes it and records the spawn.
	return c.projectSpawnUsed(ctx, parent.Status.ID)
}

// credit returns the unit a create that did not complete took. It is called
// on every undo path of design 005's create order; a child deleted later
// credits nothing, because the budget counts children created in total.
func (c *Controller) credit(ctx context.Context, parent *v1.Sandbox) error {
	if parent == nil || c.spawner == nil {
		return nil
	}
	if err := c.spawner.CreditSpawn(ctx, parent.Status.ID); err != nil {
		return err
	}
	return c.projectSpawnUsed(ctx, parent.Status.ID)
}

// projectSpawnUsed writes the ledger's count onto the parent's status, so a
// reader of the sandbox and the row that gates the next spawn are one count.
func (c *Controller) projectSpawnUsed(ctx context.Context, parentID string) error {
	obj, held := c.objects[parentID]
	if !held || c.spawner == nil {
		return nil
	}
	used, err := c.spawner.SpawnsUsed(ctx, parentID)
	if err != nil {
		return err
	}
	if obj.Status.Spawn.Used == used {
		return nil
	}
	obj.Status.Spawn.Used = used
	return c.persist(ctx, obj, MutationStatus)
}

// spawned records the act on the parent: the child, the tree they share, and
// what is left of the budget. The record carries a payload no field of the
// parent holds, so it goes through the journal seam rather than a desired
// write.
func (c *Controller) spawned(ctx context.Context, child v1.Sandbox, parentID string) error {
	if err := c.projectSpawnUsed(ctx, parentID); err != nil {
		return err
	}
	parent, held := c.objects[parentID]
	if !held {
		return nil
	}
	data := spawnedData(child, parent)
	if c.journaller != nil {
		return c.journaller.WriteRecord(ctx, clone(parent), MutationSpawned, data)
	}
	c.emitData(ctx, MutationSpawned, clone(parent), data)
	return nil
}

// SpawnedData is design 009's payload for the act: the child a workload
// created, the tree both belong to, and what is left of the parent's budget
// after the debit. The record's object is the parent, so the parent is named
// by the record and not by its data.
type SpawnedData struct {
	Child      string `json:"child"`
	Root       string `json:"root"`
	BudgetLeft int    `json:"budgetLeft"`
}

// spawnedData builds that payload, here so the durable and the snapshot paths
// carry one shape.
func spawnedData(child, parent v1.Sandbox) SpawnedData {
	return SpawnedData{
		Child:      child.Status.ID,
		Root:       parent.Status.Root,
		BudgetLeft: max(parent.Status.Spawn.Budget-parent.Status.Spawn.Used, 0),
	}
}

// Descendants is one sandbox's tree below it, deepest generation first, which
// is the order the cascade of design 022 deletes in. The tree is read from
// desired state, so no driver is asked what belongs to what.
func (c *Controller) Descendants(id string) []v1.Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.descendants(id)
}

func (c *Controller) descendants(id string) []v1.Sandbox {
	children := map[string][]string{}
	for objID, obj := range c.objects {
		if obj.Status.Parent != "" {
			children[obj.Status.Parent] = append(children[obj.Status.Parent], objID)
		}
	}
	depth := map[string]int{}
	var walk func(parent string, level int)
	walk = func(parent string, level int) {
		ids := slices.Clone(children[parent])
		sort.Strings(ids)
		for _, child := range ids {
			if _, seen := depth[child]; seen {
				// A cycle cannot arise from the create path, which writes a
				// parent that already exists. Stopping here keeps a corrupt
				// snapshot from walking forever rather than pretending the
				// shape is impossible.
				continue
			}
			depth[child] = level
			walk(child, level+1)
		}
	}
	walk(id, 1)
	out := make([]v1.Sandbox, 0, len(depth))
	for child := range depth {
		out = append(out, c.objects[child])
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Status.ID, out[j].Status.ID
		if depth[a] != depth[b] {
			return depth[a] > depth[b]
		}
		return a < b
	})
	return out
}

// Tree is one root's sandboxes, the root included, which is what
// GET /v1/sandboxes?root= returns.
func (c *Controller) Tree(root string) []v1.Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []v1.Sandbox{}
	for _, obj := range c.objects {
		if obj.Status.Root == root {
			out = append(out, export(clone(obj)))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status.ID < out[j].Status.ID })
	return out
}

// NarrowingRefusal is the update half of design 022's boundary rule: an
// update that would leave a live descendant outside its parent's boundary is
// refused, naming the descendant. It is here rather than in the API because
// the controller owns the tree.
func (c *Controller) NarrowingRefusal(updated v1.Sandbox) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now().UTC()
	for _, child := range c.descendants(updated.Status.ID) {
		if child.Status.Parent != updated.Status.ID {
			continue
		}
		if err := manifest.DescendantOutsideBoundary(&updated, &child, now); err != nil {
			return err
		}
	}
	return nil
}

// newMeshID mints the id of one mesh: the prefix of design 022 and a ULID, so
// ids sort by the instant the mesh was made and no two collide.
func newMeshID() (string, error) {
	raw, err := newULID()
	if err != nil {
		return "", err
	}
	return manifest.MeshIDPrefix + crockfordULID(raw), nil
}
