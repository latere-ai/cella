// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The JSON paths of the ceiling, named once so a refusal and a test point at
// the same string.
const (
	pathResourcesDisk = pathResources + ".disk"
	pathMaxCPU        = pathResources + ".max.cpu"
	pathMaxMemory     = pathResources + ".max.memory"
	pathMaxDisk       = pathResources + ".max.disk"
	pathPoolMax       = "spec.pool.resources.max"
)

// ceilingField is one resource of spec.resources.max beside the amount it
// starts at and what the sandbox holds of it now.
type ceilingField struct {
	path     string
	name     string
	max      v1.Quantity
	floor    v1.Quantity
	autosize bool
}

// ceilingFields gives the three ceilings a fixed order, cpu, memory, disk,
// so a manifest with two bad ceilings always names the same one.
func ceilingFields(r v1.Resources, a v1.Autosize) []ceilingField {
	return []ceilingField{
		{pathMaxCPU, "cpu", r.Max.CPU, r.CPU, a.CPU},
		{pathMaxMemory, "memory", r.Max.Memory, r.Memory, a.Memory},
		{pathMaxDisk, "disk", r.Max.Disk, r.Disk, a.Disk},
	}
}

// sameAmounts reports whether two manifests ask for the same starting size.
// The ceiling is compared apart: it is changed under another capability.
func sameAmounts(a, b v1.Resources) bool {
	return a.CPU == b.CPU && a.Memory == b.Memory && a.Disk == b.Disk
}

// validateCeiling is the syntax of spec.resources.max: each ceiling that is
// set is a positive quantity. Whether it is above the amount it bounds is
// read after defaulting, when that amount is known.
func validateCeiling(r v1.Resources) error {
	for _, f := range ceilingFields(r, v1.Autosize{}) {
		if f.max == "" {
			continue
		}
		amount, err := ParseQuantity(f.max)
		if err != nil {
			return failAt("invalid_field", f.path, upperFirst(err.Error())+".")
		}
		if amount <= 0 {
			return failAt("invalid_field", f.path, "The amount must be positive.")
		}
	}
	return nil
}

// ceilingFloor holds each ceiling at or above the amount the sandbox starts
// at. A ceiling below it would ask for a sandbox that starts above its own
// bound.
func ceilingFloor(r v1.Resources) error {
	for _, f := range ceilingFields(r, v1.Autosize{}) {
		if f.max == "" || f.floor == "" {
			continue
		}
		limit, err := ParseQuantity(f.max)
		if err != nil {
			return failAt("invalid_field", f.path, upperFirst(err.Error())+".")
		}
		floor, err := ParseQuantity(f.floor)
		if err != nil {
			return failAt("invalid_field", f.path, upperFirst(err.Error())+".")
		}
		if limit < floor {
			return failAt("invalid_field", f.path, fmt.Sprintf("The ceiling of %s is below the %s the sandbox starts at.", f.max, f.floor))
		}
	}
	return nil
}

// ceilingHeld refuses an update that would put the ceiling of memory or of
// the workspace below what the sandbox holds now. A claim cannot shrink and
// a running process's memory cannot be taken back, so the size the sandbox
// reached is the floor of every later ceiling. Removing the ceiling reads as
// lowering it to the amount the sandbox starts at. CPU is compressible and
// its limit follows the field, so it is not held.
func ceilingHeld(existing, obj *v1.Sandbox) error {
	held := existing.Status.Resources
	for _, f := range []struct {
		path       string
		max, floor v1.Quantity
		held       v1.Quantity
	}{
		{pathMaxMemory, obj.Spec.Resources.Max.Memory, obj.Spec.Resources.Memory, held.Memory},
		{pathMaxDisk, obj.Spec.Resources.Max.Disk, obj.Spec.Resources.Disk, held.Disk},
	} {
		bound := f.max
		if bound == "" {
			bound = f.floor
		}
		if f.held == "" || bound == "" {
			continue
		}
		limit, err := ParseQuantity(bound)
		if err != nil {
			return failAt("invalid_field", f.path, upperFirst(err.Error())+".")
		}
		now, err := ParseQuantity(f.held)
		if err != nil {
			// The size is the control plane's own record. One it cannot
			// read bounds nothing, as an absent one does.
			continue
		}
		if limit < now {
			return failAt("ceiling_exceeded", f.path, fmt.Sprintf("The ceiling of %s is below the %s the sandbox holds now.", bound, f.held))
		}
	}
	return nil
}

// ceilingCapability refuses a ceiling on a resource the environment's driver
// holds fixed. Accepting it would record a bound nothing grows toward.
func ceilingCapability(obj *v1.Sandbox, env *v1.Environment) error {
	for _, f := range ceilingFields(obj.Spec.Resources, env.Status.Capabilities.Autosize) {
		if f.max != "" && !f.autosize {
			return failAt("capability_unsupported", f.path, "This environment does not size a sandbox's "+f.name+" inside a ceiling.")
		}
	}
	return nil
}

// ceilingOf is the most of one resource a sandbox may come to hold: its
// ceiling, or the amount it is fixed at.
func ceilingOf(max, amount v1.Quantity) v1.Quantity {
	if max != "" {
		return max
	}
	return amount
}
