// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"slices"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// ErrNoEnvironment is a call about a sandbox whose environment this control
// plane does not hold. Design 008 answers it not_found at spec.environment.
var ErrNoEnvironment = errors.New("no environment of that name")

// driverFor is the one seam every call reaches a driver through: the driver
// of the environment named, which is spec.environment while a sandbox is
// being created and status.environment for every act after that.
//
// It takes the registry's read lock and never the controller's, so a caller
// that already holds the controller's lock reaches the same lookup as one
// that does not. Where both are wanted the order is the controller's first.
func (c *Controller) driverFor(environment string) (driver.Driver, error) {
	c.envMu.RLock()
	defer c.envMu.RUnlock()
	d, held := c.drivers[environment]
	if !held {
		return nil, fmt.Errorf("%w: %s", ErrNoEnvironment, environment)
	}
	return d, nil
}

// driverOf is driverFor over the environment one sandbox is on. A sandbox
// this control plane does not track is on the environment cellad drives
// itself: a driver object with no desired record is one this process made.
//
// It reads the object under the controller's lock, so no caller holding that
// lock calls it.
func (c *Controller) driverOf(id string) (driver.Driver, error) {
	c.mu.Lock()
	obj, held := c.objects[id]
	c.mu.Unlock()
	if !held || obj.Status.Environment == "" {
		return c.driverFor(c.environment)
	}
	// No driver holds a queued sandbox, so there is nothing to act on
	// until the scheduler places it.
	if obj.Status.Phase == PhaseQueued {
		return nil, ErrPhase
	}
	return c.driverFor(obj.Status.Environment)
}

// environmentOf is the stored object of one environment, and false where the
// registry does not hold it.
func (c *Controller) environmentOf(name string) (v1.Environment, bool) {
	c.envMu.RLock()
	defer c.envMu.RUnlock()
	obj, held := c.environments[name]
	return obj, held
}

// Isolation is what the environment cellad drives itself confines with. It is
// the answer a caller that names no environment gets; IsolationOf answers for
// one named.
func (c *Controller) Isolation() string { return c.IsolationOf(c.environment) }

// DriverName is the driver of the environment cellad drives itself.
func (c *Controller) DriverName() string { return c.DriverNameOf(c.environment) }

// Capabilities are what the environment cellad drives itself provides.
func (c *Controller) Capabilities() driver.Capabilities {
	return c.CapabilitiesOf(c.environment)
}

// IsolationOf is the isolation class one environment's driver provides, and
// none for an environment this control plane does not hold.
func (c *Controller) IsolationOf(environment string) string {
	d, err := c.driverFor(environment)
	if err != nil {
		return driver.IsolationNone
	}
	return d.Isolation()
}

// DriverNameOf is the driver serving one environment, and the empty string
// for an environment this control plane does not hold.
func (c *Controller) DriverNameOf(environment string) string {
	d, err := c.driverFor(environment)
	if err != nil {
		return ""
	}
	return d.Name()
}

// CapabilitiesOf is what one environment's driver provides. An environment
// this control plane does not hold provides nothing, so every capability
// gate refuses rather than reaching a driver that is not there.
func (c *Controller) CapabilitiesOf(environment string) driver.Capabilities {
	d, err := c.driverFor(environment)
	if err != nil {
		return driver.Capabilities{}
	}
	return d.Capabilities()
}

// environmentsHeld is every environment this control plane holds an object
// for, in the order a list returns them. It is what the phase loop passes
// over; the driver registry may hold fewer, because an environment whose
// driver could not be built is still an object an operator applied.
func (c *Controller) environmentsHeld() []string {
	c.envMu.RLock()
	defer c.envMu.RUnlock()
	out := make([]string, 0, len(c.environments))
	for name := range c.environments {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// placeable is every environment the loops act on: the ones whose phase is
// Ready. Spec 021 holds the reaper and the pool on an environment below
// Ready, because a data plane reporting nothing is not a data plane
// reporting an empty world. An environment with no stored object yet is the
// one cellad opened for itself before its object was written.
func (c *Controller) placeable() []string {
	c.envMu.RLock()
	defer c.envMu.RUnlock()
	out := make([]string, 0, len(c.drivers))
	for name := range c.drivers {
		if obj, held := c.environments[name]; held && obj.Status.Phase != v1.EnvironmentReady {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// poolOf is the prewarmed set one environment keeps and the ceiling its
// entries count against. A control plane that holds no object for the
// environment it drives itself reads the two from its own configuration,
// which is what seeds that object.
func (c *Controller) poolOf(environment string) (v1.PoolSpec, int) {
	if obj, held := c.environmentOf(environment); held {
		return obj.Spec.Pool, obj.Spec.Capacity.Sandboxes
	}
	if environment == c.environment {
		return c.pool, c.capacity
	}
	return v1.PoolSpec{}, 0
}

// setDriver puts one environment's driver in the registry, replacing what it
// held. It is the registry's write half: the apply of an environment builds a
// driver for it, and a delete drops one.
func (c *Controller) setDriver(environment string, d driver.Driver) {
	c.envMu.Lock()
	defer c.envMu.Unlock()
	c.drivers[environment] = d
}
