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
	"math"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/runtime/k8s"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultDataDir      = "/var/lib/cella"
	DefaultRuntime      = RuntimeK8s
	// DefaultReapInterval is how often the lifecycle rules of spec 005
	// run, and DefaultTouchInterval how often one sandbox's activity
	// reaches the driver. MinInterval and MaxInterval bound both: a tick
	// per millisecond is a load generator and a tick per day is not a
	// deadline.
	DefaultReapInterval  = 30 * time.Second
	DefaultTouchInterval = time.Minute
	MinInterval          = time.Second
	MaxInterval          = time.Hour
	// DefaultLostGrace is how long a sandbox the driver no longer has is
	// held before it is deleted, where no durable store can recover it
	// (spec 005). It takes the same bounds as the intervals.
	DefaultLostGrace = 10 * time.Minute
	// DefaultEnvironmentOffline is how long an environment is held live
	// without a worker heartbeat, or with its in-process driver failing
	// Ready, before it is Offline (spec 021).
	DefaultEnvironmentOffline = 2 * time.Minute
	// DefaultDBMaxConns is the pool one replica opens on the database, and
	// MaxDBMaxConns the most it may ask for: a database shared by a fleet
	// has a connection ceiling, and one replica does not hold it (spec 010).
	DefaultDBMaxConns = 4
	MaxDBMaxConns     = 32
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
	// PodmanSocket is the libpod API the podman backend drives. Empty lets
	// the driver try the rootless user socket and then the system one.
	PodmanSocket string
	// AllowUnsafeNative explicitly permits execution without isolation.
	AllowUnsafeNative bool
	// MaxBodyBytes and MaxUploadBytes bound JSON and archive requests.
	MaxBodyBytes   int64
	MaxUploadBytes int64
	// ReapInterval is how often the controller applies the lifecycle rules
	// and TouchInterval how often one sandbox's activity reaches the
	// driver, both from spec 005.
	ReapInterval  time.Duration
	TouchInterval time.Duration
	// LostGrace is how long a sandbox the data plane lost is held before it
	// is deleted, where the store cannot recover it (spec 005).
	LostGrace time.Duration
	// DBURL is spec 010's store: a postgres:// URL on a direct endpoint or
	// a session-mode pooler, or empty for the single-process snapshot under
	// DataDir. Migrations always run over it. DBPoolURL is the pooled
	// endpoint the serving path opens where a fleet's database sits behind
	// a pooler; empty, the serving path uses DBURL. DBMaxConns bounds the
	// pool.
	DBURL      string
	DBPoolURL  string
	DBMaxConns int32
	// SecretKey wraps every secret value's data key (specs 010 and 018).
	// It is read where it is set and required once a Secret exists.
	SecretKey []byte
	// K8s configures the Kubernetes driver of spec 004. It is read only
	// when CELLA_RUNTIME selects that driver, so an installation that runs
	// another backend carries no opinion about a cluster.
	K8s k8s.Options
	// Gateway is spec 018's half of the boundary: where sandboxes reach
	// the egress gateway, how long a create waits for one to hold the
	// sandbox's map, and how many connection records are kept.
	Gateway EgressGateway
	// Events is spec 009's sink: where one record per mutation and per
	// operation goes, and what signs it.
	Events Events
	// Admission is spec 007's step: the endpoint stage 3 of a resolve
	// calls, and the image default an installation without one falls back
	// on.
	Admission Admission
	// Scheduling is spec 020's half: the default environment's mode and the
	// pool it keeps prewarmed.
	Scheduling Scheduling
	// EnvironmentOffline is how long without a worker heartbeat, or with the
	// in-process driver not ready, before an environment is Offline (spec
	// 021). CELLA_ENVIRONMENT_OFFLINE sets it.
	EnvironmentOffline time.Duration
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
	c.MaxBodyBytes = byteLimit(getenv, "CELLA_MAX_BODY_BYTES", 65536, &problems)
	c.MaxUploadBytes = byteLimit(getenv, "CELLA_MAX_UPLOAD_BYTES", 1<<30, &problems)
	c.ReapInterval = interval(getenv, "CELLA_REAP_INTERVAL", DefaultReapInterval, &problems)
	c.TouchInterval = interval(getenv, "CELLA_TOUCH_INTERVAL", DefaultTouchInterval, &problems)
	c.LostGrace = interval(getenv, "CELLA_LOST_GRACE", DefaultLostGrace, &problems)
	c.DBURL = databaseURL(getenv("CELLA_DB_URL"), "CELLA_DB_URL", &problems)
	c.DBPoolURL = databaseURL(getenv("CELLA_DB_POOL_URL"), "CELLA_DB_POOL_URL", &problems)
	if c.DBPoolURL != "" && c.DBURL == "" {
		problems = append(problems, "CELLA_DB_POOL_URL needs CELLA_DB_URL: migrations run over the direct endpoint")
	}
	c.DBMaxConns = connections(getenv, &problems)
	c.SecretKey = secretKey(getenv, &problems)
	c.loadEvents(getenv, &problems)
	c.Admission = loadAdmission(getenv, &problems)
	if raw := getenv("CELLA_ALLOW_UNSAFE_NATIVE"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, "CELLA_ALLOW_UNSAFE_NATIVE must be true or false")
		}
		c.AllowUnsafeNative = value
	}
	c.Gateway = loadEgressGateway(getenv, &problems)
	c.Scheduling = loadScheduling(getenv, &problems)
	c.EnvironmentOffline = interval(getenv, "CELLA_ENVIRONMENT_OFFLINE", DefaultEnvironmentOffline, &problems)
	c.PodmanSocket = strings.TrimSpace(getenv("CELLA_PODMAN_SOCKET"))
	if c.PodmanSocket != "" && !filepath.IsAbs(c.PodmanSocket) {
		problems = append(problems, "CELLA_PODMAN_SOCKET must be an absolute path to a unix socket")
	}
	if c.Runtime == RuntimeK8s {
		c.K8s = loadK8s(getenv, &problems)
	}
	if c.Runtime == RuntimeNative && !c.AllowUnsafeNative {
		problems = append(problems, "CELLA_RUNTIME=native requires CELLA_ALLOW_UNSAFE_NATIVE=true; native execution has no isolation")
	}
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

