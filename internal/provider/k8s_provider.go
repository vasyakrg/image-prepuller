package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/local/image-prepuller/internal/model"
)

const (
	// DefaultSnapshotTTL bounds how long one list round is reused. The metrics
	// scrape, the UI polling and the reconciler call Snapshot independently, so
	// without a cache a single cluster listing would be repeated several times a
	// second against the API server and every node agent.
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
// do that. By default the provider talks to the agent HTTP/JSON API v1 (see
// k8s_agent.go): an unreachable agent makes that node's slots offline and the
// snapshot partial instead of silently pretending the images are missing.
// WithNodeInventorySource overrides the inventory source (tests, offline data).
//
// Dependency boundary (ADR-001): client-go and k8s.io/api are allowed only in
// internal/provider/k8s*.go, never in internal/web, internal/model or the agent.
type K8sProvider struct {
	clientset kubernetes.Interface
	clusterID string

	namespaces        []string // empty means all namespaces
	labelSelector     string   // workload scope ("" = every workload)
	nodeLabelSelector string   // node scope ("" = every node)
	inventory         NodeInventorySource
	agent             *agentTransport // default inventory + pull transport (k8s_agent.go)
	diskPauseRatio    float64
	kubeletGCRatio    float64
	snapshotTTL       time.Duration
	healthTimeout     time.Duration

	// now and versionFn are seams for deterministic tests; production uses
	// time.Now and the discovery client.
	now       func() time.Time
	versionFn func(ctx context.Context) (string, error)

	mu         sync.Mutex // guards the snapshot cache and lastSyncAt
	cache      snapshotCache
	lastSyncAt time.Time

	// registryAuth caches parsed imagePullSecrets and the image -> pull-secret
	// index of the last snapshot; see k8s_registry.go. Never held while p.mu is
	// taken, so a pull cannot deadlock a scrape.
	authMu      sync.Mutex
	authCache   map[string]registryAuthCacheEntry
	secretIndex map[string][]secretRef
}

type snapshotCache struct {
	snapshot  model.Snapshot
	expiresAt time.Time
	valid     bool
}

// K8sOption customizes K8sProvider construction.
type K8sOption func(*K8sProvider)

// WithNodeInventorySource overrides the node-agent inventory seam. The default
// is the live agent HTTP/JSON transport (k8s_agent.go); tests and offline
// snapshots pass a StaticInventorySource or UnavailableInventorySource instead.
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
// e.g. "prepull=true". Workloads without the label are not listed, so the UI,
// the reconciler and pull actions never see or touch them.
func WithLabelSelector(selector string) K8sOption {
	return func(p *K8sProvider) { p.labelSelector = strings.TrimSpace(selector) }
}

// WithNodeSelector restricts node listing (and therefore the image x node slot
// matrix) to one label selector, e.g. "!node-role.kubernetes.io/control-plane"
// when the agent DaemonSet runs on workers only. Nodes outside the selector are
// not shown, not counted in coverage and not pulled to.
func WithNodeSelector(selector string) K8sOption {
	return func(p *K8sProvider) { p.nodeLabelSelector = strings.TrimSpace(selector) }
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
		diskPauseRatio: DefaultDiskPauseRatio,
		kubeletGCRatio: DefaultKubeletGCRatio,
		snapshotTTL:    DefaultSnapshotTTL,
		healthTimeout:  DefaultHealthTimeout,
		now:            time.Now,
		authCache:      make(map[string]registryAuthCacheEntry),
	}
	p.agent = newAgentTransport(clientset)
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	if p.inventory == nil {
		// Live default: per-node inventory over the agent HTTP/JSON API v1.
		p.inventory = p.agent
	}
	if p.versionFn == nil {
		p.versionFn = p.apiServerVersion
	}
	return p
}

// Clientset exposes the clientset to the reconciler (WS-C) and tests.
func (p *K8sProvider) Clientset() kubernetes.Interface { return p.clientset }

// SyncNow drops the cached snapshot so the next Snapshot re-lists the cluster.
// The reconciliation pass itself belongs to WS-C.
func (p *K8sProvider) SyncNow(_ context.Context, _ string) error {
	p.mu.Lock()
	p.cache = snapshotCache{}
	p.mu.Unlock()
	return nil
}
