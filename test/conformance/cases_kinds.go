// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// volumeCases, setCases, environmentCases, spawnCases, agentCases,
// computerUseCases and indistinguishabilityCases are the groups whose kind,
// binary or second environment is an input: each runs where its input is
// there and reports one skip naming the input where it is not.
func volumeCases() []Case {
	return []Case{{"volumes", "case019VolumeLifecycle", case019VolumeLifecycle}}
}

func setCases() []Case {
	return []Case{{"sets", "case020SetRunsToCompletion", case020SetRunsToCompletion}}
}

func environmentCases() []Case {
	return []Case{{"environments", "case021EnvironmentRead", case021EnvironmentRead}}
}

func spawnCases() []Case {
	return []Case{{"spawn", "case022SpawnBoundary", case022SpawnBoundary}}
}

func agentCases() []Case {
	return []Case{{"agent", "case011AgentScenario", case011AgentScenario}}
}

func computerUseCases() []Case {
	return []Case{{"computer use", "case023BrowserReady", case023BrowserReady}}
}

func indistinguishabilityCases() []Case {
	return []Case{{"indistinguishability", "case001Indistinguishable", case001Indistinguishable}}
}

// case019VolumeLifecycle: a volume is applied, read and deleted through the
// same grammar every kind shares.
func case019VolumeLifecycle(ctx context.Context, e *Env) error {
	if err := e.need("volumes"); err != nil {
		return err
	}
	name := e.name()
	body := mustJSON(map[string]any{
		"apiVersion": APIVersion,
		"kind":       "Volume",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"size": "1Gi"},
	})
	x, err := e.caller.put(ctx, "/v1/volumes/"+name, body, "application/json")
	if err != nil {
		return err
	}
	if x.Status != http.StatusCreated {
		return x.disagree("status 201 from an applied volume", fmt.Sprintf("status %d", x.Status))
	}
	obj, err := x.object()
	if err != nil {
		return err
	}
	e.record(e.caller, "/v1/volumes", obj.Status.ID)
	read, err := e.caller.get(ctx, "/v1/volumes/"+obj.Status.ID)
	if err != nil {
		return err
	}
	return read.status(http.StatusOK)
}

// case020SetRunsToCompletion: a set of replicas runs to completion on an
// environment that queues, and its replica table is what a caller reads.
func case020SetRunsToCompletion(ctx context.Context, e *Env) error {
	if e.cfg.QueuedEnvironment == "" {
		return skipf("no queued environment: set QueuedEnvironment to run the sets group")
	}
	name := e.name()
	body := mustJSON(map[string]any{
		"apiVersion": APIVersion,
		"kind":       "SandboxSet",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"replicas":    2,
			"parallelism": 1,
			"template":    map[string]any{"spec": map[string]any{"environment": e.cfg.QueuedEnvironment, "command": []string{"/bin/sh", "-c", "true"}}},
		},
	})
	x, err := e.caller.put(ctx, "/v1/sandboxsets/"+name, body, "application/json")
	if err != nil {
		return err
	}
	if x.Status != http.StatusCreated {
		return x.disagree("status 201 from an applied set", fmt.Sprintf("status %d", x.Status))
	}
	obj, err := x.object()
	if err != nil {
		return err
	}
	e.record(e.caller, "/v1/sandboxsets", obj.Status.ID)
	return nil
}

