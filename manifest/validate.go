// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

const (
	// DefaultWorkspacePath is where a sandbox's own files are mounted.
	DefaultWorkspacePath = "/workspace"
	// ReservedPathPrefix is the control plane's own mount point inside a
	// sandbox: its token, its trust store, and its sockets.
	ReservedPathPrefix = "/run/cella"
	// ReservedKeyDomain and its subdomains are the control plane's own label
	// and annotation keys, stamped by the server and never taken from a body.
	ReservedKeyDomain = "cella.latere.ai"

	// The desktop bounds of spec 003's field table. They are stated here
	// rather than read from runtime/display, which holds the same numbers for
	// the drivers, because this package reaches no driver package.
	minDisplayWidth  = 320
	maxDisplayWidth  = 7680
	minDisplayHeight = 240
	maxDisplayHeight = 4320

	maxNameLength           = 63
	maxAnnotationValueBytes = 4 << 10
	maxAnnotationBytes      = 64 << 10
	maxEnvBytes             = 32 << 10
	maxUserNameLength       = 32
)

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	envPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	labelPart       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
	userNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)
)

// validate runs the field rules that need no defaults and no lookups. It is
// stage 1 of Resolve and runs again over an admission step's output.
func validate(obj *v1.Sandbox) error {
	if err := validateMetadata(obj.Metadata); err != nil {
		return err
	}
	return validateSpec(obj.Spec)
}

func validateMetadata(md v1.Metadata) error {
	if md.Name != "" && (len(md.Name) > maxNameLength || !namePattern.MatchString(md.Name)) {
		return failAt("invalid_field", "metadata.name", "The name must be a DNS label of at most 63 characters.")
	}
	for _, set := range []struct {
		field string
		keys  map[string]string
	}{{"metadata.labels", md.Labels}, {"metadata.annotations", md.Annotations}} {
		for _, k := range sortedKeys(set.keys) {
			if reservedKey(k) {
				return failAt("reserved_prefix", set.field+"."+k, "Keys under "+ReservedKeyDomain+" are the control plane's own.")
			}
			if !validKey(k) {
				return failAt("invalid_field", set.field+"."+k, "The key must be a label name, optionally prefixed by a DNS subdomain and a slash.")
			}
		}
	}
	for _, k := range sortedKeys(md.Labels) {
		if v := md.Labels[k]; len(v) > maxNameLength || (v != "" && !labelPart.MatchString(v)) {
			return failAt("invalid_field", "metadata.labels."+k, "A label value is at most 63 characters of letters, digits, dashes, underscores and dots.")
		}
	}
	total := 0
	for _, k := range sortedKeys(md.Annotations) {
		v := md.Annotations[k]
		if len(v) > maxAnnotationValueBytes {
			return failAt("invalid_field", "metadata.annotations."+k, "An annotation value is at most 4 KiB.")
		}
		total += len(k) + len(v)
	}
	if total > maxAnnotationBytes {
		return failAt("invalid_field", "metadata.annotations", "The annotations are at most 64 KiB in total.")
	}
	return nil
}

// reservedKey reports whether a label or annotation key is under the control
// plane's own domain. A subdomain of that domain is reserved too, so a key one
// label deeper cannot impersonate a stamped one.
func reservedKey(key string) bool {
	domain, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	return domain == ReservedKeyDomain || strings.HasSuffix(domain, "."+ReservedKeyDomain)
}

func validateSpec(s v1.SandboxSpec) error {
	if s.Environment != "" && (len(s.Environment) > maxNameLength || !namePattern.MatchString(s.Environment)) {
		return failAt("invalid_field", "spec.environment", "The environment must be a DNS label of at most 63 characters.")
	}
	for _, list := range []struct {
		field string
		args  []string
	}{{"spec.command", s.Command}, {"spec.args", s.Args}} {
		for i, arg := range list.args {
			if strings.ContainsRune(arg, 0) {
				return failAt("invalid_field", fmt.Sprintf("%s[%d]", list.field, i), "An argument cannot contain a NUL byte.")
			}
		}
	}
	if len(s.Command) > 0 && s.Command[0] == "" {
		return failAt("invalid_field", "spec.command[0]", "The command cannot start with an empty argument.")
	}
	if s.Workdir != "" && !absoluteCleanPath(s.Workdir) {
		return failAt("invalid_field", "spec.workdir", "The working directory must be a clean absolute path.")
	}
	if err := validateUser(s.User); err != nil {
		return err
	}
	if err := validateResources(s.Resources); err != nil {
		return err
	}
	if err := validateWorkspace(s.Workspace); err != nil {
		return err
	}
	if err := validateLifecycle(s.Lifecycle); err != nil {
		return err
	}
	if err := validateNetwork(s.Network); err != nil {
		return err
	}
	if err := validateDisplay(s.Display); err != nil {
		return err
	}
	if err := validateMesh(s.Mesh); err != nil {
		return err
	}
	return validateEnv(s.Env)
}

