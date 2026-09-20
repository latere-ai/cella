// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"latere.ai/x/cella/manifest"
	driver "latere.ai/x/cella/runtime"
)

// prefix qualifies every label and annotation key the driver writes. The
// domain is the contract's own, declared once in the manifest package, so a
// key this driver stamps and a key the API refuses from a body are the same
// namespace.
const prefix = manifest.ReservedKeyDomain + "/"

// The label half of the stamped identity: what a cluster can select on.
const (
	labelManagedBy = prefix + "managed-by"
	labelID        = prefix + "id"
	labelName      = prefix + "name"
	managedValue   = "cella"
)

// The annotation half: what is read back rather than selected on. Each instant
// the reaper reads is its own key, so a sweep never parses the spec.
const (
	annOwner      = prefix + "owner"
	annCreatedAt  = prefix + "created-at"
	annStartedAt  = prefix + "started-at"
	annStoppedAt  = prefix + "stopped-at"
	annActivityAt = prefix + "last-activity-at"
	annExpiresAt  = prefix + "expires-at"
	annAutoStop   = prefix + "auto-stop"
	annAutoDelete = prefix + "auto-delete"
	annImage      = prefix + "image"
	annSpec       = prefix + "spec"
)

// managedSelector narrows every list to the objects this contract owns.
const managedSelector = labelManagedBy + "=" + managedValue

// stamp is the timestamp format: RFC 3339 with nanoseconds, so a stamp taken
// twice in one millisecond is two instants and Touch is observable.
const stamp = time.RFC3339Nano

var (
	// validID is the id shape the driver accepts. The bound is the label value
	// limit, since the id is carried as one.
	validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	// dns1123 is a legal object name.
	dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// labelValue is what a label may hold.
	labelValue = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)
)

// objectName derives the name of the claim and the Pod from a sandbox id. An
// id that is already a legal object name is its own name; any other is
// lowercased with its illegal bytes replaced and suffixed with eight hex
// digits of its hash, so two ids never land on one object.
func objectName(id string) string {
	if dns1123.MatchString(id) {
		return id
	}
	lower := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, id)
	sum := sha256.Sum256([]byte(id))
	return strings.Trim(lower, "-") + "-" + hex.EncodeToString(sum[:4])
}

// workspacePath is where the claim is mounted inside the sandbox.
func workspacePath(s driver.CreateSpec) string {
	if s.Workspace.Path != "" {
		return s.Workspace.Path
	}
	return driver.DefaultWorkdir
}

// checkPath refuses anything that is not an absolute, clean, non-root path.
func checkPath(p string) error {
	if p == "" || !path.IsAbs(p) || strings.Contains(p, "\x00") ||
		path.Clean(p) != p || p == "/" || slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("%w: path %q", driver.ErrInvalid, p)
	}
	return nil
}

// under reports whether p is dir or below it.
func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// validate refuses a spec the cluster cannot render before any object exists.
func (d *Driver) validate(s driver.CreateSpec) error {
	if !validID.MatchString(s.ID) {
		return fmt.Errorf("%w: id %q", driver.ErrInvalid, s.ID)
	}
	if s.Image == "" {
		return fmt.Errorf("%w: a container needs an image", driver.ErrInvalid)
	}
	if len(s.Args) > 0 && len(s.Command) == 0 {
		return fmt.Errorf("%w: args without a command", driver.ErrInvalid)
	}
	if s.Lifecycle.TTL < 0 || s.Lifecycle.AutoStop < 0 || s.Lifecycle.AutoDelete < 0 {
		return fmt.Errorf("%w: a negative lifecycle duration", driver.ErrInvalid)
	}
	ws := workspacePath(s)
	if err := checkPath(ws); err != nil {
		return err
	}
	if s.Workdir != "" {
		if err := checkPath(s.Workdir); err != nil {
			return err
		}
	}
	if _, _, err := runAs(s.User); err != nil {
		return err
	}
	if _, err := d.resources(s.Resources); err != nil {
		return err
	}
	_, err := d.diskQuantity(s.Resources.Disk)
	return err
}

