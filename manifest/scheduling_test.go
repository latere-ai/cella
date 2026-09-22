// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"slices"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// queuedEnvironment is a container environment in the queued mode with two
// queues, the second its default, so a defaulted queue is told apart from the
// first one declared.
func queuedEnvironment() v1.Environment {
	env := container("default")
	env.Spec.Scheduling = v1.SchedulingSpec{
		Mode: v1.SchedulingQueued, Queues: []string{"default", "rollouts"}, DefaultQueue: "rollouts",
	}
	return env
}

// TestSchedulingFields is the scheduling field table of spec 057: every row
// refuses with its code at its path, and a queued environment gives a sandbox
// that names no queue the one it declares as its default.
func TestSchedulingFields(t *testing.T) {
	queued := environmentOptions(queuedEnvironment())
	direct := environmentOptions(container("default"))
	limited := environmentOptions(queuedEnvironment())
	limited.Limits.MaxPriority = 3
	barred := environmentOptions(queuedEnvironment())
	barred.Limits.MaxPriority = -1
	for _, tc := range []struct {
		name       string
		scheduling v1.Scheduling
		options    Options
		code, path string
	}{
		{"a priority on a direct environment", v1.Scheduling{Priority: 1}, direct, "capability_unsupported", pathSchedulingPriority},
		{"a queue on a direct environment", v1.Scheduling{Queue: "default"}, direct, "capability_unsupported", pathSchedulingQueue},
		{"a deadline on a direct environment", v1.Scheduling{StartDeadline: "10m"}, direct, "capability_unsupported", pathSchedulingDeadline},
		{"preemptible on a direct environment", v1.Scheduling{Preemptible: true}, direct, "capability_unsupported", pathSchedulingPreemptible},
		{"a negative priority", v1.Scheduling{Priority: -1}, queued, "invalid_field", pathSchedulingPriority},
		{"a priority above the caller's limit", v1.Scheduling{Priority: 4}, limited, "ceiling_exceeded", pathSchedulingPriority},
		{"any priority under a limit no manifest meets", v1.Scheduling{}, barred, "ceiling_exceeded", pathSchedulingPriority},
		{"a queue the environment does not declare", v1.Scheduling{Queue: "batch"}, queued, "invalid_field", pathSchedulingQueue},
		{"a queue name that is not a label", v1.Scheduling{Queue: "Roll Outs"}, queued, "invalid_field", pathSchedulingQueue},
		{"a deadline that is not a duration", v1.Scheduling{StartDeadline: "soon"}, queued, "invalid_field", pathSchedulingDeadline},
		{"a deadline of never", v1.Scheduling{StartDeadline: v1.DurationNever}, queued, "invalid_field", pathSchedulingDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			obj.Spec.Scheduling = tc.scheduling
			_, err := Resolve(t.Context(), &obj, tc.options)
			var known *Error
			if !errors.As(err, &known) {
				t.Fatalf("Resolve = %v, want %s at %s", err, tc.code, tc.path)
			}
			if known.Code != tc.code || known.Path != tc.path {
				t.Fatalf("Resolve = %s at %s, want %s at %s", known.Code, known.Path, tc.code, tc.path)
			}
		})
	}

	// A queued environment defaults the queue and reads the rest as written.
	obj := sandbox()
	obj.Spec.Scheduling = v1.Scheduling{Priority: 3, StartDeadline: "10m", Preemptible: true}
	got := resolve(t, obj, limited).Sandbox.Spec.Scheduling
	if got != (v1.Scheduling{Priority: 3, Queue: "rollouts", StartDeadline: "10m", Preemptible: true}) {
		t.Fatalf("the scheduling resolved to %+v", got)
	}
	// A queued environment that names no default takes its first queue, and
	// one that declares no queue has the one every environment has.
	first := queuedEnvironment()
	first.Spec.Scheduling.DefaultQueue = ""
	if q := resolve(t, sandbox(), environmentOptions(first)).Sandbox.Spec.Scheduling.Queue; q != "default" {
		t.Fatalf("with no default the queue is %q", q)
	}
	bare := queuedEnvironment()
	bare.Spec.Scheduling = v1.SchedulingSpec{Mode: v1.SchedulingQueued}
	if q := resolve(t, sandbox(), environmentOptions(bare)).Sandbox.Spec.Scheduling.Queue; q != v1.DefaultQueueName {
		t.Fatalf("with no queue declared the queue is %q", q)
	}
	// A direct environment leaves an absent scheduling absent.
	if s := resolve(t, sandbox(), direct).Sandbox.Spec.Scheduling; s != (v1.Scheduling{}) {
		t.Fatalf("a direct environment wrote %+v", s)
	}
}

// TestSchedulingIsImmutable: a sandbox's place in line is decided at create,
// so an update that moves any scheduling field names every one it moved.
func TestSchedulingIsImmutable(t *testing.T) {
	o := environmentOptions(queuedEnvironment())
	created := sandbox()
	created.Spec.Scheduling = v1.Scheduling{Priority: 2, Queue: "default", StartDeadline: "5m"}
	existing := resolve(t, created, o).Sandbox

	same := created
	o.Existing = &existing
	if _, err := Resolve(t.Context(), &same, o); err != nil {
		t.Fatalf("an update that keeps the scheduling was refused: %v", err)
	}
	moved := created
	moved.Spec.Scheduling = v1.Scheduling{Priority: 1, Queue: "rollouts", StartDeadline: "6m", Preemptible: true}
	_, err := Resolve(t.Context(), &moved, o)
	var known *Error
	if !errors.As(err, &known) || known.Code != "immutable_field" {
		t.Fatalf("moving the scheduling answered %v", err)
	}
	want := []string{pathSchedulingPriority, pathSchedulingQueue, pathSchedulingDeadline, pathSchedulingPreemptible}
	if !slices.Equal(known.Paths, want) {
		t.Fatalf("the refusal names %v, want %v", known.Paths, want)
	}
}
