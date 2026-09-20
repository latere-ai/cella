// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/k8s"
)

// WorkerConfig is the worker role's own configuration. It reads none of the
// control plane's variables: the role holds no store, no issuer and no
// authorizer, and connects outbound with one key to claim operations and run
// them with its own driver (spec 021).
type WorkerConfig struct {
	// URL is the control plane's public URL, which the worker registers at
	// and opens its one stream to.
	URL string
	// EnvironmentKey authenticates that stream and names the environment.
	EnvironmentKey string
	// Runtime is the driver this worker runs, one of Runtimes.
	Runtime string
	// DataDir is where the native driver keeps its sandboxes, PodmanSocket
	// the libpod API the podman driver drives, and K8s the cluster the k8s
	// driver drives. Each is read only where the selected driver needs it.
	DataDir      string
	PodmanSocket string
	K8s          k8s.Options
	// AllowUnsafeNative is the explicit consent native execution needs,
	// since it confines nothing.
	AllowUnsafeNative bool
	// Capacity is what this worker declares it can hold, which placement
	// admits against beside the environment's own ceiling.
	Capacity v1.Capacity
	// Labels are what this worker declares about itself, for an operator
	// reading the fleet.
	Labels map[string]string
	// Insecure admits a control plane URL that is http:// on a host other
	// than loopback. The stubs set it; nothing else should.
	Insecure bool
}

// LoadWorker reads the worker role's configuration, or returns one error
// naming every problem, sorted, so an operator fixes a deployment in one
// round.
func LoadWorker(getenv Getenv) (WorkerConfig, error) {
	c := WorkerConfig{
		URL:            strings.TrimSpace(getenv("CELLA_URL")),
		EnvironmentKey: strings.TrimSpace(getenv("CELLA_ENVIRONMENT_KEY")),
		Runtime:        withDefault(getenv("CELLA_RUNTIME"), DefaultRuntime),
		DataDir:        withDefault(getenv("CELLA_DATA_DIR"), DefaultDataDir),
		PodmanSocket:   strings.TrimSpace(getenv("CELLA_PODMAN_SOCKET")),
	}
	var problems []string
	if raw := strings.TrimSpace(getenv("CELLA_INSECURE_CONTROL_PLANE")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, "CELLA_INSECURE_CONTROL_PLANE must be true or false")
		}
		c.Insecure = value
	}
	if raw := strings.TrimSpace(getenv("CELLA_ALLOW_UNSAFE_NATIVE")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, "CELLA_ALLOW_UNSAFE_NATIVE must be true or false")
		}
		c.AllowUnsafeNative = value
	}
	if c.URL == "" {
		problems = append(problems, "CELLA_URL is unset, and the worker connects outbound to it")
	} else if err := checkControlPlaneURL(c.URL, c.Insecure); err != nil {
		problems = append(problems, "CELLA_URL "+err.Error())
	}
	if c.EnvironmentKey == "" {
		problems = append(problems, "CELLA_ENVIRONMENT_KEY is unset, and it is what authenticates the worker's stream")
	}
	if !slices.Contains(Runtimes, c.Runtime) {
		problems = append(problems, fmt.Sprintf("CELLA_RUNTIME is %q; one of %s", c.Runtime, strings.Join(Runtimes, ", ")))
	}
	if c.PodmanSocket != "" && !filepath.IsAbs(c.PodmanSocket) {
		problems = append(problems, "CELLA_PODMAN_SOCKET must be an absolute path to a unix socket")
	}
	if c.Runtime == RuntimeK8s {
		c.K8s = loadK8s(getenv, &problems)
	}
	if c.Runtime == RuntimeNative && !c.AllowUnsafeNative {
		problems = append(problems, "CELLA_RUNTIME=native requires CELLA_ALLOW_UNSAFE_NATIVE=true; native execution has no isolation")
	}
	c.Capacity = workerCapacity(getenv, &problems)
	c.Labels = workerLabels(getenv, &problems)
	if len(problems) > 0 {
		sort.Strings(problems)
		return WorkerConfig{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// workerCapacity reads what this worker declares it holds. It is optional: a
// worker that declares nothing is admitted against the environment's ceiling
// alone, which is the right answer for a single worker on one machine.
func workerCapacity(getenv Getenv, problems *[]string) v1.Capacity {
	c := v1.Capacity{
		CPU:    v1.Quantity(strings.TrimSpace(getenv("CELLA_CAPACITY_CPU"))),
		Memory: v1.Quantity(strings.TrimSpace(getenv("CELLA_CAPACITY_MEMORY"))),
		Disk:   v1.Quantity(strings.TrimSpace(getenv("CELLA_CAPACITY_DISK"))),
	}
	if raw := strings.TrimSpace(getenv("CELLA_CAPACITY_SANDBOXES")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			*problems = append(*problems, "CELLA_CAPACITY_SANDBOXES must be a count of sandboxes, zero or above")
		} else {
			c.Sandboxes = value
		}
	}
	return c
}

// workerLabels reads CELLA_WORKER_LABELS, comma separated key=value pairs.
func workerLabels(getenv Getenv, problems *[]string) map[string]string {
	raw := strings.TrimSpace(getenv("CELLA_WORKER_LABELS"))
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for pair := range strings.SplitSeq(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			*problems = append(*problems, "CELLA_WORKER_LABELS is a comma separated list of key=value pairs")
			return nil
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}