// runAs reads a uid or a uid:gid pair. A user name has no numeric field on a
// Pod, and uid 0 contradicts the baseline's runAsNonRoot, so both are refused
// rather than silently dropped.
func runAs(user string) (uid, gid int64, err error) {
	if user == "" {
		return 0, 0, nil
	}
	first, second, pair := strings.Cut(user, ":")
	uid, err = strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: a Pod runs as a uid, not the user name %q", driver.ErrUnsupported, user)
	}
	if uid <= 0 {
		return 0, 0, fmt.Errorf("%w: uid %d, and a sandbox never runs as root", driver.ErrInvalid, uid)
	}
	gid = uid
	if pair {
		if gid, err = strconv.ParseInt(second, 10, 64); err != nil || gid <= 0 {
			return 0, 0, fmt.Errorf("%w: gid in %q", driver.ErrInvalid, user)
		}
	}
	return uid, gid, nil
}

// resources renders the container's requests and limits. Limits are the spec's
// as written; requests are the configured fraction of them.
func (d *Driver) resources(r driver.Resources) (corev1.ResourceRequirements, error) {
	cpu, err := quantity(r.CPU, d.opts.DefaultCPU, "cpu")
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	mem, err := quantity(r.Memory, d.opts.DefaultMemory, "memory")
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	cpuReq := resource.NewMilliQuantity(scale(cpu.MilliValue(), d.opts.CPURequestRatio), resource.DecimalSI)
	memReq := resource.NewQuantity(scale(mem.Value(), d.opts.MemoryRequestRatio), resource.BinarySI)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: *cpuReq, corev1.ResourceMemory: *memReq},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
	}, nil
}

// scale multiplies by a ratio, rounding up, and never returns zero for a
// non-zero value: a request of zero is a request the scheduler ignores.
func scale(v int64, ratio float64) int64 {
	return max(int64(math.Ceil(float64(v)*ratio)), 1)
}

func quantity(value, fallback, field string) (resource.Quantity, error) {
	if value == "" {
		value = fallback
	}
	q, err := resource.ParseQuantity(value)
	if err != nil || q.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%w: %s %q", driver.ErrInvalid, field, value)
	}
	return q, nil
}

func (d *Driver) diskQuantity(disk string) (resource.Quantity, error) {
	return quantity(disk, d.opts.DefaultDisk, "disk")
}

// identity is the stamped record of one sandbox: the label half, the
// annotation half, and the spec the Pod is rendered from.
func (d *Driver) identity(s driver.CreateSpec, createdAt time.Time) (labels, annotations map[string]string, err error) {
	labels = map[string]string{labelManagedBy: managedValue, labelID: s.ID}
	if labelValue.MatchString(s.Name) {
		labels[labelName] = s.Name
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil, nil, err
	}
	annotations = map[string]string{
		annOwner:      s.Owner,
		annImage:      s.Image,
		annCreatedAt:  createdAt.Format(stamp),
		annActivityAt: createdAt.Format(stamp),
		annSpec:       string(encoded),
	}
	maps.Copy(annotations, lifecycleAnnotations(s.Lifecycle, createdAt))
	return labels, annotations, nil
}

// lifecycleAnnotations renders the deadline half the reaper reads. A zero
// duration is an absent key, so "never" is the absence of a deadline rather
// than a value that means never.
func lifecycleAnnotations(l driver.Lifecycle, createdAt time.Time) map[string]string {
	out := map[string]string{}
	if l.TTL > 0 {
		out[annExpiresAt] = createdAt.Add(l.TTL).Format(stamp)
	}
	if l.AutoStop > 0 {
		out[annAutoStop] = l.AutoStop.String()
	}
	if l.AutoDelete > 0 {
		out[annAutoDelete] = l.AutoDelete.String()
	}
	return out
}

