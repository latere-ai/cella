// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"fmt"
	"net/http"
)

// lifecycleCases prove the transitions the API reaches and the refusals a
// phase makes, including the rule that a delete is accepted in every phase.
func lifecycleCases() []Case {
	return []Case{
		{"lifecycle", "case005CreateReachesRunning", case005CreateReachesRunning},
		{"lifecycle", "case005StopAndStart", case005StopAndStart},
		{"lifecycle", "case005PhaseConflict", case005PhaseConflict},
		{"lifecycle", "case005DeleteInEveryPhase", case005DeleteInEveryPhase},
		{"lifecycle", "case008CreateWait", case008CreateWait},
	}
}

// case005CreateReachesRunning: an applied sandbox reaches Running without
// anything else being asked for. The create answers as soon as the sandbox
// is recorded, in whatever phase it holds then, and the await reads it to
// Running.
func case005CreateReachesRunning(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	if obj.Status.Phase != "Running" {
		return fmt.Errorf("the sandbox is %s, want Running", obj.Status.Phase)
	}
	return nil
}

// case005StopAndStart: the two verbs move the sandbox between Running and
// Stopped, and each answers the object it left behind.
func case005StopAndStart(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	x, err := e.caller.post(ctx, base+"/stop", nil)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	if _, err = e.await(ctx, e.caller, obj.Status.ID, "Stopped"); err != nil {
		return err
	}
	x, err = e.caller.post(ctx, base+"/start", nil)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	_, err = e.await(ctx, e.caller, obj.Status.ID, "Running")
	return err
}

// case005PhaseConflict: a verb the phase does not allow is refused with the
// code of the table and changes nothing.
func case005PhaseConflict(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	if _, err := e.caller.post(ctx, base+"/stop", nil); err != nil {
		return err
	}
	if _, err = e.await(ctx, e.caller, obj.Status.ID, "Stopped"); err != nil {
		return err
	}
	x, err := e.caller.post(ctx, base+"/stop", nil)
	if err != nil {
		return err
	}
	if err := x.refusal("phase_conflict"); err != nil {
		return err
	}
	after, err := e.await(ctx, e.caller, obj.Status.ID, "Stopped")
	if err != nil {
		return err
	}
	if after.Status.Phase != "Stopped" {
		return fmt.Errorf("the refused verb left the sandbox %s, want Stopped", after.Status.Phase)
	}
	return nil
}

// case005DeleteInEveryPhase: a delete is accepted whatever the phase is, and
// answers the object in Deleting.
func case005DeleteInEveryPhase(ctx context.Context, e *Env) error {
	running, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	stopped, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	if _, err := e.caller.post(ctx, "/v1/sandboxes/"+stopped.Status.ID+"/stop", nil); err != nil {
		return err
	}
	if _, err = e.await(ctx, e.caller, stopped.Status.ID, "Stopped"); err != nil {
		return err
	}
	for phase, id := range map[string]string{"Running": running.Status.ID, "Stopped": stopped.Status.ID} {
		x, err := e.caller.del(ctx, "/v1/sandboxes/"+id)
		if err != nil {
			return err
		}
		if err := x.status(http.StatusAccepted); err != nil {
			return fmt.Errorf("deleting a %s sandbox: %w", phase, err)
		}
		obj, err := x.object()
		if err != nil {
			return err
		}
		if obj.Status.Phase != "Deleting" {
			return x.disagree("the object in Deleting", "phase "+obj.Status.Phase)
		}
	}
	return nil
}

// case008CreateWait: a create that asks for its answer held, with ?wait=1,
// answers 201 once the sandbox has started, with the object Running and its
// Location. Without the hold a create answers as soon as the sandbox is
// recorded, which every other case reads through its await.
func case008CreateWait(ctx context.Context, e *Env) error {
	x, err := e.caller.post(ctx, "/v1/sandboxes?wait=1", e.manifest(e.name()))
	if err != nil {
		return err
	}
	if err := x.status(http.StatusCreated); err != nil {
		return err
	}
	obj, err := x.object()
	if err != nil {
		return err
	}
	if obj.Status.ID == "" {
		return x.disagree("status.id on a created object", "no id")
	}
	e.record(e.caller, "/v1/sandboxes", obj.Status.ID)
	if obj.Status.Phase != "Running" {
		return x.disagree("phase Running in the answer of a held create", "phase "+obj.Status.Phase)
	}
	return nil
}
