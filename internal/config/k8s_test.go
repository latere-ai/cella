// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/k8s"
)

// k8sEnv is the identity fixture with the Kubernetes driver selected.
func k8sEnv(t *testing.T, m map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{"CELLA_RUNTIME": "k8s"}
	maps.Copy(out, m)
	return identity(t, out)
}

func TestK8sDefaultsNameNoDeployment(t *testing.T) {
	c, err := Load(env(k8sEnv(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	o := c.K8s
	for _, row := range []struct {
		field string
		got   any
		want  any
	}{
		{"Namespace", o.Namespace, k8s.DefaultNamespace},
		{"RunAsUser", o.RunAsUser, k8s.DefaultRunAsUser},
		{"RunAsGroup", o.RunAsGroup, k8s.DefaultRunAsGroup},
		{"CPURequestRatio", o.CPURequestRatio, k8s.DefaultCPURequestRatio},
		{"MemoryRequestRatio", o.MemoryRequestRatio, k8s.DefaultMemoryRequestRatio},
		{"DefaultCPU", o.DefaultCPU, k8s.DefaultCPU},
		{"DefaultMemory", o.DefaultMemory, k8s.DefaultMemory},
		{"DefaultDisk", o.DefaultDisk, k8s.DefaultDisk},
		{"ReadyTimeout", o.ReadyTimeout, k8s.DefaultReadyTimeout},
		{"GracePeriod", o.GracePeriod, k8s.DefaultGracePeriod},
	} {
		if !reflect.DeepEqual(row.got, row.want) {
			t.Errorf("%s = %v, want the driver's own default %v", row.field, row.got, row.want)
		}
	}
	// Nothing that names one cluster has a default: an installation supplies
	// its kubeconfig, its storage class and its scheduling constraints.
	if o.Kubeconfig != "" || o.StorageClass != "" || o.NodeSelector != nil || o.Tolerations != nil || o.ImagePullSecrets != nil {
		t.Errorf("a cluster-shaped value carries a default: %+v", o)
	}
	// The driver's seams are not configuration: no variable reaches them, so
	// a deployment is described by the table above and nothing else.
	if o.Client != nil || o.REST != nil || o.Now != nil {
		t.Errorf("the environment set a seam of the driver: %+v", o)
	}
}

func TestK8sReadsEveryVariable(t *testing.T) {
	c, err := Load(env(k8sEnv(t, map[string]string{
		"CELLA_K8S_NAMESPACE":            "sandboxes",
		"CELLA_K8S_KUBECONFIG":           "/etc/cella/kubeconfig",
		"CELLA_K8S_STORAGE_CLASS":        "fast",
		"CELLA_K8S_NODE_SELECTOR":        "pool=workloads, arch=arm64",
		"CELLA_K8S_TOLERATIONS":          "dedicated=workloads:NoSchedule, spot:NoExecute, plain",
		"CELLA_K8S_IMAGE_PULL_SECRETS":   "one, two",
		"CELLA_K8S_RUN_AS_USER":          "2000",
		"CELLA_K8S_RUN_AS_GROUP":         "3000",
		"CELLA_K8S_CPU_REQUEST_RATIO":    "0.25",
		"CELLA_K8S_MEMORY_REQUEST_RATIO": "0.5",
		"CELLA_K8S_DEFAULT_CPU":          "2",
		"CELLA_K8S_DEFAULT_MEMORY":       "2Gi",
		"CELLA_K8S_DEFAULT_DISK":         "10Gi",
		"CELLA_K8S_READY_TIMEOUT":        "45s",
		"CELLA_K8S_GRACE_PERIOD":         "20s",
	})))
	if err != nil {
		t.Fatal(err)
	}
	o := c.K8s
	if o.Namespace != "sandboxes" || o.Kubeconfig != "/etc/cella/kubeconfig" || o.StorageClass != "fast" {
		t.Fatalf("names %+v", o)
	}
	if !maps.Equal(o.NodeSelector, map[string]string{"pool": "workloads", "arch": "arm64"}) {
		t.Fatalf("node selector %v", o.NodeSelector)
	}
	want := []map[string]string{
		{"key": "dedicated", "operator": "Equal", "value": "workloads", "effect": "NoSchedule"},
		{"key": "spot", "operator": "Exists", "effect": "NoExecute"},
		{"key": "plain", "operator": "Exists"},
	}
	if !reflect.DeepEqual(o.Tolerations, want) {
		t.Fatalf("tolerations %v, want %v", o.Tolerations, want)
	}
	if !reflect.DeepEqual(o.ImagePullSecrets, []string{"one", "two"}) {
		t.Fatalf("pull secrets %v", o.ImagePullSecrets)
	}
	if o.RunAsUser != 2000 || o.RunAsGroup != 3000 {
		t.Fatalf("uid %d gid %d", o.RunAsUser, o.RunAsGroup)
	}
	if o.CPURequestRatio != 0.25 || o.MemoryRequestRatio != 0.5 {
		t.Fatalf("ratios %v %v", o.CPURequestRatio, o.MemoryRequestRatio)
	}
	if o.DefaultCPU != "2" || o.DefaultMemory != "2Gi" || o.DefaultDisk != "10Gi" {
		t.Fatalf("sizes %+v", o)
	}
	if o.ReadyTimeout != 45*time.Second || o.GracePeriod != 20*time.Second {
		t.Fatalf("budgets %v %v", o.ReadyTimeout, o.GracePeriod)
	}
}

func TestK8sReportsEveryBadValue(t *testing.T) {
	for _, tc := range []struct{ name, value, wants string }{
		{"CELLA_K8S_NODE_SELECTOR", "pool", "each entry is key=value"},
		{"CELLA_K8S_NODE_SELECTOR", "=workloads", "each entry is key=value"},
		{"CELLA_K8S_TOLERATIONS", ":NoSchedule", "key[=value][:effect]"},
		{"CELLA_K8S_RUN_AS_USER", "0", "no sandbox runs as root"},
		{"CELLA_K8S_RUN_AS_USER", "root", "no sandbox runs as root"},
		{"CELLA_K8S_RUN_AS_GROUP", "-1", "no sandbox runs as root"},
		{"CELLA_K8S_CPU_REQUEST_RATIO", "2", "at most 1"},
		{"CELLA_K8S_CPU_REQUEST_RATIO", "0", "at most 1"},
		{"CELLA_K8S_MEMORY_REQUEST_RATIO", "half", "at most 1"},
		{"CELLA_K8S_READY_TIMEOUT", "soon", "not a duration"},
		{"CELLA_K8S_GRACE_PERIOD", "-1s", "positive"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			_, err := Load(env(k8sEnv(t, map[string]string{tc.name: tc.value})))
			if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("Load = %v, want a problem naming %s and %q", err, tc.name, tc.wants)
			}
		})
	}
}

func TestK8sVariablesAreReadOnlyForItsOwnRuntime(t *testing.T) {
	// A value left over from a cluster deployment does not hold back a node
	// that runs another backend.
	c, err := Load(env(identity(t, map[string]string{
		"CELLA_RUNTIME": "native", "CELLA_ALLOW_UNSAFE_NATIVE": "true",
		"CELLA_K8S_RUN_AS_USER": "0", "CELLA_K8S_NAMESPACE": "whatever",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if c.K8s.Namespace != "" {
		t.Fatalf("the cluster options were read for another runtime: %+v", c.K8s)
	}
}

// TestLoadK8sDisplay: the desktop reaches the driver from three variables,
// and the driver built from the loaded options declares Display and Input
// exactly when an image is named.
func TestLoadK8sDisplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		image   string
		limits  driver.Resources
		desktop bool
	}{
		{"none", nil, "", driver.Resources{}, false},
		{"image", map[string]string{"CELLA_K8S_DISPLAY_IMAGE": " registry.example/cella-display:v1 "},
			"registry.example/cella-display:v1", driver.Resources{}, true},
		{"image and limits", map[string]string{
			"CELLA_K8S_DISPLAY_IMAGE":  "cella-display:dev",
			"CELLA_K8S_DISPLAY_CPU":    "500m",
			"CELLA_K8S_DISPLAY_MEMORY": "1Gi",
		}, "cella-display:dev", driver.Resources{CPU: "500m", Memory: "1Gi"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(k8sEnv(t, tc.env)))
			if err != nil {
				t.Fatal(err)
			}
			if c.K8s.DisplayImage != tc.image || c.K8s.DisplayResources != tc.limits {
				t.Fatalf("display %q %+v, want %q %+v", c.K8s.DisplayImage, c.K8s.DisplayResources, tc.image, tc.limits)
			}
			opts := c.K8s
			opts.Client = fake.NewClientset()
			d, err := k8s.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Capabilities(); got.Display != tc.desktop || got.Input != tc.desktop {
				t.Fatalf("the driver declares Display %v Input %v, want %v", got.Display, got.Input, tc.desktop)
			}
		})
	}
}

// TestLoadK8sQuantities: a compute amount the driver would refuse at every
// create is refused at start, naming the variable.
func TestLoadK8sQuantities(t *testing.T) {
	for _, name := range []string{
		"CELLA_K8S_DEFAULT_CPU", "CELLA_K8S_DEFAULT_MEMORY", "CELLA_K8S_DEFAULT_DISK",
		"CELLA_K8S_DISPLAY_CPU", "CELLA_K8S_DISPLAY_MEMORY",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(k8sEnv(t, map[string]string{name: "lots"})))
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "a quantity") {
				t.Fatalf("Load = %v, want a problem naming %s", err, name)
			}
		})
	}
}

