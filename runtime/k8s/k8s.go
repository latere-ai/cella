// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package k8s runs each sandbox as one PersistentVolumeClaim and one Pod in a
// single namespace of a Kubernetes cluster. The claim is the sandbox and
// outlives every Pod: Stop deletes the Pod and keeps the claim, Start renders a
// new Pod from the spec the claim carries, Delete removes both. Identity and
// every lifecycle instant are stamped on the two objects, so List and Inspect
// rebuild State from the cluster with no store behind them.
//
// The phase is a function of the two objects:
//
//	claim absent                            ErrNotFound
//	claim deleting                          Deleting
//	claim status Lost                       Lost
//	Pod absent                              Stopped
//	Pod deleting                            Stopping
//	Pod Pending                             Pending
//	Pod Running, not Ready                  Starting
//	Pod Running and Ready                   Running
//	Pod Failed, or a container exited != 0  Failed
//	Pod Succeeded                           Stopped
package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	driver "latere.ai/x/cella/runtime"
)

// Defaults every option falls back to. None of them names a deployment: the
// namespace is this project's own name, the uid is the conventional first
// unprivileged account, and every cluster-shaped value (storage class, node
// selector, tolerations, pull secrets) is empty until an operator sets it.
const (
	DefaultNamespace          = "cella"
	DefaultRunAsUser          = int64(1000)
	DefaultRunAsGroup         = int64(1000)
	DefaultCPURequestRatio    = 0.1
	DefaultMemoryRequestRatio = 1.0
	DefaultCPU                = "1"
	DefaultMemory             = "1Gi"
	DefaultDisk               = "5Gi"
	DefaultReadyTimeout       = 90 * time.Second
	DefaultGracePeriod        = 10 * time.Second
)

// Container is the name of the container a sandbox's workload runs in, and the
// container Exec, Logs and the archive transfers address.
const Container = "main"

// DisplayContainer is the name of the container the desktop of spec 023 runs
// in, beside the workload and sharing its /tmp. Every command on the screen
// addresses this container, because the tools live in its image and not in the
// sandbox's.
const DisplayContainer = "display"