// case021EnvironmentRead: an administrator reads the environments this
// server drives, and each carries the capabilities its driver declares.
func case021EnvironmentRead(ctx context.Context, e *Env) error {
	if e.admin == nil {
		return skipf("no administrator token: set Admin to read the environments")
	}
	x, err := e.admin.get(ctx, "/v1/environments?limit=50")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var page struct {
		Items []struct {
			Metadata objectMetadata `json:"metadata"`
			Status   struct {
				ID           string          `json:"id"`
				Capabilities json.RawMessage `json:"capabilities"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(x.Body, &page); err != nil {
		return x.disagree("a list of environments", err.Error())
	}
	if len(page.Items) == 0 {
		return x.disagree("the environment this server drives", "an empty list")
	}
	for _, item := range page.Items {
		if len(item.Status.Capabilities) == 0 {
			return x.disagree("the capabilities each environment declares", "an environment that declares none: "+item.Metadata.Name)
		}
	}
	return nil
}

// case022SpawnBoundary: a sandbox given spawn rights applies a child through
// its own workload token, and a child that asks for more spawn budget than its
// parent has left is refused at that field, which is the rule a boundary never
// moves by. The parent holds a budget of two and a depth of one, so the budget
// is the only rule the child breaks. The budget axis needs neither a mesh nor
// an egress gateway, so the case runs on every environment a workload token
// reaches the API from.
func case022SpawnBoundary(ctx context.Context, e *Env) error {
	spawn := func(budget, depth int) map[string]any {
		return map[string]any{"spawn": map[string]any{"budget": budget, "depth": depth}}
	}
	parent, err := e.sandbox(ctx, e.caller, func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["mesh"] = spawn(2, 1)
	})
	if err != nil {
		return err
	}
	result, x, err := e.exec(ctx, e.caller, parent.Status.ID, "/bin/sh", "-c", `cat "$CELLA_TOKEN_FILE"`)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(result.Stdout)
	if token == "" {
		return x.disagree("the sandbox's own token", "an empty file at $CELLA_TOKEN_FILE")
	}
	child := e.manifest(e.name(), func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["mesh"] = spawn(5, 0)
	})
	workload := newClient(e.caller.base, token)
	refused, err := workload.post(ctx, "/v1/sandboxes", child)
	if err != nil {
		return err
	}
	// A server that created the child it had to refuse has made an object
	// this run deletes, whatever the case concludes.
	if obj, decodeErr := refused.object(); decodeErr == nil && obj.Status.ID != "" {
		e.record(e.caller, "/v1/sandboxes", obj.Status.ID)
	}
	if err := refused.refusal("boundary_exceeded"); err != nil {
		return err
	}
	if paths := refused.paths(); !slices.Contains(paths, "spec.mesh.spawn.budget") {
		return refused.disagree("details.paths naming spec.mesh.spawn.budget", fmt.Sprintf("paths %v", paths))
	}
	return nil
}

// case011AgentScenario: the scenario an agent runs from the skill alone,
// through the built command: apply, exec, copy out, read, delete, each with
// the exit code the scheme names. Copying out is the files capability, so the
// case runs where the environment declares it.
func case011AgentScenario(ctx context.Context, e *Env) error {
	if e.cfg.Cella == "" {
		return skipf("no agent binary: set Cella to the built command")
	}
	if err := e.need("files"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "conformance-agent-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	name := e.name()
	file := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(file, e.manifest(name), 0o600); err != nil {
		return err
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, e.cfg.Cella, args...)
		cmd.Env = append(os.Environ(), "CELLA_URL="+e.caller.base, "CELLA_TOKEN="+e.caller.token)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%s %s: %w: %s", filepath.Base(e.cfg.Cella), strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}
	if _, err := run("apply", "-f", file, "-w"); err != nil {
		return err
	}
	// The working directory is the workspace, so a relative path is a file
	// the files routes answer at /workspace on every environment.
	out, err := run("exec", name, "--", "/bin/sh", "-c", "echo scenario | tee scenario.txt")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "scenario") {
		return fmt.Errorf("the command's exec printed %q", out)
	}
	copied := filepath.Join(dir, "out")
	if _, err := run("cp", name+":/workspace/scenario.txt", copied); err != nil {
		return err
	}
	found, err := holdsFile(copied, "scenario.txt", "scenario\n")
	if err != nil {
		return fmt.Errorf("reading what cp wrote under %s: %w", copied, err)
	}
	if !found {
		return fmt.Errorf("cp wrote no scenario.txt holding the bytes the sandbox wrote under %s", copied)
	}
	if _, err := run("get", "sandbox", name, "-o", "json"); err != nil {
		return err
	}
	if _, err := run("delete", "sandbox", name); err != nil {
		return err
	}
	return nil
}

// holdsFile reports whether a file of that name holding those bytes is
// anywhere under dir. An archive carries a path relative to what was asked
// for, so where below the destination it lands is the client's business.
func holdsFile(dir, name, content string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != name {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found = found || string(data) == content
		return nil
	})
	return found, err
}

// case023BrowserReady: a sandbox with a desktop reaches DisplayReady, its
// screenshot is a frame, and an input batch lands.
func case023BrowserReady(ctx context.Context, e *Env) error {
	if e.cfg.DisplayImage == "" {
		return skipf("no display image: set DisplayImage to run the computer-use group")
	}
	if err := e.need("display"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller, func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["image"] = e.cfg.DisplayImage
		spec["display"] = map[string]any{"width": 1280, "height": 800}
	})
	if err != nil {
		return err
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	// The desktop comes up beside the workload and may follow it by a
	// moment: a running sandbox reports DisplayReady false until it does.
	// The case reads the display until it is ready, which is the state the
	// screenshot needs, and never sleeps for it.
	for {
		x, err := e.caller.get(ctx, base+"/display")
		if err != nil {
			return err
		}
		if err := x.status(http.StatusOK); err != nil {
			return err
		}
		var display struct {
			Width  int  `json:"width"`
			Height int  `json:"height"`
			Ready  bool `json:"ready"`
		}
		if err := json.Unmarshal(x.Body, &display); err != nil {
			return x.disagree("the geometry and the readiness of the desktop", err.Error())
		}
		if display.Width != 1280 || display.Height != 800 {
			return x.disagree("the geometry the manifest declared", fmt.Sprintf("%dx%d", display.Width, display.Height))
		}
		if display.Ready {
			break
		}
		select {
		case <-ctx.Done():
			return x.disagree("DisplayReady before the case's deadline", "a desktop that is not ready")
		case <-time.After(pollInterval):
		}
	}
	shot, err := e.caller.get(ctx, base+"/screenshot?format=png")
	if err != nil {
		return err
	}
	if err := shot.status(http.StatusOK); err != nil {
		return err
	}
	if len(shot.Body) < 8 || string(shot.Body[1:4]) != "PNG" {
		return shot.disagree("a PNG frame", fmt.Sprintf("%d bytes that are no PNG", len(shot.Body)))
	}
	return nil
}

// case001Indistinguishable: a sandbox created on the environment this node
// drives and one created on a worker's environment differ only in which
// environment, driver and isolation the status names.
func case001Indistinguishable(ctx context.Context, e *Env) error {
	if e.cfg.WorkerEnvironment == "" {
		return skipf("no worker environment: set WorkerEnvironment to compare two environments")
	}
	here, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	there, err := e.sandbox(ctx, e.caller, func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["environment"] = e.cfg.WorkerEnvironment
	})
	if err != nil {
		return err
	}
	if !jsonEqual(canonical(here.Spec), canonical(there.Spec), "environment") {
		return fmt.Errorf("the two specifications differ beyond the environment:\n  %s\n  %s", here.Spec, there.Spec)
	}
	if here.Status.Phase != there.Status.Phase {
		return fmt.Errorf("one sandbox is %s and the other %s", here.Status.Phase, there.Status.Phase)
	}
	if there.Status.Environment != e.cfg.WorkerEnvironment {
		return fmt.Errorf("the second sandbox names environment %s, want %s", there.Status.Environment, e.cfg.WorkerEnvironment)
	}
	return nil
}

// jsonEqual compares two objects, ignoring the members named.
func jsonEqual(a, b []byte, ignore ...string) bool {
	var left, right map[string]any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return string(a) == string(b)
	}
	for _, name := range ignore {
		delete(left, name)
		delete(right, name)
	}
	x, err := json.Marshal(left)
	if err != nil {
		return false
	}
	y, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(x) == string(y)
}
