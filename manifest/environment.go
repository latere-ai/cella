// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The JSON paths of the Environment's fields, named once so a refusal and a
// test point at the same string.
const (
	pathEnvironmentMode      = "spec.mode"
	pathEnvironmentIsolation = "spec.isolation"
	pathEnvironmentCapacity  = "spec.capacity"
	pathEnvironmentSchedule  = "spec.scheduling.mode"
	pathEnvironmentQueues    = "spec.scheduling.queues"
	pathEnvironmentDefaultQ  = "spec.scheduling.defaultQueue"
	pathEnvironmentPoolSize  = "spec.pool.size"
	pathEnvironmentPoolImage = "spec.pool.image"
	pathEnvironmentGateway   = "spec.gateway"
)

// MaxEnvironmentQueues bounds the named lines one environment declares. A
// queue is an operator's decision about how work is ordered, not a label a
// caller invents, so the count is small on purpose.
const MaxEnvironmentQueues = 32

// Isolations is every class a driver may declare, in the order spec 004 lists
// them.
var Isolations = []v1.Isolation{v1.IsolationContainer, v1.IsolationVM, v1.IsolationProcess, v1.IsolationNone}

// DecodeEnvironment reads one Environment manifest, in any syntax design 003
// admits, and discards the status a client sent. It is Decode for the kind
// that says where sandboxes run.
func DecodeEnvironment(body []byte, contentType string) (v1.Environment, error) {
	var obj v1.Environment
	if err := decode(body, contentType, v1.KindEnvironment, &obj); err != nil {
		// The capacity decodes itself, so a value it refuses reaches here as
		// a shape the schema could not read. The field is named, because a
		// caller cannot find it in a decoder's own sentence.
		var refusal *Error
		if errors.As(err, &refusal) && refusal.Code == "bad_request" && strings.Contains(refusal.Detail, v1.CapacityAuto) {
			return v1.Environment{}, failAt("invalid_field", pathEnvironmentCapacity, "A capacity is an object or the word auto.")
		}
		return v1.Environment{}, err
	}
	obj.Status = v1.EnvironmentStatus{}
	return obj, nil
}

// EnvironmentOptions are what ResolveEnvironment needs beyond the manifest:
// who is applying, the object this one replaces, the capabilities the driver
// behind it declares, and the clock the timestamps come from.
type EnvironmentOptions struct {
	Actor    Actor
	Existing *v1.Environment // the current object on update; nil on create
	// Capabilities is what the driver serving this environment declares. On
	// a create of a worker environment nothing has registered yet, so it is
	// the zero value and the pool rule is deferred to registration.
	Capabilities v1.Capabilities
	Now          func() time.Time
}

// ResolveEnvironment validates and defaults one Environment. It is
// deterministic and never mutates its input: the same manifest and the same
// options produce byte-identical output.
func ResolveEnvironment(in *v1.Environment, o EnvironmentOptions) (*v1.Environment, error) {
	if in == nil {
		return nil, fail("missing_field", "ResolveEnvironment needs a manifest")
	}
	obj := cloneEnvironment(in)
	obj.Status = v1.EnvironmentStatus{}
	if obj.APIVersion != v1.APIVersion {
		return nil, failAt("unsupported_version", "apiVersion", "This server serves "+v1.APIVersion+".")
	}
	if obj.Kind != v1.KindEnvironment {
		return nil, failAt("unsupported_kind", "kind", "This server does not serve that kind.")
	}
	if err := validateMetadata(obj.Metadata); err != nil {
		return nil, err
	}
	if obj.Metadata.Name == "" {
		return nil, failAt("missing_field", "metadata.name", "An environment is named by the operator.")
	}
	defaultEnvironment(&obj.Spec)
	if err := ValidateEnvironmentSpec(obj.Spec, o.Capabilities); err != nil {
		return nil, err
	}
	if err := environmentUpdateRule(&obj, o.Existing); err != nil {
		return nil, err
	}
	return &obj, nil
}

