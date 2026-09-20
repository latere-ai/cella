// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	driver "latere.ai/x/cella/runtime"
)

func TestRenderPodAndClaim(t *testing.T) {
	h := newHarness(t)
	s := driver.CreateSpec{
		ID: "sbx_render", Name: "render", Owner: "alice@example.com", Image: image,
		Command: []string{"/bin/app"}, Args: []string{"--serve"},
		Env:       map[string]string{"B": "2", "A": "1"},
		Workdir:   "/workspace/sub",
		User:      "1234:5678",
		Resources: driver.Resources{CPU: "2", Memory: "512Mi", Disk: "20Gi"},
		Workspace: driver.Workspace{Path: "/data"},
	}
	pod, err := h.pod(s, h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	if c.Name != Container || c.Image != image {
		t.Fatalf("container %s image %s", c.Name, c.Image)
	}
	if !reflect.DeepEqual(c.Command, s.Command) || !reflect.DeepEqual(c.Args, s.Args) {
		t.Fatalf("command %v args %v", c.Command, c.Args)
	}
	if want := []corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}; !reflect.DeepEqual(c.Env, want) {
		t.Fatalf("env %v, want it sorted as %v", c.Env, want)
	}
	if c.WorkingDir != "/workspace/sub" {
		t.Fatalf("workdir %q", c.WorkingDir)
	}
	if got := *pod.Spec.SecurityContext.RunAsUser; got != 1234 {
		t.Fatalf("runAsUser %d", got)
	}
	if got := *pod.Spec.SecurityContext.RunAsGroup; got != 5678 || *pod.Spec.SecurityContext.FSGroup != 5678 {
		t.Fatalf("runAsGroup %d fsGroup %d", got, *pod.Spec.SecurityContext.FSGroup)
	}
	if got := c.Resources.Limits.Cpu().String(); got != "2" {
		t.Fatalf("cpu limit %s", got)
	}
	if got := c.Resources.Requests.Cpu().MilliValue(); got != 200 {
		t.Fatalf("cpu request %dm, want a tenth of the limit", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "512Mi" {
		t.Fatalf("memory request %s, want the limit", got)
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if !maps.Equal(mounts, map[string]string{"workspace": "/data", "tmp": "/tmp"}) {
		t.Fatalf("mounts %v", mounts)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != objectName(s.ID) {
		t.Fatalf("workspace volume %+v", pod.Spec.Volumes[0])
	}

	claim, err := h.claim(s, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "20Gi" {
		t.Fatalf("claim size %s", got.String())
	}
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("claim names a storage class with none configured: %v", *claim.Spec.StorageClassName)
	}
	if claim.Name != pod.Name {
		t.Fatalf("claim %s and pod %s are not one name", claim.Name, pod.Name)
	}
}

func TestRenderDefaults(t *testing.T) {
	h := newHarness(t)
	pod, err := h.pod(spec("sbx_defaults"), h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	if !reflect.DeepEqual(c.Command, keepAlive) || c.Args != nil {
		t.Fatalf("a spec with no command runs %v %v, want the keep-alive", c.Command, c.Args)
	}
	if c.WorkingDir != driver.DefaultWorkdir {
		t.Fatalf("workdir %q", c.WorkingDir)
	}
	if *pod.Spec.SecurityContext.RunAsUser != DefaultRunAsUser {
		t.Fatalf("uid %d", *pod.Spec.SecurityContext.RunAsUser)
	}
	if c.Resources.Limits.Cpu().String() != DefaultCPU || c.Resources.Limits.Memory().String() != DefaultMemory {
		t.Fatalf("limits %v", c.Resources.Limits)
	}
	claim, err := h.claim(spec("sbx_defaults"), h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != DefaultDisk {
		t.Fatalf("claim size %s", got.String())
	}
}

func TestPodCarriesTheBaseline(t *testing.T) {
	h := newHarness(t)
	pod, err := h.pod(spec("sbx_baseline"), h.clock.now(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := pod.Spec.Containers[0]
	for _, row := range []struct {
		field string
		got   any
		want  any
	}{
		{"securityContext.runAsNonRoot", *pod.Spec.SecurityContext.RunAsNonRoot, true},
		{"securityContext.seccompProfile.type", pod.Spec.SecurityContext.SeccompProfile.Type, corev1.SeccompProfileTypeRuntimeDefault},
		{"automountServiceAccountToken", *pod.Spec.AutomountServiceAccountToken, false},
		{"hostNetwork", pod.Spec.HostNetwork, false},
		{"hostPID", pod.Spec.HostPID, false},
		{"hostIPC", pod.Spec.HostIPC, false},
		{"shareProcessNamespace", *pod.Spec.ShareProcessNamespace, false},
		{"restartPolicy", pod.Spec.RestartPolicy, corev1.RestartPolicyNever},
		{"container.allowPrivilegeEscalation", *c.SecurityContext.AllowPrivilegeEscalation, false},
		{"container.privileged", *c.SecurityContext.Privileged, false},
		{"container.readOnlyRootFilesystem", *c.SecurityContext.ReadOnlyRootFilesystem, true},
		{"container.runAsNonRoot", *c.SecurityContext.RunAsNonRoot, true},
		{"container.capabilities.drop", c.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"}},
		{"container.seccompProfile.type", c.SecurityContext.SeccompProfile.Type, corev1.SeccompProfileTypeRuntimeDefault},
	} {
		if !reflect.DeepEqual(row.got, row.want) {
			t.Errorf("%s = %v, want %v", row.field, row.got, row.want)
		}
	}
	// The read-only root leaves exactly two writable paths.
	var writable []string
	for _, m := range c.VolumeMounts {
		if !m.ReadOnly {
			writable = append(writable, m.MountPath)
		}
	}
	if !reflect.DeepEqual(writable, []string{driver.DefaultWorkdir, "/tmp"}) {
		t.Errorf("writable mounts %v", writable)
	}
}

func TestSchedulingOptionsReachThePod(t *testing.T) {
	cs := newHarness(t).cs
	d, err := New(Options{
		Namespace: namespace, Client: cs, StorageClass: "fast",
		NodeSelector:     map[string]string{"pool": "workloads"},
		Tolerations:      []map[string]string{{"key": "dedicated", "value": "workloads", "effect": "NoSchedule"}, {"key": "spot", "effect": "NoExecute", "tolerationSeconds": "30"}},
		ImagePullSecrets: []string{"pull"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := d.pod(spec("sbx_sched"), time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(pod.Spec.NodeSelector, map[string]string{"pool": "workloads"}) {
		t.Fatalf("node selector %v", pod.Spec.NodeSelector)
	}
	want := []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "workloads", Effect: corev1.TaintEffectNoSchedule},
		{Key: "spot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr(int64(30))},
	}
	if !reflect.DeepEqual(pod.Spec.Tolerations, want) {
		t.Fatalf("tolerations %+v, want %+v", pod.Spec.Tolerations, want)
	}
	if !reflect.DeepEqual(pod.Spec.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "pull"}}) {
		t.Fatalf("pull secrets %v", pod.Spec.ImagePullSecrets)
	}
	claim, err := d.claim(spec("sbx_sched"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "fast" {
		t.Fatalf("storage class %v", claim.Spec.StorageClassName)
	}
}

func TestResourcesAreConsistent(t *testing.T) {
	h := newHarness(t)
	s := spec("sbx_consistent")
	s.Resources = driver.Resources{CPU: "1500m", Memory: "3Gi"}
	first, err := h.pod(s, h.clock.at, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.pod(s, h.clock.at, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("one spec rendered two different Pods")
	}
	req := first.Spec.Containers[0].Resources.Requests
	if got := req.Cpu().MilliValue(); got != 150 {
		t.Fatalf("cpu request %dm, want 150m", got)
	}
	// A ratio of one gives requests equal to limits, which is the cluster that
	// runs one sandbox per node.
	d, err := New(Options{Namespace: namespace, Client: h.cs, CPURequestRatio: 1, MemoryRequestRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	equal, err := d.resources(s.Resources)
	if err != nil {
		t.Fatal(err)
	}
	if equal.Requests.Cpu().Cmp(*equal.Limits.Cpu()) != 0 || equal.Requests.Memory().Cmp(*equal.Limits.Memory()) != 0 {
		t.Fatalf("ratio 1 gave %v and %v", equal.Requests, equal.Limits)
	}
	if _, err := New(Options{Client: h.cs, CPURequestRatio: 2}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a ratio above one was accepted: %v", err)
	}
}

func TestSpecFieldsAreRefused(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name  string
		spec  driver.CreateSpec
		wants error
	}{
		{"id", driver.CreateSpec{ID: "no spaces", Image: image}, driver.ErrInvalid},
		{"empty id", driver.CreateSpec{Image: image}, driver.ErrInvalid},
		{"image", driver.CreateSpec{ID: "sbx_a"}, driver.ErrInvalid},
		{"args without a command", driver.CreateSpec{ID: "sbx_a", Image: image, Args: []string{"x"}}, driver.ErrInvalid},
		{"negative ttl", driver.CreateSpec{ID: "sbx_a", Image: image, Lifecycle: driver.Lifecycle{TTL: -time.Second}}, driver.ErrInvalid},
		{"relative workspace", driver.CreateSpec{ID: "sbx_a", Image: image, Workspace: driver.Workspace{Path: "work"}}, driver.ErrInvalid},
		{"workspace traversal", driver.CreateSpec{ID: "sbx_a", Image: image, Workspace: driver.Workspace{Path: "/a/../b"}}, driver.ErrInvalid},
		{"workspace root", driver.CreateSpec{ID: "sbx_a", Image: image, Workspace: driver.Workspace{Path: "/"}}, driver.ErrInvalid},
		{"workdir", driver.CreateSpec{ID: "sbx_a", Image: image, Workdir: "sub"}, driver.ErrInvalid},
		{"root user", driver.CreateSpec{ID: "sbx_a", Image: image, User: "0"}, driver.ErrInvalid},
		{"user name", driver.CreateSpec{ID: "sbx_a", Image: image, User: "app"}, driver.ErrUnsupported},
		{"group", driver.CreateSpec{ID: "sbx_a", Image: image, User: "1000:root"}, driver.ErrInvalid},
		{"cpu", driver.CreateSpec{ID: "sbx_a", Image: image, Resources: driver.Resources{CPU: "lots"}}, driver.ErrInvalid},
		{"memory", driver.CreateSpec{ID: "sbx_a", Image: image, Resources: driver.Resources{Memory: "-1Gi"}}, driver.ErrInvalid},
		{"disk", driver.CreateSpec{ID: "sbx_a", Image: image, Resources: driver.Resources{Disk: "0"}}, driver.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.Create(t.Context(), tc.spec); !errors.Is(err, tc.wants) {
				t.Fatalf("Create = %v, want %v", err, tc.wants)
			}
		})
	}
}

func TestObjectNameIsDerived(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"sbx-plain", "sbx-plain"},
		{"sbx_01jabc", "sbx-01jabc-62b72489"},
		{"SBX_Upper", "sbx-upper-304d676f"},
	} {
		if got := objectName(tc.id); got != tc.want {
			t.Errorf("objectName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
	for _, id := range []string{"sbx_a", "sbx.a", "sbx-a", "SBX_A", "sbx_A"} {
		name := objectName(id)
		if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
			t.Errorf("objectName(%q) = %q, which is no object name: %v", id, name, problems)
		}
	}
	// Two ids that collapse to one shape keep two objects.
	if objectName("sbx_a") == objectName("sbx-a") {
		t.Fatalf("sbx_a and sbx-a share the object name %q", objectName("sbx_a"))
	}
}

func TestStampedIdentityIsLegal(t *testing.T) {
	h := newHarness(t)
	s := driver.CreateSpec{
		ID: "sbx_legal.1", Name: "a-name", Owner: "alice@example.com", Image: image,
		Labels: map[string]string{"team/owner": "platform", "a very long key that a label could never hold as a key": "x"},
	}
	labels, annotations, err := h.identity(s, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range labels {
		if problems := validation.IsQualifiedName(key); len(problems) > 0 {
			t.Errorf("label key %q: %v", key, problems)
		}
		if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
			t.Errorf("label %s value %q: %v", key, value, problems)
		}
	}
	for key := range annotations {
		if problems := validation.IsQualifiedName(key); len(problems) > 0 {
			t.Errorf("annotation key %q: %v", key, problems)
		}
	}
	if _, ok := labels[labelName]; !ok {
		t.Error("a legal name is not stamped as a label")
	}
	// An owner holds characters no label value may hold, so it is an
	// annotation; the user's labels hold keys no annotation key may hold, so
	// they travel inside the spec.
	if annotations[annOwner] != s.Owner {
		t.Errorf("owner annotation %q", annotations[annOwner])
	}
	if !strings.Contains(annotations[annSpec], "team/owner") {
		t.Error("the user's labels are not in the spec annotation")
	}
}

func TestNameTooLongForALabelIsAnnotationOnly(t *testing.T) {
	h := newHarness(t)
	s := spec("sbx_longname")
	s.Name = strings.Repeat("n", 64)
	labels, _, err := h.identity(s, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := labels[labelName]; ok {
		t.Fatal("a name too long for a label value was stamped as one")
	}
	state := h.created(t, s)
	if state.Name != s.Name {
		t.Fatalf("name reads back as %q", state.Name)
	}
}

func TestQuantityHelpers(t *testing.T) {
	if got := scale(0, 0.1); got != 1 {
		t.Fatalf("scale(0) = %d, want a request the scheduler can read", got)
	}
	if _, err := quantity("", "1Gi", "memory"); err != nil {
		t.Fatal(err)
	}
	if got := resource.NewMilliQuantity(scale(1000, 0.1), resource.DecimalSI).String(); got != "100m" {
		t.Fatalf("scaled cpu %s", got)
	}
}
