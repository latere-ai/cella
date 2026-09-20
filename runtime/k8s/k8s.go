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
	"strings"
	"time"

	authv1 "k8s.io/api/authorization/v1"
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

	// The three fields below are seams, not configuration: no variable sets
	// them, internal/config leaves them zero, and a deployment is described
	// entirely by the fields above.
	//
	// Client replaces the clientset New would build, for a caller that
	// already holds an authenticated connection and for tests. REST is the
	// configuration the exec subresource dials; a driver with a Client and no
	// REST serves every call but Exec and the archive transfers. Now is the
	// clock every stamp reads.
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
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}

// Driver implements runtime.Driver against one namespace of one cluster.
type Driver struct {
	opts   Options
	cs     kubernetes.Interface
	stream streamer
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
		return cfg, nil
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

func (d *Driver) Name() string      { return "k8s" }
func (d *Driver) Isolation() string { return "container" }

// Capabilities declares only what this driver enforces today. Egress, mesh,
// attach, dial, display, input, resize, pool, volumes and snapshots each land
// with the slice that builds them.
func (d *Driver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Files: true}
}

// verbs are the accesses the driver uses, checked one review each so a missing
// rule is named before the first sandbox rather than at the first create.
var verbs = []struct{ resource, subresource, verb string }{
	{"pods", "", "get"}, {"pods", "", "list"}, {"pods", "", "create"},
	{"pods", "", "delete"}, {"pods", "", "patch"},
	{"pods", "exec", "create"}, {"pods", "log", "get"},
	{"persistentvolumeclaims", "", "get"}, {"persistentvolumeclaims", "", "list"},
	{"persistentvolumeclaims", "", "create"}, {"persistentvolumeclaims", "", "delete"},
	{"persistentvolumeclaims", "", "patch"},
}

// Preflight proves the cluster answers, the namespace holds the objects, the
// service account may act on them, and the storage class exists.
func (d *Driver) Preflight(ctx context.Context) error {
	if err := d.Ready(ctx); err != nil {
		return err
	}
	var missing []string
	for _, v := range verbs {
		review, err := d.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: d.opts.Namespace, Resource: v.resource, Subresource: v.subresource, Verb: v.verb,
			}},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("access review for %s %s: %w", v.verb, resourceName(v.resource, v.subresource), err)
		}
		if !review.Status.Allowed {
			missing = append(missing, v.verb+" "+resourceName(v.resource, v.subresource))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("namespace %s: the service account may not %s", d.opts.Namespace, strings.Join(missing, ", "))
	}
	if class := d.opts.StorageClass; class != "" {
		if _, err := d.cs.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("storage class %s: %w", class, err)
		}
	}
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
