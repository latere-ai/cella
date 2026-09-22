// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"slices"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The paths of spec.scheduling, in the order of the field table.
const (
	pathSchedulingPriority    = "spec.scheduling.priority"
	pathSchedulingQueue       = "spec.scheduling.queue"
	pathSchedulingDeadline    = "spec.scheduling.startDeadline"
	pathSchedulingPreemptible = "spec.scheduling.preemptible"
)

// Queued reports whether an environment admits against its capacity through
// queues rather than starting a sandbox now or failing it. An environment
// that names no mode is direct, which is the default of spec 021.
func Queued(env v1.Environment) bool {
	return env.Spec.Scheduling.Mode == v1.SchedulingQueued
}

// QueuesOf is the queues an environment declares, and the one queue every
// environment has where it declares none.
func QueuesOf(env v1.Environment) []string {
	if len(env.Spec.Scheduling.Queues) == 0 {
		return []string{v1.DefaultQueueName}
	}
	return env.Spec.Scheduling.Queues
}

// defaultQueue fills spec.scheduling.queue on a queued environment, with the
// queue the environment names as its default. A direct environment has no
// queue to join, so the field stays as the manifest wrote it and the
// capability stage refuses it if it was set.
func defaultQueue(obj *v1.Sandbox, env *v1.Environment) {
	if !Queued(*env) || obj.Spec.Scheduling.Queue != "" {
		return
	}
	obj.Spec.Scheduling.Queue = env.Spec.Scheduling.DefaultQueue
	if obj.Spec.Scheduling.Queue == "" {
		obj.Spec.Scheduling.Queue = QueuesOf(*env)[0]
	}
}

// schedulingCapability holds spec.scheduling to the environment's mode. A
// direct environment starts a sandbox now or fails it, so every field is one
// it cannot honour; a queued one reads them all, and the queue must be one it
// declares.
func schedulingCapability(obj *v1.Sandbox, env *v1.Environment) error {
	s := obj.Spec.Scheduling
	if !Queued(*env) {
		for _, f := range []struct {
			path string
			set  bool
		}{
			{pathSchedulingPriority, s.Priority != 0},
			{pathSchedulingQueue, s.Queue != ""},
			{pathSchedulingDeadline, s.StartDeadline != ""},
			{pathSchedulingPreemptible, s.Preemptible},
		} {
			if f.set {
				return failAt("capability_unsupported", f.path, "This environment starts a sandbox now or fails it, so it has no queue to wait in.")
			}
		}
		return nil
	}
	if !slices.Contains(QueuesOf(*env), s.Queue) {
		return failAt("invalid_field", pathSchedulingQueue, fmt.Sprintf("The environment has no queue named %s.", s.Queue))
	}
	return nil
}

// priorityCeiling holds spec.scheduling.priority to what the authorizer
// granted this caller. Zero grants any priority; a negative limit is one no
// manifest can meet, and it is refused rather than ignored.
func priorityCeiling(priority, limit int) error {
	if limit < 0 || (limit > 0 && priority > limit) {
		return failAt("ceiling_exceeded", pathSchedulingPriority,
			fmt.Sprintf("The priority of %d is above the limit this caller was granted.", priority))
	}
	return nil
}

// schedulingChanges names the scheduling fields an update changed. Every one
// is immutable: a sandbox's place in line is decided once, at create.
func schedulingChanges(existing, obj v1.Scheduling) []string {
	var paths []string
	for _, f := range []struct {
		path    string
		changed bool
	}{
		{pathSchedulingPriority, existing.Priority != obj.Priority},
		{pathSchedulingQueue, existing.Queue != obj.Queue},
		{pathSchedulingDeadline, existing.StartDeadline != obj.StartDeadline},
		{pathSchedulingPreemptible, existing.Preemptible != obj.Preemptible},
	} {
		if f.changed {
			paths = append(paths, f.path)
		}
	}
	return paths
}