// TestK8sEgressVariables: the gateway's Pods and cluster DNS reach the
// driver from five variables, every default is the driver's own, the driver
// built from them declares the egress modes exactly when the gateway's
// selector is set, and each malformed or orphaned value is a start-up
// problem naming its variable.
func TestK8sEgressVariables(t *testing.T) {
	c, err := Load(env(k8sEnv(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.K8s.Gateway.Labels != nil || c.K8s.Gateway.Namespace != "" || c.K8s.Gateway.Ports != nil || c.K8s.DNS.Labels != nil || c.K8s.DNS.Namespace != "" {
		t.Fatalf("an unset peer carries a value of its own: %+v %+v", c.K8s.Gateway, c.K8s.DNS)
	}
	opts := c.K8s
	opts.Client = fake.NewClientset()
	d, err := k8s.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().Egress; len(got) != 0 {
		t.Fatalf("a driver with no gateway named declares %v", got)
	}

	c, err = Load(env(k8sEnv(t, map[string]string{
		"CELLA_GATEWAY":               "gateway.example.com:3128",
		"CELLA_K8S_GATEWAY_SELECTOR":  "app.kubernetes.io/name=gateway, tier=egress",
		"CELLA_K8S_GATEWAY_NAMESPACE": "gateways",
		"CELLA_K8S_GATEWAY_PORTS":     "3129, 8081",
		"CELLA_K8S_DNS_SELECTOR":      "k8s-app=coredns",
		"CELLA_K8S_DNS_NAMESPACE":     "dns",
	})))
	if err != nil {
		t.Fatal(err)
	}
	want := k8s.Peer{Namespace: "gateways", Labels: map[string]string{"app.kubernetes.io/name": "gateway", "tier": "egress"}, Ports: []int32{3129, 8081}}
	if !reflect.DeepEqual(c.K8s.Gateway, want) {
		t.Fatalf("the gateway is %+v, want %+v", c.K8s.Gateway, want)
	}
	if !reflect.DeepEqual(c.K8s.DNS, k8s.Peer{Namespace: "dns", Labels: map[string]string{"k8s-app": "coredns"}}) {
		t.Fatalf("DNS is %+v", c.K8s.DNS)
	}
	opts = c.K8s
	opts.Client = fake.NewClientset()
	if d, err = k8s.New(opts); err != nil {
		t.Fatal(err)
	}
	if got := d.Capabilities().Egress; len(got) != 3 {
		t.Fatalf("a driver with its gateway named declares %v, want the three modes", got)
	}

	gateway := map[string]string{"CELLA_GATEWAY": "gateway.example.com:3128", "CELLA_K8S_GATEWAY_SELECTOR": "app=gateway"}
	for _, tc := range []struct {
		env   map[string]string
		name  string
		wants string
	}{
		{map[string]string{"CELLA_K8S_GATEWAY_NAMESPACE": "gateways"}, "CELLA_K8S_GATEWAY_NAMESPACE", "without CELLA_K8S_GATEWAY_SELECTOR"},
		{map[string]string{"CELLA_K8S_GATEWAY_PORTS": "3128"}, "CELLA_K8S_GATEWAY_PORTS", "without CELLA_K8S_GATEWAY_SELECTOR"},
		{map[string]string{"CELLA_K8S_DNS_SELECTOR": "k8s-app=dns"}, "CELLA_K8S_DNS_SELECTOR", "without CELLA_K8S_GATEWAY_SELECTOR"},
		{map[string]string{"CELLA_K8S_DNS_NAMESPACE": "dns"}, "CELLA_K8S_DNS_NAMESPACE", "without CELLA_K8S_GATEWAY_SELECTOR"},
		{map[string]string{"CELLA_K8S_GATEWAY_SELECTOR": "app=gateway"}, "CELLA_GATEWAY", "pointed at it"},
		{with(gateway, "CELLA_K8S_GATEWAY_SELECTOR", "app"), "CELLA_K8S_GATEWAY_SELECTOR", "each entry is key=value"},
		{with(gateway, "CELLA_K8S_GATEWAY_SELECTOR", "app=no spaces"), "CELLA_K8S_GATEWAY_SELECTOR", "not a label value"},
		{with(gateway, "CELLA_K8S_GATEWAY_NAMESPACE", "Gateways"), "CELLA_K8S_GATEWAY_NAMESPACE", "not a namespace name"},
		{with(gateway, "CELLA_K8S_GATEWAY_PORTS", "3128,proxy"), "CELLA_K8S_GATEWAY_PORTS", "not a port"},
		{with(gateway, "CELLA_K8S_DNS_SELECTOR", "bad key=x"), "CELLA_K8S_DNS_SELECTOR", "not a label key"},
		{with(gateway, "CELLA_K8S_DNS_NAMESPACE", "kube_system"), "CELLA_K8S_DNS_NAMESPACE", "not a namespace name"},
	} {
		t.Run(tc.name+" "+tc.wants, func(t *testing.T) {
			_, err := Load(env(k8sEnv(t, tc.env)))
			if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("Load = %v, want a problem naming %s and %q", err, tc.name, tc.wants)
			}
		})
	}
}

// with is a copy of an environment with one variable set.
func with(base map[string]string, name, value string) map[string]string {
	out := maps.Clone(base)
	out[name] = value
	return out
}