// defaultEnvironment fills every absent field the contract gives a default.
// An environment a caller applies is a worker's unless it says otherwise,
// because the in-process one is the control plane's own and no caller applies
// it.
func defaultEnvironment(s *v1.EnvironmentSpec) {
	if s.Mode == "" {
		s.Mode = v1.EnvironmentWorker
	}
	if s.Scheduling.Mode == "" {
		s.Scheduling.Mode = v1.SchedulingDirect
	}
	if len(s.Scheduling.Queues) == 0 {
		s.Scheduling.Queues = []string{v1.DefaultQueueName}
	}
	if s.Scheduling.DefaultQueue == "" {
		s.Scheduling.DefaultQueue = s.Scheduling.Queues[0]
	}
}

// ValidateEnvironmentSpec is the field table of spec 021, one refusal per
// rule. Capabilities is what the driver behind the environment declares; the
// zero value defers the rules that depend on one, which is what a worker
// environment carries until a worker has registered.
func ValidateEnvironmentSpec(s v1.EnvironmentSpec, caps v1.Capabilities) error {
	if !slices.Contains(v1.EnvironmentModes, s.Mode) {
		return failAt("invalid_field", pathEnvironmentMode, "A mode is one of "+strings.Join(v1.EnvironmentModes, ", ")+".")
	}
	if s.Isolation == "" {
		return failAt("missing_field", pathEnvironmentIsolation, "An environment declares the isolation class its driver provides.")
	}
	if !slices.Contains(Isolations, s.Isolation) {
		return failAt("invalid_field", pathEnvironmentIsolation, "An isolation class is one of "+strings.Join(Isolations, ", ")+".")
	}
	if err := validateCapacity(s.Capacity, s.Mode); err != nil {
		return err
	}
	if err := validateScheduling(s.Scheduling); err != nil {
		return err
	}
	if err := validateEnvironmentPool(s.Pool, caps); err != nil {
		return err
	}
	if s.Gateway != "" {
		if err := validateGatewayAddr(s.Gateway); err != nil {
			return err
		}
	} else if len(caps.Egress) > 0 {
		return failAt("missing_field", pathEnvironmentGateway,
			"This environment's driver enforces egress, so it needs the address its sandboxes reach the gateway at.")
	}
	return nil
}

// validateCapacity holds the ceiling to quantities and a count. The word auto
// is the k8s driver reading the cluster's allocatable, which only an
// in-process environment can do: a worker reports its own figures at
// registration and the control plane has nothing to read.
func validateCapacity(c v1.Capacity, mode string) error {
	if c.Auto {
		if mode != v1.EnvironmentInprocess {
			return failAt("invalid_field", pathEnvironmentCapacity,
				"Only the control plane's own environment reads its capacity from the cluster; a worker's declares figures.")
		}
		return nil
	}
	if c.IsZero() {
		return failAt("missing_field", pathEnvironmentCapacity, "An environment declares how much it holds.")
	}
	for _, q := range []struct {
		name  string
		value v1.Quantity
	}{{"cpu", c.CPU}, {"memory", c.Memory}, {"disk", c.Disk}} {
		if q.value == "" {
			continue
		}
		n, err := ParseQuantity(q.value)
		if err != nil || n <= 0 {
			return failAt("invalid_field", pathEnvironmentCapacity+"."+q.name, "This is a quantity above zero, such as 512 or 2Ti.")
		}
	}
	if c.Sandboxes < 0 {
		return failAt("invalid_field", pathEnvironmentCapacity+".sandboxes", "This is a count of sandboxes, zero or above.")
	}
	return nil
}

