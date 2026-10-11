// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// failsOnce is a main command that ends with exit code 3 the first time it
// runs in a workspace and runs on every time after: the file it leaves is in
// the workspace, so a second run that goes on is a second run on the same
// files.
const failsOnce = `if [ -f again ]; then exec sleep 300; fi; : > again; exit 3`

// awaitPhase reads a sandbox until it is in a phase.
func (f *fixture) awaitPhase(id, phase string) v1.Sandbox {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var obj v1.Sandbox
		if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/"+id, f.alice, "", http.StatusOK), &obj); err != nil {
			f.t.Fatal(err)
		}
		if obj.Status.Phase == phase {
			return obj
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the sandbox stayed %s %q, want %s", obj.Status.Phase, obj.Status.Reason, phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAFailedSandboxIsStartedAgainWithItsWorkspace is the recovery a client
// has for a sandbox whose main process ended in failure, over HTTP and on a
// real runtime. The sandbox reads Failed with the runtime's reason and the
// exit code; a start answers 200 and runs the workload again on the same
// workspace, the file the first run wrote and the one the client wrote both
// there; the started sandbox carries nothing of the failure; and a start of
// it once it runs is the conflict it always was.
func TestAFailedSandboxIsStartedAgainWithItsWorkspace(t *testing.T) {
	f := setup(t, nil)
	body, err := json.Marshal(map[string]any{
		"apiVersion": v1.APIVersion, "kind": "Sandbox", "metadata": map[string]any{"name": "work"},
		"spec": map[string]any{"command": []string{"/bin/sh", "-c", `while [ ! -f go ]; do sleep 0.01; done; rm go; ` + failsOnce}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, string(body), http.StatusCreated), &obj); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	// The client's own work, written while the sandbox runs, and then the
	// file the main process waits for before it fails.
	f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["/bin/sh","-c","printf work > kept.txt && : > go"]}`, http.StatusOK)
	failed := f.awaitPhase(obj.Status.ID, "Failed")
	if failed.Status.Reason != "Exited" || failed.Status.ExitCode == nil || *failed.Status.ExitCode != 3 {
		t.Fatalf("the failed sandbox reads reason %q and exit %v, want Exited and 3", failed.Status.Reason, failed.Status.ExitCode)
	}
	// A command needs a running sandbox; the start is what brings it back.
	f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["true"]}`, http.StatusConflict)
	f.request("POST", base+"/start", f.alice, "", http.StatusOK)
	running := f.awaitPhase(obj.Status.ID, runtime.Running)
	if running.Status.Reason != "" || running.Status.ExitCode != nil {
		t.Fatalf("the started sandbox reads reason %q and exit %v, want neither", running.Status.Reason, running.Status.ExitCode)
	}
	var result struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.Unmarshal(f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["/bin/sh","-c","cat kept.txt && test -f again"]}`, http.StatusOK), &result); err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "work" || result.ExitCode != 0 {
		t.Fatalf("the workspace after the start: %q, exit %d, want the client's file and the first run's", result.Stdout, result.ExitCode)
	}
	refused := f.request("POST", base+"/start", f.alice, "", http.StatusConflict)
	if !strings.Contains(string(refused), `"phase_conflict"`) {
		t.Fatalf("a start of a running sandbox answered %s", refused)
	}
}

// refusingCreate is a runtime that refuses every create, as one that cannot
// pull the image does.
type refusingCreate struct{ runtime.Driver }

func (refusingCreate) Create(context.Context, runtime.CreateSpec) (runtime.Ref, error) {
	return runtime.Ref{}, errors.New("the image cannot be pulled")
}

// TestAStartOfASandboxThatNeverRanIsRefused: a sandbox whose create failed is
// Failed with CreateFailed, there is nothing of it to start, and a start
// answers 409 phase_conflict with the reason in the developer detail.
func TestAStartOfASandboxThatNeverRanIsRefused(t *testing.T) {
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return refusingCreate{d} })
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, http.StatusCreated), &obj); err != nil {
		t.Fatal(err)
	}
	failed := f.awaitPhase(obj.Status.ID, "Failed")
	if failed.Status.Reason != "CreateFailed" {
		t.Fatalf("the sandbox failed with %q, want CreateFailed", failed.Status.Reason)
	}
	var refusal struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	answer := f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/start", f.alice, "", http.StatusConflict)
	if err := json.Unmarshal(answer, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error.Code != "phase_conflict" || !strings.Contains(refusal.Error.Details.Detail, "reason CreateFailed") {
		t.Fatalf("the start answered %s, want phase_conflict naming CreateFailed", answer)
	}
	if after := f.awaitPhase(obj.Status.ID, "Failed"); after.Status.Reason != "CreateFailed" {
		t.Fatalf("the refused start left the sandbox %s %q", after.Status.Phase, after.Status.Reason)
	}
}
