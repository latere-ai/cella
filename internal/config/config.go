// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package config reads the typed configuration of cellad from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultDataDir      = "/var/lib/cella"
	DefaultRuntime      = RuntimeK8s
)

// The runtime backends CELLA_RUNTIME selects. Spec 004 owns what each one
// does; this package owns the vocabulary, so a misspelled value is a
// start-up problem and not a backend that never comes up.
const (
	RuntimeK8s    = "k8s"
	RuntimePodman = "podman"
	RuntimeNative = "native"
)

// Runtimes is every accepted CELLA_RUNTIME value, in the order the
// problem message lists them.
var Runtimes = []string{RuntimeK8s, RuntimePodman, RuntimeNative}

// Getenv is the environment lookup Load reads through, so a test passes a
// map and never touches the process environment.
type Getenv func(string) string

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the CELLA_ prefix.
type Config struct {
	// PublicAddr is where the /v1 API and the public probes listen.
	PublicAddr string
	// InternalAddr is where the four probes listen for the cluster.
	InternalAddr string
	// DataDir holds what cellad keeps on local disk: the native and
	// podman backends' workspaces and the readiness probe's write test.
	DataDir string
	// Runtime is the backend cellad drives, one of Runtimes.
	Runtime string
	// Identity is spec 006's half: the issuers, the audience, the signing
	// keys, the authorizer, and the owner policy's admins.
	Identity
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
	c := Config{
		PublicAddr:   withDefault(getenv("CELLA_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr: withDefault(getenv("CELLA_INTERNAL_ADDR"), DefaultInternalAddr),
		DataDir:      withDefault(getenv("CELLA_DATA_DIR"), DefaultDataDir),
		Runtime:      withDefault(getenv("CELLA_RUNTIME"), DefaultRuntime),
	}
	problems := c.loadIdentity(getenv)
	if err := checkAddr(c.PublicAddr); err != nil {
		problems = append(problems, "CELLA_PUBLIC_ADDR "+err.Error())
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		problems = append(problems, "CELLA_INTERNAL_ADDR "+err.Error())
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		problems = append(problems, "CELLA_INTERNAL_ADDR must differ from CELLA_PUBLIC_ADDR; both are "+c.PublicAddr)
	}
	if !slices.Contains(Runtimes, c.Runtime) {
		problems = append(problems, fmt.Sprintf("CELLA_RUNTIME is %q; one of %s", c.Runtime, strings.Join(Runtimes, ", ")))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sameEndpoint reports whether two valid addresses name one socket. Port
// 0 asks the kernel for any free port, so two ":0" addresses are two
// sockets and a test that binds both on loopback is not refused.
func sameEndpoint(a, b string) bool {
	if a != b {
		return false
	}
	_, port, err := net.SplitHostPort(a)
	return err == nil && port != "0"
}

// checkAddr accepts what net.Listen accepts for a TCP address: host:port
// with the host optional.
func checkAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("is %q, not a host:port address", addr)
	}
	return nil
}