func validateScheduling(s v1.SchedulingSpec) error {
	switch s.Mode {
	case v1.SchedulingDirect:
	case v1.SchedulingQueued:
	default:
		return failAt("invalid_field", pathEnvironmentSchedule, "A scheduling mode is direct or queued.")
	}
	if len(s.Queues) > MaxEnvironmentQueues {
		return failAt("invalid_field", pathEnvironmentQueues,
			"An environment declares at most "+strconv.Itoa(MaxEnvironmentQueues)+" queues.")
	}
	for _, q := range s.Queues {
		if q == "" || len(q) > maxNameLength || !namePattern.MatchString(q) {
			return failAt("invalid_field", pathEnvironmentQueues, "A queue name is a DNS label: lower-case letters, digits and hyphens.")
		}
	}
	if !slices.Contains(s.Queues, s.DefaultQueue) {
		return failAt("invalid_field", pathEnvironmentDefaultQ, "The default queue is one of the queues this environment declares.")
	}
	return nil
}

// validateEnvironmentPool holds the prewarmed set to what a driver that
// declares Pool can keep. A pool on a driver that declares none would be
// entries nothing ever makes.
func validateEnvironmentPool(p v1.PoolSpec, caps v1.Capabilities) error {
	if p.Size < 0 {
		return failAt("invalid_field", pathEnvironmentPoolSize, "A pool size is zero or above.")
	}
	if p.Size == 0 {
		return nil
	}
	if !caps.Pool {
		return failAt("capability_unsupported", pathEnvironmentPoolSize, "This environment's driver keeps no prewarmed entries.")
	}
	if p.Image == "" {
		return failAt("missing_field", pathEnvironmentPoolImage, "A pool declares the image its entries run.")
	}
	return nil
}

// validateGatewayAddr holds spec.gateway to a host, or a host and a port, as
// a sandbox of the environment dials it. It is not a URL: the driver writes
// it into the proxy variables and into the boundary the gateway enforces.
func validateGatewayAddr(addr string) error {
	host := addr
	if h, port, err := net.SplitHostPort(addr); err == nil {
		host = h
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return failAt("invalid_field", pathEnvironmentGateway, "A gateway address is a host, or a host and a port between 1 and 65535.")
		}
	} else if strings.Contains(addr, ":") && net.ParseIP(addr) == nil {
		return failAt("invalid_field", pathEnvironmentGateway, "A gateway address is a host, or a host and a port between 1 and 65535.")
	}
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return failAt("invalid_field", pathEnvironmentGateway, "A gateway address is a host, or a host and a port, and not a URL.")
	}
	return nil
}

// environmentUpdateRule refuses a change to a field spec 021 fixes at create.
// The mode decides which half of the control plane serves the environment and
// the isolation class is what every manifest on it resolved against, so
// neither moves under a running sandbox.
func environmentUpdateRule(obj *v1.Environment, existing *v1.Environment) error {
	if existing == nil {
		return nil
	}
	var paths []string
	if existing.Metadata.Name != obj.Metadata.Name {
		paths = append(paths, "metadata.name")
	}
	if existing.Spec.Mode != "" && existing.Spec.Mode != obj.Spec.Mode {
		paths = append(paths, pathEnvironmentMode)
	}
	if existing.Spec.Isolation != "" && existing.Spec.Isolation != obj.Spec.Isolation {
		paths = append(paths, pathEnvironmentIsolation)
	}
	if len(paths) > 0 {
		return failPaths("immutable_field", "This field cannot be changed after the environment is created.", paths)
	}
	return nil
}

// cloneEnvironment copies every reference an Environment holds, so a resolve
// never writes through its caller's object.
func cloneEnvironment(in *v1.Environment) v1.Environment {
	obj := *in
	obj.Metadata.Labels = maps.Clone(in.Metadata.Labels)
	obj.Metadata.Annotations = maps.Clone(in.Metadata.Annotations)
	obj.Spec.Scheduling.Queues = slices.Clone(in.Spec.Scheduling.Queues)
	if in.Spec.Pool.Display != nil {
		display := *in.Spec.Pool.Display
		obj.Spec.Pool.Display = &display
	}
	obj.Status.Capabilities.Egress = slices.Clone(in.Status.Capabilities.Egress)
	return obj
}