// validateDisplay holds a desktop to the bounds of spec 003. Both dimensions
// are given or neither is: half a geometry names no screen, and defaulting the
// other half would hand back a desktop of a size nobody asked for.
func validateDisplay(d *v1.Display) error {
	if d == nil {
		return nil
	}
	if (d.Width == 0) != (d.Height == 0) {
		return failPaths("invalid_field", "A display is a width and a height together.",
			[]string{"spec.display.width", "spec.display.height"})
	}
	switch {
	case d.Width == 0:
		return failAt("missing_field", "spec.display", "A display names a width and a height.")
	case d.Width < minDisplayWidth || d.Width > maxDisplayWidth:
		return failAt("invalid_field", "spec.display.width", fmt.Sprintf("The width is between %d and %d pixels.", minDisplayWidth, maxDisplayWidth))
	case d.Height < minDisplayHeight || d.Height > maxDisplayHeight:
		return failAt("invalid_field", "spec.display.height", fmt.Sprintf("The height is between %d and %d pixels.", minDisplayHeight, maxDisplayHeight))
	}
	return nil
}

// validateUser accepts a uid, a uid:gid pair, or a user name the image knows.
func validateUser(user string) error {
	if user == "" {
		return nil
	}
	bad := failAt("invalid_field", "spec.user", "The user must be a uid, a uid:gid pair, or a user name.")
	id, group, pair := strings.Cut(user, ":")
	if pair {
		if !isID(id) || !isID(group) {
			return bad
		}
		return nil
	}
	if isID(user) || (len(user) <= maxUserNameLength && userNamePattern.MatchString(user)) {
		return nil
	}
	return bad
}

func isID(s string) bool {
	v, err := strconv.ParseUint(s, 10, 32)
	return err == nil && s == strconv.FormatUint(v, 10)
}

func validateResources(r v1.Resources) error {
	for _, field := range resourceFields(&r) {
		if *field.value == "" {
			continue
		}
		amount, err := ParseQuantity(*field.value)
		if err != nil {
			return failAt("invalid_field", field.path, upperFirst(err.Error())+".")
		}
		if amount <= 0 {
			return failAt("invalid_field", field.path, "The amount must be positive.")
		}
	}
	return nil
}

func validateWorkspace(w v1.Workspace) error {
	switch w.Source {
	case "", v1.WorkspaceSourceEmpty, "git", "volume":
	default:
		return failAt("invalid_field", "spec.workspace.source", "The workspace source is empty, git, or volume.")
	}
	if w.Path == "" {
		return nil
	}
	if !absoluteCleanPath(w.Path) || w.Path == "/" {
		return failAt("invalid_field", "spec.workspace.path", "The workspace path must be a clean absolute path below the root.")
	}
	if w.Path == ReservedPathPrefix || strings.HasPrefix(w.Path, ReservedPathPrefix+"/") {
		return failAt("invalid_field", "spec.workspace.path", "The workspace path cannot be under "+ReservedPathPrefix+", which the control plane owns.")
	}
	return nil
}

func validateLifecycle(l v1.Lifecycle) error {
	for _, field := range lifecycleFields(&l) {
		if *field.value == "" {
			continue
		}
		if _, _, err := ParseDuration(*field.value); err != nil {
			return failAt("invalid_field", field.path, upperFirst(err.Error())+".")
		}
	}
	return nil
}

func validateEnv(env map[string]string) error {
	total := 0
	for _, k := range sortedKeys(env) {
		if v := env[k]; !envPattern.MatchString(k) || strings.ContainsRune(v, 0) {
			return failAt("invalid_field", "spec.env."+k, "An environment key is a POSIX name and its value carries no NUL byte.")
		}
		if ReservedEnv(k) {
			return failAt("reserved_prefix", "spec.env."+k, "That environment variable is set by the sandbox boundary.")
		}
		total += len(k) + len(env[k])
	}
	if total > maxEnvBytes {
		return failAt("invalid_field", "spec.env", "The environment is at most 32 KiB in total.")
	}
	return nil
}

func absoluteCleanPath(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// resourceFields and lifecycleFields give the quantity and duration fields a
// fixed order, so a manifest with two bad fields always names the same one.
func resourceFields(r *v1.Resources) []struct {
	path  string
	value *v1.Quantity
} {
	return []struct {
		path  string
		value *v1.Quantity
	}{{"spec.resources.cpu", &r.CPU}, {"spec.resources.memory", &r.Memory}, {"spec.resources.disk", &r.Disk}}
}

func lifecycleFields(l *v1.Lifecycle) []struct {
	path  string
	value *v1.Duration
} {
	return []struct {
		path  string
		value *v1.Duration
	}{{"spec.lifecycle.autoStop", &l.AutoStop}, {"spec.lifecycle.ttl", &l.TTL}, {"spec.lifecycle.autoDelete", &l.AutoDelete}}
}

// sortedKeys orders a map's keys so a refusal is the same one on every run.
// It takes any value type: a validation failure reads a map of strings and
// the unknown-field walk of design 003 reads a decoded document.
func sortedKeys[V any](m map[string]V) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.Sort(keys)
	return keys
}

func validKey(k string) bool {
	parts := strings.Split(k, "/")
	if len(parts) > 2 {
		return false
	}
	value := parts[len(parts)-1]
	if len(value) > maxNameLength || !labelPart.MatchString(value) {
		return false
	}
	if len(parts) == 2 {
		if len(parts[0]) > 253 {
			return false
		}
		for s := range strings.SplitSeq(parts[0], ".") {
			if len(s) > maxNameLength || !namePattern.MatchString(s) {
				return false
			}
		}
	}
	return true
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
