// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"errors"

	"latere.ai/x/cella/runtime"
)

// changesSizes reports whether the driver declares any way to change a
// sandbox's size after create: Resize, or an Autosize flag for one of the two
// resources a change of size carries.
func changesSizes(c runtime.Capabilities) bool {
	return c.Resize || c.Autosize.CPU || c.Autosize.Memory
}

// sizeChangeIsHonoredOrRefused holds Change.Resources to its declaration in
// both directions. A driver that declares no way to change a size refuses the
// field, so a size is never accepted and dropped. One that declares a way
// takes the change, and a start after a stop runs the sandbox at the size it
// was last asked to hold, which State.Resources reports.
func sizeChangeIsHonoredOrRefused(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_size"
	first := runtime.Resources{CPU: "100m", Memory: "64Mi"}
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "size", Owner: "alice", Resources: first})
	next := runtime.Resources{CPU: "100m", CPULimit: "200m", Memory: "128Mi"}
	err := d.Update(ctx, id, runtime.Change{Resources: &next})
	if !changesSizes(d.Capabilities()) {
		expect(t, errors.Is(err, runtime.ErrUnsupported),
			"a change of size on a driver that declares none: got %v, want ErrUnsupported", err)
		return
	}
	must(t, err, "Update with a size the driver declares it changes")
	must(t, d.Stop(ctx, id), "Stop")
	waitPhase(t, d, id, runtime.Stopped)
	must(t, d.Start(ctx, id), "Start")
	state := waitPhase(t, d, id, runtime.Running)
	expect(t, state.Resources.Memory == next.Memory && state.Resources.CPULimit == next.CPULimit,
		"a start after a change of size holds %+v, want the size last asked for, %+v", state.Resources, next)
	expect(t, state.Resize == nil, "a sandbox started at the size asked for still reports %+v outstanding", state.Resize)
	// The workspace is not resized by a change of size.
	next.Disk = "64Gi"
	err = d.Update(ctx, id, runtime.Change{Resources: &next})
	expect(t, errors.Is(err, runtime.ErrUnsupported), "a change of size naming another disk: got %v, want ErrUnsupported", err)
}