// Options configures one driver against one namespace. The zero value is
// usable: every field falls back to the constants above.
type Options struct {
	// Namespace holds the claims and Pods this driver owns.
	Namespace string
	// Kubeconfig is the path to a kubeconfig file. Empty means the in-cluster
	// configuration and only that: a control plane outside a cluster with no
	// path configured fails at New rather than reaching whichever cluster the
	// operator's shell happens to point at.
	Kubeconfig string
	// StorageClass provisions the workspace claim; empty is the cluster's own
	// default class.
	StorageClass string
	// NodeSelector and Tolerations are the scheduling constraints an
	// installation puts on every sandbox, carried as plain maps so no name of
	// one cluster's node pools reaches this package. Each toleration map takes
	// the keys key, operator, value, effect and tolerationSeconds.
	NodeSelector map[string]string
	Tolerations  []map[string]string
	// ImagePullSecrets are the secret names every sandbox Pod references.
	ImagePullSecrets []string
	// RunAsUser and RunAsGroup are the uid and gid a sandbox runs as when its
	// manifest names no user. RunAsGroup is also the fsGroup, so the claim is
	// writable by the workload.
	RunAsUser, RunAsGroup int64
	// CPURequestRatio and MemoryRequestRatio size the requests as a fraction
	// of the limits. Requests below limits pack idle sandboxes densely; a
	// cluster that wants requests equal to limits sets both to 1.
	CPURequestRatio, MemoryRequestRatio float64
	// DefaultCPU, DefaultMemory and DefaultDisk are the limits and the claim
	// size for a spec that names none.
	DefaultCPU, DefaultMemory, DefaultDisk string
	// ReadyTimeout bounds how long Create and Start wait for a Pod to be
	// ready, and how long Stop and Delete wait for one to be gone.
	ReadyTimeout time.Duration
	// GracePeriod is the Pod's termination grace period.
	GracePeriod time.Duration
	// DisplayImage is the image the desktop container runs, the cella-display
	// image of spec 014. It has no default: an image reference names a
	// registry, and this package fixes no coordinate of any installation.
	// Empty means this driver declares no Display and no Input, so a manifest
	// that asks for a desktop is refused at resolve rather than at create.
	DisplayImage string
	// DisplayResources are the limits the desktop container runs under,
	// separate from the workload's because an X server sized by the sandbox's
	// own request would take the workload's memory. Empty falls back to the
	// driver's defaults for a sandbox that names none.
	DisplayResources driver.Resources
	// Gateway is the egress gateway's Pods and DNS the cluster's resolver,
	// the two peers a sandbox's network rule admits egress to (spec 018).
	// With no gateway labels the driver confines no egress and declares no
	// egress mode: a sandbox confined to DNS alone would reach nothing while
	// its condition said the boundary was not enforced. The gateway's
	// namespace defaults to Namespace and its ports to the two doors'
	// listen defaults; DNS defaults to kube-system, k8s-app=kube-dns, 53.
	Gateway, DNS Peer
	// FillImage is the image a volume's fill stages its program from: one
	// whose entrypoint is cellad, which the release image of the same
	// version is (spec 019). It has no default, as DisplayImage has none.
	// Empty means this driver fills no volume from an image, so a volume
	// that names one is refused at resolve.
	FillImage string
	// VolumeClass provisions each volume's claim; empty is StorageClass,
	// the workspace's. A volume shared read-only by sandboxes on several
	// nodes needs a class whose volumes attach to no single node.
	VolumeClass string
	// FillTimeout bounds one fill of a volume from an image, the pull and
	// the copy together. It is a seam with DefaultFillTimeout behind it,
	// and no variable sets it.
	FillTimeout time.Duration
	// TrustRoots are the public roots written into a sandbox's trust file
	// ahead of the gateway's authority, as egress.LoadRoots read them from
	// the process's own system bundle or SSL_CERT_FILE. No CELLA_ variable
	// sets them. The gateway tunnels every host no secret is bound to, and
	// the workload verifies such a host against these roots; none writes the
	// authority alone.
	TrustRoots []byte

	// The three fields below are seams, not configuration: no variable sets
	// them, internal/config leaves them zero, and a deployment is described
	// entirely by the fields above.
	//
	// Client replaces the clientset New would build, for a caller that
	// already holds an authenticated connection and for tests. REST is the
	// configuration the exec and port forwarding subresources dial; a driver
	// with a Client and no REST serves every call but Exec, the archive
	// transfers and Dial. Now is the clock every stamp reads.
	Client kubernetes.Interface
	REST   *rest.Config
	Now    func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.RunAsUser == 0 {
		o.RunAsUser = DefaultRunAsUser
	}
	if o.RunAsGroup == 0 {
		o.RunAsGroup = DefaultRunAsGroup
	}
	if o.CPURequestRatio <= 0 {
		o.CPURequestRatio = DefaultCPURequestRatio
	}
	if o.MemoryRequestRatio <= 0 {
		o.MemoryRequestRatio = DefaultMemoryRequestRatio
	}
	if o.DefaultCPU == "" {
		o.DefaultCPU = DefaultCPU
	}
	if o.DefaultMemory == "" {
		o.DefaultMemory = DefaultMemory
	}
	if o.DefaultDisk == "" {
		o.DefaultDisk = DefaultDisk
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.GracePeriod <= 0 {
		o.GracePeriod = DefaultGracePeriod
	}
	if o.VolumeClass == "" {
		o.VolumeClass = o.StorageClass
	}
	if o.FillTimeout <= 0 {
		o.FillTimeout = DefaultFillTimeout
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o.withPeerDefaults()
}

// Driver implements runtime.Driver against one namespace of one cluster.
type Driver struct {
	opts    Options
	cs      kubernetes.Interface
	stream  streamer
	forward forwarder
	// meter reads the metrics API, with a cluster connection behind it like
	// the two seams above. inPlace and memory are what Preflight found the
	// cluster to serve and the service account to hold: the resize
	// subresource, and that with the metrics API (spec 078).
	meter   meter
	inPlace atomic.Bool
	memory  atomic.Bool
}

var _ driver.Driver = (*Driver)(nil)

// New builds the driver. It reaches no cluster: the connection is opened by
// the first call, so a misconfigured deployment fails at Preflight with a
// message about the cluster rather than at construction.
func New(opts Options) (*Driver, error) {
	opts = opts.withDefaults()
	if opts.CPURequestRatio > 1 || opts.MemoryRequestRatio > 1 {
		return nil, fmt.Errorf("%w: request ratios are fractions of the limit", driver.ErrInvalid)
	}
	for _, peer := range []struct {
		name string
		p    Peer
	}{{"gateway", opts.Gateway}, {"DNS", opts.DNS}} {
		if err := peer.p.Check(); err != nil {
			return nil, fmt.Errorf("%w: the %s peer: %w", driver.ErrInvalid, peer.name, err)
		}
	}
	cs := opts.Client
	cfg := opts.REST
	if cs == nil {
		var err error
		if cfg == nil {
			cfg, err = restConfig(opts.Kubeconfig)
			if err != nil {
				return nil, err
			}
		}
		if cs, err = kubernetes.NewForConfig(cfg); err != nil {
			return nil, fmt.Errorf("kubernetes client: %w", err)
		}
	}
	d := &Driver{opts: opts, cs: cs}
	if cfg != nil {
		d.stream = &spdy{cfg: cfg, cs: cs, namespace: opts.Namespace}
		forward, err := newPortForward(cfg, opts.Namespace)
		if err != nil {
			return nil, err
		}
		d.forward = forward
		d.meter = &restMeter{client: cs.CoreV1().RESTClient(), namespace: opts.Namespace}
	}
	return d, nil
}

// restConfig reads the kubeconfig at path, or the in-cluster configuration
// when path is empty. The default loading rules are deliberately not used:
// they read KUBECONFIG and the operator's home directory, which would let a
// control plane with no configuration at all drive somebody's own cluster.
func restConfig(path string) (*rest.Config, error) {
	if path == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("no kubeconfig configured and not running in a cluster: %w", err)
		}
		return withRate(cfg), nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return withRate(cfg), nil
}

// ClientQPS and ClientBurst are the rate the driver's client asks the API
// server at. The library's own default, five a second with a burst of ten, is
// a command-line tool's: every read of a sandbox is a pod get, so a control
// plane serving a few dozen callers queues behind its own limiter, and a call
// that carries a short deadline, an exec's timeout among them, fails waiting
// for a token rather than for the cluster.
const (
	ClientQPS   = 50
	ClientBurst = 100
)

// withRate gives a configuration that names no rate the driver's own. A
// kubeconfig carries no rate, so this is every configuration the driver
// builds; one handed in through Options.REST is the caller's and is not
// passed through here.
func withRate(cfg *rest.Config) *rest.Config {
	if cfg.QPS == 0 {
		cfg.QPS = ClientQPS
	}
	if cfg.Burst == 0 {
		cfg.Burst = ClientBurst
	}
	return cfg
}

func (d *Driver) Name() string      { return "k8s" }
func (d *Driver) Isolation() string { return "container" }

// Capabilities declares only what this driver enforces today. Pool, because
// the claim's label is the cluster's own mutex: the guarded patch of an
// adoption tests it, so of two adopters one writes and the other is told the
// entry is gone. Mesh, because a mesh is a NetworkPolicy and a headless
// Service the driver writes per mesh. Attach, because the exec subresource
// carries a terminal and stdin. Dial, because the port forwarding
// subresource reaches a declared port from inside the Pod. Display and Input
// follow the display image: with none configured there is no desktop to
// give, and declaring one would push the refusal from resolve, where it names
// the field, to create, where it names nothing. Egress follows the gateway:
// with its Pods named, every sandbox runs under a NetworkPolicy that leaves
// the gateway as the only way out, which is the one rule all three modes
// need; with none, nothing confines egress and no mode is declared. Autosize
// of CPU always, because a limit above the request is a field of every Pod;
// of memory once Preflight has found the resize subresource and the metrics
// API served and granted, because raising a limit needs the first and
// deciding to needs the second; of the workspace not yet. Volumes always,
// because a volume is a claim the driver writes like a workspace, and their
// image source with the fill image, whose program copies an image's files
// into a claim: with none configured no fill can run. Resize and snapshots
// each land with the slice that builds them.
func (d *Driver) Capabilities() driver.Capabilities {
	desktop := d.opts.DisplayImage != ""
	caps := driver.Capabilities{Files: true, Pool: true, Mesh: true, Attach: true, Dial: true, Display: desktop, Input: desktop,
		Volumes: true, VolumeImages: d.opts.FillImage != ""}
	if d.confines() {
		caps.Egress = slices.Clone(egressModes)
	}
	caps.Autosize.CPU = true
	caps.Autosize.Memory = d.memory.Load()
	return caps
}

// verbs are the accesses the driver uses, checked one review each so a missing
// rule is named before the first sandbox rather than at the first create.
// group is the API group the resource belongs to, empty for the core one, so
// a review asks about the object the driver actually writes. pods/exec takes
// two: the executor's WebSocket upgrade is a GET, which the API server reads
// as get (and from Kubernetes 1.35 as create too), and its SPDY fallback is a
// POST, which it reads as create.
var verbs = []struct{ group, resource, subresource, verb string }{
	{"", "pods", "", "get"}, {"", "pods", "", "list"}, {"", "pods", "", "create"},
	{"", "pods", "", "delete"}, {"", "pods", "", "patch"},
	{"", "pods", "exec", "create"}, {"", "pods", "exec", "get"}, {"", "pods", "log", "get"},
	// Dial: the WebSocket session is a GET and the SPDY upgrade it falls
	// back to a POST, which the API server authorizes as get and create.
	{"", "pods", "portforward", "get"}, {"", "pods", "portforward", "create"},
	{"", "persistentvolumeclaims", "", "get"}, {"", "persistentvolumeclaims", "", "list"},
	{"", "persistentvolumeclaims", "", "create"}, {"", "persistentvolumeclaims", "", "delete"},
	{"", "persistentvolumeclaims", "", "patch"},
	// The mesh of spec 022: one headless Service and one NetworkPolicy per
	// mesh, made with its first member and removed with its last. The rule
	// of each sandbox (spec 018) is a NetworkPolicy too, replaced by a
	// delete and a create, so it needs no verb beyond these two.
	{"", "services", "", "create"}, {"", "services", "", "delete"},
	{networkGroup, "networkpolicies", "", "create"}, {networkGroup, "networkpolicies", "", "delete"},
	// The workload token of spec 006: one Secret per sandbox, projected on
	// /run/cella, replaced in place on a rotation and removed with the
	// sandbox. It holds the identity, the trust file, and the values of the
	// sandbox's environment and its gateway credential, which the claim's
	// record leaves out and the workload container references one key per
	// variable. A mounted secret's value is never in it: that stays
	// sealed in the store and the sandbox holds a placeholder for it.
	{"", "secrets", "", "create"}, {"", "secrets", "", "get"},
	{"", "secrets", "", "update"}, {"", "secrets", "", "delete"},
}

// networkGroup is the API group a NetworkPolicy lives in.
const networkGroup = "networking.k8s.io"

// Preflight proves the cluster answers, the namespace holds the objects, the
// service account may act on them, and the storage class exists.
func (d *Driver) Preflight(ctx context.Context) error {
	if err := d.Ready(ctx); err != nil {
		return err
	}
	var missing []string
	for _, v := range verbs {
		allowed, err := d.may(ctx, v)
		if err != nil {
			return err
		}
		if !allowed {
			missing = append(missing, v.verb+" "+resourceName(v.resource, v.subresource))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("namespace %s: the service account may not %s", d.opts.Namespace, strings.Join(missing, ", "))
	}
	for _, class := range []string{d.opts.StorageClass, d.opts.VolumeClass} {
		if class == "" {
			continue
		}
		if _, err := d.cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("storage class %s: %w", class, err)
		}
	}
	// What the driver can size is read last and fails nothing: a cluster
	// or a Role without it runs every sandbox at a fixed size, as before.
	d.detectSizing(ctx)
	return nil
}

func resourceName(resource, subresource string) string {
	if subresource == "" {
		return resource
	}
	return resource + "/" + subresource
}

// Ready is the cheap half of Preflight: one namespaced list of one object,
// which fails when the cluster is unreachable, the credentials are stale, or
// the namespace is gone.
func (d *Driver) Ready(ctx context.Context) error {
	_, err := d.cs.CoreV1().PersistentVolumeClaims(d.opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedSelector, Limit: 1,
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("namespace %s: %w", d.opts.Namespace, err)
		}
		return fmt.Errorf("cluster: %w", err)
	}
	return nil
}

// mapErr turns a cluster answer into the contract's sentinel.
func mapErr(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return driver.ErrNotFound
	case apierrors.IsAlreadyExists(err):
		return driver.ErrAlreadyExists
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// retryable reports whether a failed write is worth rebuilding and repeating.
// A lost compare-and-swap reaches the caller as a conflict from a real API
// server and as a plain patch error from a test double, so anything that is
// not a settled refusal is retried.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	return !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) &&
		!apierrors.IsUnauthorized(err) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}
