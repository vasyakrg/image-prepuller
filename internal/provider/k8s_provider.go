package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/local/image-prepuller/internal/model"
)

const (
	// DefaultSnapshotTTL bounds how long one list round is reused. metrics.Handler
	// calls Snapshot six times per scrape (AGENTS.md section 8), so an uncached
	// live provider would hammer the API server with 6x the list traffic.
	DefaultSnapshotTTL = 2 * time.Second
	// DefaultDiskPauseRatio is the ImageFS usage above which auto-prepull pauses.
	// It sits below the kubelet GC threshold (0.85) on purpose: stop pre-pulling
	// before the kubelet starts evicting images.
	DefaultDiskPauseRatio = 0.78
	// DefaultKubeletGCRatio is the kubelet image GC high threshold shown in the UI.
	DefaultKubeletGCRatio = 0.85
	// DefaultHealthTimeout bounds the apiserver health probe.
	DefaultHealthTimeout = 2 * time.Second
)

// K8sProvider is the live Kubernetes backend (WS-B). It lists Deployments,
// StatefulSets and DaemonSets through client-go, pairs every workload image with
// every cluster node (the "slot" model) and takes the per-node image inventory
// from the node agent through NodeInventorySource.
//
// It never talks to a container runtime directly: only the node agent (WS-A) may
// do that. Until the agent transport exists, UnavailableInventorySource makes
// every slot offline and the snapshot partial instead of silently pretending the
// images are missing.
//
// Dependency boundary (ADR-001): client-go and k8s.io/api are allowed only in
// internal/provider/k8s*.go, never in internal/web, internal/model or the agent.
type K8sProvider struct {
	clientset kubernetes.Interface
	clusterID string

	namespaces     []string // empty means all namespaces
	labelSelector  string
	inventory      NodeInventorySource
	diskPauseRatio float64
	kubeletGCRatio float64
	snapshotTTL    time.Duration
	healthTimeout  time.Duration

	// now and versionFn are seams for deterministic tests; production uses
	// time.Now and the discovery client.
	now       func() time.Time
	versionFn func(ctx context.Context) (string, error)

	mu         sync.Mutex // guards the snapshot cache and lastSyncAt
	cache      snapshotCache
	lastSyncAt time.Time
}

type snapshotCache struct {
	snapshot  model.Snapshot
	expiresAt time.Time
	valid     bool
}

// K8sOption customizes K8sProvider construction.
type K8sOption func(*K8sProvider)

// WithNodeInventorySource wires the node-agent inventory seam (WS-A). The
// default UnavailableInventorySource reports every node as agent-down.
func WithNodeInventorySource(src NodeInventorySource) K8sOption {
	return func(p *K8sProvider) { p.inventory = src }
}

// WithNamespaces restricts the workload listing to the given namespaces. No
// namespaces means every namespace in the cluster.
func WithNamespaces(namespaces ...string) K8sOption {
	return func(p *K8sProvider) {
		p.namespaces = append([]string(nil), namespaces...)
	}
}

// WithLabelSelector restricts workload listing to one label selector,
// e.g. "prepuller.io/enabled=true".
func WithLabelSelector(selector string) K8sOption {
	return func(p *K8sProvider) { p.labelSelector = selector }
}

// WithSnapshotTTL sets the snapshot cache TTL; zero or negative disables caching.
func WithSnapshotTTL(ttl time.Duration) K8sOption {
	return func(p *K8sProvider) { p.snapshotTTL = ttl }
}

// WithDiskThresholds overrides the ImageFS pause ratio and the kubelet GC ratio.
func WithDiskThresholds(pauseRatio, kubeletGCRatio float64) K8sOption {
	return func(p *K8sProvider) {
		if pauseRatio > 0 {
			p.diskPauseRatio = pauseRatio
		}
		if kubeletGCRatio > 0 {
			p.kubeletGCRatio = kubeletGCRatio
		}
	}
}

// WithHealthTimeout bounds the apiserver health probe.
func WithHealthTimeout(timeout time.Duration) K8sOption {
	return func(p *K8sProvider) {
		if timeout > 0 {
			p.healthTimeout = timeout
		}
	}
}

// withClock replaces the clock; tests only.
func withClock(now func() time.Time) K8sOption {
	return func(p *K8sProvider) {
		if now != nil {
			p.now = now
		}
	}
}

// withServerVersion replaces the apiserver version probe; tests only.
func withServerVersion(fn func(ctx context.Context) (string, error)) K8sOption {
	return func(p *K8sProvider) { p.versionFn = fn }
}

// NewK8sProvider builds the live provider from a kubeconfig or in-cluster
// rest.Config. It does not contact the apiserver until the first Snapshot/Health.
func NewK8sProvider(clusterID string, cfg *rest.Config, opts ...K8sOption) (*K8sProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s provider: nil rest config")
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s provider: build clientset: %w", err)
	}
	return NewK8sProviderWithClientset(clusterID, clientset, opts...), nil
}

// NewK8sProviderWithClientset builds a provider over an existing clientset.
// Tests use a fake clientset; production goes through NewK8sProvider.
func NewK8sProviderWithClientset(clusterID string, clientset kubernetes.Interface, opts ...K8sOption) *K8sProvider {
	if clusterID == "" {
		clusterID = "kubernetes"
	}
	p := &K8sProvider{
		clientset:      clientset,
		clusterID:      clusterID,
		inventory:      UnavailableInventorySource{},
		diskPauseRatio: DefaultDiskPauseRatio,
		kubeletGCRatio: DefaultKubeletGCRatio,
		snapshotTTL:    DefaultSnapshotTTL,
		healthTimeout:  DefaultHealthTimeout,
		now:            time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	if p.inventory == nil {
		p.inventory = UnavailableInventorySource{}
	}
	if p.versionFn == nil {
		p.versionFn = p.apiServerVersion
	}
	return p
}

// Clientset exposes the clientset to the reconciler (WS-C) and tests.
func (p *K8sProvider) Clientset() kubernetes.Interface { return p.clientset }

// TriggerPull reports ErrNotImplemented until the node-agent transport (WS-A)
// can execute CRI PullImage on a node. There is deliberately no local fallback:
// the hub runs on the control plane and cannot touch node image stores.
func (p *K8sProvider) TriggerPull(_ context.Context, _ model.PullRequest) (model.PullTask, error) {
	return model.PullTask{}, fmt.Errorf("k8s provider: TriggerPull: node-agent transport unavailable (WS-A): %w", ErrNotImplemented)
}

// TriggerWorkloadPull is unavailable for the same reason as TriggerPull.
func (p *K8sProvider) TriggerWorkloadPull(_ context.Context, _ model.WorkloadPullRequest) (int, error) {
	return 0, fmt.Errorf("k8s provider: TriggerWorkloadPull: node-agent transport unavailable (WS-A): %w", ErrNotImplemented)
}

// CancelPull is unavailable for the same reason as TriggerPull.
func (p *K8sProvider) CancelPull(_ context.Context, _ string) error {
	return fmt.Errorf("k8s provider: CancelPull: node-agent transport unavailable (WS-A): %w", ErrNotImplemented)
}

// SyncNow drops the cached snapshot so the next Snapshot re-lists the cluster.
// The reconciliation pass itself belongs to WS-C.
func (p *K8sProvider) SyncNow(_ context.Context, _ string) error {
	p.mu.Lock()
	p.cache = snapshotCache{}
	p.mu.Unlock()
	return nil
}

// PullCounters stays empty until the node agent reports per-node pull counters
// (WS-A). nil means "no series", not "zero pulls".
func (p *K8sProvider) PullCounters() []model.PullMetricSample { return nil }