// claim renders the workspace claim, the durable half of a sandbox.
func (d *Driver) claim(s driver.CreateSpec, now time.Time) (*corev1.PersistentVolumeClaim, error) {
	size, err := d.diskQuantity(s.Resources.Disk)
	if err != nil {
		return nil, err
	}
	labels, annotations, err := d.identity(s, now)
	if err != nil {
		return nil, err
	}
	annotations[annStartedAt] = now.Format(stamp)
	pvc := &corev1.PersistentVolumeClaim{
		Name: objectName(s.ID), Namespace: d.opts.Namespace,
		Labels: labels, Annotations: annotations,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if d.opts.StorageClass != "" {
		class := d.opts.StorageClass
		pvc.Spec.StorageClassName = &class
	}
	return pvc, nil
}

// keepAlive is the command of a sandbox whose manifest names none: a process
// that holds the Pod open for Exec and leaves on the first TERM.
var keepAlive = []string{"/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 60 & wait $!; done"}

// pod renders the compute half. Every field of the security baseline is set
// here rather than inherited, so a cluster with permissive defaults grants a
// sandbox nothing extra.
func (d *Driver) pod(s driver.CreateSpec, now time.Time) (*corev1.Pod, error) {
	if err := d.validate(s); err != nil {
		return nil, err
	}
	name := objectName(s.ID)
	labels, annotations, err := d.identity(s, now)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{annSpec, annActivityAt, annExpiresAt, annAutoStop, annAutoDelete} {
		delete(annotations, key)
	}
	uid, gid, err := runAs(s.User)
	if err != nil {
		return nil, err
	}
	if uid == 0 {
		uid, gid = d.opts.RunAsUser, d.opts.RunAsGroup
	}
	limits, err := d.resources(s.Resources)
	if err != nil {
		return nil, err
	}
	ws := workspacePath(s)
	workdir := s.Workdir
	if workdir == "" {
		workdir = ws
	}
	command := s.Command
	args := s.Args
	if len(command) == 0 {
		command, args = keepAlive, nil
	}
	pod := &corev1.Pod{
		Name: name, Namespace: d.opts.Namespace,
		Labels: labels, Annotations: annotations,
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr(false),
			EnableServiceLinks:            ptr(false),
			HostNetwork:                   false,
			HostPID:                       false,
			HostIPC:                       false,
			ShareProcessNamespace:         ptr(false),
			TerminationGracePeriodSeconds: ptr(int64(d.opts.GracePeriod / time.Second)),
			NodeSelector:                  maps.Clone(d.opts.NodeSelector),
			Tolerations:                   tolerations(d.opts.Tolerations),
			ImagePullSecrets:              pullSecrets(d.opts.ImagePullSecrets),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(uid),
				RunAsGroup:     ptr(gid),
				FSGroup:        ptr(gid),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:       Container,
				Image:      s.Image,
				Command:    slices.Clone(command),
				Args:       slices.Clone(args),
				Env:        env(s.Env),
				WorkingDir: workdir,
				Resources:  limits,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: ws},
					{Name: "tmp", MountPath: "/tmp"},
				},
				SecurityContext: hardened(),
			}},
			Volumes: []corev1.Volume{
				{Name: "workspace",
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}},
				{Name: "tmp", EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
	}
	return pod, nil
}

// hardened is the container half of the baseline: no new privileges, no
// capabilities, and a root file system nothing writes to. The workspace and
// /tmp are the writable mounts.
func hardened() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Privileged:               ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		RunAsNonRoot:             ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// env renders the environment sorted, so two renders of one spec are equal
// objects and a diff of a Pod is a diff of the sandbox.
func env(m map[string]string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tolerations reads the operator's generic maps into the cluster's type.
func tolerations(in []map[string]string) []corev1.Toleration {
	var out []corev1.Toleration
	for _, t := range in {
		tol := corev1.Toleration{
			Key:      t["key"],
			Operator: corev1.TolerationOperator(t["operator"]),
			Value:    t["value"],
			Effect:   corev1.TaintEffect(t["effect"]),
		}
		if tol.Operator == "" {
			tol.Operator = corev1.TolerationOpExists
			if tol.Value != "" {
				tol.Operator = corev1.TolerationOpEqual
			}
		}
		if seconds, err := strconv.ParseInt(t["tolerationSeconds"], 10, 64); err == nil {
			tol.TolerationSeconds = &seconds
		}
		out = append(out, tol)
	}
	return out
}

func pullSecrets(names []string) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, n := range names {
		out = append(out, corev1.LocalObjectReference{Name: n})
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// specOf reads the CreateSpec back from the claim. It is the rendering input
// of Start, the environment and workdir of Exec, and the user's labels of
// Inspect.
func specOf(pvc *corev1.PersistentVolumeClaim) (driver.CreateSpec, error) {
	raw, ok := pvc.Annotations[annSpec]
	if !ok {
		return driver.CreateSpec{}, fmt.Errorf("%w: claim %s carries no spec annotation", driver.ErrInvalid, pvc.Name)
	}
	var s driver.CreateSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return driver.CreateSpec{}, fmt.Errorf("%w: claim %s spec annotation: %w", driver.ErrInvalid, pvc.Name, err)
	}
	return s, nil
}

// parseStamp reads one stamped instant; an absent or unreadable value is the
// zero time, which every caller already reads as "not recorded".
func parseStamp(annotations map[string]string, key string) time.Time {
	t, err := time.Parse(stamp, annotations[key])
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseDuration(annotations map[string]string, key string) time.Duration {
	d, err := time.ParseDuration(annotations[key])
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// sortedEnv renders a merged environment as the argv assignments Exec passes
// to env, sorted so one request builds one command line.
func sortedEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