// interval reads an optional duration variable and holds it between
// MinInterval and MaxInterval. A value outside the range is a start-up
// problem rather than a silent clamp: an operator who asks for a tick this
// process will not run gets an answer instead of a different behaviour.
func interval(getenv Getenv, name string, def time.Duration, problems *[]string) time.Duration {
	d := duration(getenv, name, def, problems)
	if d < MinInterval || d > MaxInterval {
		*problems = append(*problems, fmt.Sprintf("%s is %s; between %s and %s", name, d, MinInterval, MaxInterval))
		return def
	}
	return d
}

// databaseURL reads the optional store URL. Unset keeps every state in the
// single-process snapshot under CELLA_DATA_DIR and turns recovery off, which
// the start-up line says; set, it must be a Postgres URL, because the scheme
// is what selects the driver and the migrator.
func databaseURL(value, name string, problems *[]string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	switch {
	case err != nil, !strings.HasPrefix(parsed.Scheme, "postgres"), parsed.Host == "":
		*problems = append(*problems, name+" must be a postgres:// URL naming a host")
		return ""
	}
	return raw
}

// connections bounds the pool one replica opens. A database shared by a fleet
// has a connection ceiling, and a replica that asked for all of it would take
// the fleet down with it.
func connections(getenv Getenv, problems *[]string) int32 {
	raw := strings.TrimSpace(getenv("CELLA_DB_MAX_CONNS"))
	if raw == "" {
		return DefaultDBMaxConns
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > MaxDBMaxConns {
		*problems = append(*problems, fmt.Sprintf("CELLA_DB_MAX_CONNS is %q; a whole number between 1 and %d", raw, MaxDBMaxConns))
		return DefaultDBMaxConns
	}
	return int32(value)
}

// secretKey reads the key every secret value's data key is wrapped under. It
// is validated where it is set, so a deployment learns at start-up that its
// key is the wrong length rather than on the first secret.
func secretKey(getenv Getenv, problems *[]string) []byte {
	raw := strings.TrimSpace(getenv("CELLA_SECRET_KEY"))
	if raw == "" {
		return nil
	}
	key, err := store.ParseKey(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("CELLA_SECRET_KEY is not usable: %v", err))
		return nil
	}
	return key
}

// byteLimit accepts integer bytes and binary Ki, Mi and Gi suffixes.
func byteLimit(getenv Getenv, name string, fallback int64, problems *[]string) int64 {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback
	}
	number := raw
	multiplier := int64(1)
	for suffix, scale := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30} {
		if before, ok := strings.CutSuffix(number, suffix); ok {
			number = before
			multiplier = scale
			break
		}
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value <= 0 || value > math.MaxInt64/multiplier {
		*problems = append(*problems, name+" must be positive integer bytes, optionally suffixed Ki, Mi, or Gi")
		return 0
	}
	return value * multiplier
}
