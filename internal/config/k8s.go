// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"

	"latere.ai/x/cella/runtime/k8s"
)

// loadK8s reads the variables of the Kubernetes driver. They are read only
// when that driver is selected, so a deployment that runs another backend is
// not held to the shape of a value it never uses. Every default is the
// driver's own, so this table and the driver never drift.
func loadK8s(getenv Getenv, problems *[]string) k8s.Options {
	o := k8s.Options{
		Namespace:          withDefault(getenv("CELLA_K8S_NAMESPACE"), k8s.DefaultNamespace),
		Kubeconfig:         strings.TrimSpace(getenv("CELLA_K8S_KUBECONFIG")),
		StorageClass:       strings.TrimSpace(getenv("CELLA_K8S_STORAGE_CLASS")),
		ImagePullSecrets:   splitList(getenv("CELLA_K8S_IMAGE_PULL_SECRETS")),
		DefaultCPU:         withDefault(getenv("CELLA_K8S_DEFAULT_CPU"), k8s.DefaultCPU),
		DefaultMemory:      withDefault(getenv("CELLA_K8S_DEFAULT_MEMORY"), k8s.DefaultMemory),
		DefaultDisk:        withDefault(getenv("CELLA_K8S_DEFAULT_DISK"), k8s.DefaultDisk),
		NodeSelector:       pairs(getenv, "CELLA_K8S_NODE_SELECTOR", problems),
		Tolerations:        tolerations(getenv, "CELLA_K8S_TOLERATIONS", problems),
		RunAsUser:          id(getenv, "CELLA_K8S_RUN_AS_USER", k8s.DefaultRunAsUser, problems),
		RunAsGroup:         id(getenv, "CELLA_K8S_RUN_AS_GROUP", k8s.DefaultRunAsGroup, problems),
		CPURequestRatio:    ratio(getenv, "CELLA_K8S_CPU_REQUEST_RATIO", k8s.DefaultCPURequestRatio, problems),
		MemoryRequestRatio: ratio(getenv, "CELLA_K8S_MEMORY_REQUEST_RATIO", k8s.DefaultMemoryRequestRatio, problems),
		ReadyTimeout:       duration(getenv, "CELLA_K8S_READY_TIMEOUT", k8s.DefaultReadyTimeout, problems),
		GracePeriod:        duration(getenv, "CELLA_K8S_GRACE_PERIOD", k8s.DefaultGracePeriod, problems),
	}
	return o
}

// pairs reads "key=value,key=value" into a map, which is how an operator
// writes a node selector without this repository knowing any node's labels.
func pairs(getenv Getenv, name string, problems *[]string) map[string]string {
	entries := splitList(getenv(name))
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			*problems = append(*problems, name+" holds "+strconv.Quote(entry)+"; each entry is key=value")
			continue
		}
		out[key] = value
	}
	return out
}

// tolerations reads "key[=value][:effect]" entries. The driver takes them as
// plain maps, so a toleration an operator needs is one this package never has
// to know the shape of.
func tolerations(getenv Getenv, name string, problems *[]string) []map[string]string {
	entries := splitList(getenv(name))
	if len(entries) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(entries))
	for _, entry := range entries {
		rest, effect, _ := strings.Cut(entry, ":")
		key, value, hasValue := strings.Cut(rest, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			*problems = append(*problems, name+" holds "+strconv.Quote(entry)+"; each entry is key[=value][:effect]")
			continue
		}
		t := map[string]string{"key": key, "operator": "Exists"}
		if hasValue {
			t["operator"], t["value"] = "Equal", strings.TrimSpace(value)
		}
		if effect = strings.TrimSpace(effect); effect != "" {
			t["effect"] = effect
		}
		out = append(out, t)
	}
	return out
}

// id reads a uid or a gid. Zero is refused here rather than at the first
// sandbox: the security baseline runs no workload as root.
func id(getenv Getenv, name string, fallback int64, problems *[]string) int64 {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		*problems = append(*problems, name+" is "+strconv.Quote(raw)+"; a uid above zero, since no sandbox runs as root")
		return fallback
	}
	return value
}

// ratio reads a fraction of a limit, which is what a request is sized by.
func ratio(getenv Getenv, name string, fallback float64, problems *[]string) float64 {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || value > 1 {
		*problems = append(*problems, fmt.Sprintf("%s is %s; a fraction of the limit above 0 and at most 1", name, strconv.Quote(raw)))
		return fallback
	}
	return value
}
